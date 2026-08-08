// Catálogo de superficie de whatsmeow — qué se usa hoy, qué está listo para
// cablear cuando se pida, y qué se descartó (y por qué). Vive separado del
// conector real (adapter.go/inbound.go/outbound.go) para no mezclar "lo que
// el core efectivamente puede pedir hoy" con "lo que la lib puede hacer".
//
// Nada de este archivo se llama desde el resto del repo todavía — son
// wrappers finos, ya escritos y compilando, para que sumar una feature
// futura sea "cablear el wrapper que ya existe", no "leer los docs de
// whatsmeow de cero". Cablear = exponerlo en gateway.Gateway (si el core
// lo necesita en general) o llamarlo directo desde una tool MCP boss-only
// (si es admin, como ya hacen las 6 de grupo/perfil de openwa).
package whatsmeow

import (
	"context"

	wmeow "go.mau.fi/whatsmeow"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"piumy-gateway/internal/gateway"
)

// ── USADOS (cableados en el conector, Prioridad 1) ──────────────────────
//
// - Start/Stop/Connected/QRChannel — ciclo de vida + primer login (adapter.go).
// - Inbound (events.Message → gateway.Inbound) — inbound.go.
// - Send (SendMessage texto), SetTyping (SendChatPresence), MarkRead
//   (MarkRead), MarkDelivered (no-op) — outbound.go, los 4 métodos que
//   gateway.Gateway expone al core.
// - GetJoinedGroups — llamado y logueado tras *events.Connected
//   (inbound.go: handleConnected) para confirmar que la vía sigue viva;
//   AÚN NO alimenta el store (ver el comentario en handleConnected —
//   judgment call flagueado a Citrino, no decidido acá).

// ── POR USAR (wrappers listos, un llamado cuando se pidan) ──────────────
// Visión del boss: acá van después stickers y llamadas-voz.

// React sends an emoji reaction to msgID in chatJID — reaction="" removes
// a previous reaction. Ready to expose as a Gateway method or a boss-only
// MCP tool (needs the sender JID of the ORIGINAL message, not just the
// chat — same per-message-sender gap noted in outbound.go's MarkRead).
func (a *Adapter) React(ctx context.Context, chatJID, senderJID, msgID, reaction string) error {
	chat, err := types.ParseJID(chatJID)
	if err != nil {
		return err
	}
	sender, err := types.ParseJID(senderJID)
	if err != nil {
		return err
	}
	_, err = a.client.SendMessage(ctx, chat, a.client.BuildReaction(chat, sender, types.MessageID(msgID), reaction))
	return err
}

// SendSticker uploads webp bytes and sends them as a sticker. Uses
// wmeow.MediaImage for the upload — whatsmeow has no separate "single
// sticker" MediaType (only MediaStickerPack, for pack metadata bundles, a
// different concept); stickers are WebP images uploaded the same way as a
// regular image. Unverified against a real send (Priority 2, not smoke-
// tested like the Priority 1 methods) — confirm this the first time it's
// actually wired to something.
func (a *Adapter) SendSticker(ctx context.Context, toJID string, webp []byte) (gateway.SendResult, error) {
	jid, err := types.ParseJID(toJID)
	if err != nil {
		return gateway.SendResult{}, err
	}
	up, err := a.client.Upload(ctx, webp, wmeow.MediaImage)
	if err != nil {
		return gateway.SendResult{}, err
	}
	resp, err := a.client.SendMessage(ctx, jid, &waE2E.Message{
		StickerMessage: &waE2E.StickerMessage{
			URL:           &up.URL,
			DirectPath:    &up.DirectPath,
			MediaKey:      up.MediaKey,
			Mimetype:      proto.String("image/webp"),
			FileEncSHA256: up.FileEncSHA256,
			FileSHA256:    up.FileSHA256,
			FileLength:    &up.FileLength,
		},
	})
	return gateway.SendResult{MsgID: string(resp.ID), TS: resp.Timestamp.Unix()}, err
}

// SendImage uploads image bytes and sends them with an optional caption.
func (a *Adapter) SendImage(ctx context.Context, toJID string, jpeg []byte, caption string) (gateway.SendResult, error) {
	jid, err := types.ParseJID(toJID)
	if err != nil {
		return gateway.SendResult{}, err
	}
	up, err := a.client.Upload(ctx, jpeg, wmeow.MediaImage)
	if err != nil {
		return gateway.SendResult{}, err
	}
	resp, err := a.client.SendMessage(ctx, jid, &waE2E.Message{
		ImageMessage: &waE2E.ImageMessage{
			URL:           &up.URL,
			DirectPath:    &up.DirectPath,
			MediaKey:      up.MediaKey,
			Mimetype:      proto.String("image/jpeg"),
			FileEncSHA256: up.FileEncSHA256,
			FileSHA256:    up.FileSHA256,
			FileLength:    &up.FileLength,
			Caption:       protoStringOrNil(caption),
		},
	})
	return gateway.SendResult{MsgID: string(resp.ID), TS: resp.Timestamp.Unix()}, err
}

// DownloadMedia decrypts and downloads the media in a received message —
// pair with events.Message.Message (the raw proto) from Inbound's source
// event, which piumy-gateway's gateway.Inbound doesn't carry today (F4d's
// media-by-path model was designed against open-wa's webhook, not this).
func (a *Adapter) DownloadMedia(ctx context.Context, msg *waE2E.Message) ([]byte, error) {
	return a.client.DownloadAny(ctx, msg)
}

// EditMessage / DeleteMessage: whatsmeow already exposes these as direct,
// single-call client methods — no wrapper needed, just call them with a
// parsed JID:
//
//	client.SendMessage(ctx, chat, client.BuildEdit(chat, msgID, newContent))
//	client.RevokeMessage(ctx, chat, msgID)

// IsOnWhatsApp checks whether phone numbers (no JID suffix, just digits)
// have WhatsApp accounts — useful before a first outbound to a number
// nobody has messaged yet.
func (a *Adapter) IsOnWhatsApp(ctx context.Context, phones []string) ([]types.IsOnWhatsAppResponse, error) {
	return a.client.IsOnWhatsApp(ctx, phones)
}

// Group/profile admin — cabled into mcpserver/group_tools.go (ST-E,
// ct-2026-07-11-1444) via the GroupProfile interface (mcpserver/
// server.go's Deps.GroupProfile — *Adapter satisfies it). Boss-only,
// direct client calls, no gateway.Gateway seam (group admin isn't a
// cross-vendor concept the core needs to know about — a future adapter
// could handle groups completely differently).
func (a *Adapter) CreateGroup(ctx context.Context, name string, participantJIDs []string) (*types.GroupInfo, error) {
	participants := make([]types.JID, 0, len(participantJIDs))
	for _, p := range participantJIDs {
		jid, err := types.ParseJID(p)
		if err != nil {
			return nil, err
		}
		participants = append(participants, jid)
	}
	return a.client.CreateGroup(ctx, wmeow.ReqCreateGroup{Name: name, Participants: participants})
}

func (a *Adapter) AddParticipant(ctx context.Context, groupJID, participantJID string) ([]types.GroupParticipant, error) {
	group, err := types.ParseJID(groupJID)
	if err != nil {
		return nil, err
	}
	participant, err := types.ParseJID(participantJID)
	if err != nil {
		return nil, err
	}
	return a.client.UpdateGroupParticipants(ctx, group, []types.JID{participant}, wmeow.ParticipantChangeAdd)
}

func (a *Adapter) SetGroupPhoto(ctx context.Context, groupJID string, jpeg []byte) (string, error) {
	jid, err := types.ParseJID(groupJID)
	if err != nil {
		return "", err
	}
	return a.client.SetGroupPhoto(ctx, jid, jpeg)
}

func (a *Adapter) SetGroupDescription(ctx context.Context, groupJID, description string) error {
	jid, err := types.ParseJID(groupJID)
	if err != nil {
		return err
	}
	return a.client.SetGroupDescription(ctx, jid, description)
}

// SetProfileStatus sets the host number's own status/"About" text (boss's
// decision A, ct-2026-07-11-1444) — NOT the display name, whatsmeow has no
// API for that. Wraps SetStatusMessage 1:1, same wrapper shape as the group
// methods above.
func (a *Adapter) SetProfileStatus(ctx context.Context, status string) error {
	return a.client.SetStatusMessage(ctx, types.SetStatusInput{Text: &status})
}

// ── NO USAR (evaluado y descartado, con motivo) ─────────────────────────
//
// - Newsletters (NewsletterSendReaction, UploadNewsletter*, etc.) —
//   piumy-gateway media un WhatsApp personal/de dueño, no un canal de
//   difusión masiva; fuera de la visión del producto.
// - Calls (no hay API de voz/video en whatsmeow — es texto/media, no
//   señalización de llamada) — el boss mencionó llamadas-voz como visión
//   futura, pero eso NO es esta librería; sería un componente aparte.
// - Polls — no hay pedido del boss ni caso de uso del gateway hoy.
// - FB downloads (DownloadFB/DownloadFBToFile) — mecanismo interno para
//   media servida desde infraestructura de Facebook/Meta específica
//   (ciertos stickers/GIFs), no aplica al camino normal de media.
// - Proxy (client.SetProxy y afines) — piumy-gateway corre en la red del
//   dueño; no hay caso de uso de proxy todavía, YAGNI hasta que aparezca.
// - Sticker-packs (creación/publicación de packs, distinto de ENVIAR un
//   sticker suelto, que sí está en "por usar") — gestión de contenido, no
//   parte de ser un gateway de mensajería.

// protoStringOrNil is proto.String, but "" means "field absent" (an empty
// caption should omit ImageMessage.Caption entirely, not send an empty
// string) — the one bit proto.String itself doesn't cover.
func protoStringOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return proto.String(s)
}
