// send_to_boss — T39 (ct-2026-08-08-1619), boss verbatim: "que tal una
// herramienta: send to boss en el mcp, que pueda usarlo cualqueira que
// tenga el mcp?", followed immediately by the amendment that defines the
// design: "pero que el agente se identifique".
//
// Deliberately OUTSIDE levelGateMiddleware (levelgate.go) — the first
// outbound tool in that condition, on purpose, not an oversight: every
// other send path (send_message/draft) requires an active dispatch, so a
// terminal with none can do nothing at all today (default-DENY). The real
// case this closes: an agent mid-task ("avisame por WhatsApp cuando
// termines") has no dispatch to answer and, until now, no way to reach the
// owner either.
//
// It's safe outside the gate for two reasons send_message doesn't share:
//   - No destination argument. The target is always store.BossJIDs() —
//     accepting a chat_id here would just be send_message with the
//     anti-leakage gate removed.
//   - The caller can't declare who it is. Identity comes from the
//     CONNECTION (X-Piumy-Terminal-Id, terminalIDFromContext), never a
//     parameter — nothing an agent types is trusted, only who it's
//     connected as. An unrecognized terminal_id is refused outright and
//     sends nothing (senderNameFor) — the gap CleverCoder flagged in
//     writing (an unrecognized terminal_id used to pass through silently).
package mcpserver

import (
	"context"
	"fmt"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// senderNameFor resolves termID's display name for the [name] prefix
// send_to_boss puts on every message — the principal (synthesized, never a
// real `agents` row — store.PrincipalAgent's own doc) or a registered
// secondary. ok=false means termID matches neither — the caller must call
// register_agent first.
//
// Secondaries are matched against AntennaTerminalID (via ListAgents), NOT
// GetAgent(termID)/AgentID: AgentID is fixed at registration (register_agent
// sets it to the CALLER's terminal_id at that moment), while
// AntennaTerminalID is the field set_agent_capi actually updates later. An
// agent whose antenna_terminal_id changed after registering would still
// connect with agent_id != its current X-Piumy-Terminal-Id — GetAgent(termID)
// would wrongly refuse a legitimately registered agent in that case. No new
// query: ListAgents already exists, this just filters it (Citrino's own
// instruction — match the CURRENT connection value, not the fixed identity).
//
// Falls back to termID itself when the matched agent has no Name set — the
// boss asked for "el ID de capi" as the signature; a registered display
// name is used instead when it exists because it's legible, with the raw
// id as the fallback that always works (revisable — flagged to Citrino).
func senderNameFor(d Deps, termID string) (name string, ok bool, err error) {
	if d.PrincipalTerminalID != "" && termID == d.PrincipalTerminalID {
		principal, found, err := d.Store.PrincipalAgent(d.PrincipalTerminalID)
		if err != nil || !found {
			return "", found, err
		}
		if principal.Name != "" {
			return principal.Name, true, nil
		}
		return termID, true, nil
	}
	agents, err := d.Store.ListAgents()
	if err != nil {
		return "", false, err
	}
	for _, a := range agents {
		if a.AntennaTerminalID != termID {
			continue
		}
		if a.Name != "" {
			return a.Name, true, nil
		}
		return termID, true, nil
	}
	return "", false, nil
}

func addSendToBossTool(s *server.MCPServer, d Deps, tracker *agentTracker) {
	s.AddTool(mcp.NewTool("send_to_boss",
		mcp.WithDescription("Send a WhatsApp message to the owner from any registered agent — no active dispatch required, unlike send_message. Use it to reach the owner outside a normal conversation turn (e.g. \"tell me when you're done\"). There is no destination argument: it always goes to the owner's own chat(s). The message is prefixed with your registered name (or terminal_id if you never set one) so the owner knows who's writing when more than one agent uses this. Queued through the normal anti-ban outbox, not sent instantly. Refuses if this terminal isn't a registered agent (register_agent first) — there is no way to send as someone else."),
		mcp.WithString("text", mcp.Required(), mcp.Description("The message text, sent as-is after the [name] prefix"))),
		func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			tracker.seen(ctx)
			termID := terminalIDFromContext(ctx)
			if termID == "" {
				return mcp.NewToolResultError("no terminal_id in context; set X-Piumy-Terminal-Id header"), nil
			}
			name, ok, err := senderNameFor(d, termID)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("store: %v", err)), nil
			}
			if !ok {
				return mcp.NewToolResultError(fmt.Sprintf("refused: terminal_id %q is not a registered agent — call register_agent first", termID)), nil
			}

			text, err := r.RequireString("text")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}

			bossJIDs, err := d.Store.BossJIDs()
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("store: %v", err)), nil
			}
			if len(bossJIDs) == 0 {
				return mcp.NewToolResultError("refused: no chat is marked as the owner (is_boss) yet"), nil
			}

			signed := "[" + name + "] " + text
			now := time.Now().Unix()
			for _, jid := range bossJIDs {
				if err := d.Store.EnqueueFromAgent(jid, signed, now, termID); err != nil {
					return mcp.NewToolResultError(fmt.Sprintf("store: %v", err)), nil
				}
			}
			return mcp.NewToolResultText("queued for sending"), nil
		})
}
