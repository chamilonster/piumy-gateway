// Tests for principal-terminal full authority (PrincipalTerminalID).
// The principal (DefaultTerminalID) bypasses the default-DENY gate and can
// call any tool without a dispatch, while non-principals remain gated.
package mcpserver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"piumy-gateway/internal/router"
	"piumy-gateway/internal/state"
	"piumy-gateway/internal/store"

	"github.com/mark3labs/mcp-go/server"
)

const principalTermID = "term-principal"

func serverWithPrincipal(t *testing.T) (*store.Store, *server.MCPServer, context.Context) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "piumy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	routerPath := filepath.Join(dir, "router.json")
	if err := os.WriteFile(routerPath, []byte(`{"allow_all":true,"default_mode":"dedicated"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	rtMgr := router.NewManager(routerPath)
	sm := state.NewManager(filepath.Join(dir, "status.json"), 8)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv := New(ctx, Deps{
		Store: st, State: sm, Router: rtMgr, AgentIdle: time.Minute,
		PrincipalTerminalID: principalTermID,
	})
	return st, srv, ctx
}

func principalCtx(ctx context.Context) context.Context {
	return withTerminalID(ctx, principalTermID)
}

// TestPrincipalCanInitiateToBoss verifies "iniciar autorizado" (candado
// versión segura, ct-2026-07-18-1438): the principal sin dispatch bound
// puede iniciar SIEMPRE a un chat is_boss=true (boss verbatim: "quiero que
// puedan hablarme al iniciar").
func TestPrincipalCanInitiateToBoss(t *testing.T) {
	st, srv, ctx := serverWithPrincipal(t)
	chat := "55500000088@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetIsBoss(chat, true); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDefaultRules("responder normalmente"); err != nil {
		t.Fatal(err)
	}
	_, pv := decisionPolicy("")
	out := callTool(t, principalCtx(ctx), srv, "send_message", map[string]any{
		"to": chat, "message": "hola boss", "model": "m", "policy_version": pv,
	})
	if !strings.Contains(out, "queued for sending") {
		t.Errorf("principal iniciando a is_boss sin dispatch: send_message = %s, want queued", out)
	}
}

// TestPrincipalCanInitiateToActiveWithRules verifies "iniciar autorizado":
// el principal sin dispatch bound puede iniciar a un chat NO-boss que el
// boss marcó active=true (vía set_chat_active) y que tiene rules efectivas
// — "hablarle a quienes yo se los pida".
func TestPrincipalCanInitiateToActiveWithRules(t *testing.T) {
	st, srv, ctx := serverWithPrincipal(t)
	chat := "55500000089@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetActive(chat, true); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDefaultRules("responder normalmente"); err != nil {
		t.Fatal(err)
	}
	_, pv := decisionPolicy("")
	out := callTool(t, principalCtx(ctx), srv, "send_message", map[string]any{
		"to": chat, "message": "hola", "model": "m", "policy_version": pv,
	})
	if !strings.Contains(out, "queued for sending") {
		t.Errorf("principal iniciando a active+rules sin dispatch: send_message = %s, want queued", out)
	}
}

// TestPrincipalCannotInitiateToUnauthorized verifies the actual lock: a
// chat that's neither is_boss ni active=true (aunque TENGA rules, vía el
// default) sigue exigiendo dispatch bound — ct-2026-07-13-0538's "autoridad
// plena" queda acotada, no es un free-for-all.
func TestPrincipalCannotInitiateToUnauthorized(t *testing.T) {
	st, srv, ctx := serverWithPrincipal(t)
	chat := "55500000090@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDefaultRules("responder normalmente"); err != nil {
		t.Fatal(err)
	}
	_, pv := decisionPolicy("")
	out := callTool(t, principalCtx(ctx), srv, "send_message", map[string]any{
		"to": chat, "message": "hola", "model": "m", "policy_version": pv,
	})
	if !strings.Contains(out, "locked:") {
		t.Errorf("principal iniciando a chat no-autorizado (ni boss ni active) = %s, want locked/denied", out)
	}
}

// TestPrincipalCanEnumerateWithoutDispatch: principal sin dispatch → list_chats
// / get_messages / get_chat → OK (enumeration tools and chat-scoped tools bypass).
func TestPrincipalCanEnumerateWithoutDispatch(t *testing.T) {
	st, srv, ctx := serverWithPrincipal(t)
	chat := "55500000091@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	pCtx := principalCtx(ctx)

	if out := callTool(t, pCtx, srv, "list_chats", map[string]any{"limit": 5}); strings.Contains(out, "refused") || strings.Contains(out, "locked") {
		t.Errorf("principal: list_chats = %s, want it allowed", out)
	}
	if out := callTool(t, pCtx, srv, "get_chat", map[string]any{"chat_id": chat}); strings.Contains(out, "refused") || strings.Contains(out, "locked") {
		t.Errorf("principal: get_chat = %s, want it allowed", out)
	}
	if out := callTool(t, pCtx, srv, "get_messages", map[string]any{"chat_id": chat}); strings.Contains(out, "refused") || strings.Contains(out, "locked") {
		t.Errorf("principal: get_messages = %s, want it allowed", out)
	}
}

// TestNonPrincipalStillGatedWithoutDispatch: terminal NO principal sin dispatch
// → herramientas gated → "default DENY" (gating intacto).
func TestNonPrincipalStillGatedWithoutDispatch(t *testing.T) {
	st, srv, ctx := serverWithPrincipal(t)
	chat := "55500000092@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	otherCtx := withTerminalID(ctx, "term-other-not-principal")
	_, pv := decisionPolicy("")

	cases := []struct {
		tool string
		args map[string]any
	}{
		{"list_chats", map[string]any{"limit": 5}},
		{"get_chat", map[string]any{"chat_id": chat}},
		{"send_message", map[string]any{"to": chat, "message": "x", "model": "m", "policy_version": pv}},
	}
	for _, c := range cases {
		out := callTool(t, otherCtx, srv, c.tool, c.args)
		if !strings.Contains(out, "default DENY") && !strings.Contains(out, "locked") && !strings.Contains(out, "refused") {
			t.Errorf("non-principal sin dispatch: %s = %s, want it denied", c.tool, out)
		}
	}
}

// TestPrincipalCanSendWithoutPolicyVersion: el principal puede omitir
// policy_version en send_message — es opcional para él. Usa un chat is_boss
// (autorizado a iniciar) para que el envío pase el candado nuevo también.
func TestPrincipalCanSendWithoutPolicyVersion(t *testing.T) {
	st, srv, ctx := serverWithPrincipal(t)
	chat := "55500000093@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetIsBoss(chat, true); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDefaultRules("responder normalmente"); err != nil {
		t.Fatal(err)
	}
	// sin policy_version — el principal puede omitirla
	out := callTool(t, principalCtx(ctx), srv, "send_message", map[string]any{
		"to": chat, "message": "hola boss", "model": "m",
	})
	if !strings.Contains(out, "queued for sending") {
		t.Errorf("principal sin policy_version: send_message = %s, want queued", out)
	}
}

// TestPrincipalRespectsNoRulesLaw: principal a un chat is_boss=true (así SÍ
// está autorizado a iniciar) pero SIN rules → igual rechazado — "iniciar
// autorizado" salta el candado del dispatch bound, nunca la ley de rules.
func TestPrincipalRespectsNoRulesLaw(t *testing.T) {
	st, srv, ctx := serverWithPrincipal(t)
	chat := "55500000094@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetIsBoss(chat, true); err != nil {
		t.Fatal(err)
	}
	// deliberadamente sin SetDefaultRules/SetChatRules — EffectiveRules == ""
	_, pv := decisionPolicy("")
	out := callTool(t, principalCtx(ctx), srv, "send_message", map[string]any{
		"to": chat, "message": "hola", "model": "m", "policy_version": pv,
	})
	if !strings.Contains(out, "no rules on this chat") {
		t.Errorf("principal a chat is_boss sin rules = %s, want it blocked", out)
	}
}
