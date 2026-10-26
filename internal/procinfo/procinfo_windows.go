package procinfo

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	iphlpapi            = windows.NewLazySystemDLL("iphlpapi.dll")
	getExtendedTcpTable = iphlpapi.NewProc("GetExtendedTcpTable")
)

const tcpTableOwnerPIDAll = 5

// maxDepth bounds parent-chain walks; real launch chains are a handful deep.
const maxDepth = 16

// mibTCPRowOwnerPID is MIB_TCPROW_OWNER_PID.
type mibTCPRowOwnerPID struct {
	State      uint32
	LocalAddr  [4]byte
	LocalPort  uint32
	RemoteAddr [4]byte
	RemotePort uint32
	OwningPID  uint32
}

// mibTCP6RowOwnerPID is MIB_TCP6ROW_OWNER_PID.
type mibTCP6RowOwnerPID struct {
	LocalAddr     [16]byte
	LocalScopeID  uint32
	LocalPort     uint32
	RemoteAddr    [16]byte
	RemoteScopeID uint32
	RemotePort    uint32
	State         uint32
	OwningPID     uint32
}

// port converts a table port (network byte order in the low 16 bits) to host order.
func port(p uint32) uint16 {
	var b [2]byte
	binary.LittleEndian.PutUint16(b[:], uint16(p))
	return binary.BigEndian.Uint16(b[:])
}

func tcpTable(family uint32) ([]byte, error) {
	size := uint32(64 << 10)
	for range 4 { // the table can grow between the size query and the read
		buf := make([]byte, size)
		r, _, _ := getExtendedTcpTable.Call(
			uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)),
			0, uintptr(family), tcpTableOwnerPIDAll, 0)
		switch windows.Errno(r) {
		case 0:
			return buf[:size], nil
		case windows.ERROR_INSUFFICIENT_BUFFER:
			size += 16 << 10
			continue
		default:
			return nil, fmt.Errorf("GetExtendedTcpTable: %w", windows.Errno(r))
		}
	}
	return nil, fmt.Errorf("GetExtendedTcpTable: table kept growing")
}

func connOwner(client netip.Addr, clientPort uint16, server netip.Addr, serverPort uint16) (int, error) {
	if client.Is4() {
		buf, err := tcpTable(windows.AF_INET)
		if err != nil {
			return 0, err
		}
		n := binary.LittleEndian.Uint32(buf)
		rows := unsafe.Slice((*mibTCPRowOwnerPID)(unsafe.Pointer(&buf[4])), n)
		c4, s4 := client.As4(), server.As4()
		for _, r := range rows {
			if r.LocalAddr == c4 && port(r.LocalPort) == clientPort && r.RemoteAddr == s4 && port(r.RemotePort) == serverPort {
				return int(r.OwningPID), nil
			}
		}
		return 0, ErrNotFound
	}

	buf, err := tcpTable(windows.AF_INET6)
	if err != nil {
		return 0, err
	}
	n := binary.LittleEndian.Uint32(buf)
	rows := unsafe.Slice((*mibTCP6RowOwnerPID)(unsafe.Pointer(&buf[4])), n)
	c16, s16 := client.As16(), server.As16()
	for _, r := range rows {
		if r.LocalAddr == c16 && port(r.LocalPort) == clientPort && r.RemoteAddr == s16 && port(r.RemotePort) == serverPort {
			return int(r.OwningPID), nil
		}
	}
	return 0, ErrNotFound
}

func chain(pid int) ([]Proc, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("process snapshot: %w", err)
	}
	defer windows.CloseHandle(snap)

	parents := map[int]int{}
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		parents[int(e.ProcessID)] = int(e.ParentProcessID)
	}

	var out []Proc
	seen := map[int]bool{}
	childStart := int64(-1)
	for cur := pid; len(out) < maxDepth; {
		path, start := describe(cur)
		if childStart >= 0 && start > childStart {
			break // PID reused: this "parent" is younger than its child
		}
		if path == "" && len(out) > 0 {
			// Protected system processes (svchost, services) cannot be opened, so their
			// start times are unknown and PID reuse cannot be ruled out; nothing above
			// them is attributable to the caller anyway.
			break
		}
		out = append(out, Proc{PID: cur, Path: path})
		seen[cur] = true
		next, ok := parents[cur]
		if !ok || next == 0 || seen[next] {
			break
		}
		if start > 0 {
			childStart = start
		}
		cur = next
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return out, nil
}

// describe returns a process's executable path and creation time (100 ns ticks), or zero
// values when it cannot be opened (protected or already exited).
func describe(pid int) (string, int64) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", 0
	}
	defer windows.CloseHandle(h)

	var path string
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if windows.QueryFullProcessImageName(h, 0, &buf[0], &n) == nil {
		path = windows.UTF16ToString(buf[:n])
	}
	var created, exited, kernel, user windows.Filetime
	if windows.GetProcessTimes(h, &created, &exited, &kernel, &user) != nil {
		return path, 0
	}
	return path, created.Nanoseconds() / 100
}
