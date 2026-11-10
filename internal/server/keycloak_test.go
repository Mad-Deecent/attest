package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// keycloak is a fake of the slice of Keycloak's admin API the adapter uses:
// one realm, "home", with a person (ana) and a realm-admin service client.
type keycloak struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	roles    map[string]map[string]any
	clients  []map[string]any
	mappings map[string][]map[string]any
	// failOn makes the first request whose "METHOD path" starts with it
	// fail with a 500.
	failOn  string
	logouts int
	nextID  int
	// gate, when set, holds every token request until it is closed.
	gate    chan struct{}
	blocked int
}

// waiting is how many token requests the gate is holding.
func (kc *keycloak) waiting() int {
	kc.mu.Lock()
	defer kc.mu.Unlock()
	return kc.blocked
}

const (
	kcRealm       = "home"
	kcPassword    = "pw-SEKRIT-ana"
	kcSecret      = "svc-SEKRIT-secret"
	kcAnaID       = "user-ana"
	kcServiceUser = "svc"
)

// Admin tokens are JWTs whose sub is the user id, as Keycloak's are.
var (
	kcAnaToken = fakeJWT(kcAnaID)
	kcSvcToken = fakeJWT("service-account-" + kcServiceUser)
)

func fakeJWT(sub string) string {
	enc := func(v any) string {
		raw, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return enc(map[string]string{"alg": "none"}) + "." + enc(map[string]string{"sub": sub, "jti": "kc-ADMIN-" + sub}) + ".sig"
}

func newKeycloak(t *testing.T) *keycloak {
	t.Helper()
	kc := &keycloak{t: t, roles: map[string]map[string]any{}, mappings: map[string][]map[string]any{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /realms/{realm}/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": kc.srv.URL + "/realms/" + r.PathValue("realm")})
	})
	mux.HandleFunc("POST /realms/{realm}/protocol/openid-connect/token", kc.token)
	mux.HandleFunc("POST /realms/{realm}/protocol/openid-connect/logout", func(w http.ResponseWriter, _ *http.Request) {
		kc.mu.Lock()
		kc.logouts++
		kc.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/admin/realms/{realm}/", kc.admin)
	kc.srv = httptest.NewServer(mux)
	t.Cleanup(kc.srv.Close)
	return kc
}

func (kc *keycloak) token(w http.ResponseWriter, r *http.Request) {
	kc.mu.Lock()
	gate := kc.gate
	if gate != nil {
		kc.blocked++
	}
	kc.mu.Unlock()
	if gate != nil {
		<-gate
	}
	_ = r.ParseForm()
	f := r.PostForm
	var token string
	switch {
	case r.PathValue("realm") != kcRealm:
	case f.Get("grant_type") == "password" && f.Get("client_id") == "admin-cli" &&
		f.Get("username") == "ana" && f.Get("password") == kcPassword:
		token = kcAnaToken
	case f.Get("grant_type") == "client_credentials" && f.Get("client_id") == kcServiceUser && f.Get("client_secret") == kcSecret:
		token = kcSvcToken
	}
	if token == "" {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"Invalid user credentials"}`))
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": token, "refresh_token": "kc-refresh"})
}

func (kc *keycloak) admin(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	if (auth != "Bearer "+kcAnaToken && auth != "Bearer "+kcSvcToken) || r.PathValue("realm") != kcRealm {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	kc.mu.Lock()
	defer kc.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/admin/realms/"+kcRealm)
	if kc.failOn != "" && strings.HasPrefix(r.Method+" "+path, kc.failOn) {
		kc.failOn = ""
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"errorMessage":"disk full"}`))
		return
	}
	var body any
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	out := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case r.Method == "GET" && len(parts) == 2 && parts[0] == "roles":
		role, ok := kc.roles[parts[1]]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		out(role)
	case r.Method == "POST" && path == "/roles":
		m := body.(map[string]any)
		name := m["name"].(string)
		if _, dup := kc.roles[name]; dup {
			w.WriteHeader(http.StatusConflict)
			return
		}
		kc.nextID++
		kc.roles[name] = map[string]any{"id": "role-" + strconv.Itoa(kc.nextID), "name": name}
		w.WriteHeader(http.StatusCreated)
	case r.Method == "GET" && path == "/clients":
		found := []map[string]any{}
		for _, c := range kc.clients {
			if c["clientId"] == r.URL.Query().Get("clientId") {
				found = append(found, c)
			}
		}
		out(found)
	case r.Method == "POST" && path == "/clients":
		m := roundTrip(body)
		kc.nextID++
		m["id"] = "client-" + strconv.Itoa(kc.nextID)
		kc.clients = append(kc.clients, m)
		w.WriteHeader(http.StatusCreated)
	case r.Method == "PUT" && len(parts) == 2 && parts[0] == "clients":
		i := slices.IndexFunc(kc.clients, func(c map[string]any) bool { return c["id"] == parts[1] })
		if i < 0 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		kc.clients[i] = roundTrip(body)
		w.WriteHeader(http.StatusNoContent)
	case len(parts) == 4 && parts[0] == "users" && parts[1] != kcAnaID:
		w.WriteHeader(http.StatusNotFound)
	case len(parts) == 4 && parts[0] == "users" && parts[2] == "role-mappings" && parts[3] == "realm":
		if r.Method == "GET" {
			out(append([]map[string]any{}, kc.mappings[parts[1]]...))
			return
		}
		for _, role := range body.([]any) {
			kc.mappings[parts[1]] = append(kc.mappings[parts[1]], role.(map[string]any))
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		kc.t.Errorf("fake keycloak: unexpected %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusNotImplemented)
	}
}

func roundTrip(v any) map[string]any {
	raw, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return m
}

func (kc *keycloak) client(id string) map[string]any {
	kc.mu.Lock()
	defer kc.mu.Unlock()
	for _, c := range kc.clients {
		if c["clientId"] == id {
			return roundTrip(c)
		}
	}
	return nil
}

func (kc *keycloak) fail(prefix string) {
	kc.mu.Lock()
	defer kc.mu.Unlock()
	kc.failOn = prefix
}
