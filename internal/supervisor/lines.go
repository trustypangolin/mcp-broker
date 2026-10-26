package supervisor

import (
	"bytes"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/trustypangolin/mcp-broker/internal/redact"
)

// lines is a run's output: an io.Writer that splits into lines, keeps the last n in
// memory, appends them (timestamped) to a log file, and tells onLine about each one.
// Lines are redacted before they are stored anywhere.
type lines struct {
	red    *redact.Redactor
	onLine func(string)

	mu      sync.Mutex
	buf     []string
	next    int
	full    bool
	partial []byte
	file    *rotating
}

func newLines(n int, file *rotating, red *redact.Redactor, onLine func(string)) *lines {
	return &lines{buf: make([]string, n), file: file, red: red, onLine: onLine}
}

func (l *lines) Write(p []byte) (int, error) {
	l.mu.Lock()
	l.partial = append(l.partial, p...)
	var done []string
	for {
		i := bytes.IndexByte(l.partial, '\n')
		if i < 0 {
			break
		}
		done = append(done, string(bytes.TrimRight(l.partial[:i], "\r")))
		l.partial = l.partial[i+1:]
	}
	if len(l.partial) > 64<<10 { // a runaway line without newlines
		done = append(done, string(l.partial))
		l.partial = nil
	}
	l.mu.Unlock()
	for _, line := range done {
		l.add(line)
	}
	return len(p), nil
}

func (l *lines) add(line string) {
	line = l.red.String(line)
	l.mu.Lock()
	l.buf[l.next] = line
	l.next = (l.next + 1) % len(l.buf)
	if l.next == 0 {
		l.full = true
	}
	if l.file != nil {
		l.file.writeLine(time.Now(), line)
	}
	l.mu.Unlock()
	if l.onLine != nil {
		l.onLine(line)
	}
}

func (l *lines) tail(n int) []string {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var all []string
	if l.full {
		all = append(all, l.buf[l.next:]...)
	}
	all = append(all, l.buf[:l.next]...)
	if n > 0 && len(all) > n {
		all = all[len(all)-n:]
	}
	return append([]string(nil), all...)
}

// Close flushes a trailing partial line and closes the log file.
func (l *lines) Close() {
	l.mu.Lock()
	rest := l.partial
	l.partial = nil
	l.mu.Unlock()
	if len(rest) > 0 {
		l.add(string(rest))
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		l.file.close()
		l.file = nil
	}
}

// rotating is an append-only log file that moves itself to <name>.1 past maxLogBytes.
type rotating struct {
	path string
	f    *os.File
	size int64
}

const maxLogBytes = 10 << 20

func openRotating(path string) (*rotating, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	fi, _ := f.Stat()
	r := &rotating{path: path, f: f}
	if fi != nil {
		r.size = fi.Size()
	}
	return r, nil
}

func (r *rotating) writeLine(t time.Time, line string) {
	if r.size > maxLogBytes {
		r.f.Close()
		os.Rename(r.path, r.path+".1")
		f, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			r.f = nil
			return
		}
		r.f, r.size = f, 0
	}
	if r.f == nil {
		return
	}
	n, _ := fmt.Fprintf(r.f, "%s %s\n", t.Format("2006-01-02T15:04:05.000"), line)
	r.size += int64(n)
}

func (r *rotating) close() {
	if r.f != nil {
		r.f.Close()
	}
}
