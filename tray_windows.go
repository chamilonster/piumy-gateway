//go:build windows

package main

import (
	"context"
	_ "embed"
	"log"
	"os/exec"

	"fyne.io/systray"

	"piumy-gateway/internal/version"
)

// trayIcon is la carita Piumy (círculo verde fósforo sobre negro) en
// 16/32/48 px — generado con un programa descartable stdlib-only
// (image/png + un contenedor ICO escrito a mano) desde el logo de marca y
// committeado como asset estático; ver docs/MANUAL.md para cómo se hizo.
//
//go:embed assets/tray.ico
var trayIcon []byte

// runTrayOrWait shows a Windows system tray icon (ct-2026-07-10-2312, F3) —
// menu version (disabled) / "Abrir dashboard" / "Salir" — and blocks until
// shutdown. Three ways out, all converging on the same graceful-shutdown
// path main() already has: "Salir" calls stop() (identical to Ctrl+C);
// ctx.Done() firing externally (Ctrl+C) calls systray.Quit() so the icon
// never lingers after the rest of the process starts tearing down; and
// Windows itself (shutdown, logoff, or a taskkill without /F) sends
// WM_CLOSE/WM_ENDSESSION, which fyne.io/systray's own wndProc turns into a
// call to onExit below (ct-2026-08-07) — before this, that third path
// reached systray and stopped there, never calling stop(), so the process
// could outlive its own tray icon.
//
// fyne.io/systray is CGO-free on Windows (syscall + golang.org/x/sys/windows
// only — verified with an explicit CGO_ENABLED=0 build before adding this
// dependency; its only cgo file is systray_darwin.go, never compiled here) —
// CGO_ENABLED=0 stays intact, the project's central invariant.
func runTrayOrWait(ctx context.Context, stop context.CancelFunc, dashboardURL string) {
	systray.Run(func() {
		// T37 (ct-2026-08-08-1433, boss: "quiero que el tray diga la version
		// de piumy" — acotado después, verbatim: "en el tray en el menú, no
		// al pasar el mouse") — solo el ítem de menú. El tooltip/título
		// quedan sin tocar a propósito, no es un olvido.
		systray.SetIcon(trayIcon)
		systray.SetTitle("Piumy Gateway")
		systray.SetTooltip("Piumy Gateway")
		mVersion := systray.AddMenuItem("Piumy Gateway "+version.Version, "Versión de piumy-gateway corriendo")
		mVersion.Disable()
		mOpen := systray.AddMenuItem("Abrir dashboard", "Abrir el dashboard en una ventana")
		mQuit := systray.AddMenuItem("Salir", "Cerrar piumy-gateway")

		go func() {
			for {
				select {
				case <-mOpen.ClickedCh:
					openAppWindow(dashboardURL)
				case <-mQuit.ClickedCh:
					stop()
					systray.Quit()
					return
				case <-ctx.Done():
					stop()
					systray.Quit()
					return
				}
			}
		}()
	}, func() { stop() })
}

// openAppWindow launches url as a chromeless "app window" — msedge first
// (ships with every modern Windows), chrome fallback, then the OS default
// browser as a last resort (a normal tab, not chromeless, but never fails
// silently). Each Start() is fire-and-forget — the spawned browser outlives
// this process's own lifecycle checks, same as double-clicking a shortcut.
func openAppWindow(url string) {
	for _, browser := range []string{"msedge", "chrome"} {
		if err := exec.Command(browser, "--app="+url).Start(); err == nil {
			return
		}
	}
	if err := exec.Command("cmd", "/c", "start", url).Start(); err != nil {
		log.Printf("tray: open dashboard: %v", err)
	}
}
