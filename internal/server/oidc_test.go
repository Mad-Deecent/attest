package server

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/mad-deecent/attest/internal/decision"
	"github.com/mad-deecent/attest/internal/perception"
)

// issuer is a minimal OIDC provider: discovery, keys, a way to mint tokens,
// and an authorization-code flow with PKCE that signs in whoever user says.
type issuer struct {
	t      *testing.T
	srv    *httptest.Server
	signer jose.Signer

	mu    sync.Mutex
	user  person
	codes map[string]authRequest
}

// person is who the issuer's login page signs in.
type person struct {
	sub, name, username string
	// idRoles, when non-nil, puts realm roles in the ID token too. Keycloak
	// by default puts them only in the access token.
	idRoles     []string
	accessRoles []string
}

type authRequest struct {
	client, redirect, challenge, nonce string
	user                               person
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
	is := &issuer{t: t, signer: signer, codes: map[string]authRequest{}}
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
	mux.HandleFunc("GET /auth", is.authorize)
	mux.HandleFunc("POST /token", is.exchange)
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

func (is *issuer) signIn(p person) {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.user = p
}

// authorize approves at once, as if the user had typed their password.
func (is *issuer) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		http.Error(w, "want code flow with S256 PKCE", http.StatusBadRequest)
		return
	}
	redirect, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	is.mu.Lock()
	code := rand.Text()
	is.codes[code] = authRequest{
		client: q.Get("client_id"), redirect: q.Get("redirect_uri"),
		challenge: q.Get("code_challenge"), nonce: q.Get("nonce"), user: is.user,
	}
	is.mu.Unlock()
	back := redirect.Query()
	back.Set("code", code)
	back.Set("state", q.Get("state"))
	redirect.RawQuery = back.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound)
}

func (is *issuer) exchange(w http.ResponseWriter, r *http.Request) {
	is.mu.Lock()
	req, ok := is.codes[r.PostFormValue("code")]
	delete(is.codes, r.PostFormValue("code"))
	is.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostFormValue("code_verifier")))
	switch {
	case !ok:
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
		return
	case base64.RawURLEncoding.EncodeToString(sum[:]) != req.challenge:
		http.Error(w, `{"error":"invalid_grant","error_description":"PKCE"}`, http.StatusBadRequest)
		return
	case r.PostFormValue("client_id") != req.client || r.PostFormValue("redirect_uri") != req.redirect:
		http.Error(w, `{"error":"invalid_client"}`, http.StatusBadRequest)
		return
	}
	exp := time.Now().Add(5 * time.Minute).Unix()
	id := map[string]any{
		"iss": is.srv.URL, "sub": req.user.sub, "aud": req.client, "exp": exp, "iat": time.Now().Unix(),
		"nonce": req.nonce, "name": req.user.name, "preferred_username": req.user.username,
	}
	if req.user.idRoles != nil {
		id["realm_access"] = map[string]any{"roles": req.user.idRoles}
	}
	access := map[string]any{
		"iss": is.srv.URL, "sub": req.user.sub, "azp": req.client, "exp": exp,
		"realm_access": map[string]any{"roles": req.user.accessRoles},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": is.sign(access), "id_token": is.sign(id), "token_type": "Bearer", "expires_in": 300,
	})
}

func (is *issuer) sign(claims map[string]any) string {
	raw, err := jwt.Signed(is.signer).Claims(claims).Serialize()
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
