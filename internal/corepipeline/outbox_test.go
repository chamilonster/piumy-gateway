package corepipeline

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"piumy-gateway/internal/governor"
	"piumy-gateway/internal/router"
	"piumy-gateway/internal/state"
	"piumy-gateway/internal/store"
)

func TestProcessOutboxSendsMarksSentAndRecordsSentRow(t *testing.T) {
	st, _, _, fgw, p := newTestPipeline(t)
	if err := st.Enqueue("111@c.us", "hola", 1); err != nil {
		t.Fatal(err)
	}

	p.processOutbox(context.Background())

	if calls := fgw.sentCalls(); len(calls) != 1 || calls[0].toJID != "111@c.us" || calls[0].text != "hola" {
		t.Fatalf("fakeGateway.sent = %+v, want exactly one call to 111@c.us", calls)
	}
	pending, err := st.PendingOutbox(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("PendingOutbox after a successful send = %+v, want empty (MarkSent)", pending)
	}
	msgs, err := st.GetMessages("111@c.us", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || !msgs[0].FromMe || msgs[0].ID != "fake-1" {
		t.Fatalf("got messages=%+v, want the sent row recorded with the fake's real MsgID", msgs)
	}
}

// TestProcessOutboxMetersUsageOnSuccessfulSend is the ST-D regression
// (ct-2026-07-11-074139): processOutbox is the ONE real-send choke point
// (send_message/draft-approved/autoreply-auto-send/a plain dashboard
// Enqueue ALL just queue into the same outbox table — nothing sends
// directly) — usage.AddUsage now fires exactly here, after a confirmed
// send, so "usage" means "what actually left via WhatsApp", not "what an
// agent attempted". Uses plain Enqueue (no model — the human/dashboard
// case) precisely because it's the one origin that never had any metering
// anywhere before this fix, proving the fix isn't piggy-backing on
// something else.
func TestProcessOutboxMetersUsageOnSuccessfulSend(t *testing.T) {
	st, _, _, _, p := newTestPipeline(t)
	if err := st.Enqueue("111@c.us", "hola mundo", 1); err != nil {
		t.Fatal(err)
	}

	p.processOutbox(context.Background())

	u, err := st.UsageForDay("111@c.us", store.Today())
	if err != nil {
		t.Fatal(err)
	}
	if u.OutChars != len("hola mundo") || u.Messages != 1 {
		t.Errorf("usage after a successful outbox send = %+v, want out_chars=%d messages=1", u, len("hola mundo"))
	}
}

// TestProcessOutboxMetersAutoreplyOriginatedSend covers the autoreply path
// specifically: EnqueueWithModel is exactly what autoreply.Worker.draftFor
// calls for its auto-send branch (NeedsConfirmation == false) — same outbox
// row shape as every other origin, so the SAME processOutbox metering
// covers it with no autoreply-side change needed.
func TestProcessOutboxMetersAutoreplyOriginatedSend(t *testing.T) {
	st, _, _, _, p := newTestPipeline(t)
	if err := st.EnqueueWithModel("222@c.us", "dale, te ayudo", 1, "auto"); err != nil {
		t.Fatal(err)
	}

	p.processOutbox(context.Background())

	u, err := st.UsageForDay("222@c.us", store.Today())
	if err != nil {
		t.Fatal(err)
	}
	if u.OutChars != len("dale, te ayudo") || u.Messages != 1 {
		t.Errorf("usage after an autoreply-originated send = %+v, want out_chars=%d messages=1", u, len("dale, te ayudo"))
	}
}

// TestProcessOutboxMetersApprovedDraft covers the approve_draft path: a
// draft holds NO usage on its own (see mcpserver's draft tool — a
// discarded draft must never count), only ApproveDraft's EnqueueWithModel
// followed by a real send should.
func TestProcessOutboxMetersApprovedDraft(t *testing.T) {
	st, _, _, _, p := newTestPipeline(t)
	if err := st.AddDraft("333@c.us", "aprobado por el dueño", "auto", 1); err != nil {
		t.Fatal(err)
	}
	drafts, err := st.PendingDrafts(10)
	if err != nil || len(drafts) != 1 {
		t.Fatalf("setup: PendingDrafts = %+v, err=%v", drafts, err)
	}
	if _, _, ok, err := st.ApproveDraft(drafts[0].ID, "", 2); err != nil || !ok {
		t.Fatalf("setup: ApproveDraft ok=%v err=%v", ok, err)
	}

	// Before the real send, approving alone must not have metered anything.
	preSend, err := st.UsageForDay("333@c.us", store.Today())
	if err != nil {
		t.Fatal(err)
	}
	if preSend.Messages != 0 {
		t.Fatalf("usage right after ApproveDraft, before any real send = %+v, want zero", preSend)
	}

	p.processOutbox(context.Background())

	u, err := st.UsageForDay("333@c.us", store.Today())
	if err != nil {
		t.Fatal(err)
	}
	if u.OutChars != len("aprobado por el dueño") || u.Messages != 1 {
		t.Errorf("usage after the approved draft actually sent = %+v, want out_chars=%d messages=1", u, len("aprobado por el dueño"))
	}
}

// TestProcessOutboxDoesNotMeterOnFailedSend: a failed send (still queued
// for retry, never actually left) must not count as output.
func TestProcessOutboxDoesNotMeterOnFailedSend(t *testing.T) {
	st, _, _, fgw, p := newTestPipeline(t)
	fgw.setSendErr(errFakeSend)
	if err := st.Enqueue("444@c.us", "hola", 1); err != nil {
		t.Fatal(err)
	}

	p.processOutbox(context.Background())

	u, err := st.UsageForDay("444@c.us", store.Today())
	if err != nil {
		t.Fatal(err)
	}
	if u.Messages != 0 {
		t.Errorf("usage after a FAILED send = %+v, want zero — it never actually left", u)
	}
}

func TestProcessOutboxNoOpWhenDisconnected(t *testing.T) {
	st, _, _, fgw, p := newTestPipeline(t)
	fgw.setConnected(false)
	if err := st.Enqueue("111@c.us", "hola", 1); err != nil {
		t.Fatal(err)
	}

	p.processOutbox(context.Background())

	if calls := fgw.sentCalls(); len(calls) != 0 {
		t.Fatalf("fakeGateway.sent = %+v, want none while disconnected", calls)
	}
	pending, err := st.PendingOutbox(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("got %d pending, want the item still queued (never attempted)", len(pending))
	}
}

func TestProcessOutboxKillSwitchSkipsEverything(t *testing.T) {
	st, _, gov, fgw, p := newTestPipeline(t)
	gov.SetKill(true)
	if err := st.Enqueue("111@c.us", "hola", 1); err != nil {
		t.Fatal(err)
	}

	p.processOutbox(context.Background())

	if calls := fgw.sentCalls(); len(calls) != 0 {
		t.Fatalf("fakeGateway.sent = %+v, want none — kill switch is active", calls)
	}
}

// TestProcessOutboxMutedAloneSkipsEverything is the H2+H3 hardening
// regression (ct-2026-07-10-0540): state.Muted alone — without
// gov.Killed() — must also halt the outbox drain. Before this fix the kill
// switch tool/endpoint flipped both flags but processOutbox only ever
// checked the governor's, so a divergence between the two silently kept
// sending. newTestPipeline doesn't expose the state.Manager it builds
// internally, so this test wires its own pipeline directly (same pieces,
// same testConfig) instead of extending that helper's return tuple across
// its 16 other call sites for one test.
func TestProcessOutboxMutedAloneSkipsEverything(t *testing.T) {
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

	if err := sm.SetMuted(true); err != nil {
		t.Fatal(err)
	}
	if err := st.Enqueue("111@c.us", "hola", 1); err != nil {
		t.Fatal(err)
	}

	p.processOutbox(context.Background())

	if calls := fgw.sentCalls(); len(calls) != 0 {
		t.Fatalf("fakeGateway.sent = %+v, want none — state.Muted alone must halt the drain", calls)
	}
}

// TestKillSwitchSurvivesRestartAndReallyBlocksSending is T19's own
// acceptance criterion (ct-2026-08-05-1249, Citrino verbatim: "verificá
// que de verdad no manda: no alcanza con que el estado diga que sí").
// Simulates an actual restart: the kill switch was persisted to the store
// BEFORE this test's gov/sm ever existed (a fresh governor.Limiter/
// state.Manager, exactly like a real process boot has zero prior
// in-memory state) — then applies the SAME two-flag restore main.go's
// restoreKillSwitch does (governor.SetKill + state.SetMuted from the
// persisted flag), and only THEN builds the pipeline and asks it to
// actually drain a real queued message. Proof is fakeGateway.sentCalls()
// being empty — not gov.Killed()/sm.Snapshot().Muted reading true, which
// TestRestoreKillSwitchAppliesBothFlagsTogether (main_test.go) already
// covers on its own and would pass even if processOutbox's own gate were
// broken.
func TestKillSwitchSurvivesRestartAndReallyBlocksSending(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "piumy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	// The kill switch was set in a PREVIOUS run — all that survives is
	// this persisted flag, same as store.SetSettingBool(store.
	// SettingKillSwitch, true) from set_kill_switch (mcpserver/restapi).
	if err := st.SetSettingBool(store.SettingKillSwitch, true); err != nil {
		t.Fatal(err)
	}

	// "Restart": brand-new governor.Limiter/state.Manager, never told
	// about the kill switch yet — this is the exact in-memory state a real
	// process has the instant after boot, before restoreKillSwitch runs.
	rt := router.NewManager(filepath.Join(t.TempDir(), "router.json"))
	gov := governor.NewLimiter(100, time.Minute)
	sm := state.NewManager(filepath.Join(t.TempDir(), "status.json"), 8)

	// main.go's restoreKillSwitch, inlined (corepipeline can't import
	// package main) — must run BEFORE the pipeline is even built, same
	// ordering main.go itself follows relative to ctrl.Start().
	if st.SettingBool(store.SettingKillSwitch, false) {
		gov.SetKill(true)
		if err := sm.SetMuted(true); err != nil {
			t.Fatal(err)
		}
	}

	fgw := newFakeGateway()
	p := New(fgw, st, rt, gov, sm, testConfig())
	if err := st.Enqueue("111@c.us", "no debería salir", 1); err != nil {
		t.Fatal(err)
	}

	p.processOutbox(context.Background())

	if calls := fgw.sentCalls(); len(calls) != 0 {
		t.Fatalf("fakeGateway.sent = %+v, want none — a kill switch restored from a persisted flag must block sending exactly like a live one", calls)
	}
	pending, err := st.PendingOutbox(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("PendingOutbox = %+v, want the message still queued (never attempted, not lost)", pending)
	}
}

func TestProcessOutboxRateLimitedDefers(t *testing.T) {
	st, _, gov, fgw, p := newTestPipeline(t)
	gov.SetMax(0) // no tokens ever refill above the floor -> Allow() always false
	if err := st.Enqueue("111@c.us", "hola", 1); err != nil {
		t.Fatal(err)
	}

	p.processOutbox(context.Background())

	if calls := fgw.sentCalls(); len(calls) != 0 {
		t.Fatalf("fakeGateway.sent = %+v, want none — rate-limited", calls)
	}
	pending, err := st.PendingOutbox(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("got %d pending, want the item still queued for the next tick", len(pending))
	}
}

func TestProcessOutboxInvalidJIDMarkedSentWithoutSending(t *testing.T) {
	st, _, _, fgw, p := newTestPipeline(t)
	if err := st.Enqueue("", "hola", 1); err != nil {
		t.Fatal(err)
	}

	p.processOutbox(context.Background())

	if calls := fgw.sentCalls(); len(calls) != 0 {
		t.Fatalf("fakeGateway.sent = %+v, want none — empty JID must never be attempted", calls)
	}
	pending, err := st.PendingOutbox(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("PendingOutbox = %+v, want empty — an invalid JID is marked sent (skipped, not retried)", pending)
	}
}

// TestProcessOutboxSendFailureSetsRetry covers processOutbox wiring into
// retryOrDeadLetter end to end: a failed send bumps retry_count and sets a
// backoff deadline, without immediately dead-lettering (well under
// OutboxMaxRetry=3 from testConfig).
func TestProcessOutboxSendFailureSetsRetry(t *testing.T) {
	st, _, _, fgw, p := newTestPipeline(t)
	fgw.setSendErr(errFakeSend)
	if err := st.Enqueue("111@c.us", "hola", 1); err != nil {
		t.Fatal(err)
	}

	p.processOutbox(context.Background())

	pending, err := st.PendingOutbox(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("got %d pending, want the failed item still queued (not dead-lettered yet)", len(pending))
	}
	if pending[0].RetryCount != 1 || pending[0].NextRetryTS <= 0 || pending[0].DeadLetter {
		t.Errorf("got %+v, want retry_count=1, a future next_retry_ts, dead_letter=false", pending[0])
	}
}

// TestRetryOrDeadLetterDeadLettersAtThreshold unit-tests the threshold
// directly (no need to wait out real backoff across multiple ticks): an
// item already one failure short of cfg.OutboxMaxRetry gets dead-lettered
// on its next failure, and dead-lettered items are excluded from
// PendingOutbox's underlying DueOutbox but stay visible for inspection.
func TestRetryOrDeadLetterDeadLettersAtThreshold(t *testing.T) {
	st, _, _, _, p := newTestPipeline(t) // testConfig().OutboxMaxRetry == 3
	if err := st.Enqueue("111@c.us", "hola", 1); err != nil {
		t.Fatal(err)
	}
	pending, err := st.PendingOutbox(1)
	if err != nil || len(pending) != 1 {
		t.Fatalf("setup: PendingOutbox = %+v, err=%v", pending, err)
	}
	item := pending[0]
	item.RetryCount = 2 // one more failure reaches OutboxMaxRetry=3

	p.retryOrDeadLetter(item, errFakeSend)

	due, err := st.DueOutbox(10, time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("DueOutbox after dead-lettering = %+v, want empty (excluded from the send loop)", due)
	}
	all, err := st.PendingOutbox(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || !all[0].DeadLetter {
		t.Fatalf("PendingOutbox = %+v, want the item still visible with dead_letter=true", all)
	}
}
