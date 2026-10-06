package main

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// LSP SymbolKind values we use.
const (
	kindModule   = 2
	kindClass    = 5
	kindMethod   = 6
	kindEnum     = 10
	kindFunction = 12
	kindConstant = 14
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
	Detail    string // e.g. "move(dx, dy)" or a macro's value
	Doc       string // text of /// comments directly above
}

type Index struct {
	files  map[string][]Sym // .gml path -> symbols declared in it
	assets map[string]Sym   // asset name -> asset
}

func newIndex() *Index {
	return &Index{files: map[string][]Sym{}, assets: map[string]Sym{}}
}

var (
	reFunc       = regexp.MustCompile(`^\s*function\s+([A-Za-z_]\w*)\s*\(`)
	reAssignFunc = regexp.MustCompile(`^\s*(?:static\s+)?([A-Za-z_]\w*)\s*=\s*function\s*\(`)
	reMacro      = regexp.MustCompile(`^\s*#macro\s+([A-Za-z_]\w*)`)
	reEnum       = regexp.MustCompile(`^\s*enum\s+([A-Za-z_]\w*)`)
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

func detailOf(kind int, name, rest string) string {
	switch kind {
	case kindFunction, kindMethod:
		i := strings.Index(rest, "(")
		if i < 0 {
			return name + "()"
		}
		after := rest[i+1:]
		if j := strings.Index(after, ")"); j >= 0 {
			after = after[:j]
		}
		return name + "(" + strings.TrimSpace(after) + ")"
	case kindConstant:
		return strings.TrimSpace(rest)
	}
	return ""
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
	out := []Sym{}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		line = strings.TrimSuffix(line, "\r")
		for _, d := range declRes {
			if m := d.re.FindStringSubmatchIndex(line); m != nil {
				name := line[m[2]:m[3]]
				start := utf16Len(line[:m[2]])
				out = append(out, Sym{
					Name: name, Kind: d.kind, Path: path,
					Line: i, Start: start, End: start + utf16Len(name),
					Detail: detailOf(d.kind, name, line[m[3]:]),
					Doc:    docAbove(lines, i),
				})
				break
			}
		}
	}
	return out
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
		delete(ix.files, path)
		return
	}
	ix.files[path] = parseSymbols(path, string(b))
}

func (ix *Index) indexText(path, text string) {
	ix.files[path] = parseSymbols(path, text)
}

// rescan re-reads project files that changed on disk since the last scan.
// Called on the rescan ticker; currently a no-op placeholder.
func rescan() {}

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
	lines := strings.Split(text, "\n")
	if line < 0 || line >= len(lines) {
		return ""
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
	return l[s:e]
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
