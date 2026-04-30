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
	lang, langName, ok := detectLang(path)
	if !ok {
		return nil, fmt.Errorf("parser: unsupported language for %s", path)
	}

	parser := sitter.NewParser()
	parser.SetLanguage(lang.lang)
	tree, err := parser.ParseCtx(context.Background(), nil, src)
	if err != nil {
		return nil, fmt.Errorf("parser: parse %s: %w", path, err)
	}
	defer tree.Close()

	symbols := extractSymbols(tree.RootNode(), src, lang)

	return &File{
		Path:    path,
		Lang:    langName,
		Symbols: symbols,
	}, nil
}

// SupportedExtensions returns all file extensions lup can parse.
func SupportedExtensions() []string {
	return []string{".go", ".py", ".js", ".jsx", ".ts", ".tsx"}
}

// ──────────────────────────────────────────────────────────
// Internal helpers
// ──────────────────────────────────────────────────────────

func detectLang(path string) (langDef, string, bool) {
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
	def, ok := registry[ext]
	return def, ext, ok
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
