package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// Keycloak makes sure a realm has attest's roles and a public enrollment
// client, and grants the admin role to the person who connected it. Every
// step looks before it writes, so running it twice changes nothing the
// second time, and it only ever adds to an existing client.
type Keycloak struct{ Client *http.Client }

const defaultEnrollClient = "attest-enroll"

func (k Keycloak) Connect(ctx context.Context, req Request) (Result, error) {
	base := strings.TrimRight(req.URL, "/")
	if u, err := url.Parse(base); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Result{}, inputf("url must be the absolute http(s) URL of the Keycloak server")
	}
	if req.Realm == "" {
		return Result{}, inputf("realm is required")
	}
	cred := req.Credential
	if cred.user() == cred.service() {
		return Result{}, inputf("give either username and password, or clientId and clientSecret")
	}
	public, err := url.Parse(strings.TrimRight(req.PublicURL, "/"))
	if err != nil || public.Host == "" {
		return Result{}, inputf("attest's public URL must be set first")
	}
	if len(req.Roles) == 0 {
		return Result{}, inputf("no roles to ensure")
	}
	clientID := req.ClientID
	if clientID == "" {
		clientID = defaultEnrollClient
	}
	adminRealm := req.AdminRealm
	if adminRealm == "" {
		adminRealm = req.Realm
	}

	issuer, err := Discover(ctx, k.Client, base+"/realms/"+url.PathEscape(req.Realm))
	if err != nil {
		return Result{}, err
	}
	a := &kcAdmin{client: noRedirects(k.Client), base: base, realm: url.PathEscape(req.Realm)}
	logout, err := a.login(ctx, url.PathEscape(adminRealm), cred)
	if err != nil {
		return Result{}, err
	}
	defer logout()

	res := Result{Issuer: issuer, ClientID: clientID}
	roles := map[string]map[string]any{}
	for _, name := range req.Roles {
		role, change, err := a.ensureRole(ctx, name)
		if err != nil {
			return res, err
		}
		roles[name] = role
		res.Changes = append(res.Changes, change)
	}
	change, err := a.ensureClient(ctx, clientID, public)
	if err != nil {
		return res, err
	}
	res.Changes = append(res.Changes, change)

	adminRole := req.Roles[0]
	grant := Change{Object: "role grant", Name: adminRole, Action: "skipped"}
	switch {
	case cred.service():
		grant.Note = "connected with a service client: grant " + adminRole + " to a person in Keycloak"
	case adminRealm != req.Realm:
		grant.Note = "the admin who connected lives in realm " + adminRealm + ": grant " + adminRole + " to a person in " + req.Realm
	default:
		grant, err = a.grant(ctx, roles[adminRole])
		if err != nil {
			return res, err
		}
	}
	res.Changes = append(res.Changes, grant)
	return res, nil
}

type kcAdmin struct {
	client *http.Client
	base   string
	realm  string
	token  string
	// subject is the admin token's sub: for a person, their user id in the
	// realm the token came from.
	subject string
}

// tokenSubject reads sub from a JWT without checking its signature. Only for
// a token that came straight from the token endpoint over the back channel.
func tokenSubject(raw string) string {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	_ = json.Unmarshal(payload, &claims)
	return claims.Sub
}

// login exchanges the credential for a short-lived admin token. The returned
// func ends the session the token belongs to, where there is one.
func (a *kcAdmin) login(ctx context.Context, realm string, cred Credential) (logout func(), err error) {
	form := url.Values{}
	if cred.user() {
		form.Set("grant_type", "password")
		form.Set("client_id", "admin-cli")
		form.Set("username", cred.Username)
		form.Set("password", cred.Password)
	} else {
		form.Set("grant_type", "client_credentials")
		form.Set("client_id", cred.ClientID)
		form.Set("client_secret", cred.ClientSecret)
	}
	tokenURL := a.base + "/realms/" + realm + "/protocol/openid-connect/token"
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	status, body, err := a.send(ctx, http.MethodPost, tokenURL, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()), false)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("keycloak refused the admin credential (%d%s)", status, kcReason(body))
	}
	if err := json.Unmarshal(body, &tok); err != nil || tok.AccessToken == "" {
		return nil, fmt.Errorf("keycloak token endpoint returned no access token")
	}
	a.token = tok.AccessToken
	a.subject = tokenSubject(tok.AccessToken)
	return func() {
		a.token = ""
		if tok.RefreshToken == "" {
			return
		}
		f := url.Values{"refresh_token": {tok.RefreshToken}, "client_id": {form.Get("client_id")}}
		if cred.service() {
			f.Set("client_secret", cred.ClientSecret)
		}
		// Best effort: the token expires on its own regardless.
		_, _, _ = a.send(context.WithoutCancel(ctx), http.MethodPost,
			a.base+"/realms/"+realm+"/protocol/openid-connect/logout",
			"application/x-www-form-urlencoded", strings.NewReader(f.Encode()), false)
	}, nil
}

// send makes one request and reads the reply. Errors never carry request
// bodies, which may hold the credential.
func (a *kcAdmin) send(ctx context.Context, method, target, contentType string, body io.Reader, auth bool) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return 0, nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if auth {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	resp, err := a.client.Do(req)
	u, _ := url.Parse(target)
	if err != nil {
		return 0, nil, fmt.Errorf("keycloak at %s: unreachable", u.Host)
	}
	defer resp.Body.Close()
	if redirected(resp.StatusCode) {
		return 0, nil, fmt.Errorf("keycloak at %s answered with a redirect (%d), which attest does not follow with an admin credential; check the URL", u.Host, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, raw, err
}

// admin calls the admin REST API for the realm. A 404 is returned as a status,
// not an error; anything else outside 2xx is an error.
func (a *kcAdmin) admin(ctx context.Context, method, path string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(raw)
	}
	status, raw, err := a.send(ctx, method, a.base+"/admin/realms/"+a.realm+path, "application/json", body, true)
	if err != nil {
		return 0, err
	}
	switch {
	case status == http.StatusNotFound:
		return status, nil
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return status, fmt.Errorf("keycloak: the admin credential may not %s %s (%d); it needs realm-admin on the realm", method, path, status)
	case status < 200 || status > 299:
		return status, fmt.Errorf("keycloak: %s %s: %d%s", method, path, status, kcReason(raw))
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return status, fmt.Errorf("keycloak: %s %s: unexpected reply", method, path)
		}
	}
	return status, nil
}

func kcReason(raw []byte) string {
	var e struct {
		Error        string `json:"error"`
		Description  string `json:"error_description"`
		ErrorMessage string `json:"errorMessage"`
	}
	_ = json.Unmarshal(raw, &e)
	for _, s := range []string{e.Description, e.ErrorMessage, e.Error} {
		if s != "" {
			return ": " + s
		}
	}
	return ""
}

func (a *kcAdmin) ensureRole(ctx context.Context, name string) (map[string]any, Change, error) {
	change := Change{Object: "role", Name: name, Action: "found"}
	path := "/roles/" + url.PathEscape(name)
	var role map[string]any
	status, err := a.admin(ctx, http.MethodGet, path, nil, &role)
	if err != nil {
		return nil, change, err
	}
	if status == http.StatusNotFound {
		if _, err := a.admin(ctx, http.MethodPost, "/roles", map[string]any{
			"name": name, "description": "Created by attest",
		}, nil); err != nil {
			return nil, change, err
		}
		change.Action = "created"
		if _, err := a.admin(ctx, http.MethodGet, path, nil, &role); err != nil {
			return nil, change, err
		}
	}
	return role, change, nil
}

// ensureClient makes sure a public client with PKCE can send people back to
// attest. An existing client is only ever added to: a missing redirect URI or
// web origin, PKCE, the standard flow. Nothing is removed.
func (a *kcAdmin) ensureClient(ctx context.Context, clientID string, public *url.URL) (Change, error) {
	change := Change{Object: "client", Name: clientID, Action: "found"}
	redirect := public.String() + "/enroll/callback"
	origin := public.Scheme + "://" + public.Host
	var found []map[string]any
	if _, err := a.admin(ctx, http.MethodGet, "/clients?clientId="+url.QueryEscape(clientID), nil, &found); err != nil {
		return change, err
	}
	if len(found) == 0 {
		_, err := a.admin(ctx, http.MethodPost, "/clients", map[string]any{
			"clientId":                  clientID,
			"name":                      "attest enrollment",
			"description":               "Created by attest. People sign in here to enroll.",
			"enabled":                   true,
			"protocol":                  "openid-connect",
			"publicClient":              true,
			"standardFlowEnabled":       true,
			"implicitFlowEnabled":       false,
			"directAccessGrantsEnabled": false,
			"serviceAccountsEnabled":    false,
			"redirectUris":              []string{redirect},
			"webOrigins":                []string{origin},
			"attributes":                map[string]string{"pkce.code.challenge.method": "S256"},
		}, nil)
		if err == nil {
			change.Action = "created"
		}
		return change, err
	}
	c := found[0]
	if isPublic, _ := c["publicClient"].(bool); !isPublic {
		return change, fmt.Errorf("%w: client %s already exists and is confidential; choose another client id", ErrInput, clientID)
	}
	var notes []string
	if add(c, "redirectUris", redirect) {
		notes = append(notes, "added redirect URI "+redirect)
	}
	if add(c, "webOrigins", origin) {
		notes = append(notes, "added web origin "+origin)
	}
	attrs, _ := c["attributes"].(map[string]any)
	if attrs == nil {
		attrs = map[string]any{}
		c["attributes"] = attrs
	}
	if attrs["pkce.code.challenge.method"] != "S256" {
		attrs["pkce.code.challenge.method"] = "S256"
		notes = append(notes, "required PKCE (S256)")
	}
	if on, _ := c["standardFlowEnabled"].(bool); !on {
		c["standardFlowEnabled"] = true
		notes = append(notes, "enabled the standard flow")
	}
	if len(notes) == 0 {
		return change, nil
	}
	id, _ := c["id"].(string)
	if _, err := a.admin(ctx, http.MethodPut, "/clients/"+url.PathEscape(id), c, nil); err != nil {
		return change, err
	}
	change.Action, change.Note = "updated", strings.Join(notes, "; ")
	return change, nil
}

// add appends v to the string list at c[key] unless it is there, and says
// whether it did.
func add(c map[string]any, key, v string) bool {
	list, _ := c[key].([]any)
	if slices.Contains(list, any(v)) {
		return false
	}
	c[key] = append(list, v)
	return true
}

// grant gives the admin role to the person the admin token belongs to,
// identified by the token's subject rather than a name lookup that could
// match someone else.
func (a *kcAdmin) grant(ctx context.Context, role map[string]any) (Change, error) {
	name, _ := role["name"].(string)
	change := Change{Object: "role grant", Name: name, Action: "found"}
	if a.subject == "" {
		change.Action, change.Note = "skipped", "the admin token names no user: grant "+name+" to a person in Keycloak"
		return change, nil
	}
	change.Note = "to the admin who connected"
	path := "/users/" + url.PathEscape(a.subject) + "/role-mappings/realm"
	var held []map[string]any
	status, err := a.admin(ctx, http.MethodGet, path, nil, &held)
	if err != nil {
		return change, err
	}
	if status == http.StatusNotFound {
		change.Action, change.Note = "skipped", "the admin who connected is not a user of this realm"
		return change, nil
	}
	if slices.ContainsFunc(held, func(r map[string]any) bool { return r["name"] == name }) {
		return change, nil
	}
	if _, err := a.admin(ctx, http.MethodPost, path, []map[string]any{{"id": role["id"], "name": name}}, nil); err != nil {
		return change, err
	}
	change.Action = "created"
	return change, nil
}
