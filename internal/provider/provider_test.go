package provider

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// A redirect from the provider must not carry the admin credential, or any
// request, to where it points.
func TestCredentialDoesNotFollowRedirects(t *testing.T) {
	var mu sync.Mutex
	var reached []string
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		reached = append(reached, r.Method+" "+r.URL.Path+" "+string(body))
		mu.Unlock()
	}))
	defer elsewhere.Close()
	var kc *httptest.Server
	kc = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/realms/") && strings.HasSuffix(r.URL.Path, "/.well-known/openid-configuration") {
			_, _ = w.Write([]byte(`{"issuer":"` + kc.URL + `/realms/r"}`))
			return
		}
		http.Redirect(w, r, elsewhere.URL+"/token?next=SEKRIT-location", http.StatusTemporaryRedirect)
	}))
	defer kc.Close()

	_, err := Keycloak{Client: Client}.Connect(t.Context(), Request{
		URL: kc.URL, Realm: "r", Credential: Credential{Username: "u", Password: "pw-SEKRIT"},
		PublicURL: "http://attest.home", Roles: []string{"a", "b", "c"},
	})
	if err == nil || !strings.Contains(err.Error(), "redirect (307)") || strings.Contains(err.Error(), "SEKRIT") {
		t.Fatalf("want a redirect error naming the status only, got %v", err)
	}
	// Discovery stops at a redirect too.
	if _, err := Discover(t.Context(), Client, kc.URL+"/moved"); err == nil || !strings.Contains(err.Error(), "redirect (307)") {
		t.Fatalf("discovery followed a redirect: %v", err)
	}
	if len(reached) != 0 {
		t.Fatalf("the redirect target was reached: %q", reached)
	}
}

func TestDiscoveryBoundsTheBody(t *testing.T) {
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"pad":"` + strings.Repeat("x", 2<<20) + `","issuer":"https://x"}`))
	}))
	defer big.Close()
	if _, err := Discover(t.Context(), Client, big.URL); err == nil {
		t.Fatal("read an unbounded discovery document")
	}
}
