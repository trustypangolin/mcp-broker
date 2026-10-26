package procinfo

import (
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConnOwnerAndChain(t *testing.T) {
	for _, network := range []string{"tcp4", "tcp6"} {
		t.Run(network, func(t *testing.T) {
			addr := "127.0.0.1:0"
			if network == "tcp6" {
				addr = "[::1]:0"
			}
			ln, err := net.Listen(network, addr)
			if err != nil {
				t.Skipf("no %s loopback: %v", network, err)
			}
			defer ln.Close()
			accepted := make(chan net.Conn, 1)
			go func() {
				c, _ := ln.Accept()
				accepted <- c
			}()
			client, err := net.Dial(network, ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			srv := <-accepted
			defer srv.Close()

			remote := netip.MustParseAddrPort(srv.RemoteAddr().String())
			local := netip.MustParseAddrPort(srv.LocalAddr().String())
			pid, err := ConnOwner(remote, local)
			if err != nil {
				t.Fatal(err)
			}
			if pid != os.Getpid() {
				t.Fatalf("owner = %d, want %d", pid, os.Getpid())
			}

			procs, err := Chain(pid)
			if err != nil {
				t.Fatal(err)
			}
			self, _ := os.Executable()
			if !strings.EqualFold(filepath.Clean(procs[0].Path), filepath.Clean(self)) {
				t.Errorf("chain[0] = %q, want %q", procs[0].Path, self)
			}
			if len(procs) < 2 {
				t.Errorf("expected at least one ancestor (go test), got %v", procs)
			}
		})
	}
}

func TestConnOwnerNotFound(t *testing.T) {
	_, err := ConnOwner(netip.MustParseAddrPort("127.0.0.1:1"), netip.MustParseAddrPort("127.0.0.1:2"))
	if err != ErrNotFound {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}
