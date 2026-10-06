// Semantic tokens provide editor colouring on top of the server's own
// knowledge of GML, so the user's Neovim (which enables LSP semantic tokens by
// default) can render comments, strings, keywords, built-ins and project
// symbols without needing a tree-sitter grammar for GML.
package main

import "strings"

// Semantic token types, in the exact order of semanticLegend (main.go's
// semanticTokensProvider). Standard LSP names where possible: Neovim links
// @lsp.type.<name> to the well-known treesitter highlight groups.
const (
	svKeyword = iota
	svComment
	svString
	svNumber
	svOperator
	svFunction
	svMethod
	svVariable
	svProperty
	svEnum
	svEnumMember
	svMacro
	svType
)

// semanticLegend must stay in sync with the sv* constants above.
var semanticLegend = []string{
	"keyword", "comment", "string", "number", "operator",
	"function", "method", "variable", "property", "enum", "enumMember", "macro", "type",
}

type semTok struct {
	line, start, length, typ int
}

// semanticTokens tokenizes one whole buffer and returns the delta-encoded data
// array of textDocument/semanticTokens (five ints per token: deltaLine,
// deltaStartChar, length, tokenType, tokenModifiers). Positions are UTF-16 code
// units as the protocol requires, and tokens come out in source order.
func (ix *Index) semanticTokens(text string) []int {
	lines := strings.Split(text, "\n")
	var toks []semTok
	st := &scanState{}
	for li, raw := range lines {
		toks = append(toks, scanLine(ix, strings.TrimSuffix(raw, "\r"), li, st)...)
	}
	out := make([]int, 0, len(toks)*5)
	pl, ps := 0, 0
	for _, t := range toks {
		l := lines[t.line]
		start := utf16Len(l[:t.start])
		length := utf16Len(l[t.start : t.start+t.length])
		dl := t.line - pl
		ds := start
		if dl == 0 {
			ds = start - ps
		}
		out = append(out, dl, ds, length, t.typ, 0)
		pl, ps = t.line, start
	}
	return out
}

// scanState carries syntax context across the lines of one buffer.
type scanState struct {
	inBlock  bool // inside a /* */ comment
	inEnum   bool // inside an enum body: bare identifiers are members
	enumNest int  // brace depth of the enum body
	pending  bool // 'enum' keyword seen, next '{' opens the body
}

// scanLine emits the semantic tokens for one line. st carries the block-comment
// and enum-body state across lines.
func scanLine(ix *Index, l string, li int, st *scanState) []semTok {
	var out []semTok
	n := len(l)
	if st.inBlock {
		if k := strings.Index(l, "*/"); k >= 0 {
			out = append(out, semTok{li, 0, k + 2, svComment})
			i := k + 2
			st.inBlock = false
			out = append(out, scanCode(ix, l, li, i, st)...)
			return out
		}
		if n > 0 {
			out = append(out, semTok{li, 0, n, svComment})
		}
		return out
	}
	return scanCode(ix, l, li, 0, st)
}

// scanCode scans the code part of one line starting at column i.
func scanCode(ix *Index, l string, li, i int, st *scanState) []semTok {
	var out []semTok
	n := len(l)
	for i < n {
		c := l[i]
		switch {
		case c == ' ' || c == '\t':
			i++
		case c == '/' && i+1 < n && l[i+1] == '/':
			out = append(out, semTok{li, i, n - i, svComment})
			return out
		case c == '/' && i+1 < n && l[i+1] == '*':
			if k := strings.Index(l[i:], "*/"); k >= 0 {
				out = append(out, semTok{li, i, k + 2, svComment})
				i += k + 2
			} else {
				out = append(out, semTok{li, i, n - i, svComment})
				st.inBlock = true
				return out
			}
		case c == '"' || c == '\'' || (c == '@' && i+1 < n && l[i+1] == '"'):
			end, _ := scanString(l, i)
			out = append(out, semTok{li, i, end - i, svString})
			i = end
		case c >= '0' && c <= '9' || c == '$':
			j := scanNumber(l, i)
			out = append(out, semTok{li, i, j - i, svNumber})
			i = j
		case c == '#':
			// preprocessor directive, e.g. #region, #macro NAME
			j := i + 1
			for j < n && isIdent(l[j]) {
				j++
			}
			out = append(out, semTok{li, i, j - i, svKeyword})
			if j > i+1 && (l[i+1:j] == "macro" || l[i+1:j] == "define") {
				k := j
				for k < n && (l[k] == ' ' || l[k] == '\t') {
					k++
				}
				s := k
				for k < n && isIdent(l[k]) {
					k++
				}
				if s < k {
					out = append(out, semTok{li, s, k - s, svMacro})
				}
				j = k
			}
			i = j
		case isIdent(c):
			s := i
			for i < n && isIdent(l[i]) {
				i++
			}
			w := l[s:i]
			qual := ""
			if s > 0 && l[s-1] == '.' {
				q := s - 1
				for q > 0 && isIdent(l[q-1]) {
					q--
				}
				qual = l[q : s-1]
			}
			next := byte(' ')
			if i < n {
				next = l[i]
			}
			typ := ix.classify(w, qual, next)
			if typ == svKeyword && w == "enum" {
				st.pending = true
			}
			if st.inEnum && qual == "" {
				typ = svEnumMember
			}
			out = append(out, semTok{li, s, i - s, typ})
		default:
			if c == '{' {
				if st.pending {
					st.inEnum, st.enumNest, st.pending = true, 1, false
				} else if st.inEnum {
					st.enumNest++
				}
				out = append(out, semTok{li, i, 1, svOperator})
				i++
			} else if c == '}' {
				if st.inEnum {
					st.enumNest--
					if st.enumNest <= 0 {
						st.inEnum = false
					}
				}
				out = append(out, semTok{li, i, 1, svOperator})
				i++
			} else if c == ';' {
				st.pending = false
				out = append(out, semTok{li, i, 1, svOperator})
				i++
			} else if op := opLen(l, i); op > 0 {
				out = append(out, semTok{li, i, op, svOperator})
				i += op
			} else {
				i++
			}
		}
	}
	return out
}

// classify decides the semantic type of identifier w. qual is the word written
// just before a leading dot (empty when there is none); next is the byte
// immediately after w.
func (ix *Index) classify(w, qual string, next byte) int {
	if qual != "" {
		if _, ok := ix.enums[qual]; ok {
			return svEnumMember
		}
		if qual == "global" || qual == "self" || qual == "other" {
			return svVariable
		}
		if next == '(' {
			return svMethod
		}
		return svProperty
	}
	if isKeyword(w) {
		return svKeyword
	}
	if next == '(' {
		return svFunction
	}
	if b, ok := builtins[w]; ok {
		if b.Kind == ckVariable || b.Kind == ckConstant {
			return svVariable
		}
		return svFunction
	}
	if _, ok := ix.globals[w]; ok {
		return svVariable
	}
	if _, ok := ix.assets[w]; ok {
		return svType
	}
	if _, ok := ix.enums[w]; ok {
		return svEnum
	}
	return svVariable
}

// scanString returns the index just past the string literal starting at i
// (a bare "..." or '...' or a GML verbatim @"...").
func scanString(l string, i int) (int, bool) {
	verbatim := false
	if l[i] == '@' {
		verbatim = true
		i++
	}
	q := l[i]
	j := i + 1
	n := len(l)
	for j < n {
		if verbatim && q == '"' && l[j] == '"' && j+1 < n && l[j+1] == '"' {
			j += 2 // "" inside @"..." is an escaped quote
			continue
		}
		if !verbatim && l[j] == '\\' && j+1 < n {
			j += 2
			continue
		}
		if l[j] == q {
			return j + 1, verbatim
		}
		j++
	}
	return n, verbatim
}

// scanNumber returns the index just past the number starting at i, supporting
// decimal literals (with an optional fractional part) and GML hex `$`.
func scanNumber(l string, i int) int {
	n := len(l)
	if l[i] == '$' {
		j := i + 1
		for j < n && isHex(l[j]) {
			j++
		}
		return j
	}
	j := i
	for j < n && l[j] >= '0' && l[j] <= '9' {
		j++
	}
	if j+1 < n && l[j] == '.' && l[j+1] >= '0' && l[j+1] <= '9' {
		j++
		for j < n && l[j] >= '0' && l[j] <= '9' {
			j++
		}
	}
	return j
}

func isHex(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F'
}

// opLen returns the length of the operator token starting at l[i], or 0 when
// l[i] is not an operator.
func opLen(l string, i int) int {
	n := len(l)
	if i >= n {
		return 0
	}
	switch l[i] {
	case '+', '-', '*', '/', '%', '=', '!', '<', '>', '&', '|', '^', '~',
		'?', ':', '.', ',', ';', '(', ')', '[', ']', '{', '}':
	default:
		return 0
	}
	if i+1 < n {
		switch l[i : i+2] {
		case "==", "!=", "<=", ">=", "&&", "||", "+=", "-=", "*=", "/=", "%=",
			"<<", ">>", "&=", "|=", "^=", "++", "--", "??":
			return 2
		}
	}
	return 1
}
