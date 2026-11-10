package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// OIDCConfig points attest at the identity provider that issues credentials.
// Users, roles, and SSO live there; attest only verifies what it issues.
type OIDCConfig struct {
	// Issuer is the provider's issuer URL, e.g. a Keycloak realm URL.
	Issuer string `json:"issuer"`
	// Audience, when set, must appear in the token's aud claim.
	Audience string `json:"audience,omitempty"`
	// RolesClaim is the dotted path to the token's role list.
	RolesClaim string `json:"rolesClaim,omitempty"`
	// Roles maps a credential kind (admin, producer, agent) to the provider
	// role that confers it.
	Roles map[string]string `json:"roles,omitempty"`
}

func (c OIDCConfig) withDefaults() OIDCConfig {
	if c.RolesClaim == "" {
		c.RolesClaim = "realm_access.roles"
	}
	roles := map[string]string{
		KindAdmin:    "attest-admin",
		KindProducer: "attest-producer",
		KindAgent:    "attest-agent",
	}
	for kind, role := range c.Roles {
		roles[kind] = role
	}
	c.Roles = roles
	return c
}

var errProviderUnavailable = errors.New("identity provider unavailable")

// oidcVerifier discovers the provider on first use, so attest starts and
// keeps identifying people while the provider is down.
type oidcVerifier struct {
	cfg OIDCConfig
	mu  sync.Mutex
	v   *oidc.IDTokenVerifier
}

func (o *oidcVerifier) verifier() (*oidc.IDTokenVerifier, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.v != nil {
		return o.v, nil
	}
	// The provider keeps this context for later key fetches, so it must
	// outlive the request that triggered discovery.
	ctx := oidc.ClientContext(context.Background(), &http.Client{Timeout: 10 * time.Second})
	provider, err := oidc.NewProvider(ctx, o.cfg.Issuer)
	if err != nil {
		return nil, errProviderUnavailable
	}
	o.v = provider.Verifier(&oidc.Config{
		ClientID:          o.cfg.Audience,
		SkipClientIDCheck: o.cfg.Audience == "",
	})
	return o.v, nil
}

func (o *oidcVerifier) principal(ctx context.Context, raw string) (Principal, error) {
	v, err := o.verifier()
	if err != nil {
		return Principal{}, err
	}
	token, err := v.Verify(ctx, raw)
	if err != nil {
		return Principal{}, err
	}
	var claims map[string]any
	if err := token.Claims(&claims); err != nil {
		return Principal{}, err
	}
	p := Principal{ID: token.Subject}
	// For a client-credentials token the client is the actor.
	for _, name := range []string{"azp", "client_id"} {
		if id, ok := claims[name].(string); ok && id != "" {
			p.ID = id
			break
		}
	}
	held := map[string]bool{}
	for _, role := range rolesAt(claims, o.cfg.RolesClaim) {
		held[role] = true
	}
	for kind, role := range o.cfg.Roles {
		if held[role] {
			p.kinds = append(p.kinds, kind)
		}
	}
	return p, nil
}

// rolesAt reads a string list at a dotted claim path.
func rolesAt(claims map[string]any, path string) []string {
	var node any = claims
	for _, part := range strings.Split(path, ".") {
		m, ok := node.(map[string]any)
		if !ok {
			return nil
		}
		node = m[part]
	}
	list, _ := node.([]any)
	var out []string
	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
