package proc

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

type osProc struct {
	job windows.Handle
}

func newKillOnCloseJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("CreateJobObject: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return 0, fmt.Errorf("SetInformationJobObject: %w", err)
	}
	return job, nil
}

var selfJob windows.Handle

// JoinKillOnCloseJob puts the broker in a job that kills every member when the broker's
// handle closes, which the OS does when the broker exits for any reason, crash included.
// Children inherit the job, so nothing the broker starts can outlive it.
func JoinKillOnCloseJob() error {
	if selfJob != 0 {
		return nil
	}
	job, err := newKillOnCloseJob()
	if err != nil {
		return err
	}
	if err := windows.AssignProcessToJobObject(job, windows.CurrentProcess()); err != nil {
		windows.CloseHandle(job)
		return fmt.Errorf("AssignProcessToJobObject(self): %w", err)
	}
	selfJob = job // deliberately never closed
	return nil
}

func (p *Process) start(opts Options) error {
	if p.Cmd.SysProcAttr == nil {
		p.Cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	p.Cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP
	if !opts.Console {
		p.Cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
	}

	job, err := newKillOnCloseJob()
	if err != nil {
		return err
	}
	if err := p.Cmd.Start(); err != nil {
		windows.CloseHandle(job)
		return err
	}
	// os/exec cannot start suspended, so a grandchild spawned in the instant before this
	// assignment would escape the job. killTree below catches those by parentage.
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(p.Cmd.Process.Pid))
	if err == nil {
		err = windows.AssignProcessToJobObject(job, h)
		windows.CloseHandle(h)
	}
	if err != nil {
		p.Cmd.Process.Kill()
		windows.CloseHandle(job)
		return fmt.Errorf("assigning pid %d to job: %w", p.Cmd.Process.Pid, err)
	}
	p.os.job = job
	return nil
}

func (p *Process) kill() {
	killTree(p.pid)
	if p.os.job != 0 {
		windows.TerminateJobObject(p.os.job, 1)
		windows.CloseHandle(p.os.job)
		p.os.job = 0
	}
}

// killTree terminates root and its descendants, skipping any "child" that started before its
// parent (its parent PID was reused, so it is not ours).
func killTree(root int) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return
	}
	children := map[int][]int{}
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		children[int(e.ParentProcessID)] = append(children[int(e.ParentProcessID)], int(e.ProcessID))
	}
	windows.CloseHandle(snap)

	var walk func(pid int, parentStart int64)
	walk = func(pid int, parentStart int64) {
		h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
		if err != nil {
			return
		}
		var created, exited, kernel, user windows.Filetime
		start := int64(0)
		if windows.GetProcessTimes(h, &created, &exited, &kernel, &user) == nil {
			start = created.Nanoseconds()
		}
		if parentStart > 0 && start > 0 && start < parentStart {
			windows.CloseHandle(h)
			return
		}
		for _, c := range children[pid] {
			if c != pid {
				walk(c, start)
			}
		}
		windows.TerminateProcess(h, 1)
		windows.CloseHandle(h)
	}
	walk(root, 0)
}

func (p *Process) adopt(started time.Time) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(p.pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var created, exited, kernel, user windows.Filetime
	if windows.GetProcessTimes(h, &created, &exited, &kernel, &user) != nil {
		return false
	}
	d := time.Unix(0, created.Nanoseconds()).Sub(started)
	return d > -2*time.Second && d < 2*time.Second && isAlive(p.pid)
}

func isAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	const stillActive = 259
	return windows.GetExitCodeProcess(h, &code) == nil && code == stillActive
}

// envCtrlBreak asks a copy of the broker's own executable to deliver CTRL_BREAK to a
// process group. Console control events only reach processes sharing the sender's console,
// and every owned child has its own hidden one; a helper can attach to it without
// disturbing the broker's console (the terminal running `serve`).
const envCtrlBreak = "MCP_BROKER_CTRL_BREAK"

var (
	kernel32                  = windows.NewLazySystemDLL("kernel32.dll")
	pFreeConsole              = kernel32.NewProc("FreeConsole")
	pAttachConsole            = kernel32.NewProc("AttachConsole")
	pSetConsoleCtrlHandler    = kernel32.NewProc("SetConsoleCtrlHandler")
	pGenerateConsoleCtrlEvent = kernel32.NewProc("GenerateConsoleCtrlEvent")
)

// RunHelper performs a helper action and exits if this process was started as one. Every
// binary that owns processes calls it first thing in main (and test binaries in TestMain).
func RunHelper() {
	v := os.Getenv(envCtrlBreak)
	if v == "" {
		return
	}
	pid, err := strconv.Atoi(v)
	if err != nil {
		os.Exit(2)
	}
	pFreeConsole.Call()
	if r, _, _ := pAttachConsole.Call(uintptr(pid)); r == 0 {
		os.Exit(3)
	}
	pSetConsoleCtrlHandler.Call(0, 1) // ignore the event ourselves
	const ctrlBreakEvent = 1
	if r, _, _ := pGenerateConsoleCtrlEvent.Call(ctrlBreakEvent, uintptr(pid)); r == 0 {
		os.Exit(4)
	}
	os.Exit(0)
}

func (p *Process) interrupt() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), envCtrlBreak+"="+strconv.Itoa(p.pid))
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("sending CTRL_BREAK to %d: %w", p.pid, err)
		}
		return nil
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
		return fmt.Errorf("sending CTRL_BREAK to %d: helper timed out", p.pid)
	}
}

// KillForeign terminates a process the broker did not start, and its descendants. Only for
// an approved kill_foreign.
func KillForeign(pid int) { killTree(pid) }
