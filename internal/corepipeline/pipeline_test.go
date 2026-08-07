package corepipeline

import (
	"path/filepath"
	"testing"
	"time"

	"piumy-gateway/internal/eventbus"
	"piumy-gateway/internal/gateway"
	"piumy-gateway/internal/governor"
	"piumy-gateway/internal/router"
	"piumy-gateway/internal/state"
	"piumy-gateway/internal/store"
)

// testConfig keeps every anti-ban delay tiny so tests don't pay real
// wall-clock time for them; the defaults are exercised separately via
// TestDefaultConfigFallsBack.
func testConfig() Config {
	return Config{
		OutboxPoll:       time.Hour, // tests call processOutbox directly, never via the ticker
		OutboxMaxRetry:   3,
		DispatchDelayMin: time.Millisecond,
		DispatchDelayMax: 2 * time.Millisecond,
		ReadDelayMin:     time.Millisecond,
		ReadDelayMax:     2 * time.Millisecond,
		ComposingMin:     time.Millisecond,
		ComposingMax:     2 * time.Millisecond,
	}
}

func newTestPipeline(t *testing.T) (*store.Store, *router.Manager, *governor.Limiter, *fakeGateway, *Pipeline) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "piumy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	rt := router.NewManager(filepath.Join(t.TempDir(), "router.json"))
	gov := governor.NewLimiter(100, time.Minute)
	sm := state.NewManager(filepath.Join(t.TempDir(), "status.json"), 8)
	fgw := newFakeGateway()

	p := New(fgw, st, rt, gov, sm, testConfig())
	return st, rt, gov, fgw, p
}

func TestHandleInboundStoresRoutesTouchesAndUpdatesState(t *testing.T) {
	st, rt, _, _, p := newTestPipeline(t)
	if err := rt.Update(func(c *router.Config) { c.AllowAll = true }); err != nil {
		t.Fatal(err)
	}
	// CountPendingDedicated (what feeds state.Queue) only counts active chats
	// in "dedicated" mode (ct-2026-07-21-1853) — set both before the chat
	// exists, TouchChat's upsert never overwrites mode on conflict (F1a).
	if err := st.SetMode("111@c.us", "dedicated"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetActive("111@c.us", true); err != nil {
		t.Fatal(err)
	}

	p.handleInbound(gateway.Inbound{
		ChatJID:   "111@c.us",
		SenderJID: "111@c.us",
		MsgID:     "m1",
		Text:      "hola",
		Type:      "text",
		TS:        100,
		PushName:  "Ana",
	})

	msgs, err := st.GetMessages("111@c.us", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Text != "hola" {
		t.Fatalf("got messages=%+v, want the inbound message stored", msgs)
	}

	c, ok, err := st.GetChat("111@c.us")
	if err != nil || !ok {
		t.Fatalf("GetChat: ok=%v err=%v", ok, err)
	}
	if c.Name != "Ana" {
		t.Errorf("chat name = %q, want Ana (TouchChat with PushName)", c.Name)
	}

	snap := pipelineState(p).Snapshot()
	if snap.Queue != 1 || snap.LastMsg != "hola" {
		t.Errorf("state snapshot = %+v, want Queue=1 LastMsg=hola", snap)
	}
	if snap.Mood != "new_msg" {
		t.Errorf("mood = %q, want new_msg (non-VIP sender)", snap.Mood)
	}
}

// TestHandleInboundPropagatesReplyAndForwarded covers ct-2026-07-21-1610
// (S6a backend): handleInbound must carry gateway.Inbound's
// QuotedID/QuotedPreview/Forwarded through to the stored message, not just
// the fields TestHandleInboundStoresRoutesTouchesAndUpdatesState already
// covers.
func TestHandleInboundPropagatesReplyAndForwarded(t *testing.T) {
	st, rt, _, _, p := newTestPipeline(t)
	if err := rt.Update(func(c *router.Config) { c.AllowAll = true }); err != nil {
		t.Fatal(err)
	}

	p.handleInbound(gateway.Inbound{
		ChatJID: "111@c.us", SenderJID: "111@c.us", MsgID: "m1",
		Text: "respuesta", Type: "text", TS: 100,
		QuotedID: "QUOTED1", QuotedPreview: "mensaje original", Forwarded: true,
	})

	msgs, err := st.GetMessages("111@c.us", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if msgs[0].QuotedID != "QUOTED1" || msgs[0].QuotedPreview != "mensaje original" || !msgs[0].Forwarded {
		t.Errorf("stored message = %+v, want QuotedID=QUOTED1 QuotedPreview='mensaje original' Forwarded=true", msgs[0])
	}
}

// TestHandleInboundDoesNotClobberManualModeOverride is the ST-B regression
// (ct-2026-07-11-0741): handleInbound used to re-apply the router's mode on
// EVERY inbound (SetMode, unconditional) — an owner/agent's deliberate
// set_mode/escalate call got silently reverted by the very next message
// from that chat. SyncRouterMode is a no-op once mode_source is 'manual'.
func TestHandleInboundDoesNotClobberManualModeOverride(t *testing.T) {
	st, rt, _, _, p := newTestPipeline(t)
	if err := rt.Update(func(c *router.Config) { c.AllowAll = true; c.DefaultMode = "dedicated" }); err != nil {
		t.Fatal(err)
	}
	chat := "222@c.us"
	// The owner/agent explicitly chose "auto" — the OPPOSITE of what the
	// router would resolve ("dedicated") — via set_mode/escalate/REST (all
	// of which call store.SetMode, never SyncRouterMode).
	if err := st.SetMode(chat, "auto"); err != nil {
		t.Fatal(err)
	}

	p.handleInbound(gateway.Inbound{
		ChatJID: chat, SenderJID: chat, MsgID: "m1", Text: "hola", Type: "text", TS: 100,
	})

	c, ok, err := st.GetChat(chat)
	if err != nil || !ok {
		t.Fatalf("GetChat: ok=%v err=%v", ok, err)
	}
	if c.Mode != "auto" {
		t.Errorf("Mode after inbound = %q, want %q (manual override must survive the router mirror)", c.Mode, "auto")
	}
	if c.ModeSource != "manual" {
		t.Errorf("ModeSource after inbound = %q, want %q (unchanged by the no-op sync)", c.ModeSource, "manual")
	}
}

// TestHandleInboundDoesNotOverwriteGroupNameWithSenderPushName is the
// ct-2026-07-10-1758 regression (boss escalation from the ct-2026-07-10-1656
// smoke test): msg.PushName is the SENDER's own display name, not the
// group's — passing it straight to TouchChat for a group JID clobbered the
// real group name (seeded by whatsmeow's seedGroups/GetJoinedGroups) with
// whoever happened to send the next message ("QUELENTARO INFORMADO" ->
// "Bakery" in the real smoke test). The 1:1 case (pushname DOES set the
// chat name) is already covered by
// TestHandleInboundStoresRoutesTouchesAndUpdatesState above.
func TestHandleInboundDoesNotOverwriteGroupNameWithSenderPushName(t *testing.T) {
	st, rt, _, _, p := newTestPipeline(t)
	if err := rt.Update(func(c *router.Config) { c.AllowAll = true }); err != nil {
		t.Fatal(err)
	}
	group := "555000000000000001@g.us"
	// Simulates seedGroups having already touched this chat with the real
	// group name at connect time, before any message ever arrived.
	if err := st.TouchChat(group, "Grupo De Prueba", 1); err != nil {
		t.Fatal(err)
	}

	p.handleInbound(gateway.Inbound{
		ChatJID:   group,
		SenderJID: "111@s.whatsapp.net",
		MsgID:     "m1",
		Text:      "hola",
		Type:      "text",
		TS:        100,
		PushName:  "Bakery", // the SENDER's own pushname, not the group's
	})

	c, ok, err := st.GetChat(group)
	if err != nil || !ok {
		t.Fatalf("GetChat: ok=%v err=%v", ok, err)
	}
	if c.Name != "Grupo De Prueba" {
		t.Errorf("group chat name after an inbound message = %q, want the seeded group name (Grupo De Prueba) left untouched by the sender's pushname", c.Name)
	}
}

// TestHandleInboundAppliesRouterMode: a fresh inbound chat must adopt the
// router's resolved mode with NO manual SetMode. This was the F5-smoke wiring
// gap — handleInbound resolved dec.Mode but discarded it, so every new chat
// stayed at the schema default 'auto' and capipush (dedicated-only) never
// dispatched it. The two JIDs prove the mode is sourced from the router, not
// hardcoded: a 'dedicated' route lands dedicated, an 'auto' route lands auto.
func TestHandleInboundAppliesRouterMode(t *testing.T) {
	st, rt, _, _, p := newTestPipeline(t)
	if err := rt.Update(func(c *router.Config) {
		c.AllowAll = true
		c.Routes = []router.Route{
			{Match: "ded@c.us", Mode: "dedicated"},
			{Match: "au@c.us", Mode: "auto"},
		}
	}); err != nil {
		t.Fatal(err)
	}

	p.handleInbound(gateway.Inbound{ChatJID: "ded@c.us", MsgID: "m1", Text: "hola", TS: 1})
	p.handleInbound(gateway.Inbound{ChatJID: "au@c.us", MsgID: "m2", Text: "hola", TS: 2})

	for _, tc := range []struct{ jid, want string }{
		{"ded@c.us", "dedicated"},
		{"au@c.us", "auto"},
	} {
		c, ok, err := st.GetChat(tc.jid)
		if err != nil || !ok {
			t.Fatalf("GetChat %s: ok=%v err=%v", tc.jid, ok, err)
		}
		if c.Mode != tc.want {
			t.Errorf("chat %s mode = %q, want %q (router decision applied by handleInbound)", tc.jid, c.Mode, tc.want)
		}
	}
}

// sm exposes the *state.Manager a test-built Pipeline holds, without adding
// an exported getter to the production type just for tests.
func pipelineState(p *Pipeline) *state.Manager { return p.state }

func TestHandleInboundRespectsRouterGate(t *testing.T) {
	st, _, _, _, p := newTestPipeline(t)
	// Fresh router.Manager on an empty path defaults to whitelist-only,
	// nothing whitelisted — Resolve(...).Allowed is false for everyone.

	p.handleInbound(gateway.Inbound{ChatJID: "999@c.us", MsgID: "m1", Text: "hola", TS: 1})

	msgs, err := st.GetMessages("999@c.us", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("got %d messages for a not-allowed chat, want 0", len(msgs))
	}
}

// TestHandleInboundBypassesRouterGateForBossChat is T30 (ct-2026-08-06-0159,
// boss verbatim: "el criterio de salida tiene que alinearse con el de
// entrada") — same empty-whitelist router as TestHandleInboundRespectsRouterGate
// above, but the chat is already known as the owner's own (is_boss=1): the
// message must still be stored. Before this, the owner's own inbound
// messages were silently dropped by the exact same gate that (correctly)
// filters everyone else — no store write, no log, nothing to show for it.
func TestHandleInboundBypassesRouterGateForBossChat(t *testing.T) {
	st, _, _, _, p := newTestPipeline(t)
	if err := st.SetIsBoss("boss@c.us", true); err != nil {
		t.Fatal(err)
	}

	p.handleInbound(gateway.Inbound{ChatJID: "boss@c.us", MsgID: "m1", Text: "hola", TS: 1})

	msgs, err := st.GetMessages("boss@c.us", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Text != "hola" {
		t.Fatalf("got messages=%+v for the owner's own chat, want the message stored despite the empty whitelist", msgs)
	}
}

func TestHandleInboundReactsVIPForWhitelistedSender(t *testing.T) {
	_, rt, _, _, p := newTestPipeline(t)
	const vip = "111@c.us"
	if err := rt.Update(func(c *router.Config) {
		c.AllowAll = true
		c.Whitelist = []string{vip} // whitelist membership alone confers VIP (router.IsVIP)
	}); err != nil {
		t.Fatal(err)
	}

	p.handleInbound(gateway.Inbound{ChatJID: vip, MsgID: "m1", Text: "hola jefe", TS: 1})

	if got := pipelineState(p).Snapshot().Mood; got != "vip" {
		t.Errorf("mood for whitelisted sender = %q, want vip", got)
	}
}

func TestHandleInboundPublishesOnEventbus(t *testing.T) {
	_, rt, _, _, p := newTestPipeline(t)
	if err := rt.Update(func(c *router.Config) { c.AllowAll = true }); err != nil {
		t.Fatal(err)
	}
	bus := eventbus.New()
	p.SetBus(bus)
	ch, unsubscribe := bus.Subscribe()
	defer unsubscribe()

	p.handleInbound(gateway.Inbound{ChatJID: "111@c.us", MsgID: "m1", Text: "hola", TS: 42})

	select {
	case e := <-ch:
		if e.JID != "111@c.us" || e.TS != 42 {
			t.Errorf("event = %+v, want jid=111@c.us ts=42", e)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the eventbus nudge")
	}
}

// TestMarkReadSkipsWhenKillSwitchActive is the M1 hardening regression
// (escalated into ct-2026-07-10-0540 by Citrino's audit of H6: a
// LoggedOut/TemporaryBan trips the kill switch, but before this fix
// MarkRead's background goroutine only checked ctx.Err() — read receipts
// kept going out to WhatsApp even while "everything" was supposed to be
// stopped). Uses newTestPipeline's own governor (gov.SetKill covers the
// shared killSwitchActive() check — already exercised for both halves,
// gov.Killed/state.Muted, by outbox_test.go's own tests).
func TestMarkReadSkipsWhenKillSwitchActive(t *testing.T) {
	st, _, gov, fgw, p := newTestPipeline(t)
	chat := "111@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m1", FromMe: false, Text: "hola", TS: 1}); err != nil {
		t.Fatal(err)
	}
	gov.SetKill(true)

	p.MarkRead(chat, []store.Message{{ChatJID: chat, ID: "m1", FromMe: false, ReadTS: 0}})

	// MarkRead's real work happens in a background goroutine after
	// readDelay().Sleep (testConfig: 1-2ms) — 50ms is a generous margin
	// without actually waiting on anything to poll for (a negative
	// assertion: nothing should have happened).
	time.Sleep(50 * time.Millisecond)

	if calls := fgw.markReadCalls(); len(calls) != 0 {
		t.Errorf("gw.MarkRead calls while the kill switch is active = %+v, want none", calls)
	}
	msgs, err := st.GetMessages(chat, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].ReadTS != 0 {
		t.Errorf("messages after MarkRead while killed = %+v, want ReadTS still 0 (never persisted)", msgs)
	}
}

// TestDefaultConfigFallsBack covers the anti-ban invariant: an unwired
// (zero-value) Config must never mean "instant" or "no retry cap" — every
// field falls back to a positive default.
func TestDefaultConfigFallsBack(t *testing.T) {
	cfg := defaultConfig(Config{})
	if cfg.OutboxPoll <= 0 || cfg.OutboxMaxRetry <= 0 {
		t.Errorf("zero Config: OutboxPoll=%v OutboxMaxRetry=%d, want both positive", cfg.OutboxPoll, cfg.OutboxMaxRetry)
	}
	if cfg.DispatchDelayMin <= 0 || cfg.DispatchDelayMax < cfg.DispatchDelayMin {
		t.Errorf("zero Config: DispatchDelayMin=%v Max=%v, want positive and Max>=Min", cfg.DispatchDelayMin, cfg.DispatchDelayMax)
	}
	if cfg.ReadDelayMin <= 0 || cfg.ReadDelayMax < cfg.ReadDelayMin {
		t.Errorf("zero Config: ReadDelayMin=%v Max=%v, want positive and Max>=Min", cfg.ReadDelayMin, cfg.ReadDelayMax)
	}
	if cfg.ComposingMin <= 0 || cfg.ComposingMax < cfg.ComposingMin {
		t.Errorf("zero Config: ComposingMin=%v Max=%v, want positive and Max>=Min", cfg.ComposingMin, cfg.ComposingMax)
	}
}
