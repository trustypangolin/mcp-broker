package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/server"
)

type adminDo func(method, path string, body any) (*http.Response, error)

// adminClient returns a caller for the running broker's admin API, authenticated with the
// dashboard token.
func adminClient(cfgFlag string) (adminDo, error) {
	path, err := config.ResolvePath(cfgFlag)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	token, err := server.AdminToken(cfg.Dashboard.TokenFile)
	if err != nil {
		return nil, err
	}
	_, port, _ := net.SplitHostPort(cfg.Server.Listen)
	base := "http://" + net.JoinHostPort("127.0.0.1", port)
	if cfg.Server.TLS.CertFile != "" {
		base = "https://" + net.JoinHostPort("localhost", port)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	return func(method, url string, body any) (*http.Response, error) {
		var r io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			r = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, base+url, r)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("is the broker running? %w", err)
		}
		return resp, nil
	}, nil
}

// decodeOK decodes a 200 response into v, or turns any other status into an error.
func decodeOK(resp *http.Response, v any) error {
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		var e struct{ Error string }
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s: %s", resp.Status, e.Error)
		}
		return fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(b))
	}
	return json.Unmarshal(b, v)
}

func cmdReload(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("reload", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgFlag := fs.String("config", "", "config file")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	do, err := adminClient(*cfgFlag)
	if err != nil {
		return err
	}
	resp, err := do(http.MethodPost, "/admin/reload", nil)
	if err != nil {
		return err
	}
	var res server.Reloaded
	if err := decodeOK(resp, &res); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "reloaded")
	for _, i := range res.Issues {
		fmt.Fprintln(stdout, "  ", i)
	}
	if len(res.Unloaded) > 0 {
		fmt.Fprintf(stdout, "unloaded (definition changed, restart on next use): %s\n", strings.Join(res.Unloaded, ", "))
	}
	if len(res.Restart) > 0 {
		fmt.Fprintf(stdout, "needs a broker restart to apply: %s\n", strings.Join(res.Restart, ", "))
	}
	return nil
}

func cmdStatus(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgFlag := fs.String("config", "", "config file")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	do, err := adminClient(*cfgFlag)
	if err != nil {
		return err
	}
	resp, err := do(http.MethodGet, "/admin/status", nil)
	if err != nil {
		return err
	}
	var st server.Status
	if err := decodeOK(resp, &st); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "mcp-broker %s at %s, up %s\nconfig %s\nagents %s\npending approvals %d\n\n",
		st.Version, st.URL, time.Since(st.Started).Round(time.Second), st.Config, strings.Join(st.Agents, ", "), st.Pending)
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "CONNECTOR\tSTATE\tTOOLS\tPID")
	for _, c := range st.Connectors {
		state, pid := "idle", ""
		if c.Loaded {
			state = "loaded"
		}
		if c.PID != 0 {
			pid = fmt.Sprint(c.PID)
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", c.Key, state, c.Tools, pid)
	}
	return w.Flush()
}
