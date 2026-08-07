// Avatar cache (T17 Parte 3, ct-2026-08-05-1240) — the delicate half of the
// contract: pedir fotos de perfil is activity toward WhatsApp's own
// servers, and counts for anti-ban the same as any other server-facing
// action. The boss's account has 719 numbers / 591 contacts — a bulk sweep
// over that is exactly the pattern that gets an account banned. So:
//   - NEVER a sweep. RequestAvatar is only ever called from the REST layer
//     for a chat the dashboard is actually showing right now (header or a
//     visible list row) — see restapi's handleAvatar.
//   - Even on-demand goes through a paced queue, not a synchronous fetch
//     per request — a dashboard page with many visible chats is a burst in
//     disguise if nothing paces it (Citrino's own framing of the risk).
//   - A cached avatar is served from disk unconditionally; re-ASKING
//     WhatsApp whether it changed is what's rate-limited, on a randomized
//     per-jid window (avatarRecheckWindow) — never a fixed interval
//     (Citrino's correction on the first draft of this contract: "cada 7
//     días" es un patrón, y los patrones son lo que se detecta).
package whatsmeow

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	wmeow "go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"

	"piumy-gateway/internal/governor"
	"piumy-gateway/internal/store"
)

// avatarQueueCap bounds RequestAvatar's in-memory queue — comfortably above
// a single dashboard page's worth of visible chats (header + a scrolled
// list), so a normal view never drops a hint. A queue this full already
// means something unusual is driving many chats open at once; dropping
// the overflow (RequestAvatar's own doc) is the correct response, not
// growing the buffer to absorb it.
const avatarQueueCap = 64

// avatarHTTPTimeout bounds the plain HTTP GET that downloads the actual
// image bytes once GetProfilePictureInfo hands back a URL — same
// dedicated-timeout convention as CleverInjector's own httpClient
// (capipush/clever_injector.go): a stuck download must not wedge the
// worker loop forever.
const avatarHTTPTimeout = 15 * time.Second

// avatarMaxBytes caps the downloaded image — WhatsApp's own profile
// pictures are small in practice (well under 200KB), this is a safety net
// against a misbehaving/unexpected response, not a real-world ceiling.
// Matches capi-protocolo.md §3's own "attached.data ≤5mb" convention.
const avatarMaxBytes = 5 * 1024 * 1024

// defaultAvatarRecheckMin/Max are avatarRecheckWindow's code-level
// fallback (CLAUDE.md: "cero hardcode" — config.Load()'s
// PIUMY_AVATAR_RECHECK_MIN/MAX is the real default; these only cover a
// build that skips config.Load, e.g. a test). 3-9 days keeps the same
// rough weekly order of magnitude the first draft of this contract
// proposed as a flat "7 days" — Citrino's correction: a FIXED interval is
// a pattern, and patterns are what gets detected. Every jid samples its
// own value independently (governor.DelayWindow.Random(), the SAME
// mechanism the second-scale delays elsewhere in this package already
// use — reused, not reinvented), so no two numbers ever land on the same
// cadence, and the sampled value itself is never round either way.
const (
	defaultAvatarRecheckMin = 3 * 24 * time.Hour
	defaultAvatarRecheckMax = 9 * 24 * time.Hour
)

// avatarRecheckWindow is the randomized per-jid staleness window — same
// governor.DelayWindow mechanism/live-KV-override convention as
// actionDelay() (sync.go), days-scale instead of seconds-scale. Sampled
// fresh (Random()) on every check, so next_check_at is never the same
// offset twice in a row for the same number either.
func (a *Adapter) avatarRecheckWindow() governor.DelayWindow {
	return governor.NewDelayWindow(
		a.store.SettingDuration(store.SettingAvatarRecheckMin, a.avatarRecheckMin),
		a.store.SettingDuration(store.SettingAvatarRecheckMax, a.avatarRecheckMax),
		defaultAvatarRecheckMin, defaultAvatarRecheckMax,
	)
}

// RequestAvatar enqueues jid for a paced background check — called from
// the REST layer (restapi.Deps.Avatars) when the dashboard is actually
// showing jid right now (header or a visible chat-list row), NEVER from a
// sweep over every known number (this file's own package doc). Non-
// blocking: a full queue, or a jid already queued, just drops the hint —
// avatarQueue/avatarQueued's own doc on Adapter. Safe with a nil store
// (does nothing — nowhere to persist a check result anyway).
func (a *Adapter) RequestAvatar(jid string) {
	if a.store == nil || jid == "" {
		return
	}
	if _, alreadyQueued := a.avatarQueued.LoadOrStore(jid, struct{}{}); alreadyQueued {
		return
	}
	select {
	case a.avatarQueue <- jid:
	default:
		a.avatarQueued.Delete(jid) // queue full — the next view re-requests
	}
}

// avatarWorkerLoop drains RequestAvatar's queue, one PACED real check at a
// time — actionDelay() (the same seconds-scale anti-ban window the
// contact/media backfills already use) is slept BEFORE every dequeue's
// actual protocol call, so a burst of on-demand requests (a dashboard page
// with several visible chats) still goes out one at a time, spaced out —
// "bajo demanda" without this pacing is a bulk sweep wearing a different
// trigger.
func (a *Adapter) avatarWorkerLoop(ctx context.Context) {
	if a.store == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case jid := <-a.avatarQueue:
			a.avatarQueued.Delete(jid)
			a.actionDelay().Sleep(ctx)
			if ctx.Err() != nil {
				return
			}
			if !a.client.IsConnected() || a.killSwitchActive() {
				continue // dropped; the next view re-requests
			}
			a.checkAvatar(ctx, jid)
		}
	}
}

// checkAvatar performs ONE real check for jid — split from avatarWorkerLoop
// so it's testable without a live client's timing (same convention as
// media.go's downloadNextPendingMedia). Reads the cached row first:
// RequestAvatar has no idea whether jid's cache is already fresh (the
// dashboard just says "this is visible now"), so a jid queued while still
// within its own next_check_at window is a no-op — zero protocol calls.
func (a *Adapter) checkAvatar(ctx context.Context, jid string) {
	existing, hasExisting, err := a.store.GetAvatar(jid)
	if err != nil {
		log.Printf("whatsmeow: avatar %s: read cache: %v", jid, err)
		return
	}
	now := time.Now()
	if hasExisting && existing.NextCheckAt > now.Unix() {
		return
	}

	parsed, err := types.ParseJID(jid)
	if err != nil {
		log.Printf("whatsmeow: avatar %s: parse jid: %v", jid, err)
		return
	}
	params := &wmeow.GetProfilePictureParams{}
	if hasExisting {
		// The protocol-level conditional check (§ this file's package
		// doc): if the picture id we already have still matches, the
		// server returns (nil, nil) below with NO image data transferred
		// at all — this is what makes the anti-ban lever "how often we
		// ask", not "how much we download".
		params.ExistingID = existing.PictureID
	}
	nextCheckAt := now.Add(a.avatarRecheckWindow().Random()).Unix()

	info, err := a.client.GetProfilePictureInfo(ctx, parsed, params)
	switch {
	case errors.Is(err, wmeow.ErrProfilePictureNotSet):
		// Confirmed: no photo (possibly a CHANGE from before — the user
		// removed their photo). Clear any stale cached file so the
		// dashboard falls back to initials instead of serving a photo
		// that no longer represents this contact.
		a.clearCachedAvatarFile(existing)
		if err := a.store.UpsertAvatar(store.Avatar{JID: jid, NextCheckAt: nextCheckAt}); err != nil {
			log.Printf("whatsmeow: avatar %s: clear: %v", jid, err)
			return
		}
		log.Printf("whatsmeow: avatar %s sin foto — próximo chequeo en %s", jid, time.Until(time.Unix(nextCheckAt, 0)).Round(time.Hour))
	case err != nil:
		// Any other failure (privacy/network/protocol) still counts as
		// "we asked" — bump the window regardless of the outcome (the
		// anti-ban lever is asking-frequency, not success); cached file,
		// if any, is left untouched.
		if bumpErr := a.store.UpsertAvatar(store.Avatar{
			JID: jid, PictureID: existing.PictureID, Path: existing.Path,
			FetchedAt: existing.FetchedAt, NextCheckAt: nextCheckAt,
		}); bumpErr != nil {
			log.Printf("whatsmeow: avatar %s: bump after error: %v", jid, bumpErr)
			return
		}
		log.Printf("whatsmeow: avatar %s: %v — próximo chequeo en %s", jid, err, time.Until(time.Unix(nextCheckAt, 0)).Round(time.Hour))
	case info == nil:
		// Unchanged (WhatsApp confirmed our ExistingID still matches) —
		// nothing to download, just bump the window.
		if err := a.store.UpsertAvatar(store.Avatar{
			JID: jid, PictureID: existing.PictureID, Path: existing.Path,
			FetchedAt: now.Unix(), NextCheckAt: nextCheckAt,
		}); err != nil {
			log.Printf("whatsmeow: avatar %s: bump unchanged: %v", jid, err)
		}
	default:
		a.downloadAndCacheAvatar(ctx, jid, info, nextCheckAt)
	}
}

// clearCachedAvatarFile removes existing's file from disk, if any — the
// "confirmed no photo" outcome must not keep serving a stale image.
func (a *Adapter) clearCachedAvatarFile(existing store.Avatar) {
	if existing.Path == "" {
		return
	}
	if err := os.Remove(existing.Path); err != nil && !os.IsNotExist(err) {
		log.Printf("whatsmeow: avatar clear file %s: %v", existing.Path, err)
	}
}

// downloadAndCacheAvatar downloads info.URL (a NEW or first-ever photo)
// and persists it — same save-to-disk shape as media.go's saveMedia, a
// dedicated avatars/ subdirectory under mediaDir. One plain HTTP GET to
// the URL WhatsApp itself handed back; the anti-ban-relevant protocol call
// already happened in GetProfilePictureInfo (checkAvatar, above) — this is
// just fetching bytes from a URL, same as any inbound media download.
func (a *Adapter) downloadAndCacheAvatar(ctx context.Context, jid string, info *types.ProfilePictureInfo, nextCheckAt int64) {
	if a.mediaDir == "" {
		return // nowhere to save — same nil-safe convention as media.go's downloadAndStoreMedia
	}
	fetchCtx, cancel := context.WithTimeout(ctx, avatarHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, info.URL, nil)
	if err != nil {
		log.Printf("whatsmeow: avatar download %s: build request: %v", jid, err)
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("whatsmeow: avatar download %s: %v", jid, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("whatsmeow: avatar download %s: status %d", jid, resp.StatusCode)
		return
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, avatarMaxBytes))
	if err != nil {
		log.Printf("whatsmeow: avatar download %s: read: %v", jid, err)
		return
	}

	mime, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	ext := extensionFor(strings.TrimSpace(mime))

	dir := filepath.Join(a.mediaDir, "avatars")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("whatsmeow: avatar save %s: mkdir: %v", jid, err)
		return
	}
	path := filepath.Join(dir, safeMediaName(jid, ext))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		log.Printf("whatsmeow: avatar save %s: %v", jid, err)
		return
	}

	if err := a.store.UpsertAvatar(store.Avatar{
		JID: jid, PictureID: info.ID, Path: path,
		FetchedAt: time.Now().Unix(), NextCheckAt: nextCheckAt,
	}); err != nil {
		log.Printf("whatsmeow: avatar store %s: %v", jid, err)
		return
	}
	log.Printf("whatsmeow: avatar %s actualizado (%d bytes) — próximo chequeo en %s", jid, len(data), time.Until(time.Unix(nextCheckAt, 0)).Round(time.Hour))
}
