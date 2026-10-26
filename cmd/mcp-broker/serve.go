package main

import (
	"flag"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/trustypangolin/mcp-broker/internal/approval"
	"github.com/trustypangolin/mcp-broker/internal/broker"
	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/proc"
)

func cmdServe(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgFlag := fs.String("config", "", "config file")
	listen := fs.String("listen", "", "override server.listen")
	watch := fs.Bool("watch", true, "reload the config when the file changes")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	path, err := config.ResolvePath(*cfgFlag)
	if err != nil {
		return err
	}
	defer proc.KillAll()
	b, err := broker.Start(broker.Options{ConfigPath: path, Listen: *listen, Stderr: stderr, Watch: *watch})
	if err != nil {
		return err
	}
	defer b.Close()
	go logApprovals(b.Approvals(), b.Logger())

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-b.Err():
		return err
	case <-sig:
		b.Logger().Info("shutting down")
	}
	return nil
}

// logApprovals prints new pending approvals, so they are visible before the tray exists.
func logApprovals(q *approval.Queue, log *slog.Logger) {
	ch, _ := q.Subscribe()
	for e := range ch {
		if e.Status == approval.Pending {
			log.Info("approval pending", "id", e.ID, "summary", e.Summary, "approve_with", "mcp-broker approvals approve "+e.ID)
		} else {
			log.Info("approval "+string(e.Status), "id", e.ID, "channel", e.Channel)
		}
	}
}
