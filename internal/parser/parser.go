// Package parser uses tree-sitter to extract functions, methods, and classes
// from source files.  It returns a language-agnostic list of symbols that the
// summariser can pass to the LLM.
package parser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/javascript"
	"github.com/smacker/go-tree-sitter/python"
	"github.com/smacker/go-tree-sitter/typescript/typescript"
)

// Symbol represents a single named symbol (function, method, class, etc.)
// extracted from a source file.
type Symbol struct {
	// Kind is one of: "function", "method", "class", "interface", "struct"
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Signature is the first line (or declaration line) of the symbol.
	Signature string `json:"signature"`
	// Body is the full source text of the symbol.
	Body string `json:"body"`
	// StartLine and EndLine are 1-indexed.
	StartLine uint32 `json:"start_line"`
	EndLine   uint32 `json:"end_line"`
}

// File holds the parsed results for a single source file.
type File struct {
	Path    string   `json:"path"`
	Lang    string   `json:"lang"`
	Symbols []Symbol `json:"symbols"`
}

// ──────────────────────────────────────────────────────────
// Language registry
// ──────────────────────────────────────────────────────────

type langDef struct {
	lang    *sitter.Language
	queries []query // ordered: most specific first
}

// query bundles a tree-sitter query pattern with the capture name that holds
// the symbol name and the kind label to assign to results.
type query struct {
	pattern string
	kind    string
	// nameCapture is the name of the @-capture that holds the identifier.
	nameCapture string
	// bodyCapture is the name of the @-capture for the whole node.
	bodyCapture string
}

var registry map[string]langDef

func init() {
	registry = map[string]langDef{
		// ── Go ──────────────────────────────────────────────────────────────
		"go": {
			lang: golang.GetLanguage(),
			queries: []query{
				{
					pattern:     `(function_declaration name: (identifier) @name) @body`,
					kind:        "function",
					nameCapture: "name",
					bodyCapture: "body",
				},
				{
					pattern:     `(method_declaration name: (field_identifier) @name) @body`,
					kind:        "method",
					nameCapture: "name",
					bodyCapture: "body",
				},
				{
					pattern:     `(type_declaration (type_spec name: (type_identifier) @name type: (struct_type))) @body`,
					kind:        "struct",
					nameCapture: "name",
					bodyCapture: "body",
				},
				{
					pattern:     `(type_declaration (type_spec name: (type_identifier) @name type: (interface_type))) @body`,
					kind:        "interface",
					nameCapture: "name",
					bodyCapture: "body",
				},
			},
		},
		// ── Python ──────────────────────────────────────────────────────────
		"python": {
			lang: python.GetLanguage(),
			queries: []query{
				{
					pattern:     `(function_definition name: (identifier) @name) @body`,
					kind:        "function",
					nameCapture: "name",
					bodyCapture: "body",
				},
				{
					pattern:     `(class_definition name: (identifier) @name) @body`,
					kind:        "class",
					nameCapture: "name",
					bodyCapture: "body",
				},
			},
		},
		// ── JavaScript ──────────────────────────────────────────────────────
		"javascript": {
			lang: javascript.GetLanguage(),
			queries: []query{
				{
					pattern:     `(function_declaration name: (identifier) @name) @body`,
					kind:        "function",
					nameCapture: "name",
					bodyCapture: "body",
				},
				{
					pattern:     `(method_definition name: (property_identifier) @name) @body`,
					kind:        "method",
					nameCapture: "name",
					bodyCapture: "body",
				},
				{
					pattern:     `(class_declaration name: (identifier) @name) @body`,
					kind:        "class",
					nameCapture: "name",
					bodyCapture: "body",
				},
				{
					// Arrow / const functions: const foo = (...) => { ... }
					pattern:     `(lexical_declaration (variable_declarator name: (identifier) @name value: [(arrow_function) (function_expression)])) @body`,
					kind:        "function",
					nameCapture: "name",
					bodyCapture: "body",
				},
			},
		},
		// ── TypeScript ──────────────────────────────────────────────────────
		"typescript": {
			lang: typescript.GetLanguage(),
			queries: []query{
				{
					pattern:     `(function_declaration name: (identifier) @name) @body`,
					kind:        "function",
					nameCapture: "name",
					bodyCapture: "body",
				},
				{
					pattern:     `(method_definition name: (property_identifier) @name) @body`,
					kind:        "method",
					nameCapture: "name",
					bodyCapture: "body",
				},
				{
					pattern:     `(class_declaration name: (type_identifier) @name) @body`,
					kind:        "class",
					nameCapture: "name",
					bodyCapture: "body",
				},
				{
					pattern:     `(interface_declaration name: (type_identifier) @name) @body`,
					kind:        "interface",
					nameCapture: "name",
					bodyCapture: "body",
				},
				{
					pattern:     `(lexical_declaration (variable_declarator name: (identifier) @name value: [(arrow_function) (function_expression)])) @body`,
					kind:        "function",
					nameCapture: "name",
					bodyCapture: "body",
				},
			},
		},
	}

	// tsx shares the TypeScript grammar
	registry["tsx"] = registry["typescript"]
	// jsx shares the JavaScript grammar
	registry["jsx"] = registry["javascript"]
}

// ──────────────────────────────────────────────────────────
// Public API
// ──────────────────────────────────────────────────────────

// ParseFile reads the file at path, detects its language, and extracts all
// top-level symbols.
func ParseFile(path string) (*File, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("parser: read %s: %w", path, err)
	}
	return ParseBytes(path, src)
}

// ParseBytes parses already-loaded source bytes.  The path is used only for
// language detection and is stored in the returned File.
func ParseBytes(path string, src []byte) (*File, error) {
	// Try tree-sitter first.
	if lang, langName, ok := detectLang(path); ok {
		parser := sitter.NewParser()
		parser.SetLanguage(lang.lang)
		tree, err := parser.ParseCtx(context.Background(), nil, src)
		if err != nil {
			return nil, fmt.Errorf("parser: parse %s: %w", path, err)
		}
		defer tree.Close()
		return &File{
			Path:    path,
			Lang:    langName,
			Symbols: extractSymbols(tree.RootNode(), src, lang),
		}, nil
	}

	// Plain-text fallback — chunk the file into sections.
	if langName, ok := isPlainText(path); ok {
		return &File{
			Path:    path,
			Lang:    langName,
			Symbols: chunkPlainText(src),
		}, nil
	}

	return nil, fmt.Errorf("parser: unsupported language for %s", path)
}

// SupportedExtensions returns all file extensions lup can parse,
// including plain-text fallback types.
func SupportedExtensions() []string {
	return []string{
		// tree-sitter parsed
		".go", ".py", ".js", ".jsx", ".ts", ".tsx",
		// plain-text chunked
		".sh", ".bash", ".zsh", ".toml", ".yaml", ".yml",
		".json", ".md", ".markdown",
	}
}

// plainTextTypes lists extensions handled by the plain-text fallback.
// Files with no extension whose base name matches a known name are also
// handled (e.g. "Makefile", "Dockerfile").
var plainTextNames = map[string]bool{
	"makefile":   true,
	"dockerfile": true,
	"vagrantfile": true,
	"justfile":   true,
}

var plainTextExts = map[string]bool{
	"sh": true, "bash": true, "zsh": true,
	"toml": true, "yaml": true, "yml": true,
	"json": true, "md": true, "markdown": true,
}

// ──────────────────────────────────────────────────────────
// Internal helpers
// ──────────────────────────────────────────────────────────

func detectLang(path string) (langDef, string, bool) {
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
	def, ok := registry[ext]
	return def, ext, ok
}

// isPlainText reports whether path should be handled by the plain-text
// fallback rather than tree-sitter.
func isPlainText(path string) (string, bool) {
	base := strings.ToLower(filepath.Base(path))
	ext  := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")

	if plainTextNames[base] {
		return base, true
	}
	if plainTextExts[ext] {
		return ext, true
	}
	return "", false
}

func extractSymbols(root *sitter.Node, src []byte, def langDef) []Symbol {
	seen := make(map[string]bool) // deduplicate by "kind:name:startLine"
	var symbols []Symbol

	for _, q := range def.queries {
		tsQuery, err := sitter.NewQuery([]byte(q.pattern), def.lang)
		if err != nil {
			// Bad query pattern — skip silently (shouldn't happen with
			// hard-coded patterns, but let's not crash).
			continue
		}

		cursor := sitter.NewQueryCursor()
		cursor.Exec(tsQuery, root)

		for {
			match, ok := cursor.NextMatch()
			if !ok {
				break
			}

			var nameNode, bodyNode *sitter.Node
			for _, capture := range match.Captures {
				captureName := tsQuery.CaptureNameForId(capture.Index)
				switch captureName {
				case q.nameCapture:
					nameNode = capture.Node
				case q.bodyCapture:
					bodyNode = capture.Node
				}
			}
			if nameNode == nil || bodyNode == nil {
				continue
			}

			name := nameNode.Content(src)
			startLine := bodyNode.StartPoint().Row + 1 // convert to 1-indexed
			endLine := bodyNode.EndPoint().Row + 1

			key := fmt.Sprintf("%s:%s:%d", q.kind, name, startLine)
			if seen[key] {
				continue
			}
			seen[key] = true

			body := bodyNode.Content(src)
			signature := firstLine(body)

			symbols = append(symbols, Symbol{
				Kind:      q.kind,
				Name:      name,
				Signature: signature,
				Body:      body,
				StartLine: startLine,
				EndLine:   endLine,
			})
		}
	}

	return symbols
}

// firstLine returns the first non-empty line of s, truncated to 200 chars.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > 200 {
			return line[:200] + "…"
		}
		return line
	}
	return s
}

// chunkPlainText splits src into overlapping chunks of ~40 lines so the LLM
// can summarise plain-text files (Makefiles, shell scripts, config files, etc.)
// without needing a grammar.  Each chunk becomes a Symbol with Kind="chunk".
func chunkPlainText(src []byte) []Symbol {
	const chunkSize = 40
	const overlap   = 5

	lines := strings.Split(string(src), "\n")
	var symbols []Symbol

	for start := 0; start < len(lines); start += chunkSize - overlap {
		end := start + chunkSize
		if end > len(lines) {
			end = len(lines)
		}

		chunk := lines[start:end]
		body  := strings.Join(chunk, "\n")

		// Skip chunks that are entirely blank/comment.
		nonEmpty := 0
		for _, l := range chunk {
			if strings.TrimSpace(l) != "" && !strings.HasPrefix(strings.TrimSpace(l), "#") {
				nonEmpty++
			}
		}
		if nonEmpty == 0 {
			if end >= len(lines) {
				break
			}
			continue
		}

		name := fmt.Sprintf("lines_%d_%d", start+1, end)
		symbols = append(symbols, Symbol{
			Kind:      "chunk",
			Name:      name,
			Signature: firstLine(body),
			Body:      body,
			StartLine: uint32(start + 1),
			EndLine:   uint32(end),
		})

		if end >= len(lines) {
			break
		}
	}

	return symbols
}
