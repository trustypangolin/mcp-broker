package approval

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/trustypangolin/mcp-broker/internal/config"
)

var both = []config.Channel{config.ChanTray, config.ChanDashboard}

func plan() Plan {
	return Plan{Kind: "tool.call", Agent: "vscode", Connector: "imap", Tool: "imap_send_email", ArgsHash: "sha256:x", Channels: both}
}

func TestFirstDecisionWins(t *testing.T) {
	q := NewQueue(time.Minute)
	e := q.Submit(plan())
	if e.Status != Pending || e.ID == "" {
		t.Fatalf("submit = %+v", e)
	}
	if _, err := q.Decide(e.ID, true, config.ChanTray); err != nil {
		t.Fatal(err)
	}
	got, err := q.Decide(e.ID, false, config.ChanDashboard)
	if !errors.Is(err, ErrDecided) || got.Status != Approved || got.Channel != config.ChanTray {
		t.Fatalf("second decision = %+v, %v", got, err)
	}
}

func TestChannelMustBeAllowed(t *testing.T) {
	q := NewQueue(time.Minute)
	e := q.Submit(plan())
	if _, err := q.Decide(e.ID, true, config.ChanElicitation); !errors.Is(err, ErrChannel) {
		t.Fatalf("elicitation approved a tray/dashboard-only plan: %v", err)
	}
	if got, _ := q.Get(e.ID); got.Status != Pending {
		t.Fatalf("status = %s", got.Status)
	}
}

func TestNoChannelsDeniedAtOnce(t *testing.T) {
	q := NewQueue(time.Minute)
	p := plan()
	p.Channels = nil
	if e := q.Submit(p); e.Status != Denied {
		t.Fatalf("status = %s", e.Status)
	}
}

func TestTimeoutDenies(t *testing.T) {
	q := NewQueue(30 * time.Millisecond)
	e := q.Submit(plan())
	got, _ := q.Wait(context.Background(), e.ID)
	if got.Status != Expired {
		t.Fatalf("status = %s", got.Status)
	}
	if _, err := q.Decide(e.ID, true, config.ChanTray); !errors.Is(err, ErrDecided) {
		t.Fatal("decided after expiry")
	}
}

func TestWaitReturnsOnDecision(t *testing.T) {
	q := NewQueue(time.Minute)
	e := q.Submit(plan())
	go func() {
		time.Sleep(20 * time.Millisecond)
		q.Decide(e.ID, false, config.ChanDashboard)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if got, _ := q.Wait(ctx, e.ID); got.Status != Denied {
		t.Fatalf("status = %s", got.Status)
	}
}

func TestConsumeOnceAndBound(t *testing.T) {
	q := NewQueue(time.Minute)
	e := q.Submit(plan())
	b := e.binding()

	if err := q.Consume(e.ID, b); !errors.Is(err, ErrNotApproved) {
		t.Fatalf("consumed pending plan: %v", err)
	}
	q.Decide(e.ID, true, config.ChanTray)

	other := b
	other.ArgsHash = "sha256:different"
	if err := q.Consume(e.ID, other); !errors.Is(err, ErrWrongBinding) {
		t.Fatalf("approval spent on different args: %v", err)
	}
	other = b
	other.Agent = "wsl"
	if err := q.Consume(e.ID, other); !errors.Is(err, ErrWrongBinding) {
		t.Fatalf("approval spent by another agent: %v", err)
	}
	if err := q.Consume(e.ID, b); err != nil {
		t.Fatal(err)
	}
	if err := q.Consume(e.ID, b); !errors.Is(err, ErrUsed) {
		t.Fatalf("approval reused: %v", err)
	}
}

func TestSubscribe(t *testing.T) {
	q := NewQueue(time.Minute)
	ch, cancel := q.Subscribe()
	defer cancel()
	e := q.Submit(plan())
	q.Decide(e.ID, true, config.ChanTray)
	for _, want := range []Status{Pending, Approved} {
		select {
		case got := <-ch:
			if got.ID != e.ID || got.Status != want {
				t.Fatalf("got %s %s, want %s", got.ID, got.Status, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("no %s event", want)
		}
	}
	if n := len(q.List(true)); n != 0 {
		t.Errorf("pending list has %d", n)
	}
}
