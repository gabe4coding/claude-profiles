package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Token + client-registration caches live under the unified root. Defined as
// functions so CLAUDE_PROFILES_ROOT changes during tests take effect.
func tokenDir() string  { return tokensDirPath() }
func clientDir() string { return clientsDirPath() }

func cacheKey(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h[:10])
}

// ── Token cache ───────────────────────────────────────────────────────────────

type tokenData struct {
	AccessToken   string  `json:"access_token"`
	ExpiresAt     float64 `json:"expires_at"`
	RefreshToken  string  `json:"refresh_token,omitempty"`
	TokenEndpoint string  `json:"token_endpoint,omitempty"`
	ClientID      string  `json:"client_id,omitempty"`
}

func tokenPath(serverURL string) string {
	return filepath.Join(tokenDir(), cacheKey(serverURL)+".json")
}

func loadToken(serverURL string) string {
	data, err := os.ReadFile(tokenPath(serverURL))
	if err != nil {
		return ""
	}
	var td tokenData
	if err := json.Unmarshal(data, &td); err != nil {
		return ""
	}
	if td.ExpiresAt > float64(time.Now().Unix())+60 {
		return td.AccessToken
	}
	if td.RefreshToken != "" && td.TokenEndpoint != "" {
		if tok := refreshToken(td.TokenEndpoint, td.RefreshToken, td.ClientID, serverURL); tok != "" {
			return tok
		}
	}
	return ""
}

func saveToken(serverURL string, td tokenData) {
	os.MkdirAll(tokenDir(), 0o700)
	data, _ := json.MarshalIndent(td, "", "  ")
	os.WriteFile(tokenPath(serverURL), data, 0o600)
}

func clearToken(serverURL string) {
	os.Remove(tokenPath(serverURL))
}

func refreshToken(endpoint, rt, clientID, serverURL string) string {
	vals := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}}
	if clientID != "" {
		vals.Set("client_id", clientID)
	}
	resp, err := http.PostForm(endpoint, vals)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var td tokenData
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &td); err != nil || td.AccessToken == "" {
		return ""
	}
	if td.ExpiresAt == 0 {
		td.ExpiresAt = float64(time.Now().Unix()) + 3600
	}
	td.RefreshToken = rt
	td.TokenEndpoint = endpoint
	td.ClientID = clientID
	saveToken(serverURL, td)
	return td.AccessToken
}

// ── Dynamic client registration ───────────────────────────────────────────────

func clientPath(endpoint string) string {
	return filepath.Join(clientDir(), cacheKey(endpoint)+".json")
}

func getOrRegisterClient(registrationEndpoint, redirectURI string) (string, error) {
	path := clientPath(registrationEndpoint)
	if data, err := os.ReadFile(path); err == nil {
		var m map[string]string
		if json.Unmarshal(data, &m) == nil && m["client_id"] != "" {
			return m["client_id"], nil
		}
	}

	payload, _ := json.Marshal(map[string]any{
		"client_name":                "claude-profiles-mcp-probe",
		"redirect_uris":              []string{redirectURI},
		"grant_types":                []string{"authorization_code"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
	resp, err := http.Post(registrationEndpoint, "application/json", strings.NewReader(string(payload)))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var m map[string]any
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &m); err != nil {
		return "", fmt.Errorf("registration response invalid JSON")
	}
	clientID, _ := m["client_id"].(string)
	if clientID == "" {
		return "", fmt.Errorf("no client_id in registration response")
	}
	os.MkdirAll(clientDir(), 0o700)
	data, _ := json.Marshal(map[string]string{"client_id": clientID})
	os.WriteFile(path, data, 0o600)
	return clientID, nil
}

// ── OAuth discovery ───────────────────────────────────────────────────────────

type oauthMeta struct {
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	RegistrationEndpoint  string `json:"registration_endpoint"`
}

func discoverOAuth(resourceMetaURL string) (*oauthMeta, error) {
	body, err := httpGet(resourceMetaURL)
	if err != nil {
		return nil, err
	}
	var rm struct {
		AuthorizationServers []string `json:"authorization_servers"`
	}
	if err := json.Unmarshal(body, &rm); err != nil || len(rm.AuthorizationServers) == 0 {
		return nil, fmt.Errorf("could not parse resource metadata")
	}
	asURL := strings.TrimRight(rm.AuthorizationServers[0], "/") + "/.well-known/oauth-authorization-server"
	body, err = httpGet(asURL)
	if err != nil {
		return nil, err
	}
	var meta oauthMeta
	if err := json.Unmarshal(body, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

// discoverOAuthMetadata locates the authorization server for a resource by
// trying, in order:
//
//  1. The `resource_metadata=<url>` parameter advertised in the 401's
//     WWW-Authenticate header (per RFC 9728). Compliant servers like
//     amplitude/notion use this path.
//  2. RFC 9728 well-known: `<origin>/.well-known/oauth-protected-resource`,
//     which points at the authorization server. Datadog's MCP serves this
//     but does NOT set the WWW-Authenticate header.
//  3. RFC 8414 well-known: `<origin>/.well-known/oauth-authorization-server`,
//     the AS metadata directly, for servers that conflate resource and AS.
//
// Returns the discovered AS metadata, or an explicit error listing what was
// tried — so probe/edit shows a useful message when a server is unauthable.
func discoverOAuthMetadata(serverURL, wwwAuth string) (*oauthMeta, error) {
	if rmURL := parseResourceMetadata(wwwAuth); rmURL != "" {
		if meta, err := discoverOAuth(rmURL); err == nil {
			return meta, nil
		}
	}
	origin, err := originOf(serverURL)
	if err != nil {
		return nil, err
	}
	if meta, err := discoverOAuth(origin + "/.well-known/oauth-protected-resource"); err == nil {
		return meta, nil
	}
	if body, err := httpGet(origin + "/.well-known/oauth-authorization-server"); err == nil {
		var meta oauthMeta
		if json.Unmarshal(body, &meta) == nil && meta.AuthorizationEndpoint != "" {
			return &meta, nil
		}
	}
	return nil, fmt.Errorf("no OAuth metadata at %s (tried WWW-Authenticate, /.well-known/oauth-protected-resource, /.well-known/oauth-authorization-server)", origin)
}

func originOf(serverURL string) (string, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return "", err
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("server url has no scheme/host: %s", serverURL)
	}
	return u.Scheme + "://" + u.Host, nil
}

func httpGet(u string) ([]byte, error) {
	resp, err := http.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// ── PKCE ──────────────────────────────────────────────────────────────────────

func pkce() (verifier, challenge string) {
	b := make([]byte, 32)
	rand.Read(b)
	verifier = base64.RawURLEncoding.EncodeToString(b)
	h := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(h[:])
	return
}

func randomState() string {
	b := make([]byte, 16)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// ── Callback server ───────────────────────────────────────────────────────────

func waitCallback(port int) (code string, err error) {
	codeCh := make(chan string, 1)
	mux := http.NewServeMux()
	srv := &http.Server{Addr: fmt.Sprintf("localhost:%d", port), Handler: mux}
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<h2>Authorization complete &#8212; you can close this tab.</h2>")
		codeCh <- code
	})
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return "", err
	}
	go srv.Serve(ln)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	defer srv.Shutdown(ctx)
	select {
	case code = <-codeCh:
		return code, nil
	case <-ctx.Done():
		return "", fmt.Errorf("OAuth callback timed out")
	}
}

// ── Full OAuth flow ───────────────────────────────────────────────────────────

func oauthFlow(serverURL, wwwAuth string) (string, error) {
	meta, err := discoverOAuthMetadata(serverURL, wwwAuth)
	if err != nil {
		return "", fmt.Errorf("OAuth discovery: %w", err)
	}
	if meta.RegistrationEndpoint == "" {
		return "", fmt.Errorf("server does not support dynamic registration")
	}

	// Find a free port
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		return "", err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	redirectURI := fmt.Sprintf("http://localhost:%d/callback", port)
	clientID, err := getOrRegisterClient(meta.RegistrationEndpoint, redirectURI)
	if err != nil {
		return "", fmt.Errorf("client registration: %w", err)
	}

	verifier, challenge := pkce()
	state := randomState()
	authURL := meta.AuthorizationEndpoint + "?" + url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}.Encode()

	fmt.Fprintf(os.Stderr, "\nOpening browser for OAuth authorization...\n")
	fmt.Fprintf(os.Stderr, "  If browser doesn't open, visit:\n  %s\n\n", authURL)
	openBrowser(authURL)

	fmt.Fprintln(os.Stderr, "Waiting for authorization (timeout: 120s)...")
	code, err := waitCallback(port)
	if err != nil || code == "" {
		return "", fmt.Errorf("no authorization code received")
	}

	// Exchange code for token
	vals := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {clientID},
		"code_verifier": {verifier},
	}
	resp, err := http.PostForm(meta.TokenEndpoint, vals)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var td tokenData
	if err := json.Unmarshal(body, &td); err != nil || td.AccessToken == "" {
		return "", fmt.Errorf("token exchange failed: %s", body)
	}
	if td.ExpiresAt == 0 {
		td.ExpiresAt = float64(time.Now().Unix()) + 3600
	}
	td.TokenEndpoint = meta.TokenEndpoint
	td.ClientID = clientID
	saveToken(serverURL, td)
	fmt.Fprintln(os.Stderr, "OAuth: authorized successfully.")
	return td.AccessToken, nil
}

func parseResourceMetadata(wwwAuth string) string {
	for _, part := range strings.Split(wwwAuth, ",") {
		p := strings.TrimSpace(part)
		low := strings.ToLower(p)
		if strings.HasPrefix(low, "resource_metadata=") {
			return strings.Trim(p[len("resource_metadata="):], `"'`)
		}
	}
	return ""
}
