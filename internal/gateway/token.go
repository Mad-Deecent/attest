package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// tokenSource yields the bearer token for attest calls.
type tokenSource interface {
	Token(ctx context.Context) (string, error)
	// Invalidate drops a token attest rejected.
	Invalidate()
}

type staticToken string

func (s staticToken) Token(context.Context) (string, error) { return string(s), nil }
func (staticToken) Invalidate()                             {}

// clientCredentials runs the OAuth client-credentials grant against the
// issuer's discovered token endpoint, caching until shortly before expiry.
type clientCredentials struct {
	cfg  *OIDCConfig
	http *http.Client
	now  func() time.Time

	mu       sync.Mutex
	endpoint string
	token    string
	expires  time.Time
}

const expirySkew = 30 * time.Second

func (c *clientCredentials) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && c.now().Before(c.expires.Add(-expirySkew)) {
		return c.token, nil
	}
	if c.endpoint == "" {
		ep, err := c.discover(ctx)
		if err != nil {
			return "", err
		}
		c.endpoint = ep
	}
	tok, ttl, err := c.fetch(ctx)
	if err != nil {
		return "", err
	}
	c.token, c.expires = tok, c.now().Add(ttl)
	return tok, nil
}

func (c *clientCredentials) Invalidate() {
	c.mu.Lock()
	c.token = ""
	c.mu.Unlock()
}

func (c *clientCredentials) discover(ctx context.Context) (string, error) {
	u := strings.TrimSuffix(c.cfg.Issuer, "/") + "/.well-known/openid-configuration"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	var doc struct {
		TokenEndpoint string `json:"token_endpoint"`
	}
	if err := c.do(req, &doc); err != nil {
		return "", fmt.Errorf("discovery: %w", err)
	}
	if doc.TokenEndpoint == "" {
		return "", errors.New("discovery: no token_endpoint")
	}
	return doc.TokenEndpoint, nil
}

func (c *clientCredentials) fetch(ctx context.Context) (string, time.Duration, error) {
	secret, err := c.cfg.secret() // re-read so rotated secrets are picked up
	if err != nil {
		return "", 0, err
	}
	form := url.Values{"grant_type": {"client_credentials"}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// RFC 6749 2.3.1: credentials are form-encoded before basic auth.
	req.SetBasicAuth(url.QueryEscape(c.cfg.ClientID), url.QueryEscape(secret))
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := c.do(req, &out); err != nil {
		return "", 0, fmt.Errorf("token: %w", err)
	}
	if out.AccessToken == "" {
		return "", 0, errors.New("token: empty access_token")
	}
	ttl := time.Duration(out.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = time.Minute // no expiry given; refresh often rather than hold forever
	}
	return out.AccessToken, ttl, nil
}

func (c *clientCredentials) do(req *http.Request, into any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(into)
}
