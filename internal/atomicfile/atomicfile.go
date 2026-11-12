// Package atomicfile replaces files the way the broker saves state: write a temp file
// beside the target, then rename it over the target.
package atomicfile

import (
	"errors"
	"os"
	"runtime"
	"syscall"
	"time"
)

// Rename moves tmp over path. On Windows a virus scanner, the search indexer or a backup
// tool often holds a just-written file open for a moment, and the rename then fails with
// "Access is denied" or a sharing violation; Rename retries those for about a second
// before giving up. Elsewhere it is os.Rename.
func Rename(tmp, path string) error {
	err := os.Rename(tmp, path)
	if runtime.GOOS != "windows" {
		return err
	}
	for delay := 10 * time.Millisecond; err != nil && transient(err) && delay <= 320*time.Millisecond; delay *= 2 {
		time.Sleep(delay)
		err = os.Rename(tmp, path)
	}
	return err
}

// transient reports whether err is the Windows error another process briefly holding the
// file produces: access denied, sharing violation or lock violation.
func transient(err error) bool {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case 5, 32, 33: // ERROR_ACCESS_DENIED, ERROR_SHARING_VIOLATION, ERROR_LOCK_VIOLATION
			return true
		}
	}
	return false
}
