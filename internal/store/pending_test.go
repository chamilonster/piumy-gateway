package store

import "testing"

// TestMarkHandledBeforeBounded verifies ct-2026-07-13-2105: MarkHandledBefore
// marks messages with ts<=tsResp handled but leaves later messages untouched.
func TestMarkHandledBeforeBounded(t *testing.T) {
	s := openTestStore(t)

	if err := s.SetMode("chat@c.us", "dedicated"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetActive("chat@c.us", true); err != nil {
		t.Fatal(err)
	}
	msgs := []Message{
		{ChatJID: "chat@c.us", ID: "m1", FromMe: false, Text: "one", TS: 100},
		{ChatJID: "chat@c.us", ID: "m2", FromMe: false, Text: "two", TS: 200},
		{ChatJID: "chat@c.us", ID: "m3", FromMe: false, Text: "three", TS: 300},
	}
	for _, m := range msgs {
		if err := s.AddMessage(m); err != nil {
			t.Fatal(err)
		}
	}

	// mark handled up to ts=200 — m1 and m2 handled, m3 must stay pending.
	if err := s.MarkHandledBefore("chat@c.us", 200); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingDedicated(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != "m3" {
		t.Fatalf("got %+v, want only m3 still pending", pending)
	}
}

// TestMarkPendingBeforeReversesMarkHandledBefore is T15 (ct-2026-08-05-123241):
// a rejected draft reopens exactly the messages its own creation closed
// (same ts bound), and nothing that arrived later or was handled through a
// different path.
func TestMarkPendingBeforeReversesMarkHandledBefore(t *testing.T) {
	s := openTestStore(t)
	if err := s.SetMode("chat@c.us", "dedicated"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetActive("chat@c.us", true); err != nil {
		t.Fatal(err)
	}
	msgs := []Message{
		{ChatJID: "chat@c.us", ID: "m1", FromMe: false, Text: "one", TS: 100},
		{ChatJID: "chat@c.us", ID: "m2", FromMe: false, Text: "two", TS: 200},
		{ChatJID: "chat@c.us", ID: "m3", FromMe: false, Text: "three", TS: 300},
	}
	for _, m := range msgs {
		if err := s.AddMessage(m); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.MarkHandledBefore("chat@c.us", 200); err != nil {
		t.Fatal(err)
	}

	if err := s.MarkPendingBefore("chat@c.us", 200); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingDedicated(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 3 {
		t.Fatalf("PendingDedicated after MarkPendingBefore = %+v, want all 3 messages pending again", pending)
	}
}

func TestPendingDedicatedOnlyUnhandled(t *testing.T) {
	s := openTestStore(t)

	if err := s.SetMode("dedicated@c.us", "dedicated"); err != nil {
		t.Fatalf("SetMode dedicated: %v", err)
	}
	if err := s.SetActive("dedicated@c.us", true); err != nil {
		t.Fatalf("SetActive dedicated: %v", err)
	}
	if err := s.AddMessage(Message{ChatJID: "dedicated@c.us", ID: "m1", FromMe: false, Text: "hola", TS: 100}); err != nil {
		t.Fatalf("AddMessage dedicated: %v", err)
	}

	if err := s.MarkHandled("dedicated@c.us", "m1"); err != nil {
		t.Fatalf("MarkHandled: %v", err)
	}
	pending, err := s.PendingDedicated(10)
	if err != nil {
		t.Fatalf("PendingDedicated after handled: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("got %d pending after MarkHandled, want 0", len(pending))
	}
}

// TestPendingDedicatedIncludesAutoMode is T5 (ct-2026-08-05-0311, boss
// verbatim: "todos los mensajes automaticos y por confirmacion deben
// empezar a entrar solos") — entering the agent and being sent are
// different things: an 'auto' chat's unhandled message now reaches
// PendingDedicated exactly like a 'dedicated' one. Before this, 'auto'
// chats only went to internal/autoreply — inert without
// PIUMY_BRIDGE=direct-api (off in production), so an 'auto' chat went
// unanswered with nothing to show for it.
func TestPendingDedicatedIncludesAutoMode(t *testing.T) {
	s := openTestStore(t)

	if err := s.SetMode("dedicated@c.us", "dedicated"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetActive("dedicated@c.us", true); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMessage(Message{ChatJID: "dedicated@c.us", ID: "m1", FromMe: false, Text: "hola", TS: 100}); err != nil {
		t.Fatal(err)
	}

	if err := s.SetMode("auto@c.us", "auto"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetActive("auto@c.us", true); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMessage(Message{ChatJID: "auto@c.us", ID: "m2", FromMe: false, Text: "hola", TS: 100}); err != nil {
		t.Fatal(err)
	}

	pending, err := s.PendingDedicated(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("PendingDedicated = %+v, want both dedicated@c.us and auto@c.us", pending)
	}

	count, err := s.CountPendingDedicated()
	if err != nil {
		t.Fatal(err)
	}
	if count != len(pending) {
		t.Errorf("CountPendingDedicated = %d, want %d (must match PendingDedicated exactly, or the queue count lies)", count, len(pending))
	}
}

// TestCountRecentPendingNonBossIncludesAutoMode is T20 (ct-2026-08-05-1301,
// Amatista's R1): CountRecentPendingNonBoss — capipush's own backpressure
// counter — is a THIRD query with the same mode filter T5 widened on
// PendingDedicated/CountPendingDedicated, missed when T5 was written. Before
// this fix, an 'auto' chat's pending traffic was invisible to the
// saturation counter — the semáforo lied by omission about how loaded the
// channel actually was.
func TestCountRecentPendingNonBossIncludesAutoMode(t *testing.T) {
	s := openTestStore(t)

	if err := s.SetMode("auto@c.us", "auto"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetActive("auto@c.us", true); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMessage(Message{ChatJID: "auto@c.us", ID: "m1", FromMe: false, Text: "hola", TS: 100}); err != nil {
		t.Fatal(err)
	}

	count, err := s.CountRecentPendingNonBoss(0)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("CountRecentPendingNonBoss = %d, want 1 — an 'auto' chat's pending message must count toward backpressure", count)
	}
}

// TestPendingDedicatedIncludesConfirmationRequired is T5's other half of the
// same principle: confirmation_mode governs how a reply goes OUT
// (send_message's own gate, untouched by this contract) — it must never
// decide whether the agent gets to see the message. A chat with
// confirmation_mode="always" still has to reach PendingDedicated so the
// agent can draft, even though nothing will send without approval.
func TestPendingDedicatedIncludesConfirmationRequired(t *testing.T) {
	s := openTestStore(t)
	jid := "confirm-required@c.us"

	if err := s.SetMode(jid, "auto"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetActive(jid, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetConfirmationMode(jid, "always"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMessage(Message{ChatJID: jid, ID: "m1", FromMe: false, Text: "hola", TS: 1}); err != nil {
		t.Fatal(err)
	}

	pending, err := s.PendingDedicated(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("PendingDedicated = %+v, want the confirmation-required chat's message dispatched", pending)
	}
}

// TestPendingDedicatedStillExcludesSilencedAutoChats confirms T5's mode
// widening didn't loosen the OTHER gate: an ignored or never-activated
// chat stays out regardless of mode — is_boss remains the only bypass.
// TestPendingDedicatedRespectsConfigLevel already covers this for
// 'dedicated'; this extends it to 'auto', the mode T5 actually changed.
func TestPendingDedicatedStillExcludesSilencedAutoChats(t *testing.T) {
	cases := []struct {
		name   string
		active bool
		status string
	}{
		{"never activated", false, "new"},
		{"explicitly ignored", true, "ignored"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openTestStore(t)
			jid := "silenced-auto@c.us"
			if err := s.SetMode(jid, "auto"); err != nil {
				t.Fatal(err)
			}
			if err := s.SetActive(jid, tc.active); err != nil {
				t.Fatal(err)
			}
			if err := s.SetStatus(jid, tc.status); err != nil {
				t.Fatal(err)
			}
			if err := s.AddMessage(Message{ChatJID: jid, ID: "m1", FromMe: false, Text: "hola", TS: 1}); err != nil {
				t.Fatal(err)
			}

			pending, err := s.PendingDedicated(10)
			if err != nil {
				t.Fatal(err)
			}
			if len(pending) != 0 {
				t.Errorf("PendingDedicated = %+v, want empty (silenced auto chat)", pending)
			}

			count, err := s.CountPendingDedicated()
			if err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Errorf("CountPendingDedicated = %d, want 0", count)
			}
		})
	}
}

// TestPendingDedicatedRespectsConfigLevel is ct-2026-07-21-1853: a chat's
// config_level (boss/auto/confirm/unattended/ignored — ConfigLevel/
// SetConfigLevel) must gate PendingDedicated/CountPendingDedicated, not just
// its mode. Before this fix, an unattended (active=false) or ignored chat in
// dedicated mode still dispatched to the agent — the level showed in the UI
// but the engine never actually enforced it.
func TestPendingDedicatedRespectsConfigLevel(t *testing.T) {
	levels := []struct {
		level      string
		dispatched bool
	}{
		{"boss", true},
		{"auto", true},
		{"confirm", true},
		{"unattended", false},
		{"ignored", false},
	}
	for _, tc := range levels {
		t.Run(tc.level, func(t *testing.T) {
			s := openTestStore(t)
			jid := tc.level + "@c.us"
			if err := s.SetMode(jid, "dedicated"); err != nil {
				t.Fatal(err)
			}
			if err := s.SetConfigLevel(jid, tc.level); err != nil {
				t.Fatal(err)
			}
			if err := s.AddMessage(Message{ChatJID: jid, ID: "m1", FromMe: false, Text: "hola", TS: 1}); err != nil {
				t.Fatal(err)
			}

			pending, err := s.PendingDedicated(10)
			if err != nil {
				t.Fatal(err)
			}
			gotDispatched := len(pending) == 1
			if gotDispatched != tc.dispatched {
				t.Errorf("PendingDedicated at level %q = %+v, want dispatched=%v", tc.level, pending, tc.dispatched)
			}

			count, err := s.CountPendingDedicated()
			if err != nil {
				t.Fatal(err)
			}
			wantCount := 0
			if tc.dispatched {
				wantCount = 1
			}
			if count != wantCount {
				t.Errorf("CountPendingDedicated at level %q = %d, want %d", tc.level, count, wantCount)
			}
		})
	}
}

// TestPendingDedicatedBossDispatchesEvenNeverActivated is the fix to
// ct-2026-07-21-1853's own follow-up: ConfigLevel treats IsBoss as an
// unconditional, first-priority case — a boss chat is "boss" level (must
// dispatch) regardless of active/status. The real boss chat commonly never
// goes through set_chat_active/set_config_level (is_boss is set once,
// directly), so active stays at its schema default (false). Without an
// explicit is_boss bypass, PendingDedicated would silently stop dispatching
// to the boss himself.
func TestPendingDedicatedBossDispatchesEvenNeverActivated(t *testing.T) {
	s := openTestStore(t)
	jid := "boss-never-activated@c.us"

	if err := s.SetMode(jid, "dedicated"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetIsBoss(jid, true); err != nil {
		t.Fatal(err)
	}
	// Deliberately no SetActive/SetConfigLevel call — active stays false,
	// the untouched schema default.
	if err := s.AddMessage(Message{ChatJID: jid, ID: "m1", FromMe: false, Text: "hola", TS: 1}); err != nil {
		t.Fatal(err)
	}

	c, ok, err := s.GetChat(jid)
	if err != nil || !ok {
		t.Fatalf("GetChat: ok=%v err=%v", ok, err)
	}
	if ConfigLevel(c) != "boss" {
		t.Fatalf("setup: ConfigLevel = %q, want boss", ConfigLevel(c))
	}
	if c.Active {
		t.Fatal("setup: active should still be false (never explicitly activated)")
	}

	pending, err := s.PendingDedicated(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Errorf("PendingDedicated for a never-activated boss chat = %+v, want the message dispatched", pending)
	}

	count, err := s.CountPendingDedicated()
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("CountPendingDedicated for a never-activated boss chat = %d, want 1", count)
	}
}
