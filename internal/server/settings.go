package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/mad-deecent/attest/internal/assurance"
	"github.com/mad-deecent/attest/internal/decision"
	"github.com/mad-deecent/attest/internal/provider"
)

// Settings are what an operator changes while attest runs: how to reach the
// identity provider, where people reach attest, and policy. They live in the
// data file next to identities; a config file only seeds them.
type Settings struct {
	// SetupComplete ends first-run setup: the setup code stops working and
	// only admin credentials can change settings.
	SetupComplete bool   `json:"setupComplete"`
	PublicURL     string `json:"publicUrl,omitempty"`
	// Provider says how the connection was made: keycloak, oidc, or config.
	Provider string          `json:"provider,omitempty"`
	OIDC     *OIDCConfig     `json:"oidc,omitempty"`
	Policy   decision.Policy `json:"policy"`
	// Seeds are digests of the config-file values last applied, per field.
	// A config file value is applied again only when it changes, so editing
	// the file still works and a restart does not undo changes made since.
	Seeds map[string]string `json:"seeds,omitempty"`
}

// live is the settings in force and everything derived from them. It is
// replaced whole, never changed: a request loads it once and sees one
// consistent connection and policy throughout.
type live struct {
	settings  Settings
	oidc      *oidcVerifier
	public    *url.URL
	evaluator *decision.Evaluator
}

func (l *live) enrolling() bool {
	return l.oidc != nil && l.oidc.cfg.ClientID != "" && l.public != nil
}

func (l *live) policy() decision.Policy { return l.settings.Policy }

func (s *Server) cur() *live { return s.live.Load() }

// build derives a live configuration from settings without applying it.
func (s *Server) build(st Settings) (*live, error) {
	l := &live{settings: st}
	if st.OIDC != nil {
		if st.OIDC.Issuer == "" {
			return nil, errors.New("oidc.issuer is required")
		}
		l.oidc = &oidcVerifier{cfg: st.OIDC.withDefaults()}
	}
	if st.PublicURL != "" {
		u, err := parsePublicURL(st.PublicURL)
		if err != nil {
			return nil, err
		}
		l.public = u
	}
	l.evaluator = &decision.Evaluator{
		Policy:      st.Policy,
		Fusion:      s.fusion,
		Authz:       s.rbac,
		Delegations: s.delegations,
		Bands:       assurance.Static{},
	}
	return l, nil
}

var errNotSaved = errors.New("could not save state")

// apply saves settings and then puts them in force. If they cannot be saved,
// nothing changes. Callers hold settingsMu.
func (s *Server) apply(st Settings) (*live, error) {
	l, err := s.build(st)
	if err != nil {
		return nil, err
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	if err := s.writeLocked(st); err != nil {
		log.Printf("save state: %v", err)
		return nil, errNotSaved
	}
	s.live.Store(l)
	return l, nil
}

// preparePolicy fills in the default ladder and validates, the same at
// startup and at runtime.
func preparePolicy(p decision.Policy) (decision.Policy, error) {
	if len(p.Bands) == 0 {
		p.Bands = assurance.DefaultLadder()
	}
	return p, p.Validate()
}

func parsePublicURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, errors.New("publicUrl must be an absolute http(s) URL")
	}
	return u, nil
}

// seed applies the config file's settings that changed since they were last
// applied, and reports whether anything did.
func seed(st *Settings, cfg Config) (bool, error) {
	changed := false
	take := func(field string, present bool, value any, set func()) error {
		if !present {
			return nil
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		digest := hex.EncodeToString(sum[:])
		if st.Seeds[field] == digest {
			return nil
		}
		if st.Seeds == nil {
			st.Seeds = map[string]string{}
		}
		st.Seeds[field] = digest
		set()
		changed = true
		return nil
	}
	if err := take("oidc", cfg.OIDC != nil, cfg.OIDC, func() {
		o := *cfg.OIDC
		st.OIDC, st.Provider = &o, "config"
	}); err != nil {
		return false, err
	}
	if err := take("publicUrl", cfg.PublicURL != "", cfg.PublicURL, func() {
		st.PublicURL = strings.TrimRight(cfg.PublicURL, "/")
	}); err != nil {
		return false, err
	}
	var policyErr error
	if err := take("policy", !reflect.ValueOf(cfg.Policy).IsZero(), cfg.Policy, func() {
		// Assignments seed role state, which then lives with identities;
		// they are not a setting.
		_, policyErr = preparePolicy(cfg.Policy)
		st.Policy = cfg.Policy
		st.Policy.Assignments = nil
	}); err != nil {
		return false, err
	}
	if policyErr != nil {
		return false, policyErr
	}
	// A config file that brings its own provider or admin token has done
	// setup's job.
	done := cfg.OIDC != nil || slices.ContainsFunc(mapValues(cfg.Tokens), func(p Principal) bool { return p.Kind == KindAdmin })
	if cfg.ResetSetup {
		done = false
	}
	if st.SetupComplete != done && (done || cfg.ResetSetup) {
		st.SetupComplete = done
		changed = true
	}
	return changed, nil
}

func mapValues[K comparable, V any](m map[K]V) []V {
	out := make([]V, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// providerView is the connection as the API shows it, defaults filled in.
type providerView struct {
	Kind string `json:"kind"`
	OIDCConfig
}

type settingsView struct {
	SetupComplete bool            `json:"setupComplete"`
	PublicURL     string          `json:"publicUrl"`
	Provider      *providerView   `json:"provider"`
	Policy        decision.Policy `json:"policy"`
}

func (l *live) providerView() *providerView {
	if l.oidc == nil {
		return nil
	}
	return &providerView{Kind: l.settings.Provider, OIDCConfig: l.oidc.cfg}
}

func (s *Server) settingsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/settings", s.asAdmin(s.getSettings))
	mux.HandleFunc("POST /v1/settings/provider", s.asAdmin(s.connectProvider))
	mux.HandleFunc("GET /v1/settings/public-url", s.asAdmin(s.getPublicURL))
	mux.HandleFunc("PUT /v1/settings/public-url", s.asAdmin(s.putPublicURL))
	mux.HandleFunc("GET /v1/settings/policy", s.asAdmin(s.getPolicy))
	mux.HandleFunc("PUT /v1/settings/policy", s.asAdmin(s.putPolicy))
}

func (s *Server) getSettings(w http.ResponseWriter, _ *http.Request, _ Principal) {
	l := s.cur()
	reply(w, http.StatusOK, settingsView{
		SetupComplete: l.settings.SetupComplete,
		PublicURL:     l.settings.PublicURL,
		Provider:      l.providerView(),
		Policy:        l.policy(),
	})
}

// update changes settings under settingsMu, so concurrent changes to
// different settings cannot undo each other.
func (s *Server) update(w http.ResponseWriter, change func(*Settings) error) (*live, bool) {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	st := s.cur().settings
	if err := change(&st); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return nil, false
	}
	l, err := s.apply(st)
	switch {
	case errors.Is(err, errNotSaved):
		fail(w, http.StatusInternalServerError, err.Error())
		return nil, false
	case err != nil:
		fail(w, http.StatusBadRequest, err.Error())
		return nil, false
	}
	return l, true
}

func (s *Server) getPublicURL(w http.ResponseWriter, _ *http.Request, _ Principal) {
	reply(w, http.StatusOK, map[string]string{"publicUrl": s.cur().settings.PublicURL})
}

func (s *Server) putPublicURL(w http.ResponseWriter, r *http.Request, _ Principal) {
	var body struct {
		PublicURL string `json:"publicUrl"`
	}
	if !decode(w, r, &body) {
		return
	}
	l, ok := s.update(w, func(st *Settings) error {
		u, err := parsePublicURL(body.PublicURL)
		if err != nil {
			return err
		}
		st.PublicURL = u.String()
		return nil
	})
	if !ok {
		return
	}
	reply(w, http.StatusOK, map[string]string{"publicUrl": l.settings.PublicURL})
}

func (s *Server) getPolicy(w http.ResponseWriter, _ *http.Request, _ Principal) {
	reply(w, http.StatusOK, s.cur().policy())
}

func (s *Server) putPolicy(w http.ResponseWriter, r *http.Request, _ Principal) {
	var p decision.Policy
	if !decode(w, r, &p) {
		return
	}
	if len(p.Assignments) > 0 {
		fail(w, http.StatusBadRequest, "assignments are not policy: set roles at /v1/identities/{id}/roles")
		return
	}
	p, err := preparePolicy(p)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	st := s.cur().settings
	st.Policy = p
	// Role definitions change with policy; assignments of roles that are
	// gone go with them, and come back if the change cannot be saved.
	undo := s.rbac.Redefine(p.Roles)
	if _, err := s.apply(st); err != nil {
		undo()
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	reply(w, http.StatusOK, p)
}

// connectProvider connects an identity provider in one step. The admin
// credential in the request is used for this call only: it is not stored,
// logged, or returned.
func (s *Server) connectProvider(w http.ResponseWriter, r *http.Request, _ Principal) {
	var body struct {
		provider.Request
		Audience   string            `json:"audience,omitempty"`
		RolesClaim string            `json:"rolesClaim,omitempty"`
		Roles      map[string]string `json:"roles,omitempty"`
	}
	if !decode(w, r, &body) {
		return
	}
	for kind, role := range body.Roles {
		if (kind != KindAdmin && kind != KindProducer && kind != KindAgent) || role == "" {
			fail(w, http.StatusBadRequest, "roles maps admin, producer, and agent to provider role names")
			return
		}
	}
	adapter, err := provider.For(body.Kind)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	names := OIDCConfig{Roles: body.Roles}.withDefaults().Roles
	req := body.Request
	req.PublicURL = s.cur().settings.PublicURL
	req.Roles = []string{names[KindAdmin], names[KindProducer], names[KindAgent]}

	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	res, err := adapter.Connect(ctx, req)
	if res.Changes == nil {
		res.Changes = []provider.Change{}
	}
	if err != nil {
		status := http.StatusBadGateway
		switch {
		case errors.Is(err, provider.ErrInput):
			status = http.StatusBadRequest
		case errors.Is(err, provider.ErrNotSupported):
			status = http.StatusNotImplemented
		}
		// Nothing is stored: attest keeps whatever connection it had.
		reply(w, status, map[string]any{"error": err.Error(), "changes": res.Changes})
		return
	}
	l, ok := s.update(w, func(st *Settings) error {
		st.Provider = body.Kind
		st.OIDC = &OIDCConfig{
			Issuer: res.Issuer, ClientID: res.ClientID,
			Audience: body.Audience, RolesClaim: body.RolesClaim, Roles: body.Roles,
		}
		return nil
	})
	if !ok {
		return
	}
	log.Printf("identity provider connected: %s at %s", body.Kind, res.Issuer)
	reply(w, http.StatusOK, map[string]any{"provider": l.providerView(), "changes": res.Changes})
}
