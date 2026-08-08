// Package mcpserver expone las tools MCP del agente: el core NO responde
// solo, expone la cola y las acciones para que un agente externo (por MCP)
// lea, actúe y responda. Este es el seam core<->cerebro.
//
// Tracking de actividad del agente: cada llamada a una tool marca su sesión
// MCP como vista, manteniendo Agents/AgentConnected en status.json al día.
// Un sweeper de fondo evict-ea sesiones idle tras AgentIdle sin llamadas.
package mcpserver

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"golang.org/x/crypto/bcrypt"

	"piumy-gateway/internal/eventbus"
	"piumy-gateway/internal/gateway"
	"piumy-gateway/internal/governor"
	"piumy-gateway/internal/mcpguard"
	"piumy-gateway/internal/router"
	"piumy-gateway/internal/state"
	"piumy-gateway/internal/store"
	"piumy-gateway/internal/version"
)

// Deps bundles everything the MCP server needs.
type Deps struct {
	Store     *store.Store
	State     *state.Manager
	Router    *router.Manager
	AgentIdle time.Duration // how long with no calls before AgentConnected clears

	// Governor is the anti-ban rate limiter — needed by set_kill_switch
	// (admin_tools.go, H2/H3 hardening ct-2026-07-10-0540) to flip the kill
	// switch. nil-safe: set_kill_switch just no-ops the governor side if
	// unset (main.go always wires the real one).
	Governor *governor.Limiter

	// Gateway is the messaging client (whatsmeow) — needed by send_message
	// (H6 hardening, ct-2026-07-10-0540) to refuse outright when
	// disconnected, instead of silently enqueueing into an outbox with no
	// ETA. nil-safe: skips the check (tests that never wire a Gateway keep
	// working as before).
	Gateway gateway.Gateway

	// ReadMarker marks inbound messages read when an agent actually
	// retrieves them (get_messages) — nil is a valid no-op.
	ReadMarker ReadMarker

	// PolicyPath is the editable decision-policy file — the owner edits it
	// live, no recompile. Empty or unreadable falls back to the embedded
	// default.
	PolicyPath string

	// Guard is the anti-flood limiter — nil is fine (New builds a
	// default-config one) so the MCP surface is never left unprotected.
	Guard *mcpguard.Guard

	// Gate is the lock/unlock/noting/ready state machine (gate.go) — nil is
	// fine (New builds one). capipush (F4b) calls Gate.RegisterDispatch per
	// dispatch; exposed here so callers outside this package can reach it.
	Gate *Gate

	// PrincipalTerminalID is the terminal that operates on behalf of the boss
	// (PIUMY_DEFAULT_TERMINAL_ID) — it bypasses the default-DENY gate entirely
	// and can call any tool without an active dispatch. Empty = no principal
	// (all terminals gated normally). Non-principal terminals are unaffected.
	PrincipalTerminalID string

	// ClaimTTLDefault is claim_chat's TTL when the caller omits ttl_sec.
	// Zero/negative falls back to 5 minutes.
	ClaimTTLDefault time.Duration

	// MCPAuthConfigured is true when PIUMY_MCP_KEY is set — the only
	// owner-identity signal MCP has. reset_dashboard_password refuses
	// outright when this is false. Passed as a bool, never the secret
	// itself.
	MCPAuthConfigured bool

	// GroupProfile is the client's group/profile admin surface — needed
	// only by the 5 boss-only group/profile tools (group_tools.go), which
	// call methods (CreateGroup, AddParticipant, ...) that aren't part of
	// the gateway.Gateway seam (F2) and never will be: a future adaptador
	// (Cloud API oficial, F6) handles groups differently, so inflating
	// Gateway for this one implementer would be the wrong trade. nil is
	// valid — those 5 tools refuse with "not available" instead of a
	// nil-pointer panic. *whatsmeow.Adapter satisfies this (ST-E,
	// ct-2026-07-11-1444, replaces the deleted internal/openwa).
	GroupProfile GroupProfile

	// OnAgentUpsert is called after register_agent or set_agent_capi
	// successfully persists a change — wires a new or updated CleverInjector
	// into the Pusher's injector map. nil-safe: agent tools still persist to
	// store, just skip the live injector update (useful in tests).
	OnAgentUpsert func(agentID, endpoint, terminalID, pinpass string)

	// OnAgentDelete is called after delete_agent successfully removes an
	// agent (agent_tools.go, ct-2026-07-29, agentes paso 3) — the MCP twin
	// of restapi.Deps.OnAgentDelete, both wired in main.go to the same
	// capipush.Pusher.UnregisterInjector closure. nil-safe: delete_agent
	// still persists the deletion, just skips unregistering the live
	// injector (useful in tests) — production wiring must never leave this
	// nil, or a deleted agent's old credentials keep dispatching from
	// memory.
	OnAgentDelete func(agentID string)

	// Connector is the live cAPI injector — set_capi_connector (admin_tools.go,
	// ct-2026-07-18-1638) reconfigures it in-hot, same as restapi's own
	// CAPIConnector field (*capipush.CleverInjector satisfies both — kept as
	// two structurally-identical interfaces, one per surface, rather than a
	// shared import: mcpserver and restapi are parallel surfaces, neither
	// depends on the other). nil -> set_capi_connector only persists to KV.
	Connector CAPIConnector

	// Bus nudges the dashboard's SSE auto-refresh (T16, ct-2026-08-05-123257)
	// when a draft is created or resolved via MCP (draft/approve_draft/
	// discard_draft/reject_draft/edit_draft) — "draft" event, JID-less (the
	// dashboard just refetches the whole pending list, same as
	// wa_connected/history_batch). nil-safe (eventbus.Bus itself no-ops on a
	// nil receiver) — tests that never wire one keep working unchanged.
	Bus *eventbus.Bus
}

// CAPIConnector is the reconfigurable cAPI injector set_capi_connector
// reconfigures at runtime — same shape as restapi.CAPIConnector, defined
// separately so mcpserver never imports restapi.
type CAPIConnector interface {
	SetConfig(endpoint, terminalID, pinpass string)
}

// ReadMarker is the subset of *corepipeline.Controller mcpserver needs —
// defined here (not imported) to avoid an import cycle.
type ReadMarker interface {
	MarkRead(chatJID string, msgs []store.Message)
}

//go:embed decision-policy.md
var defaultDecisionPolicy string

// Piumy's own manuals (ct-2026-07-31-1541) — same embed pattern as
// defaultDecisionPolicy above, copied on purpose ("copiá ese patrón, no
// inventes uno"). Before this, piumy-orchestrator/piumy-operator only
// existed as Claude Code skills under .claude/skills/ — useless to any
// OTHER agent connected over MCP (DeepSeek, a local model, anything that
// isn't Claude Code with those files installed). Embedded, any agent that
// connects gets them for free, no install, no sync. The repo copies here
// are now the SOURCE — .claude/skills/piumy-*/ are copies of these, each
// carrying a note saying so.
//
//go:embed manuals/orchestrator/SKILL.md
var orchestratorManualSkill string

//go:embed manuals/orchestrator/escenarios.md
var orchestratorManualEscenarios string

//go:embed manuals/orchestrator/perillas.md
var orchestratorManualPerillas string

//go:embed manuals/orchestrator/operacion.md
var orchestratorManualOperacion string

//go:embed manuals/orchestrator/direccion.md
var orchestratorManualDireccion string

//go:embed manuals/operator/SKILL.md
var operatorManual string

//go:embed manuals/connect/SKILL.md
var connectManual string

// manualFor returns the full manual text for role ("orchestrator",
// "operator", or "connect"), or ok=false for anything else. The orchestrator
// manual is its 5 files joined in the SAME reading order SKILL.md's own
// "Módulos" table gives (entry point first, then each detail file) — a
// non-Claude-Code agent has no way to lazily open escenarios.md/perillas.md/
// etc. on its own the way a Skill-aware agent would, so it needs the whole
// thing in one call. The operator and connect manuals are each a single
// self-contained file.
func manualFor(role string) (manual string, ok bool) {
	switch role {
	case "orchestrator":
		return strings.Join([]string{
			orchestratorManualSkill,
			orchestratorManualEscenarios,
			orchestratorManualPerillas,
			orchestratorManualOperacion,
			orchestratorManualDireccion,
		}, "\n\n---\n\n"), true
	case "operator":
		return operatorManual, true
	case "connect":
		return connectManual, true
	default:
		return "", false
	}
}

// manualWithVersionStamp appends the running build's version as a trailing
// HTML comment — injected here at serve time, never written into the
// embedded .md source, so it can never drift from what's actually running
// (ct-2026-08-07, sello de versión: antes de esto no había forma de saber
// si un manual leído era el de la versión que corre).
func manualWithVersionStamp(manual string) string {
	return manual + "\n\n<!-- piumy-skill-version: " + version.Version + " -->\n"
}

// decisionPolicy returns the current decision policy content and its
// sha256 hash (policy_version), fresh on every call — an owner edit takes
// effect immediately, no restart needed. Falls back to the embedded
// default if path is empty or unreadable (fail-safe: the gate must always
// have SOME policy to enforce).
func decisionPolicy(path string) (content, version string) {
	content = defaultDecisionPolicy
	if path != "" {
		if data, err := os.ReadFile(path); err == nil {
			content = string(data)
		}
	}
	sum := sha256.Sum256([]byte(content))
	return content, hex.EncodeToString(sum[:])
}

// jsonResult marshals v as indented JSON into a tool result.
func jsonResult(v any) (*mcp.CallToolResult, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(string(b)), nil
}

// agentTracker monitors MCP tool calls per session and drives
// Agents/AgentConnected. sessions is keyed by MCP session ID (empty string
// for a caller with no identifiable session — same shared-bucket fallback
// as mcpguard).
type agentTracker struct {
	state     *state.Manager
	idleAfter time.Duration

	mu       sync.Mutex
	sessions map[string]time.Time
}

func newAgentTracker(sm *state.Manager, idle time.Duration) *agentTracker {
	if idle <= 0 {
		idle = 120 * time.Second
	}
	return &agentTracker{state: sm, idleAfter: idle, sessions: map[string]time.Time{}}
}

func sessionKey(ctx context.Context) string {
	if s := server.ClientSessionFromContext(ctx); s != nil {
		return s.SessionID()
	}
	return ""
}

// seen marks a tool call for this session. Only a brand-new session
// touches status.json — a repeat call from an already-tracked session is
// free.
func (t *agentTracker) seen(ctx context.Context) {
	key := sessionKey(ctx)
	t.mu.Lock()
	_, existed := t.sessions[key]
	prevN := len(t.sessions)
	t.sessions[key] = time.Now()
	n := len(t.sessions)
	t.mu.Unlock()

	if !existed {
		_ = t.state.Update(func(s *state.Status) {
			s.Agents = n
			s.AgentConnected = n > 0
		})
	}
	if prevN == 0 && n > 0 {
		_ = t.state.React("ai_online", "brain online!", 4*time.Second)
	}
}

// sweep runs until ctx is cancelled, evicting idle sessions every 10s.
func (t *agentTracker) sweep(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.mu.Lock()
			before := len(t.sessions)
			for key, last := range t.sessions {
				if time.Since(last) >= t.idleAfter {
					delete(t.sessions, key)
				}
			}
			n := len(t.sessions)
			t.mu.Unlock()

			if n == before {
				continue
			}
			if n == 0 {
				log.Println("mcpserver: agent idle — clearing AgentConnected")
			}
			_ = t.state.Update(func(s *state.Status) {
				s.Agents = n
				s.AgentConnected = n > 0
			})
			if n == 0 {
				_ = t.state.SetResting()
			}
		}
	}
}

// refreshQueue counts pending dedicated messages and updates state.Queue.
func refreshQueue(sm *state.Manager, st *store.Store) {
	count, err := st.CountPendingDedicated()
	if err != nil {
		log.Printf("mcpserver: count queue: %v", err)
		return
	}
	_ = sm.Update(func(s *state.Status) { s.Queue = count })
}

// publishDraftChanged nudges the dashboard's SSE auto-refresh whenever a
// draft is created or resolved (T16, ct-2026-08-05-123257) — JID-less like
// wa_connected/history_batch: the dashboard just refetches the whole
// pending-drafts list, it doesn't need to know which chat changed.
func publishDraftChanged(bus *eventbus.Bus) {
	bus.Publish(eventbus.Event{Type: "draft", TS: time.Now().Unix()})
}

// claimTTLCeiling is claim_chat's hard cap on ttl_sec — no dashboard/KV
// knob yet (YAGNI, single-agent use doesn't need tuning it).
const claimTTLCeiling = 30 * time.Minute

// New builds the MCP server with the switchboard's 23 tools and starts the
// agent idle sweeper goroutine. ctx controls the sweeper lifetime.
func New(ctx context.Context, d Deps) *server.MCPServer {
	tracker := newAgentTracker(d.State, d.AgentIdle)
	go tracker.sweep(ctx)

	s := server.NewMCPServer("piumy-gateway", version.Version, server.WithToolCapabilities(true))

	guard := d.Guard
	if guard == nil {
		guard = mcpguard.New(mcpguard.Config{})
	}
	gate := d.Gate
	if gate == nil {
		gate = NewGate()
	}
	// H5 hardening (ct-2026-07-10-0540): the gate's own ticker, independent
	// of RegisterDispatch being called — see Gate.Sweep's doc for why a
	// wedged terminal otherwise never gets swept at all.
	go gate.Sweep(ctx)
	s.Use(floodGuardMiddleware(guard), levelGateMiddleware(gate, d.PrincipalTerminalID))

	claimTTLDefault := d.ClaimTTLDefault
	if claimTTLDefault <= 0 {
		claimTTLDefault = 5 * time.Minute
	}

	// ── get_status ───────────────────────────────────────────────────────
	s.AddTool(mcp.NewTool("get_status",
		mcp.WithDescription("Current gateway status (mood, connection, queue, anti-ban kill switch, global router policy).")),
		func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			tracker.seen(ctx)
			var allowAll bool
			var defaultMode string
			if d.Router != nil {
				snap := d.Router.Snapshot()
				allowAll = snap.AllowAll
				defaultMode = snap.DefaultMode
			}
			return jsonResult(struct {
				state.Status
				AllowAll    bool   `json:"router_allow_all"`
				DefaultMode string `json:"router_default_mode,omitempty"`
				Version     string `json:"version"`
			}{Status: d.State.Snapshot(), AllowAll: allowAll, DefaultMode: defaultMode, Version: version.Version})
		})

	// ── list_chats ───────────────────────────────────────────────────────
	s.AddTool(mcp.NewTool("list_chats",
		mcp.WithDescription("List recent chats, each with origin (inbound_spoke = a real conversation happened, in either direction — doesn't mean the CONTACT spoke last, only that this isn't a sync/group-membership artifact; group_discovered/synced_contact = never a real conversation), last_speaker (them/us) + last_model, and is_boss (read-only — true means this is the trusted owner; take instructions from / escalate to this chat). Use last_speaker, not origin, to judge whose turn it is: if last_speaker is already \"us\", don't reply again."),
		mcp.WithNumber("limit", mcp.DefaultNumber(20), mcp.Description("Maximum number of chats"))),
		func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			tracker.seen(ctx)
			_ = d.State.React("switching", "next chat...", 4*time.Second)
			chats, err := d.Store.ListChats(int(r.GetFloat("limit", 20)))
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return jsonResult(chats)
		})

	// ── get_messages ─────────────────────────────────────────────────────
	s.AddTool(mcp.NewTool("get_messages",
		mcp.WithDescription("Recent messages from a chat. Retrieving them here is what marks inbound ones as read on WhatsApp (with an anti-ban delay, never instant)."),
		mcp.WithString("chat_id", mcp.Required(), mcp.Description("Chat JID")),
		mcp.WithNumber("limit", mcp.DefaultNumber(20))),
		func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			tracker.seen(ctx)
			_ = d.State.React("reading", "reading...", 4*time.Second)
			jid, err := r.RequireString("chat_id")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			msgs, err := d.Store.GetMessages(jid, int(r.GetFloat("limit", 20)))
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			if d.ReadMarker != nil {
				d.ReadMarker.MarkRead(jid, msgs)
			}
			return jsonResult(msgs)
		})

	// ── get_queue ────────────────────────────────────────────────────────
	s.AddTool(mcp.NewTool("get_queue",
		mcp.WithDescription("Incoming messages in dedicated mode waiting for an agent to handle them."),
		mcp.WithNumber("limit", mcp.DefaultNumber(20))),
		func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			tracker.seen(ctx)
			_ = d.State.React("working", "on it", 3*time.Second)
			msgs, err := d.Store.PendingDedicated(int(r.GetFloat("limit", 20)))
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			refreshQueue(d.State, d.Store)
			return jsonResult(msgs)
		})

	// ── get_decision_policy ──────────────────────────────────────────────
	s.AddTool(mcp.NewTool("get_decision_policy",
		mcp.WithDescription("The agent's decision policy — READ THIS BEFORE deciding whether to reply to any chat, and before every send_message call (policy_version may have changed). Returns the policy text and policy_version (a hash) that send_message requires verbatim.")),
		func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			tracker.seen(ctx)
			content, version := decisionPolicy(d.PolicyPath)
			return jsonResult(struct {
				Policy        string `json:"policy"`
				PolicyVersion string `json:"policy_version"`
			}{content, version})
		})

	// ── get_manual ───────────────────────────────────────────────────────
	// ct-2026-07-31-1541: ONE tool with a role parameter, not two — the
	// orchestrator and operator manuals are the same kind of resource, and
	// two tools means a third the day a third role exists. Never gated by
	// level, same as get_decision_policy above (not in bossOnlyTools/
	// enumerationTools/chatScopedArg — levelGateMiddleware passes any
	// unlisted tool straight through regardless of dispatch state): an
	// agent has to be able to read its own manual before it has any work
	// assigned, not after.
	s.AddTool(mcp.NewTool("get_manual",
		mcp.WithDescription("The Piumy manual for your role — READ THIS before you have any work assigned. role=\"connect\" if you still need to wire yourself to the gateway; role=\"orchestrator\" if you set up/change the gateway (levels, owner, approvers, scenarios); role=\"operator\" if you answer WhatsApp dispatches. Never gated: available with no dispatch bound."),
		mcp.WithString("role", mcp.Required(), mcp.Enum("connect", "orchestrator", "operator"))),
		func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			tracker.seen(ctx)
			role, err := r.RequireString("role")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			manual, ok := manualFor(role)
			if !ok {
				return mcp.NewToolResultError("unknown role " + role + " — expected connect, orchestrator, or operator"), nil
			}
			return mcp.NewToolResultText(manualWithVersionStamp(manual)), nil
		})

	addSendTools(s, d, gate, tracker)

	// ── set_mode ─────────────────────────────────────────────────────────
	s.AddTool(mcp.NewTool("set_mode",
		mcp.WithDescription("Change a chat's mode: auto (the API replies) or dedicated (an agent handles it, over MCP or pushed via cAPI)."),
		mcp.WithString("chat_id", mcp.Required()),
		mcp.WithString("mode", mcp.Required(), mcp.Enum("auto", "dedicated"))),
		func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			tracker.seen(ctx)
			jid, err := r.RequireString("chat_id")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			mode, err := r.RequireString("mode")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			if err := d.Store.SetMode(jid, mode); err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			refreshQueue(d.State, d.Store)
			return mcp.NewToolResultText("mode updated to " + mode), nil
		})

	// ── escalate ─────────────────────────────────────────────────────────
	s.AddTool(mcp.NewTool("escalate",
		mcp.WithDescription("Dedicate a chat to the agent (dedicated mode) so a more capable agent/model takes it."),
		mcp.WithString("chat_id", mcp.Required())),
		func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			tracker.seen(ctx)
			_ = d.State.React("thinking", "escalating...", 4*time.Second)
			jid, err := r.RequireString("chat_id")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			if err := d.Store.SetMode(jid, "dedicated"); err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			refreshQueue(d.State, d.Store)
			return mcp.NewToolResultText("dedicated to the agent"), nil
		})

	// ── mark_handled ─────────────────────────────────────────────────────
	s.AddTool(mcp.NewTool("mark_handled",
		mcp.WithDescription("Mark a queued message as handled."),
		mcp.WithString("chat_id", mcp.Required()),
		mcp.WithString("message_id", mcp.Required())),
		func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			tracker.seen(ctx)
			jid, err := r.RequireString("chat_id")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			id, err := r.RequireString("message_id")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			if err := d.Store.MarkHandled(jid, id); err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			_ = d.State.React("done", "done!", 4*time.Second)
			refreshQueue(d.State, d.Store)
			return mcp.NewToolResultText("ok"), nil
		})

	// ── resolve_chat ─────────────────────────────────────────────────────
	s.AddTool(mcp.NewTool("resolve_chat",
		mcp.WithDescription("Router decision for a chat: whether it's whitelisted/allowed, its mode/plugin/model, VIP status, and whether it's a WhatsApp group (never reply into a group without checking this)."),
		mcp.WithString("chat_id", mcp.Required(), mcp.Description("Chat JID"))),
		func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			tracker.seen(ctx)
			_ = d.State.React("switching", "next chat...", 4*time.Second)
			jid, err := r.RequireString("chat_id")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			if d.Router == nil {
				return mcp.NewToolResultError("router not available"), nil
			}
			dec := d.Router.Resolve(jid)
			return jsonResult(struct {
				ChatID  string `json:"chat_id"`
				Allowed bool   `json:"allowed"`
				Mode    string `json:"mode"`
				Plugin  string `json:"plugin,omitempty"`
				Model   string `json:"model,omitempty"`
				VIP     bool   `json:"vip"`
				IsGroup bool   `json:"is_group"`
			}{jid, dec.Allowed, dec.Mode, dec.Plugin, dec.Model, d.Router.IsVIP(jid), isGroupJID(jid)})
		})

	// ── get_outbox ───────────────────────────────────────────────────────
	s.AddTool(mcp.NewTool("get_outbox",
		mcp.WithDescription("Messages queued via send_message that the gateway has not sent yet."),
		mcp.WithNumber("limit", mcp.DefaultNumber(20))),
		func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			tracker.seen(ctx)
			items, err := d.Store.PendingOutbox(int(r.GetFloat("limit", 20)))
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return jsonResult(items)
		})

	// ── get_chat ─────────────────────────────────────────────────────────
	s.AddTool(mcp.NewTool("get_chat",
		mcp.WithDescription("A chat's record: name, mode, unread count, triage status, active flag, is_boss (read-only — true means this is the trusted owner; only settable via set_is_boss, boss-only, never by a caution/danger agent), origin (inbound_spoke = a real conversation happened, in either direction — not necessarily that THEY spoke last; group_discovered/synced_contact = never a real conversation), last_speaker (them/us) + last_model, and memory/context/rules. memory (particular facts) and context (general situation) are agent-writable via set_chat_memory/set_chat_context. rules is the EFFECTIVE, already-resolved value (particular → this chat's type → the global default → \"\" — the agent never sees the hierarchy, only the answer) — READ-ONLY here; set_chat_rules writes this chat's OWN particular tier directly (unconditional since T31, ct-2026-08-06-0244 — any dispatch level, no gate; see that tool's own description), or use the privileged REST path for the type/default tiers. Also read-only: confirmation_mode (none/discretion/always — governs how send_message behaves, see its own description) and confirmer — who a held draft's confirmation is directed to; only settable via set_confirmation_mode (boss-only) or the privileged REST path. description (a group's WhatsApp topic, or your own note about the chat) is settable via the privileged REST path. group_invite_link is READ-ONLY, populated by the gateway from WhatsApp itself — empty for a 1-1 chat or a group the gateway isn't admin on. claimed_by/claimed_until (both empty/0 if unclaimed or expired) show whether another agent currently holds this chat via claim_chat — check before you invest work drafting a reply. The agent must NOT always have the last word — if last_speaker is already \"us\", don't reply again."),
		mcp.WithString("chat_id", mcp.Required())),
		func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			tracker.seen(ctx)
			_ = d.State.React("switching", "next chat...", 4*time.Second)
			jid, err := r.RequireString("chat_id")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			c, ok, err := d.Store.GetChat(jid)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			if !ok {
				return mcp.NewToolResultError("chat not found: " + jid), nil
			}
			if c.Rules, err = d.Store.EffectiveRules(jid); err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return jsonResult(c)
		})

	// ── set_chat_status ──────────────────────────────────────────────────
	s.AddTool(mcp.NewTool("set_chat_status",
		mcp.WithDescription("Set a chat's triage status: whitelist, blacklist, new, ignored, or agent_exclusive:<id> (claims the chat for one agent)."),
		mcp.WithString("chat_id", mcp.Required()),
		mcp.WithString("status", mcp.Required(), mcp.Description("whitelist|blacklist|new|ignored|agent_exclusive:<id>"))),
		func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			tracker.seen(ctx)
			jid, err := r.RequireString("chat_id")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			status, err := r.RequireString("status")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			if !validChatStatus(status) {
				return mcp.NewToolResultError("status must be whitelist|blacklist|new|ignored|agent_exclusive:<id>, got " + status), nil
			}
			if err := d.Store.SetStatus(jid, status); err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			// M5 (ct-2026-07-22-1903): whitelist/blacklist/new/ignored are
			// explicit triage decisions about THIS chat's mode — freeze
			// against a later origin-default re-application. agent_exclusive
			// (M3/M4) is a DIFFERENT axis (who handles it, not how) and must
			// NOT trip this.
			if _, isAgentExclusive := store.AgentExclusiveID(status); !isAgentExclusive {
				if err := d.Store.MarkConfigManual(jid); err != nil {
					return mcp.NewToolResultError(err.Error()), nil
				}
			}
			return mcp.NewToolResultText("status set to " + status), nil
		})

	// ── set_chat_active ──────────────────────────────────────────────────
	s.AddTool(mcp.NewTool("set_chat_active",
		mcp.WithDescription("Set whether the agent is allowed to handle this chat."),
		mcp.WithString("chat_id", mcp.Required()),
		mcp.WithBoolean("active", mcp.Required())),
		func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			tracker.seen(ctx)
			jid, err := r.RequireString("chat_id")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			active, err := r.RequireBool("active")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			if err := d.Store.SetActive(jid, active); err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText("active set"), nil
		})

	// ── claim_chat ───────────────────────────────────────────────────────
	s.AddTool(mcp.NewTool("claim_chat",
		mcp.WithDescription("Claim a chat for a limited time so another connected agent skips it while you're working it — 'model' is the same identity you pass to send_message. Idempotent: re-claiming your own claim renews it. Fails with a clear error (who holds it, until when) if a DIFFERENT model holds an unexpired claim. A solo agent that never calls this is never affected — send_message only blocks on a claim held by someone else."),
		mcp.WithString("chat_id", mcp.Required()),
		mcp.WithString("model", mcp.Required(), mcp.Description("Same identity you pass to send_message")),
		mcp.WithNumber("ttl_sec", mcp.Description("Optional; defaults to a few minutes, capped at a hard ceiling"))),
		func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			tracker.seen(ctx)
			jid, err := r.RequireString("chat_id")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			model, err := r.RequireString("model")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			if _, ok, err := d.Store.GetChat(jid); err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			} else if !ok {
				return mcp.NewToolResultError("chat not found: " + jid), nil
			}
			ttl := claimTTLDefault
			if v := r.GetFloat("ttl_sec", 0); v > 0 {
				ttl = time.Duration(v * float64(time.Second))
			}
			if ttl > claimTTLCeiling {
				ttl = claimTTLCeiling
			}
			ok, err := d.Store.ClaimChat(jid, model, ttl)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			if !ok {
				c, _, err := d.Store.GetChat(jid)
				if err != nil {
					return mcp.NewToolResultError(err.Error()), nil
				}
				return mcp.NewToolResultError(fmt.Sprintf("chat claimed by %s until %s",
					c.ClaimedBy, time.Unix(c.ClaimedUntil, 0).UTC().Format(time.RFC3339))), nil
			}
			return mcp.NewToolResultText("claimed until " + time.Now().Add(ttl).UTC().Format(time.RFC3339)), nil
		})

	// ── release_chat ─────────────────────────────────────────────────────
	s.AddTool(mcp.NewTool("release_chat",
		mcp.WithDescription("Release your claim_chat lock early. No-op (still returns ok) if you don't currently hold it."),
		mcp.WithString("chat_id", mcp.Required()),
		mcp.WithString("model", mcp.Required(), mcp.Description("Same identity you passed to claim_chat"))),
		func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			tracker.seen(ctx)
			jid, err := r.RequireString("chat_id")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			model, err := r.RequireString("model")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			if err := d.Store.ReleaseChat(jid, model); err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText("released"), nil
		})

	// ── set_chat_memory ──────────────────────────────────────────────────
	s.AddTool(mcp.NewTool("set_chat_memory",
		mcp.WithDescription("Set a chat's memory: particular facts learned about this contact. Overwrites the whole field — read get_chat first if you want to append rather than replace."),
		mcp.WithString("chat_id", mcp.Required()),
		mcp.WithString("memory", mcp.Required())),
		func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			tracker.seen(ctx)
			jid, err := r.RequireString("chat_id")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			memory, err := r.RequireString("memory")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			if err := d.Store.SetChatMemory(jid, memory); err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText("memory set"), nil
		})

	// ── set_chat_context ─────────────────────────────────────────────────
	s.AddTool(mcp.NewTool("set_chat_context",
		mcp.WithDescription("Set a chat's context: the general/explanatory situation of this relationship. Overwrites the whole field — read get_chat first if you want to append rather than replace."),
		mcp.WithString("chat_id", mcp.Required()),
		mcp.WithString("context", mcp.Required())),
		func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			tracker.seen(ctx)
			jid, err := r.RequireString("chat_id")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			chatContext, err := r.RequireString("context")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			if err := d.Store.SetChatContext(jid, chatContext); err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText("context set"), nil
		})

	// ── get_media ────────────────────────────────────────────────────────
	s.AddTool(mcp.NewTool("get_media",
		mcp.WithDescription("Images/videos/stickers downloaded from a chat (file paths on disk, mime, size)."),
		mcp.WithString("chat_id", mcp.Required()),
		mcp.WithNumber("limit", mcp.DefaultNumber(20))),
		func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			tracker.seen(ctx)
			jid, err := r.RequireString("chat_id")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			items, err := d.Store.ListMedia(jid, int(r.GetFloat("limit", 20)))
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			// Deliberately a reduced view (summarizeMedia, media_tools.go) —
			// NOT the raw store.Media, which includes full_path. Leaking
			// that here would let the agent read the uncompressed original
			// directly and never pay get_media_full's usage cost (F4d audit).
			return jsonResult(summarizeMedia(items))
		})

	// ── get_chat_groups ──────────────────────────────────────────────────
	s.AddTool(mcp.NewTool("get_chat_groups",
		mcp.WithDescription("Which WhatsApp groups a number is known to participate in — from the gateway's own membership scrape (group_members), refreshed on connect/reconnect, not live per membership change; a brand-new group or member may not show up until the next one."),
		mcp.WithString("chat_id", mcp.Required(), mcp.Description("A contact JID (not a group)"))),
		func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			tracker.seen(ctx)
			jid, err := r.RequireString("chat_id")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			groups, err := d.Store.GroupsOf(jid)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return jsonResult(groups)
		})

	// ── get_pending ──────────────────────────────────────────────────────
	s.AddTool(mcp.NewTool("get_pending",
		mcp.WithDescription("Chats waiting for a reply — the contact has the last word (last_speaker=\"them\"). These are candidates, NOT a to-do list: you are not obligated to reply to all of them. Judge each by date and relevance; you must NOT always have the last word — replying to every single one is a mistake. When in doubt, escalate (escalate) or ask the owner instead of guessing. If you're one of several connected agents, consider claim_chat before working one so another agent skips it — claimed_by/claimed_until here (empty/0 if unclaimed or expired) show what's already spoken for."),
		mcp.WithNumber("limit", mcp.DefaultNumber(20))),
		func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			tracker.seen(ctx)
			pending, err := d.Store.PendingChats(int(r.GetFloat("limit", 20)), time.Now().Unix())
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return jsonResult(pending)
		})

	// ── get_drafts ───────────────────────────────────────────────────────
	// READ-ONLY for every level. Resolving one (approve_draft/discard_draft,
	// admin_tools.go) is boss-only, not "MCP-forbidden" — F4c added those as
	// real MCP tools, gated by bossOnlyTools rather than absent entirely.
	s.AddTool(mcp.NewTool("get_drafts",
		mcp.WithDescription("Pending drafts awaiting approval — read-only at every level. Resolving one (approve_draft/discard_draft) is boss-only."),
		mcp.WithNumber("limit", mcp.DefaultNumber(20))),
		func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			tracker.seen(ctx)
			drafts, err := d.Store.PendingDrafts(int(r.GetFloat("limit", 20)))
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return jsonResult(drafts)
		})

	// ── reset_dashboard_password ─────────────────────────────────────────
	// OWNER-ONLY. Owner scoping rides entirely on RequireBearerToken/
	// PIUMY_MCP_KEY: MCP has no per-caller identity of its own, so the
	// bearer token IS "you are the owner" here — refuses outright when no
	// token is configured.
	s.AddTool(mcp.NewTool("reset_dashboard_password",
		mcp.WithDescription("OWNER-ONLY. Resets the dashboard's login password and returns the NEW plaintext password once — the only way to recover a lost password, since the stored bcrypt hash cannot be reversed. Pass new_password to set a specific password, or omit it to get a freshly generated random one. Refuses if the MCP server has no bearer auth configured (PIUMY_MCP_KEY)."),
		mcp.WithString("new_password", mcp.Description("Optional specific password to set (must not be empty). Omit to generate a random 24-character one."))),
		func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			tracker.seen(ctx)
			if !d.MCPAuthConfigured {
				return mcp.NewToolResultError("refused: PIUMY_MCP_KEY is not set -- an open MCP server has no owner-only boundary to scope a password reset to. Configure PIUMY_MCP_KEY, then retry."), nil
			}
			newPassword := r.GetString("new_password", "")
			if newPassword == "" {
				generated, err := generateRandomPassword()
				if err != nil {
					return mcp.NewToolResultError(err.Error()), nil
				}
				newPassword = generated
			} else if len(newPassword) == 0 {
				return mcp.NewToolResultError("new_password must not be empty"), nil
			}
			hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			if err := d.Store.KVSet(store.SettingDashPassHash, string(hash)); err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			// ct-2026-07-19-1616 (S1d): an emergency reset must end every
			// existing browser session too, not just the one MCP call —
			// same invariant the dashboard's own POST /api/admin/password
			// enforces (restapi/auth.go).
			if err := d.Store.RotateDashSessionSecret(); err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return jsonResult(struct {
				Password string `json:"password"`
				Note     string `json:"note"`
			}{
				Password: newPassword,
				Note:     "Shown once -- the stored hash cannot be reversed. Save this now; a future loss requires calling this tool again to reset it.",
			})
		})

	addGateTools(s, d, gate, tracker)
	addAdminTools(s, d, gate, tracker)
	addGroupTools(s, d, tracker)
	addMediaTools(s, d, tracker)
	addAgentTools(s, d)
	// send_to_boss (T39, ct-2026-08-08-1619): DELIBERATELY not passed
	// through levelGateMiddleware's gated-tool maps (bossOnlyTools/
	// enumerationTools/chatScopedArg in levelgate.go) — same "no chat
	// concept, stays open regardless" exemption get_status/get_decision_policy
	// already have, not a new mechanism. Safe here because the tool itself
	// has no caller-supplied destination and resolves its caller's identity
	// from the connection, never a parameter — see send_to_boss.go's own doc.
	addSendToBossTool(s, d, tracker)

	return s
}

// generateRandomPassword returns a random 24-hex-char password — no
// dashboard package needed for this, it's a 1-line helper, not a nodo (the
// LAN web dashboard isn't part of F4's plan at all).
func generateRandomPassword() (string, error) {
	return randomHex(12)
}

// randomHex returns n random bytes hex-encoded — shared by
// generateRandomPassword and the gate's unlock tokens (gate.go).
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// validChatStatus reports whether status is one of the fixed triage values
// or the agent_exclusive:<id> form (store.AgentExclusiveID — single source
// of truth for that format, shared with capipush's dispatch routing, M4).
func validChatStatus(status string) bool {
	switch status {
	case "whitelist", "blacklist", "new", "ignored":
		return true
	}
	_, ok := store.AgentExclusiveID(status)
	return ok
}

// isGroupJID reports whether jid is a WhatsApp group chat, per open-wa's
// JID suffix convention (@g.us for groups vs @c.us for direct contacts).
func isGroupJID(jid string) bool {
	return strings.HasSuffix(jid, "@g.us")
}
