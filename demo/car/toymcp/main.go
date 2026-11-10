// Command toymcp is a toy MCP server for the car demo: streamable HTTP with
// plain JSON replies, and two tools that return canned data. It sends no
// email and reads no calendar. Every tool it executes is logged, so the demo
// can show what reached it and what did not.
package main

import (
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"time"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	} `json:"params"`
}

var tools = []map[string]any{
	{
		"name":        "calendar_today",
		"description": "List today's calendar events for the car's owner.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
	},
	{
		"name":        "email_send",
		"description": "Send an email from the owner's account.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"to":      map[string]any{"type": "string"},
				"subject": map[string]any{"type": "string"},
				"body":    map[string]any{"type": "string"},
			},
			"required": []string{"to", "subject"},
		},
	},
}

func main() {
	listen := flag.String("listen", "127.0.0.1:9000", "address to serve /mcp on")
	flag.Parse()
	log.SetPrefix("toy-mcp: ")
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", serve)
	log.Printf("listening on http://%s/mcp", *listen)
	log.Fatal(http.ListenAndServe(*listen, mux))
}

func serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// No server-initiated stream in this toy.
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req rpcRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		reply(w, nil, nil, &rpcError{-32700, "parse error"})
		return
	}
	if req.ID == nil { // a notification, e.g. notifications/initialized
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch req.Method {
	case "initialize":
		w.Header().Set("Mcp-Session-Id", "toy-session")
		reply(w, req.ID, map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "car-toy", "version": "0.1.0"},
		}, nil)
	case "tools/list":
		reply(w, req.ID, map[string]any{"tools": tools}, nil)
	case "tools/call":
		text, ok := call(req.Params.Name, req.Params.Arguments)
		if !ok {
			reply(w, req.ID, nil, &rpcError{-32602, "unknown tool " + req.Params.Name})
			return
		}
		log.Printf("executed %s", req.Params.Name)
		reply(w, req.ID, map[string]any{"content": []map[string]string{{"type": "text", "text": text}}}, nil)
	default:
		reply(w, req.ID, nil, &rpcError{-32601, "method not found"})
	}
}

// call returns canned results. Nothing leaves this process.
func call(name string, args map[string]any) (string, bool) {
	switch name {
	case "calendar_today":
		day := time.Now().Format("Monday 2 January")
		return day + ": 09:30 dentist (Elm Street), 13:00 lunch with Sam, 18:15 pick up the kids.", true
	case "email_send":
		to, _ := args["to"].(string)
		subject, _ := args["subject"].(string)
		return "Sent (not really: this is a toy) to " + to + ": " + subject, true
	}
	return "", false
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func reply(w http.ResponseWriter, id json.RawMessage, result any, e *rpcError) {
	if id == nil {
		id = json.RawMessage("null")
	}
	body := map[string]any{"jsonrpc": "2.0", "id": id}
	if e != nil {
		body["error"] = e
	} else {
		body["result"] = result
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}
