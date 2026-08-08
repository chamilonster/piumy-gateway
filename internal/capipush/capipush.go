// Package capipush is the dispatch pipeline: PendingDedicated -> coalesce
// by chat -> backpressure check -> RegisterDispatch (mcpserver.Gate) ->
// build the compact payload -> inject (the CleverCoder seam). New
// construction, no Piumy equivalent (F4-DESIGN.md §1).
//
// T28 (ct-2026-08-05-2242, boss decision): dispatches travel in the clear —
// cAPI (CleverCoder's external-agent protocol) already negotiates its own
// encrypted tunnel per terminal; a second layer inside that tunnel only
// protected the payload from CleverCoder itself, and CleverCoder is the
// boss's own program, on his own machine. internal/capi (AES-256-GCM) and
// cmd/agentclient (the agent-side decryptor) are gone, not disabled — no
// flag revives them. See docs/T28-DIAGRAMA-CAPI-SIN-CIFRADO.md.
package capipush

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	mrand "math/rand"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"piumy-gateway/internal/mcpserver"
	"piumy-gateway/internal/router"
	"piumy-gateway/internal/state"
	"piumy-gateway/internal/store"
)

// Injector is the seam to CleverCoder's actual prompt injection mechanism
// (.prompt/spawn) — piumy-gateway builds the payload, addresses it by
// terminal_id; CleverCoder injects. Kept as a small interface, same
// reasoning as gateway.Gateway: the real implementation lives outside this
// codebase entirely.
type Injector interface {
	// from is the envelope's dynamic sender identity (ct-2026-07-18-1851-B,
	// "<numero>, <nivel>" — nivel is boss/caution/danger) — piumy-gateway
	// computes it, the injector just carries it through to whatever
	// transport-level "from" field it has.
	Inject(terminalID, from, payload string) error
}

// ReadReceipter is the seam for sending WhatsApp read receipts (ct-2026-07-13-2131).
// Satisfied by gateway.Gateway. Anti-ban guards (kill/mute check via
// HaltedFn) are applied by the caller, not here.
type ReadReceipter interface {
	MarkRead(ctx context.Context, chatJID string, msgIDs []string) error
}

// LIDResolver resolves a @lid JID to its phone-number JID
// (ct-2026-07-18-1416) — whatsmeow.Adapter.ResolvePN implements it, the
// same seam restapi.LIDResolver already uses. nil, or a lookup that comes
// back unresolved, just falls back to the raw @lid string in the compact
// plaintext payload — never blocks a dispatch.
type LIDResolver interface {
	ResolvePN(ctx context.Context, lidJID string) (string, error)
}

// LogInjector is the default Injector: logs instead of delivering, so
// capipush is fully runnable (and testable end-to-end) before CleverCoder's
// real injector is wired in.
type LogInjector struct{}

func (LogInjector) Inject(terminalID, from, payload string) error {
	log.Printf("capipush: [seam not wired] would inject into terminal %s from=%q (%d bytes)", terminalID, from, len(payload))
	return nil
}

// FileInjector is a debug/smoke-test Injector (ct-2026-07-10-1814): appends
// each dispatch as "terminalID\tpayload\n" to Path, so an external test
// program with no real CleverCoder wiring can pick up the dispatch and
// drive the gate itself (the MCP tools). NOT the production default —
// main.go only wires this in when explicitly asked to (see its own doc),
// LogInjector stays the default otherwise.
type FileInjector struct {
	Path string
}

func (f FileInjector) Inject(terminalID, from, payload string) error {
	file, err := os.OpenFile(f.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	// from intentionally NOT written here — TestFileInjectorAppendsTabSeparatedLines
	// locks this wire format (terminalID\tpayload\n) for an external smoke
	// consumer; adding a field would shift what "split on the first tab" reads.
	_, err = fmt.Fprintf(file, "%s\t%s\n", terminalID, payload)
	return err
}

// Config are capipush's own tunables — package-local, independent of
// internal/config, same pattern as corepipeline.Config.
type Config struct {
	// SweepInterval is how often PendingDedicated is checked.
	SweepInterval time.Duration
	// SwampedAt is the RECENT (within SwampedWindow), non-boss pending
	// count at or above which capipush pauses dispatch to every non-boss
	// chat (the boss's own chat is exempt — S3, ct-2026-07-30-030948; see
	// store.CountRecentPendingNonBoss) — the agent falls back to draining
	// get_pending over MCP at its own pace. Overridden live by
	// store.SettingCapipushSwampedAt; this is only the code-level fallback
	// (CLAUDE.md: "cero hardcode"). Coincidentally the same default number
	// as state.Manager's OWN independent "swamped" mood threshold — a
	// cosmetic queue-depth face, unrelated mechanism, do not conflate them.
	SwampedAt int
	// SwampedWindow bounds which pending messages count toward SwampedAt —
	// only messages newer than SwampedWindow ago count; old debt (a chat
	// nobody's going to answer, months of backlog) must never block the
	// PRESENT (S3's own root cause: 76 of 82 pending messages in the smoke
	// were months old, in a chat nobody was answering). Overridden live by
	// store.SettingCapipushSwampedWindow. Zero/negative falls back to 10m.
	SwampedWindow time.Duration
	// PortFallback is the terminal identity to use when a chat's route
	// defines no explicit terminal_id (AGENT-BEHAVIOR.md: "puerto como
	// fallback si la ruta no lo define") — normally cfg.DefaultTerminalID.
	// Doubles as "the principal" (ct-2026-07-13-0302): dispatch()
	// unconditionally sends every LevelBoss chat here, never to a route's
	// terminal_id — the owner's messages always reach the main agent, no
	// per-chat routing needed.
	PortFallback string
	// DispatchLimit bounds how many pending messages one sweep reads
	// before grouping by chat (mirrors PendingDedicated's own limit knob).
	DispatchLimit int

	// Weights calibrate the usage estimate (F4-DESIGN §8) — sourced from
	// config, used both for dispatch's own input-side counters (read at
	// write time isn't needed there, only the raw counters are recorded)
	// and for the quota check below.
	Weights store.UsageWeights
	// DailyQuota is the GLOBAL (every chat, today) blended-usage ceiling —
	// at or over it, sweepOnce dispatches nothing this pass (same
	// backpressure shape as SwampedAt). Zero/negative disables the check.
	DailyQuota float64

	// MaxRedispatch caps how many times ONE still-unhandled message gets
	// re-dispatched before capipush holds it instead of trying again —
	// post-incident hardening (ct-2026-07-11-074123): nothing bounded this
	// before, so an agent that never calls mark_handled (crash, bug, a
	// helper that forgets) gets re-dispatched every sweep forever — the
	// exact shape of the 15-duplicate-send incident. Mirrors the outbox's
	// own retry_count/dead_letter pattern (internal/store/outbox.go),
	// applied to the dispatch side.
	//
	// S4b (ct-2026-07-30-1255): default moved 3 → 7. Redispatch attempts now
	// back off on Citrino's Fibonacci table (1,2,3,5,8,13 minutes,
	// redispatchBackoff) — 6 values = 6 GAPS = 7 total attempts, using the
	// whole table end to end ("no hace falta más que eso"). A delivery
	// FAILURE (Inject error — the channel is down) never increments this at
	// all (S4b defect 1: it used to, burning the whole budget on outages
	// instead of on an agent that ignores messages) — only a message that
	// was actually handed to the agent and still wasn't marked handled
	// counts here. Overridden live by store.SettingCapipushMaxRedispatch;
	// this is only the code-level fallback. Zero/negative falls back to 7.
	MaxRedispatch int
	// DispatchStaleAfter is the fallback default for mcpserver.Gate's own
	// stale-dispatch reclaim window (S4b, ct-2026-07-30-1255, defect 4) —
	// applied live each sweep via gate.SetStaleAfter (see sweepOnce), so it
	// can be tuned without a restart. The 1-hour original was "last-resort
	// net so it never happens", not "recovers fast" — too long for
	// messaging. Overridden live by store.SettingCapipushDispatchStaleAfter.
	// Zero/negative falls back to 15 minutes.
	DispatchStaleAfter time.Duration
	// HaltedFn returns true when outbound activity should be suppressed
	// (kill switch or mute) — ct-2026-07-13-2131: dispatch skips sending
	// the read receipt when this is true. nil = never halted.
	HaltedFn func() bool
	// DispatchDebounce is the silence window before a chat's burst is
	// dispatched (ct-2026-07-13-2243): capipush waits until no new message
	// has arrived for this long before sending — classic debounce, restarts
	// with each new message. A ±25% jitter is added per-sweep for natural
	// pacing (anti-ban). Zero/negative falls back to 60s.
	DispatchDebounce time.Duration
	// MaxDispatchDebounce is the hard ceiling on how long a burst can be
	// deferred — if the oldest message in the burst is older than this,
	// dispatch immediately regardless of recent activity (anti-infinite
	// deferral). Zero/negative falls back to 5m.
	MaxDispatchDebounce time.Duration
}

func (c Config) withDefaults() Config {
	if c.SweepInterval <= 0 {
		c.SweepInterval = 5 * time.Second
	}
	if c.SwampedAt <= 0 {
		c.SwampedAt = 8
	}
	if c.SwampedWindow <= 0 {
		c.SwampedWindow = 10 * time.Minute
	}
	if c.DispatchLimit <= 0 {
		c.DispatchLimit = 100
	}
	if c.MaxRedispatch <= 0 {
		c.MaxRedispatch = 7
	}
	if c.DispatchStaleAfter <= 0 {
		c.DispatchStaleAfter = 15 * time.Minute
	}
	if c.DispatchDebounce <= 0 {
		c.DispatchDebounce = 60 * time.Second
	}
	if c.MaxDispatchDebounce <= 0 {
		c.MaxDispatchDebounce = 5 * time.Minute
	}
	return c
}

// dispatchAnchor identifies one message for redispatch-tracking purposes
// (S4b, ct-2026-07-30-1255) — chatJID composite with the message ID, not
// the bare ID alone: a real WhatsApp message ID is globally unique so this
// never actually collides in production, but the composite key is what
// makes that an invariant instead of an assumption.
type dispatchAnchor struct {
	chatJID string
	msgID   string
}

// Pusher runs the sweep loop. The zero value is not usable — build one
// with New.
type Pusher struct {
	store  *store.Store
	router *router.Manager
	gate   *mcpserver.Gate
	cfg    Config

	// injectors maps terminal_id → Injector. injMu guards concurrent
	// access: sweepOnce (Run's goroutine) reads; RegisterInjector (MCP
	// tool goroutines) writes. The rest of Pusher's fields are still
	// sweepOnce-only and need no lock.
	injMu     sync.RWMutex
	injectors map[string]Injector

	// receipter fires read receipts after a successful dispatch — nil = skip.
	// Set via SetReceipter after New(); no lock needed (set once at startup
	// before Run starts).
	receipter ReadReceipter

	// lidResolver backs the compact plaintext payload's "numero" field
	// (ct-2026-07-18-1416) — nil = skip resolution, dispatch falls back to
	// the raw JID. Set via SetLIDResolver after New(); same
	// once-at-startup convention as receipter.
	lidResolver LIDResolver

	// state mirrors the backpressure gate into state.Status (S3,
	// ct-2026-07-30-030948) — the AGENT's own signal, complementing (not
	// replacing) the log transition. nil = skip (tests, or a build that
	// never wires it) — never required for capipush to function.
	state *state.Manager

	// redispatchCount tracks how many times each still-pending message has
	// been SUCCESSFULLY dispatched this process's lifetime — only
	// incremented after a successful Inject (S4b, ct-2026-07-30-1255,
	// defect 1: a delivery FAILURE must never consume this budget, only an
	// agent that received the message and still didn't handle it). Keyed by
	// dispatchAnchor{chatJID, msgID} — NOT bare msgID: a real WhatsApp
	// message ID is globally unique so this never matters in production,
	// but two DIFFERENT chats sharing a bare ID (any synthetic/test setup
	// that reuses "m1") must never share a counter. sweepOnce's own
	// goroutine is the only writer/reader (Run's ticker loop is sequential,
	// tests call sweepOnce synchronously too), so no lock needed. The
	// tracked message is burst[len(burst)-1] — the NEWEST unhandled message
	// per chat (S4b defect 2: anchoring to the oldest let one stuck message
	// block every newer, never-tried sibling forever).
	redispatchCount map[dispatchAnchor]int

	// lastDispatchAt stamps the last SUCCESSFUL dispatch per tracked
	// anchor (same key as redispatchCount) — redispatchBackoff uses this to
	// space out redispatch attempts on Citrino's Fibonacci table instead of
	// retrying every sweep (S4b, ct-2026-07-30-1255, defect 3). Same
	// single-goroutine, no-lock convention as redispatchCount.
	lastDispatchAt map[dispatchAnchor]time.Time

	// logState tracks the last logged boolean for sweep-gated conditions
	// (quota, backpressure, per-chat debounce, per-terminal no-antenna /
	// in-flight, channel-down) — same single-goroutine, no-lock convention
	// as redispatchCount. See logTransition.
	logState map[string]bool

	// channelDownSince/channelDownFails back the channel-down transition
	// (S4c, ct-2026-07-30-1512), keyed by terminalID — a downed antenna
	// fails EVERY chat routed through it identically, so tracking per-chat
	// would still multiply the noise the live 14min cut exposed (306 log
	// lines, 153 of them duplicates; ~63k projected over the boss's 48h
	// resilience scenario). Since holds the moment Inject first failed
	// (for the recovery line's duration); Fails counts attempts since then
	// (for the recovery line's count). Same single-goroutine convention.
	channelDownSince map[string]time.Time
	channelDownFails map[string]int
}

// New builds a Pusher. injector is the initial Injector for PortFallback
// (the principal terminal). nil falls back to LogInjector so dispatches are
// never silently dropped — a LogInjector logs instead of delivering.
func New(st *store.Store, rt *router.Manager, gate *mcpserver.Gate, injector Injector, cfg Config) *Pusher {
	if injector == nil {
		injector = LogInjector{}
	}
	cfg = cfg.withDefaults()
	injectors := map[string]Injector{}
	if cfg.PortFallback != "" {
		injectors[cfg.PortFallback] = injector
	}
	return &Pusher{
		store: st, router: rt, gate: gate,
		cfg:              cfg,
		injectors:        injectors,
		redispatchCount:  map[dispatchAnchor]int{},
		lastDispatchAt:   map[dispatchAnchor]time.Time{},
		logState:         map[string]bool{},
		channelDownSince: map[string]time.Time{},
		channelDownFails: map[string]int{},
	}
}

// SetReceipter wires the read-receipt seam (ct-2026-07-13-2131). Call once
// at startup before Run; not safe for concurrent use after that.
func (p *Pusher) SetReceipter(r ReadReceipter) { p.receipter = r }

// SetLIDResolver wires the @lid→número resolution seam for the compact
// plaintext payload (ct-2026-07-18-1416). Call once at startup before Run;
// not safe for concurrent use after that.
func (p *Pusher) SetLIDResolver(r LIDResolver) { p.lidResolver = r }

// SetState wires the state.Manager the backpressure gate signals into (S3,
// ct-2026-07-30-030948). Call once at startup before Run; not safe for
// concurrent use after that. nil (never called) just skips the signal.
func (p *Pusher) SetState(sm *state.Manager) { p.state = sm }

// RegisterInjector registers (or replaces) the Injector for agentID.
// Safe to call concurrently with sweepOnce.
// The principal slot (PortFallback) is immutable post-New: any call with
// agentID == cfg.PortFallback is a no-op — the principal's injector is
// wired once at startup and must never be overwritten by a secondary.
func (p *Pusher) RegisterInjector(agentID string, inj Injector) {
	if p.cfg.PortFallback != "" && agentID == p.cfg.PortFallback {
		return
	}
	p.injMu.Lock()
	defer p.injMu.Unlock()
	p.injectors[agentID] = inj
}

// UnregisterInjector removes agentID's live injector — a deleted agent must
// stop being dispatchable immediately, not just disappear from the `agents`
// table while its old credentials keep working in memory (ct-2026-07-29,
// boss: "un borrado que deja las credenciales vivas es un borrado que
// miente"). Safe to call concurrently with sweepOnce. The principal slot is
// immutable here too, same as RegisterInjector — there is no path that
// deletes the principal (restapi's agent-delete handler rejects it), but
// the guard costs nothing and keeps the invariant explicit rather than
// implicit. After this call, injectorFor(agentID) falls back to
// LogInjector and dispatch's own agent_exclusive robustness guard
// (capipush.go's dispatch, InjectorFor check) routes past it to router.json
// or PortFallback — restapi's delete handler is expected to ALSO clear any
// chats.status still pointing at agentID (SetStatus back to "new"), so
// dispatch never even reaches this fallback for THIS agent's old chats.
func (p *Pusher) UnregisterInjector(agentID string) {
	if p.cfg.PortFallback != "" && agentID == p.cfg.PortFallback {
		return
	}
	p.injMu.Lock()
	defer p.injMu.Unlock()
	delete(p.injectors, agentID)
}

// injectorFor returns the registered Injector for terminalID, or LogInjector
// if none is registered (dispatch to an unknown terminal is logged, not dropped).
func (p *Pusher) injectorFor(terminalID string) Injector {
	p.injMu.RLock()
	defer p.injMu.RUnlock()
	if inj, ok := p.injectors[terminalID]; ok {
		return inj
	}
	return LogInjector{}
}

// InjectorFor is injectorFor's exported counterpart (M2, ct-2026-07-22-1301,
// the dashboard's per-agent ping) — ok reports whether agentID has an
// actually-registered injector, as opposed to injectorFor's own
// LogInjector fallback (which would silently "succeed" a ping that never
// reaches anywhere). agentID is the SAME key RegisterInjector/OnAgentUpsert
// already use — the principal's own PortFallback for the principal, each
// secondary's agent_id for the rest (see New()/agent_tools.go).
func (p *Pusher) InjectorFor(agentID string) (Injector, bool) {
	p.injMu.RLock()
	defer p.injMu.RUnlock()
	inj, ok := p.injectors[agentID]
	return inj, ok
}

// Run sweeps periodically until ctx is cancelled.
func (p *Pusher) Run(ctx context.Context) {
	ticker := time.NewTicker(p.cfg.SweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.sweepOnce()
		}
	}
}

// sweepOnce is one dispatch pass, callable directly from tests (same
// package) for a single deterministic pass instead of waiting on a ticker.
func (p *Pusher) sweepOnce() {
	// S4b (ct-2026-07-30-1255, defect 4): refresh the gate's stale-dispatch
	// reclaim window from live settings every sweep — cheap (a mutex lock +
	// assignment) and reuses SetStaleAfter, already designed to be tuned
	// without a rebuild, instead of giving Gate its own store dependency.
	p.gate.SetStaleAfter(p.store.SettingDuration(store.SettingCapipushDispatchStaleAfter, p.cfg.DispatchStaleAfter))

	if p.cfg.DailyQuota > 0 {
		total, err := p.store.TotalUsageToday(p.cfg.Weights)
		if err != nil {
			log.Printf("capipush: quota check: %v", err)
		} else {
			exceeded := total >= p.cfg.DailyQuota
			p.logTransition("quota", exceeded,
				func() {
					log.Printf("capipush: cuota diaria excedida (%.1f >= %.1f) — despacho pausado", total, p.cfg.DailyQuota)
				},
				func() {
					log.Printf("capipush: cuota diaria liberada (%.1f < %.1f) — despacho reanudado", total, p.cfg.DailyQuota)
				},
			)
			if exceeded {
				// Cuota de cuenta (F4-DESIGN §8): over quota, dispatch NOTHING
				// this sweep, boss included — messages stay queued. Global
				// (every chat), single-account for now. Unlike the swamped
				// backpressure below (S3, ct-2026-07-30-030948), this gate
				// does NOT exempt the boss's chat — out of S3's scope, left
				// as-is.
				return
			}
		}
	}

	pending, err := p.dueChats()
	if err != nil {
		log.Printf("capipush: sweep: %v", err)
		return
	}

	// Backpressure (F4-DESIGN §1, redesigned S3 ct-2026-07-30-030948): only
	// RECENT, non-boss pending traffic counts — old debt never blocks the
	// present, and the boss's own message volume never throttles anyone
	// else (store.CountRecentPendingNonBoss). Settings override the
	// code-level defaults live (CLAUDE.md: cero hardcode).
	swampedAt := p.store.SettingInt(store.SettingCapipushSwampedAt, p.cfg.SwampedAt)
	swampedWindow := p.store.SettingDuration(store.SettingCapipushSwampedWindow, p.cfg.SwampedWindow)
	recentNonBoss, err := p.store.CountRecentPendingNonBoss(time.Now().Add(-swampedWindow).Unix())
	if err != nil {
		log.Printf("capipush: backpressure count: %v", err)
		recentNonBoss = 0 // fail open this sweep, same as the quota check above
	}
	swamped := recentNonBoss >= swampedAt
	p.logTransition("swamped", swamped,
		func() {
			log.Printf("capipush: backpressure activado (%d >= %d mensajes recientes no-boss en %s) — despacho a chats no-boss pausado hasta que el agente drene (el chat del boss nunca se frena)", recentNonBoss, swampedAt, swampedWindow)
		},
		func() {
			log.Printf("capipush: backpressure liberado (%d < %d mensajes recientes no-boss en %s) — despacho reanudado", recentNonBoss, swampedAt, swampedWindow)
		},
	)
	p.signalBackpressure(swamped, recentNonBoss, swampedAt)

	p.pruneStaleState(pending)
	now := time.Now()
	for chatJID, burst := range pending {
		if swamped {
			c, ok, err := p.store.GetChat(chatJID)
			if err != nil {
				log.Printf("capipush: backpressure chat check %s: %v", chatJID, err)
				continue
			}
			if !ok || !c.IsBoss {
				// Never ahoga (chokes) the terminal — every OTHER chat waits
				// for the agent to drain get_pending at its own pace. The
				// boss's own chat falls through unblocked (below).
				continue
			}
		}
		if p.isDebounced(burst, now) {
			p.logTransition("debounce:"+chatJID, true, func() {
				log.Printf("capipush: chat %s en debounce (%d mensajes en burst, esperando silencio)", chatJID, len(burst))
			}, nil)
			continue
		}
		delete(p.logState, "debounce:"+chatJID)
		if err := p.dispatch(chatJID, burst); err != nil {
			log.Printf("capipush: dispatch %s: %v", chatJID, err)
		}
	}
}

// signalBackpressure mirrors the swamped state into state.Status (S3,
// ct-2026-07-30-030948) — the AGENT's own signal (get_status embeds
// Status), complementing logTransition's log above, not replacing it: a
// log is for humans reading the gateway's output, get_status is what the
// agent can actually read. Runs every sweep (not gated by logTransition)
// so the count in BackpressureReason stays current while steady-swamped,
// not frozen at whatever it was on the sweep that first triggered it.
func (p *Pusher) signalBackpressure(swamped bool, count, threshold int) {
	if p.state == nil {
		return
	}
	_ = p.state.Update(func(s *state.Status) {
		s.Backpressure = swamped
		if swamped {
			s.BackpressureReason = fmt.Sprintf("%d mensajes recientes no-boss pendientes (umbral %d) — despacho a chats no-boss pausado, drená get_pending a tu ritmo. El chat del boss nunca se frena.", count, threshold)
		} else {
			s.BackpressureReason = ""
		}
	})
}

// logTransition logs a state entering/leaving `active` exactly once — the
// 5s sweep ticker would otherwise repeat the same line forever while a
// condition (backpressure, debounce, no antenna registered, terminal busy)
// holds steady. onEnter/onExit may be nil to log only one direction.
func (p *Pusher) logTransition(key string, active bool, onEnter, onExit func()) {
	if active == p.logState[key] {
		return
	}
	if active {
		if onEnter != nil {
			onEnter()
		}
		p.logState[key] = true
	} else {
		if onExit != nil {
			onExit()
		}
		delete(p.logState, key)
	}
}

// recordChannelDown logs the channel-down transition exactly once
// (logTransition, keyed by terminalID) and tracks the failed-attempt count
// for recordChannelRecovered's exit line — "canal caído" is a sustained
// state, not a per-sweep event (S4c, ct-2026-07-30-1512). The entry line
// keeps the chat that first surfaced it and the exact cause (e.g. "handshake
// status 404") — that cause was S1's own payoff, the one thing that let the
// live cut get diagnosed without opening the code.
func (p *Pusher) recordChannelDown(terminalID, chatJID, level string, cause error) {
	p.channelDownFails[terminalID]++
	p.logTransition("channelDown:"+terminalID, true, func() {
		p.channelDownSince[terminalID] = time.Now()
		log.Printf("capipush: canal caído terminal=%s (detectado en chat=%s, nivel=%s): %v", terminalID, chatJID, level, cause)
	}, nil)
}

// recordChannelRecovered logs how long the channel was down and how many
// attempts failed — real operational data ("estuvo caído 14 min, 153
// intentos") that only exists at the moment the state ends, and didn't exist
// anywhere before S4c. No-op if terminalID wasn't in the down state.
func (p *Pusher) recordChannelRecovered(terminalID string) {
	since, wasDown := p.channelDownSince[terminalID]
	if !wasDown {
		return
	}
	log.Printf("capipush: canal recuperado terminal=%s — estuvo caído %s, %d intentos fallidos", terminalID, time.Since(since).Round(time.Second), p.channelDownFails[terminalID])
	delete(p.channelDownSince, terminalID)
	delete(p.channelDownFails, terminalID)
	delete(p.logState, "channelDown:"+terminalID)
}

// isDebounced returns true when a chat's burst should be held back to wait
// for more messages (ct-2026-07-13-2243). The window is DispatchDebounce +
// uniform(0, debounce/4) jitter for natural, variable pacing (boss verbatim:
// "ventanas de tiempo variables"). The check is bypassed if the oldest
// message in the burst has been waiting longer than MaxDispatchDebounce —
// anti-infinite-deferral guarantee.
func (p *Pusher) isDebounced(burst []store.Message, now time.Time) bool {
	if len(burst) == 0 {
		return false
	}
	first := time.Unix(burst[0].TS, 0)
	if now.Sub(first) >= p.cfg.MaxDispatchDebounce {
		return false
	}
	jitter := time.Duration(mrand.Int63n(int64(p.cfg.DispatchDebounce / 4)))
	effective := p.cfg.DispatchDebounce + jitter
	last := time.Unix(burst[len(burst)-1].TS, 0)
	return now.Sub(last) < effective
}

// fibonacciBackoffMinutes are S4b's redispatch backoff steps (ct-2026-07-
// 30-1255 — the boss's own idea, verbatim: "que tal una tabla con tiempos
// fibonacci?"). A real agent reads, decides, and drafts over MINUTES —
// retrying every 5s sweep (the old behavior) burned through the whole
// redispatch budget in 15 seconds, before the agent had even started
// thinking. 6 values = 6 gaps between attempts = 7 total dispatches
// (Config.MaxRedispatch's new default) — the table is used end to end,
// "no hace falta más que eso" (Citrino).
var fibonacciBackoffMinutes = []int{1, 2, 3, 5, 8, 13}

// redispatchBackoff returns how long to wait before the (attempt+1)th
// redispatch of a still-unhandled message — attempt is the message's
// CURRENT redispatchCount, always >= 1 when called (a first-ever dispatch,
// attempt 0, is never backed off). Never grows past the table's last value
// (13m) even if MaxRedispatch is configured higher than 7 — same "no hace
// falta más" ceiling, applied defensively. ±25% jitter matches
// isDebounced's own pacing (S4b, Citrino: gateway-internal traffic has no
// anti-ban reason for jitter, but it's more consistent this way).
func redispatchBackoff(attempt int) time.Duration {
	if attempt > len(fibonacciBackoffMinutes) {
		attempt = len(fibonacciBackoffMinutes)
	}
	base := time.Duration(fibonacciBackoffMinutes[attempt-1]) * time.Minute
	jitter := time.Duration(mrand.Int63n(int64(base / 4)))
	return base + jitter
}

// pruneStaleState drops per-message and per-chat tracking no longer backed
// by this sweep's pending set — the common case is mark_handled having
// landed since the last sweep, at which point the entry is dead weight;
// without this, redispatchCount/lastDispatchAt/logState grow for every
// message/chat capipush has EVER touched, never just the ones still
// eligible to be. live is keyed by dispatchAnchor{chatJID, newest message
// ID} — S4b's own anchor (ct-2026-07-30-1255, defect 2) — matching
// dispatch()'s own anchor exactly.
func (p *Pusher) pruneStaleState(pending map[string][]store.Message) {
	live := make(map[dispatchAnchor]bool, len(pending))
	for chatJID, burst := range pending {
		if len(burst) > 0 {
			live[dispatchAnchor{chatJID, burst[len(burst)-1].ID}] = true
		}
	}
	for anchor := range p.redispatchCount {
		if !live[anchor] {
			delete(p.redispatchCount, anchor)
		}
	}
	for anchor := range p.lastDispatchAt {
		if !live[anchor] {
			delete(p.lastDispatchAt, anchor)
		}
	}
	for key := range p.logState {
		if chatJID, ok := strings.CutPrefix(key, "debounce:"); ok {
			if _, stillPending := pending[chatJID]; !stillPending {
				delete(p.logState, key)
			}
			continue
		}
		if rest, ok := strings.CutPrefix(key, "backoff:"); ok {
			chatJID, msgID, found := strings.Cut(rest, "\x00")
			if !found || !live[dispatchAnchor{chatJID, msgID}] {
				delete(p.logState, key)
			}
		}
	}
}

// dueChats groups PendingDedicated by chat — the coalescing (F4-DESIGN §1:
// "burst -> 1 inyección"): a chat with several unhandled messages produces
// exactly one dispatch per sweep carrying ALL burst messages (ct-2026-07-13-2131:
// the agent must see every message, not just the last preview). Messages
// arrive ts ASC from PendingDedicated, so burst[0] is oldest, burst[n-1] newest.
func (p *Pusher) dueChats() (map[string][]store.Message, error) {
	msgs, err := p.store.PendingDedicated(p.cfg.DispatchLimit)
	if err != nil {
		return nil, err
	}
	byChat := make(map[string][]store.Message, len(msgs))
	for _, m := range msgs {
		byChat[m.ChatJID] = append(byChat[m.ChatJID], m)
	}
	return byChat, nil
}

// resolveReplyTarget is T43 (ct-2026-08-08-2043): if the newest message in
// burst — the one that just triggered this sweep — quotes a message a
// registered agent sent via send_to_boss (messages.origin_terminal_id,
// T39), the reply routes back to THAT agent's terminal. ok=false for any
// other case (not a reply, the quoted row doesn't exist, it wasn't
// agent-authored, or that agent no longer has a live injector) — the
// caller then falls through to today's unchanged precedence instead of
// stranding the message on a dead terminal_id.
func (p *Pusher) resolveReplyTarget(chatJID string, burst []store.Message) (string, bool) {
	if len(burst) == 0 {
		return "", false
	}
	quotedID := burst[len(burst)-1].QuotedID
	if quotedID == "" {
		return "", false
	}
	quoted, found, err := p.store.GetMessageByID(chatJID, quotedID)
	if err != nil || !found || quoted.OriginTerminalID == "" {
		return "", false
	}
	if _, injOK := p.InjectorFor(quoted.OriginTerminalID); !injOK {
		return "", false
	}
	return quoted.OriginTerminalID, true
}

// dispatch registers one dispatch for chatJID's terminal and hands the
// encrypted envelope (or plaintext JSON) to the injector. burst is the
// full ordered list of unhandled messages for this chat (ts ASC, so
// burst[0] is oldest). All burst message texts are included in the payload
// (ct-2026-07-13-2131) so the agent sees every message, not just the last.
func (p *Pusher) dispatch(chatJID string, burst []store.Message) error {
	c, ok, err := p.store.GetChat(chatJID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("chat %s vanished between PendingDedicated and GetChat", chatJID)
	}
	level := LevelFor(c)

	// ct-2026-07-13-0302: is_boss (LevelBoss) always dispatches to the
	// principal (PortFallback) — the owner's messages are never subject to
	// per-chat routing, no route can send them anywhere else. Routing by
	// terminal_id is for below-boss levels only (a future "suplente" agent,
	// ct-2026-07-13-0242's later scope) — "is_boss ⟹ principal" has to hold
	// with zero router.json configuration, not just as the common case.
	//
	// M4 (ct-2026-07-22-1301) — agent_exclusive precedence, boss-ratified:
	// (1) boss -> principal ALWAYS (below) > (2) agent_exclusive: <id>
	// (manual assignment, M3) -> that agent > (3) router.json's route ->
	// today's fallback, unchanged > (4) neither -> PortFallback. c.Status
	// was already WRITABLE in this exact form since chat.go's SetStatus/
	// set_chat_status (validChatStatus) but never READ by dispatch until
	// now. terminalID becomes agentID directly — the SAME identity space
	// injectorFor's map key already is (RegisterInjector/OnAgentUpsert key
	// by agent_id, not a separate "antenna terminal" — confirmed against
	// main.go:192/235 with Citrino before wiring this).
	//
	// Robustness guard (Citrino): an agent_exclusive pointing at an agentID
	// with NO real injector registered (assigned but never actually
	// configured with cAPI credentials) must NOT silently vanish via
	// injectorFor's own LogInjector fallback below — that would strand the
	// message forever, since re-sweeping resolves the exact same dead
	// agentID every time. Instead it falls through to precedence (3), the
	// SAME fallback an unassigned chat gets — InjectorFor (the exported,
	// ok-reporting sibling of injectorFor) is the check.
	//
	// T43 (ct-2026-08-08-2043) adds precedence (0), ABOVE all of the above
	// including boss->principal: boss verbatim "si yo le respondo a un
	// mensaje de un agente, se le responda a ese terminal... en un chat
	// puedo tener diferentes destinos, dependiendo a quién le respondo". A
	// reply to a message an agent sent via send_to_boss must reach that
	// agent even from an is_boss chat — otherwise (1) always wins and a
	// reply could never leave the principal.
	terminalID := p.cfg.PortFallback
	if replyTerminalID, ok := p.resolveReplyTarget(chatJID, burst); ok {
		terminalID = replyTerminalID
	} else if level != mcpserver.LevelBoss {
		resolvedByAssignment := false
		if agentID, ok := store.AgentExclusiveID(c.Status); ok {
			if _, injOK := p.InjectorFor(agentID); injOK {
				terminalID = agentID
				resolvedByAssignment = true
			}
		}
		if !resolvedByAssignment && p.router != nil {
			if tid := p.router.Resolve(chatJID).TerminalID; tid != "" {
				terminalID = tid
			}
		}
	}
	if terminalID == "" {
		return fmt.Errorf("no terminal_id for %s (no route, no port fallback configured)", chatJID)
	}
	// ct-2026-07-13-1822: LogInjector = sin antena real. Skip sin gate
	// registration, sin Inject: el mensaje queda en PendingDedicated para
	// que el agente lo levante vía get_pending/get_queue por MCP. La
	// transición a este estado sí se loguea (ct-2026-07-30-0309, S1).
	// S6 (ct-2026-07-30-031048): "no antenna" now covers TWO shapes, not
	// just LogInjector — main.go always registers the principal's live
	// *CleverInjector for PortFallback (never LogInjector), even before it
	// has real credentials, so set_capi_connector's SetConfig always
	// reaches the object dispatch() actually uses (no more orphaned
	// instance nothing ever swaps in). An unconfigured CleverInjector
	// (empty endpoint) must be treated the SAME as LogInjector here — quiet
	// retention, not a real Inject() attempt against an empty endpoint.
	inj := p.injectorFor(terminalID)
	_, isLogInjector := inj.(LogInjector)
	cr, hasConfiguredCheck := inj.(interface{ Configured() bool })
	if isLogInjector || (hasConfiguredCheck && !cr.Configured()) {
		p.logTransition("noAntenna:"+terminalID, true, func() {
			log.Printf("capipush: terminal %s sin antena registrada — despacho retenido (chat %s queda en PendingDedicated para MCP-pull)", terminalID, chatJID)
		}, nil)
		return nil
	}
	delete(p.logState, "noAntenna:"+terminalID)
	if p.gate.InFlight(terminalID) {
		// Refinement, not the security guarantee (that's RegisterDispatch's
		// force-replace, gate.go): don't interrupt a terminal's legitimate
		// in-flight work with a fresh dispatch just because another chat's
		// message is now due — reduces churn on top of the per-chat
		// coalescing above. Skipping here is always safe: the next sweep
		// picks this chat back up once the terminal frees up (or the
		// in-flight dispatch times out via the gate's own stale sweep).
		p.logTransition("inFlight:"+terminalID, true, func() {
			log.Printf("capipush: terminal %s ocupado (dispatch en vuelo) — despacho a chat %s postergado", terminalID, chatJID)
		}, nil)
		return nil
	}
	delete(p.logState, "inFlight:"+terminalID)

	// Redispatch cap anchored to the NEWEST message in the burst (S4b,
	// ct-2026-07-30-1255, defect 2 — anchoring to the oldest let one stuck
	// message block every newer, never-tried sibling forever; a genuinely
	// new message resets the anchor and gets its own fresh attempts, while
	// a chat that stops receiving anything new still caps exactly as
	// before, since newest==oldest for a single-message burst).
	anchorMsg := burst[len(burst)-1]
	anchor := dispatchAnchor{chatJID, anchorMsg.ID}
	backoffLogKey := "backoff:" + chatJID + "\x00" + anchorMsg.ID
	maxRedispatch := p.store.SettingInt(store.SettingCapipushMaxRedispatch, p.cfg.MaxRedispatch)
	attempt := p.redispatchCount[anchor]
	if attempt >= maxRedispatch {
		// The containment (ct-2026-07-11-074123, recalibrated S4b): a
		// message that's been SUCCESSFULLY dispatched MaxRedispatch times
		// without ever getting mark_handled stops going out — an agent bug
		// becomes one contained incident instead of an unbounded flood.
		// Stays in PendingDedicated (still visible via get_pending/
		// get_messages for manual recovery); this only stops the AUTOMATIC
		// re-push. Never incremented by a delivery FAILURE (see below) —
		// only by an attempt the agent actually received.
		log.Printf("capipush: message %s (chat %s) hit the redispatch cap (%d) — holding, not auto-dispatching again", anchorMsg.ID, chatJID, maxRedispatch)
		return nil
	}
	// S4b defect 3: back off on Citrino's Fibonacci table instead of
	// retrying every 5s sweep — a real agent needs minutes, not seconds,
	// to read/decide/draft. attempt==0 (never dispatched yet) skips this
	// entirely — a first attempt is never backed off.
	if attempt > 0 {
		if lastAt, ok := p.lastDispatchAt[anchor]; ok {
			if wait := redispatchBackoff(attempt); time.Since(lastAt) < wait {
				p.logTransition(backoffLogKey, true, func() {
					log.Printf("capipush: mensaje %s (chat %s) en backoff (intento %d de %d, %s desde el último) — reintento diferido para darle tiempo al agente", anchorMsg.ID, chatJID, attempt+1, maxRedispatch, wait.Round(time.Second))
				}, nil)
				return nil
			}
		}
	}
	delete(p.logState, backoffLogKey)

	nonce, err := p.newNonce()
	if err != nil {
		return err
	}

	// from (ct-2026-07-18-1851-B): the envelope's "de:" line — numero/is_boss.
	from := p.envelopeFrom(chatJID, level)

	// Burst previews: per-message text scaled to keep the total payload
	// within the cAPI protocol's 4096-char limit (see burstPreviews).
	texts := burstPreviews(burst)

	payload := p.dispatchPayload(chatJID, level, nonce, texts)

	if err := p.gate.RegisterDispatch(nonce, chatJID, level, terminalID, burst[len(burst)-1].TS); err != nil {
		return err
	}

	// Input/dispatch usage counter (F4-DESIGN §8: "contadores... en
	// capipush (input/dispatch)") — best-effort, never blocks the actual
	// dispatch on a metering write failure. Counts all burst chars.
	totalInChars := 0
	for _, m := range burst {
		totalInChars += len(m.Text)
	}
	if err := p.store.AddUsage(chatJID, store.Today(), store.UsageDelta{InChars: totalInChars, Messages: 1}); err != nil {
		log.Printf("capipush: meter dispatch %s: %v", chatJID, err)
	}

	// H5 hardening (ct-2026-07-10-0540): Inject is the last thing that can
	// still fail after the dispatch is registered — revert it immediately
	// (CancelDispatch) instead of leaving the terminal wedged. The message
	// stays in PendingDedicated (never consumed), so the next sweep retries
	// it — same as any other dispatch error path in this function.
	//
	// S4b (ct-2026-07-30-1255, defect 1): redispatchCount/lastDispatchAt are
	// deliberately NOT touched on this path — a delivery failure (the
	// channel is down) must never consume the "agent didn't handle"
	// containment budget. With the old code incrementing BEFORE Inject, a
	// 15-second outage burned all 3 (old default) attempts before the
	// message ever reached anyone; now it retries every sweep, unbounded,
	// until the channel comes back — the boss's own requirement ("tiene que
	// ser resilente si se corta 48 horas").
	if err := p.injectorFor(terminalID).Inject(terminalID, from, payload); err != nil {
		// T32 (ct-2026-08-06-1109): terminal_gone is the one PERMANENT
		// handshake failure (protocol §2, ct-2026-08-06-0221) — the injector
		// already discarded its own credential (CleverInjector.markDead), so
		// every future sweep skips it quietly via the "sin antena" path
		// above. This is the one-shot line saying why: recordChannelDown's
		// transient "canal caído" framing (below) implies a recovery line
		// will follow, which never happens here — a dead credential doesn't
		// come back on its own, only a fresh SetConfig fixes it.
		// antenna_off/position_empty (both transient) and any code this
		// side doesn't recognize (older CleverCoder, or a future one) fall
		// through unchanged to the generic retry below — same behavior as
		// before this contract.
		if errors.Is(err, errTerminalGone) {
			log.Printf("capipush: terminal %s ya no existe (terminal_gone) — credencial descartada, no se reintenta más contra este agente (chat %s queda en PendingDedicated para recuperación manual)", terminalID, chatJID)
			p.gate.CancelDispatch(nonce, terminalID)
			return nil
		}
		p.recordChannelDown(terminalID, chatJID, level, err)
		p.gate.CancelDispatch(nonce, terminalID)
		// S4c (ct-2026-07-30-1512): logged above via recordChannelDown, not
		// propagated — sweepOnce's own generic log line would otherwise
		// repeat the exact same failure every 5s sweep for as long as the
		// channel stays down (a real 14min cut produced 306 lines, 153 of
		// them this same duplication). The message stays in
		// PendingDedicated either way (never consumed), so the next sweep
		// retries it — unchanged from before.
		return nil
	}
	p.recordChannelRecovered(terminalID)
	p.redispatchCount[anchor] = attempt + 1
	p.lastDispatchAt[anchor] = time.Now()

	// ct-2026-07-13-2131: honest read receipt — the burst coalesces into ONE
	// MarkRead call (all IDs together, no burst). Anti-ban: skipped when
	// kill/mute is active (HaltedFn). Best-effort: a receipt failure is
	// logged but never propagates as a dispatch error.
	if p.receipter != nil && (p.cfg.HaltedFn == nil || !p.cfg.HaltedFn()) {
		ids := make([]string, len(burst))
		for i, m := range burst {
			ids[i] = m.ID
		}
		if err := p.receipter.MarkRead(context.Background(), chatJID, ids); err != nil {
			log.Printf("capipush: mark read chat=%s: %v", chatJID, err)
		}
	}

	log.Printf("capipush: despacho OK chat=%s terminal=%s nivel=%s mensajes=%d nonce=%s", chatJID, terminalID, level, len(burst), nonce)
	return nil
}

// envelopeFrom builds the envelope's dynamic "de:" identity (ct-2026-07-18-
// 1851-B, boss: shorten the dispatch — numero/tipo move up to the header
// CleverCoder itself renders, out of the body). "<numero>, <tipo>" — tipo is
// the dispatch's own system level (LevelBoss/LevelCaution/LevelDanger,
// "boss"/"caution"/"danger" verbatim; boss explicitly rejected "is_boss" as
// the label, wants the real level, not a boss/not-boss binary). numero
// resolves via lidResolver when chatJID is a @lid (ct-2026-07-18-1416's
// ResolvePN seam) — falls back to the raw JID's user part if unresolved or
// no resolver is wired.
func (p *Pusher) envelopeFrom(chatJID, level string) string {
	numero := jidNumber(chatJID)
	if store.IsLIDJID(chatJID) && p.lidResolver != nil {
		if pn, err := p.lidResolver.ResolvePN(context.Background(), chatJID); err != nil {
			log.Printf("capipush: resolve %s for envelope from: %v", chatJID, err)
		} else if pn != "" {
			numero = jidNumber(pn)
		}
	}
	return fmt.Sprintf("%s, %s", numero, level)
}

// newNonce generates a short 4-hex-char dispatch correlation id
// (ct-2026-07-18-1851-B, boss: "bajemoslo a 4 dijitos exadecimal... NC:8f9a").
// The anti-replay guarantee comes from the gate's one-shot consumption plus
// cAPI's own tunnel encryption (CleverCoder's, negotiated per terminal —
// T28, ct-2026-08-05-2242), not nonce entropy — 4 hex is just a short,
// human-legible correlation id. Regenerates on a collision against
// currently active dispatches (rare: with few dispatches in flight at
// once, the 65536-value space is generous).
func (p *Pusher) newNonce() (string, error) {
	for {
		nonce, err := randomHex(2)
		if err != nil {
			return "", err
		}
		if !p.gate.NonceActive(nonce) {
			return nonce, nil
		}
	}
}

// dispatchPayload builds the compact dispatch body (ct-2026-07-18-1416,
// further shortened ct-2026-07-18-1851-B: numero/nivel moved to the
// envelope's "de:" — see envelopeFrom). The nonce stays as the signature
// line at the END, "NC:<4hex>" (shortened from a full hex nonce, same
// reasoning as newNonce's doc) — get_instructions(nonce) needs it
// verbatim. Was one of two branches (the other AES-256-GCM-encrypted)
// until T28 (ct-2026-08-05-2242) removed the encrypted one — this is the
// only payload now, name simplified to match.
//
// T15 (ct-2026-08-05-123241, Citrino: "el motivo tiene que viajar con el
// mensaje, no aparte") — if chatJID has an outstanding rejected draft
// (store.PendingRejectionNote), its reason + previous text are prepended
// ahead of everything else, same store-lookup-at-dispatch-time pattern the
// rules.md block below already uses. Unconditional on level: a rejected
// draft can belong to any chat, boss included.
//
// Identity line (ct-2026-08-06, boss verbatim: "si soy boss tiene que
// decir is boss, y si no, el preámbulo son las reglas. Todo mensaje con su
// preámbulo") — ALWAYS present, unconditional on level: before this, the
// boss's own dispatch carried NEITHER the identity nor its rules (the old
// `if !isBoss` below skipped the whole block), leaving the agent to guess
// who it was talking to. Rules now ride along for the boss too — the same
// omission, same fix.
func (p *Pusher) dispatchPayload(chatJID, level, nonce string, texts []string) string {
	isBoss := level == mcpserver.LevelBoss

	var b strings.Builder
	if reason, prevText, ok, err := p.store.PendingRejectionNote(chatJID); err != nil {
		log.Printf("capipush: pending rejection note for dispatch %s: %v", chatJID, err)
	} else if ok {
		fmt.Fprintf(&b, "MOTIVO DE RECHAZO: %s\nTu borrador anterior: %s\n---\n", reason, prevText)
	}
	fmt.Fprintf(&b, "%s\n", strings.Join(texts, "\n"))

	if isBoss {
		fmt.Fprintf(&b, "is_boss: true — este chat es del DUEÑO de la cuenta\n")
	} else {
		isApprover := false
		if c, ok, err := p.store.GetChat(chatJID); err != nil {
			log.Printf("capipush: chat lookup for dispatch preamble %s: %v", chatJID, err)
		} else if ok {
			isApprover = c.IsApprover
		}
		fmt.Fprintf(&b, "is_boss: false, is_approver: %t — nivel %s\n", isApprover, level)
	}

	rules, err := p.store.EffectiveRules(chatJID)
	if err != nil {
		log.Printf("capipush: effective rules for dispatch %s: %v", chatJID, err)
	} else if rules != "" {
		fmt.Fprintf(&b, "```rules.md\n%s\n```\n", rules)
	}

	fmt.Fprintf(&b, "NC:%s\n", nonce)
	return b.String()
}

// jidNumber strips a JID down to its bare user part (the phone number, or
// the @lid identifier when unresolved) — same criterion as the dashboard's
// own jidNumber() (internal/dashboard/web/app.js), no Go equivalent existed
// yet.
func jidNumber(jid string) string {
	number, _, _ := strings.Cut(jid, "@")
	return number
}

// LevelFor derives the semáforo level from chat state (AGENT-BEHAVIOR.md:
// "el nivel sale del router/estado del chat: is_boss, status, si es
// nuevo"). is_boss -> boss (full trust); is_approver (Aprobador P1,
// ct-2026-07-31-0610) -> approver, independently of status — a chat can be
// marked approver whether it's a known contact or one WhatsApp still calls
// "new" (a boss verbatim example: "un chat automatico" can be an approver
// too); a never-seen contact otherwise (status "new") -> danger (max
// caution, per AGENT-BEHAVIOR.md's own wording: "clientes, desconocidos,
// chats nuevos"); everything else (a known, non-boss, non-approver
// contact) -> caution. Exported for restapi's GET /api/chats
// (ct-2026-07-10-2312) — single source of truth for the semáforo, not
// duplicated.
func LevelFor(c store.Chat) string {
	if c.IsBoss {
		return mcpserver.LevelBoss
	}
	if c.IsApprover {
		return mcpserver.LevelApprover
	}
	if c.Status == "new" {
		return mcpserver.LevelDanger
	}
	return mcpserver.LevelCaution
}

// maxPreviewBytes caps the message preview carried in the cAPI envelope.
// The cAPI protocol's "message" field itself rejects anything over 4096
// chars (capi-protocolo.md §3) — 2000 leaves comfortable margin under that
// ceiling (a burst dispatch over it would 400 forever, a soft wedge: every
// sweep re-dispatches and re-fails) and matches the design intent anyway
// (AGENT-BEHAVIOR.md: "el mensaje cAPI queda chico" — the agent reads the
// full message from the DB over MCP; this is only enough to wake it up).
const maxPreviewBytes = 2000

// capPreview truncates text to at most maxPreviewBytes without splitting a
// UTF-8 rune.
func capPreview(text string) string {
	return capPreviewN(text, maxPreviewBytes)
}

// capPreviewN truncates text to at most n bytes without splitting a UTF-8 rune.
func capPreviewN(text string, n int) string {
	if len(text) <= n {
		return text
	}
	for n > 0 && !utf8.RuneStart(text[n]) {
		n--
	}
	return text[:n]
}

// burstPreviews builds a per-message preview slice for the whole burst. The
// per-message cap is scaled down proportionally (maxPreviewBytes / len(burst))
// so the total text stays near maxPreviewBytes regardless of burst size —
// same anti-bloat rationale as capPreview, distributed across messages.
// Minimum 200 bytes per message keeps each text useful even for large bursts.
//
// Media messages (Type != "" && Type != "text") are prefixed with a
// "[image]"/[video]/[audio]/[document] marker so the agent knows to call
// get_media for the file — the text portion is the caption (may be empty).
func burstPreviews(burst []store.Message) []string {
	if len(burst) == 0 {
		return nil
	}
	perMsg := maxPreviewBytes / len(burst)
	if perMsg < 200 {
		perMsg = 200
	}
	out := make([]string, len(burst))
	for i, m := range burst {
		if m.Type != "" && m.Type != "text" {
			marker := "[" + mimeCategory(m.Type) + "] "
			out[i] = marker + capPreviewN(m.Text, perMsg-len(marker))
		} else {
			out[i] = capPreviewN(m.Text, perMsg)
		}
	}
	return out
}

// mimeCategory returns a short label for a MIME type — used in burst
// previews to signal media presence.
func mimeCategory(mime string) string {
	switch {
	case strings.HasPrefix(mime, "image/"):
		return "image"
	case strings.HasPrefix(mime, "video/"):
		return "video"
	case strings.HasPrefix(mime, "audio/"):
		return "audio"
	default:
		return "document"
	}
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
