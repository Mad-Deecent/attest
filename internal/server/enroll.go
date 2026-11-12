package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/mad-deecent/attest/internal/decision"
	"github.com/mad-deecent/attest/internal/identity"
)

// EnrollConfig configures the self-service enrollment page.
type EnrollConfig struct {
	// VoiceURL is the voice producer's enrollment endpoint. The browser posts
	// the recording there directly; attest never receives audio.
	VoiceURL string `json:"voiceUrl,omitempty"`
	// VoiceProducer is the principal id of the voice producer. When set, only
	// it may redeem enrollment tickets; any other producer is refused.
	VoiceProducer string `json:"voiceProducer,omitempty"`
}

const (
	sessionCookie = "attest_session"
	flowCookie    = "attest_signin"
	sessionTTL    = 15 * time.Minute
	flowTTL       = 10 * time.Minute
	ticketTTL     = 5 * time.Minute
)

type session struct {
	identityID string
	// account is how the provider names the person (username or email), kept
	// for the page only, in memory, for the session's life.
	account string
	csrf    string
	expires time.Time
}

// A ticket is good for one purpose only. An enrollment ticket can never
// delete a voice, and a deletion ticket can never enroll one.
const (
	purposeEnroll      = "enroll"
	purposeDeleteVoice = "delete-voice"
)

type ticket struct {
	identityID string
	purpose    string
	expires    time.Time
}

// enrollment is the session and ticket state behind /enroll. Sessions and
// tickets are short-lived and deliberately not persisted. They outlive a
// change of provider: they name identities, not connections.
type enrollment struct {
	voiceURL      string
	voiceProducer string
	mu            sync.Mutex
	sessions      map[string]session
	tickets       map[string]ticket
}

func newEnrollment(cfg EnrollConfig) *enrollment {
	return &enrollment{
		voiceURL:      cfg.VoiceURL,
		voiceProducer: cfg.VoiceProducer,
		sessions:      map[string]session{},
		tickets:       map[string]ticket{},
	}
}

func (l *live) secure() bool { return l.public != nil && l.public.Scheme == "https" }

func (l *live) oauth(p *oidc.Provider) *oauth2.Config {
	endpoint := p.Endpoint()
	// A public client has no secret to send in a Basic header.
	endpoint.AuthStyle = oauth2.AuthStyleInParams
	return &oauth2.Config{
		ClientID:    l.oidc.cfg.ClientID,
		Endpoint:    endpoint,
		RedirectURL: l.public.String() + "/enroll/callback",
		Scopes:      []string{oidc.ScopeOpenID, "profile"},
	}
}

func (e *enrollment) startSession(identityID, account string, now time.Time) (id string, s session) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for k, old := range e.sessions {
		if !now.Before(old.expires) {
			delete(e.sessions, k)
		}
	}
	id = randomToken()
	s = session{identityID: identityID, account: account, csrf: randomToken(), expires: now.Add(sessionTTL)}
	e.sessions[id] = s
	return id, s
}

func (e *enrollment) session(r *http.Request, now time.Time) (string, session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return "", session{}, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.sessions[c.Value]
	if !ok || !now.Before(s.expires) {
		delete(e.sessions, c.Value)
		return "", session{}, false
	}
	return c.Value, s, true
}

func (e *enrollment) endSession(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.sessions, id)
}

func (e *enrollment) issueTicket(identityID, purpose string, now time.Time) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	// A new ticket replaces the identity's earlier ones, whatever they were
	// for, so at most one request can be pending for a person at a time. In
	// particular a recording still on its way cannot store a voice after the
	// person asked to delete it.
	for k, t := range e.tickets {
		if !now.Before(t.expires) || t.identityID == identityID {
			delete(e.tickets, k)
		}
	}
	t := randomToken()
	e.tickets[t] = ticket{identityID: identityID, purpose: purpose, expires: now.Add(ticketTTL)}
	return t
}

// redeem spends a ticket for its purpose. Unknown, spent, expired, and
// other-purpose tickets look the same. A ticket presented for the wrong
// purpose is not spent: it stays good for the one thing it was issued for.
func (e *enrollment) redeem(raw, purpose string, now time.Time) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, ok := e.tickets[raw]
	if !ok || t.purpose != purpose {
		return "", false
	}
	delete(e.tickets, raw)
	if !now.Before(t.expires) {
		return "", false
	}
	return t.identityID, true
}

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *Server) enrollRoutes(mux *http.ServeMux) {
	mux.Handle("GET /enroll", s.canonical(http.HandlerFunc(s.enrollPage)))
	mux.Handle("GET /enroll/static/", s.canonical(http.StripPrefix("/enroll/static/", http.FileServerFS(enrollStatic))))
	mux.Handle("GET /enroll/login", s.canonical(http.HandlerFunc(s.enrollLogin)))
	mux.Handle("GET /enroll/callback", s.canonical(http.HandlerFunc(s.enrollCallback)))
	mux.Handle("POST /enroll/logout", s.canonical(s.withSession(s.enrollLogout)))
	mux.Handle("POST /enroll/ticket", s.canonical(s.withSession(s.enrollTicket)))
	mux.Handle("POST /enroll/voice-deletion-ticket", s.canonical(s.withSession(s.voiceDeletionTicket)))
	mux.Handle("POST /enroll/consent", s.canonical(s.withSession(s.enrollConsent)))
	mux.Handle("POST /enroll/revoke", s.canonical(s.withSession(s.enrollRevoke)))
	// The route used to be spelled /enrol; keep old bookmarks and
	// provider redirect URIs working.
	mux.HandleFunc("GET /enrol", legacyEnrolRedirect)
	mux.HandleFunc("GET /enrol/{rest...}", legacyEnrolRedirect)
}

// legacyEnrolRedirect sends GET /enrol[/...] to the /enroll equivalent,
// keeping the query string. 308 preserves the method.
func legacyEnrolRedirect(w http.ResponseWriter, r *http.Request) {
	target := "/enroll" + strings.TrimPrefix(r.URL.Path, "/enrol")
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, target, http.StatusPermanentRedirect)
}

// canonical sends a request that arrived under another host name (say
// 127.0.0.1 instead of localhost) to the same path on publicUrl. Cookies are
// per host and the provider returns people to publicUrl, so sign-in only
// works there. 307 keeps the method and body.
func (s *Server) canonical(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l := s.cur()
		if !l.enrolling() || sameHost(r.Host, l.public) {
			next.ServeHTTP(w, r)
			return
		}
		http.Redirect(w, r, l.public.String()+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	})
}

// sameHost compares a request's Host with a URL's, ignoring case and the
// scheme's default port.
func sameHost(host string, u *url.URL) bool {
	norm := func(h string) string {
		h = strings.ToLower(h)
		switch {
		case u.Scheme == "http":
			h = strings.TrimSuffix(h, ":80")
		case u.Scheme == "https":
			h = strings.TrimSuffix(h, ":443")
		}
		return h
	}
	return norm(host) == norm(u.Host)
}

type sessionHandler func(w http.ResponseWriter, r *http.Request, id string, sess session)

// withSession admits a POST only from a live session that carries its CSRF
// token, in the form or the X-CSRF-Token header.
func (s *Server) withSession(next sessionHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.cur().enrolling() {
			http.NotFound(w, r)
			return
		}
		id, sess, ok := s.enroll.session(r, s.now())
		if ok && !s.registry.Has(sess.identityID) {
			s.enroll.endSession(id)
			ok = false
		}
		if !ok {
			// The page fetches tickets; it needs an answer it can read, not
			// a redirect to HTML.
			if r.URL.Path == "/enroll/ticket" || r.URL.Path == "/enroll/voice-deletion-ticket" {
				fail(w, http.StatusUnauthorized, "not signed in")
				return
			}
			http.Redirect(w, r, "/enroll", http.StatusSeeOther)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		token := r.Header.Get("X-CSRF-Token")
		if token == "" {
			token = r.PostFormValue("csrf")
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(sess.csrf)) != 1 {
			http.Error(w, "This form has expired. Go back, reload the page, and try again.", http.StatusForbidden)
			return
		}
		next(w, r, id, sess)
	}
}

func (s *Server) enrollLogin(w http.ResponseWriter, r *http.Request) {
	l := s.cur()
	if !l.enrolling() {
		http.Redirect(w, r, "/enroll", http.StatusSeeOther)
		return
	}
	// Someone already signed in is not sent round the provider again.
	if s.enrollSignedIn(r) {
		http.Redirect(w, r, "/enroll", http.StatusSeeOther)
		return
	}
	p, err := l.oidc.provider()
	if err != nil {
		s.renderEnroll(w, r, http.StatusServiceUnavailable, "Sign-in is unavailable right now. Please try again in a few minutes.")
		return
	}
	state, nonce, verifier := randomToken(), randomToken(), oauth2.GenerateVerifier()
	http.SetCookie(w, &http.Cookie{
		Name: flowCookie, Value: state + "." + nonce + "." + verifier,
		Path: "/enroll", MaxAge: int(flowTTL.Seconds()),
		HttpOnly: true, Secure: l.secure(),
		// Lax, not Strict: the provider's redirect back is a cross-site navigation.
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, l.oauth(p).AuthCodeURL(state,
		oauth2.S256ChallengeOption(verifier), oidc.Nonce(nonce)), http.StatusFound)
}

func (s *Server) enrollCallback(w http.ResponseWriter, r *http.Request) {
	// The flow finishes against the connection it reads here. If the provider
	// changed since sign-in began, the code exchange fails and the person
	// signs in again.
	l := s.cur()
	if !l.enrolling() {
		http.Redirect(w, r, "/enroll", http.StatusSeeOther)
		return
	}
	// The console signs administrators in through the same registered
	// redirect URI; its own flow cookie says which sign-in this is.
	if s.consoleCallback(w, r, l) {
		return
	}
	const retry = "Sign-in didn't finish. Please try again."
	c, err := r.Cookie(flowCookie)
	http.SetCookie(w, &http.Cookie{Name: flowCookie, Path: "/enroll", MaxAge: -1, HttpOnly: true, Secure: l.secure(), SameSite: http.SameSiteLaxMode})
	q := r.URL.Query()
	if err != nil || q.Get("error") != "" {
		s.renderEnroll(w, r, http.StatusBadRequest, retry)
		return
	}
	flow := strings.SplitN(c.Value, ".", 3)
	if len(flow) != 3 || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(flow[0])) != 1 {
		s.renderEnroll(w, r, http.StatusBadRequest, retry)
		return
	}
	done, status := finishSignIn(r, l, flow[1], flow[2], "enroll")
	switch status {
	case http.StatusOK:
	case http.StatusServiceUnavailable:
		s.renderEnroll(w, r, status, "Sign-in is unavailable right now. Please try again in a few minutes.")
		return
	default:
		s.renderEnroll(w, r, status, retry)
		return
	}
	s.finishEnroll(w, r, l, done)
}

// enrollSignedIn reports whether the request carries a live enrollment
// session for an identity that still exists.
func (s *Server) enrollSignedIn(r *http.Request) bool {
	_, sess, ok := s.enroll.session(r, s.now())
	return ok && s.registry.Has(sess.identityID)
}

// finishEnroll signs a person in to enrollment after the provider vouched for
// them: it links (or finds) their identity, adopts the roles the policy
// defines, and starts a fresh session. Shared by /enroll/login and by the
// front door's sign-in for people who are not administrators.
func (s *Server) finishEnroll(w http.ResponseWriter, r *http.Request, l *live, done signedIn) {
	const retry = "Sign-in didn't finish. Please try again."
	idt, claims, tok := done.idt, done.claims, done.tok
	name, _ := claims["name"].(string)
	if name == "" {
		name, _ = claims["preferred_username"].(string)
	}
	account, _ := claims["email"].(string)
	if account == "" {
		account, _ = claims["preferred_username"].(string)
	}
	ident, err := s.registry.Link(identity.Provider{Issuer: idt.Issuer, Subject: idt.Subject}, name, s.now())
	if err != nil {
		s.renderEnroll(w, r, http.StatusBadRequest, retry)
		return
	}
	if err := s.rbac.SetRoles(ident.ID, l.policyRoles(claims, tok.AccessToken)); err != nil {
		s.renderEnroll(w, r, http.StatusInternalServerError, retry)
		return
	}
	if err := s.save(); err != nil {
		log.Printf("save state: %v", err)
		s.renderEnroll(w, r, http.StatusInternalServerError, "Something went wrong saving your account. Please try again.")
		return
	}

	// A planted or stale session never survives a sign-in.
	if old, err := r.Cookie(sessionCookie); err == nil {
		s.enroll.endSession(old.Value)
	}
	clearEnrollSession(w, l, "/enroll")
	id, _ := s.enroll.startSession(ident.ID, account, s.now())
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: id, Path: "/", MaxAge: int(sessionTTL.Seconds()),
		HttpOnly: true, Secure: l.secure(), SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/enroll", http.StatusSeeOther)
}

// clearEnrollSession expires the session cookie at a path. The cookie lives
// at "/" so the front door can tell who is signed in; it used to live at
// "/enroll", and one left there would shadow the new one.
func clearEnrollSession(w http.ResponseWriter, l *live, path string) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: path, MaxAge: -1, HttpOnly: true, Secure: l.secure(), SameSite: http.SameSiteLaxMode})
}

// signedIn is what a finished authorization-code flow yields.
type signedIn struct {
	idt    *oidc.IDToken
	claims map[string]any
	tok    *oauth2.Token
}

// finishSignIn exchanges the code in the callback for tokens and checks the ID
// token, its audience (the enrollment client), and the flow's nonce. Shared by
// enrollment and console sign-in; what is done with the person differs.
func finishSignIn(r *http.Request, l *live, nonce, verifier, what string) (signedIn, int) {
	p, err := l.oidc.provider()
	if err != nil {
		return signedIn{}, http.StatusServiceUnavailable
	}
	ctx, cancel := context.WithTimeout(oidc.ClientContext(r.Context(), providerClient), 15*time.Second)
	defer cancel()
	tok, err := l.oauth(p).Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(verifier))
	if err != nil {
		log.Printf("%s: token exchange: %v", what, err)
		return signedIn{}, http.StatusBadGateway
	}
	rawID, _ := tok.Extra("id_token").(string)
	idt, err := l.oidc.enrollVerifier(p).Verify(ctx, rawID)
	if err != nil || subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(nonce)) != 1 {
		log.Printf("%s: id token rejected: %v", what, err)
		return signedIn{}, http.StatusBadRequest
	}
	var claims map[string]any
	if err := idt.Claims(&claims); err != nil {
		return signedIn{}, http.StatusBadRequest
	}
	return signedIn{idt: idt, claims: claims, tok: tok}, http.StatusOK
}

// policyRoles reads the provider's roles for the person signing in and keeps
// those the policy defines. Keycloak puts realm roles in the access token by
// default, so it is the fallback when the ID token has no roles claim.
func (l *live) policyRoles(idClaims map[string]any, accessToken string) []string {
	var kept []string
	for _, role := range l.providerRoles(idClaims, accessToken) {
		if _, defined := l.settings.Policy.Roles[role]; defined && !slices.Contains(kept, role) {
			kept = append(kept, role)
		}
	}
	slices.Sort(kept)
	return kept
}

// providerRoles is every role the provider gave the person signing in, from
// the ID token's roles claim or, failing that, the access token's.
func (l *live) providerRoles(idClaims map[string]any, accessToken string) []string {
	path := l.oidc.cfg.RolesClaim
	if _, ok := claimAt(idClaims, path); !ok {
		idClaims = unverifiedClaims(accessToken)
	}
	return rolesAt(idClaims, path)
}

// unverifiedClaims decodes a JWT payload without checking its signature. Only
// for a token that came straight from the token endpoint over the back channel.
func unverifiedClaims(raw string) map[string]any {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims map[string]any
	_ = json.Unmarshal(payload, &claims)
	return claims
}

func (s *Server) enrollLogout(w http.ResponseWriter, r *http.Request, id string, _ session) {
	s.enroll.endSession(id)
	l := s.cur()
	clearEnrollSession(w, l, "/")
	clearEnrollSession(w, l, "/enroll")
	http.Redirect(w, r, "/?done=signedout", http.StatusSeeOther)
}

func (s *Server) enrollTicket(w http.ResponseWriter, _ *http.Request, _ string, sess session) {
	if s.enroll.voiceURL == "" {
		fail(w, http.StatusNotFound, "no voice producer is configured")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	reply(w, http.StatusOK, map[string]string{"ticket": s.enroll.issueTicket(sess.identityID, purposeEnroll, s.now())})
}

// voiceDeletionTicket gives the page a single-use ticket that only deletes
// the person's voice. The page sends it to the voice service, which redeems
// it at /v1/voice-deletion-tickets/redeem and then deletes what it holds.
// It is issued whether or not attest has a voice on record, so a deletion
// the voice service did not finish can always be asked for again.
func (s *Server) voiceDeletionTicket(w http.ResponseWriter, _ *http.Request, _ string, sess session) {
	if s.enroll.voiceURL == "" {
		fail(w, http.StatusNotFound, "no voice producer is configured")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	reply(w, http.StatusOK, map[string]string{"ticket": s.enroll.issueTicket(sess.identityID, purposeDeleteVoice, s.now())})
}

// redeemTicket lets a voice producer learn which identity a recording is for.
func (s *Server) redeemTicket(w http.ResponseWriter, r *http.Request, p Principal) {
	var body struct {
		Ticket string `json:"ticket"`
	}
	if !decode(w, r, &body) {
		return
	}
	// Refused before the ticket is touched, so the right producer can still
	// redeem it.
	if s.enroll.voiceProducer != "" && p.ID != s.enroll.voiceProducer {
		fail(w, http.StatusForbidden, "only the configured voice producer may redeem enrollment tickets")
		return
	}
	id, ok := s.enroll.redeem(body.Ticket, purposeEnroll, s.now())
	if !ok || !s.registry.MarkVoiceEnrolled(id, s.now()) {
		fail(w, http.StatusNotFound, "unknown, used, or expired ticket")
		return
	}
	if !s.commit(w) {
		return
	}
	reply(w, http.StatusOK, map[string]string{"identityId": id})
}

// redeemDeletionTicket lets the voice producer learn whose voice to delete.
// attest forgets the enrollment date and saves that before it answers, so
// the producer deletes only once attest no longer lists a voice: the date is
// never left set when the voice is gone. If the save fails, the date is put
// back and the producer is told to delete nothing.
func (s *Server) redeemDeletionTicket(w http.ResponseWriter, r *http.Request, p Principal) {
	var body struct {
		Ticket string `json:"ticket"`
	}
	if !decode(w, r, &body) {
		return
	}
	if s.enroll.voiceProducer != "" && p.ID != s.enroll.voiceProducer {
		fail(w, http.StatusForbidden, "only the configured voice producer may redeem voice deletion tickets")
		return
	}
	now := s.now()
	id, ok := s.enroll.redeem(body.Ticket, purposeDeleteVoice, now)
	if !ok {
		fail(w, http.StatusNotFound, "unknown, used, or expired ticket")
		return
	}
	prev, ok := s.registry.SetVoiceEnrolledAt(id, nil, now)
	if !ok {
		fail(w, http.StatusNotFound, "unknown, used, or expired ticket")
		return
	}
	if err := s.save(); err != nil {
		log.Printf("save state: %v", err)
		s.registry.SetVoiceEnrolledAt(id, prev, now)
		fail(w, http.StatusInternalServerError, "could not save state; delete nothing")
		return
	}
	reply(w, http.StatusOK, map[string]any{"identityId": id, "hadVoice": prev != nil})
}

func (s *Server) enrollConsent(w http.ResponseWriter, r *http.Request, _ string, sess session) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request.", http.StatusBadRequest)
		return
	}
	now := s.now()
	for _, offer := range s.offers(s.cur().policy(), sess.identityID) {
		// Only what the page offered can be chosen, and never "*": consent
		// must not widen when roles do.
		var chosen []string
		for _, p := range offer.permissions {
			if slices.Contains(r.PostForm["perm:"+offer.agent], p) {
				chosen = append(chosen, p)
			}
		}
		// Choices replace only what the person granted themselves. What an
		// administrator set up (zones, ceilings, expiry) is not theirs to change.
		own := s.ownConsent(sess.identityID, offer.agent, now)
		if slices.Equal(chosen, covered(offer.permissions, own)) {
			continue
		}
		// Replacing is revoke then add.
		for _, d := range own {
			s.delegations.Revoke(d.ID, now)
		}
		if len(chosen) > 0 {
			if _, err := s.delegations.Add(decision.Delegation{
				IdentityID: sess.identityID, Agent: offer.agent, Permissions: chosen, GrantedBy: sess.identityID,
			}, now); err != nil {
				s.renderEnroll(w, r, http.StatusBadRequest, "That choice couldn't be saved.")
				return
			}
		}
	}
	if err := s.save(); err != nil {
		log.Printf("save state: %v", err)
		s.renderEnroll(w, r, http.StatusInternalServerError, "Your choices couldn't be saved. Please try again.")
		return
	}
	// A newcomer finishes on the summary; someone changing their choices
	// goes back to what they have.
	if r.PostFormValue("then") == "done" {
		http.Redirect(w, r, "/enroll?step=done", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/enroll?done=saved", http.StatusSeeOther)
}

// enrollRevoke withdraws the person's own consent: one action for one agent
// (agent and perm), or, as before, a whole delegation (id). Withdrawing one
// action leaves the others they allowed in place.
func (s *Server) enrollRevoke(w http.ResponseWriter, r *http.Request, _ string, sess session) {
	now := s.now()
	if perm := r.PostFormValue("perm"); perm != "" {
		s.revokeOne(w, r, sess, r.PostFormValue("agent"), perm, now)
		return
	}
	d, ok := s.delegations.Get(r.PostFormValue("id"))
	// A person can only withdraw their own, still-live consent.
	if !ok || d.IdentityID != sess.identityID || d.RevokedAt != nil {
		http.Error(w, "Not found.", http.StatusNotFound)
		return
	}
	if d.GrantedBy != sess.identityID {
		http.Error(w, managedMessage, http.StatusForbidden)
		return
	}
	s.delegations.Revoke(d.ID, now)
	s.revoked(w, r)
}

const managedMessage = "This was set up by whoever runs this system. Ask them to change it."

func (s *Server) revokeOne(w http.ResponseWriter, r *http.Request, sess session, agent, perm string, now time.Time) {
	var own []decision.Delegation
	for _, d := range s.ownConsent(sess.identityID, agent, now) {
		if slices.Contains(d.Permissions, perm) {
			own = append(own, d)
		}
	}
	if len(own) == 0 {
		if covered([]string{perm}, s.liveDelegations(sess.identityID, agent, now)) != nil {
			http.Error(w, managedMessage, http.StatusForbidden)
			return
		}
		http.Error(w, "Not found.", http.StatusNotFound)
		return
	}
	// Replacing is revoke then add, keeping everything but this action.
	for _, d := range own {
		s.delegations.Revoke(d.ID, now)
		rest := slices.DeleteFunc(slices.Clone(d.Permissions), func(p string) bool { return p == perm })
		if len(rest) == 0 {
			continue
		}
		d.Permissions = rest
		if _, err := s.delegations.Add(d, now); err != nil {
			s.renderEnroll(w, r, http.StatusInternalServerError, "That couldn't be removed. Please try again.")
			return
		}
	}
	s.revoked(w, r)
}

func (s *Server) revoked(w http.ResponseWriter, r *http.Request) {
	if err := s.save(); err != nil {
		log.Printf("save state: %v", err)
		s.renderEnroll(w, r, http.StatusInternalServerError, "That couldn't be removed. Please try again.")
		return
	}
	http.Redirect(w, r, "/enroll?done=revoked", http.StatusSeeOther)
}

// offer is what one agent may ask to do for an identity.
type offer struct {
	agent       string
	permissions []string
}

// offers lists, per agent, the permissions the agent may request and the
// identity's roles hold. Floor permissions need no consent and permissions
// without a band can never be granted, so neither is offered.
func (s *Server) offers(policy decision.Policy, identityID string) []offer {
	held := map[string]bool{}
	for _, role := range s.rbac.Roles(identityID) {
		for _, p := range policy.Roles[role] {
			held[p] = true
		}
	}
	var agents []string
	for agent := range policy.Agents {
		agents = append(agents, agent)
	}
	slices.Sort(agents)
	var out []offer
	for _, agent := range agents {
		allowed := policy.Agents[agent]
		var perms []string
		for p := range policy.Permissions {
			if held[p] && !slices.Contains(policy.Floor, p) &&
				(slices.Contains(allowed, "*") || slices.Contains(allowed, p)) {
				perms = append(perms, p)
			}
		}
		if len(perms) > 0 {
			slices.Sort(perms)
			out = append(out, offer{agent: agent, permissions: perms})
		}
	}
	return out
}

func (s *Server) liveDelegations(identityID, agent string, now time.Time) []decision.Delegation {
	var out []decision.Delegation
	for _, d := range s.delegations.List() {
		if d.IdentityID != identityID || d.RevokedAt != nil || (d.ExpiresAt != nil && !d.ExpiresAt.After(now)) {
			continue
		}
		if agent == "" || d.Agent == agent {
			out = append(out, d)
		}
	}
	return out
}

// ownConsent is the live delegations a person granted themselves on the
// enrollment page, as opposed to those an administrator made for them.
func (s *Server) ownConsent(identityID, agent string, now time.Time) []decision.Delegation {
	return slices.DeleteFunc(s.liveDelegations(identityID, agent, now), func(d decision.Delegation) bool {
		return d.GrantedBy != identityID
	})
}

// covered returns the permissions, in order, that some live delegation grants.
func covered(perms []string, live []decision.Delegation) []string {
	var out []string
	for _, p := range perms {
		if slices.ContainsFunc(live, func(d decision.Delegation) bool {
			return slices.Contains(d.Permissions, p) || slices.Contains(d.Permissions, "*")
		}) {
			out = append(out, p)
		}
	}
	return out
}
