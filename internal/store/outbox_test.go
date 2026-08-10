package store

import "testing"

func TestOutboxDrainRetryBackoffAndDeadLetter(t *testing.T) {
	s := openTestStore(t)

	if err := s.Enqueue("111@c.us", "hola", 100); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	due, err := s.DueOutbox(10, 100)
	if err != nil {
		t.Fatalf("DueOutbox (fresh): %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("got %d due, want 1 freshly-enqueued item", len(due))
	}
	seq := due[0].Seq

	// A failed send sets a backoff deadline — must NOT be due again before it.
	if err := s.SetOutboxRetry(seq, 1, 200, "timeout"); err != nil {
		t.Fatalf("SetOutboxRetry: %v", err)
	}
	due, err = s.DueOutbox(10, 150)
	if err != nil {
		t.Fatalf("DueOutbox (backing off): %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("got %d due before backoff elapsed, want 0", len(due))
	}
	due, err = s.DueOutbox(10, 200)
	if err != nil {
		t.Fatalf("DueOutbox (backoff elapsed): %v", err)
	}
	if len(due) != 1 || due[0].RetryCount != 1 || due[0].LastError != "timeout" {
		t.Fatalf("got %+v, want 1 item with retry_count=1 last_error=timeout", due)
	}

	// Dead-lettered items are excluded from the send loop forever, but stay
	// visible in PendingOutbox for inspection.
	if err := s.DeadLetterOutbox(seq, "gave up"); err != nil {
		t.Fatalf("DeadLetterOutbox: %v", err)
	}
	due, err = s.DueOutbox(10, 999999)
	if err != nil {
		t.Fatalf("DueOutbox (dead-lettered): %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("got %d due after dead-letter, want 0", len(due))
	}
	pending, err := s.PendingOutbox(10)
	if err != nil {
		t.Fatalf("PendingOutbox: %v", err)
	}
	if len(pending) != 1 || !pending[0].DeadLetter {
		t.Fatalf("got %+v, want 1 dead-lettered item still visible", pending)
	}
}

func TestMarkSentExcludesFromOutbox(t *testing.T) {
	s := openTestStore(t)
	if err := s.Enqueue("111@c.us", "hola", 100); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	due, err := s.DueOutbox(10, 100)
	if err != nil || len(due) != 1 {
		t.Fatalf("DueOutbox: due=%+v err=%v", due, err)
	}
	if err := s.MarkSent(due[0].Seq); err != nil {
		t.Fatalf("MarkSent: %v", err)
	}
	due, err = s.DueOutbox(10, 100)
	if err != nil {
		t.Fatalf("DueOutbox after sent: %v", err)
	}
	if len(due) != 0 {
		t.Errorf("got %d due after MarkSent, want 0", len(due))
	}
}

// TestEnqueueStripsDeviceSuffix is T52's outbox-side case
// (ct-2026-08-10-1837): a JID with device suffix must be written to the
// outbox normalized — the drain loop would fail to deliver to
// "555000001:15@s.whatsapp.net" since WhatsApp's gateway never accepts a
// device-qualified number as a conversation destination.
func TestEnqueueStripsDeviceSuffix(t *testing.T) {
	s := openTestStore(t)

	if err := s.Enqueue("555000001:15@s.whatsapp.net", "hola", 100); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	pending, err := s.PendingOutbox(10)
	if err != nil {
		t.Fatalf("PendingOutbox: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("got %d rows, want 1", len(pending))
	}
	if pending[0].ToJID != "555000001@s.whatsapp.net" {
		t.Errorf("ToJID = %q, want 555000001@s.whatsapp.net", pending[0].ToJID)
	}
}

// TestBossJIDsWithGhostProducesOneOutboxRow is T52's end-to-end scenario
// (ct-2026-08-10-1837): with a ghost row (device suffix, is_boss=1) and a
// real row (same number, no suffix, is_boss=1), the full
// BossJIDs → EnqueueFromAgent loop must write exactly one outbox row to
// the normalized JID.
func TestBossJIDsWithGhostProducesOneOutboxRow(t *testing.T) {
	s := openTestStore(t)

	const ghost = "555000001:15@s.whatsapp.net"
	const real = "555000001@s.whatsapp.net"

	if err := s.SetIsBoss(ghost, true); err != nil {
		t.Fatalf("SetIsBoss ghost: %v", err)
	}
	if err := s.SetIsBoss(real, true); err != nil {
		t.Fatalf("SetIsBoss real: %v", err)
	}

	jids, err := s.BossJIDs()
	if err != nil {
		t.Fatalf("BossJIDs: %v", err)
	}
	for _, jid := range jids {
		if err := s.EnqueueFromAgent(jid, "mensaje de agente", 100, "term-1"); err != nil {
			t.Fatalf("EnqueueFromAgent(%s): %v", jid, err)
		}
	}

	pending, err := s.PendingOutbox(10)
	if err != nil {
		t.Fatalf("PendingOutbox: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("got %d outbox rows, want 1: %+v", len(pending), pending)
	}
	if pending[0].ToJID != real {
		t.Errorf("outbox ToJID = %q, want %q", pending[0].ToJID, real)
	}
}
