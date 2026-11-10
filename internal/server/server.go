// Package server is the HTTP surface over the domain packages.
package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/mad-deecent/attest/internal/assurance"
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
}

const (
	KindAdmin    = "admin"
	KindProducer = "producer"
	KindAgent    = "agent"
)

type Config struct {
	Listen string `json:"listen"`
	// Tokens maps a static bearer token to its principal.
	Tokens map[string]Principal `json:"tokens"`
	Fusion perception.Config    `json:"fusion"`
	Policy decision.Policy      `json:"policy"`
}

type Server struct {
	tokens      map[string]Principal
	registry    *identity.Registry
	fusion      *perception.Fusion
	delegations *decision.Delegations
	rbac        *decision.RBAC
	evaluator   *decision.Evaluator
	now         func() time.Time
}

func New(cfg Config, now func() time.Time) (*Server, error) {
	if len(cfg.Policy.Bands) == 0 {
		cfg.Policy.Bands = assurance.DefaultLadder()
	}
	if err := cfg.Policy.Validate(); err != nil {
		return nil, err
	}
	registry := identity.NewRegistry()
	fusion := perception.NewFusion(cfg.Fusion, registry)
	delegations := decision.NewDelegations()
	rbac := decision.NewRBAC(cfg.Policy)
	return &Server{
		tokens:      cfg.Tokens,
		registry:    registry,
		fusion:      fusion,
		delegations: delegations,
		rbac:        rbac,
		evaluator: &decision.Evaluator{
			Policy:      cfg.Policy,
			Fusion:      fusion,
			Authz:       rbac,
			Delegations: delegations,
			Bands:       assurance.Static{},
		},
		now: now,
	}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

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

	mux.HandleFunc("GET /v1/subjects/{key}/belief", s.as(KindAgent, s.getBelief))
	mux.HandleFunc("POST /v1/decisions", s.as(KindAgent, s.postDecision))
	return mux
}

type handler func(w http.ResponseWriter, r *http.Request, p Principal)

func (s *Server) as(kind string, next handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		p, known := s.tokens[token]
		if !ok || !known {
			fail(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		if p.Kind != kind {
			fail(w, http.StatusForbidden, "this endpoint requires a "+kind+" credential")
			return
		}
		next(w, r, p)
	}
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
	reply(w, http.StatusCreated, created)
}

func (s *Server) revokeDelegation(w http.ResponseWriter, r *http.Request, _ Principal) {
	if !s.delegations.Revoke(r.PathValue("id"), s.now()) {
		fail(w, http.StatusNotFound, "no such delegation")
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
	if req.Subject == "" || req.Permission == "" {
		fail(w, http.StatusBadRequest, "subject and permission are required")
		return
	}
	req.Agent = p.ID
	reply(w, http.StatusOK, s.evaluator.Decide(req, s.now()))
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
