package mcpserver

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"piumy-gateway/internal/store"
)

func addAgentTools(s *server.MCPServer, d Deps) {
	// ── register_agent ───────────────────────────────────────────────────
	// Registers the calling terminal as a secondary agent with its own
	// cAPI antenna. The principal's slot (PortFallback) is RESERVED and
	// always refused here — the principal's injector is wired at startup
	// from config/env, not via this tool.
	s.AddTool(mcp.NewTool("register_agent",
		mcp.WithDescription("Register this terminal as a secondary agent with its own cAPI antenna. Role is always 'secondary' — the principal is wired at startup."),
		mcp.WithString("endpoint", mcp.Required(), mcp.Description("cAPI endpoint URL, e.g. http://192.168.1.10:8787")),
		mcp.WithString("antenna_terminal_id", mcp.Required(), mcp.Description("Antenna's terminal_id (chat GUID from CleverCoder)")),
		mcp.WithString("pinpass", mcp.Required(), mcp.Description("Antenna's pinpass (base64 form as copied)")),
		mcp.WithString("name", mcp.Description("Optional display name for this agent (default empty — can be set later via set_agent_capi)"))),
		func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			callerID := terminalIDFromContext(ctx)
			if callerID == "" {
				return mcp.NewToolResultError("no terminal_id in context; set X-Piumy-Terminal-Id header"), nil
			}

			// INVARIANTE (gate duro, CLAUDE.md): the principal's terminal_id
			// is RESERVED — a secondary registering as PortFallback would
			// hijack is_boss dispatch (capipush:242-248). One string compare.
			if d.PrincipalTerminalID != "" && callerID == d.PrincipalTerminalID {
				return mcp.NewToolResultError("forbidden: principal terminal_id is reserved; the principal's injector is wired at startup"), nil
			}

			endpoint, err := r.RequireString("endpoint")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			antTerminalID, err := r.RequireString("antenna_terminal_id")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			pinpass, err := r.RequireString("pinpass")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}

			a := store.Agent{
				AgentID:           callerID,
				Name:              r.GetString("name", ""),
				Endpoint:          endpoint,
				AntennaTerminalID: antTerminalID,
				Pinpass:           pinpass,
				Role:              "secondary",
			}
			if err := d.Store.UpsertAgent(a); err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("store: %v", err)), nil
			}
			if d.OnAgentUpsert != nil {
				d.OnAgentUpsert(callerID, endpoint, antTerminalID, pinpass)
			}
			return mcp.NewToolResultText(fmt.Sprintf(`{"agent_id":%q,"role":"secondary","status":"registered"}`, callerID)), nil
		})

	// ── set_agent_capi ───────────────────────────────────────────────────
	// Updates the cAPI credentials of an existing agent. The principal can
	// update any agent; a secondary can only update its own.
	s.AddTool(mcp.NewTool("set_agent_capi",
		mcp.WithDescription("Update the cAPI credentials of an existing agent. The principal can update any; a secondary can only update its own."),
		mcp.WithString("agent_id", mcp.Required(), mcp.Description("The agent to update (terminal_id)")),
		mcp.WithString("endpoint", mcp.Description("New cAPI endpoint URL (omit to keep current)")),
		mcp.WithString("antenna_terminal_id", mcp.Description("New antenna terminal_id (omit to keep current)")),
		mcp.WithString("pinpass", mcp.Description("New pinpass (omit to keep current)")),
		mcp.WithString("name", mcp.Description("New display name (omit to keep current)"))),
		func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			callerID := terminalIDFromContext(ctx)
			agentID := r.GetString("agent_id", "")
			if agentID == "" {
				return mcp.NewToolResultError("agent_id required"), nil
			}

			// INVARIANTE: principal terminal_id is RESERVED — nobody can
			// route its injector away, not even the principal itself via MCP.
			//
			// S9 (ct-2026-07-30-031143): the old message ("update via the
			// dashboard") sent the caller to a door that doesn't mention the
			// one that actually exists by MCP — set_capi_connector
			// (admin_tools.go). Verified before naming it: set_capi_connector
			// is in bossOnlyTools (levelgate.go), so it's reachable only from
			// the principal terminal itself or while handling an active
			// boss-level dispatch — never "any agent" — the corrected message
			// says that too, so it never points someone at a door they can't
			// open either.
			if d.PrincipalTerminalID != "" && agentID == d.PrincipalTerminalID {
				return mcp.NewToolResultError("forbidden: principal terminal_id is reserved; use set_capi_connector instead (requires an active boss-level dispatch, or calling from the principal terminal) — or the dashboard"), nil
			}

			// Secondary can only update its own agent_id.
			if callerID != d.PrincipalTerminalID && callerID != agentID {
				return mcp.NewToolResultError("forbidden: a secondary agent can only update its own cAPI credentials"), nil
			}

			existing, ok, err := d.Store.GetAgent(agentID)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("store: %v", err)), nil
			}
			if !ok {
				return mcp.NewToolResultError(fmt.Sprintf("agent %q not found; call register_agent first", agentID)), nil
			}

			if v := r.GetString("endpoint", ""); v != "" {
				existing.Endpoint = v
			}
			if v := r.GetString("antenna_terminal_id", ""); v != "" {
				existing.AntennaTerminalID = v
			}
			if v := r.GetString("pinpass", ""); v != "" {
				existing.Pinpass = v
			}
			if v := r.GetString("name", ""); v != "" {
				existing.Name = v
			}

			if err := d.Store.UpsertAgent(existing); err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("store: %v", err)), nil
			}
			if d.OnAgentUpsert != nil {
				d.OnAgentUpsert(agentID, existing.Endpoint, existing.AntennaTerminalID, existing.Pinpass)
			}
			return mcp.NewToolResultText(fmt.Sprintf(`{"agent_id":%q,"status":"updated"}`, agentID)), nil
		})

	// ── list_agents ──────────────────────────────────────────────────────
	// Principal FIRST, then every secondary (ct-2026-07-29, agentes paso 3
	// — before this, list_agents only ever saw secondaries; the principal
	// was invisible by MCP even though GET /api/agents, REST, always showed
	// it). store.PrincipalAgent is the SAME synthesis restapi.handleAgents
	// uses — one place reads the principal, not two.
	s.AddTool(mcp.NewTool("list_agents",
		mcp.WithDescription("List every agent — the principal first (if configured), then every registered secondary — with role and antenna info. pinpass is never returned in clear — only pinpass_set:bool.")),
		func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			type row struct {
				AgentID           string `json:"agent_id"`
				Name              string `json:"name"`
				Role              string `json:"role"`
				Endpoint          string `json:"endpoint"`
				AntennaTerminalID string `json:"antenna_terminal_id"`
				PinpassSet        bool   `json:"pinpass_set"`
			}
			var rows []row
			if principal, ok, err := d.Store.PrincipalAgent(d.PrincipalTerminalID); err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("store: %v", err)), nil
			} else if ok {
				rows = append(rows, row{
					AgentID: principal.AgentID, Name: principal.Name, Role: principal.Role,
					Endpoint: principal.Endpoint, AntennaTerminalID: principal.AntennaTerminalID,
					PinpassSet: principal.Pinpass != "",
				})
			}
			agents, err := d.Store.ListAgents()
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("store: %v", err)), nil
			}
			for _, a := range agents {
				rows = append(rows, row{
					AgentID:           a.AgentID,
					Name:              a.Name,
					Role:              a.Role,
					Endpoint:          a.Endpoint,
					AntennaTerminalID: a.AntennaTerminalID,
					PinpassSet:        a.Pinpass != "",
				})
			}
			if rows == nil {
				rows = []row{}
			}
			return jsonResult(rows)
		})

	// ── assign_chat_to_agent ─────────────────────────────────────────────
	// PRINCIPAL-ONLY (M4, ct-2026-07-22-1301 — boss verbatim: "por ahora que
	// sean solo con asignacion manual o por mcp de parte del agente
	// principal"). Same gate style as register_agent/set_agent_capi above
	// (an internal PrincipalTerminalID check, NOT bossOnlyTools — that gate
	// means something different: "usable while handling a boss-originated
	// dispatch", which would let a SECONDARY assign chats too; this must be
	// the principal terminal itself, always). Same write path as the
	// dashboard's POST /api/admin/agent-assign (M3): chats.status'
	// agent_exclusive:<id> form (store.AgentExclusiveStatus), the one
	// capipush.dispatch now reads (M4's core change).
	s.AddTool(mcp.NewTool("assign_chat_to_agent",
		mcp.WithDescription("PRINCIPAL-ONLY. Manually route a chat's dispatch to one agent (writes agent_exclusive:<id>), or clear the assignment back to the default fallback (omit agent_id or pass an empty string). The principal itself is never a valid assignment target — an unassigned chat already falls back to it."),
		mcp.WithString("chat_id", mcp.Required()),
		mcp.WithString("agent_id", mcp.Description("The agent to assign to. Omit or pass \"\" to unassign."))),
		func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			callerID := terminalIDFromContext(ctx)
			if d.PrincipalTerminalID == "" || callerID != d.PrincipalTerminalID {
				return mcp.NewToolResultError("forbidden: only the principal agent can assign chats to agents"), nil
			}
			chatID, err := r.RequireString("chat_id")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			agentID := r.GetString("agent_id", "")

			if agentID == "" {
				if err := d.Store.SetStatus(chatID, "new"); err != nil {
					return mcp.NewToolResultError(fmt.Sprintf("store: %v", err)), nil
				}
				return mcp.NewToolResultText(fmt.Sprintf(`{"chat_id":%q,"status":"unassigned"}`, chatID)), nil
			}
			if agentID == d.PrincipalTerminalID {
				return mcp.NewToolResultError("cannot assign to the principal — it's already the default fallback for an unassigned chat"), nil
			}
			if _, ok, err := d.Store.GetAgent(agentID); err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("store: %v", err)), nil
			} else if !ok {
				return mcp.NewToolResultError(fmt.Sprintf("unknown agent_id %q", agentID)), nil
			}
			if err := d.Store.SetStatus(chatID, store.AgentExclusiveStatus(agentID)); err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("store: %v", err)), nil
			}
			return mcp.NewToolResultText(fmt.Sprintf(`{"chat_id":%q,"agent_id":%q,"status":"assigned"}`, chatID, agentID)), nil
		})

	// ── delete_agent ─────────────────────────────────────────────────────
	// PRINCIPAL-ONLY (ct-2026-07-29, agentes paso 3 — same gate style as
	// assign_chat_to_agent above: deleting an agent is at least as
	// consequential as assigning its chats, so it gets the same "only the
	// principal terminal itself" check, not a self-service one). Reuses the
	// EXACT same path POST /api/admin/agent-delete (restapi/admin.go) uses
	// — store.UnassignAllChatsForAgent then store.DeleteAgent then
	// d.OnAgentDelete — not a reimplementation. Without the unassign step, a
	// deleted agent leaves dangling agent_exclusive:<id> chats (boss:
	// "ningún chat queda apuntando a un agente que ya no existe"); without
	// OnAgentDelete (capipush.Pusher.UnregisterInjector), its OLD
	// credentials keep dispatching from memory after the DB row is gone
	// (boss: "un borrado que deja las credenciales vivas es un borrado que
	// miente").
	s.AddTool(mcp.NewTool("delete_agent",
		mcp.WithDescription("PRINCIPAL-ONLY. Permanently delete a secondary agent: unassigns every chat exclusively routed to it (they revert to the normal router/principal fallback) and stops its live injector from dispatching immediately. The principal itself can never be deleted — it isn't a real agent row."),
		mcp.WithString("agent_id", mcp.Required())),
		func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			callerID := terminalIDFromContext(ctx)
			if d.PrincipalTerminalID == "" || callerID != d.PrincipalTerminalID {
				return mcp.NewToolResultError("forbidden: only the principal agent can delete agents"), nil
			}
			agentID, err := r.RequireString("agent_id")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			if agentID == d.PrincipalTerminalID {
				return mcp.NewToolResultError("cannot delete the principal — it isn't a real agent row"), nil
			}
			if _, ok, err := d.Store.GetAgent(agentID); err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("store: %v", err)), nil
			} else if !ok {
				return mcp.NewToolResultError(fmt.Sprintf("unknown agent_id %q", agentID)), nil
			}
			unassigned, err := d.Store.UnassignAllChatsForAgent(agentID)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("store: %v", err)), nil
			}
			if err := d.Store.DeleteAgent(agentID); err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("store: %v", err)), nil
			}
			if d.OnAgentDelete != nil {
				d.OnAgentDelete(agentID)
			}
			return mcp.NewToolResultText(fmt.Sprintf(`{"agent_id":%q,"status":"deleted","chats_unassigned":%d}`, agentID, unassigned)), nil
		})
}
