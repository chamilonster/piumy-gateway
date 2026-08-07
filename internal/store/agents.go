package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

type Agent struct {
	AgentID  string
	Name     string // display name (ct-2026-07-22-1301, M1) — optional, "" until set
	Endpoint string
	// AntennaTerminalID is CleverCoder's own handshake id, copied verbatim
	// from the antenna's clipboard credentials — a live GUID, OR (T32,
	// ct-2026-08-06-1109 — protocol §2, ct-2026-08-06-0221) a STABLE chat_id
	// CleverCoder computes: capi-<proyecto>-<agente>-<hash> with party
	// (absolute identity, "the mineral is the mineral always"), but
	// capi-<proyecto>-<proyecto>-<hash>-<N> WITHOUT party — that second form
	// names a POSITION (the Nth terminal open in that project), not a
	// specific one. A position can be valid and simply empty right now
	// (nothing open there yet) — that's exactly what handshake's
	// position_empty means, a normal transient state, not a misconfigured
	// credential.
	AntennaTerminalID string
	Pinpass           string
	Role              string // "principal" | "secondary"
}

func (s *Store) UpsertAgent(a Agent) error {
	_, err := s.db.Exec(`
		INSERT INTO agents(agent_id, name, endpoint, antenna_terminal_id, pinpass, role)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(agent_id) DO UPDATE SET
			name=excluded.name,
			endpoint=excluded.endpoint,
			antenna_terminal_id=excluded.antenna_terminal_id,
			pinpass=excluded.pinpass,
			role=excluded.role`,
		a.AgentID, a.Name, a.Endpoint, a.AntennaTerminalID, a.Pinpass, a.Role)
	return err
}

func (s *Store) GetAgent(agentID string) (Agent, bool, error) {
	var a Agent
	err := s.db.QueryRow(
		`SELECT agent_id, name, endpoint, antenna_terminal_id, pinpass, role FROM agents WHERE agent_id=?`,
		agentID).Scan(&a.AgentID, &a.Name, &a.Endpoint, &a.AntennaTerminalID, &a.Pinpass, &a.Role)
	if err == sql.ErrNoRows {
		return Agent{}, false, nil
	}
	return a, err == nil, err
}

func (s *Store) ListAgents() ([]Agent, error) {
	rows, err := s.db.Query(
		`SELECT agent_id, name, endpoint, antenna_terminal_id, pinpass, role FROM agents ORDER BY agent_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		var a Agent
		if err := rows.Scan(&a.AgentID, &a.Name, &a.Endpoint, &a.AntennaTerminalID, &a.Pinpass, &a.Role); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) DeleteAgent(agentID string) error {
	_, err := s.db.Exec(`DELETE FROM agents WHERE agent_id=?`, agentID)
	return err
}

// PrincipalAgent synthesizes the principal's Agent-shaped info from KV — it
// never has a real `agents` row (register_agent/set_agent_capi reject the
// principal's own id: the principal's identity/injector is wired at
// startup, not self-registered like a secondary). Shared by GET /api/agents
// (restapi) and the list_agents/set_capi_connector MCP tools (ct-2026-07-29,
// agentes paso 3) so "how do I read the principal" lives in exactly one
// place — before this, restapi.handleAgents read the 4 KV keys inline and
// MCP couldn't see the principal at all. Pinpass comes back in clear, same
// convention GetAgent/ListAgents already use for secondaries — redacting to
// a bool is the CALLER's job at the output boundary, not this layer's.
// ok=false when principalID is empty (no principal configured), same
// convention as GetAgent's own ok.
func (s *Store) PrincipalAgent(principalID string) (a Agent, ok bool, err error) {
	if principalID == "" {
		return Agent{}, false, nil
	}
	endpoint, err := s.KVGet(SettingCAPIEndpoint)
	if err != nil {
		return Agent{}, false, err
	}
	terminalID, err := s.KVGet(SettingCAPITerminalID)
	if err != nil {
		return Agent{}, false, err
	}
	pinpass, err := s.KVGet(SettingCAPIPinpass)
	if err != nil {
		return Agent{}, false, err
	}
	name, err := s.KVGet(SettingPrincipalName)
	if err != nil {
		return Agent{}, false, err
	}
	return Agent{
		AgentID: principalID, Name: name, Endpoint: endpoint,
		AntennaTerminalID: terminalID, Pinpass: pinpass, Role: "principal",
	}, true, nil
}

// isAllowedPrincipalEndpoint is the ONE place that decides "is this address
// allowed for the principal's antenna" (ct-2026-07-29, agentes paso 3 —
// corrected same day: the first cut required literal 127.0.0.1, which broke
// the product's actual target deploy — a Raspberry Pi running the gateway
// with the principal agent on a different machine of the same LAN, boss
// verbatim: "este proyecto está hecho para correr en una raspberry py...
// y ahi no será local la antenita". The real invariant is "never a public
// address", not "always this machine"). Allowed: loopback, RFC1918/RFC4193
// private ranges, link-local, and the "localhost"/"*.local" names mDNS
// discovery on a Pi uses. Rejected: any public IP or public domain — a
// tunnel/public-endpoint case is coming (boss: "el mcp estará dentro de un
// tunel, despues vemos eso de otra manera") but is NOT built yet (YAGNI —
// CLAUDE.md); when that requirement lands, it's a change to THIS function,
// not a hunt through call sites.
func isAllowedPrincipalEndpoint(endpoint string) (allowed bool, host string, err error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false, "", err
	}
	host = u.Hostname()
	if host == "" {
		return false, "", fmt.Errorf("no host in %q", endpoint)
	}
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".local") {
		return true, host, nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false, host, nil // an unresolved name — treated as a public domain
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast(), host, nil
}

// ErrPrincipalEndpointPublic is SetPrincipalAgent's validation failure — a
// sentinel (errors.Is-checkable) so callers can tell "bad input, 400" from
// a genuine store failure, "something broke, 500" — same distinction
// restapi's other validated writes already make, just via a typed error
// instead of a pre-check, so the RULE itself still lives in exactly one
// place (SetPrincipalAgent/isAllowedPrincipalEndpoint), never duplicated at
// the caller.
var ErrPrincipalEndpointPublic = errors.New("principal endpoint must be loopback or a private-network address — a public IP or domain isn't allowed yet")

// SetPrincipalAgent persists the principal's cAPI config + display name
// together — the single write path POST /api/admin/agent-update (principal
// branch) and the set_capi_connector MCP tool share (ct-2026-07-29, agentes
// paso 3), so "how do I update the principal" can't drift between REST and
// MCP. Rejects a disallowed endpoint outright (ErrPrincipalEndpointPublic,
// wrapping the specific reason) instead of silently overriding it — an
// honest error, not a value the caller didn't ask for.
func (s *Store) SetPrincipalAgent(name, endpoint, terminalID, pinpass string) error {
	allowed, host, err := isAllowedPrincipalEndpoint(endpoint)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPrincipalEndpointPublic, err)
	}
	if !allowed {
		return fmt.Errorf("%w: %q resolves to a public address", ErrPrincipalEndpointPublic, host)
	}
	if err := s.SetCAPIConnector(endpoint, terminalID, pinpass); err != nil {
		return err
	}
	return s.KVSet(SettingPrincipalName, name)
}

// UnassignAllChatsForAgent reverts every chat exclusively assigned to
// agentID back to "new" — AgentExclusiveStatus's inverse, in bulk. Called
// when an agent is deleted so no chat is left pointing at an identity that
// no longer exists (ct-2026-07-29, boss: "ningún chat queda apuntando a un
// agente que ya no existe" — a dangling agent_exclusive silently falls back
// to the router/principal today, capipush's own robustness guard, but the
// boss never finds out his messages changed destination; explicit cleanup
// beats an accident that happens to be safe). Returns how many chats were
// actually reverted, for an honest "N chats unassigned" response.
func (s *Store) UnassignAllChatsForAgent(agentID string) (int64, error) {
	res, err := s.db.Exec(`UPDATE chats SET status = 'new' WHERE status = ?`, AgentExclusiveStatus(agentID))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// agentExclusivePrefix is chats.status' "claimed for one agent" form —
// already validated by mcpserver's validChatStatus and written via
// SetStatus (chat.go); AgentExclusiveID/AgentExclusiveStatus are the one
// place that format is built/parsed, shared by capipush's dispatch routing
// (M4) and the assign/unassign REST+MCP paths (M3/M4, ct-2026-07-22-1301).
const agentExclusivePrefix = "agent_exclusive:"

// AgentExclusiveStatus builds the chats.status value that exclusively
// routes a chat to agentID.
func AgentExclusiveStatus(agentID string) string {
	return agentExclusivePrefix + agentID
}

// AgentExclusiveID extracts the agent id from a chat's status if it's in
// the agent_exclusive:<id> form; ok is false otherwise (including the
// malformed "agent_exclusive:" with no id, same rule validChatStatus applies).
func AgentExclusiveID(status string) (id string, ok bool) {
	if !strings.HasPrefix(status, agentExclusivePrefix) {
		return "", false
	}
	id = status[len(agentExclusivePrefix):]
	return id, id != ""
}

// ChatsForAgent returns chats manually assigned to agentID (M3's "números
// asignados" panel per agent) — chats.status == agent_exclusive:<agentID>,
// most recently active first. Same columns/scan as ListChats/GetChat.
func (s *Store) ChatsForAgent(agentID string) ([]Chat, error) {
	rows, err := s.db.Query(`SELECT `+chatColumns+` FROM chats WHERE status = ? ORDER BY last_ts DESC`,
		AgentExclusiveStatus(agentID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Chat
	for rows.Next() {
		c, err := scanChat(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Recolectar-y-actuar (ct-2026-07-24-0127): rows cerradas antes de
	// enrichChat, que hace QueryRow — necesario con SetMaxOpenConns(1)
	// para evitar deadlock (la conn de rows no se libera hasta Close).
	for i := range out {
		if err := s.enrichChat(&out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}
