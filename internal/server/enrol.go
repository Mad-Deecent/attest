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

// EnrolConfig configures the self-service enrolment page.
type EnrolConfig struct {
	// VoiceURL is the voice producer's enrolment endpoint. The browser posts
	// the recording there directly; attest never receives audio.
	VoiceURL string `json:"voiceUrl,omitempty"`
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
	csrf       string
	expires    time.Time
}

type ticket struct {
	identityID string
	expires    time.Time
}

// enrolment is the sign-in, session, and ticket state behind /enrol. Sessions
// and tickets are short-lived and deliberately not persisted.
type enrolment struct {
	oidc     *oidcVerifier
	public   *url.URL
	voiceURL string
	mu       sync.Mutex
	idv      *oidc.IDTokenVerifier
	sessions map[string]session
	tickets  map[string]ticket
}

func newEnrolment(v *oidcVerifier, public *url.URL, cfg EnrolConfig) *enrolment {
	return &enrolment{
		oidc:     v,
		public:   public,
		voiceURL: cfg.VoiceURL,
		sessions: map[string]session{},
		tickets:  map[string]ticket{},
	}
}

func (e *enrolment) secure() bool { return e.public.Scheme == "https" }

func (e *enrolment) oauth(p *oidc.Provider) *oauth2.Config {
	endpoint := p.Endpoint()
	// A public client has no secret to send in a Basic header.
	endpoint.AuthStyle = oauth2.AuthStyleInParams
	return &oauth2.Config{
		ClientID:    e.oidc.cfg.ClientID,
		Endpoint:    endpoint,
		RedirectURL: e.public.String() + "/enrol/callback",
		Scopes:      []string{oidc.ScopeOpenID, "profile"},
	}
}

// idVerifier checks ID tokens issued to the enrolment client, never skipping
// the audience check.
func (e *enrolment) idVerifier(p *oidc.Provider) *oidc.IDTokenVerifier {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.idv == nil {
		e.idv = p.Verifier(&oidc.Config{ClientID: e.oidc.cfg.ClientID})
	}
	return e.idv
}

func (e *enrolment) startSession(identityID string, now time.Time) (id string, s session) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for k, old := range e.sessions {
		if !now.Before(old.expires) {
			delete(e.sessions, k)
		}
	}
	id = randomToken()
	s = session{identityID: identityID, csrf: randomToken(), expires: now.Add(sessionTTL)}
	e.sessions[id] = s
	return id, s
}

func (e *enrolment) session(r *http.Request, now time.Time) (string, session, bool) {
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

func (e *enrolment) endSession(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.sessions, id)
}

func (e *enrolment) issueTicket(identityID string, now time.Time) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	for k, t := range e.tickets {
		if !now.Before(t.expires) {
			delete(e.tickets, k)
		}
	}
	t := randomToken()
	e.tickets[t] = ticket{identityID: identityID, expires: now.Add(ticketTTL)}
	return t
}

// redeem spends a ticket. Unknown, spent, and expired tickets look the same.
func (e *enrolment) redeem(raw string, now time.Time) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, ok := e.tickets[raw]
	delete(e.tickets, raw)
	if !ok || !now.Before(t.expires) {
		return "", false
	}
	return t.identityID, true
}

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *Server) enrolRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /enrol", s.enrolPage)
	mux.Handle("GET /enrol/static/", http.StripPrefix("/enrol/static/", http.FileServerFS(enrolStatic)))
	mux.HandleFunc("GET /enrol/login", s.enrolLogin)
	mux.HandleFunc("GET /enrol/callback", s.enrolCallback)
	mux.HandleFunc("POST /enrol/logout", s.withSession(s.enrolLogout))
	mux.HandleFunc("POST /enrol/ticket", s.withSession(s.enrolTicket))
	mux.HandleFunc("POST /enrol/consent", s.withSession(s.enrolConsent))
	mux.HandleFunc("POST /enrol/revoke", s.withSession(s.enrolRevoke))
}

type sessionHandler func(w http.ResponseWriter, r *http.Request, id string, sess session)

// withSession admits a POST only from a live session that carries its CSRF
// token, in the form or the X-CSRF-Token header.
func (s *Server) withSession(next sessionHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.enrol == nil {
			http.NotFound(w, r)
			return
		}
		id, sess, ok := s.enrol.session(r, s.now())
		if ok && !s.registry.Has(sess.identityID) {
			s.enrol.endSession(id)
			ok = false
		}
		if !ok {
			if r.URL.Path == "/enrol/ticket" {
				fail(w, http.StatusUnauthorized, "not signed in")
				return
			}
			http.Redirect(w, r, "/enrol", http.StatusSeeOther)
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

func (s *Server) enrolLogin(w http.ResponseWriter, r *http.Request) {
	if s.enrol == nil {
		http.Redirect(w, r, "/enrol", http.StatusSeeOther)
		return
	}
	p, err := s.oidc.provider()
	if err != nil {
		s.renderEnrol(w, r, http.StatusServiceUnavailable, "Sign-in is unavailable right now. Please try again in a few minutes.")
		return
	}
	state, nonce, verifier := randomToken(), randomToken(), oauth2.GenerateVerifier()
	http.SetCookie(w, &http.Cookie{
		Name: flowCookie, Value: state + "." + nonce + "." + verifier,
		Path: "/enrol", MaxAge: int(flowTTL.Seconds()),
		HttpOnly: true, Secure: s.enrol.secure(),
		// Lax, not Strict: the provider's redirect back is a cross-site navigation.
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, s.enrol.oauth(p).AuthCodeURL(state,
		oauth2.S256ChallengeOption(verifier), oidc.Nonce(nonce)), http.StatusFound)
}

func (s *Server) enrolCallback(w http.ResponseWriter, r *http.Request) {
	if s.enrol == nil {
		http.Redirect(w, r, "/enrol", http.StatusSeeOther)
		return
	}
	const retry = "Sign-in didn't finish. Please try again."
	c, err := r.Cookie(flowCookie)
	http.SetCookie(w, &http.Cookie{Name: flowCookie, Path: "/enrol", MaxAge: -1, HttpOnly: true, Secure: s.enrol.secure(), SameSite: http.SameSiteLaxMode})
	q := r.URL.Query()
	if err != nil || q.Get("error") != "" {
		s.renderEnrol(w, r, http.StatusBadRequest, retry)
		return
	}
	flow := strings.SplitN(c.Value, ".", 3)
	if len(flow) != 3 || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(flow[0])) != 1 {
		s.renderEnrol(w, r, http.StatusBadRequest, retry)
		return
	}
	nonce, verifier := flow[1], flow[2]
	p, err := s.oidc.provider()
	if err != nil {
		s.renderEnrol(w, r, http.StatusServiceUnavailable, "Sign-in is unavailable right now. Please try again in a few minutes.")
		return
	}
	ctx, cancel := context.WithTimeout(oidc.ClientContext(r.Context(), providerClient), 15*time.Second)
	defer cancel()
	tok, err := s.enrol.oauth(p).Exchange(ctx, q.Get("code"), oauth2.VerifierOption(verifier))
	if err != nil {
		log.Printf("enrol: token exchange: %v", err)
		s.renderEnrol(w, r, http.StatusBadGateway, retry)
		return
	}
	rawID, _ := tok.Extra("id_token").(string)
	idt, err := s.enrol.idVerifier(p).Verify(ctx, rawID)
	if err != nil || subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(nonce)) != 1 {
		log.Printf("enrol: id token rejected: %v", err)
		s.renderEnrol(w, r, http.StatusBadRequest, retry)
		return
	}
	var claims map[string]any
	if err := idt.Claims(&claims); err != nil {
		s.renderEnrol(w, r, http.StatusBadRequest, retry)
		return
	}

	name, _ := claims["name"].(string)
	if name == "" {
		name, _ = claims["preferred_username"].(string)
	}
	ident, err := s.registry.Link(identity.Provider{Issuer: idt.Issuer, Subject: idt.Subject}, name, s.now())
	if err != nil {
		s.renderEnrol(w, r, http.StatusBadRequest, retry)
		return
	}
	if err := s.rbac.SetRoles(ident.ID, s.policyRoles(claims, tok.AccessToken)); err != nil {
		s.renderEnrol(w, r, http.StatusInternalServerError, retry)
		return
	}
	if err := s.save(); err != nil {
		log.Printf("save state: %v", err)
		s.renderEnrol(w, r, http.StatusInternalServerError, "Something went wrong saving your account. Please try again.")
		return
	}

	id, _ := s.enrol.startSession(ident.ID, s.now())
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: id, Path: "/enrol", MaxAge: int(sessionTTL.Seconds()),
		HttpOnly: true, Secure: s.enrol.secure(), SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/enrol", http.StatusSeeOther)
}

// policyRoles reads the provider's roles for the person signing in and keeps
// those the policy defines. Keycloak puts realm roles in the access token by
// default, so it is the fallback when the ID token has no roles claim.
func (s *Server) policyRoles(idClaims map[string]any, accessToken string) []string {
	path := s.oidc.cfg.RolesClaim
	if _, ok := claimAt(idClaims, path); !ok {
		idClaims = unverifiedClaims(accessToken)
	}
	var kept []string
	for _, role := range rolesAt(idClaims, path) {
		if _, defined := s.policy.Roles[role]; defined && !slices.Contains(kept, role) {
			kept = append(kept, role)
		}
	}
	slices.Sort(kept)
	return kept
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

func (s *Server) enrolLogout(w http.ResponseWriter, r *http.Request, id string, _ session) {
	s.enrol.endSession(id)
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/enrol", MaxAge: -1, HttpOnly: true, Secure: s.enrol.secure(), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/enrol?done=signedout", http.StatusSeeOther)
}

func (s *Server) enrolTicket(w http.ResponseWriter, _ *http.Request, _ string, sess session) {
	if s.enrol.voiceURL == "" {
		fail(w, http.StatusNotFound, "no voice producer is configured")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	reply(w, http.StatusOK, map[string]string{"ticket": s.enrol.issueTicket(sess.identityID, s.now())})
}

// redeemTicket lets a voice producer learn which identity a recording is for.
func (s *Server) redeemTicket(w http.ResponseWriter, r *http.Request, _ Principal) {
	var body struct {
		Ticket string `json:"ticket"`
	}
	if !decode(w, r, &body) {
		return
	}
	if s.enrol == nil {
		fail(w, http.StatusNotFound, "unknown, used, or expired ticket")
		return
	}
	id, ok := s.enrol.redeem(body.Ticket, s.now())
	if !ok || !s.registry.MarkVoiceEnrolled(id, s.now()) {
		fail(w, http.StatusNotFound, "unknown, used, or expired ticket")
		return
	}
	if !s.commit(w) {
		return
	}
	reply(w, http.StatusOK, map[string]string{"identityId": id})
}

func (s *Server) enrolConsent(w http.ResponseWriter, r *http.Request, _ string, sess session) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request.", http.StatusBadRequest)
		return
	}
	now := s.now()
	for _, offer := range s.offers(sess.identityID) {
		// Only what the page offered can be chosen, and never "*": consent
		// must not widen when roles do.
		var chosen []string
		for _, p := range offer.permissions {
			if slices.Contains(r.PostForm["perm:"+offer.agent], p) {
				chosen = append(chosen, p)
			}
		}
		live := s.liveDelegations(sess.identityID, offer.agent, now)
		if slices.Equal(chosen, covered(offer.permissions, live)) {
			continue
		}
		// Replacing is revoke then add.
		for _, d := range live {
			s.delegations.Revoke(d.ID, now)
		}
		if len(chosen) > 0 {
			if _, err := s.delegations.Add(decision.Delegation{
				IdentityID: sess.identityID, Agent: offer.agent, Permissions: chosen, GrantedBy: sess.identityID,
			}, now); err != nil {
				s.renderEnrol(w, r, http.StatusBadRequest, "That choice couldn't be saved.")
				return
			}
		}
	}
	if err := s.save(); err != nil {
		log.Printf("save state: %v", err)
		s.renderEnrol(w, r, http.StatusInternalServerError, "Your choices couldn't be saved. Please try again.")
		return
	}
	http.Redirect(w, r, "/enrol?done=saved", http.StatusSeeOther)
}

func (s *Server) enrolRevoke(w http.ResponseWriter, r *http.Request, _ string, sess session) {
	d, ok := s.delegations.Get(r.PostFormValue("id"))
	// A person can only withdraw their own, still-live consent.
	if !ok || d.IdentityID != sess.identityID || d.RevokedAt != nil {
		http.Error(w, "Not found.", http.StatusNotFound)
		return
	}
	s.delegations.Revoke(d.ID, s.now())
	if err := s.save(); err != nil {
		log.Printf("save state: %v", err)
		s.renderEnrol(w, r, http.StatusInternalServerError, "That couldn't be removed. Please try again.")
		return
	}
	http.Redirect(w, r, "/enrol?done=revoked", http.StatusSeeOther)
}

// offer is what one agent may ask to do for an identity.
type offer struct {
	agent       string
	permissions []string
}

// offers lists, per agent, the permissions the agent may request and the
// identity's roles hold. Floor permissions need no consent and permissions
// without a band can never be granted, so neither is offered.
func (s *Server) offers(identityID string) []offer {
	held := map[string]bool{}
	for _, role := range s.rbac.Roles(identityID) {
		for _, p := range s.policy.Roles[role] {
			held[p] = true
		}
	}
	var agents []string
	for agent := range s.policy.Agents {
		agents = append(agents, agent)
	}
	slices.Sort(agents)
	var out []offer
	for _, agent := range agents {
		allowed := s.policy.Agents[agent]
		var perms []string
		for p := range s.policy.Permissions {
			if held[p] && !slices.Contains(s.policy.Floor, p) &&
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
