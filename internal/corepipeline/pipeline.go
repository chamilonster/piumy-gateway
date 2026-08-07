// Package corepipeline: the messaging-vendor-agnostic core. Owns the two
// loops any messaging client needs regardless of vendor — inbound handling
// and outbox draining — plus the Controller facade that starts/stops them
// together with a gateway.Gateway. Never imports open-wa; only the
// gateway.Gateway seam. Rewritten clean from Piumy's gateway.go (which
// mixed whatsmeow + this logic in one type) — see
// docs/F2-PIPELINE.md for what was ported and what wasn't.
package corepipeline

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"piumy-gateway/internal/eventbus"
	"piumy-gateway/internal/gateway"
	"piumy-gateway/internal/governor"
	"piumy-gateway/internal/router"
	"piumy-gateway/internal/state"
	"piumy-gateway/internal/store"
)

// Config holds the pipeline's own tunables — deliberately separate from
// internal/config: corepipeline has no dependency on that package, the
// caller (main.go, F5) wires config.Config's values into this struct field
// by field. Zero fields fall back to defaults in New, same pattern as
// governor/mcpguard/sessionbackup.
type Config struct {
	// OutboxPoll is how often the outbox drain loop checks for due items.
	// Default 5s.
	OutboxPoll time.Duration
	// OutboxMaxRetry is how many failed send attempts an item gets before
	// it's dead-lettered (anti-ban: never resend forever). Default 5.
	OutboxMaxRetry int

	// DispatchDelayMin/Max bound the randomized human-pacing delay before
	// each outbound send. Default 1s/5s.
	DispatchDelayMin time.Duration
	DispatchDelayMax time.Duration
	// ReadDelayMin/Max bound the randomized delay before sending a read
	// receipt (anti-ban: never instant, even reads). Default 2s/8s.
	ReadDelayMin time.Duration
	ReadDelayMax time.Duration

	// ComposingMin/Max bound the brief "typing" window between showing the
	// composing indicator and actually sending (feel more human). Default
	// 500ms/1.5s — same as Piumy's hardcoded value, made configurable here
	// so tests can shrink it instead of paying it in wall-clock time.
	ComposingMin time.Duration
	ComposingMax time.Duration
}

func defaultConfig(cfg Config) Config {
	if cfg.OutboxPoll <= 0 {
		cfg.OutboxPoll = 5 * time.Second
	}
	if cfg.OutboxMaxRetry <= 0 {
		cfg.OutboxMaxRetry = 5
	}
	if cfg.DispatchDelayMin <= 0 {
		cfg.DispatchDelayMin = 1 * time.Second
	}
	if cfg.DispatchDelayMax <= 0 || cfg.DispatchDelayMax < cfg.DispatchDelayMin {
		cfg.DispatchDelayMax = 5 * time.Second
	}
	if cfg.ReadDelayMin <= 0 {
		cfg.ReadDelayMin = 2 * time.Second
	}
	if cfg.ReadDelayMax <= 0 || cfg.ReadDelayMax < cfg.ReadDelayMin {
		cfg.ReadDelayMax = 8 * time.Second
	}
	if cfg.ComposingMin <= 0 {
		cfg.ComposingMin = 500 * time.Millisecond
	}
	if cfg.ComposingMax <= 0 || cfg.ComposingMax < cfg.ComposingMin {
		cfg.ComposingMax = 1500 * time.Millisecond
	}
	return cfg
}

// Pipeline runs the inbound and outbox loops against a gateway.Gateway.
// Agnostic on purpose: store/router/governor/state/eventbus are all
// already-migrated packages (F1), nothing here knows about open-wa.
type Pipeline struct {
	gw    gateway.Gateway
	store *store.Store
	rt    *router.Manager
	gov   *governor.Limiter
	state *state.Manager
	cfg   Config

	// bus is optional (SetBus) — atomic.Pointer so it's safe to wire at any
	// time, including before Run(), without a dedicated lock on the hottest
	// path (every inbound message reads it once).
	bus atomic.Pointer[eventbus.Bus]

	// runCtx is stashed so MarkRead (triggered later by an MCP tool call,
	// F4) can spawn cancellable background work without threading ctx
	// through every caller.
	runMu  sync.Mutex
	runCtx context.Context
}

// New builds a Pipeline. gw, st, rt, gov, and sm must all be non-nil.
func New(gw gateway.Gateway, st *store.Store, rt *router.Manager, gov *governor.Limiter, sm *state.Manager, cfg Config) *Pipeline {
	return &Pipeline{
		gw:    gw,
		store: st,
		rt:    rt,
		gov:   gov,
		state: sm,
		cfg:   defaultConfig(cfg),
	}
}

// SetBus registers the event bus handleInbound publishes to. Safe to call
// at any time, including before Run().
func (p *Pipeline) SetBus(b *eventbus.Bus) {
	p.bus.Store(b)
}

func (p *Pipeline) context() context.Context {
	p.runMu.Lock()
	defer p.runMu.Unlock()
	if p.runCtx != nil {
		return p.runCtx
	}
	return context.Background()
}

func (p *Pipeline) dispatchDelay() governor.DelayWindow {
	return governor.DelayWindow{
		Min: p.store.SettingDuration(store.SettingDispatchDelayMin, p.cfg.DispatchDelayMin),
		Max: p.store.SettingDuration(store.SettingDispatchDelayMax, p.cfg.DispatchDelayMax),
	}
}

func (p *Pipeline) readDelay() governor.DelayWindow {
	return governor.DelayWindow{
		Min: p.store.SettingDuration(store.SettingReadDelayMin, p.cfg.ReadDelayMin),
		Max: p.store.SettingDuration(store.SettingReadDelayMax, p.cfg.ReadDelayMax),
	}
}

// Run launches the inbound loop and the outbox drain loop, and blocks until
// ctx is cancelled AND both loops have actually returned. Waiting for
// outboxLoop here (not just inboundLoop) matters: Controller.Stop() returns
// as soon as Run returns, and a caller is free to close the store right
// after Stop() — if outboxLoop were still mid-processOutbox (e.g. blocked
// on a real gw.Send when ctx was cancelled), its trailing store writes
// (MarkSent/AddMessage) would race a closed store ("database is closed").
func (p *Pipeline) Run(ctx context.Context) {
	p.runMu.Lock()
	p.runCtx = ctx
	p.runMu.Unlock()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		p.outboxLoop(ctx)
	}()
	p.inboundLoop(ctx)
	wg.Wait()
}

func (p *Pipeline) inboundLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-p.gw.Inbound():
			if !ok {
				return
			}
			p.handleInbound(msg)
		}
	}
}

func (p *Pipeline) outboxLoop(ctx context.Context) {
	ticker := time.NewTicker(p.cfg.OutboxPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.processOutbox(ctx)
		}
	}
}

// handleInbound: router gate (is_boss bypasses it, isBossChat) → store
// (dedup'd by store.AddMessage) → eventbus nudge (only on a successful
// store) → TouchChat → recompute queue depth → state.Update(Queue,LastMsg)
// → a transient mood (vip/new_msg). Deliberately does NOT mark anything
// read (a read receipt must reflect real agent attention, not mere receipt
// — see Pipeline.MarkRead) and does NOT touch media (shim/no-op, post-MVP —
// Inbound.Type is stored on the message row above and nothing else happens
// with it here).
func (p *Pipeline) handleInbound(msg gateway.Inbound) {
	dec := p.rt.Resolve(msg.ChatJID)
	if !dec.Allowed && !p.isBossChat(msg.ChatJID) {
		return
	}

	m := store.Message{
		ChatJID:       msg.ChatJID,
		ID:            msg.MsgID,
		FromMe:        false,
		Sender:        msg.SenderJID,
		Text:          msg.Text,
		TS:            msg.TS,
		Type:          msg.Type,
		QuotedID:      msg.QuotedID,
		QuotedPreview: msg.QuotedPreview,
		Forwarded:     msg.Forwarded,
	}
	if err := p.store.AddMessage(m); err != nil {
		log.Printf("corepipeline: store message: %v", err)
	} else if b := p.bus.Load(); b != nil {
		b.Publish(eventbus.Event{Type: "message", JID: msg.ChatJID, TS: msg.TS})
	}

	// msg.PushName is the SENDER's own display name — correct as the chat's
	// name for a 1:1, but for a group it would overwrite the group's real
	// name (seeded by whatsmeow's seedGroups/GetJoinedGroups) with whoever
	// happens to send the next message (ct-2026-07-10-1758, boss escalation:
	// a real WhatsApp group's own name was clobbered to a member's pushname
	// during the smoke test). "" makes TouchChat a no-op on name (chat.go's
	// upsert only overwrites when the new value is non-empty).
	chatName := msg.PushName
	if store.IsGroupJID(msg.ChatJID) {
		chatName = ""
	}
	if err := p.store.TouchChat(msg.ChatJID, chatName, msg.TS); err != nil {
		log.Printf("corepipeline: touch chat: %v", err)
	}

	// Mirror the router's mode decision onto the chat. PendingDedicated (cAPI
	// push) and the auto-reply sweep both filter by chats.mode, so a fresh
	// inbound chat must adopt dec.Mode — otherwise it stays at the schema
	// default 'auto' (AddMessage created it above) and nothing ever dispatches
	// it. The router is the source of truth for mode; chats.mode is a
	// queryable cache of it.
	// ST-B fix (ct-2026-07-11-0741): SyncRouterMode, not SetMode — re-applied
	// every inbound, but now a no-op once the owner/agent has set the mode
	// explicitly (mode_source='manual', set by SetMode). Before this, a
	// manual set_mode/escalate call got silently reverted by the very next
	// inbound message from that chat.
	if err := p.store.SyncRouterMode(msg.ChatJID, dec.Mode); err != nil {
		log.Printf("corepipeline: set mode chat=%s: %v", msg.ChatJID, err)
	}

	qCount, err := p.store.CountPendingDedicated()
	if err != nil {
		log.Printf("corepipeline: count queue: %v", err)
	}

	preview := msg.Text
	if len(preview) > 80 {
		preview = preview[:80] + "…"
	}
	_ = p.state.Update(func(s *state.Status) {
		s.Queue = qCount
		if preview != "" {
			s.LastMsg = preview
		}
	})

	if p.rt.IsVIP(msg.ChatJID) {
		_ = p.state.React("vip", "the owner!", 6*time.Second)
	} else {
		_ = p.state.React("new_msg", "ooh! a message", 4*time.Second)
	}
}

// isBossChat reports whether jid is already known as the owner's own chat
// (T30, ct-2026-08-06-0159, boss verbatim: "el criterio de salida tiene que
// alinearse con el de entrada"). The router whitelist was the one anti-ban
// gate is_boss never bypassed — store.PendingDedicated (entry into the
// dispatch queue), capipush.dispatch (routing: "is_boss ⟹ principal, sin
// configuración de router.json") and mcpserver's initiateAuthorized (send
// without a bound dispatch) all already treat is_boss as an unconditional
// bypass; this closes the remaining gap, on the storage gate itself. Before
// this, an empty/misconfigured whitelist silently dropped the owner's own
// inbound messages here — no store write, no eventbus publish, no log —
// worse than the equivalent gap on the send side (mcpserver.validateSend),
// which at least surfaces a tool error. A chat not yet known (never
// TouchChat'd) reports false, same "no benefit of the doubt" default
// initiateAuthorized already uses — in practice never an issue for the
// real owner, since whatsmeow marks is_boss at connect time (T12),
// before any inbound message can reach here.
func (p *Pipeline) isBossChat(jid string) bool {
	c, ok, err := p.store.GetChat(jid)
	return err == nil && ok && c.IsBoss
}

// MarkRead sends read receipts for the not-yet-read messages in msgs,
// paced by the anti-ban read delay, and returns immediately — the delay and
// the actual gw.MarkRead call run in the background so a caller (an MCP
// tool handler, F4) never blocks on it.
func (p *Pipeline) MarkRead(chatJID string, msgs []store.Message) {
	var ids []string
	for _, m := range msgs {
		if needsReadReceipt(m) {
			ids = append(ids, m.ID)
		}
	}
	if len(ids) == 0 {
		return
	}

	ctx := p.context()
	go func() {
		p.readDelay().Sleep(ctx)
		if ctx.Err() != nil {
			return
		}
		// H2+H3 audit follow-up (M1, escalated into ct-2026-07-10-0540
		// since H6 trips the kill switch on a LoggedOut/TemporaryBan):
		// processOutbox already checks this, but read receipts are their
		// own outbound WhatsApp activity — without this, "kill everything"
		// wasn't actually true, sends stopped but read receipts kept going
		// out right when staying quiet matters most.
		if p.killSwitchActive() {
			return
		}
		if err := p.gw.MarkRead(ctx, chatJID, ids); err != nil {
			log.Printf("corepipeline: mark read chat=%s: %v", chatJID, err)
			return
		}
		now := time.Now().Unix()
		for _, id := range ids {
			if err := p.store.SetRead(chatJID, id, now); err != nil {
				log.Printf("corepipeline: store read receipt chat=%s id=%s: %v", chatJID, id, err)
			}
		}
	}()
}

// needsReadReceipt reports whether m is an inbound message that hasn't been
// marked read yet.
func needsReadReceipt(m store.Message) bool {
	return !m.FromMe && m.ReadTS == 0
}
