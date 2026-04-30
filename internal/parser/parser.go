// Package parser uses tree-sitter to extract symbols (functions, methods,
// variables, constants, classes, structs, etc.) from source files, returning
// a language-agnostic list the summariser passes to the LLM.
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

// ──────────────────────────────────────────────────────────
// Language registry
// ──────────────────────────────────────────────────────────

type langDef struct {
	lang    *sitter.Language
	queries []query
}

type query struct {
	pattern     string
	kind        string
	nameCapture string
	bodyCapture string
	// skipIfFuncValue: when true, skip this match if the value is an arrow
	// function or function expression (those are already caught by the
	// function queries).
	skipIfFuncValue bool
}

var registry map[string]langDef

func init() {
	registry = map[string]langDef{
		// ── Go ──────────────────────────────────────────────────────────────
		"go": {
			lang: golang.GetLanguage(),
			queries: []query{
				// Functions and methods
				{
					pattern:     `(function_declaration name: (identifier) @name) @body`,
					kind:        "function",
					nameCapture: "name", bodyCapture: "body",
				},
				{
					pattern:     `(method_declaration name: (field_identifier) @name) @body`,
					kind:        "method",
					nameCapture: "name", bodyCapture: "body",
				},
				// Types
				{
					pattern:     `(type_declaration (type_spec name: (type_identifier) @name type: (struct_type))) @body`,
					kind:        "struct",
					nameCapture: "name", bodyCapture: "body",
				},
				{
					pattern:     `(type_declaration (type_spec name: (type_identifier) @name type: (interface_type))) @body`,
					kind:        "interface",
					nameCapture: "name", bodyCapture: "body",
				},
				// Package-level variables and constants
				{
					pattern:     `(var_declaration (var_spec name: (identifier) @name)) @body`,
					kind:        "variable",
					nameCapture: "name", bodyCapture: "body",
				},
				{
					pattern:     `(const_declaration (const_spec name: (identifier) @name)) @body`,
					kind:        "constant",
					nameCapture: "name", bodyCapture: "body",
				},
				// Short variable declarations inside functions (:=)
				{
					pattern:     `(short_var_declaration left: (expression_list (identifier) @name)) @body`,
					kind:        "variable",
					nameCapture: "name", bodyCapture: "body",
				},
			},
		},

		// ── Python ──────────────────────────────────────────────────────────
		"python": {
			lang: python.GetLanguage(),
			queries: []query{
				// Functions and classes
				{
					pattern:     `(function_definition name: (identifier) @name) @body`,
					kind:        "function",
					nameCapture: "name", bodyCapture: "body",
				},
				{
					pattern:     `(class_definition name: (identifier) @name) @body`,
					kind:        "class",
					nameCapture: "name", bodyCapture: "body",
				},
				// Module-level assignments
				{
					pattern:     `(module (expression_statement (assignment left: (identifier) @name))) @body`,
					kind:        "variable",
					nameCapture: "name", bodyCapture: "body",
				},
				// Class-level variable assignments (class variables)
				{
					pattern:     `(class_definition body: (block (expression_statement (assignment left: (identifier) @name)) @body))`,
					kind:        "variable",
					nameCapture: "name", bodyCapture: "body",
				},
				// Instance attribute assignments (self.x = ...)
				{
					pattern:     `(assignment left: (attribute object: (identifier) attribute: (identifier) @name)) @body`,
					kind:        "attribute",
					nameCapture: "name", bodyCapture: "body",
				},
				// Function-level assignments
				{
					pattern:     `(function_definition body: (block (expression_statement (assignment left: (identifier) @name)) @body))`,
					kind:        "variable",
					nameCapture: "name", bodyCapture: "body",
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
					nameCapture: "name", bodyCapture: "body",
				},
				{
					pattern:     `(method_definition name: (property_identifier) @name) @body`,
					kind:        "method",
					nameCapture: "name", bodyCapture: "body",
				},
				{
					pattern:     `(class_declaration name: (identifier) @name) @body`,
					kind:        "class",
					nameCapture: "name", bodyCapture: "body",
				},
				// Arrow / const functions (already covered, not variable)
				{
					pattern:     `(lexical_declaration (variable_declarator name: (identifier) @name value: [(arrow_function) (function_expression)])) @body`,
					kind:        "function",
					nameCapture: "name", bodyCapture: "body",
				},
				// const/let declarations that are NOT functions
				{
					pattern:          `(lexical_declaration (variable_declarator name: (identifier) @name)) @body`,
					kind:             "variable",
					nameCapture:      "name", bodyCapture: "body",
					skipIfFuncValue:  true,
				},
				// var declarations
				{
					pattern:          `(variable_declaration (variable_declarator name: (identifier) @name)) @body`,
					kind:             "variable",
					nameCapture:      "name", bodyCapture: "body",
					skipIfFuncValue:  true,
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
					nameCapture: "name", bodyCapture: "body",
				},
				{
					pattern:     `(method_definition name: (property_identifier) @name) @body`,
					kind:        "method",
					nameCapture: "name", bodyCapture: "body",
				},
				{
					pattern:     `(class_declaration name: (type_identifier) @name) @body`,
					kind:        "class",
					nameCapture: "name", bodyCapture: "body",
				},
				{
					pattern:     `(interface_declaration name: (type_identifier) @name) @body`,
					kind:        "interface",
					nameCapture: "name", bodyCapture: "body",
				},
				// Arrow / const functions
				{
					pattern:     `(lexical_declaration (variable_declarator name: (identifier) @name value: [(arrow_function) (function_expression)])) @body`,
					kind:        "function",
					nameCapture: "name", bodyCapture: "body",
				},
				// const/let non-function declarations
				{
					pattern:         `(lexical_declaration (variable_declarator name: (identifier) @name)) @body`,
					kind:            "variable",
					nameCapture:     "name", bodyCapture: "body",
					skipIfFuncValue: true,
				},
				// var declarations
				{
					pattern:         `(variable_declaration (variable_declarator name: (identifier) @name)) @body`,
					kind:            "variable",
					nameCapture:     "name", bodyCapture: "body",
					skipIfFuncValue: true,
				},
				// Class property fields
				{
					pattern:     `(public_field_definition name: (property_identifier) @name) @body`,
					kind:        "attribute",
					nameCapture: "name", bodyCapture: "body",
				},
			},
		},
	}

	registry["tsx"] = registry["typescript"]
	registry["jsx"] = registry["javascript"]
}

// ──────────────────────────────────────────────────────────
// Public API
// ──────────────────────────────────────────────────────────

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
	// Tree-sitter languages.
	if lang, langName, ok := detectLang(path); ok {
		p := sitter.NewParser()
		p.SetLanguage(lang.lang)
		tree, err := p.ParseCtx(context.Background(), nil, src)
		if err != nil {
			return nil, fmt.Errorf("parser: parse %s: %w", path, err)
		}
		defer tree.Close()
		return &File{
			Path:    path,
			Lang:    langName,
			Symbols: extractAndDedup(tree.RootNode(), src, lang),
		}, nil
	}

	// Plain-text fallback.
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

// ──────────────────────────────────────────────────────────
// Internal helpers
// ──────────────────────────────────────────────────────────

func detectLang(path string) (langDef, string, bool) {
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
	def, ok := registry[ext]
	return def, ext, ok
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

// extractAndDedup runs all queries for the language, applies the stoplist,
// then collapses duplicate names (same symbol name appearing in multiple
// scopes) into a single Symbol with OccurrenceCount > 1.
func extractAndDedup(root *sitter.Node, src []byte, def langDef) []Symbol {
	// raw: all matches before dedup
	type rawEntry struct {
		kind      string
		name      string
		signature string
		body      string
		startLine uint32
		endLine   uint32
	}
	var raw []rawEntry

	// Track which names we've emitted from "high-priority" kinds (functions,
	// methods, structs, classes, interfaces) so we don't override them with
	// a variable of the same name.
	highPriority := map[string]bool{}
	highKinds := map[string]bool{
		"function": true, "method": true, "struct": true,
		"class": true, "interface": true,
	}

	for _, q := range def.queries {
		tsQuery, err := sitter.NewQuery([]byte(q.pattern), def.lang)
		if err != nil {
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
				capName := tsQuery.CaptureNameForId(capture.Index)
				switch capName {
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

			// Stoplist check.
			if symbolStoplist[name] {
				continue
			}

			// For skipIfFuncValue queries: check if the body contains a
			// function/arrow value and skip if so (already indexed by
			// the function query).
			if q.skipIfFuncValue && bodyContainsFuncValue(bodyNode) {
				continue
			}

			body := bodyNode.Content(src)
			entry := rawEntry{
				kind:      q.kind,
				name:      name,
				signature: firstLine(body),
				body:      body,
				startLine: bodyNode.StartPoint().Row + 1,
				endLine:   bodyNode.EndPoint().Row + 1,
			}
			raw = append(raw, entry)

			if highKinds[q.kind] {
				highPriority[name] = true
			}
		}
	}

	// Dedup: group by name, collapse occurrences.
	type group struct {
		first rawEntry
		count int
	}
	groups := map[string]*group{}
	order := []string{} // preserve first-seen order

	for _, e := range raw {
		// Skip variable/constant/attribute if a function/struct/class of the
		// same name was already seen — the structural symbol is more informative.
		if (e.kind == "variable" || e.kind == "constant" || e.kind == "attribute") &&
			highPriority[e.name] {
			continue
		}

		if g, exists := groups[e.name]; exists {
			g.count++
			// Keep the entry with the higher-priority kind if kinds differ.
			if highKinds[e.kind] && !highKinds[g.first.kind] {
				g.first = e
			}
		} else {
			groups[e.name] = &group{first: e, count: 1}
			order = append(order, e.name)
		}
	}

	var symbols []Symbol
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

// bodyContainsFuncValue reports whether a node's text contains an arrow
// function or function expression value — used to skip variable declarations
// that are actually function aliases.
func bodyContainsFuncValue(node *sitter.Node) bool {
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		t := child.Type()
		if t == "arrow_function" || t == "function_expression" ||
			t == "function" || t == "generator_function" {
			return true
		}
		// Recurse one level into variable_declarator.
		if t == "variable_declarator" {
			for j := 0; j < int(child.ChildCount()); j++ {
				grandchild := child.Child(j)
				gt := grandchild.Type()
				if gt == "arrow_function" || gt == "function_expression" ||
					gt == "function" || gt == "generator_function" {
					return true
				}
			}
		}
	}
	return false
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

// chunkPlainText splits src into overlapping 40-line chunks for the LLM.
func chunkPlainText(src []byte) []Symbol {
	const chunkSize = 40
	const overlap = 5

	lines := strings.Split(string(src), "\n")
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
