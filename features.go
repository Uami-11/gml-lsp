package main

import (
	"encoding/xml"
	"fmt"
	"os"
	"sort"
	"strings"
)

// ---------- built-ins (loaded from the user's own GmlSpec.xml) ----------

// CompletionItemKind values.
const (
	ckMethod     = 2
	ckFunction   = 3
	ckVariable   = 6
	ckClass      = 7
	ckModule     = 9
	ckEnum       = 13
	ckKeyword    = 14
	ckEnumMember = 20
	ckConstant   = 21
)

type Builtin struct {
	Name   string
	Kind   int
	Sig    string
	Params []string
	Doc    string
}

var builtins = map[string]Builtin{}

var keywords = []string{
	"if", "else", "for", "while", "do", "until", "repeat", "switch", "case", "default",
	"break", "continue", "return", "exit", "with", "var", "globalvar", "static",
	"function", "constructor", "new", "delete", "enum", "try", "catch", "finally",
	"throw", "and", "or", "not", "xor", "mod", "div", "true", "false", "undefined",
	"self", "other", "all", "noone", "global", "begin", "end", "then",
}

func xmlAttr(se xml.StartElement, names ...string) string {
	for _, a := range se.Attr {
		for _, n := range names {
			if strings.EqualFold(a.Name.Local, n) {
				return a.Value
			}
		}
	}
	return ""
}

// loadSpec reads GameMaker's GmlSpec.xml tolerantly: any <Function>,
// <Constant> or <Variable> element with a Name attribute is taken, with
// <Parameter> children and <Description> text where present.
func loadSpec(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	dec := xml.NewDecoder(f)
	dec.Strict = false

	var cur *Builtin
	var params []string
	inDesc := false
	count := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch strings.ToLower(t.Name.Local) {
			case "function", "constant", "variable":
				name := xmlAttr(t, "name")
				if name == "" {
					continue
				}
				kind := ckFunction
				switch strings.ToLower(t.Name.Local) {
				case "constant":
					kind = ckConstant
				case "variable":
					kind = ckVariable
				}
				cur = &Builtin{Name: name, Kind: kind, Doc: xmlAttr(t, "description")}
				params = nil
			case "enumeration":
				// Built-in enums (e.g. AudioEffectType) complete after a dot
				// too, the same way project enums do.
				if name := xmlAttr(t, "name"); name != "" {
					cur = &Builtin{Name: name, Kind: ckEnum, Params: []string{}}
				}
			case "member":
				if cur != nil && cur.Kind == ckEnum {
					if name := xmlAttr(t, "name"); name != "" {
						index.enums[cur.Name] = append(index.enums[cur.Name], Sym{
							Name: name, Container: cur.Name, Kind: kindEnumMember,
							End: utf16Len(name),
						})
					}
				}
			case "parameter", "argument":
				if cur != nil {
					p := xmlAttr(t, "name")
					if strings.EqualFold(xmlAttr(t, "optional"), "true") {
						p += "?"
					}
					if strings.EqualFold(xmlAttr(t, "varargs"), "true") {
						p += "..."
					}
					params = append(params, p)
				}
			case "description":
				inDesc = cur != nil
			}
		case xml.CharData:
			if inDesc && cur != nil {
				cur.Doc += strings.TrimSpace(string(t)) + " "
			}
		case xml.EndElement:
			switch strings.ToLower(t.Name.Local) {
			case "description":
				inDesc = false
			case "function", "constant", "variable":
				if cur != nil {
					if cur.Kind == ckFunction {
						cur.Sig = cur.Name + "(" + strings.Join(params, ", ") + ")"
					}
					ps := params
					if ps == nil {
						ps = []string{}
					}
					cur.Params = ps
					cur.Doc = strings.TrimSpace(cur.Doc)
					builtins[cur.Name] = *cur
					count++
					cur = nil
				}
			}
		}
	}
	return count, nil
}

// ---------- comment/string stripping + references ----------

// stripNonCode blanks out comments and string literals, keeping every byte
// offset and newline in place so positions still line up with the original.
func stripNonCode(s string) string {
	const (
		code = iota
		lineC
		blockC
		dq
		sq
	)
	b := []byte(s)
	st := code
	blank := func(i int) {
		if b[i] != '\n' && b[i] != '\r' {
			b[i] = ' '
		}
	}
	for i := 0; i < len(b); i++ {
		c := b[i]
		var next byte
		if i+1 < len(b) {
			next = b[i+1]
		}
		switch st {
		case code:
			switch {
			case c == '/' && next == '/':
				st = lineC
				blank(i)
				blank(i + 1)
				i++
			case c == '/' && next == '*':
				st = blockC
				blank(i)
				blank(i + 1)
				i++
			case c == '"':
				st = dq
				blank(i)
			case c == '\'':
				st = sq
				blank(i)
			}
		case lineC:
			if c == '\n' {
				st = code
			} else {
				blank(i)
			}
		case blockC:
			if c == '*' && next == '/' {
				blank(i)
				blank(i + 1)
				i++
				st = code
			} else {
				blank(i)
			}
		case dq, sq:
			q := byte('"')
			if st == sq {
				q = '\''
			}
			switch {
			case c == '\\' && next != 0 && next != '\n':
				blank(i)
				blank(i + 1)
				i++
			case c == q:
				blank(i)
				st = code
			case c == '\n':
				st = code // unterminated string: recover at end of line
			default:
				blank(i)
			}
		}
	}
	return string(b)
}

// references finds whole-word uses of word in code (not comments/strings)
// across every indexed .gml file.
func (ix *Index) references(word string, includeDecl bool) []Location {
	decl := map[[3]any]bool{}
	if !includeDecl {
		for _, s := range ix.lookup(word) {
			decl[[3]any{s.Path, s.Line, s.Start}] = true
		}
	}
	paths := make([]string, 0, len(ix.files))
	for p := range ix.files {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var out []Location
	for _, p := range paths {
		text := textOf(p)
		orig := strings.Split(text, "\n")
		for i, sl := range strings.Split(stripNonCode(text), "\n") {
			for from := 0; ; {
				k := strings.Index(sl[from:], word)
				if k < 0 {
					break
				}
				b := from + k
				e := b + len(word)
				from = e
				if b > 0 && isIdent(sl[b-1]) || e < len(sl) && isIdent(sl[e]) {
					continue
				}
				start := utf16Len(orig[i][:b])
				if decl[[3]any{p, i, start}] {
					continue
				}
				out = append(out, Location{pathToURI(p), Range{
					Position{i, start}, Position{i, start + utf16Len(word)},
				}})
			}
		}
	}
	return out
}

// ---------- completion ----------

type MarkupContent struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}
type CompletionItem struct {
	Label         string         `json:"label"`
	Kind          int            `json:"kind"`
	Detail        string         `json:"detail,omitempty"`
	Documentation *MarkupContent `json:"documentation,omitempty"`
	SortText      string         `json:"sortText,omitempty"`
}
type CompletionList struct {
	IsIncomplete bool             `json:"isIncomplete"`
	Items        []CompletionItem `json:"items"`
}

// matchScore: 0 prefix, 1 substring, 2 subsequence, -1 no match.
func matchScore(name, prefix string) int {
	n, p := strings.ToLower(name), strings.ToLower(prefix)
	switch {
	case strings.HasPrefix(n, p):
		return 0
	case strings.Contains(n, p):
		return 1
	}
	j := 0
	for i := 0; i < len(n) && j < len(p); i++ {
		if n[i] == p[j] {
			j++
		}
	}
	if j == len(p) {
		return 2
	}
	return -1
}

func completionKind(s Sym) int {
	switch s.Kind {
	case kindFunction:
		return ckFunction
	case kindMethod:
		return ckMethod
	case kindEnum:
		return ckEnum
	case kindEnumMember:
		return ckEnumMember
	case kindClass:
		return ckClass
	case kindModule:
		return ckModule
	}
	return ckConstant
}

// completeQualified returns completion items for the qualifier written before a
// dot: the members of an enum, or the project's globals after "global.". Any
// other qualifier is unknown (struct fields and instance variables are not
// indexed), so nothing is returned. An empty prefix lists everything.
func (ix *Index) completeQualified(qual, prefix string) []CompletionItem {
	if members, ok := ix.enums[qual]; ok {
		var items []CompletionItem
		for _, m := range members {
			if matchScore(m.Name, prefix) < 0 {
				continue
			}
			items = append(items, CompletionItem{
				Label: m.Name, Kind: ckEnumMember, Detail: "enum " + qual,
			})
		}
		sort.Slice(items, func(i, j int) bool { return items[i].Label < items[j].Label })
		return items
	}
	if qual == "global" {
		var items []CompletionItem
		for _, g := range ix.globals {
			if matchScore(g.Name, prefix) < 0 {
				continue
			}
			items = append(items, CompletionItem{
				Label: g.Name, Kind: ckConstant, Detail: "global",
			})
		}
		sort.Slice(items, func(i, j int) bool { return items[i].Label < items[j].Label })
		return items
	}
	return nil
}

func (ix *Index) complete(prefix string, limit int) []CompletionItem {
	type cand struct {
		item  CompletionItem
		score int
		rank  int
	}
	seen := map[string]bool{}
	var cands []cand
	add := func(it CompletionItem, rank int) {
		if seen[it.Label] {
			return
		}
		if sc := matchScore(it.Label, prefix); sc >= 0 {
			seen[it.Label] = true
			cands = append(cands, cand{it, sc, rank})
		}
	}
	symItem := func(s Sym) CompletionItem {
		it := CompletionItem{Label: s.Name, Kind: completionKind(s), Detail: s.Detail}
		if s.Container != "" {
			it.Detail = s.Container
		}
		if s.Doc != "" {
			it.Documentation = &MarkupContent{"markdown", s.Doc}
		}
		return it
	}
	paths := make([]string, 0, len(ix.files))
	for p := range ix.files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		for _, s := range ix.files[p] {
			add(symItem(s), 0)
		}
	}
	for _, a := range ix.assets {
		add(symItem(a), 0)
	}
	for _, k := range keywords {
		add(CompletionItem{Label: k, Kind: ckKeyword}, 1)
	}
	for _, b := range builtins {
		it := CompletionItem{Label: b.Name, Kind: b.Kind, Detail: b.Sig}
		if b.Doc != "" {
			it.Documentation = &MarkupContent{"markdown", b.Doc}
		}
		add(it, 2)
	}
	sort.Slice(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.score != b.score {
			return a.score < b.score
		}
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		return a.item.Label < b.item.Label
	})
	if len(cands) > limit {
		cands = cands[:limit]
	}
	out := make([]CompletionItem, len(cands))
	for i, c := range cands {
		c.item.SortText = fmt.Sprintf("%04d", i)
		out[i] = c.item
	}
	return out
}

// ---------- signature help ----------

// callContext returns the name of the call enclosing offset off and the index
// of the parameter the cursor is inside (comma count at the same nesting
// depth), plus the byte offset of the callee identifier. ok is false when the
// cursor is not inside a call. The scan crosses newlines and is capped at
// 4000 bytes; run it on stripNonCode(text) so strings and comments don't
// confuse the nesting.
func callContext(code string, off int) (name string, arg, calleeStart int, ok bool) {
	depth := 0
	for i := off - 1; i >= 0 && off-i < 4000; i-- {
		switch code[i] {
		case ')', ']', '}':
			depth++
		case '[', '{':
			if depth == 0 {
				return "", 0, 0, false
			}
			depth--
		case '(':
			if depth > 0 {
				depth--
				continue
			}
			// unmatched '(' : the identifier before it is the callee
			j := i
			for j > 0 && (code[j-1] == ' ' || code[j-1] == '\t') {
				j--
			}
			e := j
			for j > 0 && isIdent(code[j-1]) {
				j--
			}
			return code[j:e], arg, j, j < e
		case ',':
			if depth == 0 {
				arg++
			}
		}
	}
	return "", 0, 0, false
}

func isKeyword(w string) bool {
	for _, k := range keywords {
		if k == w {
			return true
		}
	}
	return false
}

// isDeclaration reports whether the callee at nameStart is written as a
// `function NAME(` declaration rather than a call.
func isDeclaration(clean string, nameStart int) bool {
	i := nameStart - 1
	for i >= 0 && (clean[i] == ' ' || clean[i] == '\t') {
		i--
	}
	if i < 0 || !isIdent(clean[i]) {
		return false
	}
	e := i + 1
	for i >= 0 && isIdent(clean[i]) {
		i--
	}
	return clean[i+1:e] == "function"
}

// signature finds the parameter list for a callable name: a project function
// or method first, then a built-in function. Constructors work too, since
// `new Foo(` is just a call to Foo.
func (ix *Index) signature(name string) ([]string, string, bool) {
	for _, syms := range ix.files {
		for _, s := range syms {
			if s.Name == name && (s.Kind == kindFunction || s.Kind == kindMethod) {
				return s.Params, s.Detail, true
			}
		}
	}
	if b, ok := builtins[name]; ok && b.Kind == ckFunction {
		return b.Params, b.Sig, true
	}
	return nil, "", false
}

// ---------- hover ----------

func (ix *Index) hover(word string) string {
	if syms := ix.lookup(word); len(syms) > 0 {
		s := syms[0]
		var md string
		switch {
		case s.Container != "":
			return fmt.Sprintf("**%s** — %s asset", s.Name, s.Container)
		case s.Kind == kindFunction || s.Kind == kindMethod:
			md = "```gml\nfunction " + s.Detail + "\n```"
		case s.Kind == kindEnum:
			md = "```gml\nenum " + s.Name + "\n```"
		default:
			md = "```gml\n#macro " + s.Name + " " + s.Detail + "\n```"
		}
		if s.Doc != "" {
			md += "\n\n" + s.Doc
		}
		return md
	}
	if b, ok := builtins[word]; ok {
		sig := b.Sig
		if sig == "" {
			sig = b.Name
		}
		md := "```gml\n" + sig + "\n```"
		if b.Doc != "" {
			md += "\n\n" + b.Doc
		}
		return md
	}
	return ""
}
