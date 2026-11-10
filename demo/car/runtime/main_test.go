package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mad-deecent/attest/internal/decision"
	"github.com/mad-deecent/attest/internal/gateway"
	"github.com/mad-deecent/attest/internal/perception"
	"github.com/mad-deecent/attest/internal/server"
)

// stack is attest, attest-gateway, and an MCP server that records what it
// executed, wired as in the demo.
type stack struct {
	gateway  string
	attest   http.Handler
	mu       sync.Mutex
	executed []string
}

func newStack(t *testing.T) *stack {
	t.Helper()
	st := &stack{}
	srv, err := server.New(server.Config{
		Tokens: map[string]server.Principal{
			"admin": {Kind: server.KindAdmin, ID: "admin"},
			"voice": {Kind: server.KindProducer, ID: "mic"},
			"agent": {Kind: server.KindAgent, ID: "concierge"},
		},
		Fusion: perception.DefaultConfig(),
		Policy: decision.Policy{
			Permissions: map[string]string{"calendar.read": "confident", "email.send": "confident"},
			Floor:       []string{"converse"},
			Agents:      map[string][]string{"concierge": {"calendar.read", "email.send", "converse"}},
			Roles:       map[string][]string{"owner": {"calendar.read", "email.send"}},
		},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	st.attest = srv.Handler()
	as := httptest.NewServer(st.attest)
	t.Cleanup(as.Close)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&m)
		if m.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		result := map[string]any{"content": []map[string]string{{"type": "text", "text": "ok"}}}
		switch m.Method {
		case "initialize":
			result = map[string]any{"serverInfo": map[string]any{"name": "fake"}}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{"name": "calendar_today"}, {"name": "email_send"}}}
		case "tools/call":
			st.mu.Lock()
			st.executed = append(st.executed, m.Params.Name)
			st.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
	}))
	t.Cleanup(up.Close)

	gw, err := gateway.New(gateway.Config{
		Upstream: up.URL + "/mcp",
		Attest:   gateway.AttestConfig{URL: as.URL, Token: "agent"},
		Tools: map[string]gateway.Tool{
			"calendar_today": {Permission: "calendar.read"},
			"email_send":     {Permission: "email.send"},
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	gs := httptest.NewServer(gw)
	t.Cleanup(gs.Close)
	st.gateway = gs.URL + "/mcp"

	st.do(t, "admin", "PUT", "/v1/identities/owner-id", map[string]string{"displayName": "Owner"})
	st.do(t, "admin", "PUT", "/v1/identities/other-id", map[string]string{"displayName": "C"})
	st.do(t, "admin", "PUT", "/v1/identities/owner-id/roles", map[string][]string{"roles": {"owner"}})
	st.do(t, "admin", "POST", "/v1/delegations", decision.Delegation{
		IdentityID: "owner-id", Agent: "concierge", Permissions: []string{"calendar.read", "email.send"}, GrantedBy: "admin",
	})
	return st
}

func (st *stack) do(t *testing.T, token, method, path string, body any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	st.attest.ServeHTTP(rec, req)
	if rec.Code >= 300 {
		t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body)
	}
}

// voice posts what the producer would for a subject.
func (st *stack) voice(t *testing.T, subject string, owner, other float64) string {
	t.Helper()
	now := time.Now()
	st.do(t, "voice", "POST", "/v1/evidence", perception.Evidence{
		Subject: perception.SubjectRef{Key: subject, Zone: "car"}, Modality: "voice",
		ObservedAt: now, ExpiresAt: now.Add(5 * time.Minute), Calibration: "test",
		Candidates: []perception.Candidate{{IdentityID: "owner-id", LLR: owner}, {IdentityID: "other-id", LLR: other}},
	})
	return `{"type":"evidence","subject":"` + subject + `","candidates":[{"identityId":"owner-id","llr":` +
		jsonNum(owner) + `}],"sent":true}`
}

func jsonNum(f float64) string { b, _ := json.Marshal(f); return string(b) }

var triggers = []Trigger{
	{Phrase: "Colloquial definition", Tool: "calendar_today", Expect: "granted"},
	{Phrase: "embraces us", Tool: "email_send", Arguments: map[string]any{"to": "x@example.com", "subject": "hi"}, Expect: "denied"},
	{Phrase: "it was silent", Tool: "email_send", Expect: "denied"},
	{Phrase: "hotel attachment", Tool: "calendar_today", Expect: "denied"},
}

func TestCarScenario(t *testing.T) {
	st := newStack(t)
	var out bytes.Buffer
	rt := &runtime{
		gateway: st.gateway, http: http.DefaultClient, triggers: triggers, owner: "owner-id",
		names: map[string]string{"owner-id": "owner"}, out: &out,
		heard: map[string]bool{}, pending: map[string][]event{},
	}
	if err := rt.connect(); err != nil {
		t.Fatal(err)
	}
	// The owner's words arrive before the evidence for their turn, as the
	// producer emits them; the runtime must wait for it.
	stream := strings.Join([]string{
		`{"type":"utterance","subject":"s/spk0","start":8,"end":13.9,"text":"one can well appreciate the colloquial definition of cabaret."}`,
		`{"type":"utterance","subject":null,"start":6.8,"end":7.5,"text":"It was silent because"}`,
		st.voice(t, "s/spk0", 8, -5),
		`{"type":"utterance","subject":null,"start":14.7,"end":20.5,"text":"The Poodle Dog has a hotel attachment"}`,
		`{"type":"utterance","subject":"s/spk1","start":22.5,"end":24,"text":"that surrounds and embraces us"}`,
		st.voice(t, "s/spk1", -4.1, -3.9),
	}, "\n")
	if err := rt.consume(strings.NewReader(stream)); err != nil {
		t.Fatal(err)
	}
	if failures := rt.check(true); len(failures) > 0 {
		t.Fatalf("failures: %v\n%s", failures, out.String())
	}
	if len(rt.calls) != 4 {
		t.Fatalf("calls = %+v", rt.calls)
	}
	// The owner's utterance waited for its evidence line; the unattributed
	// one after it did not wait.
	if rt.calls[0].Subject != nil || rt.calls[1].Subject == nil || *rt.calls[1].Subject != "s/spk0" {
		t.Fatalf("call order: %+v", rt.calls)
	}
	// Only the owner's calendar call reached the MCP server.
	if strings.Join(st.executed, ",") != "calendar_today" {
		t.Fatalf("executed upstream: %v\n%s", st.executed, out.String())
	}
	for _, want := range []string{
		"GRANTED (subject s/spk0, identity: owner)",
		"no subject: only floor permissions apply",
		"subject is unknown: belief is unknown",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("transcript lacks %q:\n%s", want, out.String())
		}
	}
}

func TestUnheardTriggerFailsTheRun(t *testing.T) {
	st := newStack(t)
	rt := &runtime{
		gateway: st.gateway, http: http.DefaultClient, triggers: triggers, owner: "owner-id",
		names: map[string]string{}, out: io.Discard, heard: map[string]bool{}, pending: map[string][]event{},
	}
	if err := rt.consume(strings.NewReader(st.voice(t, "s/spk0", 8, -5) + "\n" +
		`{"type":"utterance","subject":"s/spk0","start":1,"end":2,"text":"the colloquial definition"}`)); err != nil {
		t.Fatal(err)
	}
	failures := strings.Join(rt.check(true), "\n")
	for _, want := range []string{`"embraces us" (expected denied) never fired`, "not seen: a call with no subject is denied"} {
		if !strings.Contains(failures, want) {
			t.Fatalf("want %q in failures:\n%s", want, failures)
		}
	}
}

func TestPhraseMatching(t *testing.T) {
	for _, c := range []struct {
		text, phrase string
		want         bool
	}{
		{"San Francisco's carefree spirit", "francisco's carefree", true},
		{"Here, as well as in a number", "here as well", true},
		{"embraces us.", "Embraces Us", true},
		{"embracesus", "embraces us", false},
		{"the great mystery", "great myst", false},
	} {
		if got := matches(c.text, c.phrase); got != c.want {
			t.Errorf("matches(%q, %q) = %v", c.text, c.phrase, got)
		}
	}
}
