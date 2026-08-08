// Gating por nivel (F4-DESIGN §3) — the middleware that enforces
// anti-leakage for caution/danger dispatches: enumeration tools are fully
// unavailable, and every chat-scoped tool is pinned to the dispatch's own
// chat. A boss-level dispatch (an explicit dispatch with Level ==
// LevelBoss, never the absence of one — F4b) is unrestricted by design.
//
// Default DENY (F4b): a terminal with NO active dispatch is denied every
// gated tool below — this replaces F4a's fail-open "no dispatch =
// unrestricted". Tools with no chat concept at all (get_status,
// get_decision_policy, the 4 gate tools) were never part of this gating
// and stay open regardless — requiring a dispatch just to read the queue
// depth would gate harmless reads for no anti-leakage benefit.
//
// send_to_boss (send_to_boss.go, T39, ct-2026-08-08-1619) joins that same
// exemption, but it's the first one with a real side effect (it sends) —
// deliberate, not an oversight. Never add it to bossOnlyTools/
// enumerationTools/chatScopedArg below: it has no destination argument (the
// owner's chat only, store.BossJIDs()) and resolves its caller from the
// connection, not a parameter — see that file's own doc for why that makes
// it safe here.
package mcpserver

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// enumerationTools reveal the EXISTENCE or content of OTHER chats, not
// just the dispatch's own — caution/danger must not see these at all
// (anti-leakage). get_outbox/get_drafts are here (not chatScopedArg)
// because PendingOutbox/PendingDrafts take no chat_id at all — they
// return every chat's queued sends/drafts unfiltered, so a caution/danger
// dispatch could read what's pending for the boss's OTHER contacts
// (found in audit, ct-2026-07-09-1641-f4b): boss-only for now
// (scope-filtering the result by chat would need a handler change, not
// just this middleware — the minimum-safe fix ships first, tracked as a
// follow-up if MVP needs finer scoping later).
var enumerationTools = map[string]bool{
	"list_chats":      true,
	"get_pending":     true,
	"get_queue":       true,
	"get_chat_groups": true,
	"get_outbox":      true,
	"get_drafts":      true,
}

// chatScopedArg names, per tool, which argument carries the target chat —
// caution/danger may only pass the dispatch's own chat_jid there.
// send_message enforces the same rule itself (server.go) since it already
// reads the dispatch for its ready-state check; duplicating it here too
// would just be two places enforcing one rule.
// approverEnumerationTools (Aprobador P1, ct-2026-07-31-0610) are the ONLY
// two enumerationTools entries an Approver-level dispatch may reach —
// get_drafts/get_pending, so it can see the cross-chat queue it needs to
// approve OTHER chats' drafts (approve_draft/discard_draft themselves are
// never in enumerationTools/chatScopedArg at all — their own handlers in
// admin_tools.go gate them directly, see isActiveApproverDispatch). Every
// OTHER enumerationTools entry (list_chats/get_queue/get_chat_groups/
// get_outbox) and every bossOnlyTools entry stay refused for Approver
// exactly like Caution/Danger — this map is the ENTIRE extra surface an
// approver gets, on purpose: an approver is a single-purpose grant, not a
// smaller boss. Widening this map widens what "aprobador" means; do not
// add to it without the same explicit decision the boss made for these two.
var approverEnumerationTools = map[string]bool{
	"get_drafts":  true,
	"get_pending": true,
}

var chatScopedArg = map[string]string{
	"get_chat":         "chat_id",
	"get_messages":     "chat_id",
	"set_chat_status":  "chat_id",
	"set_chat_active":  "chat_id",
	"set_chat_memory":  "chat_id",
	"set_chat_context": "chat_id",
	"get_media":        "chat_id",
	"get_media_full":   "chat_id",
	"set_mode":         "chat_id",
	"escalate":         "chat_id",
	"mark_handled":     "chat_id",
	"resolve_chat":     "chat_id",
	"claim_chat":       "chat_id",
	"release_chat":     "chat_id",
}

// bossOnlyTools are refused outright for any non-boss dispatch. DB-admin
// and group/profile tools (F4c) all land here — caution/danger must NEVER
// reach them, by construction (F4-DESIGN §3).
//
// set_is_boss/set_type_rules/set_default_rules/set_confirmation_mode/
// set_config_level/approve_draft/discard_draft are DELIBERATELY absent
// (S10, ct-2026-07-30-1349; approve_draft/discard_draft added S12,
// ct-2026-07-30-1622, same hole, same fix): this map is binary (boss or
// nothing), but these need per-ARGUMENT or per-TOOL logic (restricting is
// always allowed; only liberating needs an active boss dispatch; is_boss/
// type/default rules are never MCP-settable, not even by the principal) —
// that can't live in a name→bool map. Their own handlers (admin_tools.go)
// enforce it directly, which also closes the hole this middleware's
// principal bypass (below) otherwise left open: verified live (Citrino,
// 2026-07-30) that a non-principal-aware bypass let ANY MCP caller on the
// principal terminal rewrite rules/is_boss with zero gating — S12 found
// the identical shape on approve_draft. Putting the check in the tool
// handler itself means it holds regardless of what this middleware
// decides.
//
// set_chat_rules is ALSO absent, but for a different reason since T31
// (ct-2026-08-06-0244): it isn't self-gated anymore either — the boss
// unblocked it unconditionally (see its own doc, admin_tools.go). Absent
// from bossOnlyTools, chatScopedArg, AND selfGatedTools — nothing in this
// file touches it at all, on purpose.
var bossOnlyTools = map[string]bool{
	"reset_dashboard_password": true,

	// Anti-ban emergency stop (H2+H3 hardening, ct-2026-07-10-0540) — an
	// agent must never be able to un-kill itself.
	"set_kill_switch": true,

	// cAPI connector re-cableo (ct-2026-07-18-1638) — sensitive credentials
	// (terminal_id/pinpass), same class as set_kill_switch.
	"set_capi_connector": true,

	// Group/profile — irreversible, account-wide WhatsApp actions.
	"create_group":          true,
	"add_participant":       true,
	"set_group_icon":        true,
	"set_group_description": true,
	"set_profile_status":    true,
}

// selfGatedTools (S10, ct-2026-07-30-1349; approve_draft/discard_draft
// added S12, ct-2026-07-30-1622) are the tools whose authorization lives in
// their OWN handler (admin_tools.go), not in bossOnlyTools — a witness set
// purely for tests to assert "every privileged tool is gated SOMEHOW"
// without hardcoding these names a second time. levelGateMiddleware never
// reads this map; it's not part of the enforcement, only of confirming the
// enforcement exists elsewhere.
// set_chat_rules is deliberately NOT here since T31 (ct-2026-08-06-0244) —
// it has no gating logic left in its handler either, unconditional by the
// boss's own explicit call. See its own doc (admin_tools.go).
var selfGatedTools = map[string]bool{
	"set_is_boss":           true,
	"set_type_rules":        true,
	"set_default_rules":     true,
	"set_confirmation_mode": true,
	"set_config_level":      true,
	// approve_draft (sends → liberates) requires isActiveBossDispatch,
	// same reasoning as set_confirmation_mode's "none"/set_config_level's
	// "auto" — the boss saying "aprobá los pendientes" registers a
	// boss-level dispatch, so it works; a caution/danger dispatch (the
	// chat's own interested party talking the agent into it) cannot grant
	// itself this. discard_draft (never sends → restricts) has NO check at
	// all — always allowed, same "restricting is free" reasoning as
	// "always"/"discretion"/"confirm"/"unattended"/"ignored".
	"approve_draft": true,
	"discard_draft": true,
	// reject_draft/edit_draft (T15, ct-2026-08-05-123241) — same family as
	// discard_draft: never send, so no check at all, always allowed. reject
	// asks for another attempt (reason travels via capipush's redispatch),
	// edit replaces the text in place — neither causes a send by itself.
	"reject_draft": true,
	"edit_draft":   true,
	// set_is_approver (Aprobador P1, ct-2026-07-31-0610): the pin itself —
	// isActiveBossDispatch-gated, same reasoning as set_is_boss being
	// boss-only, except this one DOES have a valid MCP path (the boss's own
	// clarification: "la ia tambien puede cambiar el pin por mcp pero solo
	// si el boss lo manda") rather than being blocked unconditionally.
	"set_is_approver": true,
}

func levelGateMiddleware(gate *Gate, principalID string) server.ToolHandlerMiddleware {
	return func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
		return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			// Principal bypass: full authority, no dispatch needed.
			if principalID != "" && terminalIDFromContext(ctx) == principalID {
				return next(ctx, req)
			}
			name := req.Params.Name
			_, chatScoped := chatScopedArg[name]
			isGated := bossOnlyTools[name] || enumerationTools[name] || chatScoped

			active, ok := gate.Active(terminalIDFromContext(ctx))
			if !ok {
				if isGated {
					return mcp.NewToolResultError("refused: no active dispatch for this terminal (default DENY) — call get_instructions first"), nil
				}
				return next(ctx, req)
			}
			if active.Level == LevelBoss {
				// ST-A security fix (ct-2026-07-11-0740): a done (consumed)
				// dispatch must not keep granting the boss bypass — before
				// this, Level==Boss alone skipped every check below forever,
				// even after the dispatch that earned it was long consumed
				// (Consume marks state gateDone, never evicts byTerminal).
				// Ready now tracks "usable" for boss too (RegisterDispatch
				// starts boss dispatches gateReady; Consume ends them
				// gateDone), so this only bypasses gating while still valid.
				if !active.Ready {
					return mcp.NewToolResultError("locked: this dispatch was already consumed — call get_instructions for a new one first"), nil
				}
				return next(ctx, req)
			}

			// Aprobador P1 (ct-2026-07-31-0610): a NARROW bypass, not the
			// boss branch's blanket one above — only for the two names in
			// approverEnumerationTools, and only once this dispatch has
			// actually completed the gate ritual (Ready), same requirement
			// as boss. Falling through below for every other tool name means
			// an Approver still gets bossOnlyTools/enumerationTools/
			// chatScopedArg enforcement identically to Caution/Danger — this
			// is the entire point: one extra grant, not a smaller boss.
			if active.Level == LevelApprover && approverEnumerationTools[name] {
				if !active.Ready {
					return mcp.NewToolResultError("locked: this dispatch has not completed get_instructions -> unlock -> remember/skip yet"), nil
				}
				return next(ctx, req)
			}

			if bossOnlyTools[name] {
				return mcp.NewToolResultError("refused: " + name + " is boss-only"), nil
			}
			if enumerationTools[name] {
				return mcp.NewToolResultError("refused: " + name + " is not available at this access level (anti-leakage)"), nil
			}
			if argName, scoped := chatScopedArg[name]; scoped {
				if target := req.GetString(argName, ""); target != "" && target != active.ChatJID {
					return mcp.NewToolResultError("refused: this dispatch is scoped to " + active.ChatJID + ", not " + target + " (anti-leakage)"), nil
				}
			}
			return next(ctx, req)
		}
	}
}
