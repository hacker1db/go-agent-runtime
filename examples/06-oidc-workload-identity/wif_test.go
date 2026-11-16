package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type staticSubject string

func (s staticSubject) SubjectToken(context.Context) (string, error) { return string(s), nil }

func stsServer(t *testing.T, check func(r *http.Request), expiresIn int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		check(r)
		n.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "expires_in": expiresIn})
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func TestRFC8693ExchangeAndCache(t *testing.T) {
	srv, n := stsServer(t, func(r *http.Request) {
		f := r.PostForm
		if f.Get("grant_type") != "urn:ietf:params:oauth:grant-type:token-exchange" ||
			f.Get("subject_token_type") != "urn:ietf:params:oauth:token-type:jwt" ||
			f.Get("subject_token") != "jwt" || f.Get("audience") != "aud" {
			t.Errorf("unexpected form: %v", f)
		}
	}, 3600)
	ts := &FederatedTokenSource{Style: StyleRFC8693, TokenURL: srv.URL, Subject: staticSubject("jwt"), Audience: "aud", Client: srv.Client()}
	for range 3 {
		tok, err := ts.Token(context.Background())
		if err != nil || tok != "at" {
			t.Fatalf("Token() = %q, %v", tok, err)
		}
	}
	if got := n.Load(); got != 1 {
		t.Fatalf("exchanges = %d, want 1 (cached)", got)
	}
}

func TestRefreshesBeforeExpiry(t *testing.T) {
	srv, n := stsServer(t, func(*http.Request) {}, 300)
	now := time.Unix(0, 0)
	ts := &FederatedTokenSource{Style: StyleRFC8693, TokenURL: srv.URL, Subject: staticSubject("jwt"), Client: srv.Client(), now: func() time.Time { return now }}
	if _, err := ts.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(299 * time.Second) // inside the default 2m skew
	if _, err := ts.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := n.Load(); got != 2 {
		t.Fatalf("exchanges = %d, want 2 (refresh near expiry)", got)
	}
}

func TestClientAssertionStyle(t *testing.T) {
	srv, _ := stsServer(t, func(r *http.Request) {
		f := r.PostForm
		if f.Get("grant_type") != "client_credentials" || f.Get("client_id") != "app" ||
			f.Get("client_assertion_type") != "urn:ietf:params:oauth:client-assertion-type:jwt-bearer" ||
			f.Get("client_assertion") != "jwt" || f.Get("client_secret") != "" {
			t.Errorf("unexpected form: %v", f)
		}
	}, 3600)
	ts := &FederatedTokenSource{Style: StyleClientAssertion, TokenURL: srv.URL, Subject: staticSubject("jwt"), ClientID: "app", Client: srv.Client()}
	if _, err := ts.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRejectsPlaintextRemoteEndpoint(t *testing.T) {
	ts := &FederatedTokenSource{Style: StyleRFC8693, TokenURL: "http://sts.example.com/token", Subject: staticSubject("jwt")}
	if _, err := ts.Token(context.Background()); err == nil || !strings.Contains(err.Error(), "non-HTTPS") {
		t.Fatalf("err = %v, want non-HTTPS refusal", err)
	}
	for _, ok := range []string{"https://sts.example.com", "http://127.0.0.1:8080", "http://localhost:1"} {
		if err := requireSecureURL(ok); err != nil {
			t.Errorf("requireSecureURL(%q) = %v", ok, err)
		}
	}
}

func TestErrorBodyNotEchoed(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "subject_token=leaked-secret", http.StatusBadRequest)
	}))
	defer srv.Close()
	ts := &FederatedTokenSource{Style: StyleRFC8693, TokenURL: srv.URL, Subject: staticSubject("jwt"), Client: srv.Client()}
	_, err := ts.Token(context.Background())
	if err == nil || strings.Contains(err.Error(), "leaked-secret") {
		t.Fatalf("err = %v; want error without response body", err)
	}
}

func TestFileSubjectTokenRereadsRotatedFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "token")
	src := FileSubjectToken{Path: p}
	for _, want := range []string{"one", "two"} {
		if err := os.WriteFile(p, []byte(want+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := src.SubjectToken(context.Background())
		if err != nil || got != want {
			t.Fatalf("SubjectToken() = %q, %v; want %q", got, err, want)
		}
	}
}

func TestGitHubActionsSubjectTokenRequiresPermission(t *testing.T) {
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "")
	if _, err := (GitHubActionsSubjectToken{}).SubjectToken(context.Background()); err == nil || !strings.Contains(err.Error(), "id-token: write") {
		t.Fatalf("err = %v", err)
	}
}
