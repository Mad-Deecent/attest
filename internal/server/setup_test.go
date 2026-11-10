package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mad-deecent/attest/internal/decision"
	"github.com/mad-deecent/attest/internal/perception"
	"github.com/mad-deecent/attest/internal/provider"
)

// console drives the API as a browser would: cookies kept, the CSRF token
// sent once it has one. It keeps every response body for leak checks.
type console struct {
	t      *testing.T
	base   string
	c      *http.Client
	csrf   string
	bearer string
	bodies []string
}

func newConsole(t *testing.T, h http.Handler) *console {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	return &console{t: t, base: srv.URL, c: &http.Client{Jar: jar}}
}

func (c *console) do(method, path string, body any, want int, into any) {
	c.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			c.t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, c.base+path, &buf)
	if err != nil {
		c.t.Fatal(err)
	}
	if c.csrf != "" {
		req.Header.Set("X-CSRF-Token", c.csrf)
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	resp, err := c.c.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	c.bodies = append(c.bodies, string(raw))
	if resp.StatusCode != want {
		c.t.Fatalf("%s %s: status %d, want %d: %s", method, path, resp.StatusCode, want, raw)
	}
	if into != nil {
		if err := json.Unmarshal(raw, into); err != nil {
			c.t.Fatal(err)
		}
	}
}

// unconfigured starts attest as a first run with no config file would.
func unconfigured(t *testing.T, now *time.Time) (*Server, string) {
	t.Helper()
	data := filepath.Join(t.TempDir(), "attest.json")
	srv, err := New(Config{Data: data, Fusion: perception.DefaultConfig()}, func() time.Time { return *now })
	if err != nil {
		t.Fatal(err)
	}
	return srv, data
}

// openSetup trades the setup code for a session.
func openSetup(t *testing.T, srv *Server) *console {
	t.Helper()
	c := newConsole(t, srv.Handler())
	var sess struct {
		CSRF string `json:"csrf"`
	}
	c.do("POST", "/v1/setup/session", map[string]string{"code": srv.setup.code}, http.StatusCreated, &sess)
	c.csrf = sess.CSRF
	return c
}

func TestFirstRunSetup(t *testing.T) {
	is := newIssuer(t)
	now := time.Now()
	srv, data := unconfigured(t, &now)
	code := srv.setup.code
	if code == "" {
		t.Fatal("an unconfigured attest has no setup code")
	}
	c := newConsole(t, srv.Handler())
	var status map[string]bool
	c.do("GET", "/v1/setup/status", nil, http.StatusOK, &status)
	if status["setupComplete"] {
		t.Fatal("a first run reports setup complete")
	}
	// Reaching the port is not enough.
	c.do("GET", "/v1/settings", nil, http.StatusUnauthorized, nil)
	c.do("POST", "/v1/setup/session", map[string]string{"code": "AAAA-BBBB-CCCC-DDDD"}, http.StatusUnauthorized, nil)

	var sess struct {
		CSRF string `json:"csrf"`
	}
	// Typed loosely, as people do.
	c.do("POST", "/v1/setup/session", map[string]string{"code": strings.ToLower(strings.ReplaceAll(code, "-", " "))}, http.StatusCreated, &sess)
	// The code is single-use: nobody else can open a second session with it.
	newConsole(t, srv.Handler()).do("POST", "/v1/setup/session", map[string]string{"code": code}, http.StatusUnauthorized, nil)

	c.do("GET", "/v1/settings", nil, http.StatusOK, nil)
	c.do("PUT", "/v1/settings/public-url", map[string]string{"publicUrl": "http://attest.home:8420/"}, http.StatusForbidden, nil)
	c.csrf = sess.CSRF
	c.do("POST", "/v1/setup/complete", nil, http.StatusConflict, nil)
	c.do("PUT", "/v1/settings/public-url", map[string]string{"publicUrl": "ftp://attest.home"}, http.StatusBadRequest, nil)
	// Any OIDC issuer, with a client the operator made by hand. Enrolment
	// needs somewhere to send people back to, so the public URL comes first.
	manual := map[string]string{"kind": "oidc", "issuer": is.srv.URL, "clientId": "attest-enrol"}
	c.do("POST", "/v1/settings/provider", manual, http.StatusBadRequest, nil)
	if srv.cur().settings.OIDC != nil {
		t.Fatal("a connection attest could not restart with was stored")
	}
	c.do("PUT", "/v1/settings/public-url", map[string]string{"publicUrl": "http://attest.home:8420/"}, http.StatusOK, nil)
	c.do("POST", "/v1/settings/provider", manual, http.StatusOK, nil)
	c.do("POST", "/v1/setup/complete", nil, http.StatusOK, nil)

	// The session and the code are dead.
	c.do("GET", "/v1/settings", nil, http.StatusUnauthorized, nil)
	c.do("POST", "/v1/setup/session", map[string]string{"code": code}, http.StatusGone, nil)
	if srv.setup.code != "" {
		t.Fatal("setup left a code behind")
	}

	// From now on an admin from the provider manages settings.
	c.csrf, c.bearer = "", is.token("ops", time.Now().Add(time.Hour), "attest-admin")
	var view settingsView
	c.do("GET", "/v1/settings", nil, http.StatusOK, &view)
	if !view.SetupComplete || view.PublicURL != "http://attest.home:8420" || view.Provider == nil ||
		view.Provider.Issuer != is.srv.URL || view.Provider.ClientID != "attest-enrol" || view.Provider.Kind != "oidc" {
		t.Fatalf("settings after setup: %+v", view)
	}
	if len(view.Policy.Bands) != 3 {
		t.Fatalf("want the default ladder, got %+v", view.Policy.Bands)
	}

	// A restart remembers, and asks for no code.
	again, err := New(Config{Data: data, Fusion: perception.DefaultConfig()}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if again.setup.code != "" || !again.cur().settings.SetupComplete || !again.cur().enrolling() {
		t.Fatalf("after restart: code %q, settings %+v", again.setup.code, again.cur().settings)
	}
	newConsole(t, again.Handler()).do("POST", "/v1/setup/session", map[string]string{"code": code}, http.StatusGone, nil)
	info, err := os.Stat(data)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("data file: %v, mode %v", err, info.Mode())
	}
}

func TestSetupCodeRenewsOnlyAfterItsSessionLapses(t *testing.T) {
	now := time.Now()
	srv, _ := unconfigured(t, &now)
	code := srv.setup.code
	c := newConsole(t, srv.Handler())
	// Nobody can rotate away a code the operator has not used yet.
	c.do("GET", "/v1/setup/status", nil, http.StatusOK, nil)
	c.do("POST", "/v1/setup/session", map[string]string{"code": "WRONG"}, http.StatusUnauthorized, nil)
	if srv.setup.code != code {
		t.Fatal("an unspent setup code was replaced")
	}
	s := openSetup(t, srv)
	s.do("GET", "/v1/settings", nil, http.StatusOK, nil)

	now = now.Add(setupTTL)
	s.do("GET", "/v1/settings", nil, http.StatusUnauthorized, nil)
	c.do("GET", "/v1/setup/status", nil, http.StatusOK, nil)
	if srv.setup.code == "" || srv.setup.code == code {
		t.Fatalf("no fresh code after the session lapsed: %q", srv.setup.code)
	}
	c.do("POST", "/v1/setup/session", map[string]string{"code": code}, http.StatusUnauthorized, nil)
	openSetup(t, srv).do("GET", "/v1/settings", nil, http.StatusOK, nil)
}

func TestResetSetupReopensFirstRun(t *testing.T) {
	data := filepath.Join(t.TempDir(), "attest.json")
	cfg := Config{Data: data, Tokens: map[string]Principal{"root": {Kind: KindAdmin, ID: "root"}}, Fusion: perception.DefaultConfig()}
	srv, err := New(cfg, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if srv.setup.code != "" {
		t.Fatal("a config with an admin token still asks for setup")
	}
	cfg.ResetSetup = true
	srv, err = New(cfg, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if srv.setup.code == "" || srv.cur().settings.SetupComplete {
		t.Fatal("-reset-setup did not reopen setup")
	}
	openSetup(t, srv).do("GET", "/v1/settings", nil, http.StatusOK, nil)
}

func connectKeycloak(cred map[string]string, clientID string, kc *keycloak) map[string]any {
	body := map[string]any{"kind": "keycloak", "url": kc.srv.URL, "realm": kcRealm, "credential": cred}
	if clientID != "" {
		body["clientId"] = clientID
	}
	return body
}

type connectReply struct {
	Error    string            `json:"error"`
	Provider *providerView     `json:"provider"`
	Changes  []provider.Change `json:"changes"`
}

func actions(r connectReply) string {
	var out []string
	for _, c := range r.Changes {
		out = append(out, c.Object+" "+c.Name+": "+c.Action)
	}
	return strings.Join(out, ", ")
}

func TestKeycloakConnectIsIdempotent(t *testing.T) {
	kc := newKeycloak(t)
	now := time.Now()
	srv, data := unconfigured(t, &now)
	c := openSetup(t, srv)
	ana := map[string]string{"username": "ana", "password": kcPassword}
	// The redirect URI comes from the public URL.
	c.do("POST", "/v1/settings/provider", connectKeycloak(ana, "", kc), http.StatusBadRequest, nil)
	c.do("PUT", "/v1/settings/public-url", map[string]string{"publicUrl": "https://attest.home"}, http.StatusOK, nil)

	var first connectReply
	c.do("POST", "/v1/settings/provider", connectKeycloak(ana, "", kc), http.StatusOK, &first)
	want := "role attest-admin: created, role attest-producer: created, role attest-agent: created, " +
		"client attest-enrol: created, role grant attest-admin: created"
	if got := actions(first); got != want {
		t.Fatalf("first connect:\n got %s\nwant %s", got, want)
	}
	if first.Provider.Issuer != kc.srv.URL+"/realms/home" || first.Provider.ClientID != "attest-enrol" || first.Provider.Kind != "keycloak" {
		t.Fatalf("connection: %+v", first.Provider)
	}
	client := kc.client("attest-enrol")
	if client["publicClient"] != true || client["directAccessGrantsEnabled"] != false ||
		client["attributes"].(map[string]any)["pkce.code.challenge.method"] != "S256" ||
		client["redirectUris"].([]any)[0] != "https://attest.home/enrol/callback" ||
		client["webOrigins"].([]any)[0] != "https://attest.home" {
		t.Fatalf("enrolment client: %v", client)
	}
	if got := kc.mappings[kcAnaID]; len(got) != 1 || got[0]["name"] != "attest-admin" {
		t.Fatalf("ana's realm roles: %v", got)
	}

	var second connectReply
	c.do("POST", "/v1/settings/provider", connectKeycloak(ana, "", kc), http.StatusOK, &second)
	if got := actions(second); got != strings.ReplaceAll(want, "created", "found") {
		t.Fatalf("second connect changed something: %s", got)
	}
	if len(kc.clients) != 1 || len(kc.mappings[kcAnaID]) != 1 {
		t.Fatal("second connect duplicated something")
	}
	if kc.logouts != 2 {
		t.Fatalf("admin sessions left open: %d logouts for 2 connects", kc.logouts)
	}
	// What was set at runtime starts again.
	again, err := New(Config{Data: data, Fusion: perception.DefaultConfig()}, time.Now)
	if err != nil || !again.cur().enrolling() || again.cur().settings.Provider != "keycloak" {
		t.Fatalf("restart after connecting: %v", err)
	}
}

func TestKeycloakOnlyAddsToAnExistingClient(t *testing.T) {
	kc := newKeycloak(t)
	kc.clients = []map[string]any{
		{"id": "c1", "clientId": "mine", "publicClient": true, "standardFlowEnabled": true,
			"redirectUris": []any{"https://other.home/cb"}, "webOrigins": []any{}, "attributes": map[string]any{"keep": "me"}},
		{"id": "c2", "clientId": "secretive", "publicClient": false},
	}
	now := time.Now()
	srv, _ := unconfigured(t, &now)
	c := openSetup(t, srv)
	c.do("PUT", "/v1/settings/public-url", map[string]string{"publicUrl": "http://attest.home"}, http.StatusOK, nil)
	svc := map[string]string{"clientId": kcServiceUser, "clientSecret": kcSecret}

	var r connectReply
	c.do("POST", "/v1/settings/provider", connectKeycloak(svc, "secretive", kc), http.StatusBadRequest, &r)
	if srv.cur().settings.OIDC != nil {
		t.Fatal("a refused connect was stored")
	}
	c.do("POST", "/v1/settings/provider", connectKeycloak(svc, "mine", kc), http.StatusOK, &r)
	client := kc.client("mine")
	if r.Changes[3].Action != "updated" ||
		len(client["redirectUris"].([]any)) != 2 || client["redirectUris"].([]any)[0] != "https://other.home/cb" ||
		client["attributes"].(map[string]any)["keep"] != "me" ||
		client["attributes"].(map[string]any)["pkce.code.challenge.method"] != "S256" {
		t.Fatalf("existing client: %v (changes %s)", client, actions(r))
	}
	// A service client is nobody to make admin.
	if g := r.Changes[4]; g.Object != "role grant" || g.Action != "skipped" || len(kc.mappings) != 0 {
		t.Fatalf("grant with a service credential: %+v", g)
	}
}

func TestKeycloakPartialFailureStoresNothing(t *testing.T) {
	kc := newKeycloak(t)
	now := time.Now()
	srv, data := unconfigured(t, &now)
	c := openSetup(t, srv)
	c.do("PUT", "/v1/settings/public-url", map[string]string{"publicUrl": "http://attest.home"}, http.StatusOK, nil)
	ana := map[string]string{"username": "ana", "password": kcPassword}

	kc.fail("POST /clients")
	var r connectReply
	c.do("POST", "/v1/settings/provider", connectKeycloak(ana, "", kc), http.StatusBadGateway, &r)
	if !strings.Contains(r.Error, "disk full") || actions(r) !=
		"role attest-admin: created, role attest-producer: created, role attest-agent: created" {
		t.Fatalf("failed connect: %q, %s", r.Error, actions(r))
	}
	var view settingsView
	c.do("GET", "/v1/settings", nil, http.StatusOK, &view)
	raw, _ := os.ReadFile(data)
	if view.Provider != nil || srv.cur().oidc != nil || strings.Contains(string(raw), `"oidc"`) {
		t.Fatalf("a failed connect left a connection behind: %+v\n%s", view.Provider, raw)
	}
	c.do("POST", "/v1/setup/complete", nil, http.StatusConflict, nil)

	// Running it again finishes the job.
	c.do("POST", "/v1/settings/provider", connectKeycloak(ana, "", kc), http.StatusOK, &r)
	if got := actions(r); !strings.HasPrefix(got, "role attest-admin: found") || !strings.Contains(got, "client attest-enrol: created") {
		t.Fatalf("retry: %s", got)
	}
}

func TestAdminCredentialIsNeverKept(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	kc := newKeycloak(t)
	now := time.Now()
	srv, data := unconfigured(t, &now)
	c := openSetup(t, srv)
	c.do("PUT", "/v1/settings/public-url", map[string]string{"publicUrl": "http://attest.home"}, http.StatusOK, nil)
	wrong := map[string]string{"username": "ana", "password": "pw-SEKRIT-wrong"}
	c.do("POST", "/v1/settings/provider", connectKeycloak(wrong, "", kc), http.StatusBadGateway, nil)
	c.do("POST", "/v1/settings/provider", connectKeycloak(map[string]string{"clientId": kcServiceUser, "clientSecret": kcSecret}, "", kc), http.StatusOK, nil)
	c.do("POST", "/v1/settings/provider", connectKeycloak(map[string]string{"username": "ana", "password": kcPassword}, "", kc), http.StatusOK, nil)
	c.do("GET", "/v1/settings", nil, http.StatusOK, nil)
	// Fail at every stage, with either kind of credential.
	for _, stage := range []string{"GET /roles", "POST /roles", "GET /clients", "POST /clients", "GET /users", "POST /users"} {
		for _, cred := range []map[string]string{
			{"username": "ana", "password": kcPassword},
			{"clientId": kcServiceUser, "clientSecret": kcSecret},
		} {
			broken := newKeycloak(t)
			broken.fail(stage)
			want := http.StatusBadGateway
			if strings.Contains(stage, "users") && cred["clientId"] != "" {
				want = http.StatusOK // a service client never touches users
			}
			c.do("POST", "/v1/settings/provider", connectKeycloak(cred, "", broken), want, nil)
		}
	}

	raw, err := os.ReadFile(data)
	if err != nil {
		t.Fatal(err)
	}
	places := map[string]string{"log": logs.String(), "data file": string(raw), "responses": strings.Join(c.bodies, "\n")}
	for where, text := range places {
		for _, secret := range []string{"SEKRIT", kcAnaToken, kcSvcToken, "kc-ADMIN", "kc-refresh"} {
			if strings.Contains(text, secret) {
				t.Errorf("%s contains %q:\n%s", where, secret, text)
			}
		}
	}
	if !strings.Contains(logs.String(), "identity provider connected") {
		t.Fatalf("expected a connect log line, got:\n%s", logs.String())
	}
}

func TestUnsupportedProviders(t *testing.T) {
	now := time.Now()
	srv, _ := unconfigured(t, &now)
	c := openSetup(t, srv)
	for _, kind := range []string{"auth0", "ory"} {
		var r connectReply
		c.do("POST", "/v1/settings/provider", map[string]any{"kind": kind, "url": "https://x.example"}, http.StatusNotImplemented, &r)
		if !strings.Contains(r.Error, "not yet supported") {
			t.Fatalf("%s: %q", kind, r.Error)
		}
	}
	c.do("POST", "/v1/settings/provider", map[string]any{"kind": "okta"}, http.StatusBadRequest, nil)
	c.do("POST", "/v1/settings/provider", map[string]any{"kind": "oidc", "issuer": "http://127.0.0.1:1"}, http.StatusBadGateway, nil)
}

func TestReconnectingSwitchesVerificationLive(t *testing.T) {
	a, b := newIssuer(t), newIssuer(t)
	srv, err := New(Config{
		Data:   filepath.Join(t.TempDir(), "attest.json"),
		OIDC:   &OIDCConfig{Issuer: a.srv.URL},
		Tokens: map[string]Principal{"root": {Kind: KindAdmin, ID: "root"}},
		Fusion: perception.DefaultConfig(),
		Policy: decision.Policy{Floor: []string{"emergency.call"}, Agents: map[string][]string{"concierge": {"*"}}},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	hn := &harness{t: t, h: srv.Handler(), now: time.Now()}
	soon := time.Now().Add(time.Hour)
	fromA, fromB := a.token("concierge", soon, "attest-agent"), b.token("concierge", soon, "attest-agent")
	req := decision.Request{Subject: "s", Permission: "emergency.call"}
	hn.call(fromA, "POST", "/v1/decisions", req, http.StatusOK, nil)
	hn.call(fromB, "POST", "/v1/decisions", req, http.StatusUnauthorized, nil)

	// Requests keep flowing while the connection changes underneath them.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for _, tok := range []string{fromA, fromB, fromA, fromB} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body, _ := json.Marshal(req)
			for {
				select {
				case <-stop:
					return
				default:
				}
				r := httptest.NewRequest("POST", "/v1/decisions", bytes.NewReader(body))
				r.Header.Set("Authorization", "Bearer "+tok)
				rec := httptest.NewRecorder()
				srv.Handler().ServeHTTP(rec, r)
				if rec.Code != http.StatusOK && rec.Code != http.StatusUnauthorized {
					t.Errorf("during reconnect: status %d: %s", rec.Code, rec.Body)
					return
				}
			}
		}()
	}
	hn.call("root", "POST", "/v1/settings/provider", map[string]string{"kind": "oidc", "issuer": b.srv.URL}, http.StatusOK, nil)
	time.Sleep(20 * time.Millisecond)
	close(stop)
	wg.Wait()

	hn.call(fromA, "POST", "/v1/decisions", req, http.StatusUnauthorized, nil)
	hn.call(fromB, "POST", "/v1/decisions", req, http.StatusOK, nil)
}

func TestPolicyChangesLive(t *testing.T) {
	hn := newHarnessWith(t, filepath.Join(t.TempDir(), "attest.json"))
	var p decision.Policy
	hn.call("admin", "GET", "/v1/settings/policy", nil, http.StatusOK, &p)
	if d := hn.decide("", "emergency.call"); !d.Granted {
		t.Fatalf("floor before the change: %+v", d)
	}

	bad := p
	bad.Permissions = map[string]string{"door.unlock": "certain"}
	hn.call("admin", "PUT", "/v1/settings/policy", bad, http.StatusBadRequest, nil)
	seeded := p
	seeded.Assignments = map[string][]string{"ana": {"resident"}}
	hn.call("admin", "PUT", "/v1/settings/policy", seeded, http.StatusBadRequest, nil)
	hn.call("producer-less", "PUT", "/v1/settings/policy", p, http.StatusUnauthorized, nil)
	hn.call("agent", "PUT", "/v1/settings/policy", p, http.StatusForbidden, nil)

	// Drop the guest role and the floor; leave bands to their defaults.
	next := p
	next.Bands = nil
	next.Floor = nil
	next.Roles = map[string][]string{"resident": p.Roles["resident"]}
	var got decision.Policy
	hn.call("admin", "PUT", "/v1/settings/policy", next, http.StatusOK, &got)
	if len(got.Bands) != 3 {
		t.Fatalf("bands not defaulted: %+v", got.Bands)
	}
	if d := hn.decide("", "emergency.call"); d.Granted {
		t.Fatalf("the old floor still applies: %+v", d)
	}
	hn.call("admin", "PUT", "/v1/identities/ben/roles", map[string][]string{"roles": {"guest"}}, http.StatusBadRequest, nil)
}

func TestConfigSeedsSettingsOnce(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "attest.json")
	// A data file from before settings moved there still loads.
	if err := os.WriteFile(data, []byte(`{"identities":[],"assignments":{},"delegations":[],"nextDelegation":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Data: data, PublicURL: "http://a.example",
		Tokens: map[string]Principal{"root": {Kind: KindAdmin, ID: "root"}},
		Fusion: perception.DefaultConfig(),
	}
	start := func() *harness {
		srv, err := New(cfg, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		return &harness{t: t, h: srv.Handler(), now: time.Now()}
	}
	publicURL := func(hn *harness) string {
		var body map[string]string
		hn.call("root", "GET", "/v1/settings/public-url", nil, http.StatusOK, &body)
		return body["publicUrl"]
	}
	hn := start()
	if got := publicURL(hn); got != "http://a.example" {
		t.Fatalf("seeded public URL: %q", got)
	}
	hn.call("root", "PUT", "/v1/settings/public-url", map[string]string{"publicUrl": "http://b.example"}, http.StatusOK, nil)
	// A restart with the same file keeps what was changed at runtime.
	if got := publicURL(start()); got != "http://b.example" {
		t.Fatalf("restart undid a runtime change: %q", got)
	}
	// Editing the file applies the edit.
	cfg.PublicURL = "http://c.example"
	if got := publicURL(start()); got != "http://c.example" {
		t.Fatalf("an edited config file was ignored: %q", got)
	}
}

// residents is a config whose policy seeds alice with a role.
func residents(data string) Config {
	return Config{
		Data:   data,
		Tokens: map[string]Principal{"root": {Kind: KindAdmin, ID: "root"}},
		Fusion: perception.DefaultConfig(),
		Policy: decision.Policy{
			Permissions: map[string]string{"door.unlock": "presumed"},
			Roles:       map[string][]string{"resident": {"door.unlock"}},
			Assignments: map[string][]string{"alice": {"resident"}},
		},
	}
}

func TestRevokedRolesStayRevokedAcrossRestarts(t *testing.T) {
	restart := func(cfg Config) *Server {
		t.Helper()
		srv, err := New(cfg, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		return srv
	}
	t.Run("cleared through the identities API", func(t *testing.T) {
		cfg := residents(filepath.Join(t.TempDir(), "attest.json"))
		srv := restart(cfg)
		if got := srv.rbac.Roles("alice"); len(got) != 1 {
			t.Fatalf("the first start did not seed alice: %v", got)
		}
		hn := &harness{t: t, h: srv.Handler(), now: time.Now()}
		hn.call("root", "PUT", "/v1/identities/alice", map[string]string{"displayName": "Alice"}, http.StatusOK, nil)
		hn.call("root", "PUT", "/v1/identities/alice/roles", map[string][]string{"roles": {}}, http.StatusOK, nil)
		if got := restart(cfg).rbac.Roles("alice"); len(got) != 0 {
			t.Fatalf("a restart gave back revoked roles: %v", got)
		}
	})
	t.Run("removed with the role in policy", func(t *testing.T) {
		cfg := residents(filepath.Join(t.TempDir(), "attest.json"))
		srv := restart(cfg)
		hn := &harness{t: t, h: srv.Handler(), now: time.Now()}
		var p decision.Policy
		hn.call("root", "GET", "/v1/settings/policy", nil, http.StatusOK, &p)
		without := p
		without.Roles = map[string][]string{}
		hn.call("root", "PUT", "/v1/settings/policy", without, http.StatusOK, nil)
		hn.call("root", "PUT", "/v1/settings/policy", p, http.StatusOK, nil)
		if got := srv.rbac.Roles("alice"); len(got) != 0 {
			t.Fatalf("re-adding the role restored it: %v", got)
		}
		if got := restart(cfg).rbac.Roles("alice"); len(got) != 0 {
			t.Fatalf("a restart gave back revoked roles: %v", got)
		}
	})
}

func TestConfigEditsReplaceOnlyWhatChanged(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	cfg := residents(filepath.Join(t.TempDir(), "attest.json"))
	start := func() *Server {
		t.Helper()
		srv, err := New(cfg, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		return srv
	}
	hn := &harness{t: t, h: start().Handler(), now: time.Now()}
	var p decision.Policy
	hn.call("root", "GET", "/v1/settings/policy", nil, http.StatusOK, &p)
	p.Permissions["music.play"] = "presumed"
	hn.call("root", "PUT", "/v1/settings/policy", p, http.StatusOK, nil)

	// Assignments are first-start role state, not policy: editing them in
	// the file leaves the runtime policy alone.
	cfg.Policy.Assignments = map[string][]string{"alice": {"resident"}, "bob": {"resident"}}
	if _, ok := start().cur().policy().Permissions["music.play"]; !ok {
		t.Fatal("editing config assignments replaced the runtime policy")
	}
	if strings.Contains(logs.String(), "replaces the value set at runtime") {
		t.Fatalf("logged an override that did not happen:\n%s", logs.String())
	}
	// Editing the policy in the file replaces it whole, and says so.
	cfg.Policy.Floor = []string{"emergency.call"}
	srv := start()
	if _, ok := srv.cur().policy().Permissions["music.play"]; ok {
		t.Fatal("an edited config policy did not replace the runtime one")
	}
	if !strings.Contains(logs.String(), "config file: policy changed in the file and replaces the value set at runtime") {
		t.Fatalf("no override line in the log:\n%s", logs.String())
	}
}

func TestSettingsNeedADataFile(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	// Unconfigured and in memory: nothing set up could survive a restart.
	srv, err := New(Config{Fusion: perception.DefaultConfig()}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if srv.setup.code != "" || strings.Contains(logs.String(), "setup code") {
		t.Fatal("printed a setup code that could not set anything up")
	}
	c := newConsole(t, srv.Handler())
	var r map[string]string
	c.do("POST", "/v1/setup/session", map[string]string{"code": "ANY"}, http.StatusConflict, &r)
	if !strings.Contains(r["error"], "data file") {
		t.Fatalf("unclear refusal: %q", r["error"])
	}

	// The in-memory quickstart keeps working for everything but settings.
	hn := newHarness(t)
	hn.call("admin", "GET", "/v1/settings", nil, http.StatusOK, nil)
	hn.call("admin", "PUT", "/v1/settings/public-url", map[string]string{"publicUrl": "http://attest.home"}, http.StatusConflict, nil)
	var p decision.Policy
	hn.call("admin", "GET", "/v1/settings/policy", nil, http.StatusOK, &p)
	hn.call("admin", "PUT", "/v1/settings/policy", p, http.StatusConflict, nil)
	hn.call("admin", "POST", "/v1/settings/provider", map[string]string{"kind": "oidc", "issuer": "http://127.0.0.1:1"}, http.StatusConflict, nil)
	hn.call("admin", "PUT", "/v1/identities/eve", map[string]string{"displayName": "Eve"}, http.StatusOK, nil)
	if d := hn.decide("", "emergency.call"); !d.Granted {
		t.Fatalf("decisions stopped working: %+v", d)
	}
}

func TestConnectStartedInSetupCannotLandAfterIt(t *testing.T) {
	is, kc := newIssuer(t), newKeycloak(t)
	now := time.Now()
	srv, _ := unconfigured(t, &now)
	c := openSetup(t, srv)
	c.do("PUT", "/v1/settings/public-url", map[string]string{"publicUrl": "http://attest.home"}, http.StatusOK, nil)
	c.do("POST", "/v1/settings/provider", map[string]string{"kind": "oidc", "issuer": is.srv.URL, "clientId": "attest-enrol"}, http.StatusOK, nil)

	kc.gate = make(chan struct{})
	done := make(chan *connectReply)
	go func() {
		var r connectReply
		// Its own console: the main one's helper is not safe to share.
		slow := &console{t: t, base: c.base, c: c.c, csrf: c.csrf}
		slow.do("POST", "/v1/settings/provider", connectKeycloak(map[string]string{"username": "ana", "password": kcPassword}, "", kc), http.StatusConflict, &r)
		done <- &r
	}()
	// Wait until the connect is at the provider, then finish setup under it.
	for kc.waiting() == 0 {
		time.Sleep(time.Millisecond)
	}
	c.do("POST", "/v1/setup/complete", nil, http.StatusOK, nil)
	close(kc.gate)
	if r := <-done; !strings.Contains(r.Error, "setup completed") {
		t.Fatalf("late connect: %+v", r)
	}
	if got := srv.cur().settings.OIDC.Issuer; got != is.srv.URL {
		t.Fatalf("a connect from a dead setup session was stored: %s", got)
	}
}
