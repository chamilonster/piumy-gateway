package mcpserver

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"piumy-gateway/internal/router"
	"piumy-gateway/internal/state"
	"piumy-gateway/internal/store"
)

// callTool drives a registered tool end-to-end via HandleMessage (no HTTP
// transport needed) and returns the raw JSON-RPC response.
func callTool(t *testing.T, ctx context.Context, srv *server.MCPServer, name string, args map[string]any) string {
	t.Helper()
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": name, "arguments": args},
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	resp := srv.HandleMessage(ctx, raw)
	out, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// decodeToolText extracts a single-text-content tool result's payload from
// callTool's raw JSON-RPC envelope — needed whenever the payload itself
// contains characters json.Marshal HTML-escapes (<, >, &), which a raw
// substring match against the envelope would miss.
func decodeToolText(t *testing.T, out string) string {
	t.Helper()
	var envelope struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatalf("decoding tool result envelope: %v (raw: %s)", err, out)
	}
	if len(envelope.Result.Content) == 0 {
		t.Fatalf("tool result has no content: %s", out)
	}
	return envelope.Result.Content[0].Text
}

func newTestServer(t *testing.T) (*store.Store, *server.MCPServer, context.Context, *Gate) {
	t.Helper()
	dir := t.TempDir()

	st, err := store.Open(filepath.Join(dir, "piumy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	rtMgr := router.NewManager(filepath.Join(dir, "router.json"))
	sm := state.NewManager(filepath.Join(dir, "status.json"), 8)
	gate := NewGate()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	mcpSrv := New(ctx, Deps{Store: st, State: sm, Router: rtMgr, AgentIdle: time.Minute, Gate: gate})
	return st, mcpSrv, ctx, gate
}

// bossDispatchContext registers and pulls a boss-level dispatch for a
// synthetic terminal, returning a ctx carrying that terminal's id. Tests
// that exercise a gated tool's OWN logic (not the gate itself, which
// gate_test.go covers directly) use this to clear the F4b default-DENY
// gate cleanly — boss bypasses ready/chat-match and every level-gate
// restriction, so the tool's pre-existing behavior is reachable unchanged.
func bossDispatchContext(t *testing.T, gate *Gate, srv *server.MCPServer, ctx context.Context, chatJID string) context.Context {
	t.Helper()
	termID := "term-test"
	nonce := "nonce-test-" + chatJID
	if err := gate.RegisterDispatch(nonce, chatJID, LevelBoss, termID, 0); err != nil {
		t.Fatal(err)
	}
	termCtx := withTerminalID(ctx, termID)
	if out := callTool(t, termCtx, srv, "get_instructions", map[string]any{"nonce": nonce}); strings.Contains(out, "isError\":true") {
		t.Fatalf("bossDispatchContext setup: get_instructions failed: %s", out)
	}
	return termCtx
}
