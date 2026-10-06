package main

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// LSP SymbolKind values we use.
const (
	kindModule     = 2
	kindClass      = 5
	kindMethod     = 6
	kindEnum       = 10
	kindFunction   = 12
	kindConstant   = 14
	kindEnumMember = 22
)

// Sym is a named thing with a location. Columns are UTF-16 code units (LSP).
type Sym struct {
	Name      string
	Container string // e.g. "sprite", "object"; empty for code symbols
	Path      string
	Kind      int
	Line      int
	Start     int
	End       int
	Detail    string   // e.g. "move(dx, dy)" or a macro's value
	Params    []string // parameter names, for functions and methods
	Doc       string   // text of /// comments directly above
}

type Index struct {
	files   map[string][]Sym // .gml path -> symbols declared in it
	assets  map[string]Sym   // asset name -> asset
	enums   map[string][]Sym // enum name -> member symbols
	globals map[string]Sym   // global.name -> symbol (first definition wins)
}

func newIndex() *Index {
	return &Index{
		files:   map[string][]Sym{},
		assets:  map[string]Sym{},
		enums:   map[string][]Sym{},
		globals: map[string]Sym{},
	}
}

var (
	reFunc       = regexp.MustCompile(`^\s*function\s+([A-Za-z_]\w*)\s*\(`)
	reAssignFunc = regexp.MustCompile(`^\s*(?:static\s+)?([A-Za-z_]\w*)\s*=\s*function\s*\(`)
	reMacro      = regexp.MustCompile(`^\s*#macro\s+([A-Za-z_]\w*)`)
	reEnum       = regexp.MustCompile(`^\s*enum\s+([A-Za-z_]\w*)`)
	reGlobalSet  = regexp.MustCompile(`\bglobal\.([A-Za-z_]\w*)\s*(?:[-+*/%|&^]?=)`)
	reGlobalVar  = regexp.MustCompile(`\bglobalvar\s+((?:[A-Za-z_]\w*\s*,\s*)*[A-Za-z_]\w*)\s*;`)
)

var declRes = []struct {
	re   *regexp.Regexp
	kind int
}{
	{reFunc, kindFunction},
	{reAssignFunc, kindMethod},
	{reMacro, kindConstant},
	{reEnum, kindEnum},
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// utf16ToByte converts a UTF-16 column into a byte offset within s.
func utf16ToByte(s string, col int) int {
	n := 0
	for i, r := range s {
		if n >= col {
			return i
		}
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return len(s)
}

// docAbove collects consecutive /// lines directly above line i.
func docAbove(lines []string, i int) string {
	var doc []string
	for j := i - 1; j >= 0; j-- {
		t := strings.TrimSpace(strings.TrimSuffix(lines[j], "\r"))
		if !strings.HasPrefix(t, "///") {
			break
		}
		doc = append([]string{strings.TrimSpace(strings.TrimPrefix(t, "///"))}, doc...)
	}
	return strings.Join(doc, "\n")
}

func parseSymbols(path, text string) []Sym {
	clean := stripNonCode(text) // declarations must be real code, not comments/strings
	clines := strings.Split(clean, "\n")
	lines := strings.Split(text, "\n")
	out := []Sym{}
	lineStart := 0
	for i, cline := range clines {
		cline = strings.TrimSuffix(cline, "\r")
		for _, d := range declRes {
			if m := d.re.FindStringSubmatchIndex(cline); m != nil {
				name := cline[m[2]:m[3]]
				start := utf16Len(cline[:m[2]])
				params := paramList(clean, lineStart+m[3])
				det := name + "(" + strings.Join(params, ", ") + ")"
				if d.kind == kindConstant {
					det = strings.TrimSpace(cline[m[3]:])
				}
				out = append(out, Sym{
					Name: name, Kind: d.kind, Path: path,
					Line: i, Start: start, End: start + utf16Len(name),
					Detail: det, Params: params,
					Doc: docAbove(lines, i),
				})
				break
			}
		}
		lineStart += len(clines[i]) + 1
	}
	return out
}

// paramList returns the parameter list of the call or declaration whose name
// ends at byte offset end in clean (signatures start on the same line).
func paramList(clean string, end int) []string {
	for i := end; i < len(clean) && clean[i] != '\n'; i++ {
		if clean[i] == '(' {
			if ps, ok := scanParams(clean, i); ok {
				return ps
			}
			return nil
		}
	}
	return nil
}

// scanParams returns the top-level comma-separated pieces between the '(' at
// byte offset open and its matching ')' in clean.
func scanParams(clean string, open int) ([]string, bool) {
	depth := 0
	for i := open; i < len(clean); i++ {
		switch clean[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return splitTopLevel(clean[open+1:i], ','), true
			}
		}
	}
	return nil, false
}

// splitTopLevel splits s on sep, ignoring separators nested inside (), [] or {}.
// Defaults like a = max(1, 2) survive because the split is depth-aware.
func splitTopLevel(s string, sep byte) []string {
	var out []string
	d := 0
	last := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(', '[', '{':
			d++
		case ')', ']', '}':
			d--
		case sep:
			if d == 0 {
				out = append(out, strings.TrimSpace(s[last:i]))
				last = i + 1
			}
		}
	}
	out = append(out, strings.TrimSpace(s[last:]))
	res := out[:0]
	for _, p := range out {
		if p != "" {
			res = append(res, p)
		}
	}
	return res
}

// posToOffset converts an LSP position (line, UTF-16 column) into a byte
// offset into the whole text.
func posToOffset(text string, p Position) int {
	lines := strings.Split(text, "\n")
	if p.Line < 0 || p.Line >= len(lines) {
		return len(text)
	}
	off := 0
	for i := 0; i < p.Line; i++ {
		off += len(lines[i]) + 1
	}
	l := strings.TrimSuffix(lines[p.Line], "\r")
	return off + utf16ToByte(l, p.Character)
}

// GameMaker keeps each asset in <type-dir>/<name>/.
var assetDirs = map[string]string{
	"objects": "object", "sprites": "sprite", "sounds": "sound", "rooms": "room",
	"scripts": "script", "shaders": "shader", "fonts": "font", "tilesets": "tileset",
	"sequences": "sequence", "timelines": "timeline", "paths": "path",
	"animcurves": "animcurve", "extensions": "extension", "notes": "note",
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func (ix *Index) indexAssets(root string) {
	for dir, kindName := range assetDirs {
		entries, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			base := filepath.Join(root, dir, name)
			target := base
			switch {
			case kindName == "script" && exists(filepath.Join(base, name+".gml")):
				target = filepath.Join(base, name+".gml")
			case exists(filepath.Join(base, name+".yy")):
				target = filepath.Join(base, name+".yy")
			}
			kind := kindConstant
			switch kindName {
			case "object":
				kind = kindClass
			case "script":
				kind = kindModule
			}
			ix.assets[name] = Sym{
				Name: name, Container: kindName, Path: target, Kind: kind,
				End: utf16Len(name),
			}
		}
	}
}

func (ix *Index) indexFile(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		ix.dropFile(path)
		return
	}
	ix.indexText(path, string(b))
}

func (ix *Index) indexText(path, text string) {
	ix.clearSymbols(path)
	ix.files[path] = parseSymbols(path, text)
	ix.enumsFrom(path, text)
	ix.globalsFrom(path, text)
}

// clearSymbols removes enums and globals whose owning file is path, so
// re-indexing a file never leaves stale members behind.
func (ix *Index) clearSymbols(path string) {
	for name, members := range ix.enums {
		var keep []Sym
		for _, m := range members {
			if m.Path != path {
				keep = append(keep, m)
			}
		}
		if len(keep) == 0 {
			delete(ix.enums, name)
		} else {
			ix.enums[name] = keep
		}
	}
	for name, g := range ix.globals {
		if g.Path == path {
			delete(ix.globals, name)
		}
	}
}

// offsetToPos converts a byte offset within s into a 0-based line and a
// UTF-16 column.
func offsetToPos(s string, off int) (int, int) {
	line := 0
	idx := 0
	for i := 0; i < len(s) && i < off; i++ {
		if s[i] == '\n' {
			line++
			idx = i + 1
		}
	}
	return line, utf16Len(s[idx:off])
}

// leadingIdent returns the first identifier in piece and its byte offset.
func leadingIdent(piece string) (string, int) {
	i := 0
	for i < len(piece) && (piece[i] == ' ' || piece[i] == '\t' || piece[i] == '\n' || piece[i] == '\r') {
		i++
	}
	s := i
	for i < len(piece) && isIdent(piece[i]) {
		i++
	}
	if s == i {
		return "", 0
	}
	return piece[s:i], s
}

// span is a half-open range within a string.
type span struct{ start, end int }

// topLevelSpans returns the spans of s split on sep, ignoring separators
// nested inside (), [] or {}, with offsets into the original s.
func topLevelSpans(s string, sep byte) []span {
	var out []span
	d := 0
	last := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(', '[', '{':
			d++
		case ')', ']', '}':
			d--
		case sep:
			if d == 0 {
				out = append(out, span{last, i})
				last = i + 1
			}
		}
	}
	return append(out, span{last, len(s)})
}

// enumsFrom stores the members of every enum declared in text (which may span
// many lines) on ix.enums, with each member's real location.
func (ix *Index) enumsFrom(path, text string) {
	clean := stripNonCode(text)
	clines := strings.Split(clean, "\n")
	lineStart := 0
	for i, cline := range clines {
		clineStr := strings.TrimSuffix(cline, "\r")
		m := reEnum.FindStringSubmatchIndex(clineStr)
		if m == nil {
			lineStart += len(clines[i]) + 1
			continue
		}
		// find the '{' that starts the body, within a small window after the name
		open := -1
		limit := lineStart + m[3] + 200
		if limit > len(clean) {
			limit = len(clean)
		}
		for k := lineStart + m[3]; k < limit; k++ {
			if clean[k] == '{' {
				open = k
				break
			}
		}
		if open < 0 {
			lineStart += len(clines[i]) + 1
			continue
		}
		if bodyStart, body, ok := enumBody(clean, open); ok {
			for _, sp := range topLevelSpans(body, ',') {
				member, off := leadingIdent(body[sp.start:sp.end])
				if member == "" {
					continue
				}
				l, c := offsetToPos(clean, bodyStart+sp.start+off)
				ix.enums[clineStr[m[2]:m[3]]] = append(ix.enums[clineStr[m[2]:m[3]]], Sym{
					Name: member, Container: clineStr[m[2]:m[3]], Path: path,
					Kind: kindEnumMember, Line: l, Start: c, End: c + utf16Len(member),
				})
			}
		}
		lineStart += len(clines[i]) + 1
	}
}

// enumBody returns the body inside the braces starting at open, with its byte
// offset in clean.
func enumBody(clean string, open int) (int, string, bool) {
	d := 0
	for i := open; i < len(clean); i++ {
		switch clean[i] {
		case '{':
			d++
		case '}':
			d--
			if d == 0 {
				return open + 1, clean[open+1 : i], true
			}
		}
	}
	return 0, "", false
}

// globalsFrom records global.name assignments and globalvar declarations.
func (ix *Index) globalsFrom(path, text string) {
	clines := strings.Split(stripNonCode(text), "\n")
	for i, line := range clines {
		line = strings.TrimSuffix(line, "\r")
		for _, m := range reGlobalSet.FindAllStringSubmatchIndex(line, -1) {
			if m[1] < len(line) && line[m[1]] == '=' {
				continue // == comparison, not an assignment
			}
			ix.addGlobal(path, line[m[2]:m[3]], i, utf16Len(line[:m[2]]))
		}
		if m := reGlobalVar.FindStringSubmatchIndex(line); m != nil {
			list := line[m[2]:m[3]]
			for _, n := range strings.Split(list, ",") {
				n = strings.TrimSpace(n)
				if n == "" {
					continue
				}
				if idx := strings.Index(list, n); idx >= 0 {
					ix.addGlobal(path, n, i, utf16Len(line[:m[2]+idx]))
				}
			}
		}
	}
}

func (ix *Index) addGlobal(path, name string, line, col int) {
	if _, ok := ix.globals[name]; ok {
		return // first definition wins
	}
	ix.globals[name] = Sym{
		Name: name, Container: "global", Path: path, Kind: kindConstant,
		Line: line, Start: col, End: col + utf16Len(name),
	}
}

// stamp records enough of a file's state to detect changes on disk.
type stamp struct {
	mod  time.Time
	size int64
}

// seen is the last scan's snapshot of every .gml file's stamp.
var seen = map[string]stamp{}

// stampGMLs walks root and returns a stamp for every .gml file, skipping
// directories that are never part of the project.
func stampGMLs(root string) map[string]stamp {
	now := map[string]stamp{}
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "datafiles", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".gml") {
			if info, err := d.Info(); err == nil {
				now[p] = stamp{info.ModTime(), info.Size()}
			}
		}
		return nil
	})
	return now
}

// rescan re-indexes .gml files whose stamp changed since the last scan,
// drops files that disappeared, and refreshes the asset listings. Files open
// in the editor are skipped: their buffers are the source of truth until
// didClose re-reads from disk.
func rescan() {
	if projectRoot == "" {
		return
	}
	start := time.Now()
	root := uriToPath(projectRoot)
	now := stampGMLs(root)
	for p, s := range now {
		if seen[p] != s {
			if _, open := docs[p]; !open {
				index.indexFile(p)
			}
		}
	}
	for p := range seen {
		if _, ok := now[p]; !ok {
			index.dropFile(p)
		}
	}
	seen = now
	// Assets are cheap to rebuild: just directory listings.
	index.assets = map[string]Sym{}
	index.indexAssets(root)
	if d := time.Since(start); d > 50*time.Millisecond {
		logf("rescan took %v", d)
	}
}

// dropFile removes all indexed data for a path.
func (ix *Index) dropFile(path string) {
	delete(ix.files, path)
}

func (ix *Index) indexRoot(root string) {
	ix.indexAssets(root)
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "datafiles", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".gml") {
			ix.indexFile(p)
		}
		return nil
	})
}

func sortSyms(s []Sym) {
	sort.Slice(s, func(i, j int) bool {
		if s[i].Path != s[j].Path {
			return s[i].Path < s[j].Path
		}
		return s[i].Line < s[j].Line
	})
}

// lookupMember finds the named member of an enum, if it is defined in code
// (built-in spec enums have no source location to jump to).
func (ix *Index) lookupMember(qual, name string) *Sym {
	for i := range ix.enums[qual] {
		if ix.enums[qual][i].Name == name && ix.enums[qual][i].Path != "" {
			return &ix.enums[qual][i]
		}
	}
	return nil
}

// lookup finds declarations of name; falls back to assets.
func (ix *Index) lookup(name string) []Sym {
	var out []Sym
	for _, syms := range ix.files {
		for _, s := range syms {
			if s.Name == name {
				out = append(out, s)
			}
		}
	}
	if len(out) == 0 {
		if a, ok := ix.assets[name]; ok {
			out = append(out, a)
		}
	}
	sortSyms(out)
	return out
}

// search returns symbols whose name contains query (case-insensitive).
func (ix *Index) search(query string, limit int) []Sym {
	q := strings.ToLower(query)
	var out []Sym
	add := func(s Sym) {
		if q == "" || strings.Contains(strings.ToLower(s.Name), q) {
			out = append(out, s)
		}
	}
	for _, syms := range ix.files {
		for _, s := range syms {
			add(s)
		}
	}
	for _, a := range ix.assets {
		add(a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Path < out[j].Path
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func isIdent(b byte) bool {
	return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// wordAt returns the identifier under the cursor (UTF-16 column).
func wordAt(text string, line, col int) string {
	word, _ := qualifiedWordAt(text, line, col)
	return word
}

// qualifiedWordAt returns the identifier under the cursor plus, when it is
// written as part of a dotted access (State.Idle, global.score), the qualifier
// before the dot.
func qualifiedWordAt(text string, line, col int) (word, qual string) {
	lines := strings.Split(text, "\n")
	if line < 0 || line >= len(lines) {
		return "", ""
	}
	l := strings.TrimSuffix(lines[line], "\r")
	i := utf16ToByte(l, col)
	s, e := i, i
	for s > 0 && isIdent(l[s-1]) {
		s--
	}
	for e < len(l) && isIdent(l[e]) {
		e++
	}
	word = l[s:e]
	if s > 0 && l[s-1] == '.' {
		q := s - 1
		for q > 0 && isIdent(l[q-1]) {
			q--
		}
		qual = l[q : s-1]
	}
	return
}

func uriToPath(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return uri
	}
	return filepath.FromSlash(u.Path)
}

func pathToURI(p string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(p)}).String()
}
