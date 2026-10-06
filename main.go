package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
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
type DocumentSymbol struct {
	Name           string `json:"name"`
	Kind           int    `json:"kind"`
	Range          Range  `json:"range"`
	SelectionRange Range  `json:"selectionRange"`
}

var docs = map[string]string{} // uri -> full text

var (
	reFunc  = regexp.MustCompile(`^\s*function\s+([A-Za-z_]\w*)\s*\(`)
	reMacro = regexp.MustCompile(`^\s*#macro\s+([A-Za-z_]\w*)`)
	reEnum  = regexp.MustCompile(`^\s*enum\s+([A-Za-z_]\w*)`)
)

func symbols(text string) []DocumentSymbol {
	out := []DocumentSymbol{}
	for i, line := range strings.Split(text, "\n") {
		add := func(re *regexp.Regexp, kind int) {
			if m := re.FindStringSubmatchIndex(line); m != nil {
				r := Range{Position{i, m[2]}, Position{i, m[3]}}
				out = append(out, DocumentSymbol{line[m[2]:m[3]], kind, Range{Position{i, 0}, Position{i, len(line)}}, r})
			}
		}
		add(reFunc, 12)  // Function
		add(reMacro, 14) // Constant
		add(reEnum, 10)  // Enum
	}
	return out
}

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

func main() {
	in, out := bufio.NewReader(os.Stdin), os.Stdout
	log := func(f string, a ...any) { fmt.Fprintf(os.Stderr, "[gmlls] "+f+"\n", a...) }
	for {
		raw, err := readMsg(in)
		if err != nil {
			return
		}
		var req Request
		if json.Unmarshal(raw, &req) != nil {
			continue
		}
		reply := func(result any) { send(out, Response{"2.0", req.ID, result}) }

		switch req.Method {
		case "initialize":
			reply(map[string]any{
				"capabilities": map[string]any{
					"textDocumentSync":       1, // full sync
					"documentSymbolProvider": true,
				},
				"serverInfo": map[string]string{"name": "gmlls"},
			})
		case "shutdown":
			reply(nil)
		case "exit":
			return
		case "textDocument/didOpen":
			var p struct {
				TextDocument struct{ URI, Text string } `json:"textDocument"`
			}
			json.Unmarshal(req.Params, &p)
			docs[p.TextDocument.URI] = p.TextDocument.Text
			log("opened %s", p.TextDocument.URI)
		case "textDocument/didChange":
			var p struct {
				TextDocument   struct{ URI string }    `json:"textDocument"`
				ContentChanges []struct{ Text string } `json:"contentChanges"`
			}
			json.Unmarshal(req.Params, &p)
			if n := len(p.ContentChanges); n > 0 {
				docs[p.TextDocument.URI] = p.ContentChanges[n-1].Text
			}
		case "textDocument/didClose":
			var p struct {
				TextDocument struct{ URI string } `json:"textDocument"`
			}
			json.Unmarshal(req.Params, &p)
			delete(docs, p.TextDocument.URI)
		case "textDocument/documentSymbol":
			var p struct {
				TextDocument struct{ URI string } `json:"textDocument"`
			}
			json.Unmarshal(req.Params, &p)
			reply(symbols(docs[p.TextDocument.URI]))
		default:
			if len(req.ID) > 0 { // unknown request: must still answer
				reply(nil)
			}
		}
	}
}
