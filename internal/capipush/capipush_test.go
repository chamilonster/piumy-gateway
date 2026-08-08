package capipush

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"piumy-gateway/internal/mcpserver"
	"piumy-gateway/internal/router"
	"piumy-gateway/internal/state"
	"piumy-gateway/internal/store"
)

// fakeInjector records every Inject call under a lock (sweepOnce is called
// directly/synchronously by tests, but the lock costs nothing and matches
// the project's own fake-server test convention, e.g. openwa's endtoend_test.go).
type fakeInjector struct {
	mu    sync.Mutex
	calls []string // terminalID per call
	err   error    // returned by every Inject call until cleared via setErr
}

func (f *fakeInjector) Inject(terminalID, from, payload string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, terminalID)
	return f.err
}

func (f *fakeInjector) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeInjector) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func newTestPusher(t *testing.T, routerJSON string) (*store.Store, *router.Manager, *mcpserver.Gate, *fakeInjector, *Pusher) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "piumy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	routerPath := filepath.Join(dir, "router.json")
	if routerJSON == "" {
		routerJSON = `{"allow_all":true,"default_mode":"dedicated"}`
	}
	if err := os.WriteFile(routerPath, []byte(routerJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	rt := router.NewManager(routerPath)
	gate := mcpserver.NewGate()
	inj := &fakeInjector{}

	pusher := New(st, rt, gate, inj, Config{PortFallback: "port-fallback", SwampedAt: 8})
	return st, rt, gate, inj, pusher
}

// dedicate sets jid to dedicated mode AND active — SetMode alone no longer
// suffices post ct-2026-07-21-1853: PendingDedicated also gates on active
// (an unattended chat never reaches the agent, regardless of mode).
func dedicate(t *testing.T, st *store.Store, jid string) {
	t.Helper()
	if err := st.SetMode(jid, "dedicated"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetActive(jid, true); err != nil {
		t.Fatal(err)
	}
}

// TestCoalescingOnePerChat covers "burst -> 1 inyección": several
// unhandled messages in the same chat produce exactly one dispatch/Inject
// call per sweep.
func TestCoalescingOnePerChat(t *testing.T) {
	st, _, _, inj, pusher := newTestPusher(t, "")
	chat := "55500000002@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	for i, txt := range []string{"hola", "sigo esperando", "tercer mensaje"} {
		if err := st.AddMessage(store.Message{ChatJID: chat, ID: string(rune('a' + i)), Text: txt, TS: int64(i + 1)}); err != nil {
			t.Fatal(err)
		}
	}

	pusher.sweepOnce()

	if got := inj.count(); got != 1 {
		t.Errorf("Inject calls for a 3-message burst in one chat = %d, want 1 (coalesced)", got)
	}
}

// TestBackpressureSkipsWhenSwamped covers the "estado swamped" fallback
// (S3, ct-2026-07-30-030948: only RECENT non-boss traffic counts) — at/over
// SwampedAt RECENT non-boss pending messages, capipush stops pushing to
// every non-boss chat this sweep.
func TestBackpressureSkipsWhenSwamped(t *testing.T) {
	st, _, _, inj, pusher := newTestPusher(t, "")
	pusher.cfg.SwampedAt = 2
	now := time.Now().Unix()

	for i, chat := range []string{"55500000004@c.us", "55500000005@c.us", "55500000006@c.us"} {
		if err := st.TouchChat(chat, "C", 1); err != nil {
			t.Fatal(err)
		}
		dedicate(t, st, chat)
		if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m1", Text: "hola", TS: now - int64(i)}); err != nil {
			t.Fatal(err)
		}
	}

	pusher.sweepOnce()

	if got := inj.count(); got != 0 {
		t.Errorf("Inject calls while swamped (3 recent pending >= SwampedAt=2) = %d, want 0", got)
	}
}

// recentNotDebounced is "recent enough" to count toward SwampedAt's default
// 10m window but old enough to clear DispatchDebounce's default 60s silence
// wait — a message at bare time.Now() would get held by debounce instead of
// actually reaching Inject, which is not what these tests are about.
func recentNotDebounced() int64 {
	return time.Now().Add(-90 * time.Second).Unix()
}

// TestBackpressureIgnoresOldDebt is S3's own root-cause regression
// (ct-2026-07-30-030948): the smoke's actual bug — 76 of 82 pending
// messages were months old, in a chat nobody was going to answer, and that
// alone permanently froze the channel. Old debt over the threshold must
// never block dispatch of a genuinely new message elsewhere. freshChat gets
// its own routed terminal (a distinct fakeInjector) so its dispatch can be
// asserted precisely, independent of whatever oldDebtChat does on the
// shared port-fallback.
func TestBackpressureIgnoresOldDebt(t *testing.T) {
	freshChat := "55500000007@c.us"
	cfg := `{"allow_all":true,"default_mode":"dedicated","routes":[{"match":"` + freshChat + `","terminal_id":"fresh-term"}]}`
	st, _, _, _, pusher := newTestPusher(t, cfg)
	pusher.cfg.SwampedAt = 2
	pusher.cfg.SwampedWindow = 10 * time.Minute
	freshInj := &fakeInjector{}
	pusher.RegisterInjector("fresh-term", freshInj)

	oldDebtChat := "55500000008@c.us"
	if err := st.TouchChat(oldDebtChat, "Viejo", 1); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, oldDebtChat)
	old := time.Now().Add(-24 * time.Hour).Unix()
	for i, id := range []string{"m1", "m2", "m3"} {
		if err := st.AddMessage(store.Message{ChatJID: oldDebtChat, ID: id, Text: "vieja deuda", TS: old + int64(i)}); err != nil {
			t.Fatal(err)
		}
	}

	if err := st.TouchChat(freshChat, "Nuevo", 1); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, freshChat)
	if err := st.AddMessage(store.Message{ChatJID: freshChat, ID: "m1", Text: "hola recién", TS: recentNotDebounced()}); err != nil {
		t.Fatal(err)
	}

	pusher.sweepOnce()

	if got := freshInj.count(); got != 1 {
		t.Errorf("fresh chat Inject calls = %d, want 1 — 3 old-debt messages over the threshold must not block a genuinely recent chat", got)
	}
}

// TestBackpressureBossChatNeverThrottled: the boss's own chat dispatches
// even while every other chat is held back by backpressure — the design
// decision's own "el chat del boss nunca se frena" (store.PendingDedicated's
// is_boss bypass, reused here per Citrino's instruction not to invent a
// second criterion).
func TestBackpressureBossChatNeverThrottled(t *testing.T) {
	st, _, _, inj, pusher := newTestPusher(t, "")
	pusher.cfg.SwampedAt = 1
	ts := recentNotDebounced()

	// Enough recent non-boss traffic to trip backpressure on its own.
	swampedChat := "55500000009@c.us"
	if err := st.TouchChat(swampedChat, "C", 1); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, swampedChat)
	if err := st.AddMessage(store.Message{ChatJID: swampedChat, ID: "m1", Text: "hola", TS: ts}); err != nil {
		t.Fatal(err)
	}

	bossChat := "55500000010@c.us"
	if err := st.TouchChat(bossChat, "Boss", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetIsBoss(bossChat, true); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, bossChat)
	if err := st.AddMessage(store.Message{ChatJID: bossChat, ID: "m1", Text: "hola boss", TS: ts}); err != nil {
		t.Fatal(err)
	}

	pusher.sweepOnce()

	if len(inj.calls) != 1 || inj.calls[0] != "port-fallback" {
		t.Errorf("Inject calls = %v, want exactly 1 (the boss chat still dispatched under backpressure)", inj.calls)
	}
}

// TestBackpressureCountsAutoModeChats is the T20 (ct-2026-08-05-1301)
// regression: T5 (ct-2026-08-05-0311) widened PendingDedicated/
// CountPendingDedicated to dispatch 'auto' chats too, but
// CountRecentPendingNonBoss — capipush's OWN backpressure counter, a third
// query with the same mode filter — was never updated (Amatista's R1
// catch). An avalanche of 'auto' chats was invisible to the swamped
// threshold: the counter stayed 0 no matter how much 'auto' traffic piled
// up, so backpressure never tripped and dispatch never throttled.
//
// swampedChat here is 'auto', not 'dedicated' — if the counter still only
// counted 'dedicated' (the pre-fix bug), it would never see this message,
// swamped would stay false, and otherChat would dispatch normally. With
// the fix, the 'auto' message alone crosses SwampedAt=1 and otherChat (a
// plain dedicated, non-boss chat) gets held back — same assertion shape as
// TestBackpressureSkipsWhenSwamped.
func TestBackpressureCountsAutoModeChats(t *testing.T) {
	st, _, _, inj, pusher := newTestPusher(t, "")
	pusher.cfg.SwampedAt = 1
	ts := recentNotDebounced()

	swampedChat := "55500000011@c.us"
	if err := st.TouchChat(swampedChat, "Auto", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetMode(swampedChat, "auto"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetActive(swampedChat, true); err != nil {
		t.Fatal(err)
	}
	if err := st.AddMessage(store.Message{ChatJID: swampedChat, ID: "m1", Text: "avalancha auto", TS: ts}); err != nil {
		t.Fatal(err)
	}

	otherChat := "55500000012@c.us"
	if err := st.TouchChat(otherChat, "Otro", 1); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, otherChat)
	if err := st.AddMessage(store.Message{ChatJID: otherChat, ID: "m1", Text: "hola", TS: ts}); err != nil {
		t.Fatal(err)
	}

	pusher.sweepOnce()

	if got := inj.count(); got != 0 {
		t.Errorf("Inject calls while an 'auto' chat alone crosses SwampedAt=1 = %d, want 0 — the auto chat's traffic must count toward backpressure and throttle the other chat", got)
	}
}

// TestBackpressureReadsThresholdAndWindowFromSettings covers the contract's
// own criterio de listo: SwampedAt/SwampedWindow are settings, not
// hardcode — a live store.SetSettingInt/SetSettingDuration override must
// win over the Config-level fallback, re-read every sweep (no restart
// needed).
func TestBackpressureReadsThresholdAndWindowFromSettings(t *testing.T) {
	st, _, _, inj, pusher := newTestPusher(t, "")
	pusher.cfg.SwampedAt = 100           // Config fallback: effectively never trips
	pusher.cfg.SwampedWindow = time.Hour // Config fallback: wide window

	// Settings override to a threshold of 1 and a narrow 1-minute window.
	if err := st.SetSettingInt(store.SettingCapipushSwampedAt, 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSettingDuration(store.SettingCapipushSwampedWindow, time.Minute); err != nil {
		t.Fatal(err)
	}

	chat := "55500000011@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m1", Text: "hola", TS: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}

	pusher.sweepOnce()

	if got := inj.count(); got != 0 {
		t.Errorf("Inject calls = %d, want 0 — the settings-overridden threshold (1) should have tripped backpressure despite Config.SwampedAt=100", got)
	}
}

// TestBackpressureSignalsStateStatus covers S3's agent-facing signal
// (complementing, not replacing, the S1-style log): state.Status.Backpressure
// / BackpressureReason must reflect entering AND leaving the swamped state.
func TestBackpressureSignalsStateStatus(t *testing.T) {
	st, _, _, _, pusher := newTestPusher(t, "")
	pusher.cfg.SwampedAt = 1
	sm := state.NewManager(filepath.Join(t.TempDir(), "status.json"), 8)
	pusher.SetState(sm)

	chat := "55500000012@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m1", Text: "hola", TS: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}

	pusher.sweepOnce()
	snap := sm.Snapshot()
	if !snap.Backpressure {
		t.Fatal("Status.Backpressure after tripping the gate = false, want true")
	}
	if snap.BackpressureReason == "" {
		t.Error("Status.BackpressureReason = \"\", want an explanation while swamped")
	}

	if err := st.MarkHandled(chat, "m1"); err != nil {
		t.Fatal(err)
	}
	pusher.sweepOnce()
	snap = sm.Snapshot()
	if snap.Backpressure {
		t.Error("Status.Backpressure after draining = true, want false")
	}
	if snap.BackpressureReason != "" {
		t.Errorf("Status.BackpressureReason after draining = %q, want empty", snap.BackpressureReason)
	}
}

// TestDispatchMetersInputUsage covers F4-DESIGN §8's input/dispatch counter.
func TestDispatchMetersInputUsage(t *testing.T) {
	st, _, _, _, pusher := newTestPusher(t, "")
	chat := "55500000013@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m1", Text: "hola mundo", TS: 1}); err != nil {
		t.Fatal(err)
	}

	pusher.sweepOnce()

	u, err := st.UsageForDay(chat, store.Today())
	if err != nil {
		t.Fatal(err)
	}
	if u.InChars != len("hola mundo") || u.Messages != 1 {
		t.Errorf("usage after dispatch = %+v, want in_chars=%d messages=1", u, len("hola mundo"))
	}
}

// TestDailyQuotaBlocksDispatch covers the "cuota de cuenta" hook: at/over
// DailyQuota, sweepOnce dispatches nothing, regardless of pending messages.
func TestDailyQuotaBlocksDispatch(t *testing.T) {
	st, _, _, inj, pusher := newTestPusher(t, "")
	pusher.cfg.Weights = store.UsageWeights{MessageCost: 1}
	pusher.cfg.DailyQuota = 5

	// Pre-existing usage from an unrelated chat already at quota.
	if err := st.AddUsage("other@c.us", store.Today(), store.UsageDelta{Messages: 10}); err != nil {
		t.Fatal(err)
	}

	chat := "55500000014@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m1", Text: "hola", TS: 1}); err != nil {
		t.Fatal(err)
	}

	pusher.sweepOnce()

	if got := inj.count(); got != 0 {
		t.Errorf("Inject calls while over DailyQuota = %d, want 0", got)
	}
}

// TestInFlightTerminalSkipsNewDispatch covers the refinement (not the
// security guarantee, which lives in gate.RegisterDispatch itself): a
// terminal with an in-flight (bound, not done) dispatch doesn't get a new
// one forced on it just because a DIFFERENT chat routed to the same
// terminal has a message due — avoids interrupting legitimate in-progress
// work. Once the first dispatch is consumed, the next sweep picks the
// second chat back up.
func TestInFlightTerminalSkipsNewDispatch(t *testing.T) {
	chatA, chatB := "55500000015@c.us", "55500000016@c.us"
	st, _, gate, inj, pusher := newTestPusher(t, "")
	for _, chat := range []string{chatA, chatB} {
		if err := st.TouchChat(chat, "C", 1); err != nil {
			t.Fatal(err)
		}
		dedicate(t, st, chat)
	}
	if err := st.AddMessage(store.Message{ChatJID: chatA, ID: "m1", Text: "hola", TS: 1}); err != nil {
		t.Fatal(err)
	}

	pusher.sweepOnce() // dispatches chatA to "port-fallback" (both chats share it, no routes)
	if got := inj.count(); got != 1 {
		t.Fatalf("after first sweep, Inject calls = %d, want 1 (chatA)", got)
	}
	if !gate.InFlight("port-fallback") {
		t.Fatal("setup: gate should report the terminal in-flight after dispatching chatA")
	}

	if err := st.AddMessage(store.Message{ChatJID: chatB, ID: "m1", Text: "hola", TS: 2}); err != nil {
		t.Fatal(err)
	}
	pusher.sweepOnce() // chatB is due, but the shared terminal is still in-flight with chatA

	if got := inj.count(); got != 1 {
		t.Errorf("Inject calls while the shared terminal is in-flight = %d, want still 1 (chatB skipped)", got)
	}

	gate.Consume("port-fallback") // chatA's dispatch finishes
	pusher.sweepOnce()            // now chatB should go through

	if got := inj.count(); got != 2 {
		t.Errorf("Inject calls after the terminal freed up = %d, want 2 (chatB dispatched)", got)
	}
}

// TestTerminalIDFromRouteOverridesPortFallback covers the routing
// priority: an explicit per-route terminal_id wins; PortFallback only
// applies when the route defines none.
func TestTerminalIDFromRouteOverridesPortFallback(t *testing.T) {
	chat := "55500000017@c.us"
	cfg := `{"allow_all":true,"default_mode":"dedicated","routes":[{"match":"` + chat + `","terminal_id":"term-explicit"}]}`
	st, _, _, inj, pusher := newTestPusher(t, cfg)
	// Register the same fakeInjector for the routed terminal so its Inject
	// calls are captured (the mapa only has PortFallback by default).
	pusher.RegisterInjector("term-explicit", inj)
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m1", Text: "hola", TS: 1}); err != nil {
		t.Fatal(err)
	}

	pusher.sweepOnce()

	if len(inj.calls) != 1 || inj.calls[0] != "term-explicit" {
		t.Errorf("Inject calls = %v, want exactly one to term-explicit", inj.calls)
	}

	other := "55500000018@c.us"
	if err := st.TouchChat(other, "C2", 1); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, other)
	if err := st.AddMessage(store.Message{ChatJID: other, ID: "m1", Text: "hola", TS: 2}); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkHandled(chat, "m1"); err != nil {
		t.Fatal(err) // chat is done — isolate this sweep to the route-less chat
	}
	pusher.sweepOnce()

	found := false
	for _, id := range inj.calls {
		if id == "port-fallback" {
			found = true
		}
	}
	if !found {
		t.Errorf("Inject calls = %v, want a call to port-fallback for the route-less chat", inj.calls)
	}
}

// TestBossDispatchAlwaysGoesToPrincipalRegardlessOfRoute is
// ct-2026-07-13-0302: is_boss dispatches to the principal (PortFallback)
// unconditionally — a route's own terminal_id (meant for a future
// below-boss "suplente" agent, ct-2026-07-13-0242) must never redirect the
// owner's own messages elsewhere, with zero router.json configuration
// required for that to hold.
func TestBossDispatchAlwaysGoesToPrincipalRegardlessOfRoute(t *testing.T) {
	chat := "55500000019@c.us"
	cfg := `{"allow_all":true,"default_mode":"dedicated","routes":[{"match":"` + chat + `","terminal_id":"suplente-term"}]}`
	st, _, _, inj, pusher := newTestPusher(t, cfg)
	if err := st.TouchChat(chat, "Boss", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetIsBoss(chat, true); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m1", Text: "hola", TS: 1}); err != nil {
		t.Fatal(err)
	}

	pusher.sweepOnce()

	if len(inj.calls) != 1 || inj.calls[0] != "port-fallback" {
		t.Errorf("Inject calls = %v, want exactly one to port-fallback (the principal) — is_boss must ignore the route to suplente-term", inj.calls)
	}
}

// ── M4 (ct-2026-07-22-1301) — agent_exclusive dispatch precedence ─────────
// Boss-ratified order: (1) boss -> principal ALWAYS > (2) agent_exclusive:
// <id> -> that agent, GANA sobre router.json > (3) sin asignar -> router ->
// PortFallback, sin cambios.

// TestAgentExclusiveRoutesToItsAgent: a chat manually assigned via
// agent_exclusive:<id> (M3's own write path, store.AgentExclusiveStatus)
// dispatches to that agent's own registered injector.
func TestAgentExclusiveRoutesToItsAgent(t *testing.T) {
	chat := "55500000020@c.us"
	st, _, _, _, pusher := newTestPusher(t, `{"allow_all":true,"default_mode":"dedicated","routes":[]}`)
	secondary := &fakeInjector{}
	pusher.RegisterInjector("secondary-term", secondary)

	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	if err := st.SetStatus(chat, store.AgentExclusiveStatus("secondary-term")); err != nil {
		t.Fatal(err)
	}
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m1", Text: "hola", TS: 1}); err != nil {
		t.Fatal(err)
	}

	pusher.sweepOnce()

	if len(secondary.calls) != 1 || secondary.calls[0] != "secondary-term" {
		t.Errorf("secondary injector calls = %v, want exactly one to secondary-term", secondary.calls)
	}
}

// TestAgentExclusiveOverridesRouterRoute: agent_exclusive WINS over a
// router.json route configured for a DIFFERENT terminal on the same chat —
// manual assignment (M3) outranks router.json.
func TestAgentExclusiveOverridesRouterRoute(t *testing.T) {
	chat := "55500000021@c.us"
	cfg := `{"allow_all":true,"default_mode":"dedicated","routes":[{"match":"` + chat + `","terminal_id":"router-term"}]}`
	st, _, _, _, pusher := newTestPusher(t, cfg)
	routerInj := &fakeInjector{}
	assignedInj := &fakeInjector{}
	pusher.RegisterInjector("router-term", routerInj)
	pusher.RegisterInjector("assigned-term", assignedInj)

	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	if err := st.SetStatus(chat, store.AgentExclusiveStatus("assigned-term")); err != nil {
		t.Fatal(err)
	}
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m1", Text: "hola", TS: 1}); err != nil {
		t.Fatal(err)
	}

	pusher.sweepOnce()

	if len(assignedInj.calls) != 1 || assignedInj.calls[0] != "assigned-term" {
		t.Errorf("assigned injector calls = %v, want exactly one to assigned-term", assignedInj.calls)
	}
	if routerInj.count() != 0 {
		t.Errorf("router injector was called %d times, want 0 — agent_exclusive must win over router.json", routerInj.count())
	}
}

// TestBossIgnoresAgentExclusive: is_boss ALWAYS dispatches to the principal
// even if the same chat also carries an agent_exclusive assignment —
// precedence (1) outranks (2) unconditionally.
func TestBossIgnoresAgentExclusive(t *testing.T) {
	chat := "55500000022@c.us"
	st, _, _, inj, pusher := newTestPusher(t, `{"allow_all":true,"default_mode":"dedicated","routes":[]}`)
	assignedInj := &fakeInjector{}
	pusher.RegisterInjector("assigned-term", assignedInj)

	if err := st.TouchChat(chat, "Boss", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetIsBoss(chat, true); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	if err := st.SetStatus(chat, store.AgentExclusiveStatus("assigned-term")); err != nil {
		t.Fatal(err)
	}
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m1", Text: "hola", TS: 1}); err != nil {
		t.Fatal(err)
	}

	pusher.sweepOnce()

	if len(inj.calls) != 1 || inj.calls[0] != "port-fallback" {
		t.Errorf("principal injector calls = %v, want exactly one to port-fallback — boss must ignore agent_exclusive", inj.calls)
	}
	if assignedInj.count() != 0 {
		t.Errorf("assigned injector was called %d times, want 0 — boss must never route to the assigned agent", assignedInj.count())
	}
}

// TestAgentExclusiveWithUnregisteredAgentFallsBackToPortFallback is the
// robustness guard Citrino asked for: a chat assigned to an agentID with NO
// real injector registered (e.g. assigned but never actually configured
// with cAPI credentials) must fall back to the NORMAL precedence-3 path
// (router -> PortFallback) — not vanish into injectorFor's own
// LogInjector-skip, which would strand the message forever (every re-sweep
// would resolve to the exact same dead agentID).
func TestAgentExclusiveWithUnregisteredAgentFallsBackToPortFallback(t *testing.T) {
	chat := "55500000023@c.us"
	st, _, _, inj, pusher := newTestPusher(t, `{"allow_all":true,"default_mode":"dedicated","routes":[]}`)
	// "ghost-term" is deliberately NEVER registered via RegisterInjector.

	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	if err := st.SetStatus(chat, store.AgentExclusiveStatus("ghost-term")); err != nil {
		t.Fatal(err)
	}
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m1", Text: "hola", TS: 1}); err != nil {
		t.Fatal(err)
	}

	pusher.sweepOnce()

	if len(inj.calls) != 1 || inj.calls[0] != "port-fallback" {
		t.Errorf("principal injector calls = %v, want exactly one to port-fallback — an unresolvable assignment must fall back, not strand the message", inj.calls)
	}
}

// TestUnregisterInjectorStopsDispatchToOldCredentials is the regression test
// for ct-2026-07-29 (boss: "un borrado que deja las credenciales vivas es un
// borrado que miente"): after UnregisterInjector, a NEW message to a chat
// still assigned to that agentID must NOT reach the old injector — this
// tests the actual dispatch BEHAVIOR post-unregister, not merely that the
// map entry is gone. Falls back to the same precedence-3 path
// TestAgentExclusiveWithUnregisteredAgentFallsBackToPortFallback covers for
// an agent that was never registered in the first place — deleting one
// must land in the identical, already-safe state.
func TestUnregisterInjectorStopsDispatchToOldCredentials(t *testing.T) {
	chat := "55500000024@c.us"
	st, _, _, inj, pusher := newTestPusher(t, `{"allow_all":true,"default_mode":"dedicated","routes":[]}`)
	secondary := &fakeInjector{}
	pusher.RegisterInjector("deleted-term", secondary)

	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	if err := st.SetStatus(chat, store.AgentExclusiveStatus("deleted-term")); err != nil {
		t.Fatal(err)
	}
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m1", Text: "antes de borrar", TS: 1}); err != nil {
		t.Fatal(err)
	}
	pusher.sweepOnce()
	if len(secondary.calls) != 1 || secondary.calls[0] != "deleted-term" {
		t.Fatalf("baseline: secondary injector calls = %v, want exactly one to deleted-term before unregistering", secondary.calls)
	}

	pusher.UnregisterInjector("deleted-term")

	// chats.status still says agent_exclusive:deleted-term — a real delete
	// handler is expected to ALSO clear this (restapi's job), but the
	// dispatch-level guard must hold even if it somehow didn't.
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m2", Text: "despues de borrar", TS: 2}); err != nil {
		t.Fatal(err)
	}
	pusher.sweepOnce()

	if secondary.count() != 1 {
		t.Errorf("secondary injector calls after unregister = %d, want still 1 (the OLD credentials must never receive the new message)", secondary.count())
	}
	if len(inj.calls) != 1 || inj.calls[0] != "port-fallback" {
		t.Errorf("principal injector calls = %v, want exactly one to port-fallback for the post-unregister message", inj.calls)
	}
}

// TestReplyToAgentMessageRoutesEvenFromBossChat is T43's own test
// (ct-2026-08-08-2043): a reply quoting a message an agent sent via
// send_to_boss must reach THAT agent's terminal — even though the chat is
// is_boss, which today (precedence 1) always short-circuits straight to the
// principal. This is the one precedence the boss actually asked for: "si yo
// le respondo a un mensaje de un agente, se le responda a ese terminal".
func TestReplyToAgentMessageRoutesEvenFromBossChat(t *testing.T) {
	chat := "55500000030@c.us"
	st, _, _, principal, pusher := newTestPusher(t, `{"allow_all":true,"default_mode":"dedicated","routes":[]}`)
	agentInj := &fakeInjector{}
	pusher.RegisterInjector("agent-term", agentInj)

	if err := st.TouchChat(chat, "Boss", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetIsBoss(chat, true); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "boss-out", FromMe: true, Text: "[Agente] avisando algo", TS: 1, OriginTerminalID: "agent-term"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "reply1", Text: "gracias, listo", TS: 2, QuotedID: "boss-out"}); err != nil {
		t.Fatal(err)
	}

	pusher.sweepOnce()

	if len(agentInj.calls) != 1 || agentInj.calls[0] != "agent-term" {
		t.Errorf("agent injector calls = %v, want exactly one to agent-term", agentInj.calls)
	}
	if principal.count() != 0 {
		t.Errorf("principal injector was called %d times, want 0 — a reply to an agent's message must never fall through to boss->principal", principal.count())
	}
}

// TestReplyRoutingPerAgentInSameChat is the boss's own scenario verbatim:
// "en un chat puedo tener diferentes destinos, dependiendo a quién le
// respondo" — two different agents wrote into the SAME boss chat, and each
// reply must reach its own author, not always the same one.
func TestReplyRoutingPerAgentInSameChat(t *testing.T) {
	chat := "55500000031@c.us"
	st, _, _, _, pusher := newTestPusher(t, `{"allow_all":true,"default_mode":"dedicated","routes":[]}`)
	agentA := &fakeInjector{}
	agentB := &fakeInjector{}
	pusher.RegisterInjector("agent-a-term", agentA)
	pusher.RegisterInjector("agent-b-term", agentB)

	if err := st.TouchChat(chat, "Boss", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetIsBoss(chat, true); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "boss-out-a", FromMe: true, Text: "[A] mensaje de A", TS: 1, OriginTerminalID: "agent-a-term"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "boss-out-b", FromMe: true, Text: "[B] mensaje de B", TS: 2, OriginTerminalID: "agent-b-term"}); err != nil {
		t.Fatal(err)
	}

	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "reply-to-a", Text: "respuesta para A", TS: 3, QuotedID: "boss-out-a"}); err != nil {
		t.Fatal(err)
	}
	pusher.sweepOnce()
	if len(agentA.calls) != 1 || agentA.calls[0] != "agent-a-term" {
		t.Fatalf("after replying to A: agentA calls = %v, want exactly one to agent-a-term", agentA.calls)
	}
	if agentB.count() != 0 {
		t.Fatalf("after replying to A: agentB was called %d times, want 0", agentB.count())
	}

	// The agent (or its mark_handled call) has already drained reply-to-a —
	// simulate that so the second sweep's burst is just the new reply, not
	// both coalesced into one dispatch.
	if err := st.MarkHandled(chat, "reply-to-a"); err != nil {
		t.Fatal(err)
	}

	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "reply-to-b", Text: "respuesta para B", TS: 4, QuotedID: "boss-out-b"}); err != nil {
		t.Fatal(err)
	}
	pusher.sweepOnce()
	if len(agentB.calls) != 1 || agentB.calls[0] != "agent-b-term" {
		t.Errorf("after replying to B: agentB calls = %v, want exactly one to agent-b-term", agentB.calls)
	}
	if agentA.count() != 1 {
		t.Errorf("after replying to B: agentA was called %d times, want still 1 (must not be re-triggered)", agentA.count())
	}
}

// TestNoQuotedIDUsesTodaysPrecedence: a plain message with no QuotedID is
// completely unaffected by T43 — a boss chat still dispatches straight to
// the principal, exactly as before.
func TestNoQuotedIDUsesTodaysPrecedence(t *testing.T) {
	chat := "55500000032@c.us"
	st, _, _, principal, pusher := newTestPusher(t, `{"allow_all":true,"default_mode":"dedicated","routes":[]}`)

	if err := st.TouchChat(chat, "Boss", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetIsBoss(chat, true); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m1", Text: "hola, sin citar nada", TS: 1}); err != nil {
		t.Fatal(err)
	}

	pusher.sweepOnce()

	if len(principal.calls) != 1 || principal.calls[0] != "port-fallback" {
		t.Errorf("principal injector calls = %v, want exactly one to port-fallback", principal.calls)
	}
}

// TestReplyToNonAgentMessageFallsBackToPrincipal: the quoted message exists
// but has no OriginTerminalID (a normal AI/human reply, not one sent via
// send_to_boss) — nothing to route to, so today's precedence applies.
func TestReplyToNonAgentMessageFallsBackToPrincipal(t *testing.T) {
	chat := "55500000033@c.us"
	st, _, _, principal, pusher := newTestPusher(t, `{"allow_all":true,"default_mode":"dedicated","routes":[]}`)

	if err := st.TouchChat(chat, "Boss", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetIsBoss(chat, true); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "boss-out", FromMe: true, Text: "una respuesta normal"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "reply1", Text: "respondiendo a algo que no mandó un agente", TS: 2, QuotedID: "boss-out"}); err != nil {
		t.Fatal(err)
	}

	pusher.sweepOnce()

	if len(principal.calls) != 1 || principal.calls[0] != "port-fallback" {
		t.Errorf("principal injector calls = %v, want exactly one to port-fallback — quoting a non-agent message must not change routing", principal.calls)
	}
}

// TestReplyToAgentWithoutLiveInjectorFallsBackToPrincipal: the quoted
// message DOES have an OriginTerminalID, but that agent has no injector
// registered (never connected, or connected and gone) — must fall back to
// the principal, not strand the message on a dead terminal_id.
func TestReplyToAgentWithoutLiveInjectorFallsBackToPrincipal(t *testing.T) {
	chat := "55500000034@c.us"
	st, _, _, principal, pusher := newTestPusher(t, `{"allow_all":true,"default_mode":"dedicated","routes":[]}`)
	// "ghost-term" is deliberately NEVER registered via RegisterInjector.

	if err := st.TouchChat(chat, "Boss", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetIsBoss(chat, true); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "boss-out", FromMe: true, Text: "[Fantasma] ya no está", TS: 1, OriginTerminalID: "ghost-term"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "reply1", Text: "respondiendo al fantasma", TS: 2, QuotedID: "boss-out"}); err != nil {
		t.Fatal(err)
	}

	pusher.sweepOnce()

	if len(principal.calls) != 1 || principal.calls[0] != "port-fallback" {
		t.Errorf("principal injector calls = %v, want exactly one to port-fallback — a reply to a departed agent must not get stranded", principal.calls)
	}
}

// TestLevelDerivation covers AGENT-BEHAVIOR.md's level rule: is_boss ->
// boss, is_approver -> approver (Aprobador P1, ct-2026-07-31-0610), a
// never-seen ("new") contact -> danger, anything else -> caution.
func TestLevelDerivation(t *testing.T) {
	cases := []struct {
		name   string
		chat   store.Chat
		expect string
	}{
		{"boss", store.Chat{IsBoss: true, Status: "whitelist"}, mcpserver.LevelBoss},
		{"boss wins over approver", store.Chat{IsBoss: true, IsApprover: true, Status: "whitelist"}, mcpserver.LevelBoss},
		{"approver, known contact", store.Chat{IsApprover: true, Status: "whitelist"}, mcpserver.LevelApprover},
		{"approver, automatic/new contact", store.Chat{IsApprover: true, Status: "new"}, mcpserver.LevelApprover},
		{"new/unknown contact", store.Chat{IsBoss: false, Status: "new"}, mcpserver.LevelDanger},
		{"known contact", store.Chat{IsBoss: false, Status: "whitelist"}, mcpserver.LevelCaution},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := LevelFor(c.chat); got != c.expect {
				t.Errorf("LevelFor(%+v) = %q, want %q", c.chat, got, c.expect)
			}
		})
	}
}

// TestFileInjectorAppendsTabSeparatedLines is FileInjector's own regression
// (ct-2026-07-10-1814, smoke parte 2a): an external test agent parses
// dispatch.jsonl by splitting each line on the first tab — this locks the
// wire format (terminalID\tpayload\n, append-only across calls).
func TestFileInjectorAppendsTabSeparatedLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dispatch.jsonl")
	inj := FileInjector{Path: path}

	if err := inj.Inject("term-a", "111, boss", "ct1"); err != nil {
		t.Fatal(err)
	}
	if err := inj.Inject("term-b", "222, danger", "ct2"); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "term-a\tct1\nterm-b\tct2\n"
	if string(data) != want {
		t.Errorf("dispatch.jsonl content = %q, want %q", string(data), want)
	}
}

func TestNewInjectorNilFallsBackToLogInjector(t *testing.T) {
	st, rt, gate, _, _ := newTestPusher(t, "")
	p := New(st, rt, gate, nil, Config{PortFallback: "pf"})
	if _, ok := p.injectorFor("pf").(LogInjector); !ok {
		t.Error("New with nil injector: want LogInjector at PortFallback")
	}
}

// TestDispatchRevertsGateRegistrationWhenInjectFails is the H5 hardening
// regression (ct-2026-07-10-0540): before this fix, a failed Inject left
// the dispatch registered in the gate with zero chance of ever being
// pulled — a wedge, InFlight(terminalID) stayed true forever (until the
// gate's own hourly stale sweep eventually reclaimed it), and every future
// sweep kept skipping the chat because the terminal looked in-flight.
// CancelDispatch undoes the registration immediately so the next sweep
// retries normally.
func TestDispatchRevertsGateRegistrationWhenInjectFails(t *testing.T) {
	st, _, gate, inj, pusher := newTestPusher(t, "")
	inj.setErr(errors.New("inject failed"))
	chat := "55500000019@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m1", Text: "hola", TS: 1}); err != nil {
		t.Fatal(err)
	}

	pusher.sweepOnce()

	if gate.InFlight("port-fallback") {
		t.Error("gate.InFlight after a failed Inject = true, want false — CancelDispatch should have freed the terminal")
	}
	pending, err := st.PendingDedicated(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Errorf("PendingDedicated after a failed Inject = %+v, want the message still queued for retry", pending)
	}

	inj.setErr(nil)
	pusher.sweepOnce()
	if got := inj.count(); got != 2 {
		t.Errorf("Inject calls after the retry sweep = %d, want 2 (the failed attempt + a successful retry)", got)
	}
}

// TestRedispatchCapStopsRunawayFlood is the containment regression
// (ct-2026-07-11-074123, post-incident): before MaxRedispatch existed, an
// agent that completes the gate ritual without ever calling mark_handled
// got re-dispatched every sweep forever — this is what turned one bug into
// 15 duplicate real WhatsApp sends. Simulates exactly that shape: the
// message is never marked handled, but the terminal's dispatch keeps
// getting freed (Consume), same as an agent that finishes its ritual
// (send_message or not) without the mark_handled call.
func TestRedispatchCapStopsRunawayFlood(t *testing.T) {
	st, _, gate, inj, pusher := newTestPusher(t, "")
	pusher.cfg.MaxRedispatch = 3
	chat := "55500000025@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m1", Text: "hola", TS: 1}); err != nil {
		t.Fatal(err)
	}

	// S4b (ct-2026-07-30-1255) added Fibonacci backoff between redispatch
	// attempts — this test is about the CAP itself, not the backoff
	// (that's redispatchBackoff's own test), so backdate lastDispatchAt
	// after each sweep to guarantee the next one is never held back waiting.
	anchor := dispatchAnchor{chat, "m1"}
	for i := 0; i < 5; i++ {
		pusher.sweepOnce()
		gate.Consume("port-fallback") // frees InFlight WITHOUT mark_handled
		if lastAt, ok := pusher.lastDispatchAt[anchor]; ok {
			pusher.lastDispatchAt[anchor] = lastAt.Add(-time.Hour)
		}
	}

	if got := inj.count(); got != 3 {
		t.Errorf("Inject calls after 5 sweeps with MaxRedispatch=3 = %d, want 3 (capped, not 5)", got)
	}
	pending, err := st.PendingDedicated(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Errorf("PendingDedicated after hitting the cap = %+v, want the message still queued (held, not dropped)", pending)
	}
}

// TestRedispatchCountPrunedAfterHandled covers the memory-hygiene half: once
// mark_handled lands, the message drops out of PendingDedicated, and the
// next sweep must forget its redispatch count instead of growing the map
// forever for every message capipush has ever dispatched.
func TestRedispatchCountPrunedAfterHandled(t *testing.T) {
	st, _, _, _, pusher := newTestPusher(t, "")
	pusher.cfg.MaxRedispatch = 3
	chat := "55500000020@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m1", Text: "hola", TS: 1}); err != nil {
		t.Fatal(err)
	}

	pusher.sweepOnce()
	anchor := dispatchAnchor{chat, "m1"}
	if _, ok := pusher.redispatchCount[anchor]; !ok {
		t.Fatal("setup: want m1 tracked in redispatchCount after its first dispatch")
	}

	if err := st.MarkHandled(chat, "m1"); err != nil {
		t.Fatal(err)
	}
	pusher.sweepOnce() // m1 no longer pending — this sweep should prune it

	if _, ok := pusher.redispatchCount[anchor]; ok {
		t.Error("redispatchCount still tracks m1 after mark_handled — want it pruned")
	}
}

// ── S4b (ct-2026-07-30-1255) — los tres relojes del reintento ─────────────

// TestDeliveryFailureNeverConsumesRedispatchBudget is S4b's own root-cause
// regression (defect 1) — the boss's own scenario: "¿y si se apaga el PC o
// la API está caída?". Before this fix, redispatchCount incremented BEFORE
// Inject, so a channel down for even 15s (3 sweeps) burned the whole
// MaxRedispatch budget before the message ever reached anyone. A delivery
// FAILURE must never count — only an attempt the agent actually received.
func TestDeliveryFailureNeverConsumesRedispatchBudget(t *testing.T) {
	st, _, _, inj, pusher := newTestPusher(t, "")
	pusher.cfg.MaxRedispatch = 3
	inj.setErr(errors.New("channel down"))
	chat := "55500000026@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m1", Text: "hola", TS: 1}); err != nil {
		t.Fatal(err)
	}

	// The channel stays down for far more sweeps than the OLD MaxRedispatch
	// would ever have tolerated — every attempt must still retry.
	for i := 0; i < 10; i++ {
		pusher.sweepOnce()
	}
	if got := inj.count(); got != 10 {
		t.Fatalf("Inject calls after 10 failed sweeps = %d, want 10 (every sweep retried, none held back)", got)
	}
	anchor := dispatchAnchor{chat, "m1"}
	if got := pusher.redispatchCount[anchor]; got != 0 {
		t.Errorf("redispatchCount after 10 delivery FAILURES = %d, want 0 (a failure must never count as a real dispatch)", got)
	}

	// The channel comes back — the message must still go out.
	inj.setErr(nil)
	pusher.sweepOnce()
	if got := inj.count(); got != 11 {
		t.Fatalf("Inject calls once the channel recovers = %d, want 11 (one more, successful, attempt)", got)
	}
	if got := pusher.redispatchCount[anchor]; got != 1 {
		t.Errorf("redispatchCount after the FIRST successful delivery = %d, want 1", got)
	}
}

// TestChannelDownLogsTransitionOnceNotPerSweep is S4c's own regression
// (ct-2026-07-30-1512): the live 14min cut that verified S4b produced 306 log
// lines for a single antenna outage — 153 of them the exact same "canal
// caído" line, repeated every 5s sweep tick, a real ~63k-line/48h
// projection. The retry itself must keep firing every sweep — that's what
// got the boss's 3 messages out 6s after the antenna came back — only the
// LOG must collapse to one line per state.
func TestChannelDownLogsTransitionOnceNotPerSweep(t *testing.T) {
	st, _, _, inj, pusher := newTestPusher(t, "")
	inj.setErr(errors.New("handshake status 404"))
	chat := "55500000027@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m1", Text: "hola", TS: 1}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })

	for i := 0; i < 5; i++ {
		pusher.sweepOnce()
	}

	if got := strings.Count(buf.String(), "canal caído"); got != 1 {
		t.Errorf("\"canal caído\" logueado %d veces en 5 sweeps consecutivos con el mismo corte, want 1 (sin ruido por sweep)", got)
	}
	if !strings.Contains(buf.String(), "handshake status 404") {
		t.Error("la causa del corte (handshake status 404) no aparece en el log de transición — S1 la daba de un vistazo, S4c no puede perderla")
	}
	if got := inj.count(); got != 5 {
		t.Fatalf("Inject calls tras 5 sweeps con el canal caído = %d, want 5 (el reintento cada sweep no se toca, solo el log)", got)
	}
}

// TestChannelRecoveryLogsDurationAndFailedCount is S4c's exit-line
// requirement: "cuánto duró el corte y cuántos intentos fallaron" can only
// be reported the moment the channel recovers — real operational data that
// didn't exist anywhere before this (Citrino, sobre el corte real: "el canal
// estuvo caído 14 min, 153 intentos").
func TestChannelRecoveryLogsDurationAndFailedCount(t *testing.T) {
	st, _, _, inj, pusher := newTestPusher(t, "")
	inj.setErr(errors.New("handshake status 404"))
	chat := "55500000028@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m1", Text: "hola", TS: 1}); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		pusher.sweepOnce()
	}

	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })

	inj.setErr(nil)
	pusher.sweepOnce()

	out := buf.String()
	if !strings.Contains(out, "canal recuperado") {
		t.Fatalf("log de recuperación ausente tras volver el canal, log = %q", out)
	}
	if !strings.Contains(out, "3 intentos fallidos") {
		t.Errorf("log de recuperación no reporta el conteo de intentos fallidos (3), log = %q", out)
	}
}

// TestNewMessageStillDispatchesDespiteExhaustedSibling is S4b's defect 2
// regression: an old message that hit the redispatch cap must not block a
// genuinely NEW message arriving in the SAME chat — containment holds the
// problematic message, not the whole conversation.
func TestNewMessageStillDispatchesDespiteExhaustedSibling(t *testing.T) {
	st, _, gate, inj, pusher := newTestPusher(t, "")
	pusher.cfg.MaxRedispatch = 2
	chat := "55500000029@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "old", Text: "el primero", TS: 1}); err != nil {
		t.Fatal(err)
	}

	oldAnchor := dispatchAnchor{chat, "old"}
	for i := 0; i < 2; i++ {
		pusher.sweepOnce()
		gate.Consume("port-fallback") // frees InFlight WITHOUT mark_handled
		if lastAt, ok := pusher.lastDispatchAt[oldAnchor]; ok {
			pusher.lastDispatchAt[oldAnchor] = lastAt.Add(-time.Hour)
		}
	}
	if got := pusher.redispatchCount[oldAnchor]; got != 2 {
		t.Fatalf("setup: redispatchCount[old] = %d, want 2 (exhausted, MaxRedispatch=2)", got)
	}

	// A genuinely new message arrives in the SAME chat.
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "new", Text: "uno nuevo", TS: 2}); err != nil {
		t.Fatal(err)
	}
	before := inj.count()
	pusher.sweepOnce()

	if got := inj.count(); got != before+1 {
		t.Errorf("Inject calls after a new message joined an exhausted chat = %d (was %d), want exactly 1 more — the new message must still dispatch", got, before)
	}
	newAnchor := dispatchAnchor{chat, "new"}
	if got := pusher.redispatchCount[newAnchor]; got != 1 {
		t.Errorf("redispatchCount[new] = %d, want 1 (its own fresh budget, unaffected by the old sibling's exhausted one)", got)
	}
}

// TestBackoffHoldsBackImmediateRedispatch confirms dispatch() actually
// engages redispatchBackoff (S4b defect 3): an immediate re-sweep right
// after a successful dispatch must NOT re-inject before the backoff window
// elapses — retrying every 5s sweep is exactly the old, broken behavior.
func TestBackoffHoldsBackImmediateRedispatch(t *testing.T) {
	st, _, gate, inj, pusher := newTestPusher(t, "")
	pusher.cfg.MaxRedispatch = 3
	chat := "55500000030@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m1", Text: "hola", TS: 1}); err != nil {
		t.Fatal(err)
	}

	pusher.sweepOnce()
	gate.Consume("port-fallback")
	if got := inj.count(); got != 1 {
		t.Fatalf("setup: Inject calls after first sweep = %d, want 1", got)
	}

	pusher.sweepOnce() // immediate re-sweep, no real elapsed time
	if got := inj.count(); got != 1 {
		t.Errorf("Inject calls after an IMMEDIATE re-sweep = %d, want still 1 (backoff must hold it back)", got)
	}
}

// TestRedispatchBackoffGrowsWithJitterAndCeiling is a direct unit test of
// the Fibonacci table itself (S4b defect 3) — strictly increasing, each
// step within its ±25% jitter band, and never growing past the last value
// once attempt exceeds the table's length.
func TestRedispatchBackoffGrowsWithJitterAndCeiling(t *testing.T) {
	for i := 1; i < len(fibonacciBackoffMinutes); i++ {
		if fibonacciBackoffMinutes[i] <= fibonacciBackoffMinutes[i-1] {
			t.Fatalf("fibonacciBackoffMinutes[%d]=%d, want strictly greater than [%d]=%d", i, fibonacciBackoffMinutes[i], i-1, fibonacciBackoffMinutes[i-1])
		}
	}
	for attempt := 1; attempt <= len(fibonacciBackoffMinutes); attempt++ {
		base := time.Duration(fibonacciBackoffMinutes[attempt-1]) * time.Minute
		got := redispatchBackoff(attempt)
		if got < base || got > base+base/4 {
			t.Errorf("redispatchBackoff(%d) = %s, want within [%s, %s]", attempt, got, base, base+base/4)
		}
	}
	// Ceiling: beyond the table's length, repeats the LAST value, never
	// grows further ("no hace falta más que eso", Citrino).
	lastBase := time.Duration(fibonacciBackoffMinutes[len(fibonacciBackoffMinutes)-1]) * time.Minute
	got := redispatchBackoff(len(fibonacciBackoffMinutes) + 5)
	if got < lastBase || got > lastBase+lastBase/4 {
		t.Errorf("redispatchBackoff(beyond the table) = %s, want within [%s, %s] (ceiling)", got, lastBase, lastBase+lastBase/4)
	}
}

// TestMaxRedispatchReadsLiveFromSettings covers the contract's own
// criterio: the three clocks read live from settings, not just Config.
func TestMaxRedispatchReadsLiveFromSettings(t *testing.T) {
	st, _, gate, inj, pusher := newTestPusher(t, "")
	pusher.cfg.MaxRedispatch = 100 // Config fallback: effectively never caps
	if err := st.SetSettingInt(store.SettingCapipushMaxRedispatch, 1); err != nil {
		t.Fatal(err)
	}
	chat := "55500000031@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m1", Text: "hola", TS: 1}); err != nil {
		t.Fatal(err)
	}

	anchor := dispatchAnchor{chat, "m1"}
	pusher.sweepOnce()
	gate.Consume("port-fallback")
	pusher.lastDispatchAt[anchor] = pusher.lastDispatchAt[anchor].Add(-time.Hour)
	pusher.sweepOnce() // should be capped now — settings override (1) beats Config (100)

	if got := inj.count(); got != 1 {
		t.Errorf("Inject calls = %d, want 1 — the settings-overridden MaxRedispatch (1) should have capped it despite Config.MaxRedispatch=100", got)
	}
}

// TestSweepOnceAppliesLiveDispatchStaleAfterToGate confirms sweepOnce
// actually calls gate.SetStaleAfter with the live settings value (S4b
// defect 4) — not just that the Config fallback exists. A short override
// plus a genuinely stale dispatch confirms the gate reclaims it on THAT
// schedule (via RegisterDispatch's own opportunistic sweepLocked), not the
// 15m/1h defaults.
func TestSweepOnceAppliesLiveDispatchStaleAfterToGate(t *testing.T) {
	st, _, gate, _, pusher := newTestPusher(t, "")
	if err := gate.RegisterDispatch("nonce-stale", "chat-stale@c.us", mcpserver.LevelDanger, "term-stale", 0); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSettingDuration(store.SettingCapipushDispatchStaleAfter, 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	pusher.sweepOnce() // applies the live 10ms override to gate.SetStaleAfter

	time.Sleep(20 * time.Millisecond)

	// A second, unrelated dispatch registration triggers Gate's own
	// opportunistic sweepLocked (RegisterDispatch's side effect) — the
	// stale "term-stale" dispatch (now well past the 10ms override) gets
	// reclaimed as a result.
	otherChat := "55500000032@c.us"
	if err := st.TouchChat(otherChat, "C", 1); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, otherChat)
	if err := st.AddMessage(store.Message{ChatJID: otherChat, ID: "m1", Text: "hola", TS: 1}); err != nil {
		t.Fatal(err)
	}
	pusher.sweepOnce()

	if gate.InFlight("term-stale") {
		t.Error("InFlight after the live-overridden 10ms stale-after elapsed (and a new RegisterDispatch ran) = true, want false (reclaimed)")
	}
}

// TestLogInjectorSkipsDispatchSilently: cuando el terminal no tiene antena real
// (injector == LogInjector, wired desde nil en New()), sweepOnce NO debe
// intentar el dispatch — sin gate registration, sin Inject, sin error log spam.
// El mensaje queda en PendingDedicated para pull MCP.
func TestLogInjectorSkipsDispatchSilently(t *testing.T) {
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
	rt := router.NewManager(routerPath)
	gate := mcpserver.NewGate()
	// nil injector → LogInjector (sin antena configurada)
	pusher := New(st, rt, gate, nil, Config{PortFallback: "port-fallback", SwampedAt: 8})

	chat := "55500000033@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m1", Text: "hola", TS: 1}); err != nil {
		t.Fatal(err)
	}

	pusher.sweepOnce()

	if gate.InFlight("port-fallback") {
		t.Error("gate in-flight after sweep con LogInjector — debe saltear sin registrar el dispatch")
	}
	pending, err := st.PendingDedicated(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Errorf("PendingDedicated tras sweep con LogInjector = %d, want 1 (mensaje queda para MCP-pull)", len(pending))
	}
}

// TestLogTransitionFiresOnceEnterThenOnceExit is S1's own regression
// (ct-2026-07-30-0309): the sweep ticker runs every 5s, so a condition
// (backpressure, debounce, no antenna, terminal busy) that holds steady
// across many sweeps must log its enter/exit exactly once, not once per
// sweep.
func TestLogTransitionFiresOnceEnterThenOnceExit(t *testing.T) {
	_, _, _, _, pusher := newTestPusher(t, "")
	enters, exits := 0, 0
	enter := func() { enters++ }
	exit := func() { exits++ }

	pusher.logTransition("k", true, enter, exit)
	pusher.logTransition("k", true, enter, exit)
	pusher.logTransition("k", true, enter, exit)
	if enters != 1 {
		t.Errorf("enters while active stays true = %d, want 1 (no repeat)", enters)
	}

	pusher.logTransition("k", false, enter, exit)
	pusher.logTransition("k", false, enter, exit)
	if exits != 1 {
		t.Errorf("exits while active stays false = %d, want 1 (no repeat)", exits)
	}

	pusher.logTransition("k", true, enter, exit)
	if enters != 2 {
		t.Errorf("enters after re-entering = %d, want 2", enters)
	}
}

// TestPruneStaleStateDropsStaleDebounceEntries covers the memory-hygiene
// half of S1's debounce transition tracking: a chat that leaves `pending`
// (dispatched, or its messages cleared some other way) without ever
// revisiting the non-debounced branch must still have its logState entry
// reclaimed — same reasoning as TestRedispatchCountPrunedAfterHandled.
func TestPruneStaleStateDropsStaleDebounceEntries(t *testing.T) {
	_, _, _, _, pusher := newTestPusher(t, "")
	pusher.logState["debounce:gone@c.us"] = true
	pusher.logState["debounce:still@c.us"] = true

	pusher.pruneStaleState(map[string][]store.Message{
		"still@c.us": {{ID: "m1"}},
	})

	if pusher.logState["debounce:gone@c.us"] {
		t.Error("debounce:gone@c.us still tracked after its chat left pending — want pruned")
	}
	if !pusher.logState["debounce:still@c.us"] {
		t.Error("debounce:still@c.us dropped even though its chat is still pending")
	}
}

// TestSweepDoesNotRepeatLogWhileSteadyBackpressure is S1's explicit
// criterio de listo: "el log no crece por sweep cuando no pasa nada". A
// condition held steady across several sweep ticks (5s each in production)
// must log its transition line exactly once, not once per tick.
func TestSweepDoesNotRepeatLogWhileSteadyBackpressure(t *testing.T) {
	st, _, _, _, pusher := newTestPusher(t, "")
	pusher.cfg.SwampedAt = 1

	chat := "55500000034@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m1", Text: "hola", TS: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })

	for i := 0; i < 5; i++ {
		pusher.sweepOnce()
	}

	if got := strings.Count(buf.String(), "backpressure activado"); got != 1 {
		t.Errorf("\"backpressure activado\" logueado %d veces en 5 sweeps consecutivos con el mismo estado, want 1 (sin ruido por sweep)", got)
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	_, _, _, _, pusher := newTestPusher(t, "")
	pusher.cfg.SweepInterval = 5 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		pusher.Run(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

// TestInjectorFallsBackToLogSkipsDispatch: dispatch to a terminal_id with no
// registered injector resolves to LogInjector → sweep skips silently — no panic,
// no gate registration, message stays in PendingDedicated for MCP-pull.
// (ct-2026-07-13-1822: LogInjector = sin antena, skip silencioso.)
func TestInjectorFallsBackToLogSkipsDispatch(t *testing.T) {
	st, _, gate, _, pusher := newTestPusher(t, "")
	chat := "55500000035@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m1", Text: "hola", TS: 1}); err != nil {
		t.Fatal(err)
	}

	// Override PortFallback with an unknown terminal so no injector matches.
	pusher.cfg.PortFallback = "unknown-terminal"

	pusher.sweepOnce() // must not panic

	// No antenna → gate must NOT register an in-flight dispatch.
	if gate.InFlight("unknown-terminal") {
		t.Error("gate in-flight after sweep con LogInjector fallback — debe saltear sin registrar el dispatch")
	}
	pending, err := st.PendingDedicated(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Errorf("PendingDedicated = %d, want 1 (mensaje queda para MCP-pull)", len(pending))
	}
}

// newBootPusher replicates main.go's own S6 (ct-2026-07-30-031048) wiring:
// a *CleverInjector is ALWAYS the registered PortFallback injector, whether
// or not it starts with real credentials — never a separate LogInjector
// that set_capi_connector's SetConfig can't ever reach.
func newBootPusher(t *testing.T, endpoint, terminalID, pinpass string) (*store.Store, *CleverInjector, *Pusher) {
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
	rt := router.NewManager(routerPath)
	gate := mcpserver.NewGate()
	cleverInj := NewCleverInjector(endpoint, terminalID, pinpass)
	pusher := New(st, rt, gate, cleverInj, Config{PortFallback: "port-fallback"})
	return st, cleverInj, pusher
}

// TestUnconfiguredCleverInjectorTreatedAsNoAntenna is S6's own regression
// (ct-2026-07-30-031048): main.go now always registers the live
// *CleverInjector for PortFallback, even before it has real credentials
// (endpoint == ""). dispatch() must treat that exactly like LogInjector —
// quiet retention, no attempted Inject() against an empty endpoint — via
// CleverInjector.Configured(), not by ever falling back to a second object.
func TestUnconfiguredCleverInjectorTreatedAsNoAntenna(t *testing.T) {
	st, _, pusher := newBootPusher(t, "", "", "")
	chat := "55500000036@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m1", Text: "hola", TS: 1}); err != nil {
		t.Fatal(err)
	}

	pusher.sweepOnce() // must not attempt a real Inject(), must not panic

	if pusher.gate.InFlight("port-fallback") {
		t.Error("gate in-flight after sweep with an unconfigured CleverInjector — must skip without registering the dispatch, same as LogInjector")
	}
	pending, err := st.PendingDedicated(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Errorf("PendingDedicated = %d, want 1 (message stays for MCP-pull, no antenna configured yet)", len(pending))
	}
}

// TestSetConfigOnPrincipalInjectorHotReloadsWithoutRestart is S6's core
// fix, verified end to end through Pusher.dispatch (ct-2026-07-30-031048):
// before this, if the gateway booted with no cAPI endpoint configured,
// main.go registered a LogInjector for PortFallback and kept the real
// *CleverInjector alive but ORPHANED — set_capi_connector's SetConfig call
// reconfigured that orphan, but RegisterInjector refuses to ever swap
// PortFallback's entry, so dispatch kept using the original LogInjector
// forever ("aplica en caliente" was false in exactly this case; only a
// restart picked up the new config). Now the SAME *CleverInjector is
// ALWAYS what's registered, so SetConfig reaches the real dispatch path
// immediately — no restart.
func TestSetConfigOnPrincipalInjectorHotReloadsWithoutRestart(t *testing.T) {
	var gotHandshake bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/handshake":
			gotHandshake = true
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "tok"})
		case "/message":
			_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		}
	}))
	defer srv.Close()

	// Booted with no cAPI endpoint configured yet — the exact scenario that
	// used to orphan the real injector.
	st, cleverInj, pusher := newBootPusher(t, "", "", "")
	chat := "55500000037@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m1", Text: "hola", TS: 1}); err != nil {
		t.Fatal(err)
	}

	// set_capi_connector, live — no restart, no re-registration.
	cleverInj.SetConfig(srv.URL, "new-antenna-guid", "pin")

	pusher.sweepOnce()

	if !gotHandshake {
		t.Error("dispatch never reached the reconfigured CleverInjector after a live SetConfig — hot-reload is not actually hot")
	}
}

// TestRegisterInjectorUpdatesMap: RegisterInjector wires a new Injector for
// a secondary terminal; dispatch to that terminal uses it.
func TestRegisterInjectorUpdatesMap(t *testing.T) {
	chat := "55500000038@c.us"
	cfg := `{"allow_all":true,"default_mode":"dedicated","routes":[{"match":"` + chat + `","terminal_id":"secondary-term"}]}`
	st, _, _, _, pusher := newTestPusher(t, cfg)

	secondary := &fakeInjector{}
	pusher.RegisterInjector("secondary-term", secondary)

	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m1", Text: "hola", TS: 1}); err != nil {
		t.Fatal(err)
	}

	pusher.sweepOnce()

	if secondary.count() != 1 || secondary.calls[0] != "secondary-term" {
		t.Errorf("secondary injector calls = %v, want exactly one to secondary-term", secondary.calls)
	}
}

// TestInjectorForReportsRegisteredVsUnregistered (M2, ct-2026-07-22-1301):
// the dashboard's per-agent ping (InjectorFor) must tell "this agent_id has
// a real registered injector" apart from injectorFor's own LogInjector
// fallback — a ping that silently "succeeds" against LogInjector would lie
// to the boss about reaching the terminal.
func TestInjectorForReportsRegisteredVsUnregistered(t *testing.T) {
	cfg := `{"allow_all":true,"default_mode":"dedicated","routes":[]}`
	_, _, _, principalInj, pusher := newTestPusher(t, cfg)

	secondary := &fakeInjector{}
	pusher.RegisterInjector("secondary-term", secondary)

	if got, ok := pusher.InjectorFor("secondary-term"); !ok || got != secondary {
		t.Errorf("InjectorFor(secondary-term) = (%v, %v), want (secondary, true)", got, ok)
	}
	if got, ok := pusher.InjectorFor("port-fallback"); !ok || got != principalInj {
		t.Errorf("InjectorFor(port-fallback) = (%v, %v), want (principal injector, true)", got, ok)
	}
	if _, ok := pusher.InjectorFor("never-registered"); ok {
		t.Error("InjectorFor(never-registered) ok = true, want false — nothing was ever registered for it")
	}
}

// TestBossDispatchAlwaysGoesToPrincipalWithInjectorMap: is_boss dispatch
// goes to PortFallback's injector regardless of route, even with a mapa.
func TestBossDispatchAlwaysGoesToPrincipalWithInjectorMap(t *testing.T) {
	chat := "55500000039@c.us"
	cfg := `{"allow_all":true,"default_mode":"dedicated","routes":[{"match":"` + chat + `","terminal_id":"secondary-term"}]}`
	st, _, _, inj, pusher := newTestPusher(t, cfg)

	secondary := &fakeInjector{}
	pusher.RegisterInjector("secondary-term", secondary)

	if err := st.TouchChat(chat, "Boss", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetIsBoss(chat, true); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m1", Text: "hola", TS: 1}); err != nil {
		t.Fatal(err)
	}

	pusher.sweepOnce()

	// Must go to port-fallback (principal), NOT to secondary-term.
	if inj.count() != 1 || inj.calls[0] != "port-fallback" {
		t.Errorf("principal injector calls = %v, want exactly one to port-fallback", inj.calls)
	}
	if secondary.count() != 0 {
		t.Errorf("secondary injector calls = %v, want 0 (boss must not go to secondary)", secondary.calls)
	}
}

// TestRegisterInjectorCannotOverwritePrincipalSlot: RegisterInjector must
// silently ignore calls with agentID == PortFallback so a stale agents row
// or future caller can never hijack the principal's injector post-New.
func TestRegisterInjectorCannotOverwritePrincipalSlot(t *testing.T) {
	st, _, gate, principalInj, pusher := newTestPusher(t, "")
	intruder := &fakeInjector{}
	pusher.RegisterInjector("port-fallback", intruder) // must be a no-op

	// Fire a boss dispatch — it must arrive at the original principal injector,
	// not at the intruder.
	chat := "55500000040@c.us"
	if err := st.TouchChat(chat, "Boss", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetIsBoss(chat, true); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m1", Text: "hola", TS: 1}); err != nil {
		t.Fatal(err)
	}
	_ = gate
	pusher.sweepOnce()

	if intruder.count() != 0 {
		t.Error("intruder injector received a call — RegisterInjector must not overwrite PortFallback")
	}
	if principalInj.count() != 1 {
		t.Errorf("principal injector calls = %d, want 1 (principal slot immutable)", principalInj.count())
	}
}

func TestCapPreview(t *testing.T) {
	// short text passes through untouched
	if got := capPreview("hola"); got != "hola" {
		t.Fatalf("short text altered: %q", got)
	}
	// long text is capped at or below the byte ceiling
	long := strings.Repeat("a", maxPreviewBytes+500)
	if got := capPreview(long); len(got) != maxPreviewBytes {
		t.Fatalf("ascii truncation: got %d bytes, want %d", len(got), maxPreviewBytes)
	}
	// multibyte text is never split mid-rune (é = 2 bytes): cap must back up
	// to a rune boundary and the result must stay valid UTF-8
	multi := strings.Repeat("é", maxPreviewBytes) // 2*maxPreviewBytes bytes
	got := capPreview(multi)
	if len(got) > maxPreviewBytes {
		t.Fatalf("multibyte truncation over ceiling: %d bytes", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatal("truncation split a rune — result is not valid UTF-8")
	}
}

// TestDispatchPayloadFormat verifies ct-2026-07-18-1416 (compact format),
// ct-2026-07-18-180631 (dropped the redundant "piumy:" prefix),
// ct-2026-07-18-1851-B (numero/nivel moved OUT of the body into the
// envelope's from — see envelopeFrom — leaving the message text as the
// body's FIRST line), and the ct-2026-08-06 preamble fix: an identity
// line (is_boss/is_approver/nivel) now always follows, before the
// "NC:<4hex>" signature line at the end.
func TestDispatchPayloadFormat(t *testing.T) {
	st, _, _, inj, _ := newTestPusher(t, "")

	gate := mcpserver.NewGate()
	inj2 := &fakeInjectorPayload{}
	pusher := New(st, nil, gate, inj2, Config{
		PortFallback: "terminal1",
		SwampedAt:    8,
	})
	_ = inj

	chat := "plain@c.us"
	if err := st.TouchChat(chat, "Test", 1); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m1", FromMe: false, Text: "hola", TS: 1}); err != nil {
		t.Fatal(err)
	}

	pusher.sweepOnce()

	payload := inj2.last()
	if payload == "" {
		t.Fatal("injector received no payload")
	}
	if strings.TrimSpace(strings.SplitN(payload, "\n", 2)[0]) != "hola" {
		t.Errorf("payload = %q, want the body's first line to be just the message text (no numero/nivel line — that's in the envelope from)", payload)
	}
	if strings.Contains(payload, "whatsapp:(") {
		t.Errorf("payload = %q, want NO whatsapp:(...) line — numero/nivel moved to the envelope from", payload)
	}
	if !strings.Contains(payload, "is_boss: false, is_approver: false — nivel danger") {
		t.Errorf("payload = %q, want the identity line for a plain never-seen contact", payload)
	}
	if !strings.Contains(payload, "\nNC:") {
		t.Errorf("payload = %q, want a NC:<4hex> signature line at the end", payload)
	}
	if from := inj2.lastFrom(); from != "plain, danger" {
		t.Errorf("envelope from = %q, want \"plain, danger\" (numero, level — a never-seen contact is LevelDanger)", from)
	}
}

// TestDispatchBossGetsIdentityLineAndRules is the regression for the
// preamble fix (ct-2026-08-06, boss verbatim: "si soy boss tiene que decir
// is boss... todo mensaje con su preámbulo"). Before this, the boss's own
// dispatch carried NEITHER an identity line NOR its own rules — an agent
// had to guess who it was talking to, and the boss's rules were silently
// discarded. This is the concrete case the boss reported live: a real
// dispatch to him came through with an empty preamble.
func TestDispatchBossGetsIdentityLineAndRules(t *testing.T) {
	st, _, _, inj, _ := newTestPusher(t, "")

	gate := mcpserver.NewGate()
	inj2 := &fakeInjectorPayload{}
	pusher := New(st, nil, gate, inj2, Config{
		PortFallback: "terminal1",
		SwampedAt:    8,
	})
	_ = inj

	chat := "boss@c.us"
	if err := st.TouchChat(chat, "Boss", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetIsBoss(chat, true); err != nil {
		t.Fatal(err)
	}
	if err := st.SetChatRules(chat, "sé breve"); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m1", FromMe: false, Text: "hola", TS: 1}); err != nil {
		t.Fatal(err)
	}

	pusher.sweepOnce()

	if from := inj2.lastFrom(); !strings.HasSuffix(from, ", boss") {
		t.Errorf("envelope from = %q, want it to end in \", boss\"", from)
	}
	payload := inj2.last()
	if !strings.Contains(payload, "is_boss: true") {
		t.Errorf("payload = %q, want the identity line (is_boss: true) — this is the exact regression the boss reported: a dispatch to him with an empty preamble", payload)
	}
	if !strings.Contains(payload, "```rules.md\nsé breve\n```") {
		t.Errorf("payload = %q, want the boss's own EffectiveRules attached too — they were being silently discarded", payload)
	}
}

// TestDispatchApproverIdentityLineShowsIsApprover is the is_approver half
// of the ct-2026-08-06 preamble fix (boss verbatim: "hoy tampoco viaja y
// hace falta para el circuito de aprobación") — a non-boss chat's
// is_approver pin now rides along in the identity line, not just the
// level.
func TestDispatchApproverIdentityLineShowsIsApprover(t *testing.T) {
	st, _, _, inj, _ := newTestPusher(t, "")

	gate := mcpserver.NewGate()
	inj2 := &fakeInjectorPayload{}
	pusher := New(st, nil, gate, inj2, Config{
		PortFallback: "terminal1",
		SwampedAt:    8,
	})
	_ = inj

	chat := "approver@c.us"
	if err := st.TouchChat(chat, "Approver", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetIsApprover(chat, true); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m1", FromMe: false, Text: "hola", TS: 1}); err != nil {
		t.Fatal(err)
	}

	pusher.sweepOnce()

	if from := inj2.lastFrom(); !strings.HasSuffix(from, ", approver") {
		t.Errorf("envelope from = %q, want it to end in \", approver\"", from)
	}
	payload := inj2.last()
	if !strings.Contains(payload, "is_boss: false, is_approver: true — nivel approver") {
		t.Errorf("payload = %q, want the identity line to show is_approver: true", payload)
	}
}

// TestDispatchAttachesRulesForNonBoss verifies ct-2026-07-18-1416:
// "si no es del boss adjuntar un codigo.md con las rules" (boss verbatim) —
// a non-boss chat's EffectiveRules ride along as a fenced rules.md block.
func TestDispatchAttachesRulesForNonBoss(t *testing.T) {
	st, _, _, inj, _ := newTestPusher(t, "")

	gate := mcpserver.NewGate()
	inj2 := &fakeInjectorPayload{}
	pusher := New(st, nil, gate, inj2, Config{
		PortFallback: "terminal1",
		SwampedAt:    8,
	})
	_ = inj

	chat := "notboss@c.us"
	if err := st.SetDefaultRules("sé breve"); err != nil {
		t.Fatal(err)
	}
	if err := st.TouchChat(chat, "NotBoss", 1); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m1", FromMe: false, Text: "hola", TS: 1}); err != nil {
		t.Fatal(err)
	}

	pusher.sweepOnce()

	payload := inj2.last()
	if !strings.Contains(payload, "```rules.md\nsé breve\n```") {
		t.Errorf("payload = %q, want the EffectiveRules attached as a rules.md block", payload)
	}
}

// TestDispatchIncludesRejectionNote is T15 (ct-2026-08-05-123241, Citrino:
// "el motivo tiene que viajar con el mensaje, no aparte") — a chat with an
// outstanding rejected draft gets the reason + previous attempt prepended
// to the very payload carrying the message it's redispatching, not a
// separate lookup the agent has to make.
func TestDispatchIncludesRejectionNote(t *testing.T) {
	st, _, _, inj, _ := newTestPusher(t, "")

	gate := mcpserver.NewGate()
	inj2 := &fakeInjectorPayload{}
	pusher := New(st, nil, gate, inj2, Config{
		PortFallback: "terminal1",
		SwampedAt:    8,
	})
	_ = inj

	chat := "rejected@c.us"
	if err := st.TouchChat(chat, "Test", 1); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m1", FromMe: false, Text: "hola, dame el precio", TS: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddDraftWithConfirmer(chat, "sale $100", "m", "", 1, 2); err != nil {
		t.Fatal(err)
	}
	drafts, err := st.PendingDrafts(10)
	if err != nil || len(drafts) != 1 {
		t.Fatalf("PendingDrafts = %+v, err=%v", drafts, err)
	}
	if _, _, _, ok, err := st.RejectDraft(drafts[0].ID, "el precio real es $150"); err != nil || !ok {
		t.Fatalf("RejectDraft: ok=%v err=%v", ok, err)
	}
	// Reopen the triggering message, same as reject_draft's handler does
	// when the round is under the cap.
	if err := st.MarkPendingBefore(chat, 1); err != nil {
		t.Fatal(err)
	}

	pusher.sweepOnce()

	payload := inj2.last()
	if !strings.Contains(payload, "MOTIVO DE RECHAZO: el precio real es $150") {
		t.Errorf("payload = %q, want the rejection reason prepended", payload)
	}
	if !strings.Contains(payload, "Tu borrador anterior: sale $100") {
		t.Errorf("payload = %q, want the rejected draft's own text quoted back", payload)
	}
	if !strings.Contains(payload, "hola, dame el precio") {
		t.Errorf("payload = %q, want the original triggering message still present", payload)
	}
}

// TestDispatchOmitsRejectionNoteWithoutOne is the negative case: a chat
// with no rejected draft gets the plain payload, unchanged.
func TestDispatchOmitsRejectionNoteWithoutOne(t *testing.T) {
	st, _, _, inj, _ := newTestPusher(t, "")

	gate := mcpserver.NewGate()
	inj2 := &fakeInjectorPayload{}
	pusher := New(st, nil, gate, inj2, Config{
		PortFallback: "terminal1",
		SwampedAt:    8,
	})
	_ = inj

	chat := "plain2@c.us"
	if err := st.TouchChat(chat, "Test", 1); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m1", FromMe: false, Text: "hola", TS: 1}); err != nil {
		t.Fatal(err)
	}

	pusher.sweepOnce()

	if payload := inj2.last(); strings.Contains(payload, "MOTIVO DE RECHAZO") {
		t.Errorf("payload = %q, want no rejection note for a chat with no rejected draft", payload)
	}
}

// TestDispatchResolvesLIDNumero verifies ct-2026-07-18-1416: a
// @lid chat's "numero" resolves via the wired LIDResolver (F2's ResolvePN
// seam) instead of leaking the raw @lid string. Since ct-2026-07-18-1851-B,
// numero lives in the envelope's from, not the body.
func TestDispatchResolvesLIDNumero(t *testing.T) {
	st, _, _, inj, _ := newTestPusher(t, "")

	gate := mcpserver.NewGate()
	inj2 := &fakeInjectorPayload{}
	pusher := New(st, nil, gate, inj2, Config{
		PortFallback: "terminal1",
		SwampedAt:    8,
	})
	_ = inj
	const lidJID = "555000000000041@lid"
	const numberJID = "55500000042@s.whatsapp.net"
	pusher.SetLIDResolver(fakeLIDResolver{mapping: map[string]string{lidJID: numberJID}})

	if err := st.TouchChat(lidJID, "Boss", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetIsBoss(lidJID, true); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, lidJID)
	if err := st.AddMessage(store.Message{ChatJID: lidJID, ID: "m1", FromMe: false, Text: "hola", TS: 1}); err != nil {
		t.Fatal(err)
	}

	pusher.sweepOnce()

	if from := inj2.lastFrom(); from != "55500000042, boss" {
		t.Errorf("envelope from = %q, want the resolved bare number (55500000042), not the raw @lid", from)
	}
}

// TestNewNonceIsShortAndUnique verifies ct-2026-07-18-1851-B ("bajemoslo a 4
// dijitos exadecimal", "regenerar si ya existe en el gate byNonce"): every
// nonce is exactly 4 hex chars and never collides with one currently
// registered. Draws 2000 (out of the 65536-value space) and keeps each one
// active in the gate — past ~256 draws the birthday paradox makes hitting
// the regeneration path in practice near-certain, not just theoretical.
func TestNewNonceIsShortAndUnique(t *testing.T) {
	_, _, gate, _, p := newTestPusher(t, "")

	seen := map[string]bool{}
	for i := 0; i < 2000; i++ {
		nonce, err := p.newNonce()
		if err != nil {
			t.Fatalf("newNonce: %v", err)
		}
		if len(nonce) != 4 {
			t.Fatalf("newNonce() = %q, want exactly 4 hex chars", nonce)
		}
		if seen[nonce] {
			t.Fatalf("newNonce() returned %q twice — collision not avoided", nonce)
		}
		seen[nonce] = true
		// Keep it "active" in the gate, same as a real dispatch would, so
		// later draws must route around it. Unique terminalID per call so
		// RegisterDispatch never evicts an earlier nonce via byTerminal.
		if err := gate.RegisterDispatch(nonce, "chat@c.us", mcpserver.LevelCaution, "term-"+nonce, 0); err != nil {
			t.Fatalf("RegisterDispatch: %v", err)
		}
	}
}

type fakeLIDResolver struct {
	mapping map[string]string
}

func (f fakeLIDResolver) ResolvePN(_ context.Context, lidJID string) (string, error) {
	return f.mapping[lidJID], nil
}

// TestBurstAllMessagesInPayload verifies ct-2026-07-13-2131: a burst of N
// messages dispatches ALL texts in the payload, not just the last.
func TestBurstAllMessagesInPayload(t *testing.T) {
	st, _, _, inj, _ := newTestPusher(t, "")
	gate := mcpserver.NewGate()
	inj2 := &fakeInjectorPayload{}
	pusher := New(st, nil, gate, inj2, Config{PortFallback: "terminal1", SwampedAt: 8})
	_ = inj

	chat := "burst@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	for i, txt := range []string{"primero", "segundo", "tercero"} {
		if err := st.AddMessage(store.Message{ChatJID: chat, ID: string(rune('a' + i)), Text: txt, TS: int64(i + 1)}); err != nil {
			t.Fatal(err)
		}
	}

	pusher.sweepOnce()

	payload := inj2.last()
	if payload == "" {
		t.Fatal("injector received no payload")
	}
	want := []string{"primero", "segundo", "tercero"}
	for _, w := range want {
		if !strings.Contains(payload, w) {
			t.Errorf("payload = %q, want it to contain burst message %q", payload, w)
		}
	}
	lines := strings.Split(strings.TrimRight(payload, "\n"), "\n")
	if len(lines) < 3 || lines[0] != want[0] || lines[1] != want[1] || lines[2] != want[2] {
		t.Errorf("payload lines = %v, want the burst in order: %v", lines, want)
	}
}

// fakeReceipter captures MarkRead calls for test assertions.
type fakeReceipter struct {
	mu    sync.Mutex
	calls []struct {
		chatJID string
		msgIDs  []string
	}
}

func (f *fakeReceipter) MarkRead(_ context.Context, chatJID string, msgIDs []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, struct {
		chatJID string
		msgIDs  []string
	}{chatJID, msgIDs})
	return nil
}

func (f *fakeReceipter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// TestReadReceiptFiredOnDispatch verifies ct-2026-07-13-2131: after a
// successful dispatch, MarkRead is called once with all burst message IDs.
func TestReadReceiptFiredOnDispatch(t *testing.T) {
	st, _, _, inj, pusher := newTestPusher(t, "")
	rec := &fakeReceipter{}
	pusher.SetReceipter(rec)

	chat := "receipt@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	for i, txt := range []string{"msg1", "msg2"} {
		if err := st.AddMessage(store.Message{ChatJID: chat, ID: string(rune('a' + i)), Text: txt, TS: int64(i + 1)}); err != nil {
			t.Fatal(err)
		}
	}

	pusher.sweepOnce()

	if inj.count() != 1 {
		t.Fatalf("Inject calls = %d, want 1", inj.count())
	}
	if rec.callCount() != 1 {
		t.Fatalf("MarkRead calls = %d, want 1 (one coalesced call)", rec.callCount())
	}
	got := rec.calls[0]
	if got.chatJID != chat {
		t.Errorf("MarkRead chatJID = %q, want %q", got.chatJID, chat)
	}
	if len(got.msgIDs) != 2 || got.msgIDs[0] != "a" || got.msgIDs[1] != "b" {
		t.Errorf("MarkRead msgIDs = %v, want [a b]", got.msgIDs)
	}
}

// TestReadReceiptSkippedWhenHalted verifies ct-2026-07-13-2131: MarkRead is
// NOT sent when HaltedFn returns true (kill switch or mute active).
func TestReadReceiptSkippedWhenHalted(t *testing.T) {
	st, _, _, inj, pusher := newTestPusher(t, "")
	rec := &fakeReceipter{}
	pusher.SetReceipter(rec)
	pusher.cfg.HaltedFn = func() bool { return true }

	chat := "halt@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m1", Text: "hola", TS: 1}); err != nil {
		t.Fatal(err)
	}

	pusher.sweepOnce()

	if inj.count() != 1 {
		t.Fatalf("Inject calls = %d, want 1 (dispatch still fires)", inj.count())
	}
	if rec.callCount() != 0 {
		t.Errorf("MarkRead calls = %d, want 0 (halted)", rec.callCount())
	}
}

// TestDebounceSupressesEarlyDispatch — ct-2026-07-13-2243 Fix 1: a message
// that arrived recently (within DispatchDebounce) is NOT dispatched yet —
// capipush waits for silence before sending the burst.
func TestDebounceSupressesEarlyDispatch(t *testing.T) {
	st, _, _, inj, pusher := newTestPusher(t, "")
	pusher.cfg.DispatchDebounce = 60 * time.Second
	pusher.cfg.MaxDispatchDebounce = 5 * time.Minute

	chat := "debounce@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	// Message arrived 10s ago — within the 60s debounce window.
	recentTS := time.Now().Unix() - 10
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m1", Text: "hola", TS: recentTS}); err != nil {
		t.Fatal(err)
	}

	pusher.sweepOnce()

	if got := inj.count(); got != 0 {
		t.Errorf("Inject calls with message 10s old and debounce=60s = %d, want 0 (still waiting)", got)
	}
}

// TestMaxDebounceOverridesWhenChatNeverQuiets — ct-2026-07-13-2243 Fix 1:
// when the oldest burst message exceeds MaxDispatchDebounce, dispatch fires
// even though a recent message arrived (anti-infinite-deferral guarantee).
func TestMaxDebounceOverridesWhenChatNeverQuiets(t *testing.T) {
	st, _, _, inj, pusher := newTestPusher(t, "")
	pusher.cfg.DispatchDebounce = 60 * time.Second
	pusher.cfg.MaxDispatchDebounce = 30 * time.Second // tight ceiling for the test

	chat := "maxdebounce@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	// First message: 45s ago — older than MaxDispatchDebounce (30s).
	oldTS := time.Now().Unix() - 45
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m1", Text: "primero", TS: oldTS}); err != nil {
		t.Fatal(err)
	}
	// Second message: 5s ago — within debounce window, would normally defer.
	recentTS := time.Now().Unix() - 5
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m2", Text: "segundo", TS: recentTS}); err != nil {
		t.Fatal(err)
	}

	pusher.sweepOnce()

	if got := inj.count(); got != 1 {
		t.Errorf("Inject calls when oldest msg > MaxDispatchDebounce = %d, want 1 (forced dispatch)", got)
	}
}

// fakeInjectorPayload captures the payload string AND the envelope's from
// (ct-2026-07-18-1851-B: numero/is_boss moved out of the payload into from).
type fakeInjectorPayload struct {
	mu      sync.Mutex
	from    string
	payload string
}

func (f *fakeInjectorPayload) Inject(_, from, payload string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.from = from
	f.payload = payload
	return nil
}

func (f *fakeInjectorPayload) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.payload
}

func (f *fakeInjectorPayload) lastFrom() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.from
}

// TestBurstPreviewsMediaMarker verifies the media-inbound feature
// (ct-2026-07-14-0024): messages with a MIME Type get a [image]/[video]/
// [audio]/[document] prefix so the agent knows to call get_media, while
// text messages (Type "text" or empty) pass through unchanged.
func TestBurstPreviewsMediaMarker(t *testing.T) {
	burst := []store.Message{
		{Text: "hola", Type: "text"},
		{Text: "foto del perro", Type: "image/jpeg"},
		{Text: "", Type: "video/mp4"},
		{Text: "factura.pdf", Type: "application/pdf"},
		{Text: "voz", Type: "audio/ogg; codecs=opus"},
		{Text: "legacy", Type: ""},
	}
	got := burstPreviews(burst)
	if len(got) != len(burst) {
		t.Fatalf("len = %d, want %d", len(got), len(burst))
	}
	if got[0] != "hola" {
		t.Errorf("[0] text: got %q, want %q", got[0], "hola")
	}
	if !strings.HasPrefix(got[1], "[image] ") {
		t.Errorf("[1] image: got %q, want [image] prefix", got[1])
	}
	if !strings.HasPrefix(got[2], "[video] ") {
		t.Errorf("[2] video: got %q, want [video] prefix", got[2])
	}
	if !strings.HasPrefix(got[3], "[document] ") {
		t.Errorf("[3] doc: got %q, want [document] prefix", got[3])
	}
	if !strings.HasPrefix(got[4], "[audio] ") {
		t.Errorf("[4] audio: got %q, want [audio] prefix", got[4])
	}
	if got[5] != "legacy" {
		t.Errorf("[5] legacy empty type: got %q, want %q", got[5], "legacy")
	}
}

// TestDispatchTerminalGoneLogsPermanentNotChannelDown is T32's dispatch-level
// check (ct-2026-08-06-1109): a terminal_gone Inject error must NOT go
// through recordChannelDown's transient "canal caído" bookkeeping — that
// implies an eventual "canal recuperado" recovery line, which never comes
// for a genuinely dead credential (CleverInjector already discarded it, see
// clever_injector_test.go). Gets its own one-shot line explaining why
// instead.
func TestDispatchTerminalGoneLogsPermanentNotChannelDown(t *testing.T) {
	st, _, _, inj, pusher := newTestPusher(t, "")
	inj.setErr(errTerminalGone)
	chat := "55500000043@c.us"
	if err := st.TouchChat(chat, "C", 1); err != nil {
		t.Fatal(err)
	}
	dedicate(t, st, chat)
	if err := st.AddMessage(store.Message{ChatJID: chat, ID: "m1", Text: "hola", TS: 1}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })

	pusher.sweepOnce()

	out := buf.String()
	if !strings.Contains(out, "terminal_gone") || !strings.Contains(out, "credencial descartada") {
		t.Errorf("log doesn't explain the permanent reason, log = %q", out)
	}
	if strings.Contains(out, "canal caído") {
		t.Errorf("terminal_gone logged as transient \"canal caído\", want its own permanent line — log = %q", out)
	}
	if _, tracked := pusher.channelDownSince["port-fallback"]; tracked {
		t.Error("channelDownSince tracked for a terminal_gone failure — that bookkeeping implies a recovery line that will never come")
	}
}
