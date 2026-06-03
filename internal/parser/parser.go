// Package parser extracts named symbols from source files, returning a
// language-agnostic list the summariser passes to the LLM.
package parser

import (
	"fmt"
	"go/ast"
	goparser "go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// symbolStoplist contains names that are too generic to be worth indexing
// individually (error variables, loop counters, blank identifiers, etc.).
var symbolStoplist = map[string]bool{
	"err": true, "_": true, "ok": true, "ctx": true,
	"i": true, "j": true, "n": true, "v": true, "k": true,
	"b": true, "s": true, "r": true, "w": true, "f": true,
	"e": true, "c": true, "t": true, "x": true, "y": true,
	"buf": true, "tmp": true, "ret": true, "res": true,
	"req": true, "resp": true, "out": true, "in": true,
	"val": true, "key": true, "idx": true, "row": true,
	"rows": true, "col": true, "ch": true, "wg": true,
	"mu": true, "m": true, "p": true, "q": true, "g": true,
}

// Symbol represents a single named symbol extracted from a source file.
type Symbol struct {
	// Kind: function|method|variable|constant|class|struct|interface|attribute|chunk
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Signature is the first meaningful line of the symbol declaration.
	Signature string `json:"signature"`
	// Body is the full source text of the symbol (truncated in the LLM prompt).
	Body string `json:"body"`
	// StartLine and EndLine are 1-indexed.
	StartLine uint32 `json:"start_line"`
	EndLine   uint32 `json:"end_line"`
	// OccurrenceCount is how many times this name was declared/assigned in the
	// file. > 1 means the same name is reused across multiple scopes.
	OccurrenceCount int `json:"occurrence_count"`
}

// File holds the parsed results for a single source file.
type File struct {
	Path    string   `json:"path"`
	Lang    string   `json:"lang"`
	Symbols []Symbol `json:"symbols"`
}

type rawSymbol struct {
	kind      string
	name      string
	signature string
	body      string
	startLine uint32
	endLine   uint32
}

// ParseFile reads the file at path, detects its language, and extracts all
// symbols including variables and constants.
func ParseFile(path string) (*File, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("parser: read %s: %w", path, err)
	}
	return ParseBytes(path, src)
}

// ParseBytes parses already-loaded source bytes.
func ParseBytes(path string, src []byte) (*File, error) {
	lang := detectLang(path)
	switch lang {
	case "go":
		return &File{Path: path, Lang: lang, Symbols: parseGo(path, src)}, nil
	case "python":
		return &File{Path: path, Lang: lang, Symbols: parsePython(src)}, nil
	case "javascript", "jsx":
		return &File{Path: path, Lang: lang, Symbols: parseJavaScript(src, false)}, nil
	case "typescript", "tsx":
		return &File{Path: path, Lang: lang, Symbols: parseJavaScript(src, true)}, nil
	}

	if langName, ok := isPlainText(path); ok {
		return &File{
			Path:    path,
			Lang:    langName,
			Symbols: chunkPlainText(src),
		}, nil
	}

	return nil, fmt.Errorf("parser: unsupported language for %s", path)
}

// SupportedExtensions returns all extensions lup can parse.
func SupportedExtensions() []string {
	return []string{
		".go", ".py", ".js", ".jsx", ".ts", ".tsx",
		".sh", ".bash", ".zsh", ".toml", ".yaml", ".yml",
		".json", ".md", ".markdown",
	}
}

var plainTextNames = map[string]bool{
	"makefile": true, "dockerfile": true,
	"vagrantfile": true, "justfile": true,
}

var plainTextExts = map[string]bool{
	"sh": true, "bash": true, "zsh": true,
	"toml": true, "yaml": true, "yml": true,
	"json": true, "md": true, "markdown": true,
}

func detectLang(path string) string {
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
	switch ext {
	case "go", "py", "js", "jsx", "ts", "tsx":
		if ext == "py" {
			return "python"
		}
		if ext == "js" {
			return "javascript"
		}
		if ext == "ts" {
			return "typescript"
		}
		return ext
	default:
		return ""
	}
}

func isPlainText(path string) (string, bool) {
	base := strings.ToLower(filepath.Base(path))
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
	if plainTextNames[base] {
		return base, true
	}
	if plainTextExts[ext] {
		return ext, true
	}
	return "", false
}

func parseGo(path string, src []byte) []Symbol {
	fset := token.NewFileSet()
	file, err := goparser.ParseFile(fset, path, src, 0)
	if err != nil || file == nil {
		return chunkPlainText(src)
	}

	lines := splitLines(string(src))
	var raw []rawSymbol

	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.FuncDecl:
			kind := "function"
			if node.Recv != nil {
				kind = "method"
			}
			raw = append(raw, rawFromNode(fset, lines, kind, node.Name.Name, node))
			return true
		case *ast.GenDecl:
			for _, spec := range node.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					kind := "variable"
					switch s.Type.(type) {
					case *ast.StructType:
						kind = "struct"
					case *ast.InterfaceType:
						kind = "interface"
					}
					raw = append(raw, rawFromNode(fset, lines, kind, s.Name.Name, node))
				case *ast.ValueSpec:
					kind := "variable"
					if node.Tok == token.CONST {
						kind = "constant"
					}
					for _, name := range s.Names {
						raw = append(raw, rawFromNode(fset, lines, kind, name.Name, node))
					}
				}
			}
			return false
		case *ast.AssignStmt:
			if node.Tok != token.DEFINE {
				return true
			}
			for _, lhs := range node.Lhs {
				if ident, ok := lhs.(*ast.Ident); ok {
					raw = append(raw, rawFromNode(fset, lines, "variable", ident.Name, node))
				}
			}
		}
		return true
	})

	return dedupRaw(raw)
}

func rawFromNode(fset *token.FileSet, lines []string, kind, name string, node ast.Node) rawSymbol {
	start := fset.Position(node.Pos()).Line
	end := fset.Position(node.End()).Line
	body := bodyFromLines(lines, start, end)
	return rawSymbol{
		kind:      kind,
		name:      name,
		signature: firstLine(body),
		body:      body,
		startLine: uint32(start),
		endLine:   uint32(end),
	}
}

var (
	pyFuncRe      = regexp.MustCompile(`^\s*(?:async\s+)?def\s+([A-Za-z_]\w*)\s*\(`)
	pyClassRe     = regexp.MustCompile(`^\s*class\s+([A-Za-z_]\w*)\b`)
	pyAssignRe    = regexp.MustCompile(`^\s*([A-Za-z_]\w*)\s*(?::[^=]+)?=`)
	pyAttributeRe = regexp.MustCompile(`^\s*self\.([A-Za-z_]\w*)\s*=`)
	jsFunctionRe  = regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s+([A-Za-z_$][\w$]*)\s*\(`)
	jsClassRe     = regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?class\s+([A-Za-z_$][\w$]*)\b`)
	jsInterfaceRe = regexp.MustCompile(`^\s*(?:export\s+)?interface\s+([A-Za-z_$][\w$]*)\b`)
	jsVarRe       = regexp.MustCompile(`^\s*(?:export\s+)?(?:const|let|var)\s+([A-Za-z_$][\w$]*)\b`)
	jsFuncValueRe = regexp.MustCompile(`=>|\bfunction\b`)
	jsMethodRe    = regexp.MustCompile(`^\s*(?:public\s+|private\s+|protected\s+|static\s+|async\s+)*([A-Za-z_$][\w$]*)\s*\([^)]*\)\s*[{:]?`)
	jsTypeAliasRe = regexp.MustCompile(`^\s*(?:export\s+)?type\s+([A-Za-z_$][\w$]*)\b`)
	jsKeywords    = map[string]bool{"if": true, "for": true, "while": true, "switch": true, "catch": true, "function": true}
)

func parsePython(src []byte) []Symbol {
	lines := splitLines(string(src))
	var raw []rawSymbol
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := leadingSpaces(line)
		if m := pyFuncRe.FindStringSubmatch(line); m != nil {
			raw = append(raw, rawFromRange(lines, "function", m[1], i+1, pythonBlockEnd(lines, i, indent)))
			continue
		}
		if m := pyClassRe.FindStringSubmatch(line); m != nil {
			raw = append(raw, rawFromRange(lines, "class", m[1], i+1, pythonBlockEnd(lines, i, indent)))
			continue
		}
		if m := pyAttributeRe.FindStringSubmatch(line); m != nil {
			raw = append(raw, rawFromRange(lines, "attribute", m[1], i+1, i+1))
			continue
		}
		if m := pyAssignRe.FindStringSubmatch(line); m != nil {
			raw = append(raw, rawFromRange(lines, "variable", m[1], i+1, i+1))
		}
	}
	return dedupRaw(raw)
}

func parseJavaScript(src []byte, typescript bool) []Symbol {
	lines := splitLines(string(src))
	var raw []rawSymbol
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue
		}
		if m := jsFunctionRe.FindStringSubmatch(line); m != nil {
			raw = append(raw, rawFromRange(lines, "function", m[1], i+1, braceBlockEnd(lines, i)))
			continue
		}
		if m := jsClassRe.FindStringSubmatch(line); m != nil {
			raw = append(raw, rawFromRange(lines, "class", m[1], i+1, braceBlockEnd(lines, i)))
			continue
		}
		if typescript {
			if m := jsInterfaceRe.FindStringSubmatch(line); m != nil {
				raw = append(raw, rawFromRange(lines, "interface", m[1], i+1, braceBlockEnd(lines, i)))
				continue
			}
			if m := jsTypeAliasRe.FindStringSubmatch(line); m != nil {
				raw = append(raw, rawFromRange(lines, "interface", m[1], i+1, braceBlockEnd(lines, i)))
				continue
			}
		}
		if m := jsVarRe.FindStringSubmatch(line); m != nil {
			kind := "variable"
			if jsFuncValueRe.MatchString(line) {
				kind = "function"
			}
			raw = append(raw, rawFromRange(lines, kind, m[1], i+1, braceBlockEnd(lines, i)))
			continue
		}
		if m := jsMethodRe.FindStringSubmatch(line); m != nil && !jsKeywords[m[1]] {
			raw = append(raw, rawFromRange(lines, "method", m[1], i+1, braceBlockEnd(lines, i)))
		}
	}
	return dedupRaw(raw)
}

func rawFromRange(lines []string, kind, name string, start, end int) rawSymbol {
	body := bodyFromLines(lines, start, end)
	return rawSymbol{
		kind:      kind,
		name:      name,
		signature: firstLine(body),
		body:      body,
		startLine: uint32(start),
		endLine:   uint32(end),
	}
}

func dedupRaw(raw []rawSymbol) []Symbol {
	highKinds := map[string]bool{
		"function": true, "method": true, "struct": true,
		"class": true, "interface": true,
	}
	highPriority := map[string]bool{}
	for _, e := range raw {
		if highKinds[e.kind] && !symbolStoplist[e.name] {
			highPriority[e.name] = true
		}
	}

	type group struct {
		first rawSymbol
		count int
	}
	groups := map[string]*group{}
	var order []string

	for _, e := range raw {
		if e.name == "" || symbolStoplist[e.name] {
			continue
		}
		if (e.kind == "variable" || e.kind == "constant" || e.kind == "attribute") && highPriority[e.name] {
			continue
		}
		if g, exists := groups[e.name]; exists {
			g.count++
			if highKinds[e.kind] && !highKinds[g.first.kind] {
				g.first = e
			}
			continue
		}
		groups[e.name] = &group{first: e, count: 1}
		order = append(order, e.name)
	}

	symbols := make([]Symbol, 0, len(order))
	for _, name := range order {
		g := groups[name]
		symbols = append(symbols, Symbol{
			Kind:            g.first.kind,
			Name:            name,
			Signature:       g.first.signature,
			Body:            g.first.body,
			StartLine:       g.first.startLine,
			EndLine:         g.first.endLine,
			OccurrenceCount: g.count,
		})
	}
	return symbols
}

func pythonBlockEnd(lines []string, start, indent int) int {
	end := start + 1
	for i := start + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			end = i + 1
			continue
		}
		if leadingSpaces(lines[i]) <= indent {
			break
		}
		end = i + 1
	}
	return end
}

func braceBlockEnd(lines []string, start int) int {
	depth := 0
	seenBrace := false
	for i := start; i < len(lines); i++ {
		for _, r := range lines[i] {
			switch r {
			case '{', '(', '[':
				depth++
				seenBrace = true
			case '}', ')', ']':
				if depth > 0 {
					depth--
				}
			}
		}
		if seenBrace && depth == 0 {
			return i + 1
		}
		if i == start && !seenBrace {
			return start + 1
		}
		if !seenBrace && strings.HasSuffix(strings.TrimSpace(lines[i]), ";") {
			return i + 1
		}
	}
	return start + 1
}

func leadingSpaces(s string) int {
	count := 0
	for _, r := range s {
		switch r {
		case ' ':
			count++
		case '\t':
			count += 4
		default:
			return count
		}
	}
	return count
}

func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.Split(s, "\n")
}

func bodyFromLines(lines []string, start, end int) string {
	if start < 1 {
		start = 1
	}
	if end < start {
		end = start
	}
	if start > len(lines) {
		return ""
	}
	if end > len(lines) {
		end = len(lines)
	}
	return strings.Join(lines[start-1:end], "\n")
}

// firstLine returns the first non-empty line of s, truncated to 200 chars.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > 200 {
			return line[:200] + "..."
		}
		return line
	}
	return s
}

// chunkPlainText splits src into overlapping 40-line chunks for the LLM.
func chunkPlainText(src []byte) []Symbol {
	const chunkSize = 40
	const overlap = 5

	lines := splitLines(string(src))
	var symbols []Symbol

	for start := 0; start < len(lines); start += chunkSize - overlap {
		end := start + chunkSize
		if end > len(lines) {
			end = len(lines)
		}

		chunk := lines[start:end]
		body := strings.Join(chunk, "\n")

		nonEmpty := 0
		for _, l := range chunk {
			t := strings.TrimSpace(l)
			if t != "" && !strings.HasPrefix(t, "#") {
				nonEmpty++
			}
		}
		if nonEmpty == 0 {
			if end >= len(lines) {
				break
			}
			continue
		}

		symbols = append(symbols, Symbol{
			Kind:            "chunk",
			Name:            fmt.Sprintf("lines_%d_%d", start+1, end),
			Signature:       firstLine(body),
			Body:            body,
			StartLine:       uint32(start + 1),
			EndLine:         uint32(end),
			OccurrenceCount: 1,
		})

		if end >= len(lines) {
			break
		}
	}

	return symbols
}
