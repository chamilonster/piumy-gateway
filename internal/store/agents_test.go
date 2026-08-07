package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestUpsertAndGetAgent(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "piumy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	a := Agent{
		AgentID:           "term-secondary",
		Endpoint:          "http://192.168.1.10:8787",
		AntennaTerminalID: "ant-guid-1",
		Pinpass:           "abc123",
		Role:              "secondary",
	}
	if err := s.UpsertAgent(a); err != nil {
		t.Fatal(err)
	}

	got, ok, err := s.GetAgent("term-secondary")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("GetAgent: not found")
	}
	if got != a {
		t.Errorf("GetAgent = %+v, want %+v", got, a)
	}
}

func TestUpsertAgentOverwrites(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "piumy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	a := Agent{AgentID: "term-x", Endpoint: "http://old:8787", AntennaTerminalID: "ant-old", Pinpass: "p1", Role: "secondary"}
	if err := s.UpsertAgent(a); err != nil {
		t.Fatal(err)
	}
	a2 := Agent{AgentID: "term-x", Endpoint: "http://new:8787", AntennaTerminalID: "ant-new", Pinpass: "p2", Role: "secondary"}
	if err := s.UpsertAgent(a2); err != nil {
		t.Fatal(err)
	}

	got, _, _ := s.GetAgent("term-x")
	if got.Endpoint != "http://new:8787" {
		t.Errorf("after upsert Endpoint = %q, want http://new:8787", got.Endpoint)
	}
}

func TestGetAgentNotFound(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "piumy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	_, ok, err := s.GetAgent("nonexistent")
	if err != nil || ok {
		t.Errorf("GetAgent(nonexistent) = (_, %v, %v), want (_, false, nil)", ok, err)
	}
}

func TestListAgents(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "piumy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for _, a := range []Agent{
		{AgentID: "term-a", Endpoint: "http://a:8787", AntennaTerminalID: "ant-a", Pinpass: "pa", Role: "secondary"},
		{AgentID: "term-b", Endpoint: "http://b:8787", AntennaTerminalID: "ant-b", Pinpass: "pb", Role: "secondary"},
	} {
		if err := s.UpsertAgent(a); err != nil {
			t.Fatal(err)
		}
	}

	list, err := s.ListAgents()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("ListAgents = %d, want 2", len(list))
	}
	if list[0].AgentID != "term-a" || list[1].AgentID != "term-b" {
		t.Errorf("ListAgents order = [%s, %s], want [term-a, term-b]", list[0].AgentID, list[1].AgentID)
	}
}

// TestUpsertAgentWithName (M1, ct-2026-07-22-1301): name round-trips
// through UpsertAgent/GetAgent same as every other field.
func TestUpsertAgentWithName(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "piumy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	a := Agent{AgentID: "term-named", Name: "Sonnet", Endpoint: "http://n:8787", AntennaTerminalID: "ant-n", Pinpass: "pn", Role: "secondary"}
	if err := s.UpsertAgent(a); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetAgent("term-named")
	if err != nil || !ok {
		t.Fatalf("GetAgent = (_, %v, %v)", ok, err)
	}
	if got.Name != "Sonnet" {
		t.Errorf("Name = %q, want Sonnet", got.Name)
	}

	// Overwrite with a new name — same "upsert replaces every field" contract
	// TestUpsertAgentOverwrites already checks for Endpoint.
	a.Name = "Sonnet renombrado"
	if err := s.UpsertAgent(a); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.GetAgent("term-named")
	if got.Name != "Sonnet renombrado" {
		t.Errorf("after upsert Name = %q, want %q", got.Name, "Sonnet renombrado")
	}
}

func TestDeleteAgent(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "piumy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	a := Agent{AgentID: "term-del", Endpoint: "http://del:8787", AntennaTerminalID: "ant-del", Pinpass: "pd", Role: "secondary"}
	if err := s.UpsertAgent(a); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAgent("term-del"); err != nil {
		t.Fatal(err)
	}
	_, ok, err := s.GetAgent("term-del")
	if err != nil || ok {
		t.Errorf("after Delete: GetAgent = (_, %v, %v), want (_, false, nil)", ok, err)
	}
}

// TestPrincipalAgentSynthesizesFromKV (ct-2026-07-29, agentes paso 3):
// PrincipalAgent reads the same 4 KV keys restapi.handleAgents used to read
// inline — round-trips through SetPrincipalAgent.
func TestPrincipalAgentSynthesizesFromKV(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "piumy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, ok, err := s.PrincipalAgent(""); err != nil || ok {
		t.Errorf("PrincipalAgent(\"\") = (_, %v, %v), want (_, false, nil) — no principal configured", ok, err)
	}

	if err := s.SetPrincipalAgent("Boss", "http://127.0.0.1:8787", "ant-term", "s3cr3t=="); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.PrincipalAgent("principal-term")
	if err != nil || !ok {
		t.Fatalf("PrincipalAgent: ok=%v err=%v", ok, err)
	}
	want := Agent{
		AgentID: "principal-term", Name: "Boss", Endpoint: "http://127.0.0.1:8787",
		AntennaTerminalID: "ant-term", Pinpass: "s3cr3t==", Role: "principal",
	}
	if got != want {
		t.Errorf("PrincipalAgent = %+v, want %+v", got, want)
	}
}

// TestSetPrincipalAgentAllowsPrivateNetworkEndpoint is the regression test
// for ct-2026-07-29 (boss caught it same day: the first cut of this gate
// required literal 127.0.0.1, which breaks the product's actual target
// deploy — a Raspberry Pi running the gateway with the principal agent on a
// DIFFERENT machine of the same LAN, "y ahi no será local la antenita").
// A LAN IP (RFC1918), a bare "localhost", and an mDNS "*.local" name must
// all be accepted — none of them is a public address.
func TestSetPrincipalAgentAllowsPrivateNetworkEndpoint(t *testing.T) {
	for _, endpoint := range []string{
		"http://192.168.1.10:8787",  // Raspberry Pi on the LAN — the actual regression
		"http://10.0.0.5:8787",      // RFC1918 10/8
		"http://172.16.4.4:8787",    // RFC1918 172.16/12
		"http://localhost:8787",     // not a literal IP at all
		"http://raspberrypi.local:8787",
		"http://[::1]:8787",         // IPv6 loopback
		"http://[fd00::1]:8787",     // IPv6 ULA (RFC4193)
	} {
		s, err := Open(filepath.Join(t.TempDir(), "piumy.db"))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SetPrincipalAgent("Boss", endpoint, "ant-term", "s3cr3t=="); err != nil {
			t.Errorf("SetPrincipalAgent(%q): want no error, got %v", endpoint, err)
		}
		s.Close()
	}
}

// TestSetPrincipalAgentRejectsPublicEndpoint is the regression test for
// ct-2026-07-29 (agentes paso 3, closing a real gap: before this, nothing
// in the backend enforced "the principal's antenna is never public" — only
// the dashboard's readonly input did, cosmetically. CLAUDE.md: "el gate
// duro va en el código, no en skills ni prompts"). A public IP or a public
// domain must be rejected, not silently accepted — errors.Is against
// ErrPrincipalEndpointPublic so callers can map it to 400.
func TestSetPrincipalAgentRejectsPublicEndpoint(t *testing.T) {
	for _, endpoint := range []string{
		"http://8.8.8.8:8787",           // a public IP
		"http://antenita.example.com:8787", // a public domain
	} {
		s, err := Open(filepath.Join(t.TempDir(), "piumy.db"))
		if err != nil {
			t.Fatal(err)
		}
		err = s.SetPrincipalAgent("Boss", endpoint, "ant-term", "s3cr3t==")
		if !errors.Is(err, ErrPrincipalEndpointPublic) {
			t.Errorf("SetPrincipalAgent(%q): err = %v, want ErrPrincipalEndpointPublic", endpoint, err)
		}
		got, _, _ := s.PrincipalAgent("principal-term")
		if got.Endpoint != "" || got.Name != "" {
			t.Errorf("a rejected SetPrincipalAgent call must not have persisted anything, got %+v", got)
		}
		s.Close()
	}
}

// TestUnassignAllChatsForAgent is the regression test for ct-2026-07-29
// (boss: "ningún chat queda apuntando a un agente que ya no existe"): every
// chat assigned to agentID reverts to "new"; a chat assigned to a DIFFERENT
// agent, or never assigned at all, must be untouched.
func TestUnassignAllChatsForAgent(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "piumy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for _, jid := range []string{"1@c.us", "2@c.us", "3@c.us", "4@c.us"} {
		if err := s.TouchChat(jid, "", 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetStatus("1@c.us", AgentExclusiveStatus("term-a")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStatus("2@c.us", AgentExclusiveStatus("term-a")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStatus("3@c.us", AgentExclusiveStatus("term-b")); err != nil {
		t.Fatal(err)
	}
	// 4@c.us stays unassigned ("new", TouchChat's default).

	n, err := s.UnassignAllChatsForAgent("term-a")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("UnassignAllChatsForAgent returned %d, want 2", n)
	}

	c1, _, _ := s.GetChat("1@c.us")
	c2, _, _ := s.GetChat("2@c.us")
	c3, _, _ := s.GetChat("3@c.us")
	c4, _, _ := s.GetChat("4@c.us")
	if c1.Status != "new" || c2.Status != "new" {
		t.Errorf("term-a's chats: 1=%q 2=%q, want both new", c1.Status, c2.Status)
	}
	if c3.Status != "agent_exclusive:term-b" {
		t.Errorf("term-b's chat = %q, want untouched", c3.Status)
	}
	if c4.Status != "new" {
		t.Errorf("never-assigned chat = %q, want new (untouched)", c4.Status)
	}
}

// TestAgentExclusiveIDAndStatus (M3/M4, ct-2026-07-22-1301): the one place
// the "agent_exclusive:<id>" chats.status form is built/parsed — shared by
// capipush's dispatch routing (M4) and the assign/unassign REST+MCP paths.
func TestAgentExclusiveIDAndStatus(t *testing.T) {
	if got := AgentExclusiveStatus("term-x"); got != "agent_exclusive:term-x" {
		t.Errorf("AgentExclusiveStatus = %q, want agent_exclusive:term-x", got)
	}
	cases := []struct {
		status string
		wantID string
		wantOK bool
	}{
		{"agent_exclusive:term-x", "term-x", true},
		{"agent_exclusive:", "", false}, // malformed: no id — same rule validChatStatus applies
		{"whitelist", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		id, ok := AgentExclusiveID(c.status)
		if id != c.wantID || ok != c.wantOK {
			t.Errorf("AgentExclusiveID(%q) = (%q, %v), want (%q, %v)", c.status, id, ok, c.wantID, c.wantOK)
		}
	}
}

// TestChatsForAgent (M3, ct-2026-07-22-1301): only chats explicitly
// assigned to agentID come back — everything else (unassigned, assigned to
// a DIFFERENT agent) is excluded.
func TestChatsForAgent(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "piumy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().Unix()
	for _, jid := range []string{"1@s.whatsapp.net", "2@s.whatsapp.net", "3@s.whatsapp.net"} {
		if err := s.TouchChat(jid, "", now); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetStatus("1@s.whatsapp.net", AgentExclusiveStatus("term-a")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStatus("2@s.whatsapp.net", AgentExclusiveStatus("term-b")); err != nil {
		t.Fatal(err)
	}
	// 3@s.whatsapp.net stays unassigned (default status from TouchChat).

	got, err := s.ChatsForAgent("term-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].JID != "1@s.whatsapp.net" {
		t.Errorf("ChatsForAgent(term-a) = %+v, want exactly [1@s.whatsapp.net]", got)
	}
}
