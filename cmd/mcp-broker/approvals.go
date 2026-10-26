package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"text/tabwriter"

	"github.com/trustypangolin/mcp-broker/internal/approval"
)

const approvalsUsage = `usage: mcp-broker approvals [list | approve <id> | deny <id>] [--config PATH]
`

// cmdApprovals talks to the running broker's admin API with the dashboard token.
func cmdApprovals(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("approvals", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgFlag := fs.String("config", "", "config file")
	var pos []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return errUsage
		}
		rest = fs.Args()
		if len(rest) == 0 {
			break
		}
		pos, rest = append(pos, rest[0]), rest[1:]
	}
	do, err := adminClient(*cfgFlag)
	if err != nil {
		return err
	}

	sub := "list"
	if len(pos) > 0 {
		sub = pos[0]
	}
	switch sub {
	case "list":
		resp, err := do(http.MethodGet, "/admin/approvals", nil)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		var out struct{ Approvals []approval.Entry }
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return fmt.Errorf("broker answered %s", resp.Status)
		}
		w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tAGENT\tEXPIRES\tSUMMARY")
		for _, e := range out.Approvals {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", e.ID, e.Agent, e.Expires.Local().Format("15:04:05"), e.Summary)
		}
		return w.Flush()
	case "approve", "deny":
		if len(pos) != 2 {
			fmt.Fprint(stderr, approvalsUsage)
			return errUsage
		}
		resp, err := do(http.MethodPost, "/admin/approvals/"+pos[1], map[string]bool{"approve": sub == "approve"})
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(b))
		}
		past := map[string]string{"approve": "approved", "deny": "denied"}[sub]
		fmt.Fprintf(stdout, "%s %s\n", past, pos[1])
		return nil
	}
	fmt.Fprint(stderr, approvalsUsage)
	return errUsage
}
