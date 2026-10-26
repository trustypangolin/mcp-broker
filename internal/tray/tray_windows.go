package tray

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/trustypangolin/mcp-broker/internal/approval"
	"github.com/trustypangolin/mcp-broker/internal/broker"
	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/proc"
	"github.com/trustypangolin/mcp-broker/internal/server"
	"github.com/trustypangolin/mcp-broker/internal/supervisor"
	"github.com/trustypangolin/mcp-broker/internal/version"
	"github.com/trustypangolin/mcp-broker/scripts"
)

const (
	title     = "mcp-broker"
	wmTray    = wmApp + 1 // notification icon callback
	wmRefresh = wmApp + 2 // state changed: redraw icon and tooltip
)

// Menu command IDs.
const (
	cmdDashboard = iota + 1
	cmdConnectors
	cmdAgents
	cmdConfig
	cmdAudit
	cmdReload
	cmdRestart
	cmdStart
	cmdStop
	cmdOpenFolder
	cmdOpenLogs
	cmdAllowLAN
	cmdLoopbackOnly
	cmdAutostart
	cmdQuit
	cmdProfiles
	cmdApprovalBase = 1000
	cmdProfileBase  = 2000
)

type app struct {
	cfgPath string
	log     *slog.Logger
	hwnd    windows.HWND
	icons   map[iconState]windows.Handle

	taskbarCreated uint32

	nidMu sync.Mutex
	nid   notifyIconData

	mu       sync.Mutex
	b        *broker.Broker
	startErr error
	onClick  func() // what clicking the last notification does
	busy     bool   // a firewall change is running
}

var theApp *app // the window procedure's receiver

// Run shows the tray icon and hosts the broker until the user quits. cfgPath is created
// with a starter config if it does not exist.
func Run(cfgPath string) error {
	runtime.LockOSThread()

	mutex, err := windows.CreateMutex(nil, false, u16(`Local\mcp-broker-tray`))
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		messageBox(title, "mcp-broker is already running. Look for its icon in the notification area (you may need to click the ^ arrow).", mbOK|mbIconInfo)
		return nil
	}
	if err != nil {
		return err
	}
	defer windows.CloseHandle(mutex)

	firstRun := false
	if _, err := os.Stat(cfgPath); errors.Is(err, os.ErrNotExist) {
		if err := config.WriteStarter(cfgPath); err != nil {
			return fmt.Errorf("creating %s: %w", cfgPath, err)
		}
		firstRun = true
	}
	log, closeLog, err := openLog(filepath.Join(filepath.Dir(cfgPath), "logs"))
	if err != nil {
		return err
	}
	defer closeLog()
	log.Info("tray starting", "version", version.String(), "config", cfgPath)

	a := &app{cfgPath: cfgPath, log: log, icons: map[iconState]windows.Handle{}}
	theApp = a
	if err := a.createWindow(); err != nil {
		return err
	}
	size := smallIconSize()
	for _, st := range []iconState{iconRunning, iconPending, iconStopped} {
		h, err := makeIcon(size, st)
		if err != nil {
			return fmt.Errorf("creating icon: %w", err)
		}
		a.icons[st] = h
	}
	a.addIcon()
	defer a.removeIcon()

	if err := proc.JoinKillOnCloseJob(); err != nil {
		log.Warn("could not join kill-on-close job", "err", err)
	}
	defer proc.KillAll()
	a.start()
	if firstRun {
		a.notify(niifInfo, "mcp-broker is running", "A starter config was created. Click here to add connectors and create agent tokens.", func() { a.openDashboard("connectors") })
	}

	var m msg
	for {
		r, _, _ := pGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
	a.stop()
	log.Info("tray stopped")
	return nil
}

func openLog(dir string) (*slog.Logger, func(), error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "tray.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, err
	}
	return slog.New(slog.NewJSONHandler(f, nil)), func() { f.Close() }, nil
}

func (a *app) createWindow() error {
	cls := u16("mcp-broker-tray")
	wc := wndClassEx{WndProc: windows.NewCallback(wndProc), Instance: moduleHandle(), ClassName: cls}
	wc.Size = uint32(unsafe.Sizeof(wc))
	if r, _, err := pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		return fmt.Errorf("RegisterClassEx: %w", err)
	}
	// A hidden top-level window rather than a message-only one: only top-level windows
	// receive TaskbarCreated, which says Explorer restarted and the icon must be re-added.
	h, _, err := pCreateWindowExW.Call(0, uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(u16(title))), 0, 0, 0, 0, 0, 0, 0, uintptr(wc.Instance), 0)
	if h == 0 {
		return fmt.Errorf("CreateWindowEx: %w", err)
	}
	a.hwnd = windows.HWND(h)
	r, _, _ := pRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(u16("TaskbarCreated"))))
	a.taskbarCreated = uint32(r)
	return nil
}

func wndProc(hwnd, m, wp, lp uintptr) uintptr {
	a := theApp
	switch uint32(m) {
	case wmTray:
		switch uint32(lp & 0xffff) {
		case ninSelect, ninKeySelect:
			a.leftClick()
		case wmContextMenu:
			a.showMenu()
		case ninBalloonUserClick:
			a.mu.Lock()
			f := a.onClick
			a.onClick = nil
			a.mu.Unlock()
			if f != nil {
				go f()
			}
		}
		return 0
	case wmRefresh:
		a.refresh()
		return 0
	case wmClose:
		pDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		pPostQuitMessage.Call(0)
		return 0
	}
	if a != nil && uint32(m) == a.taskbarCreated && a.taskbarCreated != 0 {
		a.addIcon()
		return 0
	}
	r, _, _ := pDefWindowProcW.Call(hwnd, m, wp, lp)
	return r
}

// ---- icon ----

func (a *app) addIcon() {
	a.nidMu.Lock()
	defer a.nidMu.Unlock()
	a.nid = notifyIconData{HWnd: a.hwnd, ID: 1, Flags: nifMessage | nifIcon | nifTip | nifShowTip, CallbackMessage: wmTray, Icon: a.icons[iconStopped]}
	a.nid.Size = uint32(unsafe.Sizeof(a.nid))
	copyUTF16(a.nid.Tip[:], title+": starting")
	shellNotify(nimDelete, &a.nid) // after an Explorer restart the old one may linger
	if !shellNotify(nimAdd, &a.nid) {
		a.log.Error("Shell_NotifyIcon add failed")
	}
	a.nid.Version = notifyIconVersion4
	shellNotify(nimSetVersion, &a.nid)
	postMessage(a.hwnd, wmRefresh, 0, 0)
}

func (a *app) removeIcon() {
	a.nidMu.Lock()
	defer a.nidMu.Unlock()
	shellNotify(nimDelete, &a.nid)
}

// trayPending lists the pending approvals the tray may decide.
func trayPending(b *broker.Broker) []approval.Entry {
	var out []approval.Entry
	for _, e := range b.Approvals().List(true) {
		if slices.Contains(e.Channels, config.ChanTray) {
			out = append(out, e)
		}
	}
	return out
}

func (a *app) refresh() {
	a.mu.Lock()
	b, startErr := a.b, a.startErr
	a.mu.Unlock()
	st, tip := iconStopped, title+": not running"
	if startErr != nil {
		tip = title + ": " + startErr.Error()
	}
	if b != nil {
		st, tip = iconRunning, title+" "+version.Version+" on "+b.Config().Server.Listen
		if n := len(trayPending(b)); n > 0 {
			st, tip = iconPending, fmt.Sprintf("%s: %d approval(s) waiting", title, n)
		}
	}
	a.nidMu.Lock()
	defer a.nidMu.Unlock()
	a.nid.Flags = nifIcon | nifTip | nifShowTip
	a.nid.Icon = a.icons[st]
	copyUTF16(a.nid.Tip[:], tip)
	shellNotify(nimModify, &a.nid)
}

// notify shows a notification; clicking it runs onClick (if not nil).
func (a *app) notify(kind uint32, head, body string, onClick func()) {
	a.mu.Lock()
	a.onClick = onClick
	a.mu.Unlock()
	a.nidMu.Lock()
	defer a.nidMu.Unlock()
	a.nid.Flags = nifInfo
	a.nid.InfoFlags = kind
	copyUTF16(a.nid.InfoTitle[:], head)
	copyUTF16(a.nid.Info[:], body)
	shellNotify(nimModify, &a.nid)
}

// ---- broker lifecycle ----

func (a *app) start() {
	b, err := broker.Start(broker.Options{ConfigPath: a.cfgPath, Watch: true, OnReload: a.reloaded, OnProfile: a.profileChanged})
	a.mu.Lock()
	a.b, a.startErr = b, err
	a.mu.Unlock()
	postMessage(a.hwnd, wmRefresh, 0, 0)
	if err != nil {
		a.log.Error("broker start failed", "err", err)
		hint := ""
		if strings.Contains(err.Error(), "bind") || strings.Contains(err.Error(), "Only one usage") {
			hint = "\nIs `mcp-broker serve` already running in a terminal?"
		}
		a.notify(niifError, "mcp-broker could not start", err.Error()+hint, func() { a.openFolder() })
		return
	}
	go a.watchApprovals(b)
	go func() {
		if err, ok := <-b.Err(); ok && err != nil {
			a.log.Error("broker stopped serving", "err", err)
			a.mu.Lock()
			if a.b == b {
				a.b, a.startErr = nil, err
			}
			a.mu.Unlock()
			b.Close()
			postMessage(a.hwnd, wmRefresh, 0, 0)
			a.notify(niifError, "mcp-broker stopped", err.Error(), nil)
		}
	}()
}

func (a *app) stop() {
	a.mu.Lock()
	b := a.b
	a.b, a.startErr = nil, nil
	a.mu.Unlock()
	if b != nil {
		b.Close()
	}
	postMessage(a.hwnd, wmRefresh, 0, 0)
}

func (a *app) current() *broker.Broker {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.b
}

func (a *app) reloaded(res server.Reloaded, err error) {
	postMessage(a.hwnd, wmRefresh, 0, 0)
	switch {
	case err != nil:
		a.notify(niifError, "Config not applied", err.Error()+"\nThe previous config is still in force.", func() { a.openDashboard("config") })
	case len(res.Restart) > 0:
		a.notify(niifInfo, "Restart needed", "Saved. "+strings.Join(res.Restart, ", ")+" take effect after a restart. Click to restart the broker now.", func() { a.restart() })
	}
}

func (a *app) profileChanged(st supervisor.Status) {
	open := func() { a.openDashboard("profiles") }
	switch st.State {
	case supervisor.Ready:
		a.notify(niifInfo|niifNoSound, st.Profile+" is ready", st.Health, open)
	case supervisor.Unhealthy:
		msg := st.Health
		if st.Error != "" {
			msg = st.Error
		}
		a.notify(niifWarning, st.Profile+" is unhealthy", msg, open)
	case supervisor.Exited:
		code := "?"
		if st.ExitCode != nil {
			code = fmt.Sprint(*st.ExitCode)
		}
		a.notify(niifWarning, st.Profile+" exited", "Exit code "+code+". Click for its logs.", open)
	}
}

func (a *app) restart() {
	a.stop()
	a.start()
}

func (a *app) watchApprovals(b *broker.Broker) {
	ch, cancel := b.Approvals().Subscribe()
	defer cancel()
	for {
		select {
		case <-b.Err():
			return // closed: the broker stopped
		case e, ok := <-ch:
			if !ok {
				return
			}
			postMessage(a.hwnd, wmRefresh, 0, 0)
			if e.Status != approval.Pending || !slices.Contains(e.Channels, config.ChanTray) {
				continue
			}
			id := e.ID
			a.notify(niifWarning, "Approval needed: "+e.Agent, e.Summary+"\nClick to approve or deny.", func() { a.decide(b, id) })
		}
	}
}

// decide asks the user about one approval in a message box.
func (a *app) decide(b *broker.Broker, id string) {
	e, ok := b.Approvals().Get(id)
	if !ok || e.Status != approval.Pending {
		return
	}
	text := fmt.Sprintf("%s\n\nAgent: %s\nWhy: %s\nExpires: %s\n\nYes approves this one call, No denies it, Cancel decides later.",
		e.Summary, e.Agent, e.Reason, e.Expires.Local().Format("15:04:05"))
	var approve bool
	switch messageBox(title+": approval needed", text, mbYesNoCancel|mbIconQuestion|mbDefButton2) {
	case idYes:
		approve = true
	case idNo:
	default:
		return
	}
	if _, err := b.Approvals().Decide(id, approve, config.ChanTray); err != nil {
		messageBox(title, "Could not record the decision: "+err.Error(), mbOK|mbIconWarning)
	}
	postMessage(a.hwnd, wmRefresh, 0, 0)
}

// ---- menu ----

func (a *app) leftClick() {
	b := a.current()
	if b == nil {
		a.showMenu()
		return
	}
	if p := trayPending(b); len(p) > 0 {
		go a.decide(b, p[len(p)-1].ID) // oldest first
		return
	}
	go a.openDashboard("")
}

type menu uintptr

func newMenu() menu {
	h, _, _ := pCreatePopupMenu.Call()
	return menu(h)
}

func (m menu) add(id int, text string, flags uint32) {
	pAppendMenuW.Call(uintptr(m), uintptr(flags), uintptr(id), uintptr(unsafe.Pointer(u16(text))))
}

func (m menu) sub(text string, s menu, flags uint32) {
	pAppendMenuW.Call(uintptr(m), uintptr(mfPopup|flags), uintptr(s), uintptr(unsafe.Pointer(u16(text))))
}

func (m menu) sep() { pAppendMenuW.Call(uintptr(m), mfSeparator, 0, 0) }

// menuText escapes & (the accelerator marker) in user-supplied text and shortens it.
func menuText(s string) string {
	if r := []rune(s); len(r) > 70 {
		s = string(r[:69]) + "…"
	}
	return strings.ReplaceAll(s, "&", "&&")
}

func isLoopbackListen(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return true
	}
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}

func (a *app) showMenu() {
	a.mu.Lock()
	b, startErr, busy := a.b, a.startErr, a.busy
	a.mu.Unlock()

	m := newMenu()
	defer pDestroyMenu.Call(uintptr(m))
	running := b != nil
	var pending []approval.Entry
	if running {
		m.add(0, menuText(fmt.Sprintf("mcp-broker %s · %s", version.Version, b.Config().Server.Listen)), mfGrayed)
		pending = trayPending(b)
	} else if startErr != nil {
		m.add(0, menuText("Not running: "+startErr.Error()), mfGrayed)
	} else {
		m.add(0, "Not running", mfGrayed)
	}
	m.sep()
	if len(pending) > 0 {
		s := newMenu()
		for i, e := range pending {
			s.add(cmdApprovalBase+i, menuText(e.Summary), 0)
		}
		m.sub(fmt.Sprintf("Approvals waiting (%d)", len(pending)), s, 0)
		m.sep()
	}
	var profiles []supervisor.Status
	if running {
		profiles = b.Supervisor().List()
	}
	if len(profiles) > 0 {
		s := newMenu()
		for i, p := range profiles {
			verb := "Start"
			if p.State == supervisor.Starting || p.State == supervisor.Ready || p.State == supervisor.Unhealthy {
				verb = "Stop"
			}
			flags := uint32(0)
			if p.State == supervisor.Stopping || p.State == supervisor.Updating {
				flags = mfGrayed
			}
			s.add(cmdProfileBase+i, menuText(fmt.Sprintf("%s %s (%s)", verb, p.Profile, p.State)), flags)
		}
		s.sep()
		s.add(cmdProfiles, "Show profiles…", 0)
		m.sub("Pro&files", s, 0)
	}
	dis := uint32(0)
	if !running {
		dis = mfGrayed
	}
	m.add(cmdDashboard, "&Open dashboard", dis)
	m.add(cmdConnectors, "&Connectors…", dis)
	m.add(cmdAgents, "&Agents and tokens…", dis)
	m.add(cmdConfig, "Edit &config…", dis)
	m.add(cmdAudit, "Audit &log…", dis)
	m.sep()
	if running {
		m.add(cmdReload, "&Reload config", 0)
		m.add(cmdRestart, "Re&start broker", 0)
		m.add(cmdStop, "Sto&p broker", 0)
	} else {
		m.add(cmdStart, "&Start broker", 0)
	}
	m.add(cmdOpenFolder, "Open config &folder", 0)
	m.add(cmdOpenLogs, "Open lo&gs folder", 0)
	m.sep()
	fwFlags := uint32(0)
	if busy {
		fwFlags = mfGrayed
	}
	listen := ""
	if cfg, err := config.Load(a.cfgPath); err == nil {
		listen = cfg.Server.Listen
	}
	if isLoopbackListen(listen) {
		m.add(cmdAllowLAN, "Allow LAN and WSL access… (admin)", fwFlags)
	} else {
		m.add(cmdLoopbackOnly, "Restrict to this PC only… (admin)", fwFlags)
	}
	auto := uint32(0)
	if autostartEnabled() {
		auto = mfChecked
	}
	m.add(cmdAutostart, "Start with &Windows", auto)
	m.sep()
	m.add(cmdQuit, "&Quit", 0)
	pSetMenuDefaultItem.Call(uintptr(m), cmdDashboard, 0)

	var pt point
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	// Required so the menu closes when the user clicks elsewhere.
	pSetForegroundWindow.Call(uintptr(a.hwnd))
	r, _, _ := pTrackPopupMenu.Call(uintptr(m), tpmReturnCmd|tpmRightButton|tpmBottomAlign, uintptr(pt.X), uintptr(pt.Y), 0, uintptr(a.hwnd), 0)
	postMessage(a.hwnd, wmNull, 0, 0)
	a.command(int(r), b, pending, profiles)
}

func (a *app) command(id int, b *broker.Broker, pending []approval.Entry, profiles []supervisor.Status) {
	switch {
	case id >= cmdProfileBase && id-cmdProfileBase < len(profiles):
		go a.toggleProfile(b, profiles[id-cmdProfileBase])
	case id == cmdProfiles:
		go a.openDashboard("profiles")
	case id >= cmdApprovalBase && id-cmdApprovalBase < len(pending):
		go a.decide(b, pending[id-cmdApprovalBase].ID)
	case id == cmdDashboard:
		go a.openDashboard("")
	case id == cmdConnectors:
		go a.openDashboard("connectors")
	case id == cmdAgents:
		go a.openDashboard("agents")
	case id == cmdConfig:
		go a.openDashboard("config")
	case id == cmdAudit:
		go a.openDashboard("audit")
	case id == cmdReload:
		go func() {
			if res, err := b.Reload(); err == nil && len(res.Restart) == 0 {
				a.notify(niifInfo|niifNoSound, "Config reloaded", reloadText(res), nil)
			}
		}()
	case id == cmdRestart:
		go a.restart()
	case id == cmdStart:
		go a.start()
	case id == cmdStop:
		go a.stop()
	case id == cmdOpenFolder:
		go a.openFolder()
	case id == cmdOpenLogs:
		go shellOpen(filepath.Join(filepath.Dir(a.cfgPath), "logs"))
	case id == cmdAllowLAN:
		go a.setNetworkAccess(true)
	case id == cmdLoopbackOnly:
		go a.setNetworkAccess(false)
	case id == cmdAutostart:
		if err := setAutostart(!autostartEnabled()); err != nil {
			go messageBox(title, "Could not change autostart: "+err.Error(), mbOK|mbIconError)
		}
	case id == cmdQuit:
		postMessage(a.hwnd, wmClose, 0, 0)
	}
}

// toggleProfile starts (after showing its plan) or stops a profile for the user.
func (a *app) toggleProfile(b *broker.Broker, p supervisor.Status) {
	sup := b.Supervisor()
	if p.State == supervisor.Starting || p.State == supervisor.Ready || p.State == supervisor.Unhealthy {
		if _, err := sup.Stop(context.Background(), p.Profile); err != nil {
			messageBox(title, "Stopping "+p.Profile+": "+err.Error(), mbOK|mbIconWarning)
		}
		return
	}
	pl, err := sup.Plan(p.Profile, "start")
	if err != nil {
		messageBox(title, err.Error(), mbOK|mbIconError)
		return
	}
	if len(pl.Blocking) > 0 {
		messageBox(title, "Cannot start "+p.Profile+":\n\n"+pl.Text(), mbOK|mbIconWarning)
		return
	}
	if messageBox(title+": start "+p.Profile, pl.Text()+"\nStart it?", mbYesNo|mbIconQuestion) != idYes {
		return
	}
	if _, err := sup.Start(p.Profile); err != nil {
		messageBox(title, "Starting "+p.Profile+": "+err.Error(), mbOK|mbIconError)
	}
}

func reloadText(res server.Reloaded) string {
	parts := []string{"The new config is in force."}
	if len(res.Unloaded) > 0 {
		parts = append(parts, "Restarting on next use: "+strings.Join(res.Unloaded, ", ")+".")
	}
	for _, i := range res.Issues {
		if strings.HasPrefix(i, "error") {
			parts = append(parts, i)
		}
	}
	return strings.Join(parts, "\n")
}

func (a *app) openDashboard(page string) {
	b := a.current()
	if b == nil {
		return
	}
	shellOpen(b.DashboardURL(page))
}

func (a *app) openFolder() { shellOpen(filepath.Dir(a.cfgPath)) }

// setNetworkAccess switches between loopback-only and LAN/WSL access: it changes the
// firewall rules (through a UAC prompt), then server.listen, then restarts the broker.
func (a *app) setNetworkAccess(lan bool) {
	a.mu.Lock()
	if a.busy {
		a.mu.Unlock()
		return
	}
	a.busy = true
	a.mu.Unlock()
	defer func() { a.mu.Lock(); a.busy = false; a.mu.Unlock() }()

	data, err := os.ReadFile(a.cfgPath)
	if err != nil {
		messageBox(title, err.Error(), mbOK|mbIconError)
		return
	}
	cfg, err := config.Parse(data, filepath.Dir(a.cfgPath))
	if err != nil {
		messageBox(title, "Fix the config first: "+err.Error(), mbOK|mbIconError)
		return
	}
	_, port, _ := net.SplitHostPort(cfg.Server.Listen)
	if port == "" {
		port = "8765"
	}
	var prompt, host, args string
	if lan {
		prompt = "Let other devices on your home network, WSL and containers reach the broker?\n\n" +
			"Windows will ask for administrator approval to add firewall rules: TCP " + port + " on Private networks only " +
			"(closed on public Wi-Fi), and on the WSL adapter. The broker then listens on all interfaces and restarts.\n\n" +
			"Every connection still needs an agent token, and server.allowed_sources still applies."
		host, args = "0.0.0.0", "-Port "+port
	} else {
		prompt = "Make the broker reachable from this PC only?\n\nWindows will ask for administrator approval to remove the firewall rules. The broker then listens on 127.0.0.1 and restarts. WSL, containers and other devices will lose access."
		host, args = "127.0.0.1", "-Port "+port+" -Remove"
	}
	if messageBox(title, prompt, mbYesNo|mbIconQuestion) != idYes {
		return
	}

	script := filepath.Join(os.TempDir(), "mcp-broker-firewall.ps1")
	if err := os.WriteFile(script, []byte(scripts.Firewall), 0o600); err != nil {
		messageBox(title, err.Error(), mbOK|mbIconError)
		return
	}
	defer os.Remove(script)
	code, err := runElevated("powershell.exe", `-NoProfile -ExecutionPolicy Bypass -File "`+script+`" `+args)
	if err != nil {
		if errors.Is(err, windows.ERROR_CANCELLED) {
			return // the user said no at the UAC prompt
		}
		messageBox(title, "Could not run the firewall script: "+err.Error(), mbOK|mbIconError)
		return
	}
	if code != 0 {
		messageBox(title, fmt.Sprintf("The firewall script failed (exit code %d). Nothing else was changed.", code), mbOK|mbIconError)
		return
	}
	a.log.Info("firewall rules changed", "lan", lan, "port", port)

	out, err := config.SetEntry(data, "server", "listen", net.JoinHostPort(host, port))
	if err == nil {
		err = config.WriteFile(a.cfgPath, out, config.Hash(data))
	}
	if err != nil {
		messageBox(title, "The firewall was updated, but changing server.listen failed: "+err.Error(), mbOK|mbIconError)
		return
	}
	// Give the watcher's reload a moment so its restart-needed notice does not race ours.
	time.Sleep(500 * time.Millisecond)
	a.restart()
	if b := a.current(); b != nil {
		msg := "Listening on " + b.Config().Server.Listen + " for this PC only."
		if lan {
			msg = "Listening on " + b.Config().Server.Listen + ". From WSL the shim finds the broker by itself; from another device use http://<this-pc>:" + port + "/mcp with an agent token."
		}
		a.notify(niifInfo, "Network access changed", msg, nil)
	}
}

// Fatal reports an error that stopped the tray, which has no console to print to.
func Fatal(err error) {
	messageBox(title, "mcp-broker could not start:\n\n"+err.Error(), mbOK|mbIconError)
}
