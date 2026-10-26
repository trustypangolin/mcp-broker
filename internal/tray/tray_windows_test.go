package tray

import (
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestDrawIcon(t *testing.T) {
	for _, size := range []int{16, 20, 24, 32} {
		px := drawIcon(size, iconPending)
		if len(px) != size*size*4 {
			t.Fatalf("size %d: %d bytes", size, len(px))
		}
		alpha := func(x, y int) byte { return px[(y*size+x)*4+3] }
		if alpha(0, 0) != 0 {
			t.Errorf("size %d: corner not transparent", size)
		}
		if alpha(size/2, size/2) != 255 {
			t.Errorf("size %d: centre not opaque", size)
		}
	}
	// The badge only appears in the pending state.
	run, pend := drawIcon(32, iconRunning), drawIcon(32, iconPending)
	i := (25*32 + 25) * 4
	if run[i+2] == pend[i+2] {
		t.Error("pending badge not drawn")
	}
}

func TestMakeIcon(t *testing.T) {
	h, err := makeIcon(smallIconSize(), iconRunning)
	if err != nil || h == 0 {
		t.Fatalf("makeIcon: %v", err)
	}
	user32.NewProc("DestroyIcon").Call(uintptr(h))
}

func TestCopyUTF16Truncates(t *testing.T) {
	var buf [8]uint16
	copyUTF16(buf[:], "a long tooltip")
	if got := windows.UTF16ToString(buf[:]); got != "a long…" {
		t.Fatalf("got %q", got)
	}
	copyUTF16(buf[:], "ok")
	if got := windows.UTF16ToString(buf[:]); got != "ok" {
		t.Fatalf("got %q", got)
	}
}

func TestMenuText(t *testing.T) {
	if got := menuText("R&D"); got != "R&&D" {
		t.Errorf("got %q", got)
	}
	if got := menuText(strings.Repeat("x", 100)); len([]rune(got)) != 70 {
		t.Errorf("not shortened: %d", len([]rune(got)))
	}
}

func TestIsLoopbackListen(t *testing.T) {
	for listen, want := range map[string]bool{
		"127.0.0.1:8765": true, "localhost:8765": true, "[::1]:8765": true,
		"0.0.0.0:8765": false, ":8765": false, "192.168.1.5:8765": false,
	} {
		if got := isLoopbackListen(listen); got != want {
			t.Errorf("%s: got %v", listen, got)
		}
	}
}
