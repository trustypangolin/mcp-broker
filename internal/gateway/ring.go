package gateway

import (
	"bytes"
	"io"
	"strings"
	"sync"
)

// ring keeps the last n lines written to it, and optionally tees them to a file.
type ring struct {
	mu      sync.Mutex
	lines   []string
	next    int
	full    bool
	partial []byte
	file    io.WriteCloser
}

func newRing(n int, file io.WriteCloser) *ring {
	return &ring{lines: make([]string, n), file: file}
}

func (r *ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file != nil {
		r.file.Write(p)
	}
	data := append(r.partial, p...)
	for {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		r.add(strings.TrimRight(string(data[:i]), "\r"))
		data = data[i+1:]
	}
	r.partial = append(r.partial[:0], data...)
	return len(p), nil
}

func (r *ring) add(line string) {
	r.lines[r.next] = line
	r.next = (r.next + 1) % len(r.lines)
	if r.next == 0 {
		r.full = true
	}
}

// Tail returns up to n most recent lines, oldest first.
func (r *ring) Tail(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var all []string
	if r.full {
		all = append(all, r.lines[r.next:]...)
	}
	all = append(all, r.lines[:r.next]...)
	if n < len(all) {
		all = all[len(all)-n:]
	}
	return all
}

func (r *ring) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file != nil {
		err := r.file.Close()
		r.file = nil
		return err
	}
	return nil
}

func joinLines(lines []string) string { return strings.Join(lines, "\n") }
