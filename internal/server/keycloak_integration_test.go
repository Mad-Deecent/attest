package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestKeycloakForReal connects a real Keycloak realm through the setup API.
// It runs only when asked:
//
//	ATTEST_KEYCLOAK_IT=1 go test ./internal/server -run KeycloakForReal
//
// ATTEST_KEYCLOAK_URL and ATTEST_KEYCLOAK_REALM (both required) pick the realm, and
// ATTEST_KEYCLOAK_ADMIN_FILE (required) holds "clientId:secret" of a service client
// with realm-admin. The only thing it creates is the client
// attest-enroll-setup-test, which it deletes afterwards; it checks that the
// realm's attest roles and the attest-enroll client are untouched.
func TestKeycloakForReal(t *testing.T) {
	if os.Getenv("ATTEST_KEYCLOAK_IT") == "" {
		t.Skip("set ATTEST_KEYCLOAK_IT=1 to run against a real Keycloak")
	}
	base, realm, adminFile := os.Getenv("ATTEST_KEYCLOAK_URL"), os.Getenv("ATTEST_KEYCLOAK_REALM"), os.Getenv("ATTEST_KEYCLOAK_ADMIN_FILE")
	if base == "" || realm == "" || adminFile == "" {
		t.Skip("set ATTEST_KEYCLOAK_URL (e.g. https://idp.example.com), ATTEST_KEYCLOAK_REALM and ATTEST_KEYCLOAK_ADMIN_FILE")
	}
	raw, err := os.ReadFile(adminFile)
	if err != nil {
		t.Fatal(err)
	}
	id, secret, ok := strings.Cut(strings.TrimSpace(string(raw)), ":")
	if !ok {
		t.Fatal("admin file is not clientId:secret")
	}
	const testClient = "attest-enroll-setup-test"
	kc := &realKeycloak{t: t, base: base, realm: realm}
	kc.login(id, secret)

	if kc.clientByID(testClient) != nil {
		t.Fatalf("%s already exists; refusing to touch it", testClient)
	}
	before := kc.untouchable()
	for name, v := range before {
		if v == nil {
			t.Fatalf("%s not found: nothing to compare against", name)
		}
	}
	t.Cleanup(func() {
		kc.login(id, secret)
		if c := kc.clientByID(testClient); c != nil && c["clientId"] == testClient {
			kc.call("DELETE", "/clients/"+url.PathEscape(c["id"].(string)), nil)
		}
		if kc.clientByID(testClient) != nil {
			t.Errorf("%s was not deleted", testClient)
		}
		if after := kc.untouchable(); !reflect.DeepEqual(before, after) {
			t.Errorf("the realm's attest roles or attest-enroll client changed:\nbefore %v\nafter  %v", before, after)
		}
	})

	now := time.Now()
	srv, _ := unconfigured(t, &now)
	c := openSetup(t, srv)
	c.do("PUT", "/v1/settings/public-url", map[string]string{"publicUrl": "http://localhost:8499"}, http.StatusOK, nil)
	body := map[string]any{
		"kind": "keycloak", "url": base, "realm": realm, "clientId": testClient,
		"credential": map[string]string{"clientId": id, "clientSecret": secret},
	}
	var first, second connectReply
	c.do("POST", "/v1/settings/provider", body, http.StatusOK, &first)
	t.Logf("first connect: %s", actions(first))
	want := "role attest-admin: found, role attest-producer: found, role attest-agent: found, " +
		"client " + testClient + ": created, role grant attest-admin: skipped"
	if got := actions(first); got != want {
		t.Fatalf("first connect:\n got %s\nwant %s", got, want)
	}
	c.do("POST", "/v1/settings/provider", body, http.StatusOK, &second)
	t.Logf("second connect: %s", actions(second))
	if got := actions(second); got != strings.Replace(want, "created", "found", 1) {
		t.Fatalf("second connect changed something: %s", got)
	}
	client := kc.clientByID(testClient)
	if client["publicClient"] != true || client["attributes"].(map[string]any)["pkce.code.challenge.method"] != "S256" ||
		!reflect.DeepEqual(client["redirectUris"], []any{"http://localhost:8499/enroll/callback"}) {
		t.Fatalf("created client: %v", client)
	}

	c.do("POST", "/v1/setup/complete", nil, http.StatusOK, nil)

	// attest now verifies the realm's tokens: a realm token is a credential
	// (403 for lacking attest-admin, or 200), not garbage (401).
	resp, err := http.DefaultClient.Do(func() *http.Request {
		r, _ := http.NewRequest("GET", c.base+"/v1/settings", nil)
		r.Header.Set("Authorization", "Bearer "+kc.token)
		return r
	}())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusOK {
		t.Fatalf("a realm token after connecting: status %d", resp.StatusCode)
	}
	t.Logf("realm service token against /v1/settings: %d", resp.StatusCode)
}

// realKeycloak is just enough admin API to check and clean up.
type realKeycloak struct {
	t                  *testing.T
	base, realm, token string
}

func (k *realKeycloak) login(id, secret string) {
	k.t.Helper()
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {id}, "client_secret": {secret}}
	resp, err := http.PostForm(k.base+"/realms/"+k.realm+"/protocol/openid-connect/token", form)
	if err != nil {
		k.t.Fatal(err)
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil || tok.AccessToken == "" {
		k.t.Fatalf("admin login: status %d", resp.StatusCode)
	}
	k.token = tok.AccessToken
}

func (k *realKeycloak) call(method, path string, into any) int {
	k.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, method, k.base+"/admin/realms/"+k.realm+path, nil)
	req.Header.Set("Authorization", "Bearer "+k.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		k.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		k.t.Fatalf("%s %s: status %d", method, path, resp.StatusCode)
	}
	if into != nil && resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
			k.t.Fatal(err)
		}
	}
	return resp.StatusCode
}

func (k *realKeycloak) clientByID(clientID string) map[string]any {
	var found []map[string]any
	k.call("GET", "/clients?clientId="+url.QueryEscape(clientID), &found)
	for _, c := range found {
		if c["clientId"] == clientID {
			return c
		}
	}
	return nil
}

// untouchable is what the run must leave exactly as it was.
func (k *realKeycloak) untouchable() map[string]any {
	out := map[string]any{"attest-enroll": k.clientByID("attest-enroll")}
	for _, name := range []string{"attest-admin", "attest-producer", "attest-agent"} {
		var role map[string]any
		k.call("GET", "/roles/"+name, &role)
		out[name] = role
	}
	return out
}
