package whatsmeow

import (
	"context"
	"fmt"
	"time"

	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"piumy-gateway/internal/gateway"
)

// Send delivers text via whatsmeow's own SendMessage — the ONLY send path.
// The core (corepipeline's outbox drain, paced by governor) decides WHEN
// to call this; this adapter never sends on its own (anti-ban rule, see
// package doc).
func (a *Adapter) Send(ctx context.Context, toJID, text string) (gateway.SendResult, error) {
	jid, err := types.ParseJID(toJID)
	if err != nil {
		return gateway.SendResult{}, fmt.Errorf("whatsmeow: parse JID %q: %w", toJID, err)
	}
	resp, err := a.client.SendMessage(ctx, jid, &waE2E.Message{Conversation: proto.String(text)})
	if err != nil {
		return gateway.SendResult{}, err
	}
	return gateway.SendResult{MsgID: string(resp.ID), TS: resp.Timestamp.Unix()}, nil
}

// SetTyping toggles the composing indicator via SendChatPresence.
func (a *Adapter) SetTyping(ctx context.Context, toJID string, on bool) error {
	jid, err := types.ParseJID(toJID)
	if err != nil {
		return fmt.Errorf("whatsmeow: parse JID %q: %w", toJID, err)
	}
	state := types.ChatPresencePaused
	if on {
		state = types.ChatPresenceComposing
	}
	return a.client.SendChatPresence(ctx, jid, state, types.ChatPresenceMediaText)
}

// MarkRead sends read receipts for msgIDs in chatJID.
//
// Judgment call (flagged, not silently decided): whatsmeow's MarkRead
// wants the ORIGINAL SENDER of each message (matters for groups — the
// receipt is technically addressed per-sender, not per-chat), but
// gateway.Gateway's MarkRead only carries chatJID + message IDs, no
// per-message sender (same shape openwa's adapter already had to live
// with). For a 1:1 chat sender==chat, so this is exactly correct there;
// for a group, this passes the chat JID as sender too — a best-effort
// simplification, not a protocol violation the network rejects, but
// worth revisiting if group read receipts ever look wrong in practice.
func (a *Adapter) MarkRead(ctx context.Context, chatJID string, msgIDs []string) error {
	jid, err := types.ParseJID(chatJID)
	if err != nil {
		return fmt.Errorf("whatsmeow: parse JID %q: %w", chatJID, err)
	}
	ids := make([]types.MessageID, len(msgIDs))
	for i, id := range msgIDs {
		ids[i] = types.MessageID(id)
	}
	return a.client.MarkRead(ctx, ids, time.Now(), jid, jid)
}

// MarkDelivered is a no-op: WhatsApp's own protocol sends delivery
// receipts automatically as part of receiving a message — whatsmeow does
// this internally, there's no separate "ack delivery" call to make. Kept
// on the seam for parity with openwa's adapter (gateway.Gateway doc).
func (a *Adapter) MarkDelivered(ctx context.Context, chatJID string, msgIDs []string) error {
	return nil
}
