package tray

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/windows"

	"github.com/trustypangolin/mcp-broker/internal/proc"
	"github.com/trustypangolin/mcp-broker/internal/server"
	"github.com/trustypangolin/mcp-broker/internal/supervisor"
	"github.com/trustypangolin/mcp-broker/internal/update"
	"github.com/trustypangolin/mcp-broker/internal/version"
)

// Updates: the tray checks GitHub shortly after it starts and then every few hours. A newer
// release is offered (balloon, menu, dashboard) and installed only when the user says so.
const (
	updateFirstCheck = 30 * time.Second
	updateInterval   = 6 * time.Hour
)

// Options are the tray's command-line options.
type Options struct {
	// WaitPID is a tray that is exiting to hand over to this one (after an update): wait
	// for it before taking the single-instance lock.
	WaitPID int
	// UpdatedFrom is the version this tray replaced, to say so once it is up.
	UpdatedFrom string
}

// waitForExit waits up to a minute for a process to exit.
func waitForExit(pid int) {
	if pid <= 0 {
		return
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return // already gone
	}
	defer windows.CloseHandle(h)
	windows.WaitForSingleObject(h, 60_000)
}

// startUpdates begins the background checks; it is a no-op for dev builds.
func (a *app) startUpdates(ctx context.Context) {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		a.log.Warn("updates off: can't find the executable", "err", err)
		return
	}
	a.exeDir = filepath.Dir(exe)
	// Replaced programs from the last update; ones a client's shim still runs stay for now.
	go func() {
		time.Sleep(10 * time.Second)
		update.Cleanup(a.exeDir)
	}()
	a.upd = update.NewChecker(version.Version, true)
	if a.upd == nil {
		a.log.Info("updates off: dev build", "version", version.Version)
		return
	}
	go a.upd.Run(ctx, updateFirstCheck, updateInterval, func(r *update.Release) {
		a.log.Info("update available", "version", r.Version)
		a.notify(niifInfo, "mcp-broker "+r.Version+" is available",
			"You have "+version.Version+". Click to see what happens next and update.", a.confirmUpdate)
	})
}

// trayUpdater lets the dashboard offer and install the update the tray found.
type trayUpdater struct{ a *app }

func (t trayUpdater) Available() *server.UpdateInfo {
	if t.a.upd == nil {
		return nil
	}
	r := t.a.upd.Available()
	if r == nil {
		return nil
	}
	return &server.UpdateInfo{Version: r.Version, URL: r.URL}
}

func (t trayUpdater) Apply() error { return t.a.applyUpdate() }

// checkNow is the menu's "Check for updates".
func (a *app) checkNow() {
	if a.upd == nil {
		messageBox(title, "This is a development build ("+version.Version+"), so it can't tell whether a release is newer. Build from a release tag or install a release.", mbOK|mbIconInfo)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := a.upd.Check(ctx)
	switch {
	case err != nil:
		messageBox(title, "Could not check for updates:\n\n"+err.Error(), mbOK|mbIconWarning)
	case r == nil:
		messageBox(title, "mcp-broker "+version.Version+" is up to date.", mbOK|mbIconInfo)
	default:
		a.confirmUpdate()
	}
}

// confirmUpdate asks before installing the available release.
func (a *app) confirmUpdate() {
	if a.upd == nil || a.upd.Available() == nil {
		return
	}
	r := a.upd.Available()
	text := fmt.Sprintf("Update mcp-broker from %s to %s?\n\nThe tray downloads the release, checks it against its checksums, replaces mcp-broker.exe and mcp-broker-tray.exe in\n%s\nand restarts. Agents reconnect by themselves; calls in flight and pending approvals are lost.",
		version.Version, r.Version, a.exeDir)
	if running := a.runningProfiles(); len(running) > 0 {
		text += "\n\nRunning profiles stop: " + strings.Join(running, ", ") + "."
	}
	text += "\n\nRelease notes: " + r.URL
	if messageBox(title+": update", text, mbYesNo|mbIconQuestion) != idYes {
		return
	}
	if err := a.applyUpdate(); err != nil {
		messageBox(title, "The update was not installed:\n\n"+err.Error(), mbOK|mbIconError)
	}
}

func (a *app) runningProfiles() []string {
	b := a.current()
	if b == nil {
		return nil
	}
	var out []string
	for _, p := range b.Supervisor().List() {
		if p.State == supervisor.Starting || p.State == supervisor.Ready || p.State == supervisor.Unhealthy {
			out = append(out, p.Profile)
		}
	}
	return out
}

// applyUpdate installs the available release, starts the new tray and closes this one.
// The new tray waits for this process to exit, so the single-instance lock and the port
// are free when it starts.
func (a *app) applyUpdate() error {
	if a.upd == nil || a.upd.Available() == nil {
		return errors.New("no update to install")
	}
	r := a.upd.Available()
	a.mu.Lock()
	if a.updating {
		a.mu.Unlock()
		return errors.New("an update is already being installed")
	}
	a.updating = true
	a.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	replaced, err := a.upd.Install(ctx, r, a.exeDir)
	if err != nil {
		a.mu.Lock()
		a.updating = false
		a.mu.Unlock()
		a.log.Error("update failed", "version", r.Version, "err", err)
		return err
	}
	a.log.Info("update installed", "from", version.Version, "to", r.Version, "files", replaced)

	tray := filepath.Join(a.exeDir, update.Exe("mcp-broker-tray"))
	err = proc.StartDetached(tray, "-config", a.cfgPath, "-wait-pid", strconv.Itoa(os.Getpid()), "-updated-from", version.Version)
	if err != nil {
		a.log.Error("could not start the updated tray", "err", err)
		go messageBox(title, "Installed "+r.Version+", but the tray could not restart itself:\n\n"+err.Error()+
			"\n\nQuit mcp-broker and start it again to run the new version.", mbOK|mbIconWarning)
		return nil
	}
	// A moment for the dashboard's request to get its answer before the broker goes.
	time.AfterFunc(500*time.Millisecond, func() { postMessage(a.hwnd, wmClose, 0, 0) })
	return nil
}
