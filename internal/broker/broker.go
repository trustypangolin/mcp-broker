// Package broker assembles and runs the broker: config, token store, audit log, gateway and
// HTTP server. `mcp-broker serve` and the tray app both host one.
package broker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/trustypangolin/mcp-broker/internal/approval"
	"github.com/trustypangolin/mcp-broker/internal/audit"
	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/gateway"
	"github.com/trustypangolin/mcp-broker/internal/identity"
	"github.com/trustypangolin/mcp-broker/internal/proc"
	"github.com/trustypangolin/mcp-broker/internal/redact"
	"github.com/trustypangolin/mcp-broker/internal/server"
	"github.com/trustypangolin/mcp-broker/internal/version"
)

// Options configures Start.
type Options struct {
	ConfigPath string
	// Listen overrides server.listen.
	Listen string
	// Stderr receives human-readable log lines; nil discards them. JSON logs always go to
	// logs/broker.log next to the config.
	Stderr io.Writer
	// Watch reloads the config when the file changes.
	Watch bool
	// OnReload, if set, is told about every reload attempt (including the watcher's).
	OnReload func(server.Reloaded, error)
}

// Broker is a running broker.
type Broker struct {
	opts     Options
	log      *slog.Logger
	closeLog func()

	cfg     atomic.Pointer[config.Config]
	red     *redact.Redactor
	aud     *audit.Log
	queue   *approval.Queue
	gw      *gateway.Gateway
	ident   *identity.Identifier
	admin   string
	srv     *http.Server
	base    string
	started time.Time
	errc    chan error

	reloadMu sync.Mutex
	loaded   fileStamp // of the config file as last loaded

	stopWatch chan struct{}
	watchDone chan struct{}
	closeOnce sync.Once
}

var joinJob sync.Once

// Start loads the config and starts serving. The returned Broker runs until Close.
func Start(opts Options) (b *Broker, err error) {
	stamp := stat(opts.ConfigPath)
	cfg, err := config.Load(opts.ConfigPath)
	if err != nil {
		return nil, err
	}
	if opts.Listen != "" {
		cfg.Server.Listen = opts.Listen
	}
	b = &Broker{opts: opts, loaded: stamp, errc: make(chan error, 1)}
	b.cfg.Store(cfg)

	stderr := opts.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	logDir := filepath.Join(cfg.Dir, "logs")
	if b.log, b.closeLog, err = newLogger(logDir, stderr); err != nil {
		return nil, err
	}
	var cleanup []func()
	defer func() {
		if err != nil {
			for i := len(cleanup) - 1; i >= 0; i-- {
				cleanup[i]()
			}
			b.closeLog()
		}
	}()

	b.log.Info("starting", "version", version.String(), "config", cfg.Path)
	for _, i := range cfg.Check() {
		b.log.Warn("config check", "issue", i.String())
	}
	joinJob.Do(func() {
		if err := proc.JoinKillOnCloseJob(); err != nil {
			b.log.Warn("could not join kill-on-close job; children may outlive a crashed broker", "err", err)
		}
	})

	b.red = redact.New()
	if b.aud, err = audit.Open(cfg.Audit.Path, b.red); err != nil {
		return nil, err
	}
	cleanup = append(cleanup, func() { b.aud.Close() })
	store, err := identity.OpenStore(cfg.Server.TokensFile)
	if err != nil {
		return nil, err
	}
	if b.admin, err = server.AdminToken(cfg.Dashboard.TokenFile); err != nil {
		return nil, fmt.Errorf("admin token: %w", err)
	}

	ln, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		return nil, err
	}
	cleanup = append(cleanup, func() { ln.Close() })
	b.base = localBase(cfg, ln.Addr())

	b.queue = approval.NewQueue(cfg.Approvals.Timeout)
	b.gw = gateway.New(gateway.Options{
		Config: cfg, Approvals: b.queue, Audit: b.aud, Redactor: b.red, Logger: b.log, LogDir: logDir,
	})
	cleanup = append(cleanup, b.gw.Close)
	b.ident = identity.New(cfg, store)
	handler := server.Handler(server.Options{
		Identifier: b.ident, Gateway: b.gw, Audit: b.aud, Logger: b.log,
		Admin: &server.AdminOptions{Control: b, Token: b.admin, Approvals: b.queue, Audit: b.aud},
	})
	b.srv = &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		var err error
		if cfg.Server.TLS.CertFile != "" {
			err = b.srv.ServeTLS(ln, cfg.Server.TLS.CertFile, cfg.Server.TLS.KeyFile)
		} else {
			err = b.srv.Serve(ln)
		}
		if !errors.Is(err, http.ErrServerClosed) {
			b.errc <- err
		}
		close(b.errc)
	}()
	b.started = time.Now()
	b.log.Info("listening", "url", b.base+"/mcp", "listen", ln.Addr().String())
	b.aud.Write(audit.Event{Kind: audit.KindConfigChanged, Detail: "broker started " + version.Version})

	if opts.Watch {
		b.stopWatch, b.watchDone = make(chan struct{}), make(chan struct{})
		go b.watch()
	}
	return b, nil
}

// localBase is the URL this host's clients use: loopback when listening on all interfaces.
func localBase(cfg *config.Config, addr net.Addr) string {
	_, port, _ := net.SplitHostPort(addr.String())
	if cfg.Server.TLS.CertFile != "" {
		return "https://" + net.JoinHostPort("localhost", port)
	}
	return "http://" + net.JoinHostPort("127.0.0.1", port)
}

// Close stops serving, unloads every connector and closes the logs.
func (b *Broker) Close() {
	b.closeOnce.Do(func() {
		if b.stopWatch != nil {
			close(b.stopWatch)
			<-b.watchDone
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		b.srv.Shutdown(ctx)
		b.gw.Close()
		b.log.Info("stopped")
		b.aud.Close()
		b.closeLog()
	})
}

// Err delivers a serve error (the listener failing) and is closed when serving stops.
func (b *Broker) Err() <-chan error { return b.errc }

// Config implements server.Control.
func (b *Broker) Config() *config.Config { return b.cfg.Load() }

// Approvals is the approval queue, for the tray's notifications.
func (b *Broker) Approvals() *approval.Queue { return b.queue }

// BaseURL is the broker's loopback URL, without a path.
func (b *Broker) BaseURL() string { return b.base }

// AdminToken is the dashboard/admin bearer token.
func (b *Broker) AdminToken() string { return b.admin }

// Tokens is the agent token store.
func (b *Broker) Tokens() *identity.Store { return b.ident.Store() }

// Logger is the broker's logger.
func (b *Broker) Logger() *slog.Logger { return b.log }

// Status implements server.Control.
func (b *Broker) Status() server.Status {
	cfg := b.Config()
	st := server.Status{
		Version: version.Version, Config: cfg.Path, URL: b.base + "/mcp", Started: b.started,
		Pending: len(b.queue.List(true)), Connectors: b.gw.Connectors(),
	}
	for name := range cfg.Agents {
		st.Agents = append(st.Agents, name)
	}
	slices.Sort(st.Agents)
	return st
}

// Unload implements server.Control.
func (b *Broker) Unload(key string) bool {
	ok := b.gw.Unload(key)
	if ok {
		b.aud.Write(audit.Event{Kind: audit.KindStop, Connector: key, Detail: "unloaded from admin"})
	}
	return ok
}

// Reload implements server.Control. An invalid file leaves the running config in force.
func (b *Broker) Reload() (server.Reloaded, error) {
	b.reloadMu.Lock()
	defer b.reloadMu.Unlock()
	res, err := b.reloadLocked()
	if err != nil {
		b.log.Error("reload failed", "err", err)
		b.aud.Write(audit.Event{Kind: audit.KindConfigChanged, Decision: "rejected", Error: err.Error()})
	} else {
		b.log.Info("config reloaded", "unloaded", res.Unloaded, "restart_needed", res.Restart)
		b.aud.Write(audit.Event{Kind: audit.KindConfigChanged, Detail: "reloaded"})
	}
	if b.opts.OnReload != nil {
		b.opts.OnReload(res, err)
	}
	return res, err
}

func (b *Broker) reloadLocked() (server.Reloaded, error) {
	var res server.Reloaded
	stamp := stat(b.opts.ConfigPath)
	cfg, err := config.Load(b.opts.ConfigPath)
	b.loaded = stamp // a broken file is not retried until it changes again
	if err != nil {
		return res, err
	}
	if b.opts.Listen != "" {
		cfg.Server.Listen = b.opts.Listen
	}
	old := b.Config()
	for _, i := range cfg.Check() {
		res.Issues = append(res.Issues, i.String())
	}
	// These are wired into long-lived objects at start.
	for name, same := range map[string]bool{
		"server.listen":        old.Server.Listen == cfg.Server.Listen,
		"server.tls":           old.Server.TLS == cfg.Server.TLS,
		"server.tokens_file":   old.Server.TokensFile == cfg.Server.TokensFile,
		"dashboard.token_file": old.Dashboard.TokenFile == cfg.Dashboard.TokenFile,
		"audit.path":           old.Audit.Path == cfg.Audit.Path,
		"approvals.timeout":    old.Approvals.Timeout == cfg.Approvals.Timeout,
		"approvals.notify":     reflect.DeepEqual(old.Approvals.Notify, cfg.Approvals.Notify),
	} {
		if !same {
			res.Restart = append(res.Restart, name)
		}
	}
	slices.Sort(res.Restart)
	b.cfg.Store(cfg)
	b.ident.SetConfig(cfg)
	res.Unloaded = b.gw.Reload(cfg)
	return res, nil
}

// fileStamp identifies a version of a file well enough to notice edits.
type fileStamp struct {
	mod  time.Time
	size int64
}

func stat(path string) fileStamp {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{fi.ModTime(), fi.Size()}
}

// watchInterval is how often the watcher polls the config file. Polling keeps this
// dependency-free and copes with editors that replace the file rather than writing it.
var watchInterval = 2 * time.Second

func (b *Broker) watch() {
	defer close(b.watchDone)
	t := time.NewTicker(watchInterval)
	defer t.Stop()
	for {
		select {
		case <-b.stopWatch:
			return
		case <-t.C:
		}
		s := stat(b.opts.ConfigPath)
		b.reloadMu.Lock()
		changed := s != b.loaded && s != (fileStamp{})
		b.reloadMu.Unlock()
		if !changed {
			continue
		}
		// Let an editor finish writing before reading.
		select {
		case <-b.stopWatch:
			return
		case <-time.After(300 * time.Millisecond):
		}
		if stat(b.opts.ConfigPath) != s {
			continue // still changing; catch it next tick
		}
		b.Reload()
	}
}
