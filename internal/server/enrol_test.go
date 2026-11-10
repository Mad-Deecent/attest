package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mad-deecent/attest/internal/decision"
	"github.com/mad-deecent/attest/internal/identity"
	"github.com/mad-deecent/attest/internal/perception"
)

// enrolEnv is attest served over HTTP next to a fake identity provider, so a
// cookie-keeping client can walk the whole sign-in redirect chain.
type enrolEnv struct {
	t   *testing.T
	is  *issuer
	ts  *httptest.Server
	cfg Config
	srv *Server
	api *harness
}

func newEnrolEnv(t *testing.T, data string) *enrolEnv {
	t.Helper()
	e := &enrolEnv{t: t, is: newIssuer(t)}
	e.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.api.h.ServeHTTP(w, r)
	}))
	t.Cleanup(e.ts.Close)
	e.cfg = Config{
		Tokens: map[string]Principal{
			"admin": {Kind: KindAdmin, ID: "admin"},
			"face":  {Kind: KindProducer, ID: "cam"},
			"voice": {Kind: KindProducer, ID: "mic"},
			"agent": {Kind: KindAgent, ID: "concierge"},
		},
		OIDC:      &OIDCConfig{Issuer: e.is.srv.URL, ClientID: "attest-enrol"},
		PublicURL: e.ts.URL,
		Enrol:     EnrolConfig{VoiceURL: "http://voice.invalid/enrol"},
		Data:      data,
		Fusion:    perception.DefaultConfig(),
		Policy: decision.Policy{
			Permissions: map[string]string{
				"music.control": "presumed", "calendar.read": "confident", "door.unlock": "corroborated",
			},
			Floor:  []string{"emergency.call"},
			Agents: map[string][]string{"concierge": {"*"}, "doorman": {"door.unlock", "emergency.call"}},
			Roles: map[string][]string{
				"resident": {"music.control", "calendar.read", "door.unlock", "emergency.call"},
				"guest":    {"music.control"},
			},
		},
	}
	e.restart()
	return e
}

// restart replaces the server with a new one built from the same config.
func (e *enrolEnv) restart() {
	e.t.Helper()
	srv, err := New(e.cfg, time.Now)
	if err != nil {
		e.t.Fatal(err)
	}
	e.srv = srv
	e.api = &harness{t: e.t, h: srv.Handler(), now: time.Now()}
}

type browser struct {
	t    *testing.T
	c    *http.Client
	base string
	body string
}

// signIn walks /enrol/login through the provider and back as p, in a fresh
// browser.
func (e *enrolEnv) signIn(p person) *browser {
	e.t.Helper()
	e.is.signIn(p)
	jar, _ := cookiejar.New(nil)
	b := &browser{t: e.t, c: &http.Client{Jar: jar}, base: e.ts.URL}
	if code := b.get("/enrol/login"); code != http.StatusOK {
		e.t.Fatalf("sign-in ended with %d: %s", code, b.body)
	}
	if !strings.Contains(b.body, "Sign out") {
		e.t.Fatalf("not signed in after the flow: %s", b.body)
	}
	return b
}

func (b *browser) get(path string) int {
	b.t.Helper()
	res, err := b.c.Get(b.base + path)
	if err != nil {
		b.t.Fatal(err)
	}
	return b.read(res)
}

func (b *browser) post(path string, form url.Values, header ...string) int {
	b.t.Helper()
	req, _ := http.NewRequest("POST", b.base+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	res, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	return b.read(res)
}

func (b *browser) read(res *http.Response) int {
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	b.body = string(raw)
	return res.StatusCode
}

var csrfField = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func (b *browser) csrf() string {
	b.t.Helper()
	m := csrfField.FindStringSubmatch(b.body)
	if m == nil {
		b.t.Fatalf("no CSRF token on page: %s", b.body)
	}
	return m[1]
}

func (b *browser) consent(perms map[string][]string) {
	b.t.Helper()
	form := url.Values{"csrf": {b.csrf()}}
	for agent, ps := range perms {
		form["perm:"+agent] = ps
	}
	if code := b.post("/enrol/consent", form); code != http.StatusOK || !strings.Contains(b.body, "Your choices are saved.") {
		b.t.Fatalf("consent: %d %s", code, b.body)
	}
}

func (e *enrolEnv) identities() []identity.Identity {
	e.t.Helper()
	var out []identity.Identity
	e.api.call("admin", "GET", "/v1/identities", nil, http.StatusOK, &out)
	return out
}

func (e *enrolEnv) delegations() []decision.Delegation {
	e.t.Helper()
	var out []decision.Delegation
	e.api.call("admin", "GET", "/v1/delegations", nil, http.StatusOK, &out)
	return out
}

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

var ana = person{sub: "kc-ana", name: "Ana Lima", username: "ana", accessRoles: []string{"resident", "offline_access"}}

func TestSignInLinksAnIdentityAndAdoptsRoles(t *testing.T) {
	e := newEnrolEnv(t, "")
	b := e.signIn(ana)
	if !strings.Contains(b.body, "Ana Lima") || !strings.Contains(b.body, `value="door.unlock"`) {
		t.Fatalf("signed-in page missing name or choices: %s", b.body)
	}
	ids := e.identities()
	if len(ids) != 1 {
		t.Fatalf("want one identity, got %+v", ids)
	}
	got := ids[0]
	if !uuidV4.MatchString(got.ID) || got.DisplayName != "Ana Lima" {
		t.Fatalf("identity %+v: want a UUID id and the provider's name", got)
	}
	if got.Provider == nil || *got.Provider != (identity.Provider{Issuer: e.is.srv.URL, Subject: "kc-ana"}) {
		t.Fatalf("provider link = %+v", got.Provider)
	}
	// Roles come from the access token when the ID token has none, and only
	// roles the policy defines are kept.
	if roles := e.srv.rbac.Roles(got.ID); !slices.Equal(roles, []string{"resident"}) {
		t.Fatalf("roles = %v, want [resident]", roles)
	}

	// Signing in again reuses the identity and refreshes roles and name.
	again := ana
	again.name = ""
	again.idRoles = []string{"guest"}
	e.signIn(again)
	ids = e.identities()
	if len(ids) != 1 || ids[0].ID != got.ID || ids[0].DisplayName != "ana" {
		t.Fatalf("second sign-in: %+v", ids)
	}
	if roles := e.srv.rbac.Roles(got.ID); !slices.Equal(roles, []string{"guest"}) {
		t.Fatalf("roles = %v, want [guest] from the ID token", roles)
	}

	e.signIn(person{sub: "kc-ben", name: "Ben"})
	if ids = e.identities(); len(ids) != 2 {
		t.Fatalf("a different subject must be a different identity: %+v", ids)
	}
}

func TestConsentGrantsAndRevokeDenies(t *testing.T) {
	e := newEnrolEnv(t, "")
	b := e.signIn(ana)
	id := e.identities()[0].ID

	// Only offered permissions are recorded; "*" and floor permissions are not.
	b.consent(map[string][]string{"concierge": {"music.control", "*", "emergency.call"}})
	if !strings.Contains(b.body, "may: music control") {
		t.Fatalf("page does not list the new consent: %s", b.body)
	}
	dels := e.delegations()
	if len(dels) != 1 || !slices.Equal(dels[0].Permissions, []string{"music.control"}) ||
		dels[0].GrantedBy != id || dels[0].Agent != "concierge" {
		t.Fatalf("delegations = %+v", dels)
	}

	e.api.see("face", "track-1", "face", id, 8)
	decide := func(permission string) decision.Decision {
		var d decision.Decision
		e.api.call("agent", "POST", "/v1/decisions",
			decision.Request{Subject: "track-1", Permission: permission}, http.StatusOK, &d)
		return d
	}
	if d := decide("music.control"); !d.Granted || *d.IdentityID != id {
		t.Fatalf("want grant, got %+v", d)
	}
	denied(t, decide("calendar.read"), "no live delegation")

	// Saving a different choice replaces the consent.
	b.consent(map[string][]string{"concierge": {"music.control", "calendar.read"}})
	if d := decide("calendar.read"); !d.Granted {
		t.Fatalf("want grant after widening, got %+v", d)
	}
	live := e.srv.liveDelegations(id, "", time.Now())
	if len(live) != 1 {
		t.Fatalf("want the old consent replaced, got %+v", live)
	}

	// Someone else cannot withdraw it.
	ben := e.signIn(person{sub: "kc-ben", name: "Ben"})
	if code := ben.post("/enrol/revoke", url.Values{"csrf": {ben.csrf()}, "id": {live[0].ID}}); code != http.StatusNotFound {
		t.Fatalf("revoking another person's consent: %d", code)
	}

	if code := b.post("/enrol/revoke", url.Values{"csrf": {b.csrf()}, "id": {live[0].ID}}); code != http.StatusOK {
		t.Fatalf("revoke: %d %s", code, b.body)
	}
	denied(t, decide("music.control"), "no live delegation")
	if code := b.post("/enrol/revoke", url.Values{"csrf": {b.csrf()}, "id": {live[0].ID}}); code != http.StatusNotFound {
		t.Fatalf("revoking twice: %d", code)
	}
}

func TestEnrolmentFormsNeedTheCSRFToken(t *testing.T) {
	e := newEnrolEnv(t, "")
	b := e.signIn(ana)
	token := b.csrf()
	consent := url.Values{"perm:concierge": {"music.control"}}
	if code := b.post("/enrol/consent", consent); code != http.StatusForbidden {
		t.Fatalf("consent without token: %d", code)
	}
	consent.Set("csrf", "forged")
	if code := b.post("/enrol/consent", consent); code != http.StatusForbidden {
		t.Fatalf("consent with a wrong token: %d", code)
	}
	if code := b.post("/enrol/ticket", nil); code != http.StatusForbidden {
		t.Fatalf("ticket without token: %d", code)
	}
	if code := b.post("/enrol/logout", url.Values{"csrf": {token}}); code != http.StatusOK || !strings.Contains(b.body, "signed out") {
		t.Fatalf("logout: %d", code)
	}
	if n := len(e.delegations()); n != 0 {
		t.Fatalf("no form should have taken effect, got %d delegations", n)
	}
}

func TestEnrolmentTicketIsRedeemedOnce(t *testing.T) {
	e := newEnrolEnv(t, "")
	b := e.signIn(ana)
	id := e.identities()[0].ID
	if code := b.post("/enrol/ticket", nil, "X-CSRF-Token", b.csrf()); code != http.StatusOK {
		t.Fatalf("ticket: %d %s", code, b.body)
	}
	var issued struct{ Ticket string }
	if err := json.Unmarshal([]byte(b.body), &issued); err != nil || issued.Ticket == "" {
		t.Fatalf("ticket body %q: %v", b.body, err)
	}

	redeem := map[string]string{"ticket": issued.Ticket}
	e.api.call("agent", "POST", "/v1/enrolment-tickets/redeem", redeem, http.StatusForbidden, nil)
	var out struct{ IdentityID string }
	e.api.call("voice", "POST", "/v1/enrolment-tickets/redeem", redeem, http.StatusOK, &out)
	if out.IdentityID != id {
		t.Fatalf("redeemed for %q, want %q", out.IdentityID, id)
	}
	e.api.call("voice", "POST", "/v1/enrolment-tickets/redeem", redeem, http.StatusNotFound, nil)
	e.api.call("voice", "POST", "/v1/enrolment-tickets/redeem", map[string]string{"ticket": "made-up"}, http.StatusNotFound, nil)

	if e.identities()[0].VoiceEnrolledAt == nil {
		t.Fatal("redeeming must record voiceEnrolledAt")
	}
	b.get("/enrol")
	if !strings.Contains(b.body, "Your voice was saved on") {
		t.Fatalf("page does not show the voice step done: %s", b.body)
	}
}

func TestStateSurvivesARestart(t *testing.T) {
	data := filepath.Join(t.TempDir(), "attest.json")
	e := newEnrolEnv(t, data)
	b := e.signIn(ana)
	id := e.identities()[0].ID
	b.consent(map[string][]string{"concierge": {"music.control"}})
	e.api.call("admin", "PUT", "/v1/identities/dee", map[string]string{"displayName": "Dee"}, http.StatusOK, nil)
	e.api.call("admin", "PUT", "/v1/identities/dee/roles", map[string][]string{"roles": {"guest"}}, http.StatusOK, nil)
	var revoked decision.Delegation
	e.api.call("admin", "POST", "/v1/delegations", decision.Delegation{
		IdentityID: "dee", Agent: "concierge", Permissions: []string{"*"}, GrantedBy: "admin",
	}, http.StatusCreated, &revoked)
	e.api.call("admin", "DELETE", "/v1/delegations/"+revoked.ID, nil, http.StatusNoContent, nil)

	info, err := os.Stat(data)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("data file: %v %v", info, err)
	}
	identities, delegations := e.identities(), e.delegations()

	e.restart()
	if got := e.identities(); !jsonEqual(got, identities) {
		t.Fatalf("identities after restart:\n%+v\nwant\n%+v", got, identities)
	}
	if got := e.delegations(); !jsonEqual(got, delegations) {
		t.Fatalf("delegations after restart:\n%+v\nwant\n%+v", got, delegations)
	}
	if roles := e.srv.rbac.Roles("dee"); !slices.Equal(roles, []string{"guest"}) {
		t.Fatalf("roles after restart = %v", roles)
	}
	e.api.see("face", "track-1", "face", id, 8)
	var d decision.Decision
	e.api.call("agent", "POST", "/v1/decisions", decision.Request{Subject: "track-1", Permission: "music.control"}, http.StatusOK, &d)
	if !d.Granted {
		t.Fatalf("consent must survive a restart: %+v", d)
	}

	// The id counter survives too, so new delegations never reuse an id.
	var next decision.Delegation
	e.api.call("admin", "POST", "/v1/delegations", decision.Delegation{
		IdentityID: "dee", Agent: "concierge", Permissions: []string{"music.control"}, GrantedBy: "admin",
	}, http.StatusCreated, &next)
	if slices.ContainsFunc(delegations, func(d decision.Delegation) bool { return d.ID == next.ID }) {
		t.Fatalf("delegation id %s reused after restart", next.ID)
	}

	// The provider link survives, so signing in again finds the same person.
	e.signIn(ana)
	if got := e.identities(); len(got) != 2 {
		t.Fatalf("sign-in after restart created a duplicate: %+v", got)
	}
}

func TestAFailedSaveFailsTheRequest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gone")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	e := newEnrolEnv(t, filepath.Join(dir, "attest.json"))
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	e.api.call("admin", "PUT", "/v1/identities/dee", map[string]string{"displayName": "Dee"}, http.StatusInternalServerError, nil)
}

func TestEnrolPageWithoutAProvider(t *testing.T) {
	hn := newHarness(t)
	rec := httptest.NewRecorder()
	hn.h.ServeHTTP(rec, httptest.NewRequest("GET", "/enrol", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "identity provider") {
		t.Fatalf("unconfigured page: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "/enrol/login") {
		t.Fatal("unconfigured page must not offer sign-in")
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func TestConsentLeavesAdministratorDelegationsAlone(t *testing.T) {
	e := newEnrolEnv(t, "")
	b := e.signIn(ana)
	id := e.identities()[0].ID
	until := time.Now().Add(30 * 24 * time.Hour)
	var managed decision.Delegation
	e.api.call("admin", "POST", "/v1/delegations", decision.Delegation{
		IdentityID: id, Agent: "concierge", Permissions: []string{"calendar.read"},
		Zones: []string{"car"}, Ceiling: "confident", ExpiresAt: &until, GrantedBy: "admin",
	}, http.StatusCreated, &managed)

	b.get("/enrol")
	if !strings.Contains(b.body, "Set up for you by whoever runs this system (only in: car;") ||
		!strings.Contains(b.body, "until "+day(until)) {
		t.Fatalf("administrator delegation not shown with its limits: %s", b.body)
	}
	// It is shown, not offered as the person's own choice or revocable.
	if strings.Contains(b.body, `value="calendar.read" checked`) || strings.Contains(b.body, `value="`+managed.ID+`"`) {
		t.Fatalf("administrator delegation must be read-only: %s", b.body)
	}

	stillLive := func() {
		t.Helper()
		for _, d := range e.delegations() {
			if d.ID == managed.ID && d.RevokedAt == nil {
				return
			}
		}
		t.Fatalf("administrator delegation was touched: %+v", e.delegations())
	}
	b.consent(map[string][]string{"concierge": {"music.control"}})
	stillLive()
	own := e.srv.ownConsent(id, "concierge", time.Now())
	if len(own) != 1 || !slices.Equal(own[0].Permissions, []string{"music.control"}) {
		t.Fatalf("own consent = %+v", own)
	}
	// Unticking everything withdraws only the person's own consent.
	b.consent(map[string][]string{})
	stillLive()
	if own := e.srv.ownConsent(id, "concierge", time.Now()); len(own) != 0 {
		t.Fatalf("own consent should be gone: %+v", own)
	}
	if code := b.post("/enrol/revoke", url.Values{"csrf": {b.csrf()}, "id": {managed.ID}}); code != http.StatusForbidden {
		t.Fatalf("revoking an administrator delegation: %d", code)
	}
	stillLive()
}

func TestPermissionLabels(t *testing.T) {
	e := newEnrolEnv(t, "")
	e.cfg.Policy.Labels = map[string]string{"calendar.read": "Read your calendar"}
	e.restart()
	b := e.signIn(ana)
	if !strings.Contains(b.body, `value="calendar.read"> Read your calendar`) || !strings.Contains(b.body, "> Music control") {
		t.Fatalf("labels: %s", b.body)
	}
	b.consent(map[string][]string{"concierge": {"calendar.read", "music.control"}})
	if !strings.Contains(b.body, "may: Read your calendar, music control.") {
		t.Fatalf("granted list: %s", b.body)
	}

	e.cfg.Policy.Labels = map[string]string{"teleport": "Teleport you"}
	if _, err := New(e.cfg, time.Now); err == nil || !strings.Contains(err.Error(), "teleport") {
		t.Fatalf("a label for an undefined permission must be rejected, got %v", err)
	}
}

func TestOnlyTheVoiceProducerRedeemsTickets(t *testing.T) {
	e := newEnrolEnv(t, "")
	e.cfg.Enrol.VoiceProducer = "mic"
	e.restart()
	b := e.signIn(ana)
	ticket := func() map[string]string {
		t.Helper()
		if code := b.post("/enrol/ticket", nil, "X-CSRF-Token", b.csrf()); code != http.StatusOK {
			t.Fatalf("ticket: %d %s", code, b.body)
		}
		var issued struct{ Ticket string }
		_ = json.Unmarshal([]byte(b.body), &issued)
		b.get("/enrol") // back to a page with the CSRF token
		return map[string]string{"ticket": issued.Ticket}
	}

	first := ticket()
	// Another producer is refused without spending the ticket.
	e.api.call("face", "POST", "/v1/enrolment-tickets/redeem", first, http.StatusForbidden, nil)
	// A newer ticket replaces it.
	second := ticket()
	e.api.call("voice", "POST", "/v1/enrolment-tickets/redeem", first, http.StatusNotFound, nil)
	e.api.call("voice", "POST", "/v1/enrolment-tickets/redeem", second, http.StatusOK, nil)
}

func TestEnrolmentLivesOnThePublicHost(t *testing.T) {
	e := newEnrolEnv(t, "")
	serve := func(method, target, host string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, nil)
		req.Host = host
		rec := httptest.NewRecorder()
		e.api.h.ServeHTTP(rec, req)
		return rec
	}
	public, _ := url.Parse(e.ts.URL)
	for _, method := range []string{"GET", "POST"} {
		path := "/enrol/login?next=1"
		if method == "POST" {
			path = "/enrol/consent"
		}
		rec := serve(method, path, "attest.lan:8420")
		if rec.Code != http.StatusTemporaryRedirect || rec.Header().Get("Location") != e.ts.URL+path {
			t.Fatalf("%s %s on another host: %d %q", method, path, rec.Code, rec.Header().Get("Location"))
		}
	}
	if rec := serve("GET", "/enrol", strings.ToUpper(public.Host)); rec.Code != http.StatusOK {
		t.Fatalf("the public host, in any case, is served: %d", rec.Code)
	}
	// The API is not enrolment and is not redirected.
	if rec := serve("GET", "/healthz", "attest.lan:8420"); rec.Code != http.StatusNoContent {
		t.Fatalf("healthz: %d", rec.Code)
	}
}

func TestDataDirectoryIsCheckedAtStartup(t *testing.T) {
	cfg := Config{Fusion: perception.DefaultConfig(), Data: filepath.Join(t.TempDir(), "missing", "attest.json")}
	if _, err := New(cfg, time.Now); err == nil {
		t.Fatal("a missing data directory must fail startup")
	}
	cfg.Data = filepath.Join(t.TempDir(), "attest.json")
	if _, err := New(cfg, time.Now); err != nil {
		t.Fatalf("writable data directory: %v", err)
	}
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	cfg.Data = filepath.Join(dir, "attest.json")
	if _, err := New(cfg, time.Now); err == nil || !strings.Contains(err.Error(), "not writable") {
		t.Fatalf("a read-only data directory must fail startup, got %v", err)
	}
}
