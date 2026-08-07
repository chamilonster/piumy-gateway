package mcpserver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"piumy-gateway/internal/eventbus"
	"piumy-gateway/internal/gateway"
	"piumy-gateway/internal/router"
	"piumy-gateway/internal/state"
	"piumy-gateway/internal/store"
)

// fakeGateway is a minimal gateway.Gateway stub — only Connected() is
// exercised by TestSendMessageRejectsWhenGatewayDisconnected, everything
// else is an unused no-op to satisfy the interface.
type fakeGateway struct{ connected bool }

func (f *fakeGateway) Start(ctx context.Context) error { return nil }
func (f *fakeGateway) Stop()                           {}
func (f *fakeGateway) Connected() bool                 { return f.connected }
func (f *fakeGateway) Inbound() <-chan gateway.Inbound { return nil }
func (f *fakeGateway) Send(ctx context.Context, toJID, text string) (gateway.SendResult, error) {
	return gateway.SendResult{}, nil
}
func (f *fakeGateway) SetTyping(ctx context.Context, toJID string, on bool) error { return nil }
func (f *fakeGateway) MarkRead(ctx context.Context, chatJID string, msgIDs []string) error {
	return nil
}
func (f *fakeGateway) MarkDelivered(ctx context.Context, chatJID string, msgIDs []string) error {
	return nil
}
func (f *fakeGateway) QRChannel(ctx context.Context) (<-chan string, error) { return nil, nil }

// TestSendMessageDoesNotMeterAtEnqueueTime is the ST-D regression
// (ct-2026-07-11-074139): usage used to be recorded HERE, at enqueue time
// (F4-DESIGN §8's original design — "charged even for a held draft, the
// model already spent tokens producing it"). Moved to
// corepipeline.processOutbox, the one real-send choke point, so a message
// that's enqueued but never actually leaves (killed, dead-lettered) isn't
// counted as output, and a message that DOES leave is counted exactly
// once — not once here AND once again at the real send. See
// TestProcessOutboxMetersUsageOnSuccessfulSend (corepipeline) for the
// other half of this regression.
func TestSendMessageDoesNotMeterAtEnqueueTime(t *testing.T) {
	gate := NewGate()
	st, srv, ctx := serverWithGate(t, gate)
	chat := "55500000020@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDefaultRules("responder normalmente"); err != nil {
		t.Fatal(err)
	}
	termCtx := bossDispatchContext(t, gate, srv, ctx, chat)
	_, policyVersion := decisionPolicy("")

	callTool(t, termCtx, srv, "send_message", map[string]any{
		"to": chat, "message": "hola mundo", "model": "m", "policy_version": policyVersion,
	})

	u, err := st.UsageForDay(chat, store.Today())
	if err != nil {
		t.Fatal(err)
	}
	if u.Messages != 0 {
		t.Errorf("usage right after send_message (before any real send) = %+v, want zero", u)
	}
}

// TestDraftDoesNotMeterEvenIfDiscarded covers the "held-then-discarded"
// case explicitly: a draft never reaches WhatsApp regardless of what
// happens to it, so it must never contribute usage — at creation, or ever,
// since discard_draft doesn't touch the outbox at all.
func TestDraftDoesNotMeterEvenIfDiscarded(t *testing.T) {
	gate := NewGate()
	st, srv, ctx := serverWithGate(t, gate)
	chat := "55500000025@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDefaultRules("responder normalmente"); err != nil {
		t.Fatal(err)
	}
	termCtx := bossDispatchContext(t, gate, srv, ctx, chat)
	_, policyVersion := decisionPolicy("")

	callTool(t, termCtx, srv, "draft", map[string]any{
		"to": chat, "message": "borrador que nunca sale", "model": "m", "policy_version": policyVersion,
	})

	drafts, err := st.PendingDrafts(10)
	if err != nil || len(drafts) != 1 {
		t.Fatalf("setup: PendingDrafts = %+v, err=%v", drafts, err)
	}
	if ok, err := st.DiscardDraft(drafts[0].ID); err != nil || !ok {
		t.Fatalf("setup: DiscardDraft ok=%v err=%v", ok, err)
	}

	u, err := st.UsageForDay(chat, store.Today())
	if err != nil {
		t.Fatal(err)
	}
	if u.Messages != 0 {
		t.Errorf("usage after a discarded draft = %+v, want zero — it never reached WhatsApp", u)
	}
}

// TestSendMessageRejectsWhenGatewayDisconnected is the H6 hardening
// regression (ct-2026-07-10-0540): before this fix, send_message always
// enqueued regardless of gateway connectivity, so a deauthed/banned session
// silently piled messages into an outbox with no ETA — the agent read
// "queued for sending" as success. serverWithGate doesn't wire a Gateway
// (nil-safe, every other send_test.go case relies on that), so this test
// builds its own server with a fakeGateway instead of extending that helper
// for one caller.
func TestSendMessageRejectsWhenGatewayDisconnected(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "piumy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	routerPath := filepath.Join(dir, "router.json")
	if err := os.WriteFile(routerPath, []byte(`{"allow_all":true,"default_mode":"dedicated"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	rt := router.NewManager(routerPath)
	sm := state.NewManager(filepath.Join(dir, "status.json"), 8)
	gw := &fakeGateway{connected: false}
	gate := NewGate()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := New(ctx, Deps{Store: st, State: sm, Router: rt, Gate: gate, Gateway: gw, AgentIdle: time.Minute})

	chat := "55500000048@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDefaultRules("responder normalmente"); err != nil {
		t.Fatal(err)
	}
	termCtx := bossDispatchContext(t, gate, srv, ctx, chat)
	_, policyVersion := decisionPolicy("")

	out := callTool(t, termCtx, srv, "send_message", map[string]any{
		"to": chat, "message": "hola", "model": "m", "policy_version": policyVersion,
	})
	if !strings.Contains(out, "gateway is disconnected") {
		t.Errorf("send_message while disconnected = %s, want refusal", out)
	}
	pending, err := st.PendingOutbox(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("PendingOutbox after a disconnected send = %+v, want empty — never enqueued", pending)
	}

	gw.connected = true
	out2 := callTool(t, termCtx, srv, "send_message", map[string]any{
		"to": chat, "message": "hola", "model": "m", "policy_version": policyVersion,
	})
	if !strings.Contains(out2, "queued for sending") {
		t.Errorf("send_message once reconnected = %s, want it to send normally", out2)
	}
}

// TestConfirmationModeAlwaysHoldsDraft covers F4-DESIGN §4: "always" never
// sends directly — send_message creates a draft instead, fail-safe by code.
func TestConfirmationModeAlwaysHoldsDraft(t *testing.T) {
	gate := NewGate()
	st, srv, ctx := serverWithGate(t, gate)
	chat := "55500000021@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDefaultRules("responder normalmente"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetConfirmationMode(chat, "always"); err != nil {
		t.Fatal(err)
	}
	termCtx := bossDispatchContext(t, gate, srv, ctx, chat)
	_, policyVersion := decisionPolicy("")

	out := callTool(t, termCtx, srv, "send_message", map[string]any{
		"to": chat, "message": "hola", "model": "m", "policy_version": policyVersion,
	})
	if !strings.Contains(out, "held for confirmation") {
		t.Errorf("send_message under confirmation_mode=always = %s, want it held", out)
	}

	drafts, err := st.PendingDrafts(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(drafts) != 1 || drafts[0].ChatJID != chat || drafts[0].Text != "hola" {
		t.Errorf("PendingDrafts = %+v, want one draft for %s with text %q", drafts, chat, "hola")
	}

	pending, err := st.PendingOutbox(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("PendingOutbox after a held message = %+v, want empty (never enqueued)", pending)
	}
}

// TestConfirmationModeNoneSendsDirectly is the control: default/"none"
// still sends as before.
func TestConfirmationModeNoneSendsDirectly(t *testing.T) {
	gate := NewGate()
	st, srv, ctx := serverWithGate(t, gate)
	chat := "55500000022@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDefaultRules("responder normalmente"); err != nil {
		t.Fatal(err)
	}
	termCtx := bossDispatchContext(t, gate, srv, ctx, chat)
	_, policyVersion := decisionPolicy("")

	out := callTool(t, termCtx, srv, "send_message", map[string]any{
		"to": chat, "message": "hola", "model": "m", "policy_version": policyVersion,
	})
	if !strings.Contains(out, "queued for sending") {
		t.Errorf("send_message under confirmation_mode=none = %s, want it sent", out)
	}
}

// TestFreshGroupDefaultHoldsForConfirmation is the regression test for the
// HIGH finding in the F4c audit: a group chat TouchChat has never had its
// confirmation_mode explicitly set — its BY-TYPE DEFAULT (the actual,
// common case for a group the agent has never interacted with before)
// must hold for confirmation, not send directly. Before the fix, TouchChat
// wrote the legacy "required" value, which send_message's `== "always"`
// check never matched, so the fail-safe silently never fired for this
// exact, everyday case.
func TestFreshGroupDefaultHoldsForConfirmation(t *testing.T) {
	gate := NewGate()
	st, srv, ctx := serverWithGate(t, gate)
	group := "111222333-444555@g.us"
	if err := st.TouchChat(group, "Grupo", 1); err != nil {
		t.Fatal(err)
	}
	// The group starts "ignored" (0800) — whitelist it and give it rules so
	// the OTHER 6 checks pass, isolating this test to the confirmation_mode
	// behavior specifically, not a different gate.
	if err := st.SetStatus(group, "whitelist"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetChatRules(group, "responder normalmente"); err != nil {
		t.Fatal(err)
	}
	termCtx := bossDispatchContext(t, gate, srv, ctx, group)
	_, policyVersion := decisionPolicy("")

	out := callTool(t, termCtx, srv, "send_message", map[string]any{
		"to": group, "message": "hola grupo", "model": "m", "policy_version": policyVersion,
	})
	if !strings.Contains(out, "held for confirmation") {
		t.Errorf("send_message to a fresh, never-configured group = %s, want it held for confirmation (default confirmation_mode), not sent directly", out)
	}

	pending, err := st.PendingOutbox(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("PendingOutbox after a fresh-group send = %+v, want empty — it must never have sent directly", pending)
	}
}

// TestSendMessageToAnotherChatAlsoClosesDispatchChat is T33's own
// regression (ct-2026-08-06-1526) — reproduces exactly what the boss hit
// live: a boss dispatch on chat A ("escribile a mi amiga"), send_message
// replies to chat B (the third number). Before this fix, only B got marked
// handled — A's own dispatched message stayed pending and the sweep
// re-dispatched it once the terminal freed up, sending the boss's message
// to the agent a second time.
func TestSendMessageToAnotherChatAlsoClosesDispatchChat(t *testing.T) {
	gate := NewGate()
	st, srv, ctx := serverWithGate(t, gate)
	dispatchChat := "55500000095@c.us" // the boss's own chat — the dispatch
	otherChat := "55500000096@c.us"    // the third number he asked to write to
	for _, jid := range []string{dispatchChat, otherChat} {
		if err := st.TouchChat(jid, "C", 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SetMode(dispatchChat, "dedicated"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetActive(dispatchChat, true); err != nil {
		t.Fatal(err)
	}
	if err := st.AddMessage(store.Message{ChatJID: dispatchChat, ID: "m1", Text: "escribile a mi amiga", TS: 5}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDefaultRules("responder normalmente"); err != nil {
		t.Fatal(err)
	}

	termID := "term-cross-chat"
	if err := gate.RegisterDispatch("nonce-cross-chat", dispatchChat, LevelBoss, termID, 5); err != nil {
		t.Fatal(err)
	}
	termCtx := withTerminalID(ctx, termID)
	instr := callTool(t, termCtx, srv, "get_instructions", map[string]any{"nonce": "nonce-cross-chat"})
	token := unlockToken(t, instr)
	callTool(t, termCtx, srv, "unlock", map[string]any{"token": token})
	callTool(t, termCtx, srv, "skip", map[string]any{})
	_, policyVersion := decisionPolicy("")

	out := callTool(t, termCtx, srv, "send_message", map[string]any{
		"to": otherChat, "message": "hola", "model": "m", "policy_version": policyVersion,
	})
	if !strings.Contains(out, "queued for sending") {
		t.Fatalf("send_message = %s, want success", out)
	}

	pending, err := st.PendingDedicated(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("PendingDedicated after send_message to a different chat = %+v, want empty — the dispatch chat's own message must be marked handled too, not just the destination", pending)
	}
}

// TestSendMessageToAnotherChatDoesNotMarkDispatchMessagesAfterBurst is T33's
// own "no marques de más" requirement (listo cuando #4): a message that
// arrived in the dispatch's chat AFTER the burst that was actually
// dispatched must stay pending — markDispatchChatIfDifferent uses
// active.BurstMaxTS, never `now`, same discipline silent_act already
// established.
func TestSendMessageToAnotherChatDoesNotMarkDispatchMessagesAfterBurst(t *testing.T) {
	gate := NewGate()
	st, srv, ctx := serverWithGate(t, gate)
	dispatchChat := "55500000097@c.us"
	otherChat := "55500000098@c.us"
	for _, jid := range []string{dispatchChat, otherChat} {
		if err := st.TouchChat(jid, "C", 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SetMode(dispatchChat, "dedicated"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetActive(dispatchChat, true); err != nil {
		t.Fatal(err)
	}
	// The dispatched burst (ts=5) plus a message that arrived AFTER the
	// dispatch was already sent to the agent (ts=100, > burstMaxTS below).
	if err := st.AddMessage(store.Message{ChatJID: dispatchChat, ID: "m1", Text: "escribile a mi amiga", TS: 5}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddMessage(store.Message{ChatJID: dispatchChat, ID: "m2", Text: "epa, otra cosa", TS: 100}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDefaultRules("responder normalmente"); err != nil {
		t.Fatal(err)
	}

	termID := "term-cross-chat-burst"
	if err := gate.RegisterDispatch("nonce-cross-chat-burst", dispatchChat, LevelBoss, termID, 5); err != nil {
		t.Fatal(err)
	}
	termCtx := withTerminalID(ctx, termID)
	instr := callTool(t, termCtx, srv, "get_instructions", map[string]any{"nonce": "nonce-cross-chat-burst"})
	token := unlockToken(t, instr)
	callTool(t, termCtx, srv, "unlock", map[string]any{"token": token})
	callTool(t, termCtx, srv, "skip", map[string]any{})
	_, policyVersion := decisionPolicy("")

	callTool(t, termCtx, srv, "send_message", map[string]any{
		"to": otherChat, "message": "hola", "model": "m", "policy_version": policyVersion,
	})

	pending, err := st.PendingDedicated(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != "m2" {
		t.Errorf("PendingDedicated after closing the dispatch chat = %+v, want exactly m2 (arrived after the dispatched burst, must stay pending)", pending)
	}
}

// TestSendMessageSameChatDoesNotDoubleMark: the everyday case (reply in the
// same chat the dispatch came from) must behave exactly as before —
// markDispatchChatIfDifferent's own active.ChatJID != to guard must never
// fire here.
func TestSendMessageSameChatDoesNotDoubleMark(t *testing.T) {
	gate := NewGate()
	st, srv, ctx := serverWithGate(t, gate)
	chat := "55500000099@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDefaultRules("responder normalmente"); err != nil {
		t.Fatal(err)
	}
	termCtx := bossDispatchContext(t, gate, srv, ctx, chat)
	_, policyVersion := decisionPolicy("")

	out := callTool(t, termCtx, srv, "send_message", map[string]any{
		"to": chat, "message": "hola", "model": "m", "policy_version": policyVersion,
	})
	if !strings.Contains(out, "queued for sending") {
		t.Errorf("send_message in the same chat as the dispatch = %s, want it sent same as always", out)
	}
}

// TestDraftToolAlwaysDrafts covers the agent's own opt-in to hold a
// message, regardless of confirmation_mode (default here is "none").
func TestDraftToolAlwaysDrafts(t *testing.T) {
	gate := NewGate()
	st, srv, ctx := serverWithGate(t, gate)
	chat := "55500000023@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDefaultRules("responder normalmente"); err != nil {
		t.Fatal(err)
	}
	termCtx := bossDispatchContext(t, gate, srv, ctx, chat)
	_, policyVersion := decisionPolicy("")

	out := callTool(t, termCtx, srv, "draft", map[string]any{"to": chat, "message": "borrador", "model": "m", "policy_version": policyVersion})
	if !strings.Contains(out, "drafted") {
		t.Errorf("draft tool = %s, want it held as a draft", out)
	}

	drafts, err := st.PendingDrafts(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(drafts) != 1 || drafts[0].Text != "borrador" {
		t.Errorf("PendingDrafts = %+v, want one draft with text %q", drafts, "borrador")
	}
}

// TestDraftToAnotherChatAlsoClosesDispatchChat is T33's own "aplicá el
// mismo criterio a draft" requirement — the identical bug send_message had,
// reproduced against draft: a boss dispatch on chat A, draft written for
// chat B, A's own dispatched message must still end up marked handled.
func TestDraftToAnotherChatAlsoClosesDispatchChat(t *testing.T) {
	gate := NewGate()
	st, srv, ctx := serverWithGate(t, gate)
	dispatchChat := "55500000100@c.us"
	otherChat := "55500000101@c.us"
	for _, jid := range []string{dispatchChat, otherChat} {
		if err := st.TouchChat(jid, "C", 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SetMode(dispatchChat, "dedicated"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetActive(dispatchChat, true); err != nil {
		t.Fatal(err)
	}
	if err := st.AddMessage(store.Message{ChatJID: dispatchChat, ID: "m1", Text: "dejale un borrador a mi amiga", TS: 5}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDefaultRules("responder normalmente"); err != nil {
		t.Fatal(err)
	}

	termID := "term-draft-cross-chat"
	if err := gate.RegisterDispatch("nonce-draft-cross-chat", dispatchChat, LevelBoss, termID, 5); err != nil {
		t.Fatal(err)
	}
	termCtx := withTerminalID(ctx, termID)
	instr := callTool(t, termCtx, srv, "get_instructions", map[string]any{"nonce": "nonce-draft-cross-chat"})
	token := unlockToken(t, instr)
	callTool(t, termCtx, srv, "unlock", map[string]any{"token": token})
	callTool(t, termCtx, srv, "skip", map[string]any{})
	_, policyVersion := decisionPolicy("")

	out := callTool(t, termCtx, srv, "draft", map[string]any{
		"to": otherChat, "message": "borrador", "model": "m", "policy_version": policyVersion,
	})
	if !strings.Contains(out, "drafted") {
		t.Fatalf("draft = %s, want it held as a draft", out)
	}

	pending, err := st.PendingDedicated(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("PendingDedicated after draft to a different chat = %+v, want empty — the dispatch chat's own message must be marked handled too", pending)
	}
}

// TestDraftToolPublishesDraftEvent is T16 (ct-2026-08-05-123257): the
// dashboard's SSE auto-refresh needs a nudge the moment a draft is
// created, not just a status change nobody signals — "un borrador que
// aparece y no se ve hasta recargar es una respuesta que salió tarde"
// (Citrino).
func TestDraftToolPublishesDraftEvent(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "piumy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	routerPath := filepath.Join(dir, "router.json")
	if err := os.WriteFile(routerPath, []byte(`{"allow_all":true,"default_mode":"dedicated"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	rt := router.NewManager(routerPath)
	sm := state.NewManager(filepath.Join(dir, "status.json"), 8)
	gate := NewGate()
	bus := eventbus.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := New(ctx, Deps{Store: st, State: sm, Router: rt, Gate: gate, AgentIdle: time.Minute, Bus: bus})

	ch, unsubscribe := bus.Subscribe()
	defer unsubscribe()

	chat := "55500000102@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDefaultRules("responder normalmente"); err != nil {
		t.Fatal(err)
	}
	termCtx := bossDispatchContext(t, gate, srv, ctx, chat)
	_, policyVersion := decisionPolicy("")

	callTool(t, termCtx, srv, "draft", map[string]any{"to": chat, "message": "borrador", "model": "m", "policy_version": policyVersion})

	select {
	case e := <-ch:
		if e.Type != "draft" {
			t.Errorf("event = %+v, want type=draft", e)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the draft eventbus nudge")
	}
}

// TestDraftConsumesCautionDispatchOneShot: for a caution/danger dispatch
// (unlike boss, which is "sin gate" end to end — no ready/one-shot
// enforcement either), draft consumes the dispatch same as send_message.
func TestDraftConsumesCautionDispatchOneShot(t *testing.T) {
	gate := NewGate()
	st, srv, ctx := serverWithGate(t, gate)
	chat := "55500000103@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDefaultRules("responder normalmente"); err != nil {
		t.Fatal(err)
	}
	termCtx := withTerminalID(ctx, "term-draft-oneshot")
	if err := gate.RegisterDispatch("nonce-draft", chat, LevelCaution, "term-draft-oneshot", 0); err != nil {
		t.Fatal(err)
	}
	instr := callTool(t, termCtx, srv, "get_instructions", map[string]any{"nonce": "nonce-draft"})
	token := unlockToken(t, instr)
	callTool(t, termCtx, srv, "unlock", map[string]any{"token": token})
	callTool(t, termCtx, srv, "skip", map[string]any{})
	_, policyVersion := decisionPolicy("")

	out := callTool(t, termCtx, srv, "draft", map[string]any{"to": chat, "message": "borrador", "model": "m", "policy_version": policyVersion})
	if !strings.Contains(out, "drafted") {
		t.Fatalf("draft = %s, want it held", out)
	}

	replay := callTool(t, termCtx, srv, "draft", map[string]any{"to": chat, "message": "otra vez", "model": "m", "policy_version": policyVersion})
	if !strings.Contains(replay, "locked:") {
		t.Errorf("draft replay after consume = %s, want it locked again", replay)
	}
}

// TestSilentActReleasesTerminalImmediately is S11's core regression
// (ct-2026-07-30-1619, the boss's own "falta un silent act"): before this,
// gate.Consume was only ever called from send.go — deciding NOT to reply
// left the dispatch InFlight until dispatchStaleAfter (15min) reclaimed it,
// a mechanical reward for talking over staying silent. silent_act must
// release the terminal exactly as fast as send_message does.
func TestSilentActReleasesTerminalImmediately(t *testing.T) {
	gate := NewGate()
	st, srv, ctx := serverWithGate(t, gate)
	chat := "55500000104@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	termCtx := withTerminalID(ctx, "term-silent")
	if err := gate.RegisterDispatch("nonce-silent", chat, LevelCaution, "term-silent", 0); err != nil {
		t.Fatal(err)
	}
	instr := callTool(t, termCtx, srv, "get_instructions", map[string]any{"nonce": "nonce-silent"})
	token := unlockToken(t, instr)
	callTool(t, termCtx, srv, "unlock", map[string]any{"token": token})
	callTool(t, termCtx, srv, "skip", map[string]any{})

	if !gate.InFlight("term-silent") {
		t.Fatal("setup: InFlight before silent_act = false, want true")
	}
	out := callTool(t, termCtx, srv, "silent_act", map[string]any{"reason": "no me corresponde"})
	if !strings.Contains(out, "silence recorded") {
		t.Fatalf("silent_act = %s, want it to succeed", out)
	}
	if gate.InFlight("term-silent") {
		t.Error("InFlight after silent_act = true, want false — the terminal must release immediately, not wait dispatchStaleAfter")
	}

	replay := callTool(t, termCtx, srv, "silent_act", map[string]any{})
	if !strings.Contains(replay, "locked:") {
		t.Errorf("silent_act replay after consume = %s, want it locked again (one-shot, same as send_message)", replay)
	}
}

// TestSilentActMarksBurstHandled covers criterio de listo #2: the
// dispatched burst must not be re-dispatched after a silent_act, same as it
// wouldn't be after send_message — otherwise "decidí no responder" would
// just get the same messages pushed back at the agent on the next sweep.
func TestSilentActMarksBurstHandled(t *testing.T) {
	gate := NewGate()
	st, srv, ctx := serverWithGate(t, gate)
	chat := "55500000105@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetMode(chat, "dedicated"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetActive(chat, true); err != nil {
		t.Fatal(err)
	}
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m1", Text: "hola", TS: 5}); err != nil {
		t.Fatal(err)
	}
	termCtx := withTerminalID(ctx, "term-silent-mark")
	if err := gate.RegisterDispatch("nonce-silent-mark", chat, LevelCaution, "term-silent-mark", 5); err != nil {
		t.Fatal(err)
	}
	instr := callTool(t, termCtx, srv, "get_instructions", map[string]any{"nonce": "nonce-silent-mark"})
	token := unlockToken(t, instr)
	callTool(t, termCtx, srv, "unlock", map[string]any{"token": token})
	callTool(t, termCtx, srv, "skip", map[string]any{})

	callTool(t, termCtx, srv, "silent_act", map[string]any{})

	pending, err := st.PendingDedicated(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("PendingDedicated after silent_act = %d, want 0 (burst marked handled, not re-dispatched)", len(pending))
	}
}

// TestSilentActRecordsReason covers criterio de listo #3: the reason must
// be recoverable afterward (via get_chat/GetChat) — otherwise silence stays
// indistinguishable from a stuck agent, the exact gap S11 exists to close.
func TestSilentActRecordsReason(t *testing.T) {
	gate := NewGate()
	st, srv, ctx := serverWithGate(t, gate)
	chat := "55500000106@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	termCtx := withTerminalID(ctx, "term-silent-reason")
	if err := gate.RegisterDispatch("nonce-silent-reason", chat, LevelCaution, "term-silent-reason", 0); err != nil {
		t.Fatal(err)
	}
	instr := callTool(t, termCtx, srv, "get_instructions", map[string]any{"nonce": "nonce-silent-reason"})
	token := unlockToken(t, instr)
	callTool(t, termCtx, srv, "unlock", map[string]any{"token": token})
	callTool(t, termCtx, srv, "skip", map[string]any{})

	callTool(t, termCtx, srv, "silent_act", map[string]any{"reason": "ya tuve la última palabra"})

	c, ok, err := st.GetChat(chat)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("chat vanished")
	}
	if c.SilenceReason != "ya tuve la última palabra" {
		t.Errorf("SilenceReason = %q, want %q", c.SilenceReason, "ya tuve la última palabra")
	}
	if c.SilenceAt == 0 {
		t.Error("SilenceAt = 0, want a real timestamp")
	}
}

// TestSilentActRequiresBoundReadyDispatch: same gate as send_message — no
// active dispatch, or one still locked/noting, must refuse. "No debilites
// el gate" (the contract's own caution) — silence must not become a way to
// skip the unlock/remember checkpoint.
func TestSilentActRequiresBoundReadyDispatch(t *testing.T) {
	gate := NewGate()
	_, srv, ctx := serverWithGate(t, gate)
	termCtx := withTerminalID(ctx, "term-silent-locked")

	out := callTool(t, termCtx, srv, "silent_act", map[string]any{})
	if !strings.Contains(out, "locked:") {
		t.Errorf("silent_act with no active dispatch = %s, want locked", out)
	}

	chat := "55500000107@c.us"
	if err := gate.RegisterDispatch("nonce-silent-locked", chat, LevelCaution, "term-silent-locked", 0); err != nil {
		t.Fatal(err)
	}
	callTool(t, termCtx, srv, "get_instructions", map[string]any{"nonce": "nonce-silent-locked"})
	// unlock deliberately not called — still locked, not ready.
	out2 := callTool(t, termCtx, srv, "silent_act", map[string]any{})
	if !strings.Contains(out2, "locked:") {
		t.Errorf("silent_act before unlock/skip = %s, want locked", out2)
	}
}

// TestDraftRespectsNoRulesLaw: draft shares validateSend's guardrails, not
// just send_message's — no rules, no draft either.
func TestDraftRespectsNoRulesLaw(t *testing.T) {
	gate := NewGate()
	_, srv, ctx := serverWithGate(t, gate)
	chat := "55500000024@c.us"
	termCtx := bossDispatchContext(t, gate, srv, ctx, chat)
	_, policyVersion := decisionPolicy("")

	out := callTool(t, termCtx, srv, "draft", map[string]any{"to": chat, "message": "borrador", "model": "m", "policy_version": policyVersion})
	if !strings.Contains(out, "no rules on this chat") {
		t.Errorf("draft on a chat with no rules = %s, want it blocked", out)
	}
}

// TestDraftRequiresCurrentPolicyVersion is the regression test for the
// Medium finding in the F4c audit: draft's own description claims "same
// guardrails as send_message", but it never actually required
// policy_version. Now both share validateSend's check.
func TestDraftRequiresCurrentPolicyVersion(t *testing.T) {
	gate := NewGate()
	st, srv, ctx := serverWithGate(t, gate)
	chat := "55500000108@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDefaultRules("responder normalmente"); err != nil {
		t.Fatal(err)
	}
	termCtx := bossDispatchContext(t, gate, srv, ctx, chat)

	t.Run("missing policy_version rejected", func(t *testing.T) {
		out := callTool(t, termCtx, srv, "draft", map[string]any{"to": chat, "message": "borrador", "model": "m"})
		if !strings.Contains(out, "policy_version") {
			t.Errorf("draft with no policy_version = %s, want a policy_version error", out)
		}
	})
	t.Run("stale policy_version rejected", func(t *testing.T) {
		out := callTool(t, termCtx, srv, "draft", map[string]any{"to": chat, "message": "borrador", "model": "m", "policy_version": "stale"})
		if !strings.Contains(out, "stale/missing policy_version") {
			t.Errorf("draft with a stale policy_version = %s, want it rejected", out)
		}
	})
}
