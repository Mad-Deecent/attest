package server

import (
	"crypto/rand"
	"crypto/subtle"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// First-run setup. While attest is unconfigured it prints a one-time setup
// code to its log. Whoever can read the log can trade the code for a short
// setup session that may change settings. Reaching the port is not enough.

const (
	setupCookie = "attest_setup"
	setupTTL    = 30 * time.Minute
	// Unambiguous when read off a terminal: no 0/O, 1/I/L.
	setupAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"
)

type setup struct {
	mu sync.Mutex
	// code is the unspent setup code; empty once spent or after setup.
	code    string
	session string
	csrf    string
	expires time.Time
}

// newSetupCode is 16 characters from a 31-letter alphabet, about 79 bits,
// grouped for reading aloud.
func newSetupCode() string {
	var b strings.Builder
	for i := range 16 {
		if i > 0 && i%4 == 0 {
			b.WriteByte('-')
		}
		b.WriteByte(setupAlphabet[randIndex(len(setupAlphabet))])
	}
	return b.String()
}

func randIndex(n int) int {
	// Rejection sampling keeps every letter equally likely.
	var c [1]byte
	for {
		_, _ = rand.Read(c[:])
		if int(c[0]) < 256-256%n {
			return int(c[0]) % n
		}
	}
}

func normalizeCode(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' {
			return -1
		}
		return r
	}, strings.ToUpper(s))
}

func (st *setup) mintLocked() {
	st.code = newSetupCode()
	log.Printf("attest is not set up yet. One-time setup code: %s", st.code)
}

func (st *setup) begin() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.mintLocked()
}

// renewLocked prints a new code once the session the old one opened has
// lapsed without finishing setup. An unspent code is never replaced, so
// nobody can rotate away the code the operator is about to type.
func (st *setup) renewLocked(now time.Time) {
	if st.code == "" && st.session != "" && !now.Before(st.expires) {
		st.session, st.csrf = "", ""
		st.mintLocked()
	}
}

func (st *setup) renew(now time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.renewLocked(now)
}

// open spends the code for a session.
func (st *setup) open(code string, now time.Time) (session, csrf string, expires time.Time, ok bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.renewLocked(now)
	if st.code == "" || subtle.ConstantTimeCompare([]byte(normalizeCode(code)), []byte(normalizeCode(st.code))) != 1 {
		return "", "", time.Time{}, false
	}
	st.code = ""
	st.session, st.csrf, st.expires = randomToken(), randomToken(), now.Add(setupTTL)
	return st.session, st.csrf, st.expires, true
}

// check returns 200 for a live session whose request carries its CSRF token
// (when the method changes state), 403 for a missing token, 401 otherwise.
func (st *setup) check(session, csrf string, write bool, now time.Time) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.session == "" || !now.Before(st.expires) ||
		subtle.ConstantTimeCompare([]byte(session), []byte(st.session)) != 1 {
		return http.StatusUnauthorized
	}
	if write && subtle.ConstantTimeCompare([]byte(csrf), []byte(st.csrf)) != 1 {
		return http.StatusForbidden
	}
	return http.StatusOK
}

// end kills the code and any session for good.
func (st *setup) end() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.code, st.session, st.csrf = "", "", ""
}

func (s *Server) setupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/setup/status", s.setupStatus)
	mux.HandleFunc("POST /v1/setup/session", s.openSetup)
	mux.HandleFunc("POST /v1/setup/complete", s.asAdmin(s.completeSetup))
}

// setupStatus is public: it tells a console whether to show setup, and
// nothing about how attest is configured.
func (s *Server) setupStatus(w http.ResponseWriter, _ *http.Request) {
	done := s.cur().settings.SetupComplete
	if !done {
		s.setup.renew(s.now())
	}
	reply(w, http.StatusOK, map[string]bool{"setupComplete": done})
}

func (s *Server) openSetup(w http.ResponseWriter, r *http.Request) {
	if s.cur().settings.SetupComplete {
		fail(w, http.StatusGone, "setup is complete")
		return
	}
	if s.data == "" {
		fail(w, errNoData.status, errNoData.msg)
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &body) {
		return
	}
	session, csrf, expires, ok := s.setup.open(body.Code, s.now())
	if !ok {
		fail(w, http.StatusUnauthorized, "invalid or used setup code")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: setupCookie, Value: session, Path: "/v1/", MaxAge: int(setupTTL.Seconds()),
		HttpOnly: true, Secure: r.TLS != nil || s.cur().secure(), SameSite: http.SameSiteStrictMode,
	})
	w.Header().Set("Cache-Control", "no-store")
	reply(w, http.StatusCreated, map[string]any{"csrf": csrf, "expiresAt": expires})
}

// asAdmin admits an admin credential, or the setup session while attest is
// unconfigured. A request with a bearer token is judged on the token alone.
func (s *Server) asAdmin(next handler) http.HandlerFunc {
	bearer := s.as(KindAdmin, next)
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(setupCookie)
		if err != nil || r.Header.Get("Authorization") != "" {
			bearer(w, r)
			return
		}
		write := r.Method != http.MethodGet && r.Method != http.MethodHead
		status := s.setup.check(c.Value, r.Header.Get("X-CSRF-Token"), write, s.now())
		if s.cur().settings.SetupComplete {
			status = http.StatusUnauthorized
		}
		switch status {
		case http.StatusOK:
			next(w, r, Principal{Kind: KindAdmin, ID: "setup", viaSetup: true})
		case http.StatusForbidden:
			fail(w, status, "missing or wrong X-CSRF-Token")
		default:
			fail(w, status, "setup session expired or invalid")
		}
	}
}

// completeSetup ends first-run setup once attest can verify credentials from
// the provider and send people back to itself. From then on the setup code
// and session are dead, including across restarts.
func (s *Server) completeSetup(w http.ResponseWriter, _ *http.Request, _ Principal) {
	// Under the settings lock, so the provider checked here is the one that
	// is saved as set up, not one a concurrent connect is replacing.
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	l := s.cur()
	if l.settings.SetupComplete {
		fail(w, http.StatusGone, "setup is complete")
		return
	}
	if l.public == nil || l.oidc == nil {
		fail(w, http.StatusConflict, "set the public URL and connect an identity provider first")
		return
	}
	if _, err := l.oidc.provider(); err != nil {
		fail(w, http.StatusServiceUnavailable, "the identity provider is not answering; try again")
		return
	}
	if _, ok := s.updateLocked(w, func(st *Settings) error {
		st.SetupComplete = true
		return nil
	}, false); !ok {
		return
	}
	s.setup.end()
	log.Printf("setup complete: the setup code no longer works")
	reply(w, http.StatusOK, map[string]bool{"setupComplete": true})
}
