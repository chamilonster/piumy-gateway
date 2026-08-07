package corepipeline

import (
	"context"
	"log"
	"time"

	"piumy-gateway/internal/gateway"
	"piumy-gateway/internal/governor"
	"piumy-gateway/internal/state"
	"piumy-gateway/internal/store"
)

// killSwitchActive is true if either half of the kill switch is set.
// H2+H3 hardening (ct-2026-07-10-0540): set_kill_switch (mcpserver/restapi)
// flips gov.Killed and state.Muted together, but they're still two
// independent flags — check both rather than trust they never diverge.
func (p *Pipeline) killSwitchActive() bool {
	return p.gov.Killed() || p.state.Snapshot().Muted
}

// processOutbox drains due outbox items through the gateway, respecting the
// anti-ban governor and human pacing. No-op while the gateway reports
// disconnected.
func (p *Pipeline) processOutbox(ctx context.Context) {
	if !p.gw.Connected() {
		return
	}

	pending, err := p.store.DueOutbox(10, time.Now().Unix())
	if err != nil {
		log.Printf("corepipeline: pending outbox: %v", err)
		return
	}

	for _, item := range pending {
		if ctx.Err() != nil {
			return
		}
		if p.killSwitchActive() {
			log.Println("corepipeline: kill switch active, skipping outbox")
			return
		}
		if !p.gov.Allow() {
			log.Println("corepipeline: rate-limited, deferring outbox")
			return
		}

		if !validJID(item.ToJID) {
			log.Printf("corepipeline: invalid JID %q — skipping", item.ToJID)
			if err := p.store.MarkSent(item.Seq); err != nil {
				log.Printf("corepipeline: mark sent (bad JID): %v", err)
			}
			continue
		}

		// Human pacing: wait a randomized delay, then re-check the kill
		// switch (it may have flipped while we were sleeping).
		select {
		case <-ctx.Done():
			return
		case <-time.After(p.dispatchDelay().Random()):
		}
		if p.killSwitchActive() {
			return
		}

		if err := p.gw.SetTyping(ctx, item.ToJID, true); err != nil {
			log.Printf("corepipeline: set typing: %v", err)
		}

		composing := governor.DelayWindow{Min: p.cfg.ComposingMin, Max: p.cfg.ComposingMax}
		select {
		case <-ctx.Done():
			return
		case <-time.After(composing.Random()):
		}

		res, sendErr := p.gw.Send(ctx, item.ToJID, item.Text)

		if err := p.gw.SetTyping(ctx, item.ToJID, false); err != nil {
			log.Printf("corepipeline: clear typing: %v", err)
		}

		if sendErr != nil {
			log.Printf("corepipeline: send seq=%d: %v", item.Seq, sendErr)
			p.retryOrDeadLetter(item, sendErr)
			continue
		}

		if err := p.store.MarkSent(item.Seq); err != nil {
			log.Printf("corepipeline: mark sent seq=%d: %v", item.Seq, err)
		}

		// Record the outbound message with the client's real id (so
		// delivery/read receipts, which reference that id, can match it
		// later) and its model attribution.
		if err := p.store.AddMessage(sentMessageRow(item.ToJID, item, res)); err != nil {
			log.Printf("corepipeline: store sent message seq=%d: %v", item.Seq, err)
		}

		// ST-D (ct-2026-07-11-074139): the ONE metering point for every real
		// WhatsApp send, whatever queued it — send_message/draft (MCP),
		// approve_draft, autoreply's auto-send, and a plain outbox item all
		// converge here via Enqueue/EnqueueWithModel; processOutbox is the
		// only caller of gw.Send in the whole codebase (verified). usage
		// now means "what actually went out", not "what an agent attempted"
		// — a held-then-discarded draft was never metered even before this
		// fix and still isn't; that's correct, it never left the outbox.
		if err := p.store.AddUsage(item.ToJID, store.Today(), store.UsageDelta{OutChars: len(item.Text), Messages: 1}); err != nil {
			log.Printf("corepipeline: meter output seq=%d: %v", item.Seq, err)
		}

		// Sent counter: recomputed from the messages table (never a
		// separately incremented counter — see state.Status.Sent's doc
		// comment for why that drifts on a retry/crash mismatch).
		if sent, err := p.store.CountOutboundSince(0); err != nil {
			log.Printf("corepipeline: count sent: %v", err)
		} else {
			_ = p.state.Update(func(s *state.Status) { s.Sent = sent })
		}

		log.Printf("corepipeline: sent outbox seq=%d to %s", item.Seq, item.ToJID)
	}
}

// retryOrDeadLetter is the anti-ban core: a failed send must NEVER just
// retry again next tick forever (a resend loop). Bump retry_count and back
// off exponentially; once retry_count reaches cfg.OutboxMaxRetry,
// dead-letter the item instead (out of the send loop for good, never
// deleted — stays for inspection).
func (p *Pipeline) retryOrDeadLetter(item store.Outbox, sendErr error) {
	retryCount := item.RetryCount + 1
	if retryCount >= p.cfg.OutboxMaxRetry {
		if err := p.store.DeadLetterOutbox(item.Seq, sendErr.Error()); err != nil {
			log.Printf("corepipeline: dead-letter seq=%d: %v", item.Seq, err)
			return
		}
		log.Printf("corepipeline: outbox seq=%d dead-lettered after %d failures", item.Seq, retryCount)
		return
	}
	nextRetry := time.Now().Add(exponentialBackoff(retryCount)).Unix()
	if err := p.store.SetOutboxRetry(item.Seq, retryCount, nextRetry, sendErr.Error()); err != nil {
		log.Printf("corepipeline: set outbox retry seq=%d: %v", item.Seq, err)
	}
}

// exponentialBackoff returns 5s * 2^(retryCount-1) capped at 1h. Anti-ban:
// always > 0, never instant; retryCount is always >= 1 when called.
func exponentialBackoff(retryCount int) time.Duration {
	const base = 5 * time.Second
	const maxBackoff = time.Hour
	backoff := base
	for i := 1; i < retryCount; i++ {
		backoff *= 2
		if backoff > maxBackoff {
			return maxBackoff
		}
	}
	return backoff
}

// sentMessageRow builds the messages row for a successfully sent outbox
// item, using the client's real response (id + timestamp) rather than
// anything guessed at enqueue time — so delivery/read receipts, which
// reference that id, can match it later.
func sentMessageRow(chatJID string, item store.Outbox, res gateway.SendResult) store.Message {
	return store.Message{
		ChatJID: chatJID,
		ID:      res.MsgID,
		FromMe:  true,
		Text:    item.Text,
		TS:      res.TS,
		Type:    "text",
		Model:   item.Model,
	}
}

// validJID rejects only the one invalid case this agnostic layer can judge
// without knowing any vendor's JID format: empty. Deeper format validation
// belongs to the adapter (F3), not the core.
func validJID(jid string) bool {
	return jid != ""
}
