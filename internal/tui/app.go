package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/wingitman/lup/internal/config"
	"github.com/wingitman/lup/internal/llm"
	"github.com/wingitman/lup/internal/parser"
	"github.com/wingitman/lup/internal/rag"
	"github.com/wingitman/lup/internal/store"
	"github.com/wingitman/lup/internal/summarise"
	appupdate "github.com/wingitman/lup/internal/update"
	appversion "github.com/wingitman/lup/internal/version"
)

type focus int

const (
	focusSource focus = iota
	focusSummary
)

type mode int

const (
	modeMain mode = iota
	modeAgents
	modeUpdates
)

type hintsMode int

const (
	hintsFull hintsMode = iota
	hintsNavigation
	hintsActions
)

type Model struct {
	cfg         config.Config
	projectRoot string
	absPath     string
	relPath     string

	source  viewport.Model
	summary viewport.Model
	focus   focus
	mode    mode
	hints   hintsMode

	width  int
	height int

	status       string
	err          string
	generating   bool
	agentCursor  int
	agentNames   []string
	levelCursor  int
	levels       []string
	updateInfo   appupdate.Info
	updateCursor int
	checking     bool
}

type generatedMsg struct {
	summary store.FileSummary
	err     error
}

type reloadedMsg struct{ err error }
type updateMsg struct{ info appupdate.Info }
type clearStatusMsg struct{}
type editorDoneMsg struct{ err error }

var (
	colorPrimary  = lipgloss.Color("#7C9EF0")
	colorAccent   = lipgloss.Color("#F0A47C")
	colorMuted    = lipgloss.Color("#666688")
	colorBorder   = lipgloss.Color("#444466")
	colorSelected = lipgloss.Color("#5865F2")
	stylePath     = lipgloss.NewStyle().Foreground(colorPrimary).Bold(true)
	styleMuted    = lipgloss.NewStyle().Foreground(colorMuted)
	styleAccent   = lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	styleError    = lipgloss.NewStyle().Foreground(lipgloss.Color("#F07C7C")).Bold(true)
	styleSuccess  = lipgloss.NewStyle().Foreground(lipgloss.Color("#7CF09C"))
	styleHintKey  = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFE66D")).Bold(true)
	styleBox      = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorBorder).Padding(0, 1)
	styleFocusBox = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorSelected).Padding(0, 1)
)

func New(path string) (*Model, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	projectRoot := config.ProjectRoot(filepath.Dir(absPath))
	if err := config.EnsureProjectDir(projectRoot); err != nil {
		return nil, err
	}
	cfg, err := config.Load(projectRoot)
	if err != nil {
		return nil, err
	}
	relPath, err := filepath.Rel(projectRoot, absPath)
	if err != nil {
		relPath = absPath
	}
	m := &Model{
		cfg:         cfg,
		projectRoot: projectRoot,
		absPath:     absPath,
		relPath:     relPath,
		levels:      []string{"low", "medium", "high", "xhigh"},
	}
	m.refreshAgentNames()
	m.levelCursor = indexOf(m.levels, cfg.Active.Level)
	if m.levelCursor < 0 {
		m.levelCursor = 1
	}
	if err := m.reload(); err != nil {
		m.err = err.Error()
	}
	return m, nil
}

func (m *Model) Init() tea.Cmd {
	if m.cfg.Updates.DisableChecks {
		return nil
	}
	m.checking = true
	return checkUpdatesCmd(m.cfg)
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.resizePanes()
		return m, nil
	case tea.MouseMsg:
		return m.handleMouse(msg)
	case generatedMsg:
		m.generating = false
		if msg.err != nil {
			m.err = msg.err.Error()
			return m, nil
		}
		m.err = ""
		m.status = "summary regenerated"
		m.setSummary(msg.summary)
		return m, clearStatusCmd()
	case reloadedMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
			return m, nil
		}
		m.err = ""
		m.status = "reloaded"
		return m, clearStatusCmd()
	case updateMsg:
		m.checking = false
		m.updateInfo = msg.info
		return m, nil
	case editorDoneMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
			return m, nil
		}
		return m, reloadCmd(m)
	case clearStatusMsg:
		m.status = ""
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(msg.String())
	}
	return m, nil
}

func (m *Model) handleKey(key string) (tea.Model, tea.Cmd) {
	if key == m.cfg.Keybinds.ShowHints {
		m.hints = (m.hints + 1) % 3
		return m, nil
	}
	if m.mode == modeAgents {
		return m.handleAgentKey(key)
	}
	if m.mode == modeUpdates {
		return m.handleUpdateKey(key)
	}

	switch key {
	case "esc", m.cfg.Keybinds.Quit:
		return m, tea.Quit
	case "tab":
		if m.focus == focusSource {
			m.focus = focusSummary
		} else {
			m.focus = focusSource
		}
		return m, nil
	case m.cfg.Keybinds.Up:
		m.activePane().LineUp(1)
	case m.cfg.Keybinds.Down:
		m.activePane().LineDown(1)
	case m.cfg.Keybinds.PageUp:
		m.activePane().ViewUp()
	case m.cfg.Keybinds.PageDown:
		m.activePane().ViewDown()
	case m.cfg.Keybinds.JumpTop:
		m.activePane().GotoTop()
	case m.cfg.Keybinds.JumpBottom:
		m.activePane().GotoBottom()
	case m.cfg.Keybinds.Regenerate:
		if m.generating {
			return m, nil
		}
		m.generating = true
		m.err = ""
		return m, generateCmd(m)
	case m.cfg.Keybinds.SwitchAgent:
		m.mode = modeAgents
		m.refreshAgentNames()
		m.agentCursor = indexOf(m.agentNames, m.cfg.Active.Agent)
		return m, nil
	case m.cfg.Keybinds.SwitchLevel:
		m.levelCursor = (m.levelCursor + 1) % len(m.levels)
		m.cfg.Active.Level = m.levels[m.levelCursor]
		m.status = "level: " + m.cfg.Active.Level
		return m, clearStatusCmd()
	case m.cfg.Keybinds.ShowUpdates:
		m.mode = modeUpdates
		if m.updateInfo.RepoPath == "" && !m.checking {
			m.checking = true
			return m, checkUpdatesCmd(m.cfg)
		}
		return m, nil
	case m.cfg.Keybinds.Edit:
		if m.focus == focusSource {
			return m, openEditorCmd(m.cfg, m.absPath, 0)
		}
		return m, openEditorCmd(m.cfg, summaryPath(m.projectRoot, m.relPath), 0)
	case m.cfg.Keybinds.Options:
		path := filepath.Join(m.projectRoot, ".lup", "config.toml")
		if _, err := os.Stat(path); os.IsNotExist(err) {
			_ = os.WriteFile(path, []byte(config.BuildTOML(m.cfg)), 0644)
		}
		return m, openEditorCmd(m.cfg, path, 0)
	}
	return m, nil
}

func (m *Model) handleAgentKey(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "esc", m.cfg.Keybinds.Quit:
		m.mode = modeMain
	case m.cfg.Keybinds.Up:
		if m.agentCursor > 0 {
			m.agentCursor--
		}
	case m.cfg.Keybinds.Down:
		if m.agentCursor+1 < len(m.agentNames) {
			m.agentCursor++
		}
	case m.cfg.Keybinds.Confirm:
		if len(m.agentNames) > 0 {
			m.cfg.Active.Agent = m.agentNames[m.agentCursor]
			m.status = "agent: " + m.cfg.Active.Agent
			m.mode = modeMain
			return m, clearStatusCmd()
		}
	}
	return m, nil
}

func (m *Model) handleUpdateKey(key string) (tea.Model, tea.Cmd) {
	commits := m.updateCommits()
	switch key {
	case "esc", m.cfg.Keybinds.Quit:
		m.mode = modeMain
	case m.cfg.Keybinds.Up:
		if m.updateCursor > 0 {
			m.updateCursor--
		}
	case m.cfg.Keybinds.Down:
		if m.updateCursor+1 < len(commits) {
			m.updateCursor++
		}
	case "ctrl+f":
		m.checking = true
		return m, checkUpdatesCmd(m.cfg)
	case "y":
		return m, launchUpdateCmd(m, true, "")
	case "i":
		if len(commits) > 0 {
			return m, launchUpdateCmd(m, false, commits[m.updateCursor].Hash)
		}
	}
	return m, nil
}

func (m *Model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if m.mode != modeMain {
		return m, nil
	}
	leftWidth := m.leftWidth()
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		m.activePane().LineUp(3)
	case tea.MouseButtonWheelDown:
		m.activePane().LineDown(3)
	case tea.MouseButtonLeft:
		if msg.Action == tea.MouseActionPress {
			if msg.X < leftWidth {
				m.focus = focusSource
			} else {
				m.focus = focusSummary
			}
		}
	}
	return m, nil
}

func (m *Model) View() string {
	if m.width == 0 {
		return "Loading..."
	}
	if m.mode == modeAgents {
		return m.renderHeader() + "\n" + m.renderAgents() + "\n" + m.renderStatusBar()
	}
	if m.mode == modeUpdates {
		return m.renderHeader() + "\n" + m.renderUpdates() + "\n" + m.renderStatusBar()
	}
	left := m.renderPane("Document", m.source.View(), m.focus == focusSource, m.leftWidth())
	right := m.renderPane("LUP Summary", m.summary.View(), m.focus == focusSummary, m.rightWidth())
	content := lipgloss.JoinHorizontal(lipgloss.Top, left, right)
	return m.renderHeader() + "\n" + content + "\n" + m.renderStatusBar()
}

func (m *Model) renderHeader() string {
	pathStr := m.relPath
	maxPath := m.width - 34
	if maxPath < 10 {
		maxPath = 10
	}
	if len(pathStr) > maxPath {
		pathStr = "..." + pathStr[len(pathStr)-maxPath:]
	}
	badges := []string{styleMuted.Render("[" + m.cfg.Active.Agent + "]"), styleMuted.Render("[" + m.cfg.Active.Level + "]")}
	if m.generating {
		badges = append(badges, styleAccent.Render("[generating]"))
	}
	delby := lipgloss.NewStyle().Foreground(lipgloss.Color("#FFFFFF")).Bold(true).Render("delby")
	soft := lipgloss.NewStyle().Foreground(lipgloss.Color("#5865F2")).Bold(true).Render("soft")
	brand := " " + delby + soft + " "
	left := stylePath.Render(pathStr) + "  " + strings.Join(badges, " ")
	pad := m.width - lipgloss.Width(left) - lipgloss.Width(brand)
	if pad < 1 {
		pad = 1
	}
	rule := styleMuted.Render(strings.Repeat("-", clamp(m.width, 1, 100)))
	return left + strings.Repeat(" ", pad) + brand + "\n" + rule
}

func (m *Model) renderPane(title, body string, focused bool, width int) string {
	style := styleBox.Width(width - 2).Height(m.paneHeight())
	if focused {
		style = styleFocusBox.Width(width - 2).Height(m.paneHeight())
	}
	header := styleAccent.Render(" " + title + " ")
	return style.Render(header + "\n" + body)
}

func (m *Model) renderAgents() string {
	var b strings.Builder
	b.WriteString(styleAccent.Render("Agents") + "\n\n")
	for i, name := range m.agentNames {
		agent := m.cfg.Agents[name]
		line := fmt.Sprintf("  %-16s %-20s %s", name, agent.Provider, agent.Model)
		if i == m.agentCursor {
			line = lipgloss.NewStyle().Background(colorSelected).Foreground(lipgloss.Color("#EEEEFF")).Render(padRight(line, m.width))
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

func (m *Model) renderUpdates() string {
	var b strings.Builder
	b.WriteString(styleAccent.Render("Updates") + "\n")
	if m.checking {
		return b.String() + styleMuted.Render("Checking for updates...")
	}
	if m.updateInfo.CheckError != "" {
		return b.String() + styleError.Render("Check failed: "+m.updateInfo.CheckError)
	}
	if m.updateInfo.RepoPath == "" {
		return b.String() + styleMuted.Render("No update information loaded.")
	}
	b.WriteString(styleMuted.Render("Repo: ") + m.updateInfo.RepoPath + "\n")
	b.WriteString(styleMuted.Render("Current: ") + shortCommit(m.updateInfo.CurrentCommit) + "  ")
	b.WriteString(styleMuted.Render("Latest: ") + shortCommit(m.updateInfo.LatestCommit) + "\n\n")
	commits := m.updateCommits()
	if len(commits) == 0 {
		b.WriteString(styleSuccess.Render("No newer commits found."))
		return b.String()
	}
	for i, c := range commits {
		if i >= m.paneHeight() {
			break
		}
		line := fmt.Sprintf("  %s  %s  %s", c.Short, c.Date, c.Subject)
		if i == m.updateCursor {
			line = lipgloss.NewStyle().Background(colorSelected).Foreground(lipgloss.Color("#EEEEFF")).Render(padRight(line, m.width))
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

func (m *Model) renderStatusBar() string {
	if m.err != "" {
		return styleError.Render(truncate("  "+m.err+"  [?] Hints", m.width))
	}
	if m.status != "" {
		return styleSuccess.Render(truncate("  "+m.status+"  [?] Hints", m.width))
	}
	parts := []string{}
	if m.mode == modeAgents {
		parts = []string{"[up/down]Nav", "[enter]Select", "[esc]Back"}
	} else if m.mode == modeUpdates {
		parts = []string{"[up/down]Nav", "[y]Install latest", "[i]Install selected", "[ctrl+f]Refresh", "[esc]Back"}
	} else {
		switch m.hints {
		case hintsNavigation:
			parts = []string{"[tab]Focus", "[pgup/pgdown]Page", "[home/end]Top/Bottom", "[mouse]Scroll/Focus", "[?]More"}
		case hintsActions:
			parts = []string{"[r]Regenerate", "[e]Edit pane", "[o]Config", "[m]Model", "[l]Level", "[U]Updates"}
		default:
			parts = []string{"[up/down]Scroll", "[tab]Focus", "[r]Regenerate", "[m]Model", "[l]Level", "[?/o]Hints/Config"}
		}
	}
	return styleMuted.Render(renderHintKeys(truncate("  "+strings.Join(parts, "  "), m.width)))
}

func (m *Model) reload() error {
	data, err := os.ReadFile(m.absPath)
	if err != nil {
		return err
	}
	m.source.SetContent(string(data))
	if fs, err := store.ReadSummary(m.projectRoot, m.relPath); err == nil {
		m.setSummary(fs)
	} else {
		m.summary.SetContent("No summary yet. Press [r] to generate one.")
	}
	return nil
}

func (m *Model) setSummary(fs store.FileSummary) {
	var b strings.Builder
	b.WriteString(fs.File + "\n")
	b.WriteString("summarised: " + fs.SummarisedAt.Format("2006-01-02 15:04") + "\n\n")
	b.WriteString(fs.FileSummary + "\n")
	for _, sym := range fs.Symbols {
		b.WriteString("\n")
		b.WriteString(fmt.Sprintf("[%s] %s", sym.Kind, sym.Name))
		if sym.StartLine > 0 {
			b.WriteString(fmt.Sprintf("  line %d", sym.StartLine))
		}
		b.WriteString("\n")
		if sym.Signature != "" {
			b.WriteString("  " + sym.Signature + "\n")
		}
		if sym.Summary != "" {
			b.WriteString("  " + sym.Summary + "\n")
		}
		if len(sym.References) > 0 {
			b.WriteString("  refs: " + strings.Join(sym.References, ", ") + "\n")
		}
	}
	m.summary.SetContent(b.String())
}

func (m *Model) resizePanes() {
	height := m.paneHeight() - 2
	if height < 1 {
		height = 1
	}
	m.source.Width = m.leftWidth() - 4
	m.source.Height = height
	m.summary.Width = m.rightWidth() - 4
	m.summary.Height = height
}

func (m *Model) paneHeight() int {
	h := m.height - 5
	if h < 5 {
		return 5
	}
	return h
}

func (m *Model) leftWidth() int {
	if m.width < 20 {
		return m.width
	}
	return m.width / 2
}

func (m *Model) rightWidth() int {
	return m.width - m.leftWidth()
}

func (m *Model) activePane() *viewport.Model {
	if m.focus == focusSource {
		return &m.source
	}
	return &m.summary
}

func (m *Model) refreshAgentNames() {
	m.agentNames = m.agentNames[:0]
	for name := range m.cfg.Agents {
		m.agentNames = append(m.agentNames, name)
	}
	for i := 0; i < len(m.agentNames); i++ {
		for j := i + 1; j < len(m.agentNames); j++ {
			if m.agentNames[j] < m.agentNames[i] {
				m.agentNames[i], m.agentNames[j] = m.agentNames[j], m.agentNames[i]
			}
		}
	}
}

func (m *Model) updateCommits() []appupdate.Commit {
	if len(m.updateInfo.Available) > 0 {
		return m.updateInfo.Available
	}
	return m.updateInfo.History
}

func generateCmd(m *Model) tea.Cmd {
	projectRoot, relPath, absPath, cfg := m.projectRoot, m.relPath, m.absPath, m.cfg
	return func() tea.Msg {
		parsed, err := parser.ParseFile(absPath)
		if err != nil {
			return generatedMsg{err: err}
		}
		gen := llm.NewGenerator(cfg.ActiveAgent(), cfg.Active.Level)
		fs, err := summarise.File(context.Background(), gen, relPath, parsed, summarise.Options{
			SystemPrompt:        cfg.Summarisation.SystemPrompt,
			Query:               cfg.Summarisation.Query,
			MaxSymbolsPerPrompt: cfg.Summarisation.MaxSymbolsPerPrompt,
			BodyBudget:          cfg.Summarisation.BodyBudget,
		})
		if err != nil {
			return generatedMsg{err: err}
		}
		if err := store.WriteSummary(projectRoot, fs); err != nil {
			return generatedMsg{err: err}
		}
		vs, err := store.OpenVectorStore(projectRoot)
		if err == nil {
			defer vs.Close()
			embed := llm.New(cfg.Embedding.BaseURL, cfg.Embedding.ResolvedAPIKey(), cfg.ActiveAgent().Model, cfg.Embedding.Model, cfg.Embedding.TimeoutSecs)
			_ = rag.New(embed, vs, projectRoot).IndexSummary(context.Background(), fs)
		}
		return generatedMsg{summary: fs}
	}
}

func reloadCmd(m *Model) tea.Cmd {
	return func() tea.Msg {
		cfg, err := config.Load(m.projectRoot)
		if err != nil {
			return reloadedMsg{err: err}
		}
		m.cfg = cfg
		m.refreshAgentNames()
		return reloadedMsg{err: m.reload()}
	}
}

func checkUpdatesCmd(cfg config.Config) tea.Cmd {
	return func() tea.Msg {
		return updateMsg{info: appupdate.Check(&cfg, appversion.Commit, 20)}
	}
}

func launchUpdateCmd(m *Model, latest bool, target string) tea.Cmd {
	info := m.updateInfo
	cfg := m.cfg
	return func() tea.Msg {
		if latest && target == "" {
			target = info.LatestCommit
		}
		recorder, _ := os.Executable()
		err := appupdate.LaunchDetached(appupdate.InstallRequest{
			RepoPath:       info.RepoPath,
			TargetCommit:   target,
			Latest:         latest,
			Terminal:       cfg.Updates.Terminal,
			RecorderBinary: recorder,
		})
		return editorDoneMsg{err: err}
	}
}

func openEditorCmd(cfg config.Config, path string, line int) tea.Cmd {
	return func() tea.Msg {
		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) && strings.HasSuffix(path, ".json") {
				_ = os.WriteFile(path, []byte("{}\n"), 0644)
			} else {
				return editorDoneMsg{err: err}
			}
		}
		editor := cfg.Apps.Editor
		if editor == "" {
			editor = os.Getenv("EDITOR")
		}
		if editor == "" {
			editor = os.Getenv("VISUAL")
		}
		if editor == "" {
			for _, candidate := range []string{"nano", "vi", "vim", "nvim", "code", "notepad.exe"} {
				if _, err := exec.LookPath(candidate); err == nil {
					editor = candidate
					break
				}
			}
		}
		if editor == "" {
			return editorDoneMsg{err: fmt.Errorf("no editor found; set EDITOR or apps.editor")}
		}
		cmd := exec.Command(editor, editorArgs(editor, path, line)...)
		return tea.ExecProcess(cmd, func(err error) tea.Msg { return editorDoneMsg{err: err} })()
	}
}

func editorArgs(editor, path string, line int) []string {
	if line <= 0 {
		return []string{path}
	}
	name := strings.TrimSuffix(strings.ToLower(filepath.Base(editor)), ".exe")
	switch name {
	case "vi", "vim", "nvim", "nano":
		return []string{fmt.Sprintf("+%d", line), path}
	case "code", "code-insiders", "codium":
		return []string{"--goto", fmt.Sprintf("%s:%d", path, line)}
	default:
		return []string{path}
	}
}

func summaryPath(projectRoot, relPath string) string {
	safe := strings.ReplaceAll(relPath, string(filepath.Separator), "_")
	return filepath.Join(projectRoot, ".lup", "summaries", safe+".json")
}

func clearStatusCmd() tea.Cmd {
	return tea.Tick(1500*time.Millisecond, func(time.Time) tea.Msg { return clearStatusMsg{} })
}

func renderHintKeys(row string) string {
	var b strings.Builder
	for len(row) > 0 {
		start := strings.Index(row, "[")
		if start < 0 {
			b.WriteString(row)
			break
		}
		b.WriteString(row[:start])
		row = row[start:]
		end := strings.Index(row, "]")
		if end < 0 {
			b.WriteString(row)
			break
		}
		b.WriteString(styleHintKey.Render(row[:end+1]))
		row = row[end+1:]
	}
	return b.String()
}

func padRight(s string, width int) string {
	if lipgloss.Width(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-lipgloss.Width(s))
}

func truncate(s string, width int) string {
	if width <= 0 || len(s) <= width {
		return s
	}
	return s[:width-1]
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func indexOf(items []string, target string) int {
	for i, item := range items {
		if item == target {
			return i
		}
	}
	return -1
}

func shortCommit(hash string) string {
	if len(hash) > 7 {
		return hash[:7]
	}
	if hash == "" {
		return "unknown"
	}
	return hash
}
