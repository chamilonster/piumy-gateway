// The gate state machine — the hard gate CLAUDE.md #4 requires in code,
// not in a skill/prompt: locked -> get_instructions(nonce) -> unlock(token)
// -> noting -> remember|skip -> ready -> send_message consumes it.
//
// State is tracked per dispatch (per nonce), never a global flag, so two
// dispatches never step on each other. Correlating later calls
// (unlock/remember/skip/send_message, none of which carry the nonce) to the
// right dispatch is by terminal_id (F4b — not MCP session ID as F4a had it:
// capipush registers a dispatch knowing only the destination terminal_id,
// never an MCP session, which doesn't exist yet at registration time).
// get_instructions(nonce) both binds the calling terminal to that dispatch
// AND validates the dispatch was actually registered FOR that terminal —
// this is what makes it impossible for terminal B to consume a dispatch
// meant for terminal A, even with the right nonce. send_message adds its
// own belt-and-suspenders chat-match check on top (see server.go).
package mcpserver

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"piumy-gateway/internal/store"
)

// Dispatch levels — who this dispatch is talking to, from the router +
// is_boss + chat state (computed by capipush, F4b; passed in verbatim here).
//
// LevelApprover (Aprobador P1, ct-2026-07-31-0610): a chat marked
// is_approver but not is_boss — trusted for exactly one extra thing over a
// normal dispatch (approving/discarding drafts, including other chats'),
// nothing else. Sits between Boss and Caution in privilege, but does NOT
// share Boss's blanket bypass in levelGateMiddleware — it's a single-purpose
// grant, not "boss but smaller". Born gateLocked like Caution/Danger (only
// Boss is born gateReady below): an approver still reads rules/memory/
// context through the normal ritual before it can act.
const (
	LevelBoss     = "boss"
	LevelApprover = "approver"
	LevelCaution  = "caution"
	LevelDanger   = "danger"
)

const (
	gateLocked = "locked"
	gateNoting = "noting"
	gateReady  = "ready"
	gateDone   = "done" // consumed by a successful send — one-shot
)

// dispatchStaleAfter bounds how long a dispatch survives with no activity
// before opportunistic cleanup reclaims it — same idea as mcpguard's
// clientStaleAfter. Covers two cases (H5 hardening, ct-2026-07-10-0540):
// never pulled via get_instructions (the original case), AND bound but
// stuck (an agent that crashed after get_instructions, or a failed
// Encrypt/Inject in capipush before this fix, leaving InFlight(terminalID)
// == true forever with zero chance of ever progressing).
//
// S4b (ct-2026-07-30-1255, defect 4): 1h → 15m. An hour was "last-resort
// net so it never happens", not "recovers fast" — for messaging, that's
// too long: while ANY dispatch sits stuck on a terminal, InFlight blocks
// EVERY chat routed there, not just the stuck one. 15m is still well past
// the longest Fibonacci redispatch gap (13m, capipush.redispatchBackoff) —
// a legitimately slow agent mid-ritual doesn't get yanked, but a genuinely
// crashed one recovers in minutes, not most of an hour. This is only the
// value before any SetStaleAfter call (main.go/capipush's own sweep both
// apply the real, live-settings-driven value almost immediately).
const dispatchStaleAfter = 15 * time.Minute

// gateSweepInterval is how often Sweep's own ticker reclaims stale
// dispatches — independent of RegisterDispatch being called (see Sweep's
// doc for why that independence is the actual H5 fix).
const gateSweepInterval = 5 * time.Minute

// dispatch tracks one in-flight cAPI dispatch through the gate.
type dispatch struct {
	nonce      string
	chatJID    string
	level      string
	terminalID string
	state      string
	token      string
	// lastActivity stamps every transition (registration, get_instructions,
	// unlock, remember/skip) — sweepLocked reclaims a dispatch idle longer
	// than dispatchStaleAfter regardless of whether it was ever pulled.
	lastActivity time.Time
	boundToTerm  bool  // true once get_instructions has bound this dispatch
	burstMaxTS   int64 // TS of the last message in the dispatched burst (ct-2026-07-13-2243)
}

// Gate is the per-dispatch lock/unlock/noting/ready state machine. Safe for
// concurrent use. The zero value is not usable — build one with NewGate.
type Gate struct {
	mu         sync.Mutex
	byNonce    map[string]*dispatch
	byTerminal map[string]*dispatch
	staleAfter time.Duration
}

func NewGate() *Gate {
	return &Gate{byNonce: map[string]*dispatch{}, byTerminal: map[string]*dispatch{}, staleAfter: dispatchStaleAfter}
}

// SetStaleAfter overrides the reclaim window sweepLocked uses (default
// dispatchStaleAfter, 1h) — post-incident hardening (ct-2026-07-11): a
// dispatch stuck mid-ritual (agent crashed after get_instructions) silently
// blocks new dispatches to that terminal for the full window with nothing
// logged. Wired from config (PIUMY_GATE_STALE_AFTER) so the window can be
// tuned without a rebuild. d<=0 is ignored (keeps the current value).
func (g *Gate) SetStaleAfter(d time.Duration) {
	if d <= 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.staleAfter = d
}

// RegisterDispatch records a dispatch about to be pushed to terminalID —
// called by capipush (F4b) or, in tests, a synthetic stand-in. Not an MCP
// tool: the terminal's agent never calls this itself.
//
// SECURITY (found in F4c audit): immediately binds byTerminal[terminalID]
// to the NEW dispatch, replacing whatever was there — a residual
// higher-privilege binding must never survive a new dispatch's
// registration. Before this fix, RegisterDispatch only touched byNonce;
// byTerminal stayed pointed at the terminal's PREVIOUS dispatch until the
// agent itself called GetInstructions for the new one. A terminal bound to
// a done-but-still-boss dispatch would keep answering Active() as boss even
// after a new, lower-trust (caution/danger) dispatch was registered for it,
// so an agent that simply skipped get_instructions for the new dispatch
// kept operating with the OLD level's privileges — the same fail-open shape
// as F4a, now in the level TRANSITION rather than the initial state. Forcing
// the new dispatch (locked, not ready) into byTerminal here means Active()
// reflects the new level immediately, and every gated tool + send_message
// falls back to DENY/gated-by-level until the agent properly re-gates via
// get_instructions -> unlock -> remember|skip for the new dispatch.
func (g *Gate) RegisterDispatch(nonce, chatJID, level, terminalID string, burstMaxTS int64) error {
	switch level {
	case LevelBoss, LevelApprover, LevelCaution, LevelDanger:
	default:
		return fmt.Errorf("gate: invalid level %q", level)
	}
	if terminalID == "" {
		return fmt.Errorf("gate: terminalID is required")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sweepLocked(time.Now())
	// ST-A security fix (ct-2026-07-11-0740): boss dispatches start straight
	// in gateReady instead of gateLocked — boss was always meant to skip the
	// unlock/remember/skip checkpoint (F4-DESIGN §3), but starting it
	// gateLocked like everything else meant Ready never reflected boss's
	// real state at all, which is exactly what let validateSend and
	// levelGateMiddleware key off Level==Boss alone (see their own doc
	// comments) — a done (consumed) dispatch kept granting privileges
	// forever because nothing ever looked at Ready for boss. Starting ready
	// makes Ready mean "usable" uniformly for every level: true from
	// registration for boss, true only after the checkpoint for
	// caution/danger, and false for everyone once Consume marks it done.
	initialState := gateLocked
	if level == LevelBoss {
		initialState = gateReady
	}
	d := &dispatch{nonce: nonce, chatJID: chatJID, level: level, terminalID: terminalID, state: initialState, lastActivity: time.Now(), burstMaxTS: burstMaxTS}
	if prev := g.byTerminal[terminalID]; prev != nil && prev.nonce != nonce {
		delete(g.byNonce, prev.nonce)
	}
	g.byNonce[nonce] = d
	g.byTerminal[terminalID] = d
	return nil
}

// CancelDispatch undoes a RegisterDispatch that never actually reached the
// terminal — H5 hardening (ct-2026-07-10-0540): if capipush's Inject fails
// AFTER RegisterDispatch already succeeded, the dispatch would otherwise
// sit registered with zero chance of ever being pulled (the agent never
// received it) — InFlight(terminalID) reports true for no reason until
// Sweep's dispatchStaleAfter eventually reclaims it. Calling this
// immediately frees the terminal instead. No-op if nonce/terminalID no
// longer match what's currently registered (e.g. a newer dispatch already
// replaced it) — never cancels the WRONG dispatch.
func (g *Gate) CancelDispatch(nonce, terminalID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	d, ok := g.byNonce[nonce]
	if !ok || d.terminalID != terminalID {
		return
	}
	g.evictLocked(d)
}

// evictLocked removes d from both indices — guarding byTerminal against a
// newer dispatch that may have already replaced it there (RegisterDispatch's
// own force-replace, see its doc comment) is what keeps CancelDispatch and
// sweepLocked from ever evicting the WRONG (current) dispatch for a
// terminal. Caller must hold g.mu.
func (g *Gate) evictLocked(d *dispatch) {
	delete(g.byNonce, d.nonce)
	if g.byTerminal[d.terminalID] == d {
		delete(g.byTerminal, d.terminalID)
	}
}

// Sweep runs sweepLocked on its own ticker until ctx is cancelled — the
// independence from RegisterDispatch is the actual H5 fix (ct-2026-07-10-
// 0540): before this, the stale sweep only ran inline inside
// RegisterDispatch, but capipush.dispatch skips calling RegisterDispatch
// entirely for a terminal that's already InFlight — exactly the wedged
// case that needs reclaiming. A terminal stuck InFlight forever meant
// RegisterDispatch (and so the sweep) never ran again for it either. A
// ticker-driven sweep reclaims it regardless of what any terminal is doing.
func (g *Gate) Sweep(ctx context.Context) {
	ticker := time.NewTicker(gateSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.mu.Lock()
			g.sweepLocked(time.Now())
			g.mu.Unlock()
		}
	}
}

// InFlight reports whether terminalID currently has a dispatch that's
// bound and not yet done (locked/noting/ready) — capipush (F4b) checks
// this before registering a new dispatch, so a burst of inbound messages
// doesn't force-regate a terminal mid-flow on legitimate, still-in-
// progress work (an efficiency/UX refinement; RegisterDispatch's
// force-replace above is the actual security guarantee and applies
// regardless of what InFlight says — capipush choosing to call
// RegisterDispatch anyway while in-flight is still safe, just noisier).
func (g *Gate) InFlight(terminalID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	d := g.byTerminal[terminalID]
	return d != nil && d.state != gateDone
}

// NonceActive reports whether nonce is currently registered to a dispatch
// (ct-2026-07-18-1851-B) — capipush checks this before committing to a short
// (4-hex) nonce, regenerating on a collision instead of registering over an
// unrelated in-flight dispatch.
func (g *Gate) NonceActive(nonce string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.byNonce[nonce]
	return ok
}

// sweepLocked drops dispatches idle for more than dispatchStaleAfter —
// never pulled via get_instructions, OR bound but stuck (H5 hardening,
// ct-2026-07-10-0540 — see dispatchStaleAfter's doc). A gateDone dispatch
// is never in byNonce (Consume deletes it there immediately), so this
// never needs to special-case "already finished". Caller must hold g.mu.
//
// S2 (ct-2026-07-30-030928, criterio de listo #3): this eviction IS the
// "se libera solo, sin reiniciar el gateway" path — it just ran silently
// before S1 gave capipush a log channel. gateSweepInterval is 5 MINUTES
// (not capipush's 5s), so logging every reclaim here is an event, not
// per-tick noise — no transition tracking needed.
func (g *Gate) sweepLocked(now time.Time) {
	for _, d := range g.byNonce {
		if now.Sub(d.lastActivity) > g.staleAfter {
			log.Printf("mcpserver: gate: dispatch nonce=%s chat=%s terminal=%s nivel=%s liberado por timeout (idle %s > %s) — el terminal queda libre sin reiniciar el gateway", d.nonce, d.chatJID, d.terminalID, d.level, now.Sub(d.lastActivity).Round(time.Second), g.staleAfter)
			g.evictLocked(d)
		}
	}
}

// Instructions is get_instructions' payload — Token last, on purpose: it
// forces the agent to have ingested rules/memory/context to reach it.
// IsBoss/IsApprover (ct-2026-08-06, boss verbatim: "si soy boss tiene que
// decir is boss") mirror the same identity the dispatch preamble already
// carries (capipush.dispatchPayload) — an agent that reconnects mid-way
// (nonce still valid, preamble long gone) looks here instead.
type Instructions struct {
	Rules      string `json:"rules"`
	Memory     string `json:"memory"`
	Context    string `json:"context"`
	IsBoss     bool   `json:"is_boss"`
	IsApprover bool   `json:"is_approver"`
	Token      string `json:"token"`
}

// GetInstructions marks terminalID's dispatch (registered under nonce) as
// pulled and returns a fresh unlock token plus rules/memory/context read
// live from the store. Rejects outright if nonce was registered for a
// DIFFERENT terminal — the core anti-hijack check (a terminal can never
// consume another terminal's dispatch, even knowing its nonce).
// RegisterDispatch already binds byTerminal[terminalID] to this dispatch at
// registration time (see its doc comment), so this only needs to issue the
// token and mark it pulled (for the staleness sweep).
func (g *Gate) GetInstructions(terminalID, nonce string, st *store.Store) (Instructions, error) {
	g.mu.Lock()
	d, ok := g.byNonce[nonce]
	if !ok {
		g.mu.Unlock()
		// S2 (ct-2026-07-30-030928): the most likely cause is RegisterDispatch's
		// own force-replace (a newer dispatch to the same terminal evicted this
		// nonce before the agent got here) — a security guarantee, not a bug,
		// but silent until now: nothing logged which dispatch got orphaned.
		log.Printf("mcpserver: gate: get_instructions terminal=%s nonce=%s: no hay dispatch registrado (probable force-replace por un dispatch más nuevo al mismo terminal)", terminalID, nonce)
		return Instructions{}, fmt.Errorf("unknown nonce %q — no dispatch registered", nonce)
	}
	if d.terminalID != terminalID {
		g.mu.Unlock()
		log.Printf("mcpserver: gate: get_instructions terminal=%s nonce=%s: dispatch registrado para otro terminal (%s) — intento de hijack o nonce mal dirigido", terminalID, nonce, d.terminalID)
		return Instructions{}, fmt.Errorf("this dispatch was not registered for this terminal")
	}
	token, err := randomHex(16)
	if err != nil {
		g.mu.Unlock()
		return Instructions{}, err
	}
	d.token = token
	d.boundToTerm = true
	d.lastActivity = time.Now()
	chatJID := d.chatJID
	g.mu.Unlock()

	rules, err := st.EffectiveRules(chatJID)
	if err != nil {
		return Instructions{}, err
	}
	c, _, err := st.GetChat(chatJID)
	if err != nil {
		return Instructions{}, err
	}
	return Instructions{Rules: rules, Memory: c.Memory, Context: c.Context, IsBoss: c.IsBoss, IsApprover: c.IsApprover, Token: token}, nil
}

// Unlock validates token against terminalID's active dispatch and
// transitions locked -> noting. A token from a different terminal's
// dispatch never matches this terminal's own bound token — anti-replay
// falls out of the terminal-scoped lookup, no separate token index needed.
//
// S2 (ct-2026-07-30-030928): noting/ready are idempotent successes, not
// errors — a dispatch already past the unlock checkpoint (boss is BORN in
// gateReady, ST-A ct-2026-07-11-0740; or a caution/danger dispatch that
// already called unlock once) has nothing left to do here, and saying so
// used to collide with advanceToReady's own error for the exact same state
// (both said "not right" using different, contradictory words — the smoke's
// "already unlocked" / "not unlocked — call unlock first" deadlock). done
// stays a real, distinct error: a consumed dispatch must stay gated.
func (g *Gate) Unlock(terminalID, token string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	d := g.byTerminal[terminalID]
	if d == nil {
		return fmt.Errorf("no active dispatch — call get_instructions first")
	}
	switch d.state {
	case gateNoting, gateReady:
		return nil
	case gateDone:
		return fmt.Errorf("dispatch already consumed — call get_instructions for a new one")
	}
	// H4 hardening (ct-2026-07-10-0540): d.token is "" until GetInstructions
	// sets it — without the explicit empty check, an agent that never calls
	// get_instructions (so d.token is still its zero value) can unlock with
	// token="" and skip ingesting rules/memory/context entirely. Rejecting
	// "" outright (not just "doesn't match") closes that regardless of
	// what d.token happens to be.
	if token == "" || d.token != token {
		return fmt.Errorf("invalid token")
	}
	d.state = gateNoting
	d.lastActivity = time.Now()
	return nil
}

// Remember applies the noting->ready checkpoint, optionally writing
// memory/context on the dispatch's own chat — never an override, even for
// boss (F4-DESIGN's "todas las memorias" for boss is about tool
// visibility/scoping elsewhere, not a remember() target override).
func (g *Gate) Remember(terminalID, memory, context string, st *store.Store) error {
	d, err := g.advanceToReady(terminalID)
	if err != nil {
		return err
	}
	if memory != "" {
		if err := st.SetChatMemory(d.chatJID, memory); err != nil {
			return err
		}
	}
	if context != "" {
		if err := st.SetChatContext(d.chatJID, context); err != nil {
			return err
		}
	}
	return nil
}

// Skip applies the noting->ready checkpoint with no writes.
func (g *Gate) Skip(terminalID string) error {
	_, err := g.advanceToReady(terminalID)
	return err
}

// S2 (ct-2026-07-30-030928): mirrors Unlock's idempotency — ready is a
// no-op success (already past this checkpoint: boss is born here, or a
// caution/danger dispatch that already called remember/skip once), done is
// a distinct real error, and locked keeps its real, honest error (never
// unlocked yet — legitimate guidance, not a contradiction).
func (g *Gate) advanceToReady(terminalID string) (*dispatch, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	d := g.byTerminal[terminalID]
	if d == nil {
		return nil, fmt.Errorf("no active dispatch — call get_instructions first")
	}
	switch d.state {
	case gateReady:
		return d, nil
	case gateDone:
		return nil, fmt.Errorf("dispatch already consumed — call get_instructions for a new one")
	case gateLocked:
		return nil, fmt.Errorf("not unlocked — call unlock first")
	}
	d.state = gateReady
	d.lastActivity = time.Now()
	return d, nil
}

// ActiveDispatch is the read-only view send_message and the level-gating
// middleware need.
type ActiveDispatch struct {
	ChatJID    string
	Level      string
	Ready      bool
	BurstMaxTS int64 // TS of the last dispatched burst message (ct-2026-07-13-2243)
}

// Active reports terminalID's current dispatch, if any.
//
// false means no dispatch is bound for this terminal — callers MUST treat
// this as DENY for gated tools (default-DENY, F4b), never as unrestricted:
// a terminal with nothing registered has no business calling a gated tool
// at all. The only ungated exception is boss, and boss must come from an
// explicit dispatch with Level == LevelBoss (registered by capipush from
// the chat's is_boss flag) — never from the absence of a dispatch. See
// docs/F4B-DIAGRAMA-FAILCLOSED.md.
func (g *Gate) Active(terminalID string) (ActiveDispatch, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	d := g.byTerminal[terminalID]
	if d == nil {
		return ActiveDispatch{}, false
	}
	return ActiveDispatch{ChatJID: d.chatJID, Level: d.level, Ready: d.state == gateReady, BurstMaxTS: d.burstMaxTS}, true
}

// Consume retires terminalID's active dispatch after a successful send —
// one-shot, so a used dispatch can never be replayed. The entry stays in
// byTerminal (marked done, Ready() false) rather than being deleted: a
// terminal that ever engaged the gate must not fall back to the
// no-dispatch-bound default just because its last dispatch finished — it
// stays gated (denied) until a NEW get_instructions binds it to the next one.
func (g *Gate) Consume(terminalID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if d := g.byTerminal[terminalID]; d != nil {
		delete(g.byNonce, d.nonce)
		d.state = gateDone
	}
}
