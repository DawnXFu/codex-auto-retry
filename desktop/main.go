// Command codex-auto-resume-desktop is the Wails v2 desktop client for the
// Codex Auto Resume watchdog. Per ADR-0004 it is a pure protocol client: it
// embeds the shared dashboard frontend, discovers the daemon's loopback
// Management API via api.json in the runtime data dir, and imports no
// watchdog domain code. Closing the window exits only this client — the
// resident watchdog and tray are unaffected.
package main

import (
	"embed"
	"log"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// frontend/dist is staged into this directory by build.ps1 (npm build output
// copied from the repo-root frontend/ workspace). The .keep placeholder keeps
// `go build ./...` green on a fresh checkout; a real build must run
// build.ps1 first or the window serves 404s.
//
//go:embed all:frontend/dist
var assets embed.FS

func main() {
	app := NewApp()
	err := wails.Run(&options.App{
		Title:  "Codex Auto Resume",
		Width:  860,
		Height: 560,
		AssetServer: &assetserver.Options{
			Assets: assets,
			// Requests that hit no embedded asset (the /daemon/ API proxy
			// path, plus any non-GET request) fall through to this handler.
			Handler: app.proxyHandler(),
		},
		OnStartup: app.startup,
		Bind:      []interface{}{app},
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId: "codex-auto-resume-desktop",
			OnSecondInstanceLaunch: func(_ options.SecondInstanceData) {
				if app.ctx != nil {
					wailsruntime.WindowUnminimise(app.ctx)
					wailsruntime.Show(app.ctx)
				}
			},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
}
