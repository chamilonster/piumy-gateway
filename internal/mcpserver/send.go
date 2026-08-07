// send_message, draft, and silent_act — the three ways ready ->
// {sent, drafted, silenced} (F4-DESIGN §4). send_message/draft share every
// check up to the point where they diverge (enqueue vs. draft), factored
// into validateSend. silent_act (S11, ct-2026-07-30-1619) has no content to
// validate — it consumes the SAME gate turn via the SAME gate.Consume call,
// so a chat the agent deliberately doesn't answer releases the terminal
// exactly as fast as one it does.
package mcpserver

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"piumy-gateway/internal/store"
)

// validateSend runs the F4b gate check + the policy_version gate + Piumy's
// 6 base checks shared by send_message and draft. Returns the resolved
// chat on success ("" error), or a user-facing error message to return
// immediately. draft used to skip the policy_version check (send_message
// had its own copy) — found in F4c audit: draft's own description claims
// "same guardrails as send_message", which wasn't true. Folded in here so
// both tools enforce it identically, in one place.
func validateSend(ctx context.Context, d Deps, gate *Gate, to, model, policyVersion string) (store.Chat, string) {
	termID := terminalIDFromContext(ctx)
	isPrincipal := d.PrincipalTerminalID != "" && termID == d.PrincipalTerminalID
	if !isPrincipal || !initiateAuthorized(d, to) {
		active, bound := gate.Active(termID)
		if !bound {
			return store.Chat{}, "locked: no active dispatch for this terminal (default DENY) — call get_instructions first"
		}
		// ST-A security fix (ct-2026-07-11-0740): Ready is now required for
		// EVERY level, boss included — RegisterDispatch starts boss dispatches
		// gateReady (skips the checkpoint by design) and Consume marks any
		// dispatch gateDone (Ready=false) regardless of level, so a boss
		// dispatch that already sent once can no longer send again without a
		// fresh dispatch. Before this fix, this whole block was skipped for
		// boss, and a done-but-still-bound dispatch kept Level==Boss forever —
		// permanent privilege escalation, one boss message ever = boss forever.
		if !active.Ready {
			return store.Chat{}, "locked: call get_instructions -> unlock -> remember/skip before send_message"
		}
		if active.Level != LevelBoss && to != active.ChatJID {
			return store.Chat{}, "refused: this dispatch is unlocked for " + active.ChatJID + ", not " + to + " — get_instructions for the right chat first"
		}
	}
	if d.State != nil && d.State.Snapshot().Muted {
		return store.Chat{}, "muted: message not sent"
	}
	// ct-2026-07-13-1822: principal puede omitir policy_version (es opcional para él).
	// No-principal: siempre requerido (gate caution/danger intacto).
	if !isPrincipal || policyVersion != "" {
		if _, current := decisionPolicy(d.PolicyPath); policyVersion != current {
			return store.Chat{}, "stale/missing policy_version — call get_decision_policy first"
		}
	}
	if !strings.Contains(to, "@") {
		return store.Chat{}, "to must be a full JID (e.g. 55500000001@c.us), not a bare number — copy it from list_chats/get_queue/resolve_chat"
	}
	c, ok, err := d.Store.GetChat(to)
	if err != nil {
		return store.Chat{}, err.Error()
	}
	if !ok {
		return store.Chat{}, "error: no rules on this chat"
	}
	if c.ClaimedBy != "" && c.ClaimedBy != model {
		return store.Chat{}, "refusing to send: " + to + " is claimed by another agent (" + c.ClaimedBy + ") until " + time.Unix(c.ClaimedUntil, 0).UTC().Format(time.RFC3339) + " — wait, or claim_chat it yourself once expired"
	}
	effRules, err := d.Store.EffectiveRules(to)
	if err != nil {
		return store.Chat{}, err.Error()
	}
	if effRules == "" {
		return store.Chat{}, "error: no rules on this chat"
	}
	if isGroupJID(to) && c.Status == "ignored" {
		return store.Chat{}, "refusing to send: " + to + " is a WhatsApp group still marked ignored — the owner must un-ignore it first"
	}
	// T30 (ct-2026-08-06-0159, boss verbatim: "el criterio de salida tiene
	// que alinearse con el de entrada") — the owner's own chat is exempt
	// from the anti-ban whitelist, same as initiateAuthorized already
	// exempts it from needing a bound dispatch two checks up, and same as
	// store.PendingDedicated/capipush.dispatch already exempt it on the
	// entry side. Everything else in this function (rules, claim, group-
	// ignored, muted, policy_version) still applies to the owner exactly
	// as before — this is the one gate that never had the exception.
	if d.Router != nil && !c.IsBoss && !d.Router.Resolve(to).Allowed {
		return store.Chat{}, "refusing to send: " + to + " is not in the whitelist (anti-ban) — add it via router config first"
	}
	return c, ""
}

// initiateAuthorized reports whether the principal may send to `to` WITHOUT
// a bound dispatch — "candado versión segura" (ct-2026-07-18-1438, boss
// verbatim: "quiero que puedan hablarme al iniciar o hablarle a quienes yo
// se los pida"). Narrows ct-2026-07-13-0538's blanket "principal = full
// authority" down to exactly two cases: is_boss (talk to the boss, any of
// his numbers — always) or active+rules (a contact the boss deliberately
// turned on via set_chat_active — "quienes yo se los pida"). Any other chat
// still needs the normal reactive binding in validateSend, principal or
// not — every check below this (whitelist, claim, rules, group-ignored)
// still applies exactly as before. Errors or a missing chat fall through
// to false — no benefit of the doubt.
func initiateAuthorized(d Deps, to string) bool {
	c, ok, err := d.Store.GetChat(to)
	if err != nil || !ok {
		return false
	}
	if c.IsBoss {
		return true
	}
	if !c.Active {
		return false
	}
	rules, err := d.Store.EffectiveRules(to)
	return err == nil && rules != ""
}

// markDispatchChatIfDifferent also closes the ACTIVE DISPATCH's own chat
// when it differs from `to` (T33, ct-2026-08-06-1526 — a real case the boss
// hit live: ordered over WhatsApp to write a third number, change its
// rules, and note memory/context; his own message came back a second time).
// send_message/draft only ever marked `to` handled — when the dispatch
// being answered is a DIFFERENT chat than the one just written to, that
// dispatch's own inbound message was never marked, so the sweep found it
// still pending once the terminal freed up and re-dispatched it. Not
// introduced by T31 (ct-2026-08-06-0244): the principal-terminal and
// boss-dispatch bypasses in levelGateMiddleware already let send_message/
// draft target a chat other than the active dispatch's before that — T31
// made it the everyday case (the owner routinely asking to act on a third
// chat) instead of a rare one. silent_act never had this bug: it has no
// `to` at all, always targets active.ChatJID.
//
// Same bound as silent_act's own MarkHandledBefore call — active.BurstMaxTS,
// never `now` — a message that arrived in the dispatch's chat AFTER the
// burst must stay pending (T33's own "no marques de más" requirement); this
// only closes what was actually dispatched. No-op when there's no bound
// dispatch, or when it's the same chat as `to` (today's normal case,
// already marked by the caller — this must never double-write it).
func markDispatchChatIfDifferent(d Deps, active ActiveDispatch, bound bool, to string) {
	if !bound || active.ChatJID == to {
		return
	}
	if err := d.Store.MarkHandledBefore(active.ChatJID, active.BurstMaxTS); err != nil {
		log.Printf("mcpserver: mark_handled_before (dispatch chat) %s: %v", active.ChatJID, err)
	}
}

func addSendTools(s *server.MCPServer, d Deps, gate *Gate, tracker *agentTracker) {
	// ── send_message ─────────────────────────────────────────────────────
	s.AddTool(mcp.NewTool("send_message",
		mcp.WithDescription("Queue a message to send over WhatsApp. The gateway dispatches it while respecting the anti-ban governor (it is not sent instantly). 'to' must be a full JID (copy it from list_chats/get_queue/resolve_chat), not a bare phone number. Requires the current policy_version from get_decision_policy — read that FIRST, every time; a stale or missing value is rejected. LAW: rejected with \"error: no rules on this chat\" if the chat has no EFFECTIVE rules (get_chat's rules field) — the agent never acts without rules. A WhatsApp group additionally needs a non-\"ignored\" status. If the chat's confirmation_mode is \"always\", this creates a draft instead of sending (owner approval required) — use the draft tool to do that deliberately in \"discretion\" mode."),
		mcp.WithString("to", mcp.Required(), mcp.Description("Destination JID, e.g. 55500000001@c.us")),
		mcp.WithString("message", mcp.Required(), mcp.Description("Text to send")),
		mcp.WithString("model", mcp.Required(), mcp.Description("Which model is sending this — required so every reply is attributable")),
		mcp.WithString("policy_version", mcp.Description("Hash from get_decision_policy — call that tool first. Required for non-principal terminals; the principal (DefaultTerminalID) may omit it."))),
		func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			tracker.seen(ctx)
			to, err := r.RequireString("to")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			policyVersion := r.GetString("policy_version", "")
			model, err := r.RequireString("model")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			c, errMsg := validateSend(ctx, d, gate, to, model, policyVersion)
			if errMsg != "" {
				return mcp.NewToolResultError(errMsg), nil
			}
			// H6 hardening (ct-2026-07-10-0540): refuse outright while the
			// gateway is disconnected — enqueueing anyway just puts the
			// message in the outbox with no ETA, and the agent reads
			// "queued for sending" as success when it may never go out
			// (deauthed/banned session, see internal/whatsmeow's disconnect
			// handling). draft is unaffected: it never sends, connectivity
			// doesn't matter for holding a draft.
			if d.Gateway != nil && !d.Gateway.Connected() {
				return mcp.NewToolResultError("refusing to send: gateway is disconnected — the message would sit in the outbox with no ETA. Wait for reconnect or escalate to the owner."), nil
			}
			msg, err := r.RequireString("message")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}

			termID := terminalIDFromContext(ctx)
			now := time.Now().Unix()
			isPrincipal := d.PrincipalTerminalID != "" && termID == d.PrincipalTerminalID
			// T33 (ct-2026-08-06-1526): looked up unconditionally now (used to
			// be inside the `if !isPrincipal` block below) — markDispatchChatIfDifferent
			// needs it regardless of principal-ness, since a principal dispatch
			// can ALSO be answering on behalf of a different active chat.
			active, bound := gate.Active(termID)

			// ct-2026-07-13-2243: mark only what was actually dispatched. For
			// non-principal agents the gate stores the TS of the last burst
			// message; messages that arrived after the burst stay pending and
			// get re-dispatched. The principal is unrestricted — use now.
			markTS := now
			if !isPrincipal && bound {
				markTS = active.BurstMaxTS
			}

			// confirmation_mode (F4-DESIGN §4): "always" never sends
			// directly, fail-safe by code — creates a draft for the owner
			// to approve instead. "none"/"discretion" (or unset) send as
			// always.
			if c.ConfirmationMode == "always" {
				if err := d.Store.AddDraftWithConfirmer(to, msg, model, c.Confirmer, markTS, now); err != nil {
					return mcp.NewToolResultError(err.Error()), nil
				}
				if err := d.Store.MarkHandledBefore(to, markTS); err != nil {
					log.Printf("send_message: draft mark_handled_before %s: %v", to, err)
				}
				markDispatchChatIfDifferent(d, active, bound, to)
				gate.Consume(termID)
				publishDraftChanged(d.Bus)
				return mcp.NewToolResultText("held for confirmation (confirmation_mode=always) — awaiting owner approval"), nil
			}

			_ = d.State.React("responding", "replying...", 4*time.Second)
			if err := d.Store.EnqueueWithModel(to, msg, now, model); err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			// ST-D (ct-2026-07-11-074139): usage is metered once this message
			// actually leaves via WhatsApp (corepipeline.processOutbox, the
			// single real-send choke point), not here at enqueue time — a
			// message enqueued but never sent (killed, dead-lettered) must
			// not count as output.
			// One-shot: a sent dispatch can never be replayed.
			gate.Consume(termID)
			// ct-2026-07-13-2243: mark handled only up to burstMaxTS — messages
			// that arrived while the agent was composing stay pending and are
			// re-dispatched (with debounce) on the next capipush sweep.
			if err := d.Store.MarkHandledBefore(to, markTS); err != nil {
				log.Printf("send_message: mark_handled_before %s: %v", to, err)
			}
			markDispatchChatIfDifferent(d, active, bound, to)
			refreshQueue(d.State, d.Store)
			return mcp.NewToolResultText("queued for sending"), nil
		})

	// ── draft ────────────────────────────────────────────────────────────
	s.AddTool(mcp.NewTool("draft",
		mcp.WithDescription("Create a draft instead of sending directly — available in any confirmation_mode, for when you'd rather have the owner review before it goes out (see the sensitive-content checklist in the /piumy skill). Same guardrails as send_message (rules/claim/whitelist/policy_version), but this never sends: it always waits for approve_draft."),
		mcp.WithString("to", mcp.Required(), mcp.Description("Destination JID, e.g. 55500000001@c.us")),
		mcp.WithString("message", mcp.Required(), mcp.Description("Text to hold as a draft")),
		mcp.WithString("model", mcp.Required(), mcp.Description("Which model is drafting this")),
		mcp.WithString("policy_version", mcp.Description("Hash from get_decision_policy — call that tool first. Required for non-principal terminals; the principal (DefaultTerminalID) may omit it."))),
		func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			tracker.seen(ctx)
			to, err := r.RequireString("to")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			model, err := r.RequireString("model")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			policyVersion := r.GetString("policy_version", "")
			c, errMsg := validateSend(ctx, d, gate, to, model, policyVersion)
			if errMsg != "" {
				return mcp.NewToolResultError(errMsg), nil
			}
			msg, err := r.RequireString("message")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			termID := terminalIDFromContext(ctx)
			now := time.Now().Unix()
			isPrincipal := d.PrincipalTerminalID != "" && termID == d.PrincipalTerminalID
			// T33 (ct-2026-08-06-1526): looked up unconditionally, same reason
			// as send_message's own copy of this line.
			active, bound := gate.Active(termID)
			markTS := now
			if !isPrincipal && bound {
				markTS = active.BurstMaxTS
			}
			if err := d.Store.AddDraftWithConfirmer(to, msg, model, c.Confirmer, markTS, now); err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			// ct-2026-07-13-2243: mark handled up to burstMaxTS so the agent's
			// view of the chat is clean, but post-burst messages stay pending.
			if err := d.Store.MarkHandledBefore(to, markTS); err != nil {
				log.Printf("draft: mark_handled_before %s: %v", to, err)
			}
			markDispatchChatIfDifferent(d, active, bound, to)
			// ST-D (ct-2026-07-11-074139): NOT metered here — a draft that's
			// discarded (or never resolved) never reaches WhatsApp. Metering
			// happens once (if ever) at the real send in processOutbox.
			gate.Consume(termID)
			publishDraftChanged(d.Bus)
			return mcp.NewToolResultText("drafted, awaiting owner approval"), nil
		})

	// ── silent_act ───────────────────────────────────────────────────────
	s.AddTool(mcp.NewTool("silent_act",
		mcp.WithDescription("Use INSTEAD of send_message when the right move is not replying — the decision policy is explicit that always having the last word is a mistake. Unlike ignoring the dispatch, this is a deliberate action: it releases your turn immediately (the next chat doesn't wait up to 15 minutes for this one to go stale), marks this burst's messages as handled (they won't be re-dispatched), and optionally records why, so the owner can review your judgment instead of having to guess whether you're stuck. Always targets the CURRENT dispatch's chat — no override. Requires the same unlock -> remember/skip checkpoint as send_message."),
		mcp.WithString("reason", mcp.Description("Optional: why you're staying silent — e.g. \"ya tuve la última palabra\", \"no me corresponde\", \"spam\". Free text."))),
		func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			tracker.seen(ctx)
			termID := terminalIDFromContext(ctx)
			active, bound := gate.Active(termID)
			if !bound {
				return mcp.NewToolResultError("locked: no active dispatch for this terminal (default DENY) — call get_instructions first"), nil
			}
			if !active.Ready {
				return mcp.NewToolResultError("locked: call get_instructions -> unlock -> remember/skip before silent_act"), nil
			}
			reason := r.GetString("reason", "")
			if err := d.Store.SetChatSilence(active.ChatJID, reason, time.Now().Unix()); err != nil {
				log.Printf("silent_act: set silence %s: %v", active.ChatJID, err)
			}
			// Same bound applied by send_message's own MarkHandledBefore call:
			// only the dispatched burst, not messages that arrived afterward.
			if err := d.Store.MarkHandledBefore(active.ChatJID, active.BurstMaxTS); err != nil {
				log.Printf("silent_act: mark_handled_before %s: %v", active.ChatJID, err)
			}
			// One-shot, same as send_message/draft: releases the terminal so
			// the next dispatch doesn't wait on dispatchStaleAfter.
			gate.Consume(termID)
			return mcp.NewToolResultText("silence recorded — turn released, messages marked handled"), nil
		})
}
