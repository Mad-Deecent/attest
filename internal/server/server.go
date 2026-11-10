// Package server is the HTTP surface over the domain packages.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mad-deecent/attest/internal/decision"
	"github.com/mad-deecent/attest/internal/identity"
	"github.com/mad-deecent/attest/internal/perception"
)

// Principal is who a bearer token belongs to. Kinds hold disjoint powers: a
// producer submits evidence, an agent asks for decisions, an admin manages
// identities and delegations.
type Principal struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	// kinds are the kinds a provider-issued token confers through its roles.
	kinds []string
	// viaSetup marks the first-run setup session, which no token can claim.
	viaSetup bool
}

func (p Principal) is(kind string) bool {
	return p.Kind == kind || slices.Contains(p.kinds, kind)
}

const (
	KindAdmin    = "admin"
	KindProducer = "producer"
	KindAgent    = "agent"
)

// ParseConfig decodes a JSON config file into cfg. The "enrol" key is a
// deprecated alias for "enroll"; it is used (with one log line) only when
// "enroll" is absent.
func ParseConfig(raw []byte, cfg *Config) error {
	if err := json.Unmarshal(raw, cfg); err != nil {
		return err
	}
	var old struct {
		Enrol  *EnrollConfig `json:"enrol"`
		Enroll *EnrollConfig `json:"enroll"`
	}
	if err := json.Unmarshal(raw, &old); err != nil {
		return err
	}
	if old.Enrol != nil {
		log.Printf("config: key \"enrol\" is deprecated; rename it to \"enroll\"")
		if old.Enroll == nil {
			cfg.Enroll = *old.Enrol
		}
	}
	return nil
}

type Config struct {
	Listen string `json:"listen"`
	// Tokens maps a static bearer token to its principal. For development;
	// production credentials come from the identity provider.
	Tokens map[string]Principal `json:"tokens,omitempty"`
	OIDC   *OIDCConfig          `json:"oidc,omitempty"`
	// PublicURL is where people reach attest, e.g. http://localhost:8420.
	// Enrollment redirects back to it.
	PublicURL string       `json:"publicUrl,omitempty"`
	Enroll    EnrollConfig `json:"enroll"`
	// Data is the file attest keeps identities, roles, and delegations in.
	// Empty keeps them in memory only.
	Data   string            `json:"data,omitempty"`
	Fusion perception.Config `json:"fusion"`
	Policy decision.Policy   `json:"policy"`
	// ResetSetup puts attest back into first-run setup, for an operator who
	// has locked themselves out of settings. Set by a flag, never a file.
	ResetSetup bool `json:"-"`
}

type Server struct {
	tokens      map[string]Principal
	registry    *identity.Registry
	fusion      *perception.Fusion
	delegations *decision.Delegations
	rbac        *decision.RBAC
	enroll      *enrollment
	setup       *setup
	data        string
	live        atomic.Pointer[live]
	settingsMu  sync.Mutex
	saveMu      sync.Mutex
	now         func() time.Time
}

func New(cfg Config, now func() time.Time) (*Server, error) {
	if cfg.Data != "" {
		if err := checkDataDir(filepath.Dir(cfg.Data)); err != nil {
			return nil, err
		}
	}
	snap, err := readSnapshot(cfg.Data)
	if err != nil {
		return nil, err
	}
	var st Settings
	if snap != nil && snap.Settings != nil {
		st = *snap.Settings
	}
	seeded, err := seed(&st, cfg)
	if err != nil {
		return nil, err
	}
	if st.Policy, err = preparePolicy(st.Policy); err != nil {
		return nil, err
	}
	registry := identity.NewRegistry()
	// Config assignments seed role state on the first start only. Once the
	// data file holds role state it is the whole truth: roles emptied at
	// runtime stay empty.
	seedRoles := st.Policy
	seedRoles.Assignments = nil
	if snap == nil || snap.Assignments == nil {
		seedRoles.Assignments = cfg.Policy.Assignments
		if err := seedRoles.Validate(); err != nil {
			return nil, err
		}
	}
	s := &Server{
		tokens:      cfg.Tokens,
		registry:    registry,
		fusion:      perception.NewFusion(cfg.Fusion, registry),
		delegations: decision.NewDelegations(),
		rbac:        decision.NewRBAC(seedRoles),
		enroll:      newEnrollment(cfg.Enroll),
		setup:       &setup{},
		data:        cfg.Data,
		now:         now,
	}
	l, err := s.build(st)
	if err != nil {
		return nil, err
	}
	s.live.Store(l)
	if snap != nil {
		s.restore(*snap)
	}
	if s.data != "" && (snap == nil || snap.Settings == nil || seeded) {
		if err := s.save(); err != nil {
			return nil, fmt.Errorf("save data: %w", err)
		}
	}
	switch {
	case st.SetupComplete:
	case s.data == "":
		log.Printf("attest is not set up, and without a data file it cannot be: start it with -data, or set data in the config file")
	default:
		s.setup.begin()
	}
	return s, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	s.setupRoutes(mux)
	s.settingsRoutes(mux)

	mux.HandleFunc("GET /v1/identities", s.as(KindAdmin, s.listIdentities))
	mux.HandleFunc("PUT /v1/identities/{id}", s.as(KindAdmin, s.putIdentity))
	mux.HandleFunc("PUT /v1/identities/{id}/roles", s.as(KindAdmin, s.putRoles))
	mux.HandleFunc("DELETE /v1/identities/{id}", s.as(KindAdmin, s.deleteIdentity))
	mux.HandleFunc("GET /v1/delegations", s.as(KindAdmin, s.listDelegations))
	mux.HandleFunc("POST /v1/delegations", s.as(KindAdmin, s.addDelegation))
	mux.HandleFunc("DELETE /v1/delegations/{id}", s.as(KindAdmin, s.revokeDelegation))

	mux.HandleFunc("POST /v1/evidence", s.as(KindProducer, s.postEvidence))
	mux.HandleFunc("POST /v1/associations", s.as(KindProducer, s.postAssociation))
	mux.HandleFunc("POST /v1/retirements", s.as(KindProducer, s.postRetirement))

	mux.HandleFunc("POST /v1/enrollment-tickets/redeem", s.as(KindProducer, s.redeemTicket))

	s.enrollRoutes(mux)

	mux.HandleFunc("GET /v1/subjects/{key}/belief", s.as(KindAgent, s.getBelief))
	mux.HandleFunc("POST /v1/decisions", s.as(KindAgent, s.postDecision))
	return mux
}

type handler func(w http.ResponseWriter, r *http.Request, p Principal)

func (s *Server) as(kind string, next handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, status := s.authenticate(r)
		switch {
		case status == http.StatusServiceUnavailable:
			fail(w, status, "identity provider unavailable")
		case status != http.StatusOK:
			fail(w, status, "missing or invalid bearer token")
		case !p.is(kind):
			fail(w, http.StatusForbidden, "this endpoint requires a "+kind+" credential")
		default:
			next(w, r, p)
		}
	}
}

func (s *Server) authenticate(r *http.Request) (Principal, int) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		return Principal{}, http.StatusUnauthorized
	}
	if p, known := s.tokens[token]; known {
		return p, http.StatusOK
	}
	verifier := s.cur().oidc
	if verifier == nil {
		return Principal{}, http.StatusUnauthorized
	}
	p, err := verifier.principal(r.Context(), token)
	if errors.Is(err, errProviderUnavailable) {
		return Principal{}, http.StatusServiceUnavailable
	}
	if err != nil {
		return Principal{}, http.StatusUnauthorized
	}
	return p, http.StatusOK
}

func (s *Server) listIdentities(w http.ResponseWriter, _ *http.Request, _ Principal) {
	reply(w, http.StatusOK, s.registry.List())
}

func (s *Server) putIdentity(w http.ResponseWriter, r *http.Request, _ Principal) {
	var body struct {
		DisplayName string `json:"displayName"`
	}
	if !decode(w, r, &body) {
		return
	}
	ident, err := s.registry.Put(r.PathValue("id"), body.DisplayName, s.now())
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.commit(w) {
		return
	}
	reply(w, http.StatusOK, ident)
}

func (s *Server) putRoles(w http.ResponseWriter, r *http.Request, _ Principal) {
	var body struct {
		Roles []string `json:"roles"`
	}
	if !decode(w, r, &body) {
		return
	}
	id := r.PathValue("id")
	if !s.registry.Has(id) {
		fail(w, http.StatusNotFound, "no such identity")
		return
	}
	if err := s.rbac.SetRoles(id, body.Roles); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.commit(w) {
		return
	}
	reply(w, http.StatusOK, map[string][]string{"roles": s.rbac.Roles(id)})
}

func (s *Server) deleteIdentity(w http.ResponseWriter, r *http.Request, _ Principal) {
	id := r.PathValue("id")
	if !s.registry.Delete(id) {
		fail(w, http.StatusNotFound, "no such identity")
		return
	}
	s.fusion.RetireIdentity(id)
	_ = s.rbac.SetRoles(id, nil)
	s.delegations.RevokeIdentity(id, s.now())
	if !s.commit(w) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listDelegations(w http.ResponseWriter, _ *http.Request, _ Principal) {
	reply(w, http.StatusOK, s.delegations.List())
}

func (s *Server) addDelegation(w http.ResponseWriter, r *http.Request, _ Principal) {
	var d decision.Delegation
	if !decode(w, r, &d) {
		return
	}
	if !s.registry.Has(d.IdentityID) {
		fail(w, http.StatusBadRequest, "no such identity")
		return
	}
	created, err := s.delegations.Add(d, s.now())
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.commit(w) {
		return
	}
	reply(w, http.StatusCreated, created)
}

func (s *Server) revokeDelegation(w http.ResponseWriter, r *http.Request, _ Principal) {
	if !s.delegations.Revoke(r.PathValue("id"), s.now()) {
		fail(w, http.StatusNotFound, "no such delegation")
		return
	}
	if !s.commit(w) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) postEvidence(w http.ResponseWriter, r *http.Request, p Principal) {
	var e perception.Evidence
	if !decode(w, r, &e) {
		return
	}
	// A producer speaks only for itself.
	e.Source = p.ID
	if err := s.fusion.Observe(e); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) postAssociation(w http.ResponseWriter, r *http.Request, _ Principal) {
	var a perception.Association
	if !decode(w, r, &a) {
		return
	}
	if err := s.fusion.Associate(a); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) postRetirement(w http.ResponseWriter, r *http.Request, p Principal) {
	var body struct {
		Subject string `json:"subject"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Subject != "" {
		s.fusion.RetireSubject(body.Subject)
	} else {
		s.fusion.RetireSource(p.ID)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getBelief(w http.ResponseWriter, r *http.Request, _ Principal) {
	belief := s.fusion.Current(r.PathValue("key"), s.now())
	if belief == nil {
		fail(w, http.StatusNotFound, "no live evidence for subject")
		return
	}
	reply(w, http.StatusOK, belief)
}

func (s *Server) postDecision(w http.ResponseWriter, r *http.Request, p Principal) {
	var req decision.Request
	if !decode(w, r, &req) {
		return
	}
	// An absent subject is allowed: the request can then only be granted
	// floor permissions.
	if req.Permission == "" {
		fail(w, http.StatusBadRequest, "permission is required")
		return
	}
	req.Agent = p.ID
	reply(w, http.StatusOK, s.cur().evaluator.Decide(req, s.now()))
}

func decode(w http.ResponseWriter, r *http.Request, into any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		fail(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return false
	}
	return true
}

func reply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	// Reasons contain comparison operators; keep them readable.
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
}

func fail(w http.ResponseWriter, status int, message string) {
	reply(w, status, map[string]string{"error": message})
}
