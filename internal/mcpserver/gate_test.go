package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"piumy-gateway/internal/router"
	"piumy-gateway/internal/state"
	"piumy-gateway/internal/store"
)

// serverWithGate builds a server wired to a caller-supplied Gate (so tests
// can RegisterDispatch synthetic {nonce, level, chat, terminal_id}
// dispatches — cAPI doesn't exist yet, F4b) and a whitelist-everything
// router (guardrails other than the gate aren't what these tests are
// about). Callers inject a terminal id into their own per-call context via
// withTerminalID before calling callTool — no real MCP transport involved.
func serverWithGate(t *testing.T, gate *Gate) (*store.Store, *server.MCPServer, context.Context) {
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

	srv := New(ctx, Deps{Store: st, State: sm, Router: rtMgr, AgentIdle: time.Minute, Gate: gate})
	return st, srv, ctx
}

// unlockToken extracts the token from get_instructions' JSON-RPC response —
// the tool result's text content is itself JSON (Instructions), so this
// unwraps twice: outer JSON-RPC envelope, then the inner Instructions.
func unlockToken(t *testing.T, out string) string {
	t.Helper()
	var envelope struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatalf("parse JSON-RPC envelope: %v\nraw: %s", err, out)
	}
	if len(envelope.Result.Content) == 0 {
		t.Fatalf("no content in get_instructions response: %s", out)
	}
	var instr Instructions
	if err := json.Unmarshal([]byte(envelope.Result.Content[0].Text), &instr); err != nil {
		t.Fatalf("parse Instructions: %v\nraw text: %s", err, envelope.Result.Content[0].Text)
	}
	if instr.Token == "" {
		t.Fatalf("empty token in Instructions: %+v", instr)
	}
	return instr.Token
}

func TestGateDefaultDenyWithNoDispatch(t *testing.T) {
	gate := NewGate()
	st, srv, ctx := serverWithGate(t, gate)
	chat := "55500000045@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDefaultRules("responder normalmente"); err != nil {
		t.Fatal(err)
	}
	termCtx := withTerminalID(ctx, "term-nodispatch")

	// send_message: default DENY, no fallback to "unrestricted".
	_, policyVersion := decisionPolicy("")
	out := callTool(t, termCtx, srv, "send_message", map[string]any{
		"to": chat, "message": "hola", "model": "m", "policy_version": policyVersion,
	})
	if !strings.Contains(out, "default DENY") {
		t.Errorf("send_message with no dispatch registered = %s, want default DENY", out)
	}

	// A gated (chat-scoped) tool: also denied.
	if out := callTool(t, termCtx, srv, "get_chat", map[string]any{"chat_id": chat}); !strings.Contains(out, "default DENY") {
		t.Errorf("get_chat with no dispatch = %s, want default DENY", out)
	}
	// An enumeration tool: also denied.
	if out := callTool(t, termCtx, srv, "list_chats", map[string]any{}); !strings.Contains(out, "default DENY") {
		t.Errorf("list_chats with no dispatch = %s, want default DENY", out)
	}
	// An UNGATED tool (no chat concept): still open, never required a dispatch.
	if out := callTool(t, termCtx, srv, "get_status", map[string]any{}); strings.Contains(out, "DENY") {
		t.Errorf("get_status with no dispatch = %s, want it unaffected (never gated)", out)
	}
}

func TestGateSendWithoutUnlockFails(t *testing.T) {
	gate := NewGate()
	st, srv, ctx := serverWithGate(t, gate)
	chat := "55500000004@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDefaultRules("responder normalmente"); err != nil {
		t.Fatal(err)
	}
	termCtx := withTerminalID(ctx, "term-1")
	if err := gate.RegisterDispatch("nonce-1", chat, LevelCaution, "term-1", 0); err != nil {
		t.Fatal(err)
	}
	callTool(t, termCtx, srv, "get_instructions", map[string]any{"nonce": "nonce-1"})

	_, policyVersion := decisionPolicy("")
	out := callTool(t, termCtx, srv, "send_message", map[string]any{
		"to": chat, "message": "hola", "model": "m", "policy_version": policyVersion,
	})
	if !strings.Contains(out, "locked:") {
		t.Errorf("send_message before unlock = %s, want the locked error", out)
	}
}

func TestGateFullFlowThenSendSucceeds(t *testing.T) {
	gate := NewGate()
	st, srv, ctx := serverWithGate(t, gate)
	chat := "55500000005@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDefaultRules("responder normalmente"); err != nil {
		t.Fatal(err)
	}
	termCtx := withTerminalID(ctx, "term-2")
	if err := gate.RegisterDispatch("nonce-2", chat, LevelCaution, "term-2", 0); err != nil {
		t.Fatal(err)
	}
	instr := callTool(t, termCtx, srv, "get_instructions", map[string]any{"nonce": "nonce-2"})
	token := unlockToken(t, instr)

	if out := callTool(t, termCtx, srv, "unlock", map[string]any{"token": token}); !strings.Contains(out, "unlocked") {
		t.Fatalf("unlock = %s, want success", out)
	}
	if out := callTool(t, termCtx, srv, "skip", map[string]any{}); !strings.Contains(out, "ready") {
		t.Fatalf("skip = %s, want ready", out)
	}

	_, policyVersion := decisionPolicy("")
	out := callTool(t, termCtx, srv, "send_message", map[string]any{
		"to": chat, "message": "hola", "model": "m", "policy_version": policyVersion,
	})
	if !strings.Contains(out, "queued for sending") {
		t.Errorf("send_message after full gate flow = %s, want it to pass", out)
	}

	// One-shot: replaying the same flow (no new get_instructions) must fail.
	replay := callTool(t, termCtx, srv, "send_message", map[string]any{
		"to": chat, "message": "otra vez", "model": "m", "policy_version": policyVersion,
	})
	if !strings.Contains(replay, "locked:") {
		t.Errorf("send_message replay after consume = %s, want it locked again", replay)
	}
}

func TestGateCheckpointRequiredBeforeReady(t *testing.T) {
	gate := NewGate()
	st, srv, ctx := serverWithGate(t, gate)
	chat := "55500000006@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	termCtx := withTerminalID(ctx, "term-3")
	if err := gate.RegisterDispatch("nonce-3", chat, LevelDanger, "term-3", 0); err != nil {
		t.Fatal(err)
	}
	instr := callTool(t, termCtx, srv, "get_instructions", map[string]any{"nonce": "nonce-3"})
	token := unlockToken(t, instr)
	callTool(t, termCtx, srv, "unlock", map[string]any{"token": token})

	_, policyVersion := decisionPolicy("")
	out := callTool(t, termCtx, srv, "send_message", map[string]any{
		"to": chat, "message": "hola", "model": "m", "policy_version": policyVersion,
	})
	if !strings.Contains(out, "locked:") {
		t.Errorf("send_message after unlock but before remember/skip = %s, want it still locked", out)
	}
}

// TestGateAntiReplay covers the anti-replay DoD scenario: a token from a
// nonce this terminal no longer holds (replaced by a newer get_instructions)
// must fail — terminal-scoped lookup makes this fall out for free, no
// separate token index needed.
func TestGateAntiReplay(t *testing.T) {
	gate := NewGate()
	_, srv, ctx := serverWithGate(t, gate)
	termCtx := withTerminalID(ctx, "term-4")

	if err := gate.RegisterDispatch("nonce-a", "chatA@c.us", LevelCaution, "term-4", 0); err != nil {
		t.Fatal(err)
	}
	instrA := callTool(t, termCtx, srv, "get_instructions", map[string]any{"nonce": "nonce-a"})
	tokenA := unlockToken(t, instrA)

	if err := gate.RegisterDispatch("nonce-b", "chatB@c.us", LevelCaution, "term-4", 0); err != nil {
		t.Fatal(err)
	}
	callTool(t, termCtx, srv, "get_instructions", map[string]any{"nonce": "nonce-b"}) // replaces A

	out := callTool(t, termCtx, srv, "unlock", map[string]any{"token": tokenA})
	if !strings.Contains(out, "invalid token") {
		t.Errorf("unlock with nonce-a's stale token after nonce-b replaced it = %s, want invalid token", out)
	}
}

// TestGateUnlockRejectsEmptyTokenWithoutGetInstructions is the H4
// hardening regression (ct-2026-07-10-0540): d.token is "" until
// GetInstructions sets it, so an agent that never calls get_instructions
// could previously call unlock(token="") and slip past locked -> noting
// without ever ingesting rules/memory/context.
func TestGateUnlockRejectsEmptyTokenWithoutGetInstructions(t *testing.T) {
	gate := NewGate()
	_, srv, ctx := serverWithGate(t, gate)
	termCtx := withTerminalID(ctx, "term-5")

	if err := gate.RegisterDispatch("nonce-5", "chat5@c.us", LevelCaution, "term-5", 0); err != nil {
		t.Fatal(err)
	}
	// Deliberately skip get_instructions — d.token is still its zero value.

	out := callTool(t, termCtx, srv, "unlock", map[string]any{"token": ""})
	if !strings.Contains(out, "invalid token") {
		t.Errorf("unlock(token=\"\") without get_instructions = %s, want invalid token", out)
	}
}

// TestGateCrossTerminalHijackFails is the critical F4b evidence: terminal B
// must never be able to consume a dispatch registered for terminal A, even
// knowing the correct nonce.
func TestGateCrossTerminalHijackFails(t *testing.T) {
	gate := NewGate()
	_, srv, ctx := serverWithGate(t, gate)

	if err := gate.RegisterDispatch("nonce-x", "chatX@c.us", LevelCaution, "term-A", 0); err != nil {
		t.Fatal(err)
	}

	// Terminal B tries to pull terminal A's dispatch by guessing/knowing the nonce.
	termB := withTerminalID(ctx, "term-B")
	out := callTool(t, termB, srv, "get_instructions", map[string]any{"nonce": "nonce-x"})
	if !strings.Contains(out, "not registered for this terminal") {
		t.Errorf("terminal B pulling terminal A's dispatch = %s, want a hijack refusal", out)
	}

	// Terminal A (the rightful owner) still works normally.
	termA := withTerminalID(ctx, "term-A")
	instr := callTool(t, termA, srv, "get_instructions", map[string]any{"nonce": "nonce-x"})
	if strings.Contains(instr, "not registered") {
		t.Errorf("terminal A pulling its OWN dispatch = %s, want it to succeed", instr)
	}
}

func TestLevelGateDangerBlocksEnumerationAndPrivileged(t *testing.T) {
	gate := NewGate()
	_, srv, ctx := serverWithGate(t, gate)
	termCtx := withTerminalID(ctx, "term-d")
	if err := gate.RegisterDispatch("nonce-d", "chatD@c.us", LevelDanger, "term-d", 0); err != nil {
		t.Fatal(err)
	}
	callTool(t, termCtx, srv, "get_instructions", map[string]any{"nonce": "nonce-d"})

	if out := callTool(t, termCtx, srv, "list_chats", map[string]any{}); !strings.Contains(out, "anti-leakage") {
		t.Errorf("list_chats under danger = %s, want it blocked", out)
	}
	if out := callTool(t, termCtx, srv, "reset_dashboard_password", map[string]any{}); !strings.Contains(out, "boss-only") {
		t.Errorf("reset_dashboard_password under danger = %s, want boss-only refusal", out)
	}
	// get_outbox/get_drafts take no chat_id at all — PendingOutbox/
	// PendingDrafts return every chat's queue unfiltered, so danger/caution
	// must not see them either (found in F4b audit, ct-2026-07-09-1641).
	if out := callTool(t, termCtx, srv, "get_outbox", map[string]any{}); !strings.Contains(out, "anti-leakage") {
		t.Errorf("get_outbox under danger = %s, want it blocked (unfiltered cross-chat data)", out)
	}
	if out := callTool(t, termCtx, srv, "get_drafts", map[string]any{}); !strings.Contains(out, "anti-leakage") {
		t.Errorf("get_drafts under danger = %s, want it blocked (unfiltered cross-chat data)", out)
	}
}

func TestLevelGateBossUnrestricted(t *testing.T) {
	gate := NewGate()
	st, srv, ctx := serverWithGate(t, gate)
	chat := "55500000017@c.us"
	if err := st.TouchChat(chat, "Boss", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDefaultRules("responder normalmente"); err != nil {
		t.Fatal(err)
	}
	termCtx := withTerminalID(ctx, "term-boss")
	if err := gate.RegisterDispatch("nonce-boss", chat, LevelBoss, "term-boss", 0); err != nil {
		t.Fatal(err)
	}
	callTool(t, termCtx, srv, "get_instructions", map[string]any{"nonce": "nonce-boss"})

	if out := callTool(t, termCtx, srv, "list_chats", map[string]any{}); strings.Contains(out, "anti-leakage") {
		t.Errorf("list_chats under boss = %s, want it unrestricted", out)
	}
	if out := callTool(t, termCtx, srv, "get_outbox", map[string]any{}); strings.Contains(out, "anti-leakage") {
		t.Errorf("get_outbox under boss = %s, want it unrestricted", out)
	}

	// Boss sends WITHOUT unlock — "sin gate" (level=boss must come from an
	// explicit dispatch, which this test registers — never from absence).
	_, policyVersion := decisionPolicy("")
	out := callTool(t, termCtx, srv, "send_message", map[string]any{
		"to": chat, "message": "avisale a Juan", "model": "m", "policy_version": policyVersion,
	})
	if !strings.Contains(out, "queued for sending") {
		t.Errorf("boss send_message without unlock = %s, want it to pass (sin gate)", out)
	}
}

func TestLevelGateChatScopedToolsBlockCrossChat(t *testing.T) {
	gate := NewGate()
	st, srv, ctx := serverWithGate(t, gate)
	chatA, chatB := "55500000075@c.us", "55500000076@c.us"
	if err := st.TouchChat(chatA, "A", 1); err != nil {
		t.Fatal(err)
	}
	termCtx := withTerminalID(ctx, "term-scope")
	if err := gate.RegisterDispatch("nonce-scope", chatA, LevelCaution, "term-scope", 0); err != nil {
		t.Fatal(err)
	}
	callTool(t, termCtx, srv, "get_instructions", map[string]any{"nonce": "nonce-scope"})

	if out := callTool(t, termCtx, srv, "get_chat", map[string]any{"chat_id": chatB}); !strings.Contains(out, "anti-leakage") {
		t.Errorf("get_chat on a DIFFERENT chat under caution = %s, want it blocked", out)
	}
	if out := callTool(t, termCtx, srv, "get_chat", map[string]any{"chat_id": chatA}); strings.Contains(out, "anti-leakage") {
		t.Errorf("get_chat on the dispatch's OWN chat under caution = %s, want it allowed through", out)
	}
}

func TestGateRememberWritesMemoryAndContext(t *testing.T) {
	gate := NewGate()
	st, srv, ctx := serverWithGate(t, gate)
	chat := "55500000077@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	termCtx := withTerminalID(ctx, "term-r")
	if err := gate.RegisterDispatch("nonce-r", chat, LevelCaution, "term-r", 0); err != nil {
		t.Fatal(err)
	}
	instr := callTool(t, termCtx, srv, "get_instructions", map[string]any{"nonce": "nonce-r"})
	token := unlockToken(t, instr)
	callTool(t, termCtx, srv, "unlock", map[string]any{"token": token})
	callTool(t, termCtx, srv, "remember", map[string]any{"memory": "le gusta el café", "context": "cliente frecuente"})

	c, _, err := st.GetChat(chat)
	if err != nil {
		t.Fatal(err)
	}
	if c.Memory != "le gusta el café" || c.Context != "cliente frecuente" {
		t.Errorf("got memory=%q context=%q, want them written by remember", c.Memory, c.Context)
	}
}

// TestGateBossConsumedDispatchNoLongerGrantsPrivileges is the ST-A security
// regression (ct-2026-07-11-0740, CRITICAL — Amatista's audit): Consume()
// marks a dispatch done but left it bound in byTerminal with Level still
// Boss. validateSend and levelGateMiddleware both keyed off Level==Boss
// alone, never checking Ready — so a terminal that consumed ONE boss
// dispatch could keep sending anywhere and calling boss-only tools forever,
// no new dispatch ever required. Unlike
// TestRegisterDispatchClosesResidualPrivilegeWindow (which needs a NEW
// dispatch to arrive to demonstrate the fix), this is the direct case: the
// SAME terminal, SAME (already-consumed) dispatch, no new registration at
// all.
func TestGateBossConsumedDispatchNoLongerGrantsPrivileges(t *testing.T) {
	gate := NewGate()
	st, srv, ctx := serverWithGate(t, gate)
	bossChat := "55500000078@c.us"
	if err := st.TouchChat(bossChat, "Boss", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDefaultRules("responder normalmente"); err != nil {
		t.Fatal(err)
	}
	termCtx := withTerminalID(ctx, "term-boss-done")

	if err := gate.RegisterDispatch("nonce-boss-done", bossChat, LevelBoss, "term-boss-done", 0); err != nil {
		t.Fatal(err)
	}
	_, policyVersion := decisionPolicy("")
	first := callTool(t, termCtx, srv, "send_message", map[string]any{
		"to": bossChat, "message": "hola", "model": "m", "policy_version": policyVersion,
	})
	if !strings.Contains(first, "queued for sending") {
		t.Fatalf("setup: first boss send (no unlock needed, sin gate) = %s, want it to pass", first)
	}

	if active, ok := gate.Active("term-boss-done"); !ok || active.Ready {
		t.Fatalf("setup: Active after consume = %+v, ok=%v, want bound with Ready=false", active, ok)
	}

	// The vulnerability: a SECOND send, with no new dispatch, must now be
	// denied instead of sailing through on the residual Level=boss.
	second := callTool(t, termCtx, srv, "send_message", map[string]any{
		"to": bossChat, "message": "otra vez, sin nuevo dispatch", "model": "m", "policy_version": policyVersion,
	})
	if !strings.Contains(second, "locked:") {
		t.Errorf("second send_message after boss consume, no new dispatch = %s, want locked/denied", second)
	}

	// Same story for a boss-only DB-admin tool (levelGateMiddleware's path,
	// not validateSend's).
	setBoss := callTool(t, termCtx, srv, "set_is_boss", map[string]any{"chat_id": bossChat, "is_boss": true})
	if !strings.Contains(setBoss, "locked:") {
		t.Errorf("set_is_boss after boss consume, no new dispatch = %s, want locked/denied", setBoss)
	}
}

// TestRegisterDispatchClosesResidualPrivilegeWindow covers the security fix
// found while writing F4c: a terminal bound to a boss dispatch (even
// consumed/done) must NOT keep answering as boss once a new, lower-trust
// dispatch is registered for it. Before the fix, RegisterDispatch only
// touched byNonce, leaving byTerminal pointed at the stale boss dispatch
// until the agent itself called GetInstructions for the new one — a
// prompt-injected agent that simply skipped that call kept operating with
// boss privileges. See TestGateBossConsumedDispatchNoLongerGrantsPrivileges
// for the ST-A follow-up (ct-2026-07-11-0740): that fix closes the same hole
// even WITHOUT a new dispatch ever arriving.
func TestRegisterDispatchClosesResidualPrivilegeWindow(t *testing.T) {
	gate := NewGate()
	st, srv, ctx := serverWithGate(t, gate)
	bossChat := "55500000079@c.us"
	dangerChat := "55500000080@c.us"
	if err := st.TouchChat(bossChat, "Boss", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDefaultRules("responder normalmente"); err != nil {
		t.Fatal(err)
	}
	termCtx := withTerminalID(ctx, "term-residual")

	// 1) Boss dispatch, pulled and consumed via a real send.
	if err := gate.RegisterDispatch("nonce-boss", bossChat, LevelBoss, "term-residual", 0); err != nil {
		t.Fatal(err)
	}
	callTool(t, termCtx, srv, "get_instructions", map[string]any{"nonce": "nonce-boss"})
	_, policyVersion := decisionPolicy("")
	callTool(t, termCtx, srv, "send_message", map[string]any{
		"to": bossChat, "message": "hola", "model": "m", "policy_version": policyVersion,
	})
	// ST-A fix (ct-2026-07-11-0740): Level legitimately stays Boss after
	// consume (Consume never changes level, only state) — that part was
	// never the bug. The bug was that Ready ALSO stayed irrelevant for
	// boss forever, so a done dispatch kept granting access. Ready must
	// now read false post-consume, same as any other level.
	active, ok := gate.Active("term-residual")
	if !ok || active.Level != LevelBoss {
		t.Fatalf("setup: Active after consume = %+v, ok=%v, want still bound with Level=boss (level itself doesn't change on consume)", active, ok)
	}
	if active.Ready {
		t.Fatal("setup: Active().Ready after consume = true, want false (ST-A: a done dispatch must not read as ready, even for boss)")
	}

	// 2) A NEW danger dispatch arrives for the SAME terminal — WITHOUT the
	// agent calling get_instructions for it yet.
	if err := gate.RegisterDispatch("nonce-danger", dangerChat, LevelDanger, "term-residual", 0); err != nil {
		t.Fatal(err)
	}

	// Active() must reflect the NEW dispatch immediately — not the stale boss one.
	active, ok = gate.Active("term-residual")
	if !ok {
		t.Fatal("Active after registering the new dispatch: want bound=true")
	}
	if active.Level != LevelDanger {
		t.Errorf("Active().Level = %q, want %q (residual boss privilege must not survive)", active.Level, LevelDanger)
	}
	if active.Ready {
		t.Error("Active().Ready = true, want false (new dispatch is locked, not gated yet)")
	}
	if active.ChatJID != dangerChat {
		t.Errorf("Active().ChatJID = %q, want %q", active.ChatJID, dangerChat)
	}

	// A boss-only tool called before get_instructions(danger) must be refused.
	if out := callTool(t, termCtx, srv, "reset_dashboard_password", map[string]any{}); !strings.Contains(out, "boss-only") {
		t.Errorf("reset_dashboard_password on the residual-privilege terminal = %s, want boss-only refusal", out)
	}

	// send_message to the OLD (boss) chat must also be denied now.
	out := callTool(t, termCtx, srv, "send_message", map[string]any{
		"to": bossChat, "message": "otra vez", "model": "m", "policy_version": policyVersion,
	})
	if !strings.Contains(out, "locked:") {
		t.Errorf("send_message to the old boss chat after a new danger dispatch = %s, want it locked/denied", out)
	}
}

func TestInFlightReportsBoundNonDoneDispatch(t *testing.T) {
	gate := NewGate()
	if gate.InFlight("term-x") {
		t.Error("InFlight with nothing registered: want false")
	}
	if err := gate.RegisterDispatch("n1", "chat@c.us", LevelCaution, "term-x", 0); err != nil {
		t.Fatal(err)
	}
	if !gate.InFlight("term-x") {
		t.Error("InFlight right after RegisterDispatch (locked, not done): want true")
	}
	gate.Consume("term-x")
	if gate.InFlight("term-x") {
		t.Error("InFlight after Consume (done): want false")
	}
}

// TestNonceActive covers ct-2026-07-18-1851-B: capipush.newNonce checks this
// before committing to a short (4-hex) nonce, regenerating on a collision.
func TestNonceActive(t *testing.T) {
	gate := NewGate()
	if gate.NonceActive("ab12") {
		t.Error("NonceActive with nothing registered: want false")
	}
	if err := gate.RegisterDispatch("ab12", "chat@c.us", LevelCaution, "term-y", 0); err != nil {
		t.Fatal(err)
	}
	if !gate.NonceActive("ab12") {
		t.Error("NonceActive right after RegisterDispatch: want true")
	}
	if gate.NonceActive("cd34") {
		t.Error("NonceActive for an unrelated nonce: want false")
	}
	gate.Consume("term-y")
	if gate.NonceActive("ab12") {
		t.Error("NonceActive after Consume (done, evicted from byNonce): want false")
	}
}

// TestSweepReclaimsStaleBoundDispatch is the H5 hardening regression
// (ct-2026-07-10-0540): an agent that calls get_instructions and then
// crashes (or never calls unlock/remember/skip/send_message) used to leave
// InFlight(terminalID) == true forever — sweepLocked previously only
// reclaimed a dispatch that was NEVER pulled (!boundToTerm), never a
// bound-but-stuck one. Backdates lastActivity directly (same package,
// white-box) instead of actually waiting dispatchStaleAfter (1h).
func TestSweepReclaimsStaleBoundDispatch(t *testing.T) {
	gate := NewGate()
	_, srv, ctx := serverWithGate(t, gate)
	termCtx := withTerminalID(ctx, "term-stale")

	if err := gate.RegisterDispatch("nonce-stale", "chat-stale@c.us", LevelCaution, "term-stale", 0); err != nil {
		t.Fatal(err)
	}
	callTool(t, termCtx, srv, "get_instructions", map[string]any{"nonce": "nonce-stale"})
	if !gate.InFlight("term-stale") {
		t.Fatal("setup: want the terminal in-flight after get_instructions")
	}

	gate.mu.Lock()
	gate.byNonce["nonce-stale"].lastActivity = time.Now().Add(-dispatchStaleAfter - time.Minute)
	gate.sweepLocked(time.Now())
	gate.mu.Unlock()

	if gate.InFlight("term-stale") {
		t.Error("InFlight after sweeping a stale bound dispatch = true, want false (freed)")
	}
	if _, ok := gate.Active("term-stale"); ok {
		t.Error("Active after sweeping a stale bound dispatch = ok, want nothing bound")
	}
}

// TestSweepDoesNotReclaimActiveBoundDispatch is the control: a dispatch
// well within dispatchStaleAfter must survive the sweep untouched — a
// legitimately slow agent (still thinking between get_instructions and
// send_message) must never get force-denied mid-flow.
func TestSweepDoesNotReclaimActiveBoundDispatch(t *testing.T) {
	gate := NewGate()
	if err := gate.RegisterDispatch("nonce-fresh", "chat-fresh@c.us", LevelCaution, "term-fresh", 0); err != nil {
		t.Fatal(err)
	}

	gate.mu.Lock()
	gate.sweepLocked(time.Now())
	gate.mu.Unlock()

	if !gate.InFlight("term-fresh") {
		t.Error("InFlight after sweeping a fresh dispatch = false, want true (not stale yet)")
	}
}

// TestSetStaleAfterOverridesReclaimWindow is the configurable-knob
// regression (ct-2026-07-11-074123, post-incident): dispatchStaleAfter used to
// be a hardcoded 1h with no way to shorten it — a dispatch orphaned by a
// crashed agent silently wedged its terminal for up to an hour. Confirms
// SetStaleAfter actually changes what sweepLocked reclaims against.
func TestSetStaleAfterOverridesReclaimWindow(t *testing.T) {
	gate := NewGate()
	gate.SetStaleAfter(10 * time.Minute)

	if err := gate.RegisterDispatch("nonce-short", "chat@c.us", LevelCaution, "term-short", 0); err != nil {
		t.Fatal(err)
	}

	gate.mu.Lock()
	gate.byNonce["nonce-short"].lastActivity = time.Now().Add(-15 * time.Minute)
	gate.sweepLocked(time.Now())
	gate.mu.Unlock()

	if gate.InFlight("term-short") {
		t.Error("InFlight after sweeping past the overridden 10m stale window = true, want false (reclaimed)")
	}
}

// TestSetStaleAfterIgnoresNonPositive confirms the default (dispatchStaleAfter,
// 1h) survives a bogus override instead of silently disabling reclamation.
func TestSetStaleAfterIgnoresNonPositive(t *testing.T) {
	gate := NewGate()
	gate.SetStaleAfter(0)
	gate.SetStaleAfter(-5 * time.Minute)

	if err := gate.RegisterDispatch("nonce-default", "chat@c.us", LevelCaution, "term-default", 0); err != nil {
		t.Fatal(err)
	}

	gate.mu.Lock()
	gate.byNonce["nonce-default"].lastActivity = time.Now().Add(-15 * time.Minute)
	gate.sweepLocked(time.Now())
	gate.mu.Unlock()

	if !gate.InFlight("term-default") {
		t.Error("InFlight after 15m with a non-positive SetStaleAfter (should be ignored, default 1h stands) = false, want it still in-flight")
	}
}

// TestCancelDispatchFreesTerminal is capipush's revert path (H5 hardening,
// ct-2026-07-10-0540): if Inject fails right after RegisterDispatch
// succeeded, CancelDispatch must free the terminal immediately instead of
// leaving it wedged until the hourly sweep.
func TestCancelDispatchFreesTerminal(t *testing.T) {
	gate := NewGate()
	if err := gate.RegisterDispatch("nonce-cancel", "chat-cancel@c.us", LevelCaution, "term-cancel", 0); err != nil {
		t.Fatal(err)
	}
	if !gate.InFlight("term-cancel") {
		t.Fatal("setup: want the terminal in-flight after RegisterDispatch")
	}

	gate.CancelDispatch("nonce-cancel", "term-cancel")

	if gate.InFlight("term-cancel") {
		t.Error("InFlight after CancelDispatch = true, want false (freed)")
	}
	if _, ok := gate.Active("term-cancel"); ok {
		t.Error("Active after CancelDispatch = ok, want nothing bound")
	}
}

// TestCancelDispatchIgnoresMismatch confirms CancelDispatch never touches
// the WRONG dispatch: a late cancel for a nonce a newer dispatch has
// already superseded (RegisterDispatch's own force-replace already dropped
// it from byNonce — see its doc comment) is a no-op, not an eviction of
// the terminal's current, still-valid dispatch.
func TestCancelDispatchIgnoresMismatch(t *testing.T) {
	gate := NewGate()
	if err := gate.RegisterDispatch("nonce-old", "chat@c.us", LevelCaution, "term-shared", 0); err != nil {
		t.Fatal(err)
	}
	// A newer dispatch replaces the old one for the same terminal — mirrors
	// RegisterDispatch's own force-replace (gate.go doc comment).
	if err := gate.RegisterDispatch("nonce-new", "chat@c.us", LevelCaution, "term-shared", 0); err != nil {
		t.Fatal(err)
	}

	// A late CancelDispatch for the stale nonce (e.g. a slow Inject error
	// arriving after a newer dispatch already took over) must not evict it.
	gate.CancelDispatch("nonce-old", "term-shared")

	if !gate.InFlight("term-shared") {
		t.Error("InFlight after CancelDispatch on a superseded nonce = false, want true (the newer dispatch must survive)")
	}
	active, ok := gate.Active("term-shared")
	if !ok || active.ChatJID != "chat@c.us" {
		t.Errorf("Active after CancelDispatch on a superseded nonce = %+v, ok=%v, want the newer dispatch untouched", active, ok)
	}
}

// TestRegisterDispatchStoresBurstMaxTS — ct-2026-07-13-2243 Fix 2: the
// burstMaxTS passed to RegisterDispatch is visible via Active so send_message
// can use it for MarkHandledBefore instead of time.Now(), preventing
// messages that arrived during the in-flight period from being marked handled.
func TestRegisterDispatchStoresBurstMaxTS(t *testing.T) {
	gate := NewGate()
	const wantTS int64 = 9999
	if err := gate.RegisterDispatch("nonce-bts", "chat@c.us", LevelCaution, "term-bts", wantTS); err != nil {
		t.Fatal(err)
	}
	active, ok := gate.Active("term-bts")
	if !ok {
		t.Fatal("Active returned nothing after RegisterDispatch")
	}
	if active.BurstMaxTS != wantTS {
		t.Errorf("Active.BurstMaxTS = %d, want %d", active.BurstMaxTS, wantTS)
	}
}

// TestBossUnlockAndSkipAreIdempotentNoLongerContradict is S2's core
// regression (ct-2026-07-30-030928, reproduces the smoke's live deadlock):
// a boss dispatch is BORN gateReady (ST-A, ct-2026-07-11-0740), so
// unlock/skip used to disagree about the exact same state — "already
// unlocked" then "not unlocked — call unlock first". Both must now succeed
// as harmless no-ops, and the ritual must still end in a normal send.
func TestBossUnlockAndSkipAreIdempotentNoLongerContradict(t *testing.T) {
	gate := NewGate()
	st, srv, ctx := serverWithGate(t, gate)
	chat := "55500000081@c.us"
	if err := st.TouchChat(chat, "Boss", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDefaultRules("responder normalmente"); err != nil {
		t.Fatal(err)
	}
	termCtx := withTerminalID(ctx, "term-boss-idempotent")
	if err := gate.RegisterDispatch("nonce-boss-idem", chat, LevelBoss, "term-boss-idempotent", 0); err != nil {
		t.Fatal(err)
	}
	callTool(t, termCtx, srv, "get_instructions", map[string]any{"nonce": "nonce-boss-idem"})

	// The habitual ritual an agent might still run out of habit for a
	// born-ready dispatch — an empty token proves the idempotent path never
	// even looks at it (this isn't the real locked->noting transition).
	unlockOut := callTool(t, termCtx, srv, "unlock", map[string]any{"token": ""})
	if strings.Contains(unlockOut, "already unlocked") || !strings.Contains(unlockOut, "unlocked") {
		t.Errorf("unlock on a born-ready boss dispatch = %s, want a plain success (no contradiction)", unlockOut)
	}
	skipOut := callTool(t, termCtx, srv, "skip", map[string]any{})
	if strings.Contains(skipOut, "not unlocked") || !strings.Contains(skipOut, "ready") {
		t.Errorf("skip on a born-ready boss dispatch = %s, want a plain success (no contradiction)", skipOut)
	}

	_, policyVersion := decisionPolicy("")
	out := callTool(t, termCtx, srv, "send_message", map[string]any{
		"to": chat, "message": "hola", "model": "m", "policy_version": policyVersion,
	})
	if !strings.Contains(out, "queued for sending") {
		t.Errorf("send_message after the (idempotent) unlock/skip ritual = %s, want it to pass", out)
	}
}

// TestUnlockAndSkipIdempotentOnDoubleCall covers S2's fix beyond boss: a
// SECOND unlock (already noting) or a second skip (already ready) on the
// same dispatch is a harmless no-op, not an error — same mechanism, no
// special-casing by level.
func TestUnlockAndSkipIdempotentOnDoubleCall(t *testing.T) {
	gate := NewGate()
	_, srv, ctx := serverWithGate(t, gate)
	termCtx := withTerminalID(ctx, "term-double")
	if err := gate.RegisterDispatch("nonce-double", "chat@c.us", LevelCaution, "term-double", 0); err != nil {
		t.Fatal(err)
	}
	instr := callTool(t, termCtx, srv, "get_instructions", map[string]any{"nonce": "nonce-double"})
	token := unlockToken(t, instr)

	if out := callTool(t, termCtx, srv, "unlock", map[string]any{"token": token}); !strings.Contains(out, "unlocked") {
		t.Fatalf("first unlock = %s, want success", out)
	}
	if out := callTool(t, termCtx, srv, "unlock", map[string]any{"token": token}); !strings.Contains(out, "unlocked") {
		t.Errorf("second unlock (already noting) = %s, want idempotent success", out)
	}
	if out := callTool(t, termCtx, srv, "skip", map[string]any{}); !strings.Contains(out, "ready") {
		t.Fatalf("first skip = %s, want success", out)
	}
	if out := callTool(t, termCtx, srv, "skip", map[string]any{}); !strings.Contains(out, "ready") {
		t.Errorf("second skip (already ready) = %s, want idempotent success", out)
	}
}

// TestUnlockAndSkipErrorDistinctlyAfterConsume is the other half of S2's
// fix: a truly consumed (done) dispatch must still be refused, but with its
// OWN message — before this, it was indistinguishable from the harmless
// boss/double-call case ("already unlocked" either way).
func TestUnlockAndSkipErrorDistinctlyAfterConsume(t *testing.T) {
	gate := NewGate()
	st, srv, ctx := serverWithGate(t, gate)
	chat := "55500000082@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDefaultRules("responder normalmente"); err != nil {
		t.Fatal(err)
	}
	termCtx := withTerminalID(ctx, "term-consumed")
	if err := gate.RegisterDispatch("nonce-consumed", chat, LevelCaution, "term-consumed", 0); err != nil {
		t.Fatal(err)
	}
	instr := callTool(t, termCtx, srv, "get_instructions", map[string]any{"nonce": "nonce-consumed"})
	token := unlockToken(t, instr)
	callTool(t, termCtx, srv, "unlock", map[string]any{"token": token})
	callTool(t, termCtx, srv, "skip", map[string]any{})
	_, policyVersion := decisionPolicy("")
	callTool(t, termCtx, srv, "send_message", map[string]any{
		"to": chat, "message": "hola", "model": "m", "policy_version": policyVersion,
	})

	unlockOut := callTool(t, termCtx, srv, "unlock", map[string]any{"token": token})
	if !strings.Contains(unlockOut, "already consumed") {
		t.Errorf("unlock after consume = %s, want the distinct \"already consumed\" error", unlockOut)
	}
	skipOut := callTool(t, termCtx, srv, "skip", map[string]any{})
	if !strings.Contains(skipOut, "already consumed") {
		t.Errorf("skip after consume = %s, want the distinct \"already consumed\" error", skipOut)
	}
}

// TestStaleSweepLogsReclaim is S2's criterio de listo #3: a stuck dispatch
// already released itself via the stale sweep without a gateway restart
// (H5, ct-2026-07-10-0540) — it just did so in total silence. S1's log
// channel now covers it too.
func TestStaleSweepLogsReclaim(t *testing.T) {
	gate := NewGate()
	if err := gate.RegisterDispatch("nonce-stale-log", "chat-stale-log@c.us", LevelCaution, "term-stale-log", 0); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })

	gate.mu.Lock()
	gate.byNonce["nonce-stale-log"].lastActivity = time.Now().Add(-dispatchStaleAfter - time.Minute)
	gate.sweepLocked(time.Now())
	gate.mu.Unlock()

	if !strings.Contains(buf.String(), "liberado por timeout") {
		t.Errorf("log tras evictar un dispatch stale = %q, want mención de \"liberado por timeout\"", buf.String())
	}
	if gate.InFlight("term-stale-log") {
		t.Error("InFlight tras el sweep stale = true, want false (liberado)")
	}
}

// TestGetInstructionsLogsUnknownNonce covers the diagnosability half of
// Citrino's second S2 finding (nonces orphaned by RegisterDispatch's own
// force-replace — a security guarantee, not touched here): the failure was
// silent server-side even though the agent got an MCP error back.
func TestGetInstructionsLogsUnknownNonce(t *testing.T) {
	gate := NewGate()
	_, srv, ctx := serverWithGate(t, gate)
	termCtx := withTerminalID(ctx, "term-orphan")

	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })

	callTool(t, termCtx, srv, "get_instructions", map[string]any{"nonce": "nonce-nunca-registrado"})

	if !strings.Contains(buf.String(), "no hay dispatch registrado") {
		t.Errorf("log tras get_instructions con nonce desconocido = %q, want mención del caso", buf.String())
	}
}
