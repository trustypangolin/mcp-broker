package proc

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/trustypangolin/mcp-broker/internal/testutil/alive"
)

// The test binary doubles as the helper: PROC_HELPER=parent spawns a grandchild and
// prints its PID; PROC_HELPER=child just waits to be killed.
func TestMain(m *testing.M) {
	switch os.Getenv("PROC_HELPER") {
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
