// gmlls: a minimal GameMaker Language server (stdlib only).
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

type Request struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result"`
}

type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}
type DocumentSymbol struct {
	Name           string `json:"name"`
	Kind           int    `json:"kind"`
	Range          Range  `json:"range"`
	SelectionRange Range  `json:"selectionRange"`
}
type SymbolInformation struct {
	Name          string   `json:"name"`
	Kind          int      `json:"kind"`
	Location      Location `json:"location"`
	ContainerName string   `json:"containerName,omitempty"`
}

var (
	docs  = map[string]string{} // path -> text of open buffers
	index = newIndex()

	// projectRoot is the project directory, set at initialize.
	projectRoot string

	// tickCh fires every rescanSeconds after initialize (nil disables it).
	tickTimer *time.Ticker
	tickCh    <-chan time.Time

	// Diagnostics are pushed for open buffers; serverOut is only written by
	// the main goroutine (set at serve).
	serverOut   io.Writer
	diagEnabled = true
	ignoreSet   = map[string]bool{}
	diagTimer   *time.Timer
	diagCh      <-chan time.Time
)

func logf(f string, a ...any) { fmt.Fprintf(os.Stderr, "[gmlls] "+f+"\n", a...) }

func (s Sym) selRange() Range {
	return Range{Position{s.Line, s.Start}, Position{s.Line, s.End}}
}
func (s Sym) location() Location { return Location{pathToURI(s.Path), s.selRange()} }

func send(w io.Writer, v any) {
	b, _ := json.Marshal(v)
	fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(b), b)
}

func readMsg(r *bufio.Reader) ([]byte, error) {
	n := 0
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		if v, ok := strings.CutPrefix(line, "Content-Length:"); ok {
			n, _ = strconv.Atoi(strings.TrimSpace(v))
		}
	}
	buf := make([]byte, n)
	_, err := io.ReadFull(r, buf)
	return buf, err
}

// textOf returns the buffer text if open, else the file on disk.
func textOf(path string) string {
	if t, ok := docs[path]; ok {
		return t
	}
	b, _ := os.ReadFile(path)
	return string(b)
}

func isGML(path string) bool { return strings.HasSuffix(path, ".gml") }

func main() {
	if len(os.Args) >= 3 && os.Args[1] == "-check" {
		spec := os.Getenv("GMLLS_SPEC")
		if len(os.Args) >= 4 {
			spec = os.Args[3]
		}
		os.Exit(runCheck(os.Args[2], spec))
	}
	if err := serve(os.Stdin, os.Stdout); err != nil {
		logf("server error: %v", err)
		os.Exit(1)
	}
}

// serve runs the LSP event loop. All server state (docs, index, tickers) is
// touched only on this goroutine; the reader goroutine only feeds raw messages
// into the msgs channel.
func serve(in io.Reader, out io.Writer) error {
	serverOut = out
	msgs := make(chan []byte)
	go func() { // reader goroutine: only reads stdin
		br := bufio.NewReader(in)
		for {
			raw, err := readMsg(br)
			if err != nil {
				close(msgs)
				return
			}
			msgs <- raw
		}
	}()

	for {
		select {
		case raw, ok := <-msgs:
			if !ok {
				return nil // stdin closed
			}
			if quit := handle(raw, out); quit {
				return nil
			}
		case <-tickCh:
			rescan()
		case <-diagCh:
			diagCh = nil
			diagTimer = nil
			publishBuffers(index.knownSet())
		}
	}
}

// handle processes one request. It returns true only after "exit".
func handle(raw []byte, out io.Writer) bool {
	var req Request
	if json.Unmarshal(raw, &req) != nil {
		return false
	}
	reply := func(result any) { send(out, Response{"2.0", req.ID, result}) }

	switch req.Method {
	case "initialize":
		var p struct {
			RootURI               string `json:"rootUri"`
			InitializationOptions struct {
				GMLSpec       string   `json:"gmlSpec"`
				RescanSeconds *int     `json:"rescanSeconds"`
				Diagnostics   *bool    `json:"diagnostics"`
				Ignore        []string `json:"ignore"`
			} `json:"initializationOptions"`
			WorkspaceFolders []struct {
				URI string `json:"uri"`
			} `json:"workspaceFolders"`
		}
		json.Unmarshal(req.Params, &p)
		projectRoot = p.RootURI
		if len(p.WorkspaceFolders) > 0 {
			projectRoot = p.WorkspaceFolders[0].URI
		}
		reply(map[string]any{
			"capabilities": map[string]any{
				"textDocumentSync":        1, // full sync
				"documentSymbolProvider":  true,
				"definitionProvider":      true,
				"referencesProvider":      true,
				"hoverProvider":           true,
				"completionProvider":      map[string]any{"triggerCharacters": []string{"."}},
				"signatureHelpProvider":   map[string]any{"triggerCharacters": []string{"(", ","}, "retriggerCharacters": []string{","}},
				"workspaceSymbolProvider": true,
			},
			"serverInfo": map[string]string{"name": "gmlls", "version": "0.4.0"},
		})
		specPath := p.InitializationOptions.GMLSpec
		if specPath == "" {
			specPath = os.Getenv("GMLLS_SPEC")
		}
		if specPath != "" {
			if n, err := loadSpec(specPath); err != nil {
				logf("could not read spec %s: %v", specPath, err)
			} else {
				logf("loaded %d built-ins from %s", n, specPath)
			}
		} else {
			logf("no GmlSpec.xml configured: built-in completion/hover disabled")
		}
		// Rescan interval: init_options.rescanSeconds, default 2, 0 disables.
		secs := 2
		if p.InitializationOptions.RescanSeconds != nil {
			secs = *p.InitializationOptions.RescanSeconds
		}
		if secs > 0 {
			if tickTimer != nil {
				tickTimer.Stop()
			}
			tickTimer = time.NewTicker(time.Duration(secs) * time.Second)
			tickCh = tickTimer.C
		} else {
			tickCh = nil
		}
		if p.InitializationOptions.Diagnostics != nil {
			diagEnabled = *p.InitializationOptions.Diagnostics
		}
		ignoreSet = map[string]bool{}
		for _, n := range p.InitializationOptions.Ignore {
			ignoreSet[n] = true
		}
		if projectRoot != "" {
			dir := uriToPath(projectRoot)
			index.indexRoot(dir)
			seen = stampGMLs(dir) // first tick must not re-index everything
			n := 0
			for _, s := range index.files {
				n += len(s)
			}
			logf("indexed %s: %d files, %d symbols, %d assets",
				dir, len(index.files), n, len(index.assets))
		}

	case "shutdown":
		reply(nil)
	case "exit":
		return true

	case "textDocument/didOpen":
		var p struct {
			TextDocument struct{ URI, Text string } `json:"textDocument"`
		}
		json.Unmarshal(req.Params, &p)
		path := uriToPath(p.TextDocument.URI)
		docs[path] = p.TextDocument.Text
		if isGML(path) {
			index.indexText(path, p.TextDocument.Text)
			publishBuffers(index.knownSet())
		}
	case "textDocument/didChange":
		var p struct {
			TextDocument   struct{ URI string }    `json:"textDocument"`
			ContentChanges []struct{ Text string } `json:"contentChanges"`
		}
		json.Unmarshal(req.Params, &p)
		path := uriToPath(p.TextDocument.URI)
		if n := len(p.ContentChanges); n > 0 {
			docs[path] = p.ContentChanges[n-1].Text
			if isGML(path) {
				index.indexText(path, docs[path])
				scheduleDiagnostics()
			}
		}
	case "textDocument/didClose":
		var p struct {
			TextDocument struct{ URI string } `json:"textDocument"`
		}
		json.Unmarshal(req.Params, &p)
		path := uriToPath(p.TextDocument.URI)
		delete(docs, path)
		if isGML(path) {
			index.indexFile(path) // drop unsaved edits; trust disk again
			clearDiagnostics(path)
		}

	case "textDocument/documentSymbol":
		var p struct {
			TextDocument struct{ URI string } `json:"textDocument"`
		}
		json.Unmarshal(req.Params, &p)
		path := uriToPath(p.TextDocument.URI)
		res := []DocumentSymbol{}
		for _, s := range parseSymbols(path, textOf(path)) {
			res = append(res, DocumentSymbol{
				Name: s.Name, Kind: s.Kind, SelectionRange: s.selRange(),
				Range: Range{Position{s.Line, 0}, Position{s.Line, s.End}},
			})
		}
		reply(res)

	case "textDocument/definition":
		var p struct {
			TextDocument struct{ URI string } `json:"textDocument"`
			Position     Position             `json:"position"`
		}
		json.Unmarshal(req.Params, &p)
		path := uriToPath(p.TextDocument.URI)
		res := []Location{}
		w, qual := qualifiedWordAt(textOf(path), p.Position.Line, p.Position.Character)
		if w != "" {
			if qual != "" {
				// enum member or global: jump to the member, not any symbol
				if m := index.lookupMember(qual, w); m != nil {
					res = append(res, m.location())
					reply(res)
					break
				}
				if qual == "global" {
					if g, ok := index.globals[w]; ok {
						res = append(res, g.location())
						reply(res)
						break
					}
				}
			}
			for _, s := range index.lookup(w) {
				res = append(res, s.location())
			}
		}
		reply(res)

	case "textDocument/references":
		var p struct {
			TextDocument struct{ URI string } `json:"textDocument"`
			Position     Position             `json:"position"`
			Context      struct {
				IncludeDeclaration bool `json:"includeDeclaration"`
			} `json:"context"`
		}
		json.Unmarshal(req.Params, &p)
		res := []Location{}
		path := uriToPath(p.TextDocument.URI)
		if w := wordAt(textOf(path), p.Position.Line, p.Position.Character); w != "" {
			res = index.references(w, p.Context.IncludeDeclaration)
		}
		reply(res)

	case "textDocument/hover":
		var p struct {
			TextDocument struct{ URI string } `json:"textDocument"`
			Position     Position             `json:"position"`
		}
		json.Unmarshal(req.Params, &p)
		path := uriToPath(p.TextDocument.URI)
		md := ""
		w, qual := qualifiedWordAt(textOf(path), p.Position.Line, p.Position.Character)
		if w != "" {
			if qual != "" {
				if m := index.lookupMember(qual, w); m != nil {
					md = fmt.Sprintf("**%s.%s** — enum member", qual, w)
				} else if qual == "global" {
					if _, ok := index.globals[w]; ok {
						md = fmt.Sprintf("**global.%s** — global variable", w)
					}
				}
			}
			if md == "" {
				md = index.hover(w)
			}
		}
		if md == "" {
			reply(nil)
		} else {
			reply(map[string]any{"contents": MarkupContent{"markdown", md}})
		}

	case "textDocument/signatureHelp":
		var p struct {
			TextDocument struct{ URI string } `json:"textDocument"`
			Position     Position             `json:"position"`
		}
		json.Unmarshal(req.Params, &p)
		path := uriToPath(p.TextDocument.URI)
		text := textOf(path)
		clean := stripNonCode(text)
		name, arg, calleeStart, ok := callContext(clean, posToOffset(text, p.Position))
		if !ok || isKeyword(name) || isDeclaration(clean, calleeStart) {
			reply(nil) // no call context: let the popup close
			break
		}
		params, label, found := index.signature(name)
		if !found {
			reply(nil)
			break
		}
		active := arg
		if n := len(params); n > 0 {
			if active > n-1 {
				active = n - 1
			}
		} else {
			active = 0
		}
		pl := []any{}
		for _, pn := range params {
			pl = append(pl, map[string]any{"label": pn})
		}
		reply(map[string]any{
			"signatures":      []any{map[string]any{"label": label, "parameters": pl}},
			"activeSignature": 0,
			"activeParameter": active,
		})

	case "textDocument/completion":
		var p struct {
			TextDocument struct{ URI string } `json:"textDocument"`
			Position     Position             `json:"position"`
		}
		json.Unmarshal(req.Params, &p)
		path := uriToPath(p.TextDocument.URI)
		list := CompletionList{IsIncomplete: true, Items: []CompletionItem{}}
		lines := strings.Split(textOf(path), "\n")
		if p.Position.Line >= 0 && p.Position.Line < len(lines) {
			l := strings.TrimSuffix(lines[p.Position.Line], "\r")
			end := utf16ToByte(l, p.Position.Character)
			s := end
			for s > 0 && isIdent(l[s-1]) {
				s--
			}
			prefix := l[s:end]
			if s > 0 && l[s-1] == '.' {
				// qualified: enum members or globals; an empty prefix is fine here
				q := s - 1
				for q > 0 && isIdent(l[q-1]) {
					q--
				}
				list.IsIncomplete = false
				list.Items = index.completeQualified(l[q:s-1], prefix)
			} else if prefix != "" && !(prefix[0] >= '0' && prefix[0] <= '9') {
				// no flooding on empty prefixes or numbers
				list.Items = index.complete(prefix, 200)
			}
		}
		reply(list)

	case "workspace/didChangeWatchedFiles":
		// Some clients watch files; treat the notification as a request to
		// rescan right now. Polling on the ticker covers the rest.
		rescan()
		if len(req.ID) > 0 {
			reply(nil)
		}

	case "workspace/symbol":
		var p struct {
			Query string `json:"query"`
		}
		json.Unmarshal(req.Params, &p)
		res := []SymbolInformation{}
		for _, s := range index.search(p.Query, 500) {
			res = append(res, SymbolInformation{s.Name, s.Kind, s.location(), s.Container})
		}
		reply(res)

	default:
		if len(req.ID) > 0 { // unknown request: must still answer
			reply(nil)
		}
	}
	return false
}
