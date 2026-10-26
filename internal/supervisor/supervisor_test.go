package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/proc"
	"github.com/trustypangolin/mcp-broker/internal/testutil/alive"
)

// The test binary is also the supervised app, selected by SUP_APP.
func TestMain(m *testing.M) {
	proc.RunHelper()
	switch os.Getenv("SUP_APP") {
	case "server":
		runServer()
	case "fail":
		fmt.Println("failing on purpose")
		os.Exit(7)
	case "echo":
		fmt.Println("echo", strings.Join(os.Args[1:], " "), "FOO="+os.Getenv("FOO"))
		os.Exit(0)
	case "sleep":
		time.Sleep(time.Hour)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runServer: optional grandchild, slow start, health endpoint, graceful exit code 3
// (unless SUP_STUBBORN, which ignores the graceful signal).
func runServer() {
	if os.Getenv("SUP_STUBBORN") == "1" {
		signal.Ignore(os.Interrupt, syscall.SIGTERM)
	}
	sig := make(chan os.Signal, 1)
	if os.Getenv("SUP_STUBBORN") != "1" {
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	}
	c := exec.Command(os.Args[0])
	c.Env = append(os.Environ(), "SUP_APP=sleep")
	c.Start()
	fmt.Println("grandchild", c.Process.Pid)
	fmt.Println("env FOO=" + os.Getenv("FOO"))
	time.Sleep(300 * time.Millisecond) // "loading models"
	ln, err := net.Listen("tcp", "127.0.0.1:"+os.Getenv("SUP_PORT"))
	if err != nil {
		fmt.Println("listen:", err)
		os.Exit(1)
	}
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "ok") }))
	fmt.Println("server is up")
	<-sig
	fmt.Println("bye")
	os.Exit(3)
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

type fixture struct {
	t    *testing.T
	dir  string
	sup  *Supervisor
	port int
}

// profile YAML for the test app; extra is indented under the profile.
func newFixture(t *testing.T, app string, extra string) *fixture {
	t.Helper()
	f := &fixture{t: t, dir: t.TempDir(), port: freePort(t)}
	exe, _ := json.Marshal(os.Args[0])
	text := fmt.Sprintf(`
profiles:
  app:
    description: Test app
    command: %s
    env: {SUP_APP: %s, SUP_PORT: "%d", FOO: bar}
    ports: [%d]
    stop: {timeout: 3s}
%s
`, exe, app, f.port, f.port, extra)
	cfg, err := config.Parse([]byte(text), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	f.sup = New(Options{Config: cfg, LogDir: f.dir, StateFile: filepath.Join(f.dir, "runs.json")})
	t.Cleanup(f.sup.Close)
	return f
}

func (f *fixture) waitState(want State, within time.Duration) Status {
	f.t.Helper()
	deadline := time.Now().Add(within)
	for {
		st := f.sup.Status("app")
		if st.State == want {
			return st
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("state %s, want %s (health %q, error %q)\nlogs:\n%s", st.State, want, st.Health, st.Error, strings.Join(f.sup.Logs("app", 30), "\n"))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (f *fixture) grandchild() int {
	for _, l := range f.sup.Logs("app", 0) {
		if v, ok := strings.CutPrefix(l, "grandchild "); ok {
			pid, _ := strconv.Atoi(v)
			return pid
		}
	}
	return 0
}

func TestLifecycle(t *testing.T) {
	f := newFixture(t, "server", "")
	p := f.sup.cfg.Load().Profiles["app"]
	p.Health = config.Health{HTTP: fmt.Sprintf("http://127.0.0.1:%d/", f.port), Interval: 100 * time.Millisecond, StartTimeout: 20 * time.Second}
	f.sup.cfg.Load().Profiles["app"] = p

	st, err := f.sup.Start("app")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != Starting || st.PID == 0 {
		t.Fatalf("Start returned %+v, want starting with a pid", st)
	}
	f.waitState(Ready, 20*time.Second)
	if _, err := os.Stat(filepath.Join(f.dir, "runs.json")); err != nil {
		t.Fatal("no state file")
	}
	if pl, _ := f.sup.Plan("app", "start"); len(pl.Blocking) == 0 {
		t.Fatal("a second start is not blocked")
	}
	if _, err := f.sup.Start("app"); err == nil {
		t.Fatal("started twice")
	}
	logs := strings.Join(f.sup.Logs("app", 0), "\n")
	if !strings.Contains(logs, "server is up") || !strings.Contains(logs, "env FOO=bar") {
		t.Fatalf("logs:\n%s", logs)
	}
	gc := f.grandchild()
	if gc == 0 || !alive.PID(gc) {
		t.Fatal("grandchild not running")
	}

	st, err = f.sup.Stop(context.Background(), "app")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != Stopped || st.ExitCode == nil || *st.ExitCode != 3 {
		t.Fatalf("after stop: %+v (want stopped, exit 3 from the graceful signal)", st)
	}
	deadline := time.Now().Add(5 * time.Second)
	for alive.PID(gc) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if alive.PID(gc) {
		t.Fatal("grandchild survived stop")
	}
	if portInUse(f.port) {
		t.Fatal("port still in use after stop")
	}
	if b, _ := os.ReadFile(filepath.Join(f.dir, "profile-app.log")); !strings.Contains(string(b), "bye") {
		t.Fatal("log file missing output")
	}
	// Restartable after a stop.
	if _, err := f.sup.Start("app"); err != nil {
		t.Fatal(err)
	}
	f.waitState(Ready, 20*time.Second)
}

func TestExitedByItself(t *testing.T) {
	f := newFixture(t, "fail", "")
	if _, err := f.sup.Start("app"); err != nil {
		t.Fatal(err)
	}
	st := f.waitState(Exited, 10*time.Second)
	if st.ExitCode == nil || *st.ExitCode != 7 {
		t.Fatalf("exit code %v", st.ExitCode)
	}
	if _, err := f.sup.Stop(context.Background(), "app"); err == nil {
		t.Fatal("stopping an exited run should say it is not running")
	}
}

func TestStubbornIsKilled(t *testing.T) {
	f := newFixture(t, "server", "")
	p := f.sup.cfg.Load().Profiles["app"]
	p.Env["SUP_STUBBORN"] = "1"
	p.Stop.Timeout = 500 * time.Millisecond
	f.sup.cfg.Load().Profiles["app"] = p
	if _, err := f.sup.Start("app"); err != nil {
		t.Fatal(err)
	}
	f.waitState(Ready, 10*time.Second) // no health check: ready on start
	time.Sleep(500 * time.Millisecond)
	start := time.Now()
	st, err := f.sup.Stop(context.Background(), "app")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != Stopped || (st.ExitCode != nil && *st.ExitCode == 3) {
		t.Fatalf("stubborn: %+v", st)
	}
	if time.Since(start) > 8*time.Second {
		t.Fatalf("stop took %s", time.Since(start))
	}
}

func TestLogReadyRegex(t *testing.T) {
	f := newFixture(t, "server", `    health: {log_ready_regex: "server is up", start_timeout: 20s}`)
	if _, err := f.sup.Start("app"); err != nil {
		t.Fatal(err)
	}
	st := f.waitState(Ready, 10*time.Second)
	if !strings.Contains(st.Health, "log matched") {
		t.Fatalf("health %q", st.Health)
	}
}

func TestPlanPreflight(t *testing.T) {
	f := newFixture(t, "server", "")
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", f.port))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	pl, err := f.sup.Plan("app", "start")
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.Blocking) == 0 || !strings.Contains(pl.Blocking[0], strconv.Itoa(f.port)) {
		t.Fatalf("port conflict not reported: %+v", pl.Blocking)
	}
	if runtime.GOOS == "windows" && pl.ForeignPID != os.Getpid() {
		t.Fatalf("foreign pid %d, want %d", pl.ForeignPID, os.Getpid())
	}
	if _, err := f.sup.Start("app"); err == nil {
		t.Fatal("started despite a port conflict")
	}

	p := f.sup.cfg.Load().Profiles["app"]
	p.Command, p.Args = filepath.Join(f.dir, "missing.exe"), []string{"--listen"}
	p.Env["API_TOKEN"] = "${FOO}"
	f.sup.cfg.Load().Profiles["app"] = p
	pl, _ = f.sup.Plan("app", "start")
	if !strings.Contains(strings.Join(pl.Blocking, ";"), "command not found") {
		t.Fatalf("missing command not reported: %v", pl.Blocking)
	}
	if pl.Bind != "0.0.0.0" || !pl.NetworkExposed {
		t.Fatalf("bare --listen: bind %q exposed %v", pl.Bind, pl.NetworkExposed)
	}
	if !strings.Contains(strings.Join(pl.EnvKeys, ","), "API_TOKEN (redacted)") {
		t.Fatalf("env keys %v", pl.EnvKeys)
	}
	if !strings.Contains(pl.Text(), "BLOCKED") || !strings.Contains(pl.Summary(), "NETWORK EXPOSED") {
		t.Fatalf("text:\n%s\nsummary: %s", pl.Text(), pl.Summary())
	}
}

func TestBindAddress(t *testing.T) {
	for args, want := range map[string]string{
		"main.py --listen 127.0.0.1 --port 8188": "127.0.0.1",
		"main.py --listen --port 8188":           "0.0.0.0",
		"serve --host=0.0.0.0":                   "0.0.0.0",
		"serve":                                  "",
	} {
		if got := bindAddress(strings.Fields(args)); got != want {
			t.Errorf("%q: got %q, want %q", args, got, want)
		}
	}
	if exposed("127.0.0.1") || exposed("localhost") || exposed("") || !exposed("0.0.0.0") || !exposed("192.168.1.2") {
		t.Error("exposed")
	}
}

func TestUpdate(t *testing.T) {
	exe, _ := json.Marshal(os.Args[0])
	f := newFixture(t, "echo", fmt.Sprintf("    update:\n      - [%s, step-one, \"${FOO}\"]\n      - [%s, step-two]", exe, exe))
	pl, err := f.sup.Plan("app", "update")
	if err != nil || len(pl.Steps) != 2 || pl.Steps[0][2] != "bar" {
		t.Fatalf("plan: %+v %v", pl.Steps, err)
	}
	st, err := f.sup.Update("app")
	if err != nil || st.State != Updating {
		t.Fatalf("update: %+v %v", st, err)
	}
	st = f.waitState(Stopped, 20*time.Second)
	logs := strings.Join(f.sup.Logs("app", 0), "\n")
	if st.Health != "update finished" || !strings.Contains(logs, "echo step-one bar") || !strings.Contains(logs, "echo step-two") {
		t.Fatalf("status %+v\nlogs:\n%s", st, logs)
	}

	// A failing step stops the sequence.
	p := f.sup.cfg.Load().Profiles["app"]
	p.Env["SUP_APP"] = "fail"
	f.sup.cfg.Load().Profiles["app"] = p
	f.sup.Update("app")
	st = f.waitState(Exited, 20*time.Second)
	if !strings.Contains(st.Error, "step 1 failed") {
		t.Fatalf("failed update: %+v", st)
	}
}

func TestAdoptAfterRestart(t *testing.T) {
	f := newFixture(t, "server", "")
	if _, err := f.sup.Start("app"); err != nil {
		t.Fatal(err)
	}
	first := f.waitState(Ready, 10*time.Second)

	// A new supervisor over the same state file, as after a broker restart.
	again := New(Options{Config: f.sup.cfg.Load(), StateFile: filepath.Join(f.dir, "runs.json")})
	st := again.Status("app")
	if !st.Adopted || st.PID != first.PID || !st.State.running() {
		t.Fatalf("adopted: %+v", st)
	}
	if _, err := again.Stop(context.Background(), "app"); err != nil {
		t.Fatal(err)
	}
	if alive.PID(first.PID) {
		t.Fatal("adopted run survived stop")
	}
	// The first supervisor sees its run end too; it did not ask for the stop.
	f.waitState(Exited, 10*time.Second)
}

func TestProfilesSharingAPortAreExclusive(t *testing.T) {
	f := newFixture(t, "server", "")
	cfg := f.sup.cfg.Load()
	other := cfg.Profiles["app"]
	other.Env = map[string]string{"SUP_APP": "sleep"} // never binds the port: still loading
	cfg.Profiles["other"] = other
	if _, err := f.sup.Start("other"); err != nil {
		t.Fatal(err)
	}
	defer f.sup.Stop(context.Background(), "other")
	pl, _ := f.sup.Plan("app", "start")
	if len(pl.Blocking) == 0 || !strings.Contains(pl.Blocking[0], "profile other") || pl.ForeignPID != 0 {
		t.Fatalf("plan: %+v", pl.Blocking)
	}
	if _, err := f.sup.Start("app"); err == nil {
		t.Fatal("started a second profile on the same port")
	}
}

func TestRequiresPaths(t *testing.T) {
	f := newFixture(t, "server", "")
	cfg := f.sup.cfg.Load()
	p := cfg.Profiles["app"]
	p.RequiresPaths = []string{f.dir, filepath.Join(f.dir, "not-mounted")}
	cfg.Profiles["app"] = p
	pl, _ := f.sup.Plan("app", "start")
	if len(pl.Blocking) != 1 || !strings.Contains(pl.Blocking[0], "not-mounted is not available") {
		t.Fatalf("blocking: %v", pl.Blocking)
	}
	os.Mkdir(filepath.Join(f.dir, "not-mounted"), 0o700)
	if pl, _ := f.sup.Plan("app", "start"); len(pl.Blocking) != 0 {
		t.Fatalf("still blocked: %v", pl.Blocking)
	}
}
