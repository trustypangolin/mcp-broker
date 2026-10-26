package proc

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/trustypangolin/mcp-broker/internal/testutil/alive"
)

// The test binary doubles as the helper: PROC_HELPER=parent spawns a grandchild and
// prints its PID; PROC_HELPER=child just waits to be killed.
func TestMain(m *testing.M) {
	RunHelper()
	switch os.Getenv("PROC_HELPER") {
	case "trap":
		// Exit 3 on a graceful signal, so the test can tell it apart from a kill.
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
		fmt.Println("ready")
		<-ch
		os.Exit(3)
	case "parent":
		c := exec.Command(os.Args[0])
		c.Env = append(os.Environ(), "PROC_HELPER=child")
		if err := c.Start(); err != nil {
			fmt.Println("error", err)
			os.Exit(1)
		}
		fmt.Println(c.Process.Pid)
		time.Sleep(time.Hour)
	case "child":
		time.Sleep(time.Hour)
	}
	os.Exit(m.Run())
}

func TestKillTakesWholeTree(t *testing.T) {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "PROC_HELPER=parent")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	p, err := Start(cmd, Options{})
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	grandchild, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("helper said %q", line)
	}
	parent := p.PID()
	if !alive.PID(parent) || !alive.PID(grandchild) {
		t.Fatal("helpers not running")
	}

	p.Kill()
	cmd.Wait() // reap our direct child

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && (alive.PID(parent) || alive.PID(grandchild)) {
		time.Sleep(50 * time.Millisecond)
	}
	if alive.PID(parent) {
		t.Error("parent survived Kill")
	}
	if alive.PID(grandchild) {
		t.Error("grandchild survived Kill")
	}
	p.Kill() // second Kill is a no-op
}

func TestInterruptIsGraceful(t *testing.T) {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "PROC_HELPER=trap")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	p, err := Start(cmd, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Kill()
	if line, _ := bufio.NewReader(out).ReadString('\n'); strings.TrimSpace(line) != "ready" {
		t.Fatalf("helper said %q", line)
	}
	if err := p.Interrupt(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		if code := cmd.ProcessState.ExitCode(); code != 3 {
			t.Fatalf("exit code %d, want 3 (caught the signal)", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("interrupt not delivered")
	}
}

var runtimeIsWindows = runtime.GOOS == "windows"

func TestAdopt(t *testing.T) {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "PROC_HELPER=child")
	p, err := Start(cmd, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Kill()
	started := time.Now()
	if _, ok := Adopt(p.PID(), started.Add(-time.Hour)); ok && runtimeIsWindows {
		t.Error("adopted a process whose start time does not match")
	}
	a, ok := Adopt(p.PID(), started)
	if !ok {
		t.Fatal("could not adopt own child")
	}
	a.Kill()
	deadline := time.Now().Add(5 * time.Second)
	for a.Alive() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if a.Alive() {
		t.Fatal("adopted process survived Kill")
	}
}
