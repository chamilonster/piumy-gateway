// ST-E (ct-2026-07-11-1444): group_tools.go never had its own test file —
// every existing test only exercised the "GroupProfile nil" refusal path
// (see admin_tools_test.go). fakeGroupProfile lets these tests drive the
// tools' OWN logic (JSON shape, the data-URL decode step, error
// propagation) without a live WhatsApp session.
package mcpserver

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/server"
	"go.mau.fi/whatsmeow/types"

	"piumy-gateway/internal/router"
	"piumy-gateway/internal/state"
	"piumy-gateway/internal/store"
)

type fakeGroupProfile struct {
	err error // if set, every method returns this instead of succeeding

	lastCreateGroupName         string
	lastCreateGroupParticipants []string
	lastAddParticipantGroup     string
	lastAddParticipantID        string
	lastSetGroupPhotoGroup      string
	lastSetGroupPhotoBytes      []byte
	lastSetGroupDescGroup       string
	lastSetGroupDescText        string
	lastSetProfileStatus        string
}

func (f *fakeGroupProfile) CreateGroup(ctx context.Context, name string, participantJIDs []string) (*types.GroupInfo, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.lastCreateGroupName = name
	f.lastCreateGroupParticipants = participantJIDs
	return &types.GroupInfo{GroupName: types.GroupName{Name: name}}, nil
}

func (f *fakeGroupProfile) AddParticipant(ctx context.Context, groupJID, participantJID string) ([]types.GroupParticipant, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.lastAddParticipantGroup = groupJID
	f.lastAddParticipantID = participantJID
	return []types.GroupParticipant{{}}, nil
}

func (f *fakeGroupProfile) SetGroupPhoto(ctx context.Context, groupJID string, jpeg []byte) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.lastSetGroupPhotoGroup = groupJID
	f.lastSetGroupPhotoBytes = jpeg
	return "photo-id", nil
}

func (f *fakeGroupProfile) SetGroupDescription(ctx context.Context, groupJID, description string) error {
	if f.err != nil {
		return f.err
	}
	f.lastSetGroupDescGroup = groupJID
	f.lastSetGroupDescText = description
	return nil
}

func (f *fakeGroupProfile) SetProfileStatus(ctx context.Context, status string) error {
	if f.err != nil {
		return f.err
	}
	f.lastSetProfileStatus = status
	return nil
}

// serverWithGroupProfile builds a boss-dispatched server wired to fgp —
// same shape as helpers_test.go's newTestServer/bossDispatchContext, just
// with GroupProfile set (those helpers don't expose that field).
func serverWithGroupProfile(t *testing.T, fgp *fakeGroupProfile) (context.Context, *server.MCPServer) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "piumy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	rtMgr := router.NewManager(filepath.Join(dir, "router.json"))
	sm := state.NewManager(filepath.Join(dir, "status.json"), 8)
	gate := NewGate()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	srv := New(ctx, Deps{Store: st, State: sm, Router: rtMgr, AgentIdle: time.Minute, Gate: gate, GroupProfile: fgp})
	chat := "55500000066@c.us"
	termCtx := bossDispatchContext(t, gate, srv, ctx, chat)
	return termCtx, srv
}

func TestCreateGroupCallsGroupProfileAndReturnsJSON(t *testing.T) {
	fgp := &fakeGroupProfile{}
	ctx, srv := serverWithGroupProfile(t, fgp)

	out := callTool(t, ctx, srv, "create_group", map[string]any{
		"name": "Asado del viernes", "participants": []string{"111@c.us", "222@c.us"},
	})
	if fgp.lastCreateGroupName != "Asado del viernes" {
		t.Errorf("CreateGroup name = %q, want %q", fgp.lastCreateGroupName, "Asado del viernes")
	}
	if len(fgp.lastCreateGroupParticipants) != 2 {
		t.Errorf("CreateGroup participants = %v, want 2 entries", fgp.lastCreateGroupParticipants)
	}
	if !strings.Contains(out, "Asado del viernes") {
		t.Errorf("create_group result = %s, want the group name serialized", out)
	}
}

func TestAddParticipantCallsGroupProfile(t *testing.T) {
	fgp := &fakeGroupProfile{}
	ctx, srv := serverWithGroupProfile(t, fgp)

	callTool(t, ctx, srv, "add_participant", map[string]any{
		"group_id": "g1@g.us", "participant_id": "333@c.us",
	})
	if fgp.lastAddParticipantGroup != "g1@g.us" || fgp.lastAddParticipantID != "333@c.us" {
		t.Errorf("AddParticipant got group=%q participant=%q, want g1@g.us / 333@c.us", fgp.lastAddParticipantGroup, fgp.lastAddParticipantID)
	}
}

// TestSetGroupIconDecodesDataURLBeforeCallingGroupProfile is the ST-E
// angle Amatista flagged: set_group_icon receives data_url, but
// SetGroupPhoto wants raw jpeg bytes — the decode has to happen in
// group_tools.go itself.
func TestSetGroupIconDecodesDataURLBeforeCallingGroupProfile(t *testing.T) {
	fgp := &fakeGroupProfile{}
	ctx, srv := serverWithGroupProfile(t, fgp)

	// "aGVsbG8=" is base64 for "hello".
	out := callTool(t, ctx, srv, "set_group_icon", map[string]any{
		"group_id": "g1@g.us", "data_url": "data:image/jpeg;base64,aGVsbG8=",
	})
	if !strings.Contains(out, "group icon set") {
		t.Errorf("set_group_icon = %s, want success", out)
	}
	if fgp.lastSetGroupPhotoGroup != "g1@g.us" {
		t.Errorf("SetGroupPhoto group = %q, want g1@g.us", fgp.lastSetGroupPhotoGroup)
	}
	if string(fgp.lastSetGroupPhotoBytes) != "hello" {
		t.Errorf("SetGroupPhoto bytes = %q, want the DECODED payload %q, not the raw data_url", fgp.lastSetGroupPhotoBytes, "hello")
	}
}

// TestSetGroupIconRejectsMalformedDataURLWithoutCallingGroupProfile: a
// decode failure must be caught in group_tools.go, before ever reaching
// the client — SetGroupPhoto must not be called with garbage.
func TestSetGroupIconRejectsMalformedDataURLWithoutCallingGroupProfile(t *testing.T) {
	fgp := &fakeGroupProfile{}
	ctx, srv := serverWithGroupProfile(t, fgp)

	out := callTool(t, ctx, srv, "set_group_icon", map[string]any{
		"group_id": "g1@g.us", "data_url": "not-a-data-url",
	})
	if !strings.Contains(out, "invalid data_url") {
		t.Errorf("set_group_icon with a malformed data_url = %s, want an invalid data_url error", out)
	}
	if fgp.lastSetGroupPhotoGroup != "" {
		t.Error("SetGroupPhoto was called despite the data_url failing to decode")
	}
}

func TestSetGroupDescriptionCallsGroupProfile(t *testing.T) {
	fgp := &fakeGroupProfile{}
	ctx, srv := serverWithGroupProfile(t, fgp)

	out := callTool(t, ctx, srv, "set_group_description", map[string]any{
		"group_id": "g1@g.us", "description": "grupo de prueba",
	})
	if !strings.Contains(out, "group description set") {
		t.Errorf("set_group_description = %s, want success", out)
	}
	if fgp.lastSetGroupDescGroup != "g1@g.us" || fgp.lastSetGroupDescText != "grupo de prueba" {
		t.Errorf("SetGroupDescription got group=%q text=%q, want g1@g.us / grupo de prueba", fgp.lastSetGroupDescGroup, fgp.lastSetGroupDescText)
	}
}

// TestSetProfileStatusCallsSetStatusMessage is the ST-E rename regression
// (ct-2026-07-11-1444): set_profile_name became set_profile_status and
// wraps whatsmeow's SetStatusMessage (the "About" text), never a display
// name — there is no whatsmeow API for that.
func TestSetProfileStatusCallsSetStatusMessage(t *testing.T) {
	fgp := &fakeGroupProfile{}
	ctx, srv := serverWithGroupProfile(t, fgp)

	out := callTool(t, ctx, srv, "set_profile_status", map[string]any{"status": "disponible"})
	if !strings.Contains(out, "profile status set") {
		t.Errorf("set_profile_status = %s, want success", out)
	}
	if fgp.lastSetProfileStatus != "disponible" {
		t.Errorf("SetProfileStatus got %q, want %q", fgp.lastSetProfileStatus, "disponible")
	}
}

// TestSetProfilePicToolNoLongerExists: whatsmeow has no API to change the
// own profile picture — the boss's decision was to remove the tool
// entirely, not leave a permanently-erroring stub.
func TestSetProfilePicToolNoLongerExists(t *testing.T) {
	for _, tool := range listTools(t) {
		if tool.Name == "set_profile_pic" {
			t.Error("set_profile_pic is still registered — ST-E removed it (whatsmeow has no API for it)")
		}
	}
}
