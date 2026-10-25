package audit

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/trustypangolin/mcp-broker/internal/redact"
)

func TestWriteRedactsAndAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "audit.jsonl")
	red := redact.New()
	red.Add("sk-very-secret")

	l, err := Open(path, red)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.Write(Event{Kind: KindToolCall, Agent: "vscode", Connector: "imap", Tool: "imap_send_email",
				ArgsHash: HashArgs(json.RawMessage(`{"to":"a@b"}`)), Decision: "approve",
				Error: "downstream said: bad key sk-very-secret"})
		}()
	}
	wg.Wait()
	l.Close()

	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "sk-very-secret") {
		t.Fatal("secret leaked into audit log")
	}
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	n := 0
	for sc.Scan() {
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("line %d not JSON: %v", n, err)
		}
		if e.TS.IsZero() || e.Kind != KindToolCall {
			t.Fatalf("bad event %+v", e)
		}
		n++
	}
	if n != 50 {
		t.Fatalf("got %d lines, want 50", n)
	}
}

func TestHashArgsStable(t *testing.T) {
	a := HashArgs(json.RawMessage(`{"b":1,"a":[1,2]}`))
	b := HashArgs(json.RawMessage(`{ "a": [1, 2], "b": 1 }`))
	if a != b || !strings.HasPrefix(a, "sha256:") {
		t.Errorf("hashes differ: %s vs %s", a, b)
	}
	if HashArgs(json.RawMessage(`{"a":2}`)) == a {
		t.Error("different args hashed equal")
	}
	if HashArgs(nil) != "" {
		t.Error("empty args should hash to empty")
	}
}
