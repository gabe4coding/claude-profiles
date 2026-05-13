package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// ToolInfo carries name and read-only hint from the MCP server.
type ToolInfo struct {
	Name         string `json:"name"`
	ReadOnlyHint bool   `json:"readOnlyHint"`
}

// toolInfoPrefix prepends "mcp__<sname>__" to each tool name.
func toolInfoPrefix(tools []ToolInfo, sname string) []ToolInfo {
	out := make([]ToolInfo, len(tools))
	for i, t := range tools {
		out[i] = ToolInfo{Name: "mcp__" + sname + "__" + t.Name, ReadOnlyHint: t.ReadOnlyHint}
	}
	return out
}

// FetchTools probes an MCP server and returns its tools with readOnlyHint.
// Returns (tools, nil) on success; (nil, err) on error; (nil, errNeedsAuth) if OAuth needed.
var errNeedsAuth = fmt.Errorf("authentication required")

func FetchTools(cfg ServerConfig, sname string) ([]ToolInfo, error) {
	switch cfg.Type {
	case "stdio":
		tools, err := toolsStdio(cfg.Command, cfg.Args)
		if err != nil {
			return nil, err
		}
		return toolInfoPrefix(tools, sname), nil
	default: // "http" or empty
		tools, err := toolsHTTP(cfg.URL)
		if err != nil {
			return nil, err
		}
		return toolInfoPrefix(tools, sname), nil
	}
}

// ── HTTP transport ────────────────────────────────────────────────────────────

var mcpHTTPClient = &http.Client{Timeout: 15 * time.Second}

type mcpReq struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int   `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type mcpResp struct {
	Result *json.RawMessage `json:"result"`
	Error  *json.RawMessage `json:"error"`
}

func toolsHTTP(serverURL string) ([]ToolInfo, error) {
	token := loadToken(serverURL)
	tools, status, wwwAuth, err := attemptHTTP(serverURL, token)
	if err == nil {
		return tools, nil
	}
	if status != 401 {
		return nil, err
	}
	// 401 — try OAuth
	clearToken(serverURL)
	token, oauthErr := oauthFlow(serverURL, wwwAuth)
	if oauthErr != nil {
		return nil, fmt.Errorf("%w: %v", errNeedsAuth, oauthErr)
	}
	tools, _, _, err = attemptHTTP(serverURL, token)
	return tools, err
}

// attemptHTTP tries direct tools/list then initialize-handshake.
// Returns (tools, httpStatus, www-authenticate, error).
func attemptHTTP(serverURL, token string) ([]ToolInfo, int, string, error) {
	// 1. Stateless: try tools/list directly
	resp, status, wwwAuth, sid, err := mcpPost(serverURL, token, "", toolsListMsg(1))
	if status == 401 {
		return nil, 401, wwwAuth, fmt.Errorf("unauthorized")
	}
	if err == nil && resp.Result != nil {
		if tools, err := extractTools(*resp.Result); err == nil && len(tools) > 0 {
			return tools, 0, "", nil
		}
	}

	// 2. Stateful: initialize → notifications/initialized → tools/list
	resp, status, wwwAuth, sid, err = mcpPost(serverURL, token, "", initMsg())
	if status == 401 {
		return nil, 401, wwwAuth, fmt.Errorf("unauthorized")
	}
	if err != nil || resp.Result == nil {
		return nil, status, "", fmt.Errorf("initialize failed")
	}
	mcpPost(serverURL, token, sid, notifyMsg()) // fire-and-forget
	resp, status, wwwAuth, _, err = mcpPost(serverURL, token, sid, toolsListMsg(2))
	if status == 401 {
		return nil, 401, wwwAuth, fmt.Errorf("unauthorized")
	}
	if err != nil || resp.Result == nil {
		return nil, status, "", fmt.Errorf("tools/list failed")
	}
	tools, err := extractTools(*resp.Result)
	return tools, status, "", err
}

// mcpPost sends a single JSON-RPC request and returns the raw response.
func mcpPost(serverURL, token, sessionID, payload string) (data mcpResp, status int, wwwAuth, sid string, err error) {
	req, _ := http.NewRequest("POST", serverURL, strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}

	resp, err := mcpHTTPClient.Do(req)
	if err != nil {
		return data, 0, "", "", err
	}
	defer resp.Body.Close()

	status = resp.StatusCode
	wwwAuth = resp.Header.Get("Www-Authenticate")
	sid = resp.Header.Get("Mcp-Session-Id")

	if status == 401 {
		return data, 401, wwwAuth, sid, fmt.Errorf("unauthorized")
	}

	body, _ := io.ReadAll(resp.Body)
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "text/event-stream") {
		data = parseSSE(body)
	} else {
		json.Unmarshal(body, &data)
	}
	return data, status, wwwAuth, sid, nil
}

func parseSSE(body []byte) mcpResp {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data:") {
			var r mcpResp
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data:")), &r) == nil {
				if r.Result != nil || r.Error != nil {
					return r
				}
			}
		}
	}
	return mcpResp{}
}

func extractTools(raw json.RawMessage) ([]ToolInfo, error) {
	var result struct {
		Tools []struct {
			Name        string `json:"name"`
			Annotations *struct {
				ReadOnlyHint bool `json:"readOnlyHint"`
			} `json:"annotations"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	if result.Tools == nil {
		return nil, fmt.Errorf("no tools in result")
	}
	out := make([]ToolInfo, len(result.Tools))
	for i, t := range result.Tools {
		ro := false
		if t.Annotations != nil {
			ro = t.Annotations.ReadOnlyHint
		}
		out[i] = ToolInfo{Name: t.Name, ReadOnlyHint: ro}
	}
	return out, nil
}

func initMsg() string {
	return `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"mcp-probe","version":"1.0"}}}`
}

func notifyMsg() string {
	return `{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`
}

func toolsListMsg(id int) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/list","params":{}}`, id)
}

// ── Stdio transport ───────────────────────────────────────────────────────────

func toolsStdio(command string, args []string) ([]ToolInfo, error) {
	cmd := exec.Command(command, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer func() {
		stdin.Close()
		cmd.Process.Kill()
		cmd.Wait()
	}()

	send := func(msg string) {
		fmt.Fprintln(stdin, msg)
	}
	recv := func() *mcpResp {
		type result struct {
			resp *mcpResp
			err  error
		}
		ch := make(chan result, 1)
		go func() {
			scanner := bufio.NewScanner(stdout)
			if scanner.Scan() {
				var r mcpResp
				if json.Unmarshal(scanner.Bytes(), &r) == nil {
					ch <- result{resp: &r}
					return
				}
			}
			ch <- result{err: fmt.Errorf("no response")}
		}()
		select {
		case r := <-ch:
			if r.err != nil {
				return nil
			}
			return r.resp
		case <-time.After(5 * time.Second):
			return nil
		}
	}

	send(initMsg())
	if recv() == nil {
		return nil, fmt.Errorf("no response to initialize")
	}
	send(notifyMsg())
	send(toolsListMsg(2))
	resp := recv()
	if resp == nil || resp.Result == nil {
		return nil, fmt.Errorf("no tools/list response")
	}
	return extractTools(*resp.Result)
}
