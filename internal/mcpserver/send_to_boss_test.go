package mcpserver

import (
	"strings"
	"testing"

	"piumy-gateway/internal/store"
)

// TestSendToBossRegisteredAgentQueuesWithSignedOriginAndFanOut is send_to_boss's
// main DoD case (T39, ct-2026-08-08-1619): a registered agent sends and the
// message ends up queued, once per is_boss chat, prefixed with the agent's
// name, with the origin recorded.
//
// AntennaTerminalID is DELIBERATELY set to a DIFFERENT value than AgentID —
// this is the regression for the identity-resolution bug Citrino's contract
// warned about: matching the connecting terminal against agents.agent_id
// (fixed at registration) instead of agents.antenna_terminal_id (the field
// set_agent_capi actually updates) would refuse a legitimately registered
// agent whose antenna terminal_id changed since registering. The connecting
// context here presents "term-live" — the CURRENT antenna_terminal_id, not
// the original agent_id "agent-A".
func TestSendToBossRegisteredAgentQueuesWithSignedOriginAndFanOut(t *testing.T) {
	st, mcpSrv, ctx, _ := newTestServer(t)

	if err := st.UpsertAgent(store.Agent{
		AgentID: "agent-A", Name: "Agente Uno",
		AntennaTerminalID: "term-live", Role: "secondary",
	}); err != nil {
		t.Fatal(err)
	}
	// Two boss chats — send_to_boss has no destination argument, so BOTH
	// must receive the message (store.BossJIDs' own fan-out, MANUAL.md).
	if err := st.SetIsBoss("555000010@s.whatsapp.net", true); err != nil {
		t.Fatal(err)
	}
	if err := st.SetIsBoss("555000011@s.whatsapp.net", true); err != nil {
		t.Fatal(err)
	}

	termCtx := withTerminalID(ctx, "term-live")
	out := callTool(t, termCtx, mcpSrv, "send_to_boss", map[string]any{"text": "ya terminé la tarea"})
	if strings.Contains(out, "isError\":true") {
		t.Fatalf("send_to_boss for a registered agent (matched via AntennaTerminalID) failed: %s", out)
	}

	pending, err := st.PendingOutbox(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("got %d queued items, want 2 (one per is_boss chat): %+v", len(pending), pending)
	}
	byJID := map[string]store.Outbox{}
	for _, o := range pending {
		byJID[o.ToJID] = o
	}
	for _, jid := range []string{"555000010@s.whatsapp.net", "555000011@s.whatsapp.net"} {
		o, ok := byJID[jid]
		if !ok {
			t.Fatalf("no queued item for boss chat %s — got %+v", jid, pending)
		}
		if o.OriginTerminalID != "term-live" {
			t.Errorf("%s: origin_terminal_id = %q, want the connecting terminal_id %q", jid, o.OriginTerminalID, "term-live")
		}
		if o.Text != "[Agente Uno] ya terminé la tarea" {
			t.Errorf("%s: text = %q, want signed with the agent's registered Name", jid, o.Text)
		}
	}
}

// TestSendToBossFallsBackToTerminalIDWhenNameEmpty covers the signature's
// fallback: no registered Name -> the raw terminal_id, per the contract's
// own text ("el ID de capi... con el id como respaldo").
func TestSendToBossFallsBackToTerminalIDWhenNameEmpty(t *testing.T) {
	st, mcpSrv, ctx, _ := newTestServer(t)
	if err := st.UpsertAgent(store.Agent{
		AgentID: "term-noname", AntennaTerminalID: "term-noname", Role: "secondary",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetIsBoss("555000012@s.whatsapp.net", true); err != nil {
		t.Fatal(err)
	}

	termCtx := withTerminalID(ctx, "term-noname")
	out := callTool(t, termCtx, mcpSrv, "send_to_boss", map[string]any{"text": "hola"})
	if strings.Contains(out, "isError\":true") {
		t.Fatalf("send_to_boss failed: %s", out)
	}
	pending, err := st.PendingOutbox(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Text != "[term-noname] hola" {
		t.Fatalf("got %+v, want one item signed with the raw terminal_id (no Name registered)", pending)
	}
}

// TestSendToBossUnregisteredTerminalRefusesAndQueuesNothing is the DoD case
// Citrino called out as the one that matters most: an unregistered
// terminal_id gets an explicit error AND enqueues nothing at all — this is
// literally what CleverCoder flagged in writing (an unrecognized terminal_id
// used to pass through silently).
func TestSendToBossUnregisteredTerminalRefusesAndQueuesNothing(t *testing.T) {
	st, mcpSrv, ctx, _ := newTestServer(t)
	if err := st.SetIsBoss("555000013@s.whatsapp.net", true); err != nil {
		t.Fatal(err)
	}

	termCtx := withTerminalID(ctx, "term-never-registered")
	out := callTool(t, termCtx, mcpSrv, "send_to_boss", map[string]any{"text": "no debería salir"})
	if !strings.Contains(out, "isError\":true") {
		t.Fatalf("send_to_boss for an unregistered terminal_id = %s, want an explicit error", out)
	}
	if !strings.Contains(out, "term-never-registered") {
		t.Errorf("error does not name the unrecognized terminal_id: %s", out)
	}

	pending, err := st.PendingOutbox(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("got %d queued items for an unregistered terminal_id, want 0: %+v", len(pending), pending)
	}
}

// TestSendToBossNoTerminalIDRefusesAndQueuesNothing covers the other half of
// the identity requirement: no X-Piumy-Terminal-Id at all (a bare/manual MCP
// call) is refused exactly like an unregistered one, nothing queued.
func TestSendToBossNoTerminalIDRefusesAndQueuesNothing(t *testing.T) {
	st, mcpSrv, ctx, _ := newTestServer(t)
	if err := st.SetIsBoss("555000014@s.whatsapp.net", true); err != nil {
		t.Fatal(err)
	}

	out := callTool(t, ctx, mcpSrv, "send_to_boss", map[string]any{"text": "sin terminal_id"})
	if !strings.Contains(out, "isError\":true") {
		t.Fatalf("send_to_boss with no terminal_id in context = %s, want an explicit error", out)
	}

	pending, err := st.PendingOutbox(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("got %d queued items with no terminal_id, want 0: %+v", len(pending), pending)
	}
}

// TestSendToBossNoDispatchRequired confirms the whole point of the feature:
// a terminal with NO active dispatch at all — the default-DENY every other
// gated tool enforces — can still reach send_to_boss, as long as it's a
// registered agent. Never calls gate.RegisterDispatch/get_instructions.
func TestSendToBossNoDispatchRequired(t *testing.T) {
	st, mcpSrv, ctx, _ := newTestServer(t)
	if err := st.UpsertAgent(store.Agent{
		AgentID: "agent-nodispatch", AntennaTerminalID: "agent-nodispatch", Role: "secondary",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetIsBoss("555000015@s.whatsapp.net", true); err != nil {
		t.Fatal(err)
	}

	termCtx := withTerminalID(ctx, "agent-nodispatch")
	out := callTool(t, termCtx, mcpSrv, "send_to_boss", map[string]any{"text": "avisando sin dispatch activo"})
	if strings.Contains(out, "isError\":true") {
		t.Fatalf("send_to_boss with no active dispatch = %s, want it to succeed (the entire point of the tool)", out)
	}
}

// TestSendToBossNoBossChatConfiguredRefuses covers the edge case where
// BossJIDs() is empty — reporting "queued" while nothing actually went out
// would be a silent lie, so this must refuse explicitly instead.
func TestSendToBossNoBossChatConfiguredRefuses(t *testing.T) {
	st, mcpSrv, ctx, _ := newTestServer(t)
	if err := st.UpsertAgent(store.Agent{
		AgentID: "agent-noboss", AntennaTerminalID: "agent-noboss", Role: "secondary",
	}); err != nil {
		t.Fatal(err)
	}

	termCtx := withTerminalID(ctx, "agent-noboss")
	out := callTool(t, termCtx, mcpSrv, "send_to_boss", map[string]any{"text": "no hay a quién"})
	if !strings.Contains(out, "isError\":true") {
		t.Fatalf("send_to_boss with no is_boss chat configured = %s, want an explicit error, not a silent no-op", out)
	}
	pending, err := st.PendingOutbox(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("got %d queued items with no boss chat configured, want 0: %+v", len(pending), pending)
	}
}
