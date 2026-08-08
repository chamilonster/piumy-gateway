package whatsmeow

import (
	"context"
	"log"
	"time"

	"go.mau.fi/whatsmeow/appstate"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/reflect/protoreflect"

	"piumy-gateway/internal/eventbus"
	"piumy-gateway/internal/gateway"
	"piumy-gateway/internal/state"
)

// isSelfChat checks if the chat JID is our own number (same User+Server).
func isSelfChat(chat, own types.JID) bool {
	return chat.User == own.User && chat.Server == own.Server
}

// isOwnDevice checks if the sender IS our own whatsmeow device (same
// User+Server+Device) — used to distinguish the gateway's own echo from
// another-device notes in Note to Self (Pieza D, ct-2026-07-24-0527).
func isOwnDevice(sender, own types.JID) bool {
	return sender.User == own.User && sender.Server == own.Server && sender.Device == own.Device
}

// handleEvent is whatsmeow's single AddEventHandler callback (registered
// once in New) — every event the client emits passes through here.
//
// H6 hardening (ct-2026-07-10-0540): before this, only Connected/Message
// were handled — a deauthed/banned/replaced session went "silently dead":
// whatsmeow logged one line internally and the rest of the gateway (state,
// eventbus, send_message's caller) never found out. LoggedOut/TemporaryBan
// additionally trip the H2/H3 kill switch — the session cannot recover on
// its own, so further sends must stop until the owner investigates and
// re-pairs. StreamReplaced/ClientOutdated/Disconnected only update the
// display + notify (Disconnected in particular is whatsmeow's own
// auto-reconnecting case, not in its PermanentDisconnect set — no kill).
func (a *Adapter) handleEvent(evt any) {
	switch v := evt.(type) {
	case *events.Connected:
		a.handleConnected()
	case *events.Message:
		a.handleMessage(v)
	case *events.HistorySync:
		// Backgrounded (ct-2026-07-19-0148, backup Sub 3): a single
		// HistorySync can carry thousands of messages — never block
		// whatsmeow's own event dispatcher on processing it.
		go a.handleHistorySync(v)
	case *events.LoggedOut:
		a.handleDisconnect("logged out from WhatsApp ("+v.Reason.String()+")", true)
	case *events.TemporaryBan:
		a.handleDisconnect(v.String(), true)
	case *events.StreamReplaced:
		a.handleDisconnect("stream replaced — another session connected with the same credentials", false)
	case *events.ClientOutdated:
		a.handleDisconnect("client outdated — whatsmeow library needs an update", false)
	case *events.Disconnected:
		a.handleDisconnect("disconnected", false)
	case *events.PushNameSetting:
		// ct-2026-07-24-0041: despues de un pareo fresco, whatsmeow
		// setea Store.PushName asincronicamente (appstate.go:367) —
		// handleConnected se ejecuta antes de que llegue, dejando
		// name="" en GET /api/status. Escuchar PushNameSetting
		// refresca el estado cuando el nombre finalmente llega.
		a.recordOwnIdentity()
	case *events.Receipt:
		// ct-2026-08-07, caso real: un contacto real recibió un mensaje
		// nuestro ilegible (WhatsApp le mostró "Esperando mensaje" en vez
		// del texto) y el gateway nunca se enteró — se supo un día
		// después, por una captura de pantalla. types.ReceiptTypeRetry es
		// exactamente esta señal (documentado por la propia librería: "the
		// message was delivered to the device, but decrypting the message
		// failed") y whatsmeow ya la despachaba antes de este cambio —
		// solo faltaba escucharla. *events.Receipt llega para TODO tipo de
		// acuse (entregado, leído, etc.); filtrar acá evita convertir esto
		// en el mismo ruido que hoy nos esconde la señal real.
		if v.Type == types.ReceiptTypeRetry {
			a.handleRetryReceipt(v)
		}
	case *events.AppStateSyncComplete:
		// ct-2026-07-31 ("no llegan contactos en una instalación nueva"):
		// whatsmeow fires this once a given app-state collection finishes a
		// FULL sync (appstate.go — only on fullSync, i.e. the collection's
		// very first sync since pairing, or a forced resync; never on a
		// routine incremental patch). appstate.WAPatchCriticalUnblockLow is
		// the collection whatsmeow's own source comments document as "the
		// user's contact list" (appstate/keys.go) — verified by reading the
		// library, not assumed (Citrino's own instruction: the obvious guess
		// was "regular", which is wrong — that collection is local chat
		// settings like mute/starred, not contacts). This is the real
		// signal that client.Store.Contacts just got its initial fill.
		if v.Name == appstate.WAPatchCriticalUnblockLow {
			a.scheduleContactsSync()
		}
	}
}

// handleDisconnect is the shared path for every "connection is no longer
// good" event: log at a severity matching kill, mark the display state
// (mood=error, WAConnected=false), and nudge the eventbus. kill=true
// additionally trips the H2/H3 kill switch (governor.SetKill +
// state.Muted, kept together — see corepipeline.killSwitchActive's doc for
// why they must move as a pair) — LoggedOut/TemporaryBan mean the account
// cannot recover by itself, so sends must stop until the owner intervenes.
// Deliberately does NOT auto-clear the kill switch on a later reconnect
// (see handleConnected) — that's an owner decision (set_kill_switch), never
// implicit in a network event.
func (a *Adapter) handleDisconnect(reason string, kill bool) {
	if kill {
		log.Printf("whatsmeow: FATAL — %s — tripping kill switch", reason)
	} else {
		log.Printf("whatsmeow: %s", reason)
	}
	if a.state != nil {
		_ = a.state.UpdateMood(func(s *state.Status) {
			s.Mood = "error"
			s.WAConnected = false
			if kill {
				s.Muted = true
			}
		})
	}
	if kill && a.governor != nil {
		a.governor.SetKill(true)
	}
	if a.bus != nil {
		a.bus.Publish(eventbus.Event{Type: "wa_disconnected", TS: time.Now().Unix()})
	}
}

// handleConnected seeds the store with every group the account has
// already joined — THE fix for the boss's original complaint (a group
// with no inbound history never showed up in list_chats/get_chat_groups,
// because the store only ever learned about a chat reactively, from
// handleMessage's TouchChat on an inbound message). GetJoinedGroups is a
// live request to WhatsApp's servers and only works once actually
// connected (the smoke test's own veredicto, ct-2026-07-10-0338) — never
// call it before *events.Connected fires.
//
// TouchChat(jid, name, ts) is called for every joined group regardless of
// whether it's ever sent a message — TouchChat's own upsert takes
// MAX(last_ts) on conflict (store/chat.go), so this never regresses a
// chat's last-seen time backwards for one that's already active. SetMode
// mirrors corepipeline.handleInbound's own pattern (router.Resolve(jid)
// .Mode) so a freshly-seeded group respects router.json the same way an
// inbound-message-seeded one would.
func (a *Adapter) handleConnected() {
	log.Println("whatsmeow: connected")
	a.clearErrorState()
	a.recordOwnIdentity()
	// One-shot initial contact backfill (ct-2026-07-19-0115, backup Sub 2a) —
	// paced with actionDelay, runs in the background so handleConnected
	// (called synchronously from whatsmeow's event dispatcher) never blocks
	// on a potentially long, deliberately slow sweep. Placed before the
	// groups fetch below so a GetJoinedGroups failure never skips it.
	go a.syncContacts(a.context())
	groups, err := a.client.GetJoinedGroups(context.Background())
	if err != nil {
		log.Printf("whatsmeow: GetJoinedGroups after connect: %v", err)
		return
	}
	log.Printf("whatsmeow: %d joined groups known at connect", len(groups))
	a.seedGroups(groups)
}

// KickResync (D4, ct-2026-07-22-2100 — the boss's "partir de 0" smoke-test
// reset) re-runs the SAME two triggers handleConnected fires on a real
// (re)connect — syncContacts + GetJoinedGroups/seedGroups — as an explicit
// kick after a DB reset. Needed because the WhatsApp session normally
// STAYS connected through a reset (nothing else touches whatsmeow.db/the
// pairing), so there's no natural reconnect event to re-populate
// chats/group_members on its own; without this, the next repopulation
// would only happen at syncLoop's next periodic tick (up to 6h later),
// defeating the point of an immediate "partir de 0". Reuses handleConnected's
// own logic verbatim rather than duplicating it — no new sync machinery.
// No-op if not connected (reset without a live session has nothing to
// re-sync from).
func (a *Adapter) KickResync() {
	if a.client == nil || !a.client.IsConnected() {
		return
	}
	go a.syncContacts(a.context())
	groups, err := a.client.GetJoinedGroups(context.Background())
	if err != nil {
		log.Printf("whatsmeow: KickResync: GetJoinedGroups: %v", err)
		return
	}
	log.Printf("whatsmeow: KickResync: %d joined groups", len(groups))
	a.seedGroups(groups)
}

// clearErrorState resets the display-only "error"/"qr" state a prior
// handleDisconnect/pending-QR may have set — split out from handleConnected
// so it's testable without a live client (GetJoinedGroups needs one, this
// doesn't; same reasoning as seedGroups below). WAConnected=true always;
// Mood only resets if it's still "error" or "qr" (don't clobber some other
// system mood, e.g. "muted", set for an unrelated reason in between).
// Deliberately does NOT clear state.Muted/governor.Killed — see
// handleDisconnect's doc: un-killing is the owner's call (set_kill_switch),
// never implicit in reconnecting.
//
// Publishes "wa_connected" on the eventbus (S1f, ct-2026-07-19-1735) — the
// dashboard's SSE listener uses it to flip live from the QR screen to the
// admin panel the instant pairing succeeds, no polling wait, mirroring
// handleDisconnect's own "wa_disconnected" nudge for the reverse direction.
func (a *Adapter) clearErrorState() {
	if a.state == nil {
		return
	}
	_ = a.state.UpdateMood(func(s *state.Status) {
		if s.Mood == "error" || s.Mood == "qr" {
			s.Mood = "idle"
		}
		s.WAConnected = true
		s.ShowQR = false
	})
	if a.bus != nil {
		a.bus.Publish(eventbus.Event{Type: "wa_connected", TS: time.Now().Unix()})
	}
}

// recordOwnIdentity mirrors the linked account's own JID + display name
// into state.Status — GET /api/status's own_number/name (ct-2026-07-10-2312).
// Split out from clearErrorState so that one stays testable without a live
// client; this one needs a.client.Store, only available post-connect.
//
// T17 (ct-2026-08-05-1240, Part 1): OwnName is only overwritten when
// Store.PushName is non-empty — same "never blank a known value" guard
// TouchChat's own SQL already applies to chats.name (chat.go), reused here.
// Store.PushName is populated EXCLUSIVELY by a PushNameSetting appstate
// mutation (confirmed against the vendored whatsmeow library —
// appstate.go:367 is the only writer in the whole module); on some
// sessions that mutation is never replayed again after the very first
// capture, so recordOwnIdentity can run — on every single reconnect, for
// the rest of the process's life — with Store.PushName=="" despite the
// account genuinely having a name. Before this guard, every one of those
// reconnects blanked OwnName back to "" unconditionally, discarding
// whatever NewManager seeded from a previous run's status.json (state.go)
// or whatever an earlier *events.PushNameSetting had already delivered —
// this is why the header could show a real número with "(sin nombre)"
// instead of transiently, but for the ENTIRE runtime. OwnJID stays
// unconditional: a JID never "un-happens" once paired, so the freshest
// value always wins.
func (a *Adapter) recordOwnIdentity() {
	if a.state == nil || a.client.Store.ID == nil {
		return
	}
	ownJID := a.client.Store.ID.String()
	ownName := a.client.Store.PushName
	_ = a.state.Update(func(s *state.Status) {
		s.OwnJID = ownJID
		if ownName != "" {
			s.OwnName = ownName
		}
	})
	a.markOwner(ownJID, ownName)
}

// markOwner marks jid — the number WhatsApp is paired with — as the trusted
// owner (T12, ct-2026-08-05-1231), UNLESS is_boss was already explicitly
// decided for it (store.MarkOwnerIfUntouched's own guard). Split out from
// recordOwnIdentity so it's testable without a live whatsmeow client — same
// reasoning as clearErrorState/seedGroups above. TouchChat runs first to
// guarantee the row exists with a normal individual chat's defaults (see
// MarkOwnerIfUntouched's doc for why a bare insert there would be wrong).
func (a *Adapter) markOwner(jid, name string) {
	if a.store == nil {
		return
	}
	if err := a.store.TouchChat(jid, name, time.Now().Unix()); err != nil {
		log.Printf("whatsmeow: markOwner TouchChat %s: %v", jid, err)
		return
	}
	if err := a.store.MarkOwnerIfUntouched(jid); err != nil {
		log.Printf("whatsmeow: markOwner MarkOwnerIfUntouched %s: %v", jid, err)
	}
}

// seedGroups persists groups into the store — split out from
// handleConnected so it's testable without a live whatsmeow connection
// (GetJoinedGroups itself needs one, this doesn't).
func (a *Adapter) seedGroups(groups []*types.GroupInfo) {
	if a.store == nil {
		return
	}
	now := time.Now().Unix()
	for _, g := range groups {
		jid := g.JID.String()
		if err := a.store.TouchChat(jid, g.Name, now); err != nil {
			log.Printf("whatsmeow: seed group %s: %v", jid, err)
			continue
		}
		if a.router != nil {
			// ST-B (ct-2026-07-11-0741): SyncRouterMode, not SetMode — this is
			// the router mirror re-running at connect time, same as
			// corepipeline.handleInbound's per-message mirror. SetMode now
			// marks mode_source='manual' (an owner/agent's deliberate
			// choice); calling it here would mislabel every reconnect as a
			// manual override and permanently block the router from ever
			// syncing that group's mode again.
			if err := a.store.SyncRouterMode(jid, a.router.Resolve(jid).Mode); err != nil {
				log.Printf("whatsmeow: seed group mode %s: %v", jid, err)
			}
		}

		// ct-2026-07-19-0138 (backup Sub 2b): topic + membership scraping.
		// Both g.Topic and g.Participants already came with GetJoinedGroups
		// — no extra WhatsApp-server call here, so this is store-only, no
		// actionDelay needed (unlike sync.go's contact backfill, which DOES
		// hit the server per contact).
		if err := a.store.SetChatDescription(jid, g.Topic); err != nil {
			log.Printf("whatsmeow: seed group description %s: %v", jid, err)
		}
		for _, p := range g.Participants {
			// UpsertGroupMember (group_members, Sub 1) — the only group-
			// membership write path (T18B, ct-2026-08-05-1243: chat_groups,
			// a second table this used to deliberately NOT write, was
			// retired — it never had a production writer at all).
			// DisplayName is only populated for anonymous announcement-group
			// participants (whatsmeow's own doc) — empty for a regular
			// member, same as "no pisa": UpsertGroupMember never overwrites
			// a name already known with "". The number (p.JID) is what
			// matters here.
			if err := a.store.UpsertGroupMember(jid, p.JID.String(), p.DisplayName, now); err != nil {
				log.Printf("whatsmeow: seed group member %s/%s: %v", jid, p.JID, err)
			}
		}
	}
}

// ResolvePN resolves a @lid JID string to its phone-number JID string via
// whatsmeow's persistent LID map. Survives the F2 revert (ct-2026-07-18-
// 171940) because capipush.plaintextPayload (ct-2026-07-18-1416, kept) is
// the seam's only caller now — it resolves the dispatch payload's "numero"
// for a @lid chat. "" (nil error) means not resolved yet. .ToNonAD() for
// canonical per-person identity, not a per-device one.
func (a *Adapter) ResolvePN(ctx context.Context, lidJID string) (string, error) {
	jid, err := types.ParseJID(lidJID)
	if err != nil {
		return "", err
	}
	if a.client == nil || a.client.Store == nil || a.client.Store.LIDs == nil {
		return "", nil
	}
	pn, err := a.client.Store.LIDs.GetPNForLID(ctx, jid)
	if err != nil {
		return "", err
	}
	if pn.User == "" {
		return "", nil
	}
	return pn.ToNonAD().String(), nil
}

// resolveChatJID converts a 1:1 chat's @lid identity to its real
// phone-number JID (S7b, ct-2026-07-30-0332 — "los JID no se usen si ya
// tenemos el número", boss verbatim) — ResolvePN already did this for
// dispatch/read, never for ingestion, so a chat could exist twice: once
// under @lid (write), once under the number (everything else read/wrote
// to). Groups are never touched (a group's own JID is always @g.us, never
// @lid) and neither is an already-number-addressed chat.
//
// The gate is the chat's OWN JID identity (Chat.Server == HiddenUserServer
// — the same test ResolvePN/GetPNForLID apply, parsing the JID itself),
// NOT info.AddressingMode. S7c (ct-2026-07-30-0524) found S7b shipped with
// exactly that bug: AddressingMode is a SEPARATE stanza attribute
// (parseMessageSource reads it from "addressing_mode") independent of
// which Server the Chat/Sender JIDs actually ended up with (from.Server, a
// different field) — nothing guarantees the two agree. Confirmed against
// a real boss message (S7c, 2026-07-30 09:03): addressing_mode arrived ""
// while chat was a real @lid WITH a resolvable SenderAlt sitting right
// there — the old AddressingMode gate returned before ever looking at it,
// so the message saved under @lid despite the fix being otherwise correct.
// Also fixes history.go's identical unresolved chatJID: ParseWebMessage
// (whatsmeow's own history-message constructor) never sets AddressingMode/
// SenderAlt/RecipientAlt at all (those are live-stanza-only fields) — a
// JID-identity gate works there too, GetPNForLID fallback the only path
// that ever fires for history (its own doc there explains the cost).
//
// Prefers the protocol-provided alt JID over a DB lookup: whatsmeow already
// resolves this PER-MESSAGE from the stanza itself (confirmed against its
// own message.go — it's also how whatsmeow keeps its OWN persistent LID map
// fresh, StoreLIDPNMapping), so most live messages cost zero extra
// whatsmeow.db hits. Which field carries it depends on who sent the
// message — get this backwards and a from_me message (synced from another
// linked device) writes the GATEWAY's own number into a CONTACT's chat,
// not just a cosmetic mislabel (Citrino, verified against
// message.go:126-144's parseMessageSource before approving this):
//   - a real inbound message: SenderAlt (Chat == Sender, the contact).
//   - IsFromMe: RecipientAlt — Sender here is the gateway's own identity;
//     SenderAlt is never populated in this branch at all.
//
// GetPNForLID (piumy-gateway's own whatsmeow.db call) is only the fallback
// when the protocol didn't carry an alt this time — and even then it's
// cheap: whatsmeow's own CachedLIDMap checks an in-memory cache before ever
// touching the DB. "" (never resolved yet) falls back to the raw @lid JID —
// a chat that would be saved today must never be lost because resolution
// isn't available yet (contract's cuidado #1).
//
// Takes types.MessageSource, not types.MessageInfo (T36, ct-2026-08-08-1312):
// every field this reads (IsGroup/Chat/SenderAlt/IsFromMe/RecipientAlt) lives
// in MessageSource, which events.Message.Info AND events.Receipt both embed
// (parseMessageSource builds both the same way — receipt.go). handleMessage
// and handleRetryReceipt call this SAME function so the chat_jid a message
// is saved under and the chat_jid its retry receipt marks can never diverge
// again — before this, handleRetryReceipt used evt.Chat raw (still @lid),
// while the message it was trying to mark had been saved under the number
// this function resolves to, so the UPDATE silently touched zero rows.
func (a *Adapter) resolveChatJID(src types.MessageSource) types.JID {
	if src.IsGroup || src.Chat.Server != types.HiddenUserServer {
		return src.Chat
	}
	alt := src.SenderAlt
	if src.IsFromMe {
		alt = src.RecipientAlt
	}
	if alt.Server == types.DefaultUserServer && !alt.IsEmpty() {
		return alt.ToNonAD()
	}
	if a.client == nil || a.client.Store == nil || a.client.Store.LIDs == nil {
		return src.Chat
	}
	pn, err := a.client.Store.LIDs.GetPNForLID(context.Background(), src.Chat)
	if err != nil || pn.User == "" {
		return src.Chat // not resolved yet — stays @lid, same as today
	}
	return pn.ToNonAD()
}

// handleMessage maps a whatsmeow events.Message to gateway.Inbound and
// pushes it, non-blocking (a full buffer drops the message with a log —
// see inboundBuffer's doc comment). Filters IsFromMe (mirrors openwa's own
// fromMe filter, docs/F3-OPENWA-ADAPTER.md point 1 — a message we sent
// ourselves, echoed back by the multi-device sync, is not new inbound
// work). Groups (@g.us) flow through identically to 1:1 chats — the core
// already distinguishes them by JID suffix (store.isGroupJID), nothing
// whatsmeow-specific needed here.
//
// Media (Image/Video/Audio/Document/Sticker): detected via detectMedia,
// downloaded and persisted by downloadAndStoreMedia (Config.MediaDir +
// Config.Store required; empty MediaDir skips saving but Type/Text still
// reflect the media) — but ONLY for a chat the router actually allows
// (ct-2026-07-21-1727 fix): before this, detectMedia+downloadAndStoreMedia
// ran unconditionally, so a group the whitelist was never meant to track
// still got every photo fully downloaded and stored (disk + a `media` row)
// — the exact "526 files on disk, 0 exposed" desync Citrino flagged, since
// corepipeline.handleInbound's own Allowed gate (pipeline.go) discards the
// message right after, before it ever reaches store.AddMessage. MIME type →
// Inbound.Type; caption → Inbound.Text. gateway.Inbound stays clean — no
// vendor types cross the seam.
//
// Reply/forward (ct-2026-07-21-1610, S6a): detectReply reads the concrete
// sub-message's own ContextInfo into Inbound.QuotedID/QuotedPreview/Forwarded
// — "" / false for a plain message.
func (a *Adapter) handleMessage(evt *events.Message) {
	if evt.Info.IsFromMe {
		if a.client == nil || a.client.Store.ID == nil || !isSelfChat(evt.Info.Chat, *a.client.Store.ID) || isOwnDevice(evt.Info.Sender, *a.client.Store.ID) {
			return
		}
	}

	chatJID := a.resolveChatJID(evt.Info.MessageSource).String()
	text := evt.Message.GetConversation()
	if text == "" {
		// Replies/quotes/link-preview messages arrive as ExtendedTextMessage
		// instead of a plain Conversation — same text, different wrapper.
		text = evt.Message.GetExtendedTextMessage().GetText()
	}
	msgType := evt.Info.Type

	m, isMedia := detectMedia(evt)
	if isMedia && (a.router == nil || a.router.Resolve(chatJID).Allowed) {
		msgType, text = a.downloadAndStoreMedia(
			context.Background(),
			string(evt.Info.ID),
			chatJID,
			evt.Info.Timestamp.Unix(),
			m,
		)
	} else if text == "" && !isMedia {
		// S5 (ct-2026-07-30-031027): evt.Info.Type is the WIRE-level stanza
		// attribute (whatsmeow's parseMessageInfo reads it off the raw node,
		// not the decrypted payload) — it reads "text" for reactions,
		// protocol messages (history-sync notifications, app-state key
		// shares, message revokes...), poll votes, and dozens of other
		// waE2E.Message variants that carry no displayable text or media at
		// all. Before this, those got stored as a real inbound message —
		// type:"text", text:"" — indistinguishable from a genuine blank
		// message. One of those was the exact envelope capipush dispatched
		// to the agent during the incident that triggered S2: the agent had
		// nothing to act on, never closed the gate, and the channel jammed
		// behind it. Dropped here, at ingestion, instead of reaching the
		// store or the dispatch pipeline at all — logged with WHICH field
		// was actually populated (via protobuf reflection: waE2E.Message has
		// 100+ oneof variants, far too many to keep a switch statement
		// current against).
		log.Printf("whatsmeow: mensaje sin texto ni media descartado (no es contenido de chat real) chat=%s id=%s campo=%s", chatJID, evt.Info.ID, firstSetFieldName(evt.Message))
		return
	}
	reply := detectReply(evt.Message)

	inb := gateway.Inbound{
		ChatJID:       chatJID,
		SenderJID:     evt.Info.Sender.String(),
		MsgID:         string(evt.Info.ID),
		Text:          text,
		Type:          msgType,
		TS:            evt.Info.Timestamp.Unix(),
		PushName:      evt.Info.PushName,
		QuotedID:      reply.quotedID,
		QuotedPreview: reply.quotedPreview,
		Forwarded:     reply.forwarded,
	}
	select {
	case a.inbound <- inb:
	default:
		log.Printf("whatsmeow: inbound buffer full, dropping message chat=%s id=%s", inb.ChatJID, inb.MsgID)
	}
}

// handleRetryReceipt leaves a trace when a message WE sent reached the
// recipient's device but couldn't be decrypted there — today the only
// observable signal for this (ct-2026-08-07). It logs (dev-visible) AND
// persists decrypt_retry_at on the store (T35, ct-2026-08-08-1258 — the log
// alone never reaches production: no log file, -H windowsgui has no
// console). Still does not retry, resend, or change how messages are
// addressed — that decision (whether/when to update whatsmeow's LID
// handling) belongs to Citrino/the boss, not this handler. Nil-safe: a.store
// is optional (Config.Store), same convention as the rest of this file.
//
// chatJID goes through resolveChatJID (T36, ct-2026-08-08-1312), NOT
// evt.Chat raw: a receipt's Chat arrives in the same unresolved @lid form a
// message's does (see resolveChatJID's own doc) — using it raw meant the
// mark could never match a message saved under the resolved number, the
// exact case this whole feature exists for (a LID-addressed contact).
func (a *Adapter) handleRetryReceipt(evt *events.Receipt) {
	chatJID := a.resolveChatJID(evt.MessageSource).String()
	ts := evt.Timestamp.Unix()
	for _, id := range evt.MessageIDs {
		log.Printf("whatsmeow: mensaje a %s (id %s) llegó al dispositivo pero no se pudo descifrar — WhatsApp pidió reenvío (retry receipt)", chatJID, id)
		if a.store == nil {
			continue
		}
		if err := a.store.MarkDecryptRetry(chatJID, id, ts); err != nil {
			log.Printf("whatsmeow: MarkDecryptRetry %s/%s: %v", chatJID, id, err)
		}
	}
}

// firstSetFieldName names whichever field is actually populated on msg,
// via protobuf reflection — used only for the diagnostic log above (S5,
// ct-2026-07-30-031027) when a message carries no text/media: waE2E.Message
// has 100+ possible oneof payload variants (reactions, protocol messages,
// poll votes, and more WhatsApp adds over time), so this answers "what was
// it?" without a switch statement that goes stale the next time WhatsApp
// ships a new message kind. messageContextInfo is skipped — it's metadata
// (quoted message, ephemeral settings...) that rides along with almost any
// OTHER content type, never the answer to "what kind of message is this".
func firstSetFieldName(msg *waE2E.Message) string {
	var name string
	msg.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		if fd.Name() == "messageContextInfo" {
			return true // metadata, keep scanning for the real content field
		}
		name = string(fd.Name())
		return false
	})
	if name == "" {
		return "(sin campos poblados)"
	}
	return name
}
