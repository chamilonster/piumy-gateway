package store

// GroupMember is one row of group_members — a group's participant, with
// whatever name is known for them (ct-2026-07-19-0102, backup — "scrapear
// los numeros de los miembros de los grupos", boss verbatim; the backfill,
// Sub 2, populates this).
type GroupMember struct {
	GroupJID   string `json:"group_jid"`
	MemberJID  string `json:"member_jid"`
	MemberName string `json:"member_name,omitempty"`
	AddedTS    int64  `json:"added_ts"`
}

// UpsertGroupMember records/refreshes one group_members row — carries the
// member's name + when it was learned, keyed group_jid-first (the
// backfill's own access pattern, "for this group, list its members"). A
// re-scrape only overwrites memberName when the new one is non-empty, so a
// later lookup that comes back blank never erases a name already known.
//
// T18B (ct-2026-08-05-1243, follow-up): the ONLY writer of group
// membership now — chat_groups (AddGroupMember/GroupsOf's old backing
// table) is retired. It never had a production writer of its own (only
// group_members did, via seedGroups/whatsmeow.inbound.go), so the two
// tables' answers could never actually agree; keeping one instead of
// syncing two is the fix, not a workaround.
func (s *Store) UpsertGroupMember(groupJID, memberJID, memberName string, addedTS int64) error {
	_, err := s.db.Exec(`INSERT INTO group_members (group_jid, member_jid, member_name, added_ts) VALUES (?, ?, ?, ?)
		ON CONFLICT(group_jid, member_jid) DO UPDATE SET
			member_name = CASE WHEN excluded.member_name != '' THEN excluded.member_name ELSE group_members.member_name END,
			added_ts = excluded.added_ts`,
		groupJID, memberJID, memberName, addedTS)
	return err
}

// ListGroupMembers returns every known member of groupJID.
func (s *Store) ListGroupMembers(groupJID string) ([]GroupMember, error) {
	rows, err := s.db.Query(`SELECT group_jid, member_jid, COALESCE(member_name,''), added_ts
		FROM group_members WHERE group_jid = ? ORDER BY added_ts`, groupJID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GroupMember{}
	for rows.Next() {
		var m GroupMember
		if err := rows.Scan(&m.GroupJID, &m.MemberJID, &m.MemberName, &m.AddedTS); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListAllGroupMembers returns EVERY known group_members row, across every
// group — the dashboard's collapsible group zone (S1g, ct-2026-07-19-1801)
// needs all of them at once (one page, every group rendered), so this is
// one query instead of ListGroupMembers called once per group (N+1).
func (s *Store) ListAllGroupMembers() ([]GroupMember, error) {
	rows, err := s.db.Query(`SELECT group_jid, member_jid, COALESCE(member_name,''), added_ts
		FROM group_members ORDER BY group_jid, added_ts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GroupMember{}
	for rows.Next() {
		var m GroupMember
		if err := rows.Scan(&m.GroupJID, &m.MemberJID, &m.MemberName, &m.AddedTS); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GroupsOf returns the group JIDs memberJID is known to participate in —
// the reverse of ListGroupMembers ("for this group, who's in it" vs. "for
// this number, which groups"), same group_members table.
//
// T18B (ct-2026-08-05-1243, follow-up): used to read chat_groups (written
// by AddGroupMember, now retired — see UpsertGroupMember's doc).
// group_members' own PRIMARY KEY (group_jid, member_jid) already guarantees
// one row per pair, so a re-seed can't produce a duplicate group_jid here.
func (s *Store) GroupsOf(memberJID string) ([]string, error) {
	rows, err := s.db.Query(`SELECT group_jid FROM group_members WHERE member_jid = ?`, memberJID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
