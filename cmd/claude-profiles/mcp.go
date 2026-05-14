package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"
)

// ToolInfo carries name and read-only hint from the MCP server.
type ToolInfo struct {
	Name         string
	ReadOnlyHint bool
}

// toolInfoPrefix prepends "mcp__<sname>__" to each tool name.
func toolInfoPrefix(tools []ToolInfo, sname string) []ToolInfo {
	out := make([]ToolInfo, len(tools))
	for i, t := range tools {
		out[i] = ToolInfo{Name: "mcp__" + sname + "__" + t.Name, ReadOnlyHint: t.ReadOnlyHint}
	}
	return out
}

// errNeedsAuth signals that the caller should surface "OAuth is required" to
// the user rather than treat the failure as a network/protocol problem.
var errNeedsAuth = errors.New("authentication required")

// fetchTimeout caps a tools/list round-trip. Streamable HTTP can wait on a
// standalone SSE stream forever, so we always run under a context deadline.
const fetchTimeout = 30 * time.Second

// FetchTools probes an MCP server and returns its tools with readOnlyHint.
// Uses the official modelcontextprotocol/go-sdk for both transports —
// previously a ~250-line hand-rolled JSON-RPC/SSE/initialize implementation.
func FetchTools(cfg ServerConfig, sname string) ([]ToolInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()

	transport, err := buildTransport(cfg)
	if err != nil {
		return nil, err
	}

	client := mcp.NewClient(&mcp.Implementation{
		Name:    "claude-profiles",
		Version: "dev",
	}, nil)

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		// SDK surfaces auth failures as wrapped auth.ErrOAuth / ErrInvalidToken.
		if errors.Is(err, auth.ErrOAuth) || errors.Is(err, auth.ErrInvalidToken) {
			return nil, fmt.Errorf("%w: %v", errNeedsAuth, err)
		}
		return nil, err
	}
	defer session.Close()

	res, err := session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		return nil, err
	}

	out := make([]ToolInfo, len(res.Tools))
	for i, t := range res.Tools {
		ro := false
		if t.Annotations != nil {
			ro = t.Annotations.ReadOnlyHint
		}
		out[i] = ToolInfo{Name: t.Name, ReadOnlyHint: ro}
	}
	return toolInfoPrefix(out, sname), nil
}

// buildTransport picks the right Transport based on profile-server type. For
// HTTP it wires our token cache + oauthFlow into the SDK via an OAuthHandler;
// for stdio it spawns the command and lets the SDK speak newline-delimited
// JSON over its pipes.
func buildTransport(cfg ServerConfig) (mcp.Transport, error) {
	switch cfg.Type {
	case "stdio":
		if cfg.Command == "" {
			return nil, fmt.Errorf("stdio server has no command")
		}
		cmd := exec.Command(cfg.Command, cfg.Args...)
		return &mcp.CommandTransport{Command: cmd}, nil
	default: // "http" or empty
		if cfg.URL == "" {
			return nil, fmt.Errorf("http server has no url")
		}
		return &mcp.StreamableClientTransport{
			Endpoint:     cfg.URL,
			OAuthHandler: &cachedOAuthHandler{serverURL: cfg.URL},
		}, nil
	}
}

// cachedOAuthHandler adapts our existing token cache and oauthFlow to the
// SDK's auth.OAuthHandler interface. TokenSource is intentionally stateless
// (re-reads from disk each call) so the SDK picks up a fresh token after
// Authorize completes the interactive flow.
type cachedOAuthHandler struct {
	serverURL string
}

func (h *cachedOAuthHandler) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	return &diskTokenSource{serverURL: h.serverURL}, nil
}

func (h *cachedOAuthHandler) Authorize(ctx context.Context, req *http.Request, resp *http.Response) error {
	wwwAuth := ""
	if resp != nil {
		wwwAuth = resp.Header.Get("Www-Authenticate")
		if resp.Body != nil {
			resp.Body.Close()
		}
	}
	clearToken(h.serverURL)
	if _, err := oauthFlow(h.serverURL, wwwAuth); err != nil {
		return fmt.Errorf("%w: %v", auth.ErrOAuth, err)
	}
	return nil
}

// diskTokenSource looks up the cached token each time the SDK asks. Returns a
// zero-Token (no AccessToken) when nothing is cached — the SDK treats that as
// "no Authorization header", lets the server respond 401, then calls Authorize.
type diskTokenSource struct {
	serverURL string
}

func (s *diskTokenSource) Token() (*oauth2.Token, error) {
	tok := loadToken(s.serverURL)
	if tok == "" {
		return &oauth2.Token{}, nil
	}
	return &oauth2.Token{AccessToken: tok, TokenType: "Bearer"}, nil
}
