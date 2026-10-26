package broker

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

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
