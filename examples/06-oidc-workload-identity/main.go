// Example 06: model calls authenticated with OIDC workload identity
// federation (WIF) instead of a static API key.
//
// The workload's platform-issued OIDC token (Kubernetes projected token,
// AKS AZURE_FEDERATED_TOKEN_FILE, GitHub Actions id-token, ...) is exchanged at
// a security token service for a short-lived access token, which is cached
// and refreshed before expiry. The agentruntime package never sees a
// credential: it only talks to a Provider.
//
// With no configuration it runs a self-contained demo against local TLS
// fakes of the STS and model endpoint. Set WIF_TOKEN_URL to use real
// endpoints (see README.md in this directory).
//
// Run: go run ./examples/06-oidc-workload-identity
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/hacker1db/go-agent-runtime/agentruntime"
)

func main() {
	ctx := context.Background()

	var provider *ChatProvider
	var exchanges *atomic.Int32
	model := os.Getenv("MODEL_NAME")
	if os.Getenv("WIF_TOKEN_URL") != "" {
		var err error
		provider, err = fromEnv()
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println("mode: real endpoints from environment")
	} else {
		var cleanup func()
		provider, exchanges, cleanup = demo()
		defer cleanup()
		model = "demo-model"
		fmt.Println("mode: local demo (fake STS + model over TLS)")
	}

	reg := agentruntime.NewRegistry()
	reg.AddProvider("wif", provider)
	reg.AddModel(agentruntime.ModelDefinition{Name: "m", Provider: "wif", Model: model})
	reg.AddPrompt(agentruntime.PromptDefinition{Name: "assistant_p", Model: "m", System: "You are concise."})
	reg.AddAgent(agentruntime.AgentDefinition{
		Name:  "assistant",
		SideA: agentruntime.SideConfig{Prompt: "assistant_p"},
		SideB: agentruntime.SideConfig{Human: true},
	})

	rt := agentruntime.New(reg, agentruntime.NewMemoryStore(), agentruntime.Config{})
	if err := rt.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer rt.Close()

	th, err := rt.CreateThread(ctx, agentruntime.CreateThreadOptions{AgentID: "assistant"})
	if err != nil {
		log.Fatal(err)
	}
	for _, q := range []string{"hello", "how are you authenticated?"} {
		if err := rt.SendMessage(ctx, th.ThreadID(), agentruntime.MessageInput{Role: agentruntime.RoleUser, Content: q}); err != nil {
			log.Fatal(err)
		}
		wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := rt.WaitIdle(wctx, th.ThreadID())
		cancel()
		if err != nil {
			log.Fatal(err)
		}
	}

	res, err := th.GetMessages(ctx, agentruntime.GetMessagesOptions{Order: "asc"})
	if err != nil {
		log.Fatal(err)
	}
	for _, m := range res.Messages {
		fmt.Printf("%-9s %s\n", m.Role+":", m.ContentString())
	}
	st, _ := th.Status(ctx)
	fmt.Printf("status: %s\n", st.State)
	if exchanges != nil {
		fmt.Printf("token exchanges for 2 model calls: %d (cached until near expiry)\n", exchanges.Load())
	}
}

// fromEnv builds the real configuration. Nothing here is a secret: the only
// credential is the platform-issued OIDC token, read at exchange time.
//
//	WIF_TOKEN_URL           STS / token endpoint (HTTPS)
//	WIF_STYLE               rfc8693 (default) | client-assertion
//	WIF_AUDIENCE            RFC 8693 audience, e.g. GCP workload identity provider
//	WIF_CLIENT_ID           Entra application ID (client-assertion style)
//	WIF_SCOPE               requested scope
//	WIF_SUBJECT_TOKEN_FILE  projected OIDC token file; if unset and running in
//	                        GitHub Actions, the Actions OIDC token is used
//	WIF_OIDC_AUDIENCE       audience to request from GitHub Actions OIDC
//	MODEL_BASE_URL          OpenAI-compatible base URL (HTTPS)
//	MODEL_NAME              model identifier
func fromEnv() (*ChatProvider, error) {
	var subject SubjectTokenSource
	switch {
	case os.Getenv("WIF_SUBJECT_TOKEN_FILE") != "":
		subject = FileSubjectToken{Path: os.Getenv("WIF_SUBJECT_TOKEN_FILE")}
	case os.Getenv("AZURE_FEDERATED_TOKEN_FILE") != "":
		subject = FileSubjectToken{Path: os.Getenv("AZURE_FEDERATED_TOKEN_FILE")}
	case os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL") != "":
		subject = GitHubActionsSubjectToken{Audience: os.Getenv("WIF_OIDC_AUDIENCE")}
	default:
		return nil, fmt.Errorf("no OIDC subject token: set WIF_SUBJECT_TOKEN_FILE or run in GitHub Actions with id-token: write")
	}
	style := ExchangeStyle(os.Getenv("WIF_STYLE"))
	if style == "" {
		style = StyleRFC8693
	}
	base := os.Getenv("MODEL_BASE_URL")
	if base == "" || os.Getenv("MODEL_NAME") == "" {
		return nil, fmt.Errorf("MODEL_BASE_URL and MODEL_NAME are required")
	}
	tokens := &FederatedTokenSource{
		Style:    style,
		TokenURL: os.Getenv("WIF_TOKEN_URL"),
		Subject:  subject,
		Audience: os.Getenv("WIF_AUDIENCE"),
		ClientID: os.Getenv("WIF_CLIENT_ID"),
		Scope:    os.Getenv("WIF_SCOPE"),
	}
	return &ChatProvider{BaseURL: base, Tokens: tokens}, nil
}

// demo starts local TLS fakes of an STS and a model endpoint and writes a
// fake projected OIDC token to a temp file.
func demo() (*ChatProvider, *atomic.Int32, func()) {
	const subjectJWT = "eyJhbGciOiJub25lIn0.eyJzdWIiOiJkZW1vLXdvcmtsb2FkIn0." // demo only, unsigned
	var issued atomic.Value
	var exchanges atomic.Int32

	sts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil ||
			r.PostForm.Get("grant_type") != "urn:ietf:params:oauth:grant-type:token-exchange" ||
			r.PostForm.Get("subject_token") != subjectJWT {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		n := exchanges.Add(1)
		tok := fmt.Sprintf("short-lived-access-token-%d", n)
		issued.Store(tok)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": tok, "token_type": "Bearer", "expires_in": 3600})
	}))

	modelSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want, _ := issued.Load().(string)
		if want == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+want)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var in struct {
			Messages []chatMessage `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		last := in.Messages[len(in.Messages)-1].Content
		reply := fmt.Sprintf("You said %q. I was called with a federated token, not an API key.", last)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "demo-model",
			"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": reply}}},
		})
	}))

	dir, err := os.MkdirTemp("", "wif-demo-*")
	if err != nil {
		log.Fatal(err)
	}
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte(subjectJWT), 0o600); err != nil {
		log.Fatal(err)
	}

	// Both fakes share one test CA; trust only it.
	httpClient := sts.Client()
	tokens := &FederatedTokenSource{
		Style:    StyleRFC8693,
		TokenURL: sts.URL + "/v1/token",
		Subject:  FileSubjectToken{Path: tokenFile},
		Audience: "//iam.googleapis.com/projects/0/locations/global/workloadIdentityPools/demo/providers/demo",
		Client:   httpClient,
	}
	provider := &ChatProvider{BaseURL: modelSrv.URL + "/v1", Tokens: tokens, Client: modelSrv.Client()}
	return provider, &exchanges, func() {
		sts.Close()
		modelSrv.Close()
		_ = os.RemoveAll(dir)
	}
}
