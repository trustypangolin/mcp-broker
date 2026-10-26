package proc

import (
	"fmt"
	"syscall"
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
	killTree(p.Cmd.Process.Pid)
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
