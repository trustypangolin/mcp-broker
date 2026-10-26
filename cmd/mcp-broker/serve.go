package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/trustypangolin/mcp-broker/internal/approval"
	"github.com/trustypangolin/mcp-broker/internal/audit"
	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/gateway"
	"github.com/trustypangolin/mcp-broker/internal/identity"
	"github.com/trustypangolin/mcp-broker/internal/policy"
	"github.com/trustypangolin/mcp-broker/internal/proc"
	"github.com/trustypangolin/mcp-broker/internal/redact"
	"github.com/trustypangolin/mcp-broker/internal/server"
	"github.com/trustypangolin/mcp-broker/internal/version"
)

func cmdServe(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgFlag := fs.String("config", "", "config file")
	listen := fs.String("listen", "", "override server.listen")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}

	path, err := config.ResolvePath(*cfgFlag)
	if err != nil {
		return err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	if *listen != "" {
		cfg.Server.Listen = *listen
	}

	logDir := filepath.Join(cfg.Dir, "logs")
	log, closeLog, err := newLogger(logDir, stderr)
	if err != nil {
		return err
	}
	defer closeLog()
	log.Info("starting", "version", version.String(), "config", cfg.Path)
	for _, i := range cfg.Check() {
		log.Warn("config check", "issue", i.String())
	}

	if err := proc.JoinKillOnCloseJob(); err != nil {
		log.Warn("could not join kill-on-close job; children may outlive a crashed broker", "err", err)
	}
	defer proc.KillAll()

	red := redact.New()
	aud, err := audit.Open(cfg.Audit.Path, red)
	if err != nil {
		return err
	}
	defer aud.Close()
	store, err := identity.OpenStore(cfg.Server.TokensFile)
	if err != nil {
		return err
	}
	adminToken, err := server.AdminToken(cfg.Dashboard.TokenFile)
	if err != nil {
		return fmt.Errorf("admin token: %w", err)
	}

	queue := approval.NewQueue(cfg.Approvals.Timeout)
	go logApprovals(queue, log)
	gw := gateway.New(gateway.Options{
		Config: cfg, Policy: policy.New(cfg), Approvals: queue, Audit: aud, Redactor: red,
		Logger: log, LogDir: logDir,
	})
	defer gw.Close()

	handler := server.Handler(server.Options{
		Config: cfg, Identifier: identity.New(cfg, store), Gateway: gw, Audit: aud, Logger: log,
		Admin: &server.AdminOptions{Config: cfg, Token: adminToken, Approvals: queue, Audit: aud},
	})
	ln, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() {
		if cfg.Server.TLS.CertFile != "" {
			errc <- srv.ServeTLS(ln, cfg.Server.TLS.CertFile, cfg.Server.TLS.KeyFile)
		} else {
			errc <- srv.Serve(ln)
		}
	}()
	scheme := "http"
	if cfg.Server.TLS.CertFile != "" {
		scheme = "https"
	}
	log.Info("listening", "url", fmt.Sprintf("%s://%s/mcp", scheme, ln.Addr()))
	aud.Write(audit.Event{Kind: audit.KindConfigChanged, Detail: "broker started " + version.Version})

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		log.Info("shutting down")
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
	return nil
}

// newLogger logs human text to stderr and JSON to logs/broker.log.
func newLogger(dir string, stderr io.Writer) (*slog.Logger, func(), error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "broker.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, err
	}
	h := multiHandler{
		slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}),
		slog.NewJSONHandler(f, &slog.HandlerOptions{Level: slog.LevelDebug}),
	}
	return slog.New(h), func() { f.Close() }, nil
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

type multiHandler []slog.Handler

func (m multiHandler) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range m {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (m multiHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range m {
		if h.Enabled(ctx, r.Level) {
			h.Handle(ctx, r.Clone())
		}
	}
	return nil
}

func (m multiHandler) WithAttrs(a []slog.Attr) slog.Handler {
	out := make(multiHandler, len(m))
	for i, h := range m {
		out[i] = h.WithAttrs(a)
	}
	return out
}

func (m multiHandler) WithGroup(n string) slog.Handler {
	out := make(multiHandler, len(m))
	for i, h := range m {
		out[i] = h.WithGroup(n)
	}
	return out
}
