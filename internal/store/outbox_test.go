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
