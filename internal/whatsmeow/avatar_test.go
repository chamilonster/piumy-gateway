package whatsmeow

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"

	"piumy-gateway/internal/store"
)

func openTestAvatarStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "piumy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// TestRequestAvatarDedupesAndDropsWhenFull covers RequestAvatar's two
// non-blocking guarantees (T17 Parte 3, ct-2026-08-05-1240): the SAME jid
// queued twice (e.g. the header and a visible list row both requesting it
// in one page load) occupies only one slot, and a full queue drops the
// overflow instead of blocking the REST request path.
func TestRequestAvatarDedupesAndDropsWhenFull(t *testing.T) {
	st := openTestAvatarStore(t)
	a := &Adapter{store: st, avatarQueue: make(chan string, 2)}

	a.RequestAvatar("1@s.whatsapp.net")
	a.RequestAvatar("1@s.whatsapp.net") // repeat — must not occupy a second slot
	a.RequestAvatar("2@s.whatsapp.net")
	if got := len(a.avatarQueue); got != 2 {
		t.Fatalf("queue depth = %d, want 2 (dedupe kept the repeat out)", got)
	}

	a.RequestAvatar("3@s.whatsapp.net") // queue full — must drop, not block
	if got := len(a.avatarQueue); got != 2 {
		t.Errorf("queue depth after overflow = %d, want still 2 (dropped, not blocked)", got)
	}
}

// TestRequestAvatarNilStoreIsNoOp: same nil-safe convention as
// markOwner/killSwitchActive elsewhere in this package.
func TestRequestAvatarNilStoreIsNoOp(t *testing.T) {
	a := &Adapter{}
	a.RequestAvatar("1@s.whatsapp.net") // must not panic
}

// TestCheckAvatarSkipsWhenStillFresh is the "bajo demanda sin cola sería
// una ráfaga" guard's OTHER half: RequestAvatar has no idea whether a jid's
// cache is already fresh, so checkAvatar itself must short-circuit BEFORE
// attempting any protocol call when the cached row's next_check_at hasn't
// arrived yet. Verified by asserting the store row is byte-identical after
// the call — any real check (even one that errors) always rewrites it with
// a freshly-sampled next_check_at.
func TestCheckAvatarSkipsWhenStillFresh(t *testing.T) {
	st := openTestAvatarStore(t)
	a := &Adapter{store: st} // a.client is nil — proves the skip never reaches it
	jid := "55500000021@s.whatsapp.net"
	want := store.Avatar{
		JID: jid, PictureID: "pic-1", Path: "cached.jpg",
		FetchedAt: 1000, NextCheckAt: time.Now().Add(48 * time.Hour).Unix(),
	}
	if err := st.UpsertAvatar(want); err != nil {
		t.Fatal(err)
	}

	a.checkAvatar(context.Background(), jid)

	got, ok, err := st.GetAvatar(jid)
	if err != nil || !ok {
		t.Fatalf("GetAvatar: ok=%v err=%v", ok, err)
	}
	if got != want {
		t.Errorf("avatar row = %+v after checkAvatar on a still-fresh jid, want unchanged %+v", got, want)
	}
}

// TestDownloadAndCacheAvatarSavesFileAndStoreRow covers the actual byte
// download + disk save + store row — this half needs no live whatsmeow
// client (unlike checkAvatar's GetProfilePictureInfo call, which needs a
// real protocol round-trip and isn't unit-tested here, same coverage
// boundary as media.go's downloadAndStoreMedia/downloadMediaPending).
func TestDownloadAndCacheAvatarSavesFileAndStoreRow(t *testing.T) {
	imgBytes := []byte{0xFF, 0xD8, 0xFF, 0xE0, 'j', 'f', 'i', 'f'}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(imgBytes)
	}))
	defer srv.Close()

	st := openTestAvatarStore(t)
	dir := t.TempDir()
	a := &Adapter{store: st, mediaDir: dir}

	jid := "55500000021@s.whatsapp.net"
	nextCheckAt := time.Now().Add(5 * 24 * time.Hour).Unix()
	a.downloadAndCacheAvatar(context.Background(), jid, &types.ProfilePictureInfo{URL: srv.URL, ID: "pic-new"}, nextCheckAt)

	got, ok, err := st.GetAvatar(jid)
	if err != nil || !ok {
		t.Fatalf("GetAvatar: ok=%v err=%v", ok, err)
	}
	if got.PictureID != "pic-new" {
		t.Errorf("PictureID = %q, want pic-new", got.PictureID)
	}
	if got.NextCheckAt != nextCheckAt {
		t.Errorf("NextCheckAt = %d, want %d", got.NextCheckAt, nextCheckAt)
	}
	if filepath.Ext(got.Path) != ".jpg" {
		t.Errorf("saved path %q, want a .jpg extension (from Content-Type: image/jpeg)", got.Path)
	}
	data, err := os.ReadFile(got.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, imgBytes) {
		t.Errorf("saved file bytes = %v, want %v", data, imgBytes)
	}
}

// TestDownloadAndCacheAvatarNoMediaDirIsNoOp: same nil-safe convention as
// downloadAndStoreMedia's own a.mediaDir=="" guard.
func TestDownloadAndCacheAvatarNoMediaDirIsNoOp(t *testing.T) {
	st := openTestAvatarStore(t)
	a := &Adapter{store: st} // mediaDir == ""
	a.downloadAndCacheAvatar(context.Background(), "1@s.whatsapp.net", &types.ProfilePictureInfo{URL: "http://unused"}, 0)
	if _, ok, _ := st.GetAvatar("1@s.whatsapp.net"); ok {
		t.Error("a row was written despite mediaDir being unset")
	}
}

// TestClearCachedAvatarFileRemovesFile covers the "confirmed no photo"
// outcome's cleanup — a stale cached image must not keep being served once
// WhatsApp confirms the contact no longer has one.
func TestClearCachedAvatarFileRemovesFile(t *testing.T) {
	a := &Adapter{}
	dir := t.TempDir()
	path := filepath.Join(dir, "old.jpg")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	a.clearCachedAvatarFile(store.Avatar{Path: path})

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file still exists after clearCachedAvatarFile: err=%v", err)
	}
}

// TestClearCachedAvatarFileEmptyPathIsNoOp: "never had a file" must not
// error (os.Remove("") would).
func TestClearCachedAvatarFileEmptyPathIsNoOp(t *testing.T) {
	a := &Adapter{}
	a.clearCachedAvatarFile(store.Avatar{Path: ""}) // must not panic
}

// TestAvatarRecheckWindowRespectsKVOverride covers the live-KV-override
// wiring (same convention as actionDelay()) — a dashboard-edited setting
// must apply without a restart.
func TestAvatarRecheckWindowRespectsKVOverride(t *testing.T) {
	st := openTestAvatarStore(t)
	a := &Adapter{store: st, avatarRecheckMin: 3 * 24 * time.Hour, avatarRecheckMax: 9 * 24 * time.Hour}

	w := a.avatarRecheckWindow()
	if w.Min != 3*24*time.Hour || w.Max != 9*24*time.Hour {
		t.Fatalf("window before override = %v/%v, want the Config fallback (3d/9d)", w.Min, w.Max)
	}

	if err := st.SetSettingDuration(store.SettingAvatarRecheckMin, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSettingDuration(store.SettingAvatarRecheckMax, 48*time.Hour); err != nil {
		t.Fatal(err)
	}

	w2 := a.avatarRecheckWindow()
	if w2.Min != 24*time.Hour || w2.Max != 48*time.Hour {
		t.Errorf("window after KV override = %v/%v, want 24h/48h", w2.Min, w2.Max)
	}
}

// TestAvatarWorkerLoopPacesRequestsWithVariableGaps is T17 Parte 3's
// required evidence (ct-2026-08-05-1240 — Citrino: "mostrame la evidencia
// del pacing: cuántas peticiones salieron, con qué separación. Es la parte
// que puede costar una cuenta"). Drains several queued jids through the
// REAL avatarWorkerLoop (not a shortcut — same goroutine Start() launches
// in production) and records the wall-clock moment each leaves the queue.
// actionDelay().Sleep runs unconditionally right after every dequeue,
// BEFORE the connection check (avatar.go) — so the gap between consecutive
// dequeues IS the real inter-request pacing, whether or not a live
// connection ends up letting checkAvatar's own protocol call through.
// Asserts what actually matters for anti-ban: never near-instant (a burst
// in disguise), and never the SAME gap twice — a repeated interval is
// exactly the fixed-pattern risk Citrino's own correction on this
// contract's first draft called out.
func TestAvatarWorkerLoopPacesRequestsWithVariableGaps(t *testing.T) {
	st := openTestAvatarStore(t)
	a := &Adapter{
		store:          st,
		avatarQueue:    make(chan string, 8),
		actionDelayMin: 30 * time.Millisecond,
		actionDelayMax: 90 * time.Millisecond,
	}
	const n = 5
	for i := 0; i < n; i++ {
		a.RequestAvatar(fmt.Sprintf("%d@s.whatsapp.net", i))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.avatarWorkerLoop(ctx)

	var dequeues []time.Time
	last := n
	deadline := time.Now().Add(5 * time.Second)
	for len(dequeues) < n && time.Now().Before(deadline) {
		if cur := len(a.avatarQueue); cur < last {
			now := time.Now()
			for i := 0; i < last-cur; i++ {
				dequeues = append(dequeues, now)
			}
			last = cur
		}
		time.Sleep(time.Millisecond)
	}
	if len(dequeues) < n {
		t.Fatalf("only observed %d/%d dequeues before the test deadline", len(dequeues), n)
	}

	t.Logf("T17 Parte 3 — evidencia del pacing real: %d peticiones, ventana configurada %v-%v", n, a.actionDelayMin, a.actionDelayMax)
	var gaps []time.Duration
	for i := 1; i < len(dequeues); i++ {
		gap := dequeues[i].Sub(dequeues[i-1])
		gaps = append(gaps, gap)
		t.Logf("  petición %d -> %d: separación %v", i, i+1, gap.Round(time.Millisecond))
		if gap < a.actionDelayMin {
			t.Errorf("separación %d = %v, want >= actionDelayMin (%v) — nunca casi-instantáneo, siempre paceado", i, gap, a.actionDelayMin)
		}
	}
	for i := 1; i < len(gaps); i++ {
		if gaps[i] == gaps[i-1] {
			t.Errorf("separación %d == separación %d (ambas %v) — un intervalo repetido es exactamente el patrón fijo que este diseño evita", i, i+1, gaps[i])
		}
	}
}
