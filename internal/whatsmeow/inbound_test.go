package whatsmeow

import (
	"bytes"
	"context"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/appstate"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"piumy-gateway/internal/eventbus"
	"piumy-gateway/internal/gateway"
	"piumy-gateway/internal/governor"
	"piumy-gateway/internal/router"
	"piumy-gateway/internal/state"
	"piumy-gateway/internal/store"
)

func newTestAdapter() *Adapter {
	return &Adapter{inbound: make(chan gateway.Inbound, 4)}
}

func TestHandleMessageMapsToInbound(t *testing.T) {
	a := newTestAdapter()
	ts := time.Unix(1700000000, 0)
	evt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat:   types.NewJID("123", "g.us"),
				Sender: types.NewJID("456", "s.whatsapp.net"),
			},
			ID:        "MSGID1",
			Type:      "text",
			PushName:  "Alice",
			Timestamp: ts,
		},
		Message: &waE2E.Message{Conversation: proto.String("hola")},
	}
	a.handleMessage(evt)

	select {
	case got := <-a.inbound:
		if got.ChatJID != "123@g.us" || got.SenderJID != "456@s.whatsapp.net" {
			t.Errorf("ChatJID/SenderJID = %q/%q, want 123@g.us/456@s.whatsapp.net", got.ChatJID, got.SenderJID)
		}
		if got.MsgID != "MSGID1" || got.Text != "hola" || got.Type != "text" || got.PushName != "Alice" {
			t.Errorf("got = %+v, unexpected fields", got)
		}
		if got.TS != ts.Unix() {
			t.Errorf("TS = %d, want %d", got.TS, ts.Unix())
		}
	default:
		t.Fatal("handleMessage did not push anything onto Inbound()")
	}
}

func TestHandleMessageFiltersFromMe(t *testing.T) {
	a := newTestAdapter()
	evt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat:     types.NewJID("123", "s.whatsapp.net"),
				IsFromMe: true,
			},
			ID: "MSGID2",
		},
		Message: &waE2E.Message{Conversation: proto.String("eco de mi propio mensaje")},
	}
	a.handleMessage(evt)

	select {
	case got := <-a.inbound:
		t.Errorf("want IsFromMe filtered out, got %+v", got)
	default:
		// expected: nothing pushed
	}
}

// TestHandleMessageFromMeNoteToSelf covers Pieza D (ct-2026-07-24-0527):
// TestHandleMessageFiltersFromMe already proved the general rule (an
// IsFromMe echo is dropped) — this covers the one carve-out isSelfChat/
// isOwnDevice add on top of it: a Note-to-Self message relayed by ANOTHER
// linked device (e.g. the boss's phone) must reach the pipeline as regular
// inbound, while the gateway's OWN echo of what it just sent (same Device)
// must still be dropped, or a reply into the self-chat would loop forever.
func TestHandleMessageFromMeNoteToSelf(t *testing.T) {
	own := types.JID{User: "5550000000201", Server: "s.whatsapp.net", Device: 3}
	fromAnotherDevice := types.JID{User: own.User, Server: own.Server, Device: own.Device + 1}
	fromGatewayItself := types.JID{User: own.User, Server: own.Server, Device: own.Device}
	someoneElsesChat := types.JID{User: "999", Server: "s.whatsapp.net"}

	cases := []struct {
		name       string
		chat       types.JID
		sender     types.JID
		pairDevice bool // false leaves client.Store.ID nil, as if not yet paired
		wantPassed bool
	}{
		{"self-chat note from another device passes through", own, fromAnotherDevice, true, true},
		{"self-chat echo of the gateway's own send is dropped", own, fromGatewayItself, true, false},
		{"IsFromMe on a chat that isn't Note-to-Self is dropped", someoneElsesChat, fromAnotherDevice, true, false},
		{"Store.ID nil (not yet paired) drops without panicking", own, fromAnotherDevice, false, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client := newTestWmeowClient(t)
			if c.pairDevice {
				client.Store.ID = &own
			}
			a := &Adapter{inbound: make(chan gateway.Inbound, 4), client: client}

			evt := &events.Message{
				Info: types.MessageInfo{
					MessageSource: types.MessageSource{Chat: c.chat, Sender: c.sender, IsFromMe: true},
					ID:            "MSGID-NOTE",
				},
				Message: &waE2E.Message{Conversation: proto.String("nota")},
			}
			a.handleMessage(evt)

			select {
			case <-a.inbound:
				if !c.wantPassed {
					t.Error("handleMessage pushed a message, want it dropped")
				}
			default:
				if c.wantPassed {
					t.Error("handleMessage dropped the message, want it passed through")
				}
			}
		})
	}
}

// ── S7b (ct-2026-07-30-0332) — resolveChatJID ──────────────────────────────

// TestResolveChatJIDUsesSenderAltForRealInbound covers the free, protocol-
// provided path: whatsmeow already resolved the sender's number in the
// stanza itself (SenderAlt), no whatsmeow.db call needed — no client wired
// at all, proving the DB fallback is never reached in this case.
func TestResolveChatJIDUsesSenderAltForRealInbound(t *testing.T) {
	a := &Adapter{inbound: make(chan gateway.Inbound, 1)}
	lid := types.NewJID("111", "lid")
	number := types.NewJID("55500000002", "s.whatsapp.net")
	info := types.MessageInfo{MessageSource: types.MessageSource{
		Chat: lid, Sender: lid,
		AddressingMode: types.AddressingModeLID,
		SenderAlt:      number,
	}}

	got := a.resolveChatJID(info)
	if got.String() != number.ToNonAD().String() {
		t.Errorf("resolveChatJID = %s, want %s (resolved via SenderAlt)", got, number.ToNonAD())
	}
}

// TestResolveChatJIDResolvesRealBossMessageWithEmptyAddressingMode is S7c's
// own regression (ct-2026-07-30-0524) — the exact values Citrino captured
// from a REAL live boss message (2026-07-30 09:03) via the S7C-DEBUG-TEMP
// log, not synthetic values we picked: addressing_mode arrived EMPTY while
// Chat was a genuine @lid with a resolvable SenderAlt sitting right there.
// S7b's gate (`AddressingMode != types.AddressingModeLID`) returned before
// ever looking at it — this is the case none of S7b's 8 tests covered,
// because all of them assumed AddressingMode==LID, which production never
// actually sent.
func TestResolveChatJIDResolvesRealBossMessageWithEmptyAddressingMode(t *testing.T) {
	a := &Adapter{inbound: make(chan gateway.Inbound, 1)}
	info := types.MessageInfo{MessageSource: types.MessageSource{
		Chat:           types.NewJID("555000000000200", "lid"),
		Sender:         types.NewJID("555000000000200", "lid"),
		AddressingMode: "", // observed empty in production, not "pn" or anything else
		SenderAlt:      types.NewJID("55500000044", "s.whatsapp.net"),
		IsFromMe:       false,
		IsGroup:        false,
	}}

	got := a.resolveChatJID(info)
	want := "55500000044@s.whatsapp.net"
	if got.String() != want {
		t.Errorf("resolveChatJID (real boss message, empty addressing_mode) = %s, want %s — the JID-identity gate must resolve this regardless of AddressingMode", got, want)
	}
}

// TestResolveChatJIDUsesRecipientAltForFromMe is Citrino's explicit
// regression request: a message synced from another linked device
// (IsFromMe) has Sender == the GATEWAY's own identity and Chat == the real
// recipient — SenderAlt is never populated in this case (confirmed against
// whatsmeow's own parseMessageSource), only RecipientAlt is. SenderAlt is
// deliberately set here to a DIFFERENT, wrong number: if resolveChatJID
// ever used the wrong field, this test catches the exact corruption Citrino
// flagged (the gateway's own number landing in a contact's chat) instead of
// just confirming "something" got resolved.
func TestResolveChatJIDUsesRecipientAltForFromMe(t *testing.T) {
	a := &Adapter{inbound: make(chan gateway.Inbound, 1)}
	lid := types.NewJID("222", "lid")
	gatewayOwnNumber := types.NewJID("55500000045", "s.whatsapp.net") // WRONG if ever picked
	contactNumber := types.NewJID("55500000015", "s.whatsapp.net")    // correct answer
	info := types.MessageInfo{MessageSource: types.MessageSource{
		Chat: lid, Sender: types.NewJID("55500000045", "s.whatsapp.net"),
		IsFromMe:       true,
		AddressingMode: types.AddressingModeLID,
		SenderAlt:      gatewayOwnNumber,
		RecipientAlt:   contactNumber,
	}}

	got := a.resolveChatJID(info)
	if got.String() != contactNumber.ToNonAD().String() {
		t.Errorf("resolveChatJID(from_me) = %s, want %s (RecipientAlt) — got the gateway's own number if this is %s", got, contactNumber.ToNonAD(), gatewayOwnNumber.ToNonAD())
	}
}

// TestResolveChatJIDFallsBackToGetPNForLID covers the DB-lookup path: no
// alt came with the stanza this time, but whatsmeow's persistent LID map
// already has the mapping from an earlier message.
func TestResolveChatJIDFallsBackToGetPNForLID(t *testing.T) {
	client := newTestWmeowClient(t)
	lid := types.NewJID("333", "lid")
	number := types.NewJID("55500000016", "s.whatsapp.net")
	if err := client.Store.LIDs.PutLIDMapping(context.Background(), lid, number); err != nil {
		t.Fatal(err)
	}
	a := &Adapter{inbound: make(chan gateway.Inbound, 1), client: client}
	info := types.MessageInfo{MessageSource: types.MessageSource{
		Chat: lid, Sender: lid, AddressingMode: types.AddressingModeLID,
	}}

	got := a.resolveChatJID(info)
	if got.String() != number.ToNonAD().String() {
		t.Errorf("resolveChatJID (DB fallback) = %s, want %s", got, number.ToNonAD())
	}
}

// TestResolveChatJIDFallsBackToRawJIDWhenUnresolved is the contract's
// cuidado #1: no alt, no mapping yet — the chat must not be lost, it stays
// under the raw @lid, same as today.
func TestResolveChatJIDFallsBackToRawJIDWhenUnresolved(t *testing.T) {
	client := newTestWmeowClient(t)
	a := &Adapter{inbound: make(chan gateway.Inbound, 1), client: client}
	lid := types.NewJID("444", "lid")
	info := types.MessageInfo{MessageSource: types.MessageSource{
		Chat: lid, Sender: lid, AddressingMode: types.AddressingModeLID,
	}}

	got := a.resolveChatJID(info)
	if got.String() != lid.String() {
		t.Errorf("resolveChatJID (unresolved) = %s, want the raw @lid %s unchanged", got, lid)
	}
}

// TestResolveChatJIDNeverTouchesGroups: a group's own JID is always @g.us,
// never @lid — resolveChatJID must return it untouched regardless of
// AddressingMode.
func TestResolveChatJIDNeverTouchesGroups(t *testing.T) {
	a := &Adapter{inbound: make(chan gateway.Inbound, 1)}
	group := types.NewJID("555000000000000001", "g.us")
	info := types.MessageInfo{MessageSource: types.MessageSource{
		Chat: group, Sender: types.NewJID("111", "lid"),
		IsGroup: true, AddressingMode: types.AddressingModeLID,
		SenderAlt: types.NewJID("55500000013", "s.whatsapp.net"),
	}}

	got := a.resolveChatJID(info)
	if got.String() != group.String() {
		t.Errorf("resolveChatJID (group) = %s, want the group JID %s untouched", got, group)
	}
}

// TestResolveChatJIDSkipsAlreadyNumberJID: a chat whose OWN JID is already a
// phone number (Chat.Server != HiddenUserServer — the gate, S7c,
// ct-2026-07-30-0524) needs no resolution at all, regardless of
// AddressingMode (which no longer gates anything — see resolveChatJID's
// own doc for why it used to and why that was the S7c bug).
func TestResolveChatJIDSkipsAlreadyNumberJID(t *testing.T) {
	a := &Adapter{inbound: make(chan gateway.Inbound, 1)}
	number := types.NewJID("55500000014", "s.whatsapp.net")
	info := types.MessageInfo{MessageSource: types.MessageSource{
		Chat: number, Sender: number, AddressingMode: types.AddressingModePN,
	}}

	got := a.resolveChatJID(info)
	if got.String() != number.String() {
		t.Errorf("resolveChatJID (already a number JID) = %s, want %s untouched", got, number)
	}
}

// TestHandleMessageResolvesLIDChatToNumber is the end-to-end regression
// (through the real ingestion entry point, not just the unit-level helper):
// a real inbound message from a contact whose number is already resolvable
// must land in gateway.Inbound.ChatJID under the number, not the @lid.
func TestHandleMessageResolvesLIDChatToNumber(t *testing.T) {
	a := newTestAdapter()
	lid := types.NewJID("555", "lid")
	number := types.NewJID("55500000019", "s.whatsapp.net")
	evt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat: lid, Sender: lid,
				AddressingMode: types.AddressingModeLID,
				SenderAlt:      number,
			},
			ID: "MSGID-LID",
		},
		Message: &waE2E.Message{Conversation: proto.String("hola")},
	}
	a.handleMessage(evt)

	select {
	case got := <-a.inbound:
		if got.ChatJID != number.ToNonAD().String() {
			t.Errorf("ChatJID = %q, want the resolved number %q, not the raw @lid", got.ChatJID, number.ToNonAD().String())
		}
	default:
		t.Fatal("handleMessage did not push anything onto Inbound()")
	}
}

// TestHandleMessageFromMeNoteToSelfResolvesViaRecipientAlt extends
// TestHandleMessageFromMeNoteToSelf's "passes through" case with S7b's
// resolution: a Note-to-Self message synced from another device, addressed
// via LID, must resolve Chat via RecipientAlt end-to-end — SenderAlt is set
// to a deliberately wrong number to catch the exact corruption Citrino
// flagged if the wrong field were ever used.
func TestHandleMessageFromMeNoteToSelfResolvesViaRecipientAlt(t *testing.T) {
	own := types.JID{User: "5550000000201", Server: "lid", Device: 3}
	fromAnotherDevice := types.JID{User: own.User, Server: own.Server, Device: own.Device + 1}
	resolvedOwn := types.NewJID("5550000000201", "s.whatsapp.net")
	wrongNumber := types.NewJID("55500000045", "s.whatsapp.net")

	client := newTestWmeowClient(t)
	client.Store.ID = &own
	a := &Adapter{inbound: make(chan gateway.Inbound, 4), client: client}

	evt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat: own, Sender: fromAnotherDevice, IsFromMe: true,
				AddressingMode: types.AddressingModeLID,
				SenderAlt:      wrongNumber,
				RecipientAlt:   resolvedOwn,
			},
			ID: "MSGID-NOTE-LID",
		},
		Message: &waE2E.Message{Conversation: proto.String("nota")},
	}
	a.handleMessage(evt)

	select {
	case got := <-a.inbound:
		if got.ChatJID != resolvedOwn.ToNonAD().String() {
			t.Errorf("ChatJID = %q, want RecipientAlt's %q (got the wrong-number %q if SenderAlt was used instead)",
				got.ChatJID, resolvedOwn.ToNonAD().String(), wrongNumber.ToNonAD().String())
		}
	default:
		t.Fatal("handleMessage did not push anything onto Inbound() for the Note-to-Self case")
	}
}

func TestHandleMessageFallsBackToExtendedText(t *testing.T) {
	a := newTestAdapter()
	evt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: types.NewJID("123", "s.whatsapp.net")},
			ID:            "MSGID3",
		},
		Message: &waE2E.Message{
			ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("con preview de link")},
		},
	}
	a.handleMessage(evt)

	select {
	case got := <-a.inbound:
		if got.Text != "con preview de link" {
			t.Errorf("Text = %q, want the ExtendedTextMessage fallback", got.Text)
		}
	default:
		t.Fatal("handleMessage did not push anything onto Inbound()")
	}
}

// TestHandleMessagePropagatesReplyAndForwarded covers ct-2026-07-21-1610
// (S6a backend): a reply/forward on the live path must reach gateway.Inbound
// with QuotedID/QuotedPreview/Forwarded set, same as detectReply's own unit
// tests (reply_test.go) but exercised through the real ingestion entry point.
func TestHandleMessagePropagatesReplyAndForwarded(t *testing.T) {
	a := newTestAdapter()
	evt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: types.NewJID("123", "s.whatsapp.net")},
			ID:            "MSGID-REPLY",
		},
		Message: &waE2E.Message{
			ExtendedTextMessage: &waE2E.ExtendedTextMessage{
				Text: proto.String("respuesta"),
				ContextInfo: &waE2E.ContextInfo{
					StanzaID:      proto.String("QUOTED1"),
					QuotedMessage: &waE2E.Message{Conversation: proto.String("original")},
					IsForwarded:   proto.Bool(true),
				},
			},
		},
	}
	a.handleMessage(evt)

	select {
	case got := <-a.inbound:
		if got.QuotedID != "QUOTED1" || got.QuotedPreview != "original" || !got.Forwarded {
			t.Errorf("got = %+v, want QuotedID=QUOTED1 QuotedPreview=original Forwarded=true", got)
		}
	default:
		t.Fatal("handleMessage did not push anything onto Inbound()")
	}
}

// TestHandleMessagePlainTextHasEmptyReplyFields confirms a normal message
// (no ContextInfo) leaves the reply/forward fields at their zero value.
func TestHandleMessagePlainTextHasEmptyReplyFields(t *testing.T) {
	a := newTestAdapter()
	evt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: types.NewJID("123", "s.whatsapp.net")},
			ID:            "MSGID-PLAIN",
		},
		Message: &waE2E.Message{Conversation: proto.String("hola")},
	}
	a.handleMessage(evt)

	select {
	case got := <-a.inbound:
		if got.QuotedID != "" || got.QuotedPreview != "" || got.Forwarded {
			t.Errorf("got = %+v, want empty QuotedID/QuotedPreview and Forwarded=false", got)
		}
	default:
		t.Fatal("handleMessage did not push anything onto Inbound()")
	}
}

// TestHandleMessageMediaSetsTypeToMimeAndTextToCaption is the regression
// test for ct-2026-07-21-1727: handleMessage used to assign
// downloadAndStoreMedia's (mime, caption) return into (text, msgType)
// instead of (msgType, text) — every live media message stored the raw
// MIME string in Inbound.Text (shown as "image/jpeg" instead of a photo)
// and the caption in Inbound.Type (store.MediaKind never recognizes it, so
// the message never displayed as media even once downloaded). DirectPath
// is deliberately unset so the real (unconnected) wmeow client fails the
// download deterministically, no network touched — downloadAndStoreMedia
// still returns (mime, caption) on that failure path (media.go's own doc).
func TestHandleMessageMediaSetsTypeToMimeAndTextToCaption(t *testing.T) {
	a := &Adapter{inbound: make(chan gateway.Inbound, 4), mediaDir: t.TempDir(), client: newTestWmeowClient(t)}
	evt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: types.NewJID("123", "s.whatsapp.net")},
			ID:            "MSGID-MEDIA",
		},
		Message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
			Mimetype: proto.String("image/jpeg"), Caption: proto.String("una foto"),
		}},
	}
	a.handleMessage(evt)

	select {
	case got := <-a.inbound:
		if got.Type != "image/jpeg" {
			t.Errorf("Type = %q, want image/jpeg (the MIME)", got.Type)
		}
		if got.Text != "una foto" {
			t.Errorf("Text = %q, want %q (the caption)", got.Text, "una foto")
		}
	default:
		t.Fatal("handleMessage did not push anything onto Inbound()")
	}
}

// TestHandleMessageDropsProtocolMessageWithNoText is S5's own regression
// (ct-2026-07-30-031027): evt.Info.Type reads "text" at the WIRE level for
// a protocol message too (whatsmeow's parseMessageInfo reads it off the raw
// stanza attribute, never the decrypted payload) — before this fix, a bare
// ProtocolMessage (revoke, history-sync notification, app-state key
// share...) with no Conversation/ExtendedTextMessage got stored as a real
// inbound message, type:"text" text:"" — indistinguishable from a genuine
// blank message. This is the concrete shape of the nine-in-fifteen-seconds
// burst found in chat 55500000042@s.whatsapp.net (Note-to-Self): WhatsApp's
// own multi-device sync chatter (key shares, history-sync notifications)
// routes through the self-chat as protocol messages exactly like this one.
func TestHandleMessageDropsProtocolMessageWithNoText(t *testing.T) {
	a := newTestAdapter()
	evt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: types.NewJID("55500000042", "s.whatsapp.net")},
			ID:            "MSGID-PROTO",
			Type:          "text",
		},
		Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{}},
	}
	a.handleMessage(evt)

	select {
	case got := <-a.inbound:
		t.Errorf("handleMessage pushed a ProtocolMessage as if it were real content: %+v", got)
	default:
	}
}

// TestHandleMessageDropsReactionMessageWithNoText covers the same S5 shape
// for an emoji reaction — another waE2E.Message variant that carries no
// Conversation/ExtendedTextMessage, arrives with Info.Type=="text" at the
// wire level, and must never be stored as a blank text message.
func TestHandleMessageDropsReactionMessageWithNoText(t *testing.T) {
	a := newTestAdapter()
	evt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: types.NewJID("123", "s.whatsapp.net")},
			ID:            "MSGID-REACT",
			Type:          "text",
		},
		Message: &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Text: proto.String("👍")}},
	}
	a.handleMessage(evt)

	select {
	case got := <-a.inbound:
		t.Errorf("handleMessage pushed a ReactionMessage as if it were real content: %+v", got)
	default:
	}
}

// TestHandleMessageDiagnosticLogNamesRealPayloadField confirms "decime qué
// eran" is answered for real, not just guessed: the dropped-message log
// names the ACTUAL populated field via protobuf reflection, and skips
// messageContextInfo (metadata that rides along with almost any content
// type, field 35 — lower field numbers like reactionMessage's neighbors
// would otherwise report the wrong thing if reflection just took whatever
// Range() visits first without that skip).
func TestHandleMessageDiagnosticLogNamesRealPayloadField(t *testing.T) {
	a := newTestAdapter()
	evt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: types.NewJID("123", "s.whatsapp.net")},
			ID:            "MSGID-REACT-CTX",
			Type:          "text",
		},
		Message: &waE2E.Message{
			ReactionMessage:    &waE2E.ReactionMessage{Text: proto.String("👍")},
			MessageContextInfo: &waE2E.MessageContextInfo{},
		},
	}

	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })

	a.handleMessage(evt)

	if !strings.Contains(buf.String(), "reactionMessage") {
		t.Errorf("diagnostic log = %q, want it to name reactionMessage (not messageContextInfo, which is metadata riding along)", buf.String())
	}
}

// TestHandleMessageSkipsMediaDownloadForDisallowedChat is the regression
// test for the OTHER half of ct-2026-07-21-1727 (the "526 files on disk, 0
// exposed" desync): before this, detectMedia+downloadAndStoreMedia ran
// regardless of the router's whitelist, so a chat corepipeline.handleInbound
// would immediately discard (Allowed=false) still got its media fully
// downloaded and a `media` row written — orphaned forever, since no
// `messages` row for it would ever exist to associate it with. A disallowed
// chat must skip the download entirely: no store.Media row, no wasted
// bandwidth/anti-ban risk for a chat nobody asked to track.
func TestHandleMessageSkipsMediaDownloadForDisallowedChat(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "piumy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rt := router.NewManager(filepath.Join(t.TempDir(), "router.json")) // no whitelist, allow_all=false by default

	a := &Adapter{
		inbound: make(chan gateway.Inbound, 4), mediaDir: t.TempDir(),
		client: newTestWmeowClient(t), store: st, router: rt,
	}
	evt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: types.NewJID("999", "g.us")},
			ID:            "MSGID-NOTALLOWED",
		},
		Message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Mimetype: proto.String("image/jpeg")}},
	}
	a.handleMessage(evt)

	media, err := st.ListMedia("999@g.us", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(media) != 0 {
		t.Errorf("ListMedia = %d, want 0 — a disallowed chat must never trigger a media download", len(media))
	}

	select {
	case got := <-a.inbound:
		if got.Type == "image/jpeg" {
			t.Errorf("Type = %q, want the raw frame type — download must have been skipped", got.Type)
		}
	default:
		t.Fatal("handleMessage did not push anything onto Inbound() — corepipeline's own Allowed gate is what discards it, not this one")
	}
}

// TestSeedGroupsTouchesChatAndSetsMode is the regression test for the
// audit's second finding (ct-2026-07-10-0420 review): a group with no
// inbound history must still show up in list_chats/get_chat_groups (the
// boss's original complaint) — seedGroups is what handleConnected calls
// with a real GetJoinedGroups result; this exercises it without needing a
// live whatsmeow connection.
func TestSeedGroupsTouchesChatAndSetsMode(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "piumy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// A router.json that doesn't exist yet resolves to router.Load's own
	// safe default (DefaultMode: "dedicated") — no explicit setup needed.
	rt := router.NewManager(filepath.Join(t.TempDir(), "router.json"))

	a := &Adapter{inbound: make(chan gateway.Inbound, 4), store: st, router: rt}
	groups := []*types.GroupInfo{
		{JID: types.NewJID("555000000000000001", "g.us"), GroupName: types.GroupName{Name: "Grupo De Prueba"}},
	}
	a.seedGroups(groups)

	chat, ok, err := st.GetChat("555000000000000001@g.us")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("seedGroups: want the group touched into the store even with zero inbound messages")
	}
	if chat.Name != "Grupo De Prueba" {
		t.Errorf("Name = %q, want Grupo De Prueba", chat.Name)
	}
	if chat.Mode != "dedicated" {
		t.Errorf("Mode = %q, want dedicated (from router.Resolve, mirroring corepipeline.handleInbound)", chat.Mode)
	}
}

// TestMarkOwnerCleanInstallMarksOwnChat is the T12 (ct-2026-08-05-1231)
// clean-install case: markOwner is what recordOwnIdentity calls at connect
// (handleConnected) with state.OwnJID — a chat nobody has ever decided
// is_boss for gets marked owner, with no dashboard action needed.
func TestMarkOwnerCleanInstallMarksOwnChat(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "piumy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a := &Adapter{inbound: make(chan gateway.Inbound, 4), store: st}

	a.markOwner("55500000021@s.whatsapp.net", "Yo")

	c, ok, err := st.GetChat("55500000021@s.whatsapp.net")
	if err != nil || !ok {
		t.Fatalf("GetChat: ok=%v err=%v", ok, err)
	}
	if !c.IsBoss {
		t.Error("IsBoss = false after markOwner on a clean install, want true")
	}
}

// TestMarkOwnerSurvivesReconnect: markOwner runs on EVERY *events.Connected
// (recordOwnIdentity fires on every reconnect, not just the first pairing —
// see handleConnected/the PushNameSetting retry). A second call (simulating
// a reconnect) must not do anything strange to an already-marked chat.
func TestMarkOwnerSurvivesReconnect(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "piumy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a := &Adapter{inbound: make(chan gateway.Inbound, 4), store: st}
	ownJID := "55500000021@s.whatsapp.net"

	a.markOwner(ownJID, "Yo")
	a.markOwner(ownJID, "Yo")

	c, ok, err := st.GetChat(ownJID)
	if err != nil || !ok {
		t.Fatalf("GetChat: ok=%v err=%v", ok, err)
	}
	if !c.IsBoss {
		t.Error("IsBoss = false after a second markOwner call, want it to stay true")
	}
}

// TestMarkOwnerRespectsManualUnmark: if the owner explicitly unmarked his
// own chat (SetIsBoss(false), via the REST admin path), a later reconnect
// must never re-apply the auto-mark — that would fight the owner's own
// decision (contract's explicit "no se la vuelvas a poner" requirement).
func TestMarkOwnerRespectsManualUnmark(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "piumy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a := &Adapter{inbound: make(chan gateway.Inbound, 4), store: st}
	ownJID := "55500000021@s.whatsapp.net"

	a.markOwner(ownJID, "Yo")
	if err := st.SetIsBoss(ownJID, false); err != nil {
		t.Fatalf("SetIsBoss(false): %v", err)
	}

	a.markOwner(ownJID, "Yo") // simulates a reconnect

	c, ok, err := st.GetChat(ownJID)
	if err != nil || !ok {
		t.Fatalf("GetChat: ok=%v err=%v", ok, err)
	}
	if c.IsBoss {
		t.Error("IsBoss = true after a reconnect following a manual unmark, want it to stay false")
	}
}

// TestMarkOwnerLeavesOtherChatsAlone: markOwner is only ever called with
// state.OwnJID — every other chat, including one already marked boss by
// hand, must be untouched (T12's "ningún otro chat cambia" acceptance
// criterion).
func TestMarkOwnerLeavesOtherChatsAlone(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "piumy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a := &Adapter{inbound: make(chan gateway.Inbound, 4), store: st}
	other := "55500000074@s.whatsapp.net"
	if err := st.TouchChat(other, "Otro", 1); err != nil {
		t.Fatal(err)
	}

	a.markOwner("55500000021@s.whatsapp.net", "Yo")

	c, ok, err := st.GetChat(other)
	if err != nil || !ok {
		t.Fatalf("GetChat: ok=%v err=%v", ok, err)
	}
	if c.IsBoss {
		t.Error("IsBoss = true on a chat that is not OwnJID, want it left alone")
	}
}

// TestMarkOwnerNilStoreIsNoOp: same nil-safe convention as Store/Router
// elsewhere in this package.
func TestMarkOwnerNilStoreIsNoOp(t *testing.T) {
	a := &Adapter{inbound: make(chan gateway.Inbound, 4)}
	a.markOwner("55500000021@s.whatsapp.net", "Yo")
}

// TestSeedGroupsPopulatesGroupMembersAndDescription covers ct-2026-07-19-0138
// (backup Sub 2b): seedGroups scrapes g.Participants into group_members
// (Sub 1's table, via UpsertGroupMember) and the topic into
// chats.description. Store-only, no live whatsmeow client needed —
// g.Participants already came with GetJoinedGroups by the time seedGroups
// runs. Since T18B (ct-2026-08-05-1243), group_members is ALSO what
// GroupsOf/ChatOrigin's group_discovered read (chat_groups, the table this
// test used to assert seedGroups left untouched, was retired) — so this
// same write is now the single source for both "who's in this group" and
// "which groups is this number in".
func TestSeedGroupsPopulatesGroupMembersAndDescription(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "piumy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rt := router.NewManager(filepath.Join(t.TempDir(), "router.json"))
	a := &Adapter{inbound: make(chan gateway.Inbound, 4), store: st, router: rt}

	groupJID := types.NewJID("555000000000000001", "g.us")
	member1 := types.NewJID("111", "s.whatsapp.net")
	member2 := types.NewJID("222", "s.whatsapp.net")
	groups := []*types.GroupInfo{
		{
			JID:        groupJID,
			GroupName:  types.GroupName{Name: "Grupo De Prueba"},
			GroupTopic: types.GroupTopic{Topic: "Cumple el sábado"},
			Participants: []types.GroupParticipant{
				{JID: member1, DisplayName: "Alice"},
				{JID: member2}, // no DisplayName — the common case for a regular member
			},
		},
	}

	a.seedGroups(groups)

	chat, ok, err := st.GetChat(groupJID.String())
	if err != nil || !ok {
		t.Fatalf("GetChat = ok=%v err=%v", ok, err)
	}
	if chat.Description != "Cumple el sábado" {
		t.Errorf("Description = %q, want the group topic", chat.Description)
	}

	members, err := st.ListGroupMembers(groupJID.String())
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 2 {
		t.Fatalf("ListGroupMembers = %d, want 2", len(members))
	}
	byJID := map[string]store.GroupMember{}
	for _, m := range members {
		byJID[m.MemberJID] = m
	}
	if byJID[member1.String()].MemberName != "Alice" {
		t.Errorf("member1.MemberName = %q, want Alice", byJID[member1.String()].MemberName)
	}
	if byJID[member2.String()].MemberName != "" {
		t.Errorf("member2.MemberName = %q, want empty (no DisplayName)", byJID[member2.String()].MemberName)
	}

	// T18B: GroupsOf now reads group_members too — the same seedGroups
	// write above must be enough for it to report this group.
	memberGroups, err := st.GroupsOf(member1.String())
	if err != nil {
		t.Fatal(err)
	}
	if len(memberGroups) != 1 || memberGroups[0] != groupJID.String() {
		t.Errorf("GroupsOf(member1) = %v, want [%s]", memberGroups, groupJID.String())
	}

	// A re-seed with no DisplayName never erases a name already known.
	groups[0].Participants[0] = types.GroupParticipant{JID: member1}
	a.seedGroups(groups)
	members, err = st.ListGroupMembers(groupJID.String())
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		if m.MemberJID == member1.String() && m.MemberName != "Alice" {
			t.Errorf("after re-seed with blank DisplayName, member1.MemberName = %q, want it to keep Alice", m.MemberName)
		}
	}
}

// TestSeedGroupsNilStoreIsNoOp confirms the nil-safe convention (same as
// openwa.Adapter's Store/MediaDir): without a store, seedGroups must not
// panic, just skip persisting.
func TestSeedGroupsNilStoreIsNoOp(t *testing.T) {
	a := &Adapter{inbound: make(chan gateway.Inbound, 4)}
	a.seedGroups([]*types.GroupInfo{{JID: types.NewJID("1", "g.us")}})
}

// TestHandleEventTripsKillSwitchOnLoggedOutAndTemporaryBan is the H6
// hardening regression (ct-2026-07-10-0540): these two events mean the
// account cannot recover on its own — the gateway must stop sending until
// the owner intervenes, not silently keep queueing into a dead connection.
func TestHandleEventTripsKillSwitchOnLoggedOutAndTemporaryBan(t *testing.T) {
	cases := []struct {
		name string
		evt  any
	}{
		{"LoggedOut", &events.LoggedOut{Reason: events.ConnectFailureLoggedOut}},
		{"TemporaryBan", &events.TemporaryBan{Code: events.TempBanBlockedByUsers}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sm := state.NewManager(filepath.Join(t.TempDir(), "status.json"), 8)
			gov := governor.NewLimiter(10, time.Minute)
			a := &Adapter{inbound: make(chan gateway.Inbound, 4), state: sm, governor: gov}

			a.handleEvent(c.evt)

			if !gov.Killed() {
				t.Error("governor.Killed() = false, want true")
			}
			snap := sm.Snapshot()
			if !snap.Muted || snap.Mood != "error" || snap.WAConnected {
				t.Errorf("state = %+v, want Muted=true Mood=error WAConnected=false", snap)
			}
		})
	}
}

// TestHandleEventDoesNotKillOnRecoverableEvents covers the other 3 "silent
// death" cases the H6 hardening also added — these update the display but
// must NOT trip the kill switch: StreamReplaced/ClientOutdated aren't
// necessarily unrecoverable, and Disconnected is whatsmeow's own
// auto-reconnecting case (not in its PermanentDisconnect set).
func TestHandleEventDoesNotKillOnRecoverableEvents(t *testing.T) {
	cases := []struct {
		name string
		evt  any
	}{
		{"StreamReplaced", &events.StreamReplaced{}},
		{"ClientOutdated", &events.ClientOutdated{}},
		{"Disconnected", &events.Disconnected{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sm := state.NewManager(filepath.Join(t.TempDir(), "status.json"), 8)
			gov := governor.NewLimiter(10, time.Minute)
			a := &Adapter{inbound: make(chan gateway.Inbound, 4), state: sm, governor: gov}

			a.handleEvent(c.evt)

			if gov.Killed() {
				t.Error("governor.Killed() = true, want false — this event must not trip the kill switch")
			}
			snap := sm.Snapshot()
			if snap.Muted {
				t.Error("state.Muted = true, want false")
			}
			if snap.Mood != "error" || snap.WAConnected {
				t.Errorf("state = %+v, want Mood=error WAConnected=false (display only)", snap)
			}
		})
	}
}

// TestHandleEventDisconnectNilSafe confirms the nil-safe convention (same
// as Store/Router): without bus/state/governor wired, handling a
// disconnect event must not panic.
func TestHandleEventDisconnectNilSafe(t *testing.T) {
	a := &Adapter{inbound: make(chan gateway.Inbound, 4)}
	a.handleEvent(&events.LoggedOut{Reason: events.ConnectFailureLoggedOut})
}

// TestClearErrorStateResetsMoodAndConnectedButNotKillSwitch is the
// reconnect half of the H6 regression: a successful reconnect (Connected)
// must clear the display-only error state, but must NOT silently un-kill a
// LoggedOut/TemporaryBan — that stays the owner's explicit call
// (set_kill_switch).
func TestClearErrorStateResetsMoodAndConnectedButNotKillSwitch(t *testing.T) {
	sm := state.NewManager(filepath.Join(t.TempDir(), "status.json"), 8)
	gov := governor.NewLimiter(10, time.Minute)
	a := &Adapter{inbound: make(chan gateway.Inbound, 4), state: sm, governor: gov}

	a.handleEvent(&events.LoggedOut{Reason: events.ConnectFailureLoggedOut})
	if !gov.Killed() {
		t.Fatal("setup: want governor killed after LoggedOut")
	}

	a.clearErrorState()

	snap := sm.Snapshot()
	if !snap.WAConnected {
		t.Error("WAConnected = false after clearErrorState, want true")
	}
	if snap.Mood == "error" {
		t.Error("Mood still error after clearErrorState, want it reset")
	}
	if !snap.Muted {
		t.Error("Muted = false after clearErrorState, want it left true — un-killing is the owner's call, not implicit in reconnecting")
	}
	if !gov.Killed() {
		t.Error("governor.Killed() = false after clearErrorState, want it left true")
	}
}

// TestClearErrorStateResetsQRMood covers the S1f half (ct-2026-07-19-1735):
// main.go sets Mood="qr" while a pairing code is pending — a successful
// connect must reset it away from "qr" too, not only "error", or the
// dashboard's carita (and its "still pairing" gate) would stay stuck
// showing the QR mood forever after the very first link.
func TestClearErrorStateResetsQRMood(t *testing.T) {
	sm := state.NewManager(filepath.Join(t.TempDir(), "status.json"), 8)
	if err := sm.Update(func(s *state.Status) { s.Mood = "qr"; s.ShowQR = true }); err != nil {
		t.Fatal(err)
	}
	a := &Adapter{inbound: make(chan gateway.Inbound, 4), state: sm}

	a.clearErrorState()

	snap := sm.Snapshot()
	if snap.Mood == "qr" {
		t.Error("Mood still qr after clearErrorState, want it reset")
	}
	if snap.ShowQR {
		t.Error("ShowQR = true after clearErrorState, want false")
	}
}

// TestClearErrorStatePublishesConnectedEvent: the dashboard's SSE listener
// (app.js) needs "wa_connected" to flip live from the QR screen to the
// admin panel — same nudge pattern handleDisconnect already uses for
// "wa_disconnected".
func TestClearErrorStatePublishesConnectedEvent(t *testing.T) {
	sm := state.NewManager(filepath.Join(t.TempDir(), "status.json"), 8)
	bus := eventbus.New()
	ch, unsubscribe := bus.Subscribe()
	defer unsubscribe()
	a := &Adapter{inbound: make(chan gateway.Inbound, 4), state: sm, bus: bus}

	a.clearErrorState()

	select {
	case ev := <-ch:
		if ev.Type != "wa_connected" {
			t.Errorf("event type = %q, want wa_connected", ev.Type)
		}
	default:
		t.Error("no event published on clearErrorState")
	}
}

// TestClearErrorStateNilBusIsNoOp: same nil-safe convention as Store/Router
// — clearErrorState must not panic without a bus wired (tests, a stripped
// build).
func TestClearErrorStateNilBusIsNoOp(t *testing.T) {
	sm := state.NewManager(filepath.Join(t.TempDir(), "status.json"), 8)
	a := &Adapter{inbound: make(chan gateway.Inbound, 4), state: sm}
	a.clearErrorState()
}

// TestKickResyncNilClientIsNoOp (D4, ct-2026-07-22-2100): a reset without a
// live WhatsApp session (client nil, e.g. tests or a not-yet-paired
// gateway) must not panic — nothing to re-sync from.
func TestKickResyncNilClientIsNoOp(t *testing.T) {
	a := &Adapter{inbound: make(chan gateway.Inbound, 4)}
	a.KickResync()
}

// TestHandleEventAppStateSyncCompleteContactsSchedulesSync is the other half
// of ct-2026-07-31's fix ("no llegan contactos en una instalación nueva"):
// appstate.WAPatchCriticalUnblockLow is verified against the vendored
// whatsmeow source (appstate/keys.go: "contains the user's contact list") —
// not assumed. A long debounce override means the timer will not actually
// fire during this test; the assertion is that scheduleContactsSync armed
// it at all, which is the thing handleEvent is responsible for.
func TestHandleEventAppStateSyncCompleteContactsSchedulesSync(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "piumy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a := &Adapter{inbound: make(chan gateway.Inbound, 4), store: st, contactsSyncDebounce: time.Hour}

	a.handleEvent(&events.AppStateSyncComplete{Name: appstate.WAPatchCriticalUnblockLow})

	if a.contactsSyncTimer == nil {
		t.Error("AppStateSyncComplete for the contact-list collection did not schedule a contacts sync")
	}
}

// TestHandleEventAppStateSyncCompleteOtherCollectionIgnored guards against
// over-triggering: AppStateSyncComplete fires once per collection
// (critical_block, regular, regular_high, regular_low too — appstate.
// AllPatchNames), and only the contact list's completion should schedule a
// re-sync. syncContacts walks every contact with a real anti-ban delay per
// contact — firing it for every unrelated collection would be wasteful
// with no benefit.
func TestHandleEventAppStateSyncCompleteOtherCollectionIgnored(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "piumy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a := &Adapter{inbound: make(chan gateway.Inbound, 4), store: st, contactsSyncDebounce: time.Hour}

	a.handleEvent(&events.AppStateSyncComplete{Name: appstate.WAPatchRegular})

	if a.contactsSyncTimer != nil {
		t.Error("AppStateSyncComplete for an unrelated collection (regular) scheduled a contacts sync")
	}
}

// TestHandleEventRetryReceiptLeavesATrace is the regression test for the
// real incident (ct-2026-08-07): a real contact received an undecryptable
// message from the gateway and nobody found out until she sent a
// screenshot a day later. types.ReceiptTypeRetry is WhatsApp telling the
// sender exactly that ("delivered to the device, but decrypting failed")
// — this confirms the gateway now logs it instead of silently dropping it
// in handleEvent's switch.
func TestHandleEventRetryReceiptLeavesATrace(t *testing.T) {
	a := newTestAdapter()
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })

	a.handleEvent(&events.Receipt{
		MessageSource: types.MessageSource{Chat: types.NewJID("555000001", "s.whatsapp.net")},
		MessageIDs:    []types.MessageID{"MSGID-UNDECRYPTABLE"},
		Type:          types.ReceiptTypeRetry,
	})

	got := buf.String()
	if !strings.Contains(got, "555000001") || !strings.Contains(got, "MSGID-UNDECRYPTABLE") {
		t.Errorf("retry receipt log = %q, want it to name the chat and the message id", got)
	}
	if !strings.Contains(got, "no se pudo descifrar") {
		t.Errorf("retry receipt log = %q, want it to say plainly that decryption failed", got)
	}
}

// TestHandleEventNonRetryReceiptStaysSilent guards the other half of the
// contract: *events.Receipt fires for EVERY acknowledgment kind
// (delivered, read, played...), not just retry — logging all of them would
// bury the one signal that actually matters back in the same noise this
// change exists to cut through.
func TestHandleEventNonRetryReceiptStaysSilent(t *testing.T) {
	for _, rt := range []types.ReceiptType{types.ReceiptTypeDelivered, types.ReceiptTypeRead, types.ReceiptTypeReadSelf, types.ReceiptTypePlayed, types.ReceiptTypeSender} {
		t.Run(string(rt), func(t *testing.T) {
			a := newTestAdapter()
			var buf bytes.Buffer
			orig := log.Writer()
			log.SetOutput(&buf)
			t.Cleanup(func() { log.SetOutput(orig) })

			a.handleEvent(&events.Receipt{
				MessageSource: types.MessageSource{Chat: types.NewJID("555000001", "s.whatsapp.net")},
				MessageIDs:    []types.MessageID{"MSGID-OK"},
				Type:          rt,
			})

			if got := buf.String(); got != "" {
				t.Errorf("receipt type %q logged %q, want silence — only retry receipts should leave a trace", rt, got)
			}
		})
	}
}
