package corepipeline

import (
	"path/filepath"
	"testing"
	"time"

	"piumy-gateway/internal/gateway"
	"piumy-gateway/internal/governor"
	"piumy-gateway/internal/router"
	"piumy-gateway/internal/state"
	"piumy-gateway/internal/store"
)

func TestControllerRunsPipelineEndToEnd(t *testing.T) {
	st, rt, _, fgw, p := newTestPipeline(t)
	if err := rt.Update(func(c *router.Config) { c.AllowAll = true }); err != nil {
		t.Fatal(err)
	}
	c := NewController(fgw, p)

	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer c.Stop()

	if !c.Status().Connected {
		t.Error("Status().Connected = false, want true (fakeGateway starts connected)")
	}

	fgw.inbound <- gateway.Inbound{ChatJID: "111@c.us", MsgID: "m1", Text: "hola", TS: 1}

	deadline := time.Now().Add(2 * time.Second)
	for {
		msgs, err := st.GetMessages("111@c.us", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("inbound message never reached the store through Controller.Start's pipeline")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestControllerStartStopIdempotent(t *testing.T) {
	_, _, _, fgw, p := newTestPipeline(t)
	c := NewController(fgw, p)

	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil { // second Start: no-op, not an error
		t.Fatal(err)
	}

	c.Stop()
	c.Stop() // second Stop: no-op, must not hang or panic
}

func TestControllerMarkReadNoOpWhenNotRunning(t *testing.T) {
	_, _, _, fgw, p := newTestPipeline(t)
	c := NewController(fgw, p)
	// Never started — MarkRead must return immediately, not panic on a nil
	// run context or block forever.
	c.MarkRead("111@c.us", nil)
}

// TestControllerStopWaitsForInFlightOutboxSend is a regression test: Run
// used to launch outboxLoop in its own untracked goroutine, so Stop() could
// return while a processOutbox call was still mid-flight — its trailing
// store writes (MarkSent/AddMessage) then race a caller's store.Close()
// right after Stop() returns ("database is closed", the flaky seen in
// openwa's TestEndToEndWithRealPipeline). Fixed by 6b326dc's sync.WaitGroup
// in Run (waits for outboxLoop too, not just inboundLoop) — verified still
// intact here.
//
// ct-2026-07-18-1507: this test itself used to synchronize with a FIXED
// time.Sleep(20ms) before calling Stop(), gambling that outboxLoop's
// ticker + the dispatch/composing pacing delays had reliably reached
// gw.Send by then. Under scheduler load that gamble occasionally lost —
// Stop() was invoked before Send had even started, so it correctly had
// nothing to join and returned fast, which the test misread as "the join
// is broken" (characterized flaky via -count=5, Citrino). fakeGateway's
// sendStarted channel now signals the INSTANT Send is entered, so the test
// waits on a real event instead of guessing a duration — deterministic,
// no production code changed (Run/Stop's join was already correct).
func TestControllerStopWaitsForInFlightOutboxSend(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "piumy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rt := router.NewManager(filepath.Join(t.TempDir(), "router.json"))
	gov := governor.NewLimiter(100, time.Minute)
	sm := state.NewManager(filepath.Join(t.TempDir(), "status.json"), 8)
	fgw := newFakeGateway()
	const sendDelay = 60 * time.Millisecond
	fgw.setSendDelay(sendDelay)
	sendStarted := make(chan struct{}, 1)
	fgw.setSendStarted(sendStarted)

	if err := st.Enqueue("111@c.us", "hola", time.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	p := New(fgw, st, rt, gov, sm, Config{
		OutboxPoll:       5 * time.Millisecond,
		DispatchDelayMin: time.Millisecond, DispatchDelayMax: 2 * time.Millisecond,
		ReadDelayMin: time.Millisecond, ReadDelayMax: 2 * time.Millisecond,
		ComposingMin: time.Millisecond, ComposingMax: 2 * time.Millisecond,
	})
	c := NewController(fgw, p)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}

	// Wait for outboxLoop to actually be blocked inside gw.Send's artificial
	// delay before we try to stop it — a real event, not a timing guess.
	select {
	case <-sendStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("outbox Send was never entered")
	}

	stopped := make(chan struct{})
	go func() {
		c.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
		t.Fatal("Stop() returned before the in-flight Send finished — outboxLoop isn't joined")
	case <-time.After(sendDelay / 2):
		// Still blocked partway through the delay, as expected.
	}

	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() never returned")
	}
}

func TestControllerResumeIsStart(t *testing.T) {
	_, _, _, fgw, p := newTestPipeline(t)
	c := NewController(fgw, p)
	if err := c.Resume(); err != nil {
		t.Fatal(err)
	}
	defer c.Stop()
	if !c.Status().Connected {
		t.Error("Status().Connected = false after Resume(), want true")
	}
}
