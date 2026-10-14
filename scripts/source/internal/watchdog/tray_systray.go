//go:build windows || (linux && !android) || (darwin && cgo)

package watchdog

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"fyne.io/systray"
)

// trayApp drives the cross-platform notification-area icon through
// fyne.io/systray. The management snapshot is polled once per second and only
// changed tooltips, icons, titles, and menu structures are pushed, so the tray
// is not spammed with no-op updates. Explorer restart recovery is handled
// inside the library: it registers the TaskbarCreated message and re-adds the
// icon itself, keeping the last icon and tooltip from its notify-icon record.
//
// darwin is supported only in cgo builds because the library's menu-bar code
// is Objective-C; !cgo darwin builds (e.g. cross-compiles) use tray_headless.
type trayApp struct {
	dataDir string
	logger  *safeLogger
	cancel  context.CancelFunc
	service *managementService

	mu              sync.Mutex
	statusItem      *systray.MenuItem
	pauseItem       *systray.MenuItem
	menuBuilt       bool
	quotaThreads    []string
	lastTip         string
	lastIcon        string
	lastStatusTitle string
	lastPauseTitle  string
	notified        trayNotifyCounts
	initialized     bool
}

func runTray(ctx context.Context, cancel context.CancelFunc, dataDir string, logger *safeLogger) error {
	// The library's hidden window and message pump must stay on one OS thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	app := &trayApp{
		dataDir: dataDir, logger: logger, cancel: cancel,
		service: newManagementService(dataDir),
	}
	systray.Run(func() { app.ready(ctx) }, func() {})
	return nil
}

func (a *trayApp) ready(ctx context.Context) {
	systray.SetIcon(trayIconImage("running"))
	systray.SetTooltip("Codex Auto Resume")
	// A left click opens the client; the menu opens on right click (and left
	// on platforms without a tapped callback).
	systray.SetOnTapped(func() { go a.openSettings() })
	a.refresh()
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				systray.Quit()
				return
			case <-ticker.C:
				a.refresh()
			}
		}
	}()
}

func (a *trayApp) refresh() {
	a.mu.Lock()
	defer a.mu.Unlock()
	snapshot, err := a.service.snapshot(time.Now().UTC())
	if err != nil {
		a.setTip("Codex Auto Resume - 状态读取失败")
		// Keep 退出 reachable even while the snapshot is unavailable.
		a.syncMenu(ManagementSnapshot{})
		return
	}
	tip, iconState := composeTrayTip(snapshot)
	a.setVisual(iconState, tip)
	a.syncMenu(snapshot)
	notification, next, fired := a.notified.diff(snapshot)
	a.notified = next
	if a.initialized && snapshot.ShowNotifications && fired {
		a.notify(notification.title, notification.message)
	}
	a.initialized = true
}

func (a *trayApp) setTip(tip string) {
	a.setVisual(a.lastIcon, tip)
}

func (a *trayApp) setVisual(iconState, tip string) {
	if iconState == "" {
		iconState = "running"
	}
	if tip != a.lastTip {
		systray.SetTooltip(tip)
		a.lastTip = tip
	}
	if iconState != a.lastIcon {
		systray.SetIcon(trayIconImage(iconState))
		a.lastIcon = iconState
	}
}

// syncMenu keeps the dynamic part of the menu aligned with the snapshot. The
// quota rows are rebuilt only when the parked thread set changes; titles are
// updated in place on every refresh.
func (a *trayApp) syncMenu(snapshot ManagementSnapshot) {
	want := quotaWaitThreadIDs(snapshot.Retries)
	if !a.menuBuilt || !slices.Equal(want, a.quotaThreads) {
		a.rebuildMenu(want)
	}
	status := strings.TrimPrefix(a.lastTip, "Codex Auto Resume - ")
	if status != a.lastStatusTitle {
		a.statusItem.SetTitle(status)
		a.lastStatusTitle = status
	}
	pauseTitle := "暂停自动恢复 Pause auto-resume"
	if snapshot.Paused {
		pauseTitle = "恢复自动恢复 Resume auto-resume"
	}
	if pauseTitle != a.lastPauseTitle {
		a.pauseItem.SetTitle(pauseTitle)
		a.lastPauseTitle = pauseTitle
	}
}

// rebuildMenu recreates the whole menu because the library has no removable
// separators; ResetMenu + append keeps the flat layout the Win32 tray had:
// status, settings, pause, one section per parked thread, exit.
func (a *trayApp) rebuildMenu(threads []string) {
	systray.ResetMenu()
	a.statusItem = systray.AddMenuItem("", "")
	a.statusItem.Disable()
	systray.AddSeparator()
	a.watch(systray.AddMenuItem("打开设置…", ""), func() { go a.openSettings() })
	a.pauseItem = systray.AddMenuItem("", "")
	a.watch(a.pauseItem, a.togglePause)
	if len(threads) > 0 {
		systray.AddSeparator()
		for _, threadID := range threads {
			label := shortThreadID(threadID)
			a.watch(systray.AddMenuItem(fmt.Sprintf("立即恢复 Resume Now %s", label), ""),
				func() { a.quotaCommand(threadID, false) })
			a.watch(systray.AddMenuItem(fmt.Sprintf("取消配额恢复 Cancel Pending Resume %s", label), ""),
				func() { a.quotaCommand(threadID, true) })
		}
	}
	systray.AddSeparator()
	a.watch(systray.AddMenuItem("退出", ""), func() {
		a.cancel()
		systray.Quit()
	})
	a.quotaThreads = threads
	a.menuBuilt = true
	a.lastStatusTitle = ""
	a.lastPauseTitle = ""
}

// watch consumes one item's click channel until the item is removed (Remove
// closes ClickedCh when the menu is rebuilt).
func (a *trayApp) watch(item *systray.MenuItem, fn func()) {
	go func() {
		for range item.ClickedCh {
			fn()
		}
	}()
}

func (a *trayApp) quotaCommand(threadID string, cancel bool) {
	if cancel {
		_, _ = a.service.cancelRetry(threadID, time.Now().UTC())
	} else {
		_, _ = a.service.retryNow(threadID, time.Now().UTC())
	}
	a.refresh()
}

func (a *trayApp) togglePause() {
	snapshot, err := a.service.snapshot(time.Now().UTC())
	if err == nil {
		_, _ = a.service.setPaused(!snapshot.Paused, time.Now().UTC())
	}
	a.refresh()
}

func (a *trayApp) notify(title, text string) {
	if err := trayNotify(title, text); err != nil {
		a.logger.Printf("notification failed category=tray_notify")
	}
}

// openSettings launches the desktop client next to the watchdog binary, using
// the same executable-relative resolution as the data dir. When no client is
// installed the miss is logged and surfaced as a notification; there is no
// in-band settings fallback anymore (ADR-0004).
func (a *trayApp) openSettings() {
	if client := desktopClientPath(); client != "" {
		if err := exec.Command(client).Start(); err != nil {
			a.logger.Printf("desktop client launch failed category=settings_client")
		}
		return
	}
	a.logger.Printf("desktop client not found category=settings_client")
	a.notify("Codex Auto Resume", "Desktop client is not installed next to the watchdog.")
}

func desktopClientPath() string {
	executable, err := os.Executable()
	if err != nil {
		return ""
	}
	name := "codex-auto-resume-desktop"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(filepath.Dir(executable), name)
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}
