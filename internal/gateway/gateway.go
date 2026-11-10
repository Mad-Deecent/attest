package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/mad-deecent/attest/internal/decision"
)

const (
	maxBody          = 1 << 20
	anonymous        = "anonymous"
	subjectMeta      = "attest/subject"
	decisionMeta     = "attest/decision"
	attestCallBudget = 5 * time.Second
)

type Gateway struct {
	tools    map[string]Tool
	attest   *attestClient
	upstream *url.URL
	proxy    *httputil.ReverseProxy
}

// New builds a gateway from a config. httpc is used for attest and token
// calls; nil means a client with sane timeouts.
func New(cfg Config, httpc *http.Client) (*Gateway, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if httpc == nil {
		httpc = &http.Client{Timeout: attestCallBudget}
	}
	var tokens tokenSource = staticToken(cfg.Attest.Token)
	if cfg.Attest.OIDC != nil {
		tokens = &clientCredentials{cfg: cfg.Attest.OIDC, http: httpc, now: time.Now}
	}
	up, _ := url.Parse(cfg.Upstream)
	g := &Gateway{
		tools:    cfg.Tools,
		attest:   &attestClient{url: cfg.Attest.URL, tokens: tokens, http: httpc},
		upstream: up,
	}
	g.proxy = &httputil.ReverseProxy{
		// Every path maps to the one upstream endpoint.
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host, pr.Out.URL.Path = up.Scheme, up.Host, up.Path
			pr.Out.URL.RawPath = up.RawPath
			pr.Out.Host = up.Host
		},
		// SSE must not be buffered.
		FlushInterval: -1,
	}
	return g, nil
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		g.proxy.ServeHTTP(w, r)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		rpcError(w, http.StatusRequestEntityTooLarge, nil, -32600, "request body too large")
		return
	}
	// A body we cannot read plainly is a body we cannot gate.
	if enc := r.Header.Get("Content-Encoding"); enc != "" && !strings.EqualFold(enc, "identity") {
		rpcError(w, http.StatusUnsupportedMediaType, nil, -32600, "content-encoding not supported")
		return
	}
	msg, batch, hasCall, err := parseBody(raw)
	switch {
	case err != nil:
		rpcError(w, http.StatusBadRequest, nil, -32700, "unparseable request")
		return
	case batch && hasCall:
		rpcError(w, http.StatusBadRequest, nil, -32600, "batches containing tools/call are not supported")
		return
	case hasCall:
		if d, ok := g.gate(r.Context(), msg); !ok {
			g.refuse(w, msg, d)
			return
		}
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	r.ContentLength = int64(len(raw))
	g.proxy.ServeHTTP(w, r)
}

// outcome is a decision plus the text to show when it is a denial.
type outcome struct {
	dec    *decision.Decision
	reason string
}

// gate reports whether the call may be forwarded.
func (g *Gateway) gate(ctx context.Context, msg rpcMessage) (outcome, bool) {
	subject := msg.Subject
	if subject == "" {
		subject = anonymous
	}
	tool, mapped := g.tools[msg.Name]
	if !mapped {
		log.Printf("gateway: tool=%q subject=%q granted=false identity=- (unmapped)", msg.Name, subject)
		return outcome{reason: fmt.Sprintf("tool %q is not permitted by gateway policy", msg.Name)}, false
	}
	ctx, cancel := context.WithTimeout(ctx, attestCallBudget)
	defer cancel()
	d, err := g.attest.Decide(ctx, decision.Request{Subject: subject, Permission: tool.Permission, Resource: tool.Resource})
	if err != nil {
		log.Printf("gateway: tool=%q subject=%q granted=false identity=- (attest: %v)", msg.Name, subject, err)
		return outcome{reason: "identity service unavailable"}, false
	}
	id := "-"
	if d.IdentityID != nil {
		id = *d.IdentityID
	}
	log.Printf("gateway: tool=%q subject=%q granted=%t identity=%q", msg.Name, subject, d.Granted, id)
	if d.Granted {
		return outcome{}, true
	}
	reason := strings.Join(d.Reasons, "; ")
	if reason == "" {
		reason = "denied"
	}
	return outcome{dec: &d, reason: reason}, false
}

// refuse answers a denied tools/call with an MCP tool error the model can read.
func (g *Gateway) refuse(w http.ResponseWriter, msg rpcMessage, o outcome) {
	if msg.ID == nil { // a notification gets no reply
		w.WriteHeader(http.StatusAccepted)
		return
	}
	result := map[string]any{
		"content": []map[string]string{{"type": "text", "text": "Not permitted: " + o.reason}},
		"isError": true,
	}
	if o.dec != nil {
		result["_meta"] = map[string]any{decisionMeta: o.dec}
	}
	writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
}

func rpcError(w http.ResponseWriter, status int, id json.RawMessage, code int, text string) {
	if id == nil {
		id = json.RawMessage("null")
	}
	writeJSON(w, status, map[string]any{
		"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": text},
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	// Reasons contain comparison operators; keep them readable.
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
}
