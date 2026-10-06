package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Diagnostic is an LSP publishDiagnostics entry.
type Diagnostic struct {
	Range    Range  `json:"range"`
	Severity int    `json:"severity"`
	Code     string `json:"code"`
	Source   string `json:"source"`
	Message  string `json:"message"`
}

const (
	diagWarning = 2 // LSP DiagnosticSeverity.Warning
	diagCode    = "unknown-function"
)

var (
	reNameString  = regexp.MustCompile(`"name"\s*:\s*"([^"]+)"`)
	reLocalVar    = regexp.MustCompile(`\bvar\s+([A-Za-z_]\w*(?:\s*,\s*[A-Za-z_]\w*)*)`)
	reLocalStatic = regexp.MustCompile(`\bstatic\s+([A-Za-z_]\w*)`)
	reLocalAssign = regexp.MustCompile(`\b([A-Za-z_]\w*)\s*=`)
	reLocalCatch  = regexp.MustCompile(`\bcatch\s*\(([^()]*)\)`)
)

// indexExtensions records quoted names in extensions/*/*.yy so extension
// functions (defined in .yy rather than .gml) count as known. Being
// over-permissive here only means fewer warnings.
func (ix *Index) indexExtensions(root string) {
	ix.extensionFns = map[string]bool{}
	extDir := filepath.Join(root, "extensions")
	entries, err := os.ReadDir(extDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		filepath.WalkDir(filepath.Join(extDir, e.Name()), func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() || !strings.HasSuffix(p, ".yy") {
				return nil
			}
			if b, err := os.ReadFile(p); err == nil {
				for _, m := range reNameString.FindAllStringSubmatch(string(b), -1) {
					ix.extensionFns[m[1]] = true
				}
			}
			return nil
		})
	}
}

// knownSet builds the set of names that may legitimately be called: project
// symbols, assets, globals, extension functions, spec built-ins and keywords.
func (ix *Index) knownSet() map[string]bool {
	k := map[string]bool{}
	for _, syms := range ix.files {
		for _, s := range syms {
			k[s.Name] = true
		}
	}
	for _, a := range ix.assets {
		k[a.Name] = true
	}
	for n := range ix.globals {
		k[n] = true
	}
	for n := range ix.extensionFns {
		k[n] = true
	}
	for n := range builtins {
		k[n] = true
	}
	for _, kw := range keywords {
		k[kw] = true
	}
	return k
}

// fnParams returns parameter names collected from every "function" keyword on
// the (stripped) line, handling both `function foo(a, b)` and `= function(a)`.
func fnParams(line string) map[string]bool {
	out := map[string]bool{}
	for from := 0; ; {
		idx := strings.Index(line[from:], "function")
		if idx < 0 {
			return out
		}
		f := from + idx
		from = f + len("function")
		if from < len(line) && isIdent(line[from]) {
			continue // "functionality" and friends
		}
		open := strings.IndexByte(line[from:], '(')
		if open < 0 {
			return out
		}
		open += from
		if ps, ok := scanParams(line, open); ok {
			for _, p := range ps {
				if n, _ := leadingIdent(p); n != "" {
					out[n] = true
				}
			}
		}
		from = open + 1
	}
}

// locals returns the names this file may call without being declared
// functions: var/static declarations, function parameters, catch parameters,
// and any assignment target. Conservative on purpose, since callbacks are
// called like functions.
func (ix *Index) locals(path, clean string) map[string]bool {
	loc := map[string]bool{}
	for _, line := range strings.Split(clean, "\n") {
		line = strings.TrimSuffix(line, "\r")
		for _, m := range reLocalVar.FindAllStringSubmatchIndex(line, -1) {
			for _, n := range strings.Split(line[m[2]:m[3]], ",") {
				if nn := strings.TrimSpace(n); nn != "" {
					loc[nn] = true
				}
			}
		}
		for _, m := range reLocalStatic.FindAllStringSubmatchIndex(line, -1) {
			name := line[m[2]:m[3]]
			if name == "function" {
				continue // static function decl; the name is indexed separately
			}
			rest := strings.TrimLeft(line[m[3]:], " \t")
			if strings.HasPrefix(rest, "=") || strings.HasPrefix(rest, ";") ||
				strings.HasPrefix(rest, ",") || rest == "" {
				loc[name] = true
			}
		}
		for n := range fnParams(line) {
			loc[n] = true
		}
		if m := reLocalCatch.FindStringSubmatch(line); m != nil {
			for _, p := range strings.Split(m[1], ",") {
				if n, _ := leadingIdent(p); n != "" {
					loc[n] = true
				}
			}
		}
		for _, m := range reLocalAssign.FindAllStringSubmatchIndex(line, -1) {
			if m[1] < len(line) && line[m[1]] == '=' {
				continue // == comparison, not an assignment
			}
			loc[line[m[2]:m[3]]] = true
		}
	}
	return loc
}

// unknownCall reports whether the call `name(` whose identifier starts at byte
// offset abs in clean should be flagged as unknown.
func (ix *Index) unknownCall(clean string, abs int, name string, known, loc map[string]bool) bool {
	if len(name) == 0 || !isIdent(name[0]) {
		return false
	}
	// member calls (obj.method(), global.foo()) — struct contents are unknown
	if abs > 0 && clean[abs-1] == '.' {
		return false
	}
	// `function NAME(` is a declaration, not a call
	if isDeclaration(clean, abs) {
		return false
	}
	if ignoreSet[name] {
		return false
	}
	if known[name] {
		return false
	}
	if loc[name] {
		return false
	}
	return true
}

// checkFile finds calls to functions that exist nowhere: not a project symbol,
// asset, global, extension, built-in, keyword, or a local of this file.
func (ix *Index) checkFile(path, text string, known map[string]bool) []Diagnostic {
	start := time.Now()
	clean := stripNonCode(text)
	loc := ix.locals(path, clean)
	var out []Diagnostic
	mlines := strings.Split(clean, "\n")
	lineStart := 0
	for i, mline := range mlines {
		mline = strings.TrimSuffix(mline, "\r")
		for s := 0; s < len(mline); s++ {
			if !isIdent(mline[s]) {
				continue
			}
			e := s
			for e < len(mline) && isIdent(mline[e]) {
				e++
			}
			name := mline[s:e]
			k := e
			for k < len(mline) && (mline[k] == ' ' || mline[k] == '\t') {
				k++
			}
			if k >= len(mline) || mline[k] != '(' {
				s = e
				continue
			}
			if ix.unknownCall(clean, lineStart+s, name, known, loc) {
				col := utf16Len(mline[:s])
				out = append(out, Diagnostic{
					Range: Range{
						Start: Position{i, col},
						End:   Position{i, col + utf16Len(name)},
					},
					Severity: diagWarning,
					Code:     diagCode,
					Source:   "gmlls",
					Message:  "unknown function: " + name,
				})
			}
			s = e
		}
		lineStart += len(mlines[i]) + 1
	}
	if d := time.Since(start); d > 10*time.Millisecond {
		logf("checked %s in %v (%d diagnostics)", path, d, len(out))
	}
	return out
}

// publishBuffers sends unknown-function diagnostics for every open .gml buffer
// using one shared known set (a rescan or edit must not rebuild it per file).
// Called from the main goroutine only.
func publishBuffers(known map[string]bool) {
	if !diagEnabled || serverOut == nil {
		return
	}
	for p := range docs {
		if !isGML(p) {
			continue
		}
		diags := index.checkFile(p, textOf(p), known)
		if diags == nil {
			diags = []Diagnostic{}
		}
		send(serverOut, map[string]any{
			"jsonrpc": "2.0",
			"method":  "textDocument/publishDiagnostics",
			"params": map[string]any{
				"uri":         pathToURI(p),
				"diagnostics": diags,
			},
		})
	}
}

// clearDiagnostics empties the diagnostics for a closed file so the editor
// does not keep stale warnings on screen.
func clearDiagnostics(path string) {
	if !diagEnabled || serverOut == nil {
		return
	}
	send(serverOut, map[string]any{
		"jsonrpc": "2.0",
		"method":  "textDocument/publishDiagnostics",
		"params": map[string]any{
			"uri":         pathToURI(path),
			"diagnostics": []Diagnostic{},
		},
	})
}

// scheduleDiagnostics (re)arms the debounce timer that triggers a re-check
// after a burst of edits, so warnings do not appear mid-word.
func scheduleDiagnostics() {
	if !diagEnabled {
		return
	}
	if diagTimer != nil {
		diagTimer.Stop()
	}
	diagTimer = time.NewTimer(300 * time.Millisecond)
	diagCh = diagTimer.C
}

// runCheck is the `gmlls -check <root>` CLI mode: report every unknown
// function in the project as file:line:col message. Used to tune the checker
// against a real project without an editor in the loop.
func runCheck(root, spec string) int {
	if spec != "" {
		if n, err := loadSpec(spec); err != nil {
			logf("could not read spec %s: %v", spec, err)
		} else {
			logf("loaded %d built-ins", n)
		}
	} else {
		logf("no spec configured: the check will flag every name it cannot resolve")
	}
	dir := root
	if strings.HasPrefix(root, "file://") {
		dir = uriToPath(root)
	}
	index.indexRoot(dir)
	known := index.knownSet()
	total := 0
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
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
			if b, err := os.ReadFile(p); err == nil {
				for _, diag := range index.checkFile(p, string(b), known) {
					fmt.Printf("%s:%d:%d %s\n",
						p, diag.Range.Start.Line+1, diag.Range.Start.Character+1, diag.Message)
					total++
				}
			}
		}
		return nil
	})
	fmt.Fprintf(os.Stderr, "%d potential unknown functions\n", total)
	return 0
}
