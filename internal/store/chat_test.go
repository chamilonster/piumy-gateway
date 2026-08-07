package store

import (
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestTouchChatConfirmationModeByType(t *testing.T) {
	s := openTestStore(t)

	if err := s.TouchChat("111@c.us", "Ana", 100); err != nil {
		t.Fatalf("TouchChat 1-1: %v", err)
	}
	c, ok, err := s.GetChat("111@c.us")
	if err != nil || !ok {
		t.Fatalf("GetChat 1-1: ok=%v err=%v", ok, err)
	}
	if c.ConfirmationMode != "none" || c.Status != "new" {
		t.Errorf("1-1 chat: got confirmation_mode=%q status=%q, want none/new", c.ConfirmationMode, c.Status)
	}

	if err := s.TouchChat("222@g.us", "Grupo", 100); err != nil {
		t.Fatalf("TouchChat group: %v", err)
	}
	g, ok, err := s.GetChat("222@g.us")
	if err != nil || !ok {
		t.Fatalf("GetChat group: ok=%v err=%v", ok, err)
	}
	if g.ConfirmationMode != "always" || g.Status != "ignored" {
		t.Errorf("group chat: got confirmation_mode=%q status=%q, want always/ignored", g.ConfirmationMode, g.Status)
	}
}

// TestMigrateRequiredToAlwaysReconcilesLegacyValue covers the F4c audit
// fix directly: a row a pre-fix build wrote with the legacy 'required'
// value gets reconciled to 'always' on open (send_message's gate only
// checks "always" — "required" silently disabled the fail-safe).
func TestMigrateRequiredToAlwaysReconcilesLegacyValue(t *testing.T) {
	s := openTestStore(t)
	if err := s.TouchChat("333@g.us", "Legacy", 100); err != nil {
		t.Fatal(err)
	}
	if err := s.SetConfirmationMode("333@g.us", "required"); err != nil {
		t.Fatal(err)
	}
	if err := migrateRequiredToAlways(s.db); err != nil {
		t.Fatal(err)
	}
	c, _, err := s.GetChat("333@g.us")
	if err != nil {
		t.Fatal(err)
	}
	if c.ConfirmationMode != "always" {
		t.Errorf("confirmation_mode after migrateRequiredToAlways = %q, want always", c.ConfirmationMode)
	}
}

func TestSetModeNormalizesLegacyAdvanced(t *testing.T) {
	s := openTestStore(t)

	if err := s.SetMode("111@c.us", "advanced"); err != nil {
		t.Fatalf("SetMode: %v", err)
	}
	c, ok, err := s.GetChat("111@c.us")
	if err != nil || !ok {
		t.Fatalf("GetChat: ok=%v err=%v", ok, err)
	}
	if c.Mode != "dedicated" {
		t.Errorf("got mode=%q, want 'advanced' normalized to 'dedicated'", c.Mode)
	}
}

// TestSetActiveSweepsOldBacklogOnActivation is S8's own regression (ct-
// 2026-07-30-031126): activating a chat with months of unhandled backlog
// used to dump ALL of it into PendingDedicated the instant active flipped,
// ts ASC — the boss's own live smoke (a real contact's chat) jumped the
// queue from 76 to 82 instantly, with a message from February ahead of
// today's conversation. Activating means "de ahora en adelante", not
// "reprocesá su historia completa".
func TestSetActiveSweepsOldBacklogOnActivation(t *testing.T) {
	s := openTestStore(t)
	jid := "555000001@c.us"
	if err := s.SetMode(jid, "dedicated"); err != nil {
		t.Fatal(err)
	}
	// Old backlog arrives while the chat is still inactive (the default).
	if err := s.AddMessage(Message{ChatJID: jid, ID: "old1", FromMe: false, Text: "Hola! Con quién hablo?", TS: 1772038322}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMessage(Message{ChatJID: jid, ID: "old2", FromMe: false, Text: "otro mensaje viejo", TS: 1772038400}); err != nil {
		t.Fatal(err)
	}

	if err := s.SetActive(jid, true); err != nil {
		t.Fatal(err)
	}

	pending, err := s.PendingDedicated(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("PendingDedicated right after activation = %+v, want 0 (old backlog swept to handled, not dumped into the live queue)", pending)
	}

	// Cuidado del contrato: no borrar, no ocultar del historial.
	msgs, err := s.GetMessages(jid, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Errorf("GetMessages after activation = %d, want 2 — the backlog must stay fully visible, never deleted or hidden", len(msgs))
	}
}

// TestSetActiveSweepPreservesMessageThatPromptedActivation is Citrino's own
// catch on the first S8 pass: sweeping everything up to `now` swallowed the
// EXACT message that prompted the activation — the boss's real flow is
// "atendé a este número" (verbatim, 2026-07-30) AFTER spotting a message
// that just arrived, seconds to minutes earlier, never a simultaneous
// coincidence. A message from a minute ago must survive the sweep and
// still dispatch, while genuinely old backlog (same chat, same activation)
// still gets swept — both truths at once, or the fix just moves the bug.
func TestSetActiveSweepPreservesMessageThatPromptedActivation(t *testing.T) {
	s := openTestStore(t)
	jid := "9849083@c.us"
	if err := s.SetMode(jid, "dedicated"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	// Old backlog — months old, must still be swept.
	if err := s.AddMessage(Message{ChatJID: jid, ID: "old", FromMe: false, Text: "viejo, de meses atrás", TS: now.Add(-90 * 24 * time.Hour).Unix()}); err != nil {
		t.Fatal(err)
	}
	// The message that prompted "atendé a este número" — arrived a minute
	// before the boss's order, well inside activationSweepWindow's default.
	if err := s.AddMessage(Message{ChatJID: jid, ID: "trigger", FromMe: false, Text: "hola, necesito ayuda", TS: now.Add(-1 * time.Minute).Unix()}); err != nil {
		t.Fatal(err)
	}

	if err := s.SetActive(jid, true); err != nil {
		t.Fatal(err)
	}

	pending, err := s.PendingDedicated(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != "trigger" {
		t.Fatalf("PendingDedicated after activation = %+v, want exactly the 1-minute-old message that prompted it — swallowing it means the agent replies to someone without ever seeing what they said", pending)
	}
}

// TestSetActiveDoesNotResweepAlreadyActiveChat guards the transition check
// itself: a redundant SetActive(jid, true) call on an ALREADY active chat
// (e.g. set_config_level re-confirming a level that was already set) must
// never sweep a message the agent genuinely hasn't gotten to yet — only a
// real inactive→active transition triggers the sweep.
func TestSetActiveDoesNotResweepAlreadyActiveChat(t *testing.T) {
	s := openTestStore(t)
	jid := "already-active@c.us"
	if err := s.SetMode(jid, "dedicated"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetActive(jid, true); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMessage(Message{ChatJID: jid, ID: "fresh", FromMe: false, Text: "hola", TS: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}

	// Redundant re-activation — must be a no-op regarding messages.
	if err := s.SetActive(jid, true); err != nil {
		t.Fatal(err)
	}

	pending, err := s.PendingDedicated(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != "fresh" {
		t.Errorf("PendingDedicated after a redundant re-activation = %+v, want the fresh message left untouched", pending)
	}
}

// TestEnrichChatLastText covers enrichChat's LastText fill — reuses the
// same LastMessage call already made for LastSpeaker, no extra query.
func TestEnrichChatLastText(t *testing.T) {
	s := openTestStore(t)
	if err := s.TouchChat("111@c.us", "Ana", 100); err != nil {
		t.Fatalf("TouchChat: %v", err)
	}
	c, ok, err := s.GetChat("111@c.us")
	if err != nil || !ok {
		t.Fatalf("GetChat before any message: ok=%v err=%v", ok, err)
	}
	if c.LastText != "" {
		t.Errorf("LastText with no messages = %q, want empty", c.LastText)
	}

	if err := s.AddMessage(Message{ChatJID: "111@c.us", ID: "m1", FromMe: false, Text: "hola", TS: 100}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMessage(Message{ChatJID: "111@c.us", ID: "m2", FromMe: true, Text: "qué tal", TS: 200}); err != nil {
		t.Fatal(err)
	}
	c, ok, err = s.GetChat("111@c.us")
	if err != nil || !ok {
		t.Fatalf("GetChat after messages: ok=%v err=%v", ok, err)
	}
	if c.LastText != "qué tal" {
		t.Errorf("LastText = %q, want the most recent message's text (qué tal)", c.LastText)
	}
}

// TestConfigLevel covers every branch of ConfigLevel's priority-ordered
// mapping directly against the Chat struct — no store needed, it's a pure
// function.
func TestConfigLevel(t *testing.T) {
	cases := []struct {
		name string
		c    Chat
		want string
	}{
		{"is_boss wins over everything", Chat{IsBoss: true, Status: "ignored", Active: false, ConfirmationMode: "always"}, "boss"},
		{"ignored status", Chat{IsBoss: false, Status: "ignored", Active: true, ConfirmationMode: "none"}, "ignored"},
		{"inactive -> unattended", Chat{IsBoss: false, Status: "new", Active: false, ConfirmationMode: "none"}, "unattended"},
		{"confirmation always -> confirm", Chat{IsBoss: false, Status: "new", Active: true, ConfirmationMode: "always"}, "confirm"},
		{"confirmation discretion -> confirm", Chat{IsBoss: false, Status: "new", Active: true, ConfirmationMode: "discretion"}, "confirm"},
		{"confirmation none -> auto", Chat{IsBoss: false, Status: "new", Active: true, ConfirmationMode: "none"}, "auto"},
		{"unrecognized confirmation_mode -> unattended", Chat{IsBoss: false, Status: "new", Active: true, ConfirmationMode: "bogus"}, "unattended"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ConfigLevel(tc.c); got != tc.want {
				t.Errorf("ConfigLevel(%+v) = %q, want %q", tc.c, got, tc.want)
			}
		})
	}
}

// TestSetConfigLevelRoundTrip covers each of the 5 levels: setting it
// persists the 4 fields as documented, and ConfigLevel reads the SAME level
// back. Starts from a fresh individual chat (status "new", TouchChat's
// default) for every case — the one edge case where round-trip doesn't
// hold (a chat already "ignored" before setting "confirm"/"boss") is
// flagged in SetConfigLevel's own doc comment, not exercised here.
func TestSetConfigLevelRoundTrip(t *testing.T) {
	for _, level := range []string{"boss", "auto", "confirm", "unattended", "ignored"} {
		t.Run(level, func(t *testing.T) {
			s := openTestStore(t)
			jid := "111@c.us"
			if err := s.TouchChat(jid, "Ana", 100); err != nil {
				t.Fatalf("TouchChat: %v", err)
			}
			if err := s.SetConfigLevel(jid, level); err != nil {
				t.Fatalf("SetConfigLevel(%q): %v", level, err)
			}
			c, ok, err := s.GetChat(jid)
			if err != nil || !ok {
				t.Fatalf("GetChat: ok=%v err=%v", ok, err)
			}
			if got := ConfigLevel(c); got != level {
				t.Errorf("ConfigLevel after SetConfigLevel(%q) = %q, want %q (chat=%+v)", level, got, level, c)
			}
		})
	}
}

// TestSetConfigLevelAutoPromotesIgnoredStatus covers the one status
// transition "auto" makes: an ignored (or blacklisted) chat becomes
// "whitelist" — SetConfigLevel's documented exception to "otherwise leave
// Status alone".
func TestSetConfigLevelAutoPromotesIgnoredStatus(t *testing.T) {
	s := openTestStore(t)
	jid := "222@g.us" // group default: status "ignored"
	if err := s.TouchChat(jid, "Grupo", 100); err != nil {
		t.Fatalf("TouchChat: %v", err)
	}
	if err := s.SetConfigLevel(jid, "auto"); err != nil {
		t.Fatalf("SetConfigLevel(auto): %v", err)
	}
	c, ok, err := s.GetChat(jid)
	if err != nil || !ok {
		t.Fatalf("GetChat: ok=%v err=%v", ok, err)
	}
	if c.Status != "whitelist" {
		t.Errorf("Status after SetConfigLevel(auto) on an ignored chat = %q, want whitelist", c.Status)
	}
	if ConfigLevel(c) != "auto" {
		t.Errorf("ConfigLevel after SetConfigLevel(auto) = %q, want auto", ConfigLevel(c))
	}
}

// TestSetConfigLevelUnattendedResetsIgnoredStatus covers "unattended"'s own
// status exception: an ignored chat reverts to "new" (same baseline
// TouchChat/handleSetIgnored already use to un-ignore).
func TestSetConfigLevelUnattendedResetsIgnoredStatus(t *testing.T) {
	s := openTestStore(t)
	jid := "333@c.us"
	if err := s.TouchChat(jid, "Bea", 100); err != nil {
		t.Fatalf("TouchChat: %v", err)
	}
	if err := s.SetStatus(jid, "ignored"); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if err := s.SetConfigLevel(jid, "unattended"); err != nil {
		t.Fatalf("SetConfigLevel(unattended): %v", err)
	}
	c, ok, err := s.GetChat(jid)
	if err != nil || !ok {
		t.Fatalf("GetChat: ok=%v err=%v", ok, err)
	}
	if c.Status != "new" {
		t.Errorf("Status after SetConfigLevel(unattended) on an ignored chat = %q, want new", c.Status)
	}
}

func TestSetConfigLevelRejectsInvalidLevel(t *testing.T) {
	s := openTestStore(t)
	jid := "444@c.us"
	if err := s.TouchChat(jid, "Cami", 100); err != nil {
		t.Fatalf("TouchChat: %v", err)
	}
	if err := s.SetConfigLevel(jid, "bogus"); err == nil {
		t.Error("SetConfigLevel(bogus) = nil error, want an error")
	}
}

func TestClaimChatExclusive(t *testing.T) {
	s := openTestStore(t)
	if err := s.TouchChat("111@c.us", "Ana", 100); err != nil {
		t.Fatalf("TouchChat: %v", err)
	}

	ok, err := s.ClaimChat("111@c.us", "model-a", 60*time.Second)
	if err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v", ok, err)
	}
	ok, err = s.ClaimChat("111@c.us", "model-b", 60*time.Second)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if ok {
		t.Error("model-b claimed a chat already held by model-a")
	}
	if err := s.ReleaseChat("111@c.us", "model-a"); err != nil {
		t.Fatalf("ReleaseChat: %v", err)
	}
	ok, err = s.ClaimChat("111@c.us", "model-b", 60*time.Second)
	if err != nil || !ok {
		t.Fatalf("claim after release: ok=%v err=%v", ok, err)
	}
}

// TestNewChatDefaultsHistoryStatePending covers the migration's default
// (ct-2026-07-21-1306) — a brand-new chat, and by extension every
// pre-existing row backfilled by the ALTER, starts out "pending" so the
// history worker picks it up.
func TestNewChatDefaultsHistoryStatePending(t *testing.T) {
	s := openTestStore(t)
	if err := s.TouchChat("500@c.us", "Ana", 100); err != nil {
		t.Fatalf("TouchChat: %v", err)
	}
	c, ok, err := s.GetChat("500@c.us")
	if err != nil || !ok {
		t.Fatalf("GetChat: ok=%v err=%v", ok, err)
	}
	if c.HistoryState != "pending" {
		t.Errorf("HistoryState = %q, want pending", c.HistoryState)
	}
}

// TestHistorySummaryCounts covers the post-ct-2026-07-24-2004 meaning: how
// many chats have at least one message on file, over the total — the
// ON_DEMAND worker's old history_state-based progress is gone (the worker
// itself was removed).
func TestHistorySummaryCounts(t *testing.T) {
	s := openTestStore(t)
	if err := s.TouchChat("506@c.us", "A", 100); err != nil {
		t.Fatalf("TouchChat: %v", err)
	}
	if err := s.TouchChat("507@c.us", "B", 100); err != nil {
		t.Fatalf("TouchChat: %v", err)
	}
	if err := s.TouchChat("508@c.us", "C", 100); err != nil {
		t.Fatalf("TouchChat: %v", err)
	}
	if err := s.AddMessage(Message{ChatJID: "506@c.us", ID: "m1", Text: "hola", TS: 100}); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}

	withMessages, total, err := s.HistorySummary()
	if err != nil {
		t.Fatalf("HistorySummary: %v", err)
	}
	if withMessages != 1 || total != 3 {
		t.Errorf("HistorySummary = (%d, %d), want (1, 3)", withMessages, total)
	}
}

// ── M5 (ct-2026-07-22-1903) — defaults de atención por origen, pipeline ────

// configSource reads chats.config_level_source directly (white-box, same
// package) — not exposed on Chat itself, an internal bookkeeping column
// only applyOriginDefaultIfUnset needs to read.
func configSource(t *testing.T, s *Store, jid string) string {
	t.Helper()
	var v string
	if err := s.db.QueryRow(`SELECT config_level_source FROM chats WHERE jid = ?`, jid).Scan(&v); err != nil {
		t.Fatalf("configSource(%s): %v", jid, err)
	}
	return v
}

func TestTouchChatAppliesOriginDefaultForNewNumber(t *testing.T) {
	s := openTestStore(t)
	if err := s.KVSet(SettingConfigLevelDefaultNew, "auto"); err != nil {
		t.Fatal(err)
	}
	jid := "1@s.whatsapp.net"
	if err := s.TouchChat(jid, "Alguien", 1); err != nil {
		t.Fatal(err)
	}
	c, ok, err := s.GetChat(jid)
	if err != nil || !ok {
		t.Fatalf("GetChat: ok=%v err=%v", ok, err)
	}
	if !c.Active || c.ConfirmationMode != "none" {
		t.Errorf("chat = %+v, want active=true confirmation_mode=none (the configured 'auto' default)", c)
	}
	if got := configSource(t, s, jid); got != "default" {
		t.Errorf("config_level_source = %q, want default", got)
	}
}

func TestTouchChatSkipsOriginDefaultForGroup(t *testing.T) {
	s := openTestStore(t)
	if err := s.KVSet(SettingConfigLevelDefaultNew, "auto"); err != nil {
		t.Fatal(err)
	}
	jid := "123-456@g.us"
	if err := s.TouchChat(jid, "Grupo", 1); err != nil {
		t.Fatal(err)
	}
	c, ok, err := s.GetChat(jid)
	if err != nil || !ok {
		t.Fatalf("GetChat: ok=%v err=%v", ok, err)
	}
	// Groups keep TouchChat's own baseline (ignored/always/active=false) —
	// the M5 origin axis must never touch a group row, even with a KV
	// default configured.
	if c.Active || c.Status != "ignored" || c.ConfirmationMode != "always" {
		t.Errorf("group chat = %+v, want TouchChat's unchanged group baseline", c)
	}
	if got := configSource(t, s, jid); got != "manual" {
		t.Errorf("group config_level_source = %q, want manual (groups are out of the origin axis)", got)
	}
}

func TestSetContactNameAppliesContactDefaultOverridingNewNumber(t *testing.T) {
	s := openTestStore(t)
	if err := s.KVSet(SettingConfigLevelDefaultNew, "unattended"); err != nil {
		t.Fatal(err)
	}
	if err := s.KVSet(SettingConfigLevelDefaultContact, "auto"); err != nil {
		t.Fatal(err)
	}
	jid := "2@s.whatsapp.net"
	if err := s.TouchChat(jid, "Alguien", 1); err != nil {
		t.Fatal(err)
	}
	c, _, _ := s.GetChat(jid)
	if c.Active {
		t.Fatalf("precondition: chat should start unattended (active=false), got %+v", c)
	}

	if err := s.SetContactName(jid, "Juan Pérez"); err != nil {
		t.Fatal(err)
	}
	c, ok, err := s.GetChat(jid)
	if err != nil || !ok {
		t.Fatalf("GetChat: ok=%v err=%v", ok, err)
	}
	if !c.Active || c.ConfirmationMode != "none" {
		t.Errorf("chat after becoming a contact = %+v, want the 'auto' contact default (contact wins over new-number)", c)
	}
}

func TestApplyOriginDefaultRespectsManualOverride(t *testing.T) {
	s := openTestStore(t)
	if err := s.KVSet(SettingConfigLevelDefaultContact, "auto"); err != nil {
		t.Fatal(err)
	}
	jid := "3@s.whatsapp.net"
	if err := s.TouchChat(jid, "Alguien", 1); err != nil {
		t.Fatal(err)
	}
	// The owner explicitly sets confirmation_mode BEFORE the contact syncs —
	// a real decision that must survive the later contact-name transition.
	if err := s.SetConfirmationMode(jid, "always"); err != nil {
		t.Fatal(err)
	}
	if got := configSource(t, s, jid); got != "manual" {
		t.Fatalf("config_level_source after SetConfirmationMode = %q, want manual", got)
	}

	if err := s.SetContactName(jid, "Juan Pérez"); err != nil {
		t.Fatal(err)
	}
	c, _, _ := s.GetChat(jid)
	if c.ConfirmationMode != "always" {
		t.Errorf("ConfirmationMode = %q, want always — the owner's explicit choice must survive becoming a contact", c.ConfirmationMode)
	}
}

// TestApplyOriginDefaultFallsBackToUnattendedWhenKVUnset (ct-2026-07-22-2100,
// safety default: "por defecto el agente NO atiende, hasta que configure
// explícitamente" — boss verbatim): no config_level_default_* KV ever
// set still lands on the SAME unattended baseline TouchChat's own schema
// defaults already produce (active=false) — EffectiveConfigLevelDefault's
// "" -> "unattended" substitution is idempotent here, not a behavior
// change for a chat that was already going to be unattended anyway. The
// real effect is elsewhere (a chat that HAD an "auto" new-number default
// applied must fall back to unattended, not stay auto, once it becomes a
// contact with no contact-default configured — that's a separate risk this
// test doesn't need to re-cover, applyConfigLevelDefault's own unit
// behavior + TestSetContactNameAppliesContactDefaultOverridingNewNumber
// already establish the write path).
func TestApplyOriginDefaultFallsBackToUnattendedWhenKVUnset(t *testing.T) {
	s := openTestStore(t)
	// No config_level_default_* KV ever set — M5 untouched by the boss.
	jid := "4@s.whatsapp.net"
	if err := s.TouchChat(jid, "Alguien", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.SetContactName(jid, "Juan Pérez"); err != nil {
		t.Fatal(err)
	}
	c, ok, err := s.GetChat(jid)
	if err != nil || !ok {
		t.Fatalf("GetChat: ok=%v err=%v", ok, err)
	}
	if c.Active || c.Status != "new" || c.ConfirmationMode != "none" {
		t.Errorf("chat = %+v, want unattended (active=false), status/confirmation_mode untouched by the 'unattended' case", c)
	}
}

// TestEffectiveConfigLevelDefault (ct-2026-07-22-2100): unset -> "unattended"
// (the safety default); an explicitly-configured value passes through
// unchanged.
func TestEffectiveConfigLevelDefault(t *testing.T) {
	s := openTestStore(t)
	got, err := s.EffectiveConfigLevelDefault(SettingConfigLevelDefaultNew)
	if err != nil || got != "unattended" {
		t.Errorf("EffectiveConfigLevelDefault(unset) = %q, err=%v, want unattended", got, err)
	}
	if err := s.KVSet(SettingConfigLevelDefaultNew, "auto"); err != nil {
		t.Fatal(err)
	}
	got, err = s.EffectiveConfigLevelDefault(SettingConfigLevelDefaultNew)
	if err != nil || got != "auto" {
		t.Errorf("EffectiveConfigLevelDefault(set to auto) = %q, err=%v, want auto", got, err)
	}
}

// TestEffectiveRulesOriginAxis (M5, ct-2026-07-22-1903): for an individual
// chat with no particular rules, contact vs new-number decides which
// origin tier applies — contact WINS (boss verbatim). Groups are
// unaffected, still rules_type_group -> rules_default.
func TestEffectiveRulesOriginAxis(t *testing.T) {
	s := openTestStore(t)
	if err := s.SetDefaultRules("global: default"); err != nil {
		t.Fatal(err)
	}
	if err := s.KVSet(SettingRulesDefaultNewNumber, "número nuevo: cauteloso"); err != nil {
		t.Fatal(err)
	}
	if err := s.KVSet(SettingRulesDefaultContact, "contacto: cálido"); err != nil {
		t.Fatal(err)
	}

	newNumber := "10@s.whatsapp.net"
	if err := s.TouchChat(newNumber, "Alguien", 1); err != nil {
		t.Fatal(err)
	}
	if got, err := s.EffectiveRules(newNumber); err != nil || got != "número nuevo: cauteloso" {
		t.Errorf("EffectiveRules(new number) = %q, err=%v, want the new-number origin tier", got, err)
	}

	contact := "11@s.whatsapp.net"
	if err := s.TouchChat(contact, "Alguien", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.SetContactName(contact, "Juan Pérez"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.EffectiveRules(contact); err != nil || got != "contacto: cálido" {
		t.Errorf("EffectiveRules(contact) = %q, err=%v, want the contact origin tier — contact must WIN", got, err)
	}

	// A particular rule on the chat itself still outranks the origin tier.
	if err := s.SetChatRules(contact, "particular: mío"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.EffectiveRules(contact); err != nil || got != "particular: mío" {
		t.Errorf("EffectiveRules(contact with particular rules) = %q, err=%v, want the particular rule", got, err)
	}

	// Groups: unaffected by the origin axis, still rules_type_group.
	group := "12345@g.us"
	if err := s.TouchChat(group, "Grupo", 1); err != nil {
		t.Fatal(err)
	}
	if got, err := s.EffectiveRules(group); err != nil || got != "global: default" {
		t.Errorf("EffectiveRules(group, no type rules) = %q, err=%v, want the global default (origin axis doesn't apply to groups)", got, err)
	}
	if err := s.SetTypeRules("group", "grupo: tipo"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.EffectiveRules(group); err != nil || got != "grupo: tipo" {
		t.Errorf("EffectiveRules(group with type rules) = %q, err=%v, want the group type tier", got, err)
	}
}

func TestMarkConfigManualBlocksFutureOriginDefault(t *testing.T) {
	s := openTestStore(t)
	if err := s.KVSet(SettingConfigLevelDefaultNew, "auto"); err != nil {
		t.Fatal(err)
	}
	jid := "5@s.whatsapp.net"
	if err := s.TouchChat(jid, "Alguien", 1); err != nil { // config_level_source starts 'default'
		t.Fatal(err)
	}
	if got := configSource(t, s, jid); got != "default" {
		t.Fatalf("precondition: config_level_source = %q, want default", got)
	}

	// Simulates handleSetIgnored's own pair of calls (SetStatus + MarkConfigManual).
	if err := s.SetStatus(jid, "ignored"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkConfigManual(jid); err != nil {
		t.Fatal(err)
	}
	if got := configSource(t, s, jid); got != "manual" {
		t.Fatalf("config_level_source after MarkConfigManual = %q, want manual", got)
	}

	// A later TouchChat (another inbound message) must NOT revive it via
	// the origin default — the owner's ignore decision wins.
	if err := s.TouchChat(jid, "Alguien", 2); err != nil {
		t.Fatal(err)
	}
	c, _, _ := s.GetChat(jid)
	if c.Status != "ignored" {
		t.Errorf("Status = %q, want ignored to survive TouchChat", c.Status)
	}
}

// TestSetIsApproverRoundTrip (Aprobador P1, ct-2026-07-31-0610): the pin
// round-trips independently of is_boss/config_level_source — orthogonal by
// construction, see Chat.IsApprover's doc.
func TestSetIsApproverRoundTrip(t *testing.T) {
	s := openTestStore(t)
	jid := "222@c.us"
	if err := s.TouchChat(jid, "Secretaria", 1); err != nil {
		t.Fatal(err)
	}

	if err := s.SetIsApprover(jid, true); err != nil {
		t.Fatalf("SetIsApprover(true): %v", err)
	}
	c, ok, err := s.GetChat(jid)
	if err != nil || !ok {
		t.Fatalf("GetChat: ok=%v err=%v", ok, err)
	}
	if !c.IsApprover {
		t.Error("IsApprover = false after SetIsApprover(true)")
	}
	if c.IsBoss {
		t.Error("IsBoss = true after SetIsApprover(true) — the pin must not imply ownership")
	}

	if err := s.SetIsApprover(jid, false); err != nil {
		t.Fatalf("SetIsApprover(false): %v", err)
	}
	c, _, _ = s.GetChat(jid)
	if c.IsApprover {
		t.Error("IsApprover = true after SetIsApprover(false)")
	}
}

// TestSetIsApproverIndependentOfConfigLevel: marking a chat approver must
// not look like a manual config-level override — is_approver is a separate
// axis from the 5 unified config levels (boss/auto/confirm/unattended/
// ignored), unlike SetIsBoss/SetActive/SetConfirmationMode which all mark
// config_level_source='manual' because they DO set one of those 5.
func TestSetIsApproverIndependentOfConfigLevel(t *testing.T) {
	s := openTestStore(t)
	jid := "223@c.us"
	if err := s.TouchChat(jid, "Auto", 1); err != nil {
		t.Fatal(err)
	}
	before := configSource(t, s, jid)

	if err := s.SetIsApprover(jid, true); err != nil {
		t.Fatal(err)
	}
	if got := configSource(t, s, jid); got != before {
		t.Errorf("config_level_source after SetIsApprover = %q, want unchanged from %q — the pin is orthogonal to config_level", got, before)
	}
}

// TestMarkOwnerIfUntouchedMarksFreshChat is the T12 (ct-2026-08-05-1231)
// clean-install case: a chat nobody has ever decided is_boss for gets
// marked owner the first time MarkOwnerIfUntouched runs (whatsmeow's
// recordOwnIdentity, at connect).
func TestMarkOwnerIfUntouchedMarksFreshChat(t *testing.T) {
	s := openTestStore(t)
	jid := "55500000021@s.whatsapp.net"
	if err := s.TouchChat(jid, "Yo", 1); err != nil {
		t.Fatal(err)
	}

	if err := s.MarkOwnerIfUntouched(jid); err != nil {
		t.Fatalf("MarkOwnerIfUntouched: %v", err)
	}

	c, ok, err := s.GetChat(jid)
	if err != nil || !ok {
		t.Fatalf("GetChat: ok=%v err=%v", ok, err)
	}
	if !c.IsBoss {
		t.Error("IsBoss = false after MarkOwnerIfUntouched on a fresh chat, want true")
	}
	if c.ConfirmationMode != "none" {
		t.Errorf("ConfirmationMode = %q, want none — TouchChat's own individual-chat default must survive, or every reply to the owner becomes a draft awaiting his own approval (send.go's confirmation_mode gate ignores is_boss)", c.ConfirmationMode)
	}
}

// TestMarkOwnerIfUntouchedSkipsManualUnmark is the T12 reconnect case: once
// the owner has explicitly unmarked his own chat (SetIsBoss(false), via the
// REST admin path), a later reconnect (MarkOwnerIfUntouched again) must
// never re-apply the auto-mark — that would fight the owner's own decision.
func TestMarkOwnerIfUntouchedSkipsManualUnmark(t *testing.T) {
	s := openTestStore(t)
	jid := "55500000021@s.whatsapp.net"
	if err := s.TouchChat(jid, "Yo", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkOwnerIfUntouched(jid); err != nil {
		t.Fatal(err)
	}
	if err := s.SetIsBoss(jid, false); err != nil {
		t.Fatalf("SetIsBoss(false): %v", err)
	}

	// Simulates a reconnect (*events.Connected fires again).
	if err := s.MarkOwnerIfUntouched(jid); err != nil {
		t.Fatalf("MarkOwnerIfUntouched (reconnect): %v", err)
	}

	c, ok, err := s.GetChat(jid)
	if err != nil || !ok {
		t.Fatalf("GetChat: ok=%v err=%v", ok, err)
	}
	if c.IsBoss {
		t.Error("IsBoss = true after a reconnect following a manual unmark — must stay false, the owner's decision must not be fought")
	}
}

// TestMarkOwnerIfUntouchedNoopOnUntouchedRow: a chat row that pre-dates T12
// (is_boss_touched defaults to 1 on migration, see schema.go) must never be
// silently flipped to owner just because it happens to be OwnJID one day —
// MarkOwnerIfUntouched only ever fires its effect on a genuinely untouched
// (is_boss_touched=0) row.
func TestMarkOwnerIfUntouchedNoopOnUntouchedRow(t *testing.T) {
	s := openTestStore(t)
	jid := "55500000074@s.whatsapp.net"
	if err := s.TouchChat(jid, "Otro", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE chats SET is_boss_touched = 1 WHERE jid = ?`, jid); err != nil {
		t.Fatal(err)
	}

	if err := s.MarkOwnerIfUntouched(jid); err != nil {
		t.Fatal(err)
	}

	c, _, _ := s.GetChat(jid)
	if c.IsBoss {
		t.Error("IsBoss = true after MarkOwnerIfUntouched on an already-touched row, want it left alone")
	}
}

// ── ChatOrigin (T18, ct-2026-08-05-1243) ────────────────────────────────

// TestChatOriginRealMessageEitherDirection is T18's core regression:
// ChatOrigin used to check ONLY from_me=0 (inbound) — a chat the OWNER
// started that never got a reply read as group_discovered/synced_contact
// instead of the real conversation it is. Both directions must now count.
func TestChatOriginRealMessageEitherDirection(t *testing.T) {
	s := openTestStore(t)

	t.Run("owner-initiated, no reply, also a group member", func(t *testing.T) {
		jid := "111@c.us"
		if err := s.TouchChat(jid, "A", 1); err != nil {
			t.Fatal(err)
		}
		if err := s.AddMessage(Message{ChatJID: jid, ID: "m1", FromMe: true, Text: "hola, tenes stock?", TS: 1}); err != nil {
			t.Fatal(err)
		}
		if err := s.UpsertGroupMember("group1@g.us", jid, "", 1); err != nil {
			t.Fatal(err)
		}
		origin, err := s.ChatOrigin(jid)
		if err != nil {
			t.Fatal(err)
		}
		if origin != "inbound_spoke" {
			t.Errorf("ChatOrigin = %q, want inbound_spoke (a real outbound-only conversation, even though the contact shares a group)", origin)
		}
	})

	t.Run("owner-initiated, no reply, no group", func(t *testing.T) {
		jid := "222@c.us"
		if err := s.TouchChat(jid, "B", 1); err != nil {
			t.Fatal(err)
		}
		if err := s.AddMessage(Message{ChatJID: jid, ID: "m1", FromMe: true, Text: "hola, tenes stock?", TS: 1}); err != nil {
			t.Fatal(err)
		}
		origin, err := s.ChatOrigin(jid)
		if err != nil {
			t.Fatal(err)
		}
		if origin != "inbound_spoke" {
			t.Errorf("ChatOrigin = %q, want inbound_spoke", origin)
		}
	})

	t.Run("real inbound reply exists (control)", func(t *testing.T) {
		jid := "333@c.us"
		if err := s.TouchChat(jid, "C", 1); err != nil {
			t.Fatal(err)
		}
		if err := s.AddMessage(Message{ChatJID: jid, ID: "m1", FromMe: true, Text: "hola", TS: 1}); err != nil {
			t.Fatal(err)
		}
		if err := s.AddMessage(Message{ChatJID: jid, ID: "m2", FromMe: false, Text: "hola, si tenemos", TS: 2}); err != nil {
			t.Fatal(err)
		}
		origin, err := s.ChatOrigin(jid)
		if err != nil {
			t.Fatal(err)
		}
		if origin != "inbound_spoke" {
			t.Errorf("ChatOrigin = %q, want inbound_spoke", origin)
		}
	})
}

// TestChatOriginGroupDiscoveredAndSyncedContact: no real message anywhere —
// the two remaining cases split on group_members membership.
func TestChatOriginGroupDiscoveredAndSyncedContact(t *testing.T) {
	s := openTestStore(t)

	groupMember := "444@c.us"
	if err := s.TouchChat(groupMember, "D", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertGroupMember("group2@g.us", groupMember, "", 1); err != nil {
		t.Fatal(err)
	}
	if origin, err := s.ChatOrigin(groupMember); err != nil || origin != "group_discovered" {
		t.Errorf("ChatOrigin(no message, in a group) = %q, err=%v, want group_discovered", origin, err)
	}

	plain := "555@c.us"
	if err := s.TouchChat(plain, "E", 1); err != nil {
		t.Fatal(err)
	}
	if origin, err := s.ChatOrigin(plain); err != nil || origin != "synced_contact" {
		t.Errorf("ChatOrigin(no message, no group) = %q, err=%v, want synced_contact", origin, err)
	}
}

// TestChatOriginIgnoresProtocolNoise: a `messages` row with no real text/type
// (a delivery receipt or reaction WhatsApp still persists as a row) must not
// count as a real conversation — same realMessageSQL criterion
// ChatJIDsWithMessages already uses, reused here on purpose (T18) so the two
// can't disagree about whether a chat "really happened".
func TestChatOriginIgnoresProtocolNoise(t *testing.T) {
	s := openTestStore(t)
	jid := "666@c.us"
	if err := s.TouchChat(jid, "F", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertGroupMember("group3@g.us", jid, "", 1); err != nil {
		t.Fatal(err)
	}
	// Protocol noise: both Text and Type empty — realMessageSQL excludes it.
	if err := s.AddMessage(Message{ChatJID: jid, ID: "noise1", FromMe: false, Text: "", Type: "", TS: 1}); err != nil {
		t.Fatal(err)
	}

	origin, err := s.ChatOrigin(jid)
	if err != nil {
		t.Fatal(err)
	}
	if origin != "group_discovered" {
		t.Errorf("ChatOrigin with only a content-less message row = %q, want group_discovered (protocol noise isn't a conversation)", origin)
	}
}
