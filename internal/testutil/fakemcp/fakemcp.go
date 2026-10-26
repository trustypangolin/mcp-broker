// Package fakemcp is a downstream MCP server for tests. A test binary runs it as a child
// process of itself: call MaybeRun at the top of TestMain, and point a connector at
// os.Args[0] with Env() in its environment.
package fakemcp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	envMode = "FAKEMCP"
	// ModeCurrent speaks the SDK's newest protocol (elicitation via input requests).
	ModeCurrent = "current"
	// ModeLegacy pins 2025-11-25 (elicitation via server-initiated requests).
	ModeLegacy = "legacy"
	envSleep   = "FAKEMCP_SLEEPER"
)

// Env returns the environment entries that make a test binary act as the fake server.
func Env(mode string) map[string]string { return map[string]string{envMode: mode} }

// MaybeRun serves MCP on stdio and exits if this process was started as the fake server.
func MaybeRun() {
	if os.Getenv(envSleep) == "1" {
		time.Sleep(time.Hour)
		os.Exit(0)
	}
	mode := os.Getenv(envMode)
	if mode == "" {
		return
	}
	opts := &mcp.ServerOptions{}
	if mode == ModeLegacy {
		opts.SupportedProtocolVersions = []string{"2025-11-25"}
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "fakemcp", Version: "1"}, opts)
	register(s)
	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "fakemcp:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

type textIn struct {
	Text string `json:"text"`
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{Name: "get_echo", Description: "Echo text back"},
		func(_ context.Context, _ *mcp.CallToolRequest, in textIn) (*mcp.CallToolResult, any, error) {
			return text("echo: " + in.Text), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "delete_thing", Description: "Pretend to delete"},
		func(_ context.Context, _ *mcp.CallToolRequest, in textIn) (*mcp.CallToolResult, any, error) {
			return text("deleted " + in.Text), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "send_message", Description: "Pretend to send"},
		func(_ context.Context, _ *mcp.CallToolRequest, in textIn) (*mcp.CallToolResult, any, error) {
			return text("sent " + in.Text), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "get_secret_echo", Description: "Echo an env var value"},
		func(_ context.Context, _ *mcp.CallToolRequest, in textIn) (*mcp.CallToolResult, any, error) {
			return text("value: " + os.Getenv(in.Text)), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "get_big", Description: "Return a large result"},
		func(_ context.Context, _ *mcp.CallToolRequest, in textIn) (*mcp.CallToolResult, any, error) {
			n, _ := strconv.Atoi(in.Text)
			b := make([]byte, n)
			for i := range b {
				b[i] = 'x'
			}
			return text(string(b)), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "ask_colour", Description: "Ask the user for a colour"},
		func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			params := &mcp.ElicitParams{
				Mode:    "form",
				Message: "Pick a colour",
				RequestedSchema: map[string]any{
					"type":       "object",
					"properties": map[string]any{"colour": map[string]any{"type": "string"}},
					"required":   []string{"colour"},
				},
			}
			var res *mcp.ElicitResult
			if req.ProtocolVersion() >= "2026-07-28" {
				r, ok := req.Params.InputResponses["colour"]
				if !ok {
					return &mcp.CallToolResult{InputRequests: mcp.InputRequestMap{"colour": params}}, nil, nil
				}
				res, _ = r.(*mcp.ElicitResult)
			} else {
				var err error
				if res, err = req.Session.Elicit(ctx, params); err != nil {
					return nil, nil, err
				}
			}
			if res == nil || res.Action != "accept" {
				return text("no colour"), nil, nil
			}
			return text(fmt.Sprintf("colour: %v", res.Content["colour"])), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "get_progress", Description: "Report progress three times"},
		func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			if tok := req.Params.GetProgressToken(); tok != nil {
				// Spaced like real work: notifications racing the final result may arrive after
				// the call has completed, which neither the SDK nor the broker relays.
				for i := 1; i <= 3; i++ {
					req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{ProgressToken: tok, Progress: float64(i), Total: 3})
					time.Sleep(50 * time.Millisecond)
				}
			}
			return text("done"), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "get_child_pid", Description: "Spawn a grandchild and return its PID"},
		func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			c := exec.Command(os.Args[0])
			c.Env = append(os.Environ(), envSleep+"=1", envMode+"=")
			if err := c.Start(); err != nil {
				return nil, nil, err
			}
			return text(strconv.Itoa(c.Process.Pid)), nil, nil
		})
}
