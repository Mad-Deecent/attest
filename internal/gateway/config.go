// Package gateway is an MCP proxy that asks attest before forwarding tool calls.
package gateway

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

type Config struct {
	Listen   string          `json:"listen"`
	Upstream string          `json:"upstream"`
	Attest   AttestConfig    `json:"attest"`
	Tools    map[string]Tool `json:"tools"`
}

// Tool maps an MCP tool to the attest permission a caller needs for it.
type Tool struct {
	Permission string `json:"permission"`
	Resource   string `json:"resource,omitempty"`
}

type AttestConfig struct {
	URL string `json:"url"`
	// Token is a static agent token, for development.
	Token string      `json:"token,omitempty"`
	OIDC  *OIDCConfig `json:"oidc,omitempty"`
}

type OIDCConfig struct {
	Issuer           string `json:"issuer"`
	ClientID         string `json:"clientId"`
	ClientSecretFile string `json:"clientSecretFile,omitempty"`
	ClientSecretEnv  string `json:"clientSecretEnv,omitempty"`
}

func (c *Config) validate() error {
	if c.Listen == "" {
		c.Listen = ":8430"
	}
	if err := absURL("upstream", c.Upstream); err != nil {
		return err
	}
	if err := absURL("attest.url", c.Attest.URL); err != nil {
		return err
	}
	if (c.Attest.Token == "") == (c.Attest.OIDC == nil) {
		return errors.New("set exactly one of attest.token and attest.oidc")
	}
	if o := c.Attest.OIDC; o != nil {
		if o.Issuer == "" || o.ClientID == "" {
			return errors.New("attest.oidc needs issuer and clientId")
		}
		if (o.ClientSecretFile == "") == (o.ClientSecretEnv == "") {
			return errors.New("set exactly one of attest.oidc.clientSecretFile and clientSecretEnv")
		}
	}
	for name, t := range c.Tools {
		if t.Permission == "" {
			return fmt.Errorf("tool %q has no permission", name)
		}
	}
	return nil
}

func absURL(field, v string) error {
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%s must be an http(s) URL", field)
	}
	return nil
}

func (o *OIDCConfig) secret() (string, error) {
	if o.ClientSecretEnv != "" {
		s := os.Getenv(o.ClientSecretEnv)
		if s == "" {
			return "", fmt.Errorf("$%s is empty", o.ClientSecretEnv)
		}
		return s, nil
	}
	raw, err := os.ReadFile(o.ClientSecretFile)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return "", errors.New("client secret file is empty")
	}
	return s, nil
}
