package store

// Pending is a chat waiting for a reply: its most recent message is inbound
// (from_me=0) — the contact has the last word. The golden rule: the agent
// must NOT always have the last word, so a chat where WE spoke last is never
// pending, no matter how long ago that was.
type Pending struct {
	JID     string `json:"jid"`
	Name    string `json:"name"`
	Origin  string `json:"origin"`
	Mode    string `json:"mode"`
	Status  string `json:"status"`
	Active  bool   `json:"active"`
	LastTS  int64  `json:"last_ts"`
	AgeSec  int64  `json:"age_sec"`
	Preview string `json:"preview"`

	// ClaimedBy / ClaimedUntil: same effective (already-expiry-resolved)
	// claim state as Chat's — see its doc comment.
	ClaimedBy    string `json:"claimed_by,omitempty"`
	ClaimedUntil int64  `json:"claimed_until,omitempty"`
}

// PendingChats returns chats whose most recent message is inbound, oldest
// first (longest-waiting first — the age the agent should weigh most when
// judging what to attend to). now is the reference timestamp for AgeSec
// (pass time.Now().Unix()).
func (s *Store) PendingChats(limit int, now int64) ([]Pending, error) {
	if limit <= 0 {
		limit = 20
	}
	// A chat's most recent message = the one with the largest (ts, rowid).
	// Matching m.rowid against that per-chat max picks exactly the latest
	// message per chat_jid; filtering to from_me=0 then keeps only chats
	// where the contact — not us — has the last word.
	rows, err := s.db.Query(`
		SELECT m.chat_jid, COALESCE(c.name,''), c.mode, c.status, c.active, m.ts, COALESCE(m.text,''),
			COALESCE(c.claimed_by,''), c.claimed_until
		FROM messages m
		JOIN chats c ON c.jid = m.chat_jid
		WHERE m.from_me = 0
		AND m.rowid = (
			SELECT m2.rowid FROM messages m2
			WHERE m2.chat_jid = m.chat_jid
			ORDER BY m2.ts DESC, m2.rowid DESC LIMIT 1
		)
		ORDER BY m.ts ASC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Pending{}
	for rows.Next() {
		var p Pending
		var active int
		if err := rows.Scan(&p.JID, &p.Name, &p.Mode, &p.Status, &active, &p.LastTS, &p.Preview,
			&p.ClaimedBy, &p.ClaimedUntil); err != nil {
			return nil, err
		}
		p.Active = active != 0
		if len(p.Preview) > 80 {
			p.Preview = p.Preview[:80] + "…"
		}
		p.AgeSec = now - p.LastTS
		// Always inbound_spoke here: the WHERE clause above already requires
		// an inbound message to exist (from_me=0), which is exactly what
		// makes ChatOrigin return inbound_spoke — no extra query needed.
		p.Origin = "inbound_spoke"
		p.ClaimedBy, p.ClaimedUntil = effectiveClaim(p.ClaimedBy, p.ClaimedUntil, now)
		out = append(out, p)
	}
	return out, rows.Err()
}

// PendingDedicated returns incoming messages not yet handled, from any chat
// the owner hasn't explicitly silenced (T5, ct-2026-08-05-0311, boss
// verbatim: "todos los mensajes automaticos y por confirmacion deben
// empezar a entrar solos") — entering the agent and being sent are two
// different things: mode ('dedicated' or 'auto') and confirmation_mode
// decide how a reply goes OUT (send_message's own gate), never whether the
// agent gets to see the message at all. Before this, an 'auto' chat only
// went to internal/autoreply — inert by default (PIUMY_BRIDGE=none) and
// off in production, so an 'auto' chat with nobody bridging it went
// unanswered with nothing to show for it. Excludes chats whose config_level
// is "unattended" or "ignored" (ConfigLevel: active=false or
// status='ignored') — ct-2026-07-21-1853: dispatch must honor the same
// silence the level already implies elsewhere, not just show it in the UI.
// is_boss is an unconditional bypass of that gate, mirroring ConfigLevel's
// own precedence (IsBoss is its first, unconditional case): the owner's
// chat is always "boss" level and must always dispatch, even if it was
// never explicitly activated (active=false is the schema default, and the
// boss's own chat commonly never goes through set_chat_active/
// set_config_level).
func (s *Store) PendingDedicated(limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(`SELECT `+messageColumns+`
		FROM messages m
		JOIN chats c ON c.jid = m.chat_jid
		WHERE m.from_me = 0 AND m.handled = 0 AND c.mode IN ('dedicated', 'auto')
		AND (c.is_boss = 1 OR (c.active = 1 AND c.status != 'ignored'))
		ORDER BY m.ts ASC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	return scanMessages(rows)
}

func (s *Store) MarkHandled(jid, id string) error {
	_, err := s.db.Exec(`UPDATE messages SET handled = 1 WHERE chat_jid = ? AND id = ?`, jid, id)
	return err
}

// MarkHandledBefore marks all unhandled inbound messages for chatJID with
// ts <= tsResp as handled — called automatically by send_message and
// approve_draft (ct-2026-07-13-2105) so the gate releases without a
// manual mark_handled call. Bounded by ts (not blind) so a message that
// arrived WHILE the agent was composing its reply isn't swallowed.
func (s *Store) MarkHandledBefore(chatJID string, tsResp int64) error {
	_, err := s.db.Exec(`UPDATE messages SET handled = 1
		WHERE chat_jid = ? AND from_me = 0 AND handled = 0 AND ts <= ?`, chatJID, tsResp)
	return err
}

// MarkPendingBefore reverses MarkHandledBefore for chatJID's inbound
// messages with ts <= tsResp — called when a draft answering them is
// rejected (T15, ct-2026-08-05-123241): the underlying request genuinely is
// still unanswered, so it re-enters PendingDedicated exactly like a fresh
// unhandled message (honest state, not a special "rejected" side-channel)
// and rides the existing capipush sweep/backoff/routing machinery for the
// redispatch — no separate mechanism needed.
func (s *Store) MarkPendingBefore(chatJID string, tsResp int64) error {
	_, err := s.db.Exec(`UPDATE messages SET handled = 0
		WHERE chat_jid = ? AND from_me = 0 AND handled = 1 AND ts <= ?`, chatJID, tsResp)
	return err
}

// CountPendingDedicated returns the total number of unhandled inbound
// messages PendingDedicated would return — same mode/config_level gate,
// kept in lockstep (T5): a mismatch here would make the queue count lie
// about what's actually pending.
func (s *Store) CountPendingDedicated() (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM messages m
		JOIN chats c ON c.jid = m.chat_jid
		WHERE m.from_me = 0 AND m.handled = 0 AND c.mode IN ('dedicated', 'auto')
		AND (c.is_boss = 1 OR (c.active = 1 AND c.status != 'ignored'))`).Scan(&count)
	return count, err
}

// CountRecentPendingNonBoss is capipush's own backpressure count (S3,
// ct-2026-07-30-030948): unlike CountPendingDedicated, this deliberately
// EXCLUDES is_boss (a boss message storm must never throttle everyone
// else — the same unconditional bypass PendingDedicated already gives
// is_boss, applied here too, per Citrino's call not to invent a second
// criterion) and only counts messages with ts >= sinceTS — old debt outside
// the window (a chat nobody's going to answer, months of backlog) must
// never count toward "is the channel under pressure RIGHT NOW".
//
// mode IN ('dedicated', 'auto') (T20, ct-2026-08-05-1301): kept in lockstep
// with PendingDedicated/CountPendingDedicated's own mode gate — T5
// (ct-2026-08-05-0311) widened what DISPATCHES to include 'auto' chats but
// missed this THIRD query with the same filter (Amatista's R1 catch). Before
// this fix, an avalanche of 'auto' chats was invisible to the backpressure
// counter — the dispatch loop would keep pushing them through unthrottled
// even while genuinely swamped, since the count it checks against never saw
// them. Doesn't open the send gate itself (send.go's checks are untouched) —
// the semáforo just lied by omission about how loaded the channel actually was.
func (s *Store) CountRecentPendingNonBoss(sinceTS int64) (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM messages m
		JOIN chats c ON c.jid = m.chat_jid
		WHERE m.from_me = 0 AND m.handled = 0 AND c.mode IN ('dedicated', 'auto')
		AND c.is_boss = 0 AND c.active = 1 AND c.status != 'ignored'
		AND m.ts >= ?`, sinceTS).Scan(&count)
	return count, err
}

// CountOutboundSince counts outbound (from_me=1) messages sent at or after
// ts. Used to reconstruct the governor's daily send count across a restart:
// the daily anti-ban cap must survive a power cut — an in-memory-only
// counter would silently reset to 0 and let the bot blow past its daily
// limit right after a crash/reboot.
func (s *Store) CountOutboundSince(ts int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE from_me = 1 AND ts >= ?`, ts).Scan(&n)
	return n, err
}
