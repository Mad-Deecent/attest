package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/mad-deecent/attest/internal/decision"
)

// attestClient asks attest whether the agent may act for a subject.
type attestClient struct {
	url    string
	tokens tokenSource
	http   *http.Client
}

// Decide returns an error for anything but a well-formed 200; callers deny.
func (a *attestClient) Decide(ctx context.Context, req decision.Request) (decision.Decision, error) {
	var d decision.Decision
	tok, err := a.tokens.Token(ctx)
	if err != nil {
		return d, fmt.Errorf("credential: %w", err)
	}
	body, _ := json.Marshal(req)
	hr, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(a.url, "/")+"/v1/decisions", bytes.NewReader(body))
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Authorization", "Bearer "+tok)
	resp, err := a.http.Do(hr)
	if err != nil {
		return d, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized {
			a.tokens.Invalidate()
		}
		return d, fmt.Errorf("attest status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&d); err != nil {
		return d, err
	}
	return d, nil
}
