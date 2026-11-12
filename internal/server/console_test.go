package server

import (
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// admin holds the provider's admin role; ana (enroll_test.go) does not.
var admin = person{sub: "kc-admin", name: "Sam Admin", username: "sam", accessRoles: []string{"attest-admin", "resident"}}

// adminBrowser is a browser on the console: cookies kept, redirects followed.
type adminBrowser struct {
	t    *testing.T
	c    *http.Client
	jar  *cookiejar.Jar
	base string
	// at is where the last navigation ended.
	at   *url.URL
	csrf string
}

func (e *enrollEnv) newAdminBrowser() *adminBrowser {
	jar, _ := cookiejar.New(nil)
	return &adminBrowser{t: e.t, c: &http.Client{Jar: jar}, jar: jar, base: e.ts.URL}
}

// consoleSignIn walks /console/login through the provider and back as p.
func (e *enrollEnv) consoleSignIn(p person, back string) *adminBrowser {
	e.t.Helper()
	b := e.newAdminBrowser()
	b.signIn(e, p, back)
	return b
}

func (b *adminBrowser) signIn(e *enrollEnv, p person, back string) {
	b.t.Helper()
	e.is.signIn(p)
	b.navigate("/console/login?return=" + url.QueryEscape(back))
	var info struct {
		SignedIn bool   `json:"signedIn"`
		CSRF     string `json:"csrf"`
	}
	b.api("GET", "/v1/console/session", nil, http.StatusOK, &info)
	b.csrf = info.CSRF
}

func (b *adminBrowser) navigate(path string) {
	b.t.Helper()
	res, err := b.c.Get(b.base + path)
	if err != nil {
		b.t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		b.t.Fatalf("GET %s ended at %s with %d", path, res.Request.URL, res.StatusCode)
	}
	b.at = res.Request.URL
}

// api calls the API with the session cookie, sending b.csrf when set.
func (b *adminBrowser) api(method, path string, body any, want int, into any) string {
	b.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, b.base+path, &buf)
	if b.csrf != "" {
		req.Header.Set("X-CSRF-Token", b.csrf)
	}
	res, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != want {
		b.t.Fatalf("%s %s: %d, want %d: %s", method, path, res.StatusCode, want, raw)
	}
	if into != nil {
		if err := json.Unmarshal(raw, into); err != nil {
			b.t.Fatal(err)
		}
	}
	return string(raw)
}

func (b *adminBrowser) cookie(name string) string {
	u, _ := url.Parse(b.base + "/v1/")
	for _, c := range b.jar.Cookies(u) {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

func (b *adminBrowser) setCookie(name, value string) {
	u, _ := url.Parse(b.base + "/")
	b.jar.SetCookies(u, []*http.Cookie{{Name: name, Value: value, Path: "/"}})
}

func TestConsoleSignInNeedsTheAdminRole(t *testing.T) {
	e := newEnrollEnv(t, "")
	b := e.consoleSignIn(admin, "/console/policy/door.unlock")
	if b.at.Path != "/console/policy/door.unlock" {
		t.Fatalf("after sign-in the console is at %s, want the return path", b.at)
	}
	var info struct {
		SignedIn bool      `json:"signedIn"`
		Name     string    `json:"name"`
		CSRF     string    `json:"csrf"`
		Expires  time.Time `json:"expiresAt"`
	}
	b.api("GET", "/v1/console/session", nil, http.StatusOK, &info)
	if !info.SignedIn || info.Name != "Sam Admin" || info.CSRF == "" || info.Expires.IsZero() {
		t.Fatalf("session: %+v", info)
	}
	// The session is an admin credential for settings and the admin API.
	b.api("GET", "/v1/settings", nil, http.StatusOK, nil)
	b.api("GET", "/v1/identities", nil, http.StatusOK, nil)
	// Signing in to the console enrolls nobody.
	if ids := e.identities(); len(ids) != 0 {
		t.Fatalf("console sign-in created identities: %+v", ids)
	}

	// Someone without the role is told so, and holds no session.
	n := e.consoleSignIn(ana, "/console/policy")
	if n.at.Path != "/console/no-access" || n.cookie(consoleCookie) != "" {
		t.Fatalf("a non-admin ended at %s with cookie %q", n.at, n.cookie(consoleCookie))
	}
	n.api("GET", "/v1/settings", nil, http.StatusUnauthorized, nil)
	n.api("GET", "/v1/identities", nil, http.StatusUnauthorized, nil)
	var refused map[string]any
	n.api("GET", "/v1/console/session", nil, http.StatusOK, &refused)
	if refused["signedIn"] != false || refused["csrf"] != nil {
		t.Fatalf("a refused sign-in has a session: %v", refused)
	}

	// The role counts wherever the provider puts it, under whatever name the
	// connection gives it.
	idOnly := admin
	idOnly.accessRoles, idOnly.idRoles = nil, []string{"attest-admin"}
	e.consoleSignIn(idOnly, "").api("GET", "/v1/settings", nil, http.StatusOK, nil)
	e.cfg.OIDC.Roles = map[string]string{KindAdmin: "site-ops"}
	e.restart()
	e.consoleSignIn(admin, "").api("GET", "/v1/settings", nil, http.StatusUnauthorized, nil)
	ops := admin
	ops.accessRoles = []string{"site-ops"}
	e.consoleSignIn(ops, "").api("GET", "/v1/settings", nil, http.StatusOK, nil)
}

func TestConsoleWritesNeedTheCSRFToken(t *testing.T) {
	e := newEnrollEnv(t, filepath.Join(t.TempDir(), "attest.json"))
	b := e.consoleSignIn(admin, "")
	token := b.csrf
	move := map[string]string{"publicUrl": e.ts.URL + "/"}
	for _, csrf := range []string{"", "wrong", "x" + token} {
		b.csrf = csrf
		b.api("PUT", "/v1/settings/public-url", move, http.StatusForbidden, nil)
		b.api("PUT", "/v1/settings/policy", e.srv.cur().policy(), http.StatusForbidden, nil)
		b.api("PUT", "/v1/identities/x", map[string]string{"displayName": "X"}, http.StatusForbidden, nil)
		b.api("POST", "/v1/console/sign-out", nil, http.StatusForbidden, nil)
	}
	// Reading needs no token.
	b.api("GET", "/v1/settings/policy", nil, http.StatusOK, nil)
	b.csrf = token
	b.api("PUT", "/v1/settings/public-url", move, http.StatusOK, nil)
	b.api("PUT", "/v1/settings/policy", e.srv.cur().policy(), http.StatusOK, nil)
}

func TestConsoleSessionsExpire(t *testing.T) {
	e := newEnrollEnv(t, "")
	clock := time.Now()
	e.srv.now = func() time.Time { return clock }
	idle := e.consoleSignIn(admin, "")
	busy := e.consoleSignIn(admin, "")

	clock = clock.Add(consoleIdle - time.Minute)
	idle.api("GET", "/v1/settings", nil, http.StatusOK, nil)
	busy.api("GET", "/v1/settings", nil, http.StatusOK, nil)
	clock = clock.Add(consoleIdle + time.Second)
	idle.api("GET", "/v1/settings", nil, http.StatusUnauthorized, nil)

	// Use keeps a session alive, but never past its absolute end.
	busy2 := e.consoleSignIn(admin, "")
	started := clock
	for clock.Before(started.Add(consoleMax - consoleIdle/2)) {
		clock = clock.Add(consoleIdle / 2)
		busy2.api("GET", "/v1/settings", nil, http.StatusOK, nil)
	}
	clock = started.Add(consoleMax)
	busy2.api("GET", "/v1/settings", nil, http.StatusUnauthorized, nil)
	var info map[string]any
	busy2.api("GET", "/v1/console/session", nil, http.StatusOK, &info)
	if info["signedIn"] != false {
		t.Fatalf("an expired session reports signed in: %v", info)
	}
}

func TestConsoleSignOut(t *testing.T) {
	e := newEnrollEnv(t, "")
	b := e.consoleSignIn(admin, "")
	other := e.consoleSignIn(admin, "")
	b.api("POST", "/v1/console/sign-out", nil, http.StatusNoContent, nil)
	if b.cookie(consoleCookie) != "" {
		t.Fatal("sign-out left the session cookie")
	}
	b.api("GET", "/v1/settings", nil, http.StatusUnauthorized, nil)
	// Signing out one browser leaves the others signed in.
	other.api("GET", "/v1/settings", nil, http.StatusOK, nil)
	// Signing out twice is harmless.
	b.api("POST", "/v1/console/sign-out", nil, http.StatusNoContent, nil)
}

func TestConsoleSignInStartsAFreshSession(t *testing.T) {
	e := newEnrollEnv(t, "")
	victim := e.newAdminBrowser()
	victim.setCookie(consoleCookie, "planted-by-someone-else")
	victim.signIn(e, admin, "")
	if got := victim.cookie(consoleCookie); got == "" || got == "planted-by-someone-else" {
		t.Fatalf("session id after sign-in: %q", got)
	}

	// A live session id carried into a sign-in is ended, not adopted.
	first := e.consoleSignIn(admin, "")
	carried := e.newAdminBrowser()
	carried.setCookie(consoleCookie, first.cookie(consoleCookie))
	carried.signIn(e, admin, "")
	if carried.cookie(consoleCookie) == first.cookie(consoleCookie) {
		t.Fatal("sign-in kept the session id it was given")
	}
	first.api("GET", "/v1/settings", nil, http.StatusUnauthorized, nil)
	carried.api("GET", "/v1/settings", nil, http.StatusOK, nil)
}

func TestConsoleReturnPathStaysInTheConsole(t *testing.T) {
	for raw, want := range map[string]string{
		"":                             "/console/",
		"/console/":                    "/console/",
		"/console":                     "/console/",
		"/console/policy":              "/console/policy",
		"/console/policy/door.unlock":  "/console/policy/door.unlock",
		"/console/connections/?x=1#y":  "/console/connections/",
		"https://evil.example/console": "/console/",
		"//evil.example/console/":      "/console/",
		"/console//evil.example":       "/console/",
		"/\\evil.example":              "/console/",
		"/console/\\evil.example":      "/console/",
		"/console/../enroll":           "/console/",
		"/console/%2e%2e/enroll":       "/console/",
		"/console/login":               "/console/",
		"/enroll":                      "/console/",
		"javascript:alert(1)":          "/console/",
		"/console/\r\nSet-Cookie: x=1": "/console/",
		"/console/" + strings.Repeat("a", maxReturn): "/console/",
	} {
		if got := safeReturn(raw); got != want {
			t.Errorf("safeReturn(%q) = %q, want %q", raw, got, want)
		}
	}
	e := newEnrollEnv(t, "")
	if b := e.consoleSignIn(admin, "//evil.example/"); b.at.Host != strings.TrimPrefix(e.ts.URL, "http://") || b.at.Path != "/console/" {
		t.Fatalf("an off-site return path sent the browser to %s", b.at)
	}
}

func TestBearerAccessIsUnchangedByTheConsole(t *testing.T) {
	e := newEnrollEnv(t, "")
	b := e.consoleSignIn(admin, "")
	call := func(token string, want int) {
		t.Helper()
		req, _ := http.NewRequest("GET", e.ts.URL+"/v1/settings", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := b.c.Do(req) // carries the console cookie
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != want {
			t.Fatalf("bearer %q with a console cookie: %d, want %d", token, res.StatusCode, want)
		}
	}
	// A request with a token is judged on the token alone.
	call("admin", http.StatusOK)
	call("agent", http.StatusForbidden)
	call("nonsense", http.StatusUnauthorized)
	call(e.is.token("ops", time.Now().Add(time.Hour), "attest-admin"), http.StatusOK)
	// And without any cookie, exactly as before.
	e.api.call("admin", "GET", "/v1/settings", nil, http.StatusOK, nil)
	e.api.call("admin", "PUT", "/v1/settings/public-url", map[string]string{"publicUrl": e.ts.URL}, http.StatusConflict, nil)
	e.api.call("face", "GET", "/v1/identities", nil, http.StatusForbidden, nil)
}

func TestSetupHandsOffToTheConsole(t *testing.T) {
	is := newIssuer(t)
	now := time.Now()
	srv, _ := unconfigured(t, &now)
	c := openSetup(t, srv)
	// Before setup there is nobody to sign in, and the setup session is not
	// an admin credential beyond settings.
	b := &adminBrowser{t: t, c: c.c, jar: c.c.Jar.(*cookiejar.Jar), base: c.base}
	b.navigate("/console/login")
	if b.at.Path != "/console/" {
		t.Fatalf("console sign-in before setup ended at %s", b.at)
	}
	c.do("GET", "/v1/identities", nil, http.StatusUnauthorized, nil)
	c.do("GET", "/v1/settings/provider/status", nil, http.StatusOK, nil)

	c.do("PUT", "/v1/settings/public-url", map[string]string{"publicUrl": c.base}, http.StatusOK, nil)
	c.do("POST", "/v1/settings/provider", map[string]string{"kind": "oidc", "issuer": is.srv.URL, "clientId": "attest-enroll"}, http.StatusOK, nil)
	var status struct {
		Reachable bool `json:"reachable"`
	}
	c.do("GET", "/v1/settings/provider/status", nil, http.StatusOK, &status)
	if !status.Reachable {
		t.Fatal("the connected provider is reported unreachable")
	}
	c.do("POST", "/v1/setup/complete", nil, http.StatusOK, nil)
	if b.cookie(setupCookie) != "" {
		t.Fatal("finishing setup left the setup cookie behind")
	}

	// The same browser signs in as the provider's admin, even with a stale
	// setup cookie still around.
	u, _ := url.Parse(c.base + "/v1/")
	b.jar.SetCookies(u, []*http.Cookie{{Name: setupCookie, Value: "stale", Path: "/v1/"}})
	is.signIn(admin)
	b.navigate("/console/login?return=/console/policy")
	if b.at.Path != "/console/policy" {
		t.Fatalf("after setup, sign-in ended at %s", b.at)
	}
	var info struct {
		CSRF string `json:"csrf"`
	}
	b.api("GET", "/v1/console/session", nil, http.StatusOK, &info)
	b.csrf = info.CSRF
	b.api("GET", "/v1/settings", nil, http.StatusOK, nil)
	b.api("PUT", "/v1/settings/public-url", map[string]string{"publicUrl": c.base + "/"}, http.StatusOK, nil)
}

func TestConsoleIsServedWithStrictHeaders(t *testing.T) {
	e := newEnrollEnv(t, "")
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.Host = strings.TrimPrefix(e.ts.URL, "http://")
		rec := httptest.NewRecorder()
		e.api.h.ServeHTTP(rec, req)
		return rec
	}
	// After setup, "/" is the sign-in page, under the same strict policy.
	if rec := get("/"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Continue with your account") ||
		!strings.Contains(rec.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatalf("GET /: %d %v", rec.Code, rec.Header())
	}
	if rec := get("/no-such-thing"); rec.Code != http.StatusNotFound {
		t.Fatalf("an unknown path: %d", rec.Code)
	}
	for _, path := range []string{"/console/", "/console/policy/door.unlock", "/console/static/app.js"} {
		rec := get(path)
		h := rec.Header()
		csp := h.Get("Content-Security-Policy")
		if rec.Code != http.StatusOK || !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") ||
			!strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "unsafe") ||
			h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" || h.Get("X-Frame-Options") != "DENY" {
			t.Fatalf("GET %s: %d %v", path, rec.Code, h)
		}
	}
	if ct := get("/console/static/app.js").Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Fatalf("app.js served as %q", ct)
	}
	if rec := get("/console/static/"); rec.Code != http.StatusNotFound {
		t.Fatalf("static directory listing: %d", rec.Code)
	}
	if rec := get("/console/policy"); !strings.Contains(rec.Body.String(), `type="module"`) {
		t.Fatalf("a console path does not serve the console: %s", rec.Body)
	}

	// The policy forbids inline script and style: keep the files to it.
	walk := func(files fs.FS, path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, _ := fs.ReadFile(files, path)
		text := string(raw)
		if strings.Contains(text, `style="`) || strings.Contains(text, "<style") || strings.Contains(text, " on"+"click=") {
			t.Errorf("%s has inline style or handlers, which the policy blocks", path)
		}
		if strings.HasSuffix(path, ".html") {
			for _, tag := range regexp.MustCompile(`<script[^>]*>`).FindAllString(text, -1) {
				if !strings.Contains(tag, "src=") {
					t.Errorf("%s has an inline script: %s", path, tag)
				}
			}
		}
		return nil
	}
	_ = fs.WalkDir(consoleFiles, "console", func(path string, d fs.DirEntry, err error) error { return walk(consoleFiles, path, d, err) })
	_ = fs.WalkDir(enrollFiles, "enroll", func(path string, d fs.DirEntry, err error) error { return walk(enrollFiles, path, d, err) })
}

func TestConsoleLivesOnThePublicHost(t *testing.T) {
	e := newEnrollEnv(t, "")
	for _, path := range []string{"/console/policy", "/console/login"} {
		req := httptest.NewRequest("GET", path, nil)
		req.Host = "attest.lan:8420"
		rec := httptest.NewRecorder()
		e.api.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusTemporaryRedirect || rec.Header().Get("Location") != e.ts.URL+path {
			t.Fatalf("%s on another host: %d %q", path, rec.Code, rec.Header().Get("Location"))
		}
	}
	// During setup the console stays wherever setup began.
	now := time.Now()
	srv, _ := unconfigured(t, &now)
	req := httptest.NewRequest("GET", "/console/", nil)
	req.Host = "attest.lan:8420"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("console during setup: %d", rec.Code)
	}
}

func TestProviderStatusAsksNow(t *testing.T) {
	e := newEnrollEnv(t, "")
	var status struct {
		Connected bool   `json:"connected"`
		Reachable bool   `json:"reachable"`
		Problem   string `json:"problem"`
	}
	e.api.call("admin", "GET", "/v1/settings/provider/status", nil, http.StatusOK, &status)
	if !status.Connected || !status.Reachable || status.Problem != "" {
		t.Fatalf("status of a live provider: %+v", status)
	}
	e.is.srv.Close()
	e.api.call("admin", "GET", "/v1/settings/provider/status", nil, http.StatusOK, &status)
	if !status.Connected || status.Reachable || status.Problem == "" {
		t.Fatalf("status of a provider that went away: %+v", status)
	}
}

func TestIdentitiesListCarriesRoles(t *testing.T) {
	e := newEnrollEnv(t, "")
	e.signIn(ana)
	var list []identityView
	e.api.call("admin", "GET", "/v1/identities", nil, http.StatusOK, &list)
	if len(list) != 1 || len(list[0].Roles) != 1 || list[0].Roles[0] != "resident" {
		t.Fatalf("identities: %+v", list)
	}
}
