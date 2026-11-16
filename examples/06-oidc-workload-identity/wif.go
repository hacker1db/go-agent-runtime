package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// SubjectTokenSource yields the workload's OIDC identity token (a JWT issued
// by the platform: Kubernetes, GitHub Actions, a cloud VM, ...). It is fetched
// fresh for every exchange because platforms rotate it.
type SubjectTokenSource interface {
	SubjectToken(ctx context.Context) (string, error)
}

// FileSubjectToken reads a projected OIDC token from disk, e.g.
// AZURE_FEDERATED_TOKEN_FILE on AKS or a Kubernetes projected
// service-account token volume.
type FileSubjectToken struct{ Path string }

// SubjectToken returns the current contents of the token file.
func (f FileSubjectToken) SubjectToken(context.Context) (string, error) {
	b, err := os.ReadFile(f.Path)
	if err != nil {
		return "", fmt.Errorf("read subject token: %w", err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", errors.New("subject token file is empty")
	}
	return tok, nil
}

// GitHubActionsSubjectToken requests an OIDC token from the GitHub Actions
// runtime. The job needs `permissions: id-token: write`; no stored secret is
// involved.
type GitHubActionsSubjectToken struct {
	Audience string
	Client   *http.Client
}

// SubjectToken requests a token for Audience from the Actions OIDC endpoint.
func (g GitHubActionsSubjectToken) SubjectToken(ctx context.Context) (string, error) {
	reqURL, reqTok := os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL"), os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN")
	if reqURL == "" || reqTok == "" {
		return "", errors.New("GitHub Actions OIDC unavailable: grant the job `id-token: write`")
	}
	u, err := url.Parse(reqURL)
	if err != nil {
		return "", fmt.Errorf("parse ACTIONS_ID_TOKEN_REQUEST_URL: %w", err)
	}
	if g.Audience != "" {
		q := u.Query()
		q.Set("audience", g.Audience)
		u.RawQuery = q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+reqTok)
	var out struct {
		Value string `json:"value"`
	}
	if err := doJSON(client(g.Client), req, &out); err != nil {
		return "", fmt.Errorf("github actions oidc: %w", err)
	}
	if out.Value == "" {
		return "", errors.New("github actions oidc: empty token")
	}
	return out.Value, nil
}

// ExchangeStyle selects the federation protocol of the token endpoint.
type ExchangeStyle string

const (
	// StyleRFC8693 is OAuth 2.0 Token Exchange, used by Google Cloud STS
	// (Workload Identity Federation) and many other STSes.
	StyleRFC8693 ExchangeStyle = "rfc8693"
	// StyleClientAssertion is the client-credentials grant with the OIDC token
	// as a JWT client assertion, used by Microsoft Entra workload identity
	// federation.
	StyleClientAssertion ExchangeStyle = "client-assertion"
)

// FederatedTokenSource exchanges a workload OIDC token for a short-lived
// access token and caches it until shortly before expiry. Nothing long-lived
// is stored anywhere: no API key, no PAT, no client secret.
type FederatedTokenSource struct {
	Style    ExchangeStyle
	TokenURL string
	Subject  SubjectTokenSource
	// Audience is the RFC 8693 `audience` (e.g. the GCP workload identity
	// provider resource name). Unused for StyleClientAssertion.
	Audience string
	// ClientID is the Entra application (client) ID. Unused for StyleRFC8693.
	ClientID string
	Scope    string
	Client   *http.Client
	// Skew refreshes the token this long before it expires (default 2m).
	Skew time.Duration

	mu      sync.Mutex
	token   string
	expires time.Time
	now     func() time.Time // test hook
}

// Token returns a valid access token, exchanging a fresh subject token when
// the cached one is missing or about to expire.
func (f *FederatedTokenSource) Token(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now
	if f.now != nil {
		now = f.now
	}
	skew := f.Skew
	if skew == 0 {
		skew = 2 * time.Minute
	}
	if f.token != "" && now().Add(skew).Before(f.expires) {
		return f.token, nil
	}
	if err := requireSecureURL(f.TokenURL); err != nil {
		return "", err
	}
	subject, err := f.Subject.SubjectToken(ctx)
	if err != nil {
		return "", err
	}
	form := url.Values{}
	switch f.Style {
	case StyleRFC8693:
		form.Set("grant_type", "urn:ietf:params:oauth:grant-type:token-exchange")
		form.Set("subject_token_type", "urn:ietf:params:oauth:token-type:jwt")
		form.Set("requested_token_type", "urn:ietf:params:oauth:token-type:access_token")
		form.Set("subject_token", subject)
		if f.Audience != "" {
			form.Set("audience", f.Audience)
		}
	case StyleClientAssertion:
		if f.ClientID == "" {
			return "", errors.New("client-assertion exchange needs ClientID")
		}
		form.Set("grant_type", "client_credentials")
		form.Set("client_id", f.ClientID)
		form.Set("client_assertion_type", "urn:ietf:params:oauth:client-assertion-type:jwt-bearer")
		form.Set("client_assertion", subject)
	default:
		return "", fmt.Errorf("unknown exchange style %q", f.Style)
	}
	if f.Scope != "" {
		form.Set("scope", f.Scope)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := doJSON(client(f.Client), req, &out); err != nil {
		return "", fmt.Errorf("token exchange: %w", err)
	}
	if out.AccessToken == "" || out.ExpiresIn <= 0 {
		return "", errors.New("token exchange: response missing access_token or expires_in")
	}
	f.token = out.AccessToken
	f.expires = now().Add(time.Duration(out.ExpiresIn) * time.Second)
	return f.token, nil
}

// requireSecureURL rejects plaintext token endpoints except on loopback, so a
// misconfiguration cannot send identity tokens over the network unencrypted.
func requireSecureURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL %q: %w", raw, err)
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" {
		if ip := net.ParseIP(u.Hostname()); (ip != nil && ip.IsLoopback()) || u.Hostname() == "localhost" {
			return nil
		}
	}
	return fmt.Errorf("refusing non-HTTPS endpoint %q", u.Redacted())
}

func client(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// maxBody caps how much of any response is read.
const maxBody = 1 << 20

// doJSON performs req and decodes a JSON body. Error bodies are not echoed:
// token endpoints can reflect credentials back in error payloads.
func doJSON(c *http.Client, req *http.Request, out any) error {
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body := io.LimitReader(resp.Body, maxBody)
	if resp.StatusCode/100 != 2 {
		_, _ = io.Copy(io.Discard, body)
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, req.URL.Redacted())
	}
	return json.NewDecoder(body).Decode(out)
}
