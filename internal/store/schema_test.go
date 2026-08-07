package store

import (
	"path/filepath"
	"testing"
)

// TestOpenCreatesSchemaAndIsIdempotent covers the smoke DoD (7 tables exist
// after Open) plus migrate()'s idempotency contract (a second Open on the
// same file must not error on "duplicate column name").
func TestOpenCreatesSchemaAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "piumy.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	wantTables := []string{"chats", "messages", "outbox", "kv", "media", "drafts", "agents"}
	for _, tbl := range wantTables {
		var name string
		if err := s.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&name); err != nil {
			t.Errorf("table %q missing: %v", tbl, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen: migrate() must be a no-op, not an error, on an already-migrated DB.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open (migrate idempotency): %v", err)
	}
	if err := s2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestMigrateMimeInTextSwapsCorruptedRows is the regression for
// ct-2026-07-21-1727: a pre-fix build's handleMessage stored a media
// message's mime in messages.text and its caption in messages.type
// (swapped). Any row with a matching `media` row whose mime equals the
// corrupted text must come back swapped — text=caption (here empty),
// type=mime — so store.MediaKind(type) recognizes it again. An ordinary
// text message that happens to contain a slash, with no matching media
// row, must be left untouched (the EXISTS guard is what tells the two
// apart).
func TestMigrateMimeInTextSwapsCorruptedRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "piumy.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.AddMessage(Message{ChatJID: "1@lid", ID: "M1", Text: "image/jpeg", Type: "", TS: 100}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMedia(Media{MsgID: "M1", ChatJID: "1@lid", Path: "p", FullPath: "p", Mime: "image/jpeg", TS: 100}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMessage(Message{ChatJID: "1@lid", ID: "M2", Text: "no es un mime, solo 1/3 de la torta", Type: "text", TS: 200}); err != nil {
		t.Fatal(err)
	}

	if err := migrateMimeInText(s.db); err != nil {
		t.Fatal(err)
	}

	byID := func(id string) Message {
		t.Helper()
		msgs, err := s.GetMessages("1@lid", 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range msgs {
			if m.ID == id {
				return m
			}
		}
		t.Fatalf("message %q not found", id)
		return Message{}
	}

	fixed := byID("M1")
	if fixed.Text != "" || fixed.Type != "image/jpeg" {
		t.Errorf("M1 = %+v, want Text=\"\" Type=image/jpeg", fixed)
	}

	untouched := byID("M2")
	if untouched.Text != "no es un mime, solo 1/3 de la torta" || untouched.Type != "text" {
		t.Errorf("M2 = %+v, want left untouched (no matching media row)", untouched)
	}

	// Idempotent: a second run must not re-corrupt the already-fixed row.
	if err := migrateMimeInText(s.db); err != nil {
		t.Fatal(err)
	}
	fixed2 := byID("M1")
	if fixed2.Text != "" || fixed2.Type != "image/jpeg" {
		t.Errorf("M1 after second migrate run = %+v, want unchanged", fixed2)
	}
}
