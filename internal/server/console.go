package server

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/mad-deecent/attest/internal/provider"
)

// The web console. attest serves it from the binary: plain ES modules and CSS,
// no build step. Administrators sign in through the connected identity
// provider and must hold its admin role; the console then holds a server-side
// session, named by an HttpOnly cookie, and sends that session's CSRF token on
// every request that changes something, as the setup session does.

//go:embed console
var consoleFiles embed.FS

var (
	consoleStatic, _ = fs.Sub(consoleFiles, "console/static")
	consoleIndex, _  = consoleFiles.ReadFile("console/index.html")
)

const (
	consoleCookie     = "attest_console"
	consoleFlowCookie = "attest_console_signin"
	// consoleIdle ends a session nobody has used for this long; consoleMax
	// ends any session, used or not. Roles removed at the provider take
	// effect at the next sign-in, so consoleMax bounds how long they linger.
	consoleIdle = 30 * time.Minute
	consoleMax  = 8 * time.Hour
	// maxReturn caps the post-sign-in path kept in the flow cookie.
	maxReturn = 512
)

type adminSession struct {
	subject, name, csrf string
	started, seen       time.Time
}

func (a adminSession) expires() time.Time {
	idle, end := a.seen.Add(consoleIdle), a.started.Add(consoleMax)
	if idle.Before(end) {
		return idle
	}
	return end
}

// consoleSessions are kept in memory only: a restart signs everyone out.
type consoleSessions struct {
	mu   sync.Mutex
	byID map[string]adminSession
}

func newConsoleSessions() *consoleSessions {
	return &consoleSessions{byID: map[string]adminSession{}}
}

// start always mints a new id, so a session id planted before sign-in is
// never the one that becomes signed in.
func (c *consoleSessions) start(subject, name string, now time.Time) (string, adminSession) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, s := range c.byID {
		if !now.Before(s.expires()) {
			delete(c.byID, id)
		}
	}
	id := randomToken()
	s := adminSession{subject: subject, name: name, csrf: randomToken(), started: now, seen: now}
	c.byID[id] = s
	return id, s
}

// use returns a live session and counts this as activity.
func (c *consoleSessions) use(id string, now time.Time) (adminSession, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.byID[id]
	if !ok || !now.Before(s.expires()) {
		delete(c.byID, id)
		return adminSession{}, false
	}
	s.seen = now
	c.byID[id] = s
	return s, true
}

func (c *consoleSessions) end(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.byID, id)
}

// consoleSession is the request's live console session, if any.
func (s *Server) consoleSession(r *http.Request) (string, adminSession, bool) {
	c, err := r.Cookie(consoleCookie)
	if err != nil || c.Value == "" {
		return "", adminSession{}, false
	}
	sess, ok := s.console.use(c.Value, s.now())
	return c.Value, sess, ok
}

func csrfMatches(r *http.Request, want string) bool {
	got := r.Header.Get("X-CSRF-Token")
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func changesState(r *http.Request) bool {
	return r.Method != http.MethodGet && r.Method != http.MethodHead
}

// asAdmin admits an admin credential: a bearer token, a console session, or,
// while attest is unconfigured, the setup session. For settings only: the
// setup session may change settings and nothing else.
func (s *Server) asAdmin(next handler) http.HandlerFunc { return s.admin(next, true) }

// asConsoleAdmin admits a bearer token or a console session, never the setup
// session.
func (s *Server) asConsoleAdmin(next handler) http.HandlerFunc { return s.admin(next, false) }

// admin checks, in order: a bearer token, judged on the token alone; a live
// console session; the setup session, while setup is incomplete. A request
// that changes state with a session must carry the session's CSRF token.
func (s *Server) admin(next handler, setupOK bool) http.HandlerFunc {
	bearer := s.as(KindAdmin, next)
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			bearer(w, r)
			return
		}
		if _, sess, ok := s.consoleSession(r); ok {
			if changesState(r) && !csrfMatches(r, sess.csrf) {
				fail(w, http.StatusForbidden, "missing or wrong X-CSRF-Token")
				return
			}
			next(w, r, Principal{Kind: KindAdmin, ID: "console:" + sess.subject})
			return
		}
		c, err := r.Cookie(setupCookie)
		if err != nil || !setupOK || s.cur().settings.SetupComplete {
			bearer(w, r)
			return
		}
		switch s.setup.check(c.Value, r.Header.Get("X-CSRF-Token"), changesState(r), s.now()) {
		case http.StatusOK:
			next(w, r, Principal{Kind: KindAdmin, ID: "setup", viaSetup: true})
		case http.StatusForbidden:
			fail(w, http.StatusForbidden, "missing or wrong X-CSRF-Token")
		default:
			fail(w, http.StatusUnauthorized, "setup session expired or invalid")
		}
	}
}

func (s *Server) consoleRoutes(mux *http.ServeMux) {
	// Exactly "/": a bare "GET /" pattern would catch every unknown path.
	mux.HandleFunc("GET /{$}", s.frontDoor)
	mux.Handle("GET /login", s.canonical(http.HandlerFunc(s.login)))
	mux.Handle("GET /console/static/", consoleHeaders(http.StripPrefix("/console/static/", noListing(http.FileServerFS(consoleStatic)))))
	mux.Handle("GET /console/login", s.canonical(http.HandlerFunc(s.consoleLogin)))
	mux.Handle("GET /console/", s.consoleCanonical(consoleHeaders(http.HandlerFunc(serveConsole))))
	mux.HandleFunc("GET /v1/console/session", s.consoleSessionInfo)
	mux.HandleFunc("POST /v1/console/sign-out", s.consoleSignOut)
	mux.HandleFunc("GET /v1/settings/provider/status", s.asAdmin(s.providerStatus))
}

// consoleHeaders keeps the console to its own origin: scripts and styles from
// attest only, nothing inline, no framing, no referrer.
func consoleHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; "+
			"connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		// Embedded files carry no modification time; revalidate so an
		// upgrade is never served stale.
		h.Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

func noListing(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "" || strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// serveConsole serves the one page every console path renders; the page
// routes itself.
func serveConsole(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(consoleIndex)
}

// consoleCanonical sends console pages to publicUrl once setup is complete,
// where sign-in and its cookie live. During setup it does not: the setup
// session's cookie belongs to whatever host setup began on.
func (s *Server) consoleCanonical(next http.Handler) http.Handler {
	canon := s.canonical(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cur().settings.SetupComplete {
			canon.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// safeReturn keeps a post-sign-in destination inside the console. Anything
// else, including other paths on attest, becomes the console's front page.
func safeReturn(raw string) string {
	const home = "/console/"
	if raw == "" || len(raw) > maxReturn || !strings.HasPrefix(raw, home) ||
		strings.ContainsAny(raw, "\\\x00\r\n\t") || strings.Contains(raw, "//") {
		return home
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" {
		return home
	}
	clean := path.Clean(u.Path)
	if !strings.HasPrefix(clean+"/", home) || clean == "/console/login" {
		return home
	}
	if strings.HasSuffix(u.Path, "/") && clean != "/console" {
		clean += "/"
	}
	if clean == "/console" {
		return home
	}
	return clean
}

// consoleProblem sends a sign-in that did not finish back to the console with
// a fixed reason, so nothing from the provider is echoed into the page.
func consoleProblem(w http.ResponseWriter, r *http.Request, reason string) {
	http.Redirect(w, r, "/console/sign-in?problem="+reason, http.StatusSeeOther)
}

// Sign-in modes carried in the flow cookie. A console sign-in needs the
// admin role; a sign-in from the front door routes by it.
const (
	modeConsole = "console"
	modeAuto    = "auto"
)

// frontDoor is "/". Until setup is complete it leads to setup in the
// console. Afterwards it is the sign-in page, on publicUrl's host, unless
// the browser is already signed in to the console or to enrollment.
func (s *Server) frontDoor(w http.ResponseWriter, r *http.Request) {
	if !s.cur().settings.SetupComplete {
		http.Redirect(w, r, "/console/", http.StatusFound)
		return
	}
	s.canonical(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if where := s.signedInAt(r); where != "" {
			http.Redirect(w, r, where, http.StatusFound)
			return
		}
		s.renderFront(w, r, http.StatusOK, "")
	})).ServeHTTP(w, r)
}

// signedInAt is where a browser that is already signed in belongs: the
// console for a console session, else enrollment for an enrollment session.
func (s *Server) signedInAt(r *http.Request) string {
	if _, _, ok := s.consoleSession(r); ok {
		return "/console/"
	}
	if s.enrollSignedIn(r) {
		return "/enroll"
	}
	return ""
}

// login is the front door's one sign-in. When the provider sends the person
// back, holders of the admin role land in the console and everyone else in
// their enrollment.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	l := s.cur()
	if !l.settings.SetupComplete {
		http.Redirect(w, r, "/console/", http.StatusSeeOther)
		return
	}
	if where := s.signedInAt(r); where != "" {
		http.Redirect(w, r, where, http.StatusSeeOther)
		return
	}
	if !l.enrolling() {
		s.renderFront(w, r, http.StatusServiceUnavailable, "Sign-in isn't available: the identity provider is connected without a sign-in client.")
		return
	}
	if !s.startSignIn(w, r, l, modeAuto, "/console/") {
		s.renderFront(w, r, http.StatusServiceUnavailable, "Sign-in is unavailable right now. Please try again in a few minutes.")
	}
}

func (s *Server) consoleLogin(w http.ResponseWriter, r *http.Request) {
	l := s.cur()
	if !l.settings.SetupComplete {
		http.Redirect(w, r, "/console/", http.StatusSeeOther)
		return
	}
	if !l.enrolling() {
		consoleProblem(w, r, "no-client")
		return
	}
	if !s.startSignIn(w, r, l, modeConsole, safeReturn(r.URL.Query().Get("return"))) {
		consoleProblem(w, r, "unavailable")
	}
}

// startSignIn sends the browser to the provider with a fresh state, nonce
// and PKCE verifier, kept with the mode and the post-sign-in path in a flow
// cookie only the callback sees. It reports false when the provider cannot
// be reached.
func (s *Server) startSignIn(w http.ResponseWriter, r *http.Request, l *live, mode, back string) bool {
	p, err := l.oidc.provider()
	if err != nil {
		return false
	}
	state, nonce, verifier := randomToken(), randomToken(), oauth2.GenerateVerifier()
	encoded := base64.RawURLEncoding.EncodeToString([]byte(safeReturn(back)))
	http.SetCookie(w, &http.Cookie{
		Name: consoleFlowCookie, Value: state + "." + nonce + "." + verifier + "." + encoded + "." + mode,
		Path: "/enroll/callback", MaxAge: int(flowTTL.Seconds()),
		HttpOnly: true, Secure: l.secure(),
		// Lax: the provider's redirect back is a cross-site navigation.
		SameSite: http.SameSiteLaxMode,
	})
	// Sign-in returns through the redirect URI the provider already has for
	// enrollment, so no provider needs reconfiguring for the console.
	http.Redirect(w, r, l.oauth(p).AuthCodeURL(state,
		oauth2.S256ChallengeOption(verifier), oidc.Nonce(nonce)), http.StatusFound)
	return true
}

// consoleCallback finishes a console sign-in when the callback carries the
// console's flow cookie and its state. It reports whether it handled the
// request; anything else is an enrollment sign-in.
func (s *Server) consoleCallback(w http.ResponseWriter, r *http.Request, l *live) bool {
	c, err := r.Cookie(consoleFlowCookie)
	if err != nil {
		return false
	}
	flow := strings.Split(c.Value, ".")
	q := r.URL.Query()
	if len(flow) != 5 || (flow[4] != modeConsole && flow[4] != modeAuto) || q.Get("state") == "" || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(flow[0])) != 1 {
		return false
	}
	http.SetCookie(w, &http.Cookie{Name: consoleFlowCookie, Path: "/enroll/callback", MaxAge: -1, HttpOnly: true, Secure: l.secure(), SameSite: http.SameSiteLaxMode})
	if !l.settings.SetupComplete {
		http.Redirect(w, r, "/console/", http.StatusSeeOther)
		return true
	}
	// A front-door sign-in that did not finish goes back to the front door.
	problem := func(reason string) {
		if flow[4] == modeConsole {
			consoleProblem(w, r, reason)
			return
		}
		msg := "Sign-in didn't finish. Please try again."
		if reason == "unavailable" {
			msg = "Sign-in is unavailable right now. Please try again in a few minutes."
		}
		s.renderFront(w, r, http.StatusBadRequest, msg)
	}
	if q.Get("error") != "" {
		problem("failed")
		return true
	}
	done, status := finishSignIn(r, l, flow[1], flow[2], "console")
	switch status {
	case http.StatusOK:
	case http.StatusServiceUnavailable:
		problem("unavailable")
		return true
	default:
		problem("failed")
		return true
	}
	// A planted or stale session never survives a sign-in.
	if old, err := r.Cookie(consoleCookie); err == nil {
		s.console.end(old.Value)
	}
	adminRole := l.oidc.cfg.Roles[KindAdmin]
	if !slices.Contains(l.providerRoles(done.claims, done.tok.AccessToken), adminRole) {
		http.SetCookie(w, &http.Cookie{Name: consoleCookie, Path: "/", MaxAge: -1, HttpOnly: true, Secure: l.secure(), SameSite: http.SameSiteLaxMode})
		// From the front door, anyone who is not an administrator is
		// signed in to their own enrollment instead.
		if flow[4] == modeAuto {
			s.finishEnroll(w, r, l, done)
			return true
		}
		log.Printf("console: refused a sign-in without the %s role", adminRole)
		http.Redirect(w, r, "/console/no-access", http.StatusSeeOther)
		return true
	}
	name, _ := done.claims["name"].(string)
	if name == "" {
		name, _ = done.claims["preferred_username"].(string)
	}
	id, _ := s.console.start(done.idt.Subject, name, s.now())
	http.SetCookie(w, &http.Cookie{
		Name: consoleCookie, Value: id, Path: "/", MaxAge: int(consoleMax.Seconds()),
		HttpOnly: true, Secure: l.secure(), SameSite: http.SameSiteLaxMode,
	})
	back := "/console/"
	if raw, err := base64.RawURLEncoding.DecodeString(flow[3]); err == nil {
		back = safeReturn(string(raw))
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
	return true
}

// consoleSessionInfo is public. It tells the console whether to show setup,
// sign-in, or itself, and gives a signed-in console its CSRF token.
func (s *Server) consoleSessionInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	l := s.cur()
	if _, sess, ok := s.consoleSession(r); ok {
		reply(w, http.StatusOK, map[string]any{
			"signedIn": true, "setupComplete": l.settings.SetupComplete,
			"name": sess.name, "csrf": sess.csrf, "expiresAt": sess.expires(),
		})
		return
	}
	reply(w, http.StatusOK, map[string]any{
		"signedIn": false, "setupComplete": l.settings.SetupComplete,
		"signInAvailable": l.settings.SetupComplete && l.enrolling(),
	})
}

func (s *Server) consoleSignOut(w http.ResponseWriter, r *http.Request) {
	if id, sess, ok := s.consoleSession(r); ok {
		if !csrfMatches(r, sess.csrf) {
			fail(w, http.StatusForbidden, "missing or wrong X-CSRF-Token")
			return
		}
		s.console.end(id)
	}
	http.SetCookie(w, &http.Cookie{Name: consoleCookie, Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.cur().secure(), SameSite: http.SameSiteLaxMode})
	w.WriteHeader(http.StatusNoContent)
}

// providerStatus asks the provider now, rather than trusting what attest
// discovered earlier, whether it answers as the issuer attest is connected to.
func (s *Server) providerStatus(w http.ResponseWriter, r *http.Request, _ Principal) {
	l := s.cur()
	out := map[string]any{"connected": l.oidc != nil, "checkedAt": s.now()}
	if l.oidc == nil {
		reply(w, http.StatusOK, out)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	issuer, err := provider.Discover(ctx, provider.Client, l.oidc.cfg.Issuer)
	switch {
	case err != nil:
		out["reachable"], out["problem"] = false, err.Error()
	case strings.TrimRight(issuer, "/") != strings.TrimRight(l.oidc.cfg.Issuer, "/"):
		out["reachable"], out["problem"] = false, "it answers as a different issuer ("+issuer+"), so its sign-ins would be refused"
	default:
		out["reachable"] = true
	}
	reply(w, http.StatusOK, out)
}
