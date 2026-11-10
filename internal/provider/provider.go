// Package provider connects attest to an identity provider in one step: given
// a one-time admin credential, an adapter makes sure the provider has what
// attest needs (roles, an enrollment client) and reports how to reach it.
//
// An adapter uses the credential for the length of one Connect call. It never
// returns, stores, or logs it, nor the admin token it obtains with it.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Credential is the one-time admin credential: a person's username and
// password, or a service client's id and secret.
type Credential struct {
	Username     string `json:"username,omitempty"`
	Password     string `json:"password,omitempty"`
	ClientID     string `json:"clientId,omitempty"`
	ClientSecret string `json:"clientSecret,omitempty"`
}

func (c Credential) user() bool    { return c.Username != "" }
func (c Credential) service() bool { return c.ClientID != "" }

// Request is what a person fills in to connect a provider.
type Request struct {
	// Kind picks the adapter: keycloak, auth0, ory, or oidc for any issuer
	// whose client the operator creates by hand.
	Kind string `json:"kind"`
	// URL is the provider's base URL (keycloak).
	URL string `json:"url,omitempty"`
	// Realm is the Keycloak realm people and agents live in.
	Realm string `json:"realm,omitempty"`
	// AdminRealm is where the admin credential lives, when not Realm (often
	// "master").
	AdminRealm string     `json:"adminRealm,omitempty"`
	Credential Credential `json:"credential"`
	// Issuer is the issuer URL (oidc).
	Issuer string `json:"issuer,omitempty"`
	// ClientID is the public client people enroll through. The keycloak
	// adapter creates it when missing (default attest-enroll); with oidc it is
	// whatever client the operator made, or empty for no enrollment.
	ClientID string `json:"clientId,omitempty"`

	// Set by attest, not by the caller.

	// PublicURL is where people reach attest; it decides the redirect URI.
	PublicURL string `json:"-"`
	// Roles are the provider roles attest reads, the admin role first.
	Roles []string `json:"-"`
}

// Result is how to reach the provider, and what connecting changed there.
type Result struct {
	Issuer   string   `json:"issuer"`
	ClientID string   `json:"clientId,omitempty"`
	Changes  []Change `json:"changes"`
}

// Change is one thing an adapter made sure of.
type Change struct {
	// Object is "role", "client", or "role grant".
	Object string `json:"object"`
	Name   string `json:"name"`
	// Action is created, found, updated, or skipped.
	Action string `json:"action"`
	Note   string `json:"note,omitempty"`
}

// Provider connects one kind of identity provider. On error, the Result still
// lists what was changed before the failure, so a person can see it; running
// Connect again picks up where it stopped.
type Provider interface {
	Connect(ctx context.Context, req Request) (Result, error)
}

// ErrNotSupported is returned by adapters that exist in name only.
var ErrNotSupported = errors.New("not yet supported")

// ErrInput marks a request that is wrong as written, as opposed to a provider
// that refused or failed.
var ErrInput = errors.New("invalid request")

func inputf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInput, fmt.Sprintf(format, args...))
}

// Client is the HTTP client adapters use.
var Client = &http.Client{Timeout: 15 * time.Second}

// noRedirects returns a copy of c that stops at a redirect. A redirect would
// carry the admin credential, or a token, to wherever it points.
func noRedirects(c *http.Client) *http.Client {
	cp := *c
	cp.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &cp
}

func redirected(status int) bool { return status >= 300 && status <= 399 }

// For returns the adapter for a kind.
func For(kind string) (Provider, error) {
	switch kind {
	case "keycloak":
		return Keycloak{Client: Client}, nil
	case "oidc":
		return Manual{Client: Client}, nil
	case "auth0", "ory":
		return unsupported(kind), nil
	}
	return nil, inputf("unknown provider kind %q (want keycloak, auth0, ory, or oidc)", kind)
}

type unsupported string

func (u unsupported) Connect(context.Context, Request) (Result, error) {
	return Result{}, fmt.Errorf("connecting %s in one step is %w; use kind oidc with a client you create", string(u), ErrNotSupported)
}

// Manual connects any OIDC issuer whose enrollment client the operator made by
// hand. It only checks that the issuer answers discovery.
type Manual struct{ Client *http.Client }

func (m Manual) Connect(ctx context.Context, req Request) (Result, error) {
	if req.Issuer == "" {
		return Result{}, inputf("issuer is required")
	}
	issuer, err := Discover(ctx, m.Client, req.Issuer)
	if err != nil {
		return Result{}, err
	}
	return Result{Issuer: issuer, ClientID: req.ClientID, Changes: []Change{}}, nil
}

// Discover fetches an issuer's discovery document and returns the issuer it
// names, which is what tokens will carry.
func Discover(ctx context.Context, client *http.Client, issuer string) (string, error) {
	u, err := url.Parse(issuer)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", inputf("issuer must be an absolute http(s) URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(issuer, "/")+"/.well-known/openid-configuration", nil)
	if err != nil {
		return "", err
	}
	resp, err := noRedirects(client).Do(req)
	if err != nil {
		return "", fmt.Errorf("discovery at %s: %w", u.Host, err)
	}
	defer resp.Body.Close()
	if redirected(resp.StatusCode) {
		return "", fmt.Errorf("discovery at %s: answered with a redirect (%d), which attest does not follow; check the URL", u.Host, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("discovery at %s: status %d", u.Host, resp.StatusCode)
	}
	var doc struct {
		Issuer string `json:"issuer"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil || doc.Issuer == "" {
		return "", fmt.Errorf("discovery at %s: not an OIDC discovery document", u.Host)
	}
	return doc.Issuer, nil
}
