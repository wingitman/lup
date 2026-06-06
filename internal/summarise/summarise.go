// Package summarise turns parsed source files into stored LUP summaries.
package summarise

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/wingitman/lup/internal/parser"
	"github.com/wingitman/lup/internal/store"
)

// Generator is the minimal text-generation contract used by the summariser.
type Generator interface {
	Complete(ctx context.Context, systemPrompt, userPrompt string) (string, error)
}

// Options controls prompt construction.
type Options struct {
	SystemPrompt        string
	Query               string
	MaxSymbolsPerPrompt int
	BodyBudget          int
}

// DefaultSystemPrompt is the built-in prompt used when config does not override it.
const DefaultSystemPrompt = `You are a code documentation assistant. Given a source file and its extracted
symbols, produce concise, accurate summaries.

Rules:
- FUNCTIONS/METHODS: describe what it does and its side effects in one sentence.
- VARIABLES/CONSTANTS: describe the CONCEPT the name represents and HOW it is
  used. If occurrence_count > 1, note the pattern of usage across scopes.
  Example: "outputJSON controls whether CLI output is emitted as JSON or
  human-readable text; enabled by the --json global flag."
- CLASSES/STRUCTS/INTERFACES: describe the entity and its responsibilities.
- CHUNKS (plain-text sections): summarise what this section configures or does.
- For ALL symbols: populate "references" with names of other symbols, variables,
  or functions this symbol uses, calls, reads, or depends on. Be specific —
  list actual names visible in the code, not generic descriptions.

Respond ONLY with valid JSON matching this schema exactly:
{
  "file_summary": "One or two sentences describing the overall purpose of the file.",
  "symbols": [
    {
      "name": "<symbol name>",
      "kind": "<function|method|variable|constant|class|struct|interface|attribute|chunk>",
      "signature": "<first line of the symbol declaration>",
      "summary": "<description>",
      "references": ["name1", "name2"]
    }
  ]
}
Do not add any text outside the JSON object.`

type llmSummaryResponse struct {
	FileSummary string                `json:"file_summary"`
	Symbols     []store.SymbolSummary `json:"symbols"`
}

// File summarises a parsed file with the supplied generator.
func File(ctx context.Context, gen Generator, relPath string, parsed *parser.File, opts Options) (store.FileSummary, error) {
	if opts.SystemPrompt == "" {
		opts.SystemPrompt = DefaultSystemPrompt
	}
	if opts.MaxSymbolsPerPrompt <= 0 {
		opts.MaxSymbolsPerPrompt = 25
	}
	if opts.BodyBudget <= 0 {
		opts.BodyBudget = 8000
	}

	userPrompt := BuildPrompt(relPath, parsed, opts)
	raw, err := gen.Complete(ctx, opts.SystemPrompt, userPrompt)
	if err != nil {
		return store.FileSummary{}, fmt.Errorf("llm summarise: %w", err)
	}

	raw = cleanJSON(raw)

	var resp llmSummaryResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		return store.FileSummary{}, fmt.Errorf("llm response parse: %w\nraw: %s", err, raw)
	}

	lineMap := map[string]parser.Symbol{}
	for _, s := range parsed.Symbols {
		lineMap[s.Name] = s
	}
	for i := range resp.Symbols {
		if ps, ok := lineMap[resp.Symbols[i].Name]; ok {
			resp.Symbols[i].StartLine = ps.StartLine
			resp.Symbols[i].EndLine = ps.EndLine
			resp.Symbols[i].OccurrenceCount = ps.OccurrenceCount
		}
	}

	return store.FileSummary{
		File:         relPath,
		SummarisedAt: time.Now().UTC(),
		FileSummary:  resp.FileSummary,
		Symbols:      resp.Symbols,
	}, nil
}

// BuildPrompt builds the user prompt sent to the generator.
func BuildPrompt(relPath string, parsed *parser.File, opts Options) string {
	var b strings.Builder
	if strings.TrimSpace(opts.Query) != "" {
		b.WriteString(strings.TrimSpace(opts.Query))
		b.WriteString("\n\n")
	}
	b.WriteString(fmt.Sprintf("File: %s\nLanguage: %s\n\n", relPath, parsed.Lang))

	if len(parsed.Symbols) == 0 {
		b.WriteString("No symbols were extracted. Summarise the file based on its path alone.\n")
		return b.String()
	}

	symsToSend := parsed.Symbols
	if len(symsToSend) > opts.MaxSymbolsPerPrompt {
		var priority0, priority1, priority2 []parser.Symbol
		for _, s := range symsToSend {
			switch s.Kind {
			case "variable", "constant", "attribute":
				priority0 = append(priority0, s)
			case "struct", "interface", "class":
				priority1 = append(priority1, s)
			default:
				priority2 = append(priority2, s)
			}
		}
		symsToSend = nil
		for _, bucket := range [][]parser.Symbol{priority0, priority1, priority2} {
			for _, s := range bucket {
				symsToSend = append(symsToSend, s)
				if len(symsToSend) >= opts.MaxSymbolsPerPrompt {
					break
				}
			}
			if len(symsToSend) >= opts.MaxSymbolsPerPrompt {
				break
			}
		}
	}

	b.WriteString("Symbols:\n")
	for _, sym := range symsToSend {
		occ := ""
		if sym.OccurrenceCount > 1 {
			occ = fmt.Sprintf(" [occurrence_count=%d]", sym.OccurrenceCount)
		}
		b.WriteString(fmt.Sprintf("  [%s] %s%s\n    signature: %s\n", sym.Kind, sym.Name, occ, sym.Signature))
	}

	used := 0
	var bodyLines []string
	priority := func(kind string) int {
		switch kind {
		case "variable", "constant", "attribute":
			return 0
		case "struct", "interface", "class":
			return 1
		default:
			return 2
		}
	}
	sortedForBody := make([]parser.Symbol, len(symsToSend))
	copy(sortedForBody, symsToSend)
	for i := 1; i < len(sortedForBody); i++ {
		for j := i; j > 0 && priority(sortedForBody[j].Kind) < priority(sortedForBody[j-1].Kind); j-- {
			sortedForBody[j], sortedForBody[j-1] = sortedForBody[j-1], sortedForBody[j]
		}
	}

	for _, sym := range sortedForBody {
		lines := strings.Split(sym.Body, "\n")
		maxLines := 30
		if sym.Kind == "variable" || sym.Kind == "constant" || sym.Kind == "attribute" {
			maxLines = 5
		}
		if len(lines) > maxLines {
			lines = lines[:maxLines]
		}
		clean := strings.ReplaceAll(strings.Join(lines, "\n"), "\t", "    ")
		chunk := fmt.Sprintf("\n--- %s %s ---\n%s\n", sym.Kind, sym.Name, clean)
		if used+len(chunk) > opts.BodyBudget {
			break
		}
		bodyLines = append(bodyLines, chunk)
		used += len(chunk)
	}
	if len(bodyLines) > 0 {
		b.WriteString("\nSymbol bodies (truncated):\n")
		for _, l := range bodyLines {
			b.WriteString(l)
		}
	}

	return b.String()
}

func cleanJSON(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	return strings.TrimSpace(raw)
}
