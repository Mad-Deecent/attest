package server

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/mad-deecent/attest/internal/decision"
	"github.com/mad-deecent/attest/internal/perception"
)

// issuer is a minimal OIDC provider: discovery, keys, and a way to mint tokens.
type issuer struct {
	t      *testing.T
	srv    *httptest.Server
	signer jose.Signer
}

func newIssuer(t *testing.T) *issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: "k1"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	is := &issuer{t: t, signer: signer}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                is.srv.URL,
			"jwks_uri":                              is.srv.URL + "/keys",
			"authorization_endpoint":                is.srv.URL + "/auth",
			"token_endpoint":                        is.srv.URL + "/token",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"},
		}})
	})
	is.srv = httptest.NewServer(mux)
	t.Cleanup(is.srv.Close)
	return is
}

func (is *issuer) token(client string, expires time.Time, roles ...string) string {
	is.t.Helper()
	raw, err := jwt.Signed(is.signer).Claims(map[string]any{
		"iss":          is.srv.URL,
		"sub":          "service-account-" + client,
		"azp":          client,
		"exp":          expires.Unix(),
		"realm_access": map[string]any{"roles": roles},
	}).Serialize()
	if err != nil {
		is.t.Fatal(err)
	}
	return raw
}

func TestProviderIssuedCredentials(t *testing.T) {
	is := newIssuer(t)
	srv, err := New(Config{
		OIDC:   &OIDCConfig{Issuer: is.srv.URL},
		Fusion: perception.DefaultConfig(),
		Policy: decision.Policy{
			Floor:  []string{"emergency.call"},
			Agents: map[string][]string{"concierge": {"*"}},
		},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	hn := &harness{t: t, h: srv.Handler(), now: time.Now()}
	soon := time.Now().Add(time.Hour)
	req := decision.Request{Subject: "s", Permission: "emergency.call"}

	// The agent's identity is the client the provider issued the token to.
	var d decision.Decision
	hn.call(is.token("concierge", soon, "attest-agent"), "POST", "/v1/decisions", req, http.StatusOK, &d)
	if !d.Granted || d.Agent != "concierge" {
		t.Fatalf("want grant for concierge, got %+v", d)
	}
	hn.call(is.token("unlisted-bot", soon, "attest-agent"), "POST", "/v1/decisions", req, http.StatusOK, &d)
	denied(t, d, "agent may not request")

	// Roles in the token decide which endpoints it opens.
	producer := is.token("cam", soon, "attest-producer")
	hn.call(producer, "POST", "/v1/decisions", req, http.StatusForbidden, nil)
	hn.call(producer, "POST", "/v1/retirements", map[string]string{}, http.StatusNoContent, nil)
	hn.call(is.token("nobody", soon), "POST", "/v1/decisions", req, http.StatusForbidden, nil)

	hn.call(is.token("concierge", time.Now().Add(-time.Minute), "attest-agent"),
		"POST", "/v1/decisions", req, http.StatusUnauthorized, nil)
	other := newIssuer(t)
	hn.call(other.token("concierge", soon, "attest-agent"), "POST", "/v1/decisions", req, http.StatusUnauthorized, nil)
}

func TestProviderOutageIsReportedNotFatal(t *testing.T) {
	srv, err := New(Config{
		OIDC:   &OIDCConfig{Issuer: "http://127.0.0.1:1"},
		Tokens: map[string]Principal{"dev": {Kind: KindAgent, ID: "concierge"}},
		Fusion: perception.DefaultConfig(),
		Policy: decision.Policy{Floor: []string{"emergency.call"}, Agents: map[string][]string{"concierge": {"*"}}},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	hn := &harness{t: t, h: srv.Handler(), now: time.Now()}
	req := decision.Request{Subject: "s", Permission: "emergency.call"}
	hn.call("some.provider.token", "POST", "/v1/decisions", req, http.StatusServiceUnavailable, nil)
	hn.call("dev", "POST", "/v1/decisions", req, http.StatusOK, nil)
}
