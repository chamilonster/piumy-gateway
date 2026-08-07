// Group/profile tools (F4-DESIGN §5, boss-only) — call d.GroupProfile
// directly, not through gateway.Gateway (see Deps.GroupProfile's doc
// comment for why). d.GroupProfile nil (not wired, or a future adapter
// that doesn't support group/profile admin) means every tool here refuses
// cleanly.
//
// ST-E (ct-2026-07-11-1444): cabled to whatsmeow (*whatsmeow.Adapter
// satisfies GroupProfile), replacing the deleted internal/openwa. Two
// boss decisions from that migration: set_profile_name became
// set_profile_status (whatsmeow has no API for the display name, only the
// "About" status text — SetStatusMessage); set_profile_pic is gone
// entirely (whatsmeow has no API for the own profile picture).
package mcpserver

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"go.mau.fi/whatsmeow/types"

	"piumy-gateway/internal/mediautil"
)

// GroupProfile is the subset of *whatsmeow.Adapter this file needs — group
// admin isn't part of the gateway.Gateway seam (F2): that interface is for
// Send/SetTyping/MarkRead, the cross-vendor messaging concept every
// adapter shares. Groups aren't — inflating Gateway for this one
// implementer would couple every future adapter to a group-admin shape
// only WhatsApp has. Defined here (not imported from whatsmeow) so a fake
// can drive this file's own logic (JSON shape, data-URL decode, error
// paths) in tests without a live WhatsApp session.
type GroupProfile interface {
	CreateGroup(ctx context.Context, name string, participantJIDs []string) (*types.GroupInfo, error)
	AddParticipant(ctx context.Context, groupJID, participantJID string) ([]types.GroupParticipant, error)
	SetGroupPhoto(ctx context.Context, groupJID string, jpeg []byte) (string, error)
	SetGroupDescription(ctx context.Context, groupJID, description string) error
	SetProfileStatus(ctx context.Context, status string) error
}

const groupProfileNotAvailable = "not available: no group/profile client wired"

func addGroupTools(s *server.MCPServer, d Deps, tracker *agentTracker) {
	// ── create_group ─────────────────────────────────────────────────────
	s.AddTool(mcp.NewTool("create_group",
		mcp.WithDescription("OWNER-ONLY. Create a new WhatsApp group with the given participants."),
		mcp.WithString("name", mcp.Required()),
		mcp.WithArray("participants", mcp.Required(), mcp.Description("Participant JIDs, e.g. 55500000001@c.us"), mcp.Items(map[string]any{"type": "string"}))),
		func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			tracker.seen(ctx)
			if d.GroupProfile == nil {
				return mcp.NewToolResultError(groupProfileNotAvailable), nil
			}
			name, err := r.RequireString("name")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			participants, err := r.RequireStringSlice("participants")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			info, err := d.GroupProfile.CreateGroup(ctx, name, participants)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return jsonResult(info)
		})

	// ── add_participant ──────────────────────────────────────────────────
	s.AddTool(mcp.NewTool("add_participant",
		mcp.WithDescription("OWNER-ONLY. Add one participant to an existing WhatsApp group."),
		mcp.WithString("group_id", mcp.Required()),
		mcp.WithString("participant_id", mcp.Required())),
		func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			tracker.seen(ctx)
			if d.GroupProfile == nil {
				return mcp.NewToolResultError(groupProfileNotAvailable), nil
			}
			groupID, err := r.RequireString("group_id")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			participantID, err := r.RequireString("participant_id")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			participants, err := d.GroupProfile.AddParticipant(ctx, groupID, participantID)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return jsonResult(participants)
		})

	// ── set_group_icon ───────────────────────────────────────────────────
	s.AddTool(mcp.NewTool("set_group_icon",
		mcp.WithDescription("OWNER-ONLY. Set a group's icon from a data: URL image string (no media pipeline yet, F4d — supply a data URL directly)."),
		mcp.WithString("group_id", mcp.Required()),
		mcp.WithString("data_url", mcp.Required(), mcp.Description("A data: URL, e.g. data:image/jpeg;base64,..."))),
		func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			tracker.seen(ctx)
			if d.GroupProfile == nil {
				return mcp.NewToolResultError(groupProfileNotAvailable), nil
			}
			groupID, err := r.RequireString("group_id")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			dataURL, err := r.RequireString("data_url")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			raw, _, err := mediautil.DecodeDataURL(dataURL)
			if err != nil {
				return mcp.NewToolResultError("invalid data_url: " + err.Error()), nil
			}
			if _, err := d.GroupProfile.SetGroupPhoto(ctx, groupID, raw); err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText("group icon set"), nil
		})

	// ── set_group_description ────────────────────────────────────────────
	s.AddTool(mcp.NewTool("set_group_description",
		mcp.WithDescription("OWNER-ONLY. Change a group's description/topic."),
		mcp.WithString("group_id", mcp.Required()),
		mcp.WithString("description", mcp.Required())),
		func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			tracker.seen(ctx)
			if d.GroupProfile == nil {
				return mcp.NewToolResultError(groupProfileNotAvailable), nil
			}
			groupID, err := r.RequireString("group_id")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			description, err := r.RequireString("description")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			if err := d.GroupProfile.SetGroupDescription(ctx, groupID, description); err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText("group description set"), nil
		})

	// ── set_profile_status ───────────────────────────────────────────────
	s.AddTool(mcp.NewTool("set_profile_status",
		mcp.WithDescription("OWNER-ONLY. Set the host number's own status/\"About\" text — NOT the display name (whatsmeow has no API to change that). Renamed from set_profile_name (ST-E, ct-2026-07-11-1444) to avoid confusing agents/skills about what it actually changes."),
		mcp.WithString("status", mcp.Required())),
		func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			tracker.seen(ctx)
			if d.GroupProfile == nil {
				return mcp.NewToolResultError(groupProfileNotAvailable), nil
			}
			status, err := r.RequireString("status")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			if err := d.GroupProfile.SetProfileStatus(ctx, status); err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText("profile status set"), nil
		})
}
