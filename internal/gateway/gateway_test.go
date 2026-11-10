package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mad-deecent/attest/internal/decision"
	"github.com/mad-deecent/attest/internal/perception"
	"github.com/mad-deecent/attest/internal/server"
)

var testNow = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// rig is a gateway in front of a fake upstream, with a real attest behind it.
type rig struct {
	t         *testing.T
	gw        *httptest.Server
	attest    *httptest.Server
	upstreams atomic.Int32 // requests that reached upstream
	release   chan struct{}
}

func newRig(t *testing.T) *rig {
	t.Helper()
	rg := &rig{t: t, release: make(chan struct{})}
	up := httptest.NewServer(http.HandlerFunc(rg.fakeUpstream))
	t.Cleanup(up.Close)
	rg.attest = httptest.NewServer(newAttest(t))
	t.Cleanup(rg.attest.Close)
	rg.start(up.URL+"/mcp", rg.attest.URL)
	return rg
}

func (rg *rig) start(upstream, attestURL string) {
	rg.t.Helper()
	gw, err := New(Config{
		Upstream: upstream,
		Attest:   AttestConfig{URL: attestURL, Token: "agent"},
		Tools: map[string]Tool{
			"play":   {Permission: "music.control"},
			"unlock": {Permission: "door.unlock", Resource: "front"},
			"sos":    {Permission: "emergency.call"},
		},
	}, nil)
	if err != nil {
		rg.t.Fatal(err)
	}
	rg.gw = httptest.NewServer(gw)
	rg.t.Cleanup(rg.gw.Close)
}

func (rg *rig) fakeUpstream(w http.ResponseWriter, r *http.Request) {
	rg.upstreams.Add(1)
	if r.URL.Path != "/mcp" {
		http.Error(w, "wrong path "+r.URL.Path, http.StatusNotFound)
		return
	}
	w.Header().Set("Mcp-Session-Id", "sess-"+r.Header.Get("Mcp-Session-Id"))
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var m struct {
		Method string          `json:"method"`
		ID     json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal(body, &m)
	if m.Method == "stream" {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		<-rg.release
		io.WriteString(w, "data: two\n\n")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	out := map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"from": "upstream", "method": m.Method, "echo": string(body)}}
	_ = json.NewEncoder(w).Encode(out)
}

func newAttest(t *testing.T) http.Handler {
	t.Helper()
	srv, err := server.New(server.Config{
		Tokens: map[string]server.Principal{
			"admin": {Kind: server.KindAdmin, ID: "admin"},
			"face":  {Kind: server.KindProducer, ID: "cam"},
			"agent": {Kind: server.KindAgent, ID: "concierge"},
		},
		Fusion: perception.DefaultConfig(),
		Policy: decision.Policy{
			Permissions: map[string]string{"music.control": "presumed", "door.unlock": "corroborated"},
			Floor:       []string{"emergency.call"},
			Agents:      map[string][]string{"concierge": {"*"}},
			Roles:       map[string][]string{"resident": {"music.control", "door.unlock"}},
		},
	}, func() time.Time { return testNow })
	if err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()
	post := func(token, method, path string, body any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code >= 300 {
			t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body)
		}
	}
	post("admin", "PUT", "/v1/identities/ana", map[string]string{"displayName": "Ana"})
	post("admin", "PUT", "/v1/identities/ana/roles", map[string][]string{"roles": {"resident"}})
	post("admin", "POST", "/v1/delegations", decision.Delegation{
		IdentityID: "ana", Agent: "concierge", Permissions: []string{"*"}, GrantedBy: "self"})
	post("face", "POST", "/v1/evidence", perception.Evidence{
		Subject:    perception.SubjectRef{Key: "spk-ana", Zone: "lobby"},
		Modality:   "face",
		ObservedAt: testNow, ExpiresAt: testNow.Add(10 * time.Minute),
		Candidates:  []perception.Candidate{{IdentityID: "ana", LLR: 6}},
		Calibration: "v1",
	})
	return h
}

type reply struct {
	status int
	header http.Header
	body   map[string]any
}

func (rg *rig) post(body string) reply {
	rg.t.Helper()
	resp, err := http.Post(rg.gw.URL+"/mcp", "application/json", strings.NewReader(body))
	if err != nil {
		rg.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := reply{status: resp.StatusCode, header: resp.Header}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

func call(id int, tool, subject string) string {
	meta := ""
	if subject != "" {
		meta = `,"_meta":{"attest/subject":"` + subject + `"}`
	}
	return `{"jsonrpc":"2.0","id":` + itoa(id) + `,"method":"tools/call","params":{"name":"` + tool + `","arguments":{"secret":"x"}` + meta + `}}`
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

func result(t *testing.T, r reply) map[string]any {
	t.Helper()
	res, ok := r.body["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %+v", r.body)
	}
	return res
}

func requireDenied(t *testing.T, rg *rig, r reply, id float64) map[string]any {
	t.Helper()
	res := result(t, r)
	if r.status != 200 || res["isError"] != true || r.body["id"] != id {
		t.Fatalf("want tool error for id %v, got %d %+v", id, r.status, r.body)
	}
	return res
}

func text(res map[string]any) string {
	return res["content"].([]any)[0].(map[string]any)["text"].(string)
}

func TestGrantedCallIsForwarded(t *testing.T) {
	rg := newRig(t)
	body := call(1, "play", "spk-ana")
	r := rg.post(body)
	res := result(t, r)
	if res["from"] != "upstream" || res["echo"] != body || res["isError"] != nil {
		t.Fatalf("want unmodified upstream response, got %+v", r.body)
	}
	if rg.upstreams.Load() != 1 {
		t.Fatalf("upstream hits = %d", rg.upstreams.Load())
	}
}

func TestDeniedCallNeverReachesUpstream(t *testing.T) {
	rg := newRig(t)
	// Ana is seen by one modality; door.unlock needs two.
	r := rg.post(call(7, "unlock", "spk-ana"))
	res := requireDenied(t, rg, r, 7)
	if !strings.HasPrefix(text(res), "Not permitted: ") || !strings.Contains(text(res), "modalities 1 < min 2") {
		t.Fatalf("text = %q", text(res))
	}
	d := res["_meta"].(map[string]any)["attest/decision"].(map[string]any)
	if d["granted"] != false || d["permission"] != "door.unlock" || d["resource"] != "front" || len(d["reasons"].([]any)) == 0 {
		t.Fatalf("decision = %+v", d)
	}
	if n := rg.upstreams.Load(); n != 0 {
		t.Fatalf("upstream hits = %d", n)
	}
}

func TestUnmappedToolIsDenied(t *testing.T) {
	rg := newRig(t)
	r := rg.post(call(2, "format_disk", "spk-ana"))
	res := requireDenied(t, rg, r, 2)
	if !strings.Contains(text(res), "format_disk") || rg.upstreams.Load() != 0 {
		t.Fatalf("%q, hits %d", text(res), rg.upstreams.Load())
	}
}

func TestMissingSubjectGetsOnlyFloor(t *testing.T) {
	rg := newRig(t)
	if res := result(t, rg.post(call(1, "sos", ""))); res["from"] != "upstream" {
		t.Fatalf("floor permission must pass for anonymous: %+v", res)
	}
	requireDenied(t, rg, rg.post(call(2, "play", "")), 2)
	requireDenied(t, rg, rg.post(call(3, "play", "unknown-speaker")), 3)
	if n := rg.upstreams.Load(); n != 1 {
		t.Fatalf("upstream hits = %d, want 1", n)
	}
}

func TestOtherMethodsPassThrough(t *testing.T) {
	rg := newRig(t)
	req, _ := http.NewRequest("POST", rg.gw.URL+"/anything", strings.NewReader(`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`))
	req.Header.Set("Mcp-Session-Id", "abc")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out["result"].(map[string]any)["method"] != "tools/list" || resp.Header.Get("Mcp-Session-Id") != "sess-abc" {
		t.Fatalf("got %+v %v", out, resp.Header)
	}
	// Non-POST verbs reach upstream untouched.
	get, err := http.Get(rg.gw.URL + "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	get.Body.Close()
	if get.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d", get.StatusCode)
	}
}

func TestSSEStreams(t *testing.T) {
	rg := newRig(t)
	resp, err := http.Post(rg.gw.URL+"/mcp", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"stream"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}
	// The first event must arrive while upstream is still holding the stream open.
	buf := make([]byte, len("data: one\n\n"))
	got := make(chan error, 1)
	go func() { _, err := io.ReadFull(resp.Body, buf); got <- err }()
	select {
	case err := <-got:
		if err != nil || string(buf) != "data: one\n\n" {
			t.Fatalf("first event: %q %v", buf, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stream was buffered")
	}
	close(rg.release)
	rest, _ := io.ReadAll(resp.Body)
	if string(rest) != "data: two\n\n" {
		t.Fatalf("rest = %q", rest)
	}
}

func TestAttestDownDenies(t *testing.T) {
	rg := newRig(t)
	rg.attest.Close()
	res := requireDenied(t, rg, rg.post(call(4, "sos", "spk-ana")), 4)
	if !strings.Contains(text(res), "identity service unavailable") || rg.upstreams.Load() != 0 {
		t.Fatalf("%q, hits %d", text(res), rg.upstreams.Load())
	}

	// A non-200 from attest is the same.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	defer bad.Close()
	rg2 := newRig(t)
	rg2.start("http://127.0.0.1:1/mcp", bad.URL)
	resp, _ := http.Post(rg2.gw.URL, "application/json", strings.NewReader(call(5, "sos", "x")))
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(raw), "identity service unavailable") {
		t.Fatalf("body = %s", raw)
	}
}

func TestBatchWithToolCallRejected(t *testing.T) {
	rg := newRig(t)
	batch := `[{"jsonrpc":"2.0","id":1,"method":"ping"},` + call(2, "sos", "") + `]`
	r := rg.post(batch)
	if r.status != 400 || r.body["error"] == nil || rg.upstreams.Load() != 0 {
		t.Fatalf("got %d %+v, hits %d", r.status, r.body, rg.upstreams.Load())
	}
	// A batch without tools/call is not ours to gate.
	r = rg.post(`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`)
	if r.status != 200 || rg.upstreams.Load() != 1 {
		t.Fatalf("got %d, hits %d", r.status, rg.upstreams.Load())
	}
}

func TestAmbiguousBodiesRejected(t *testing.T) {
	rg := newRig(t)
	for _, body := range []string{
		// Parsers disagree on which duplicate (or case variant) wins.
		`{"jsonrpc":"2.0","id":1,"method":"ping","method":"tools/call","params":{"name":"sos"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"ping","Method":"tools/call","params":{"name":"sos"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"play","name":"sos"}}`,
		`not json`,
	} {
		if r := rg.post(body); r.status != 400 {
			t.Fatalf("%s: status %d", body, r.status)
		}
	}
	if rg.upstreams.Load() != 0 {
		t.Fatal("ambiguous request reached upstream")
	}
}

func TestOIDCTokenIsCachedAndRefreshed(t *testing.T) {
	var fetches int
	var idp *httptest.Server
	idp = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/.well-known/openid-configuration") {
			json.NewEncoder(w).Encode(map[string]string{"token_endpoint": idp.URL + "/token"})
			return
		}
		user, pass, _ := r.BasicAuth()
		r.ParseForm()
		if user != "gw" || pass != "s3cret" || r.PostForm.Get("grant_type") != "client_credentials" {
			w.WriteHeader(401)
			return
		}
		fetches++
		json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 100})
	}))
	defer idp.Close()
	t.Setenv("GW_SECRET", "s3cret")
	now := testNow
	cc := &clientCredentials{
		cfg:  &OIDCConfig{Issuer: idp.URL, ClientID: "gw", ClientSecretEnv: "GW_SECRET"},
		http: idp.Client(), now: func() time.Time { return now },
	}
	get := func() {
		t.Helper()
		if tok, err := cc.Token(t.Context()); err != nil || tok != "tok" {
			t.Fatalf("token %q %v", tok, err)
		}
	}
	get()
	get()
	if fetches != 1 {
		t.Fatalf("fetches = %d, want cached", fetches)
	}
	now = now.Add(80 * time.Second) // inside the 30s skew of a 100s token
	get()
	if fetches != 2 {
		t.Fatalf("fetches = %d, want refresh before expiry", fetches)
	}
}
