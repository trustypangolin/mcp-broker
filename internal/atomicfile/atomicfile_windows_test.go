package atomicfile

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestRenameWaitsForAHolder holds the target open the way a virus scanner does (no delete
// sharing, so a rename over it is refused), lets go after a moment, and expects Rename to
// have waited rather than failed.
func TestRenameWaitsForAHolder(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "state.yaml")
	tmp := filepath.Join(dir, ".state.tmp")
	os.WriteFile(dst, []byte("old"), 0o600)
	os.WriteFile(tmp, []byte("new"), 0o600)

	name, _ := syscall.UTF16PtrFromString(dst)
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ, syscall.FILE_SHARE_READ, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, dst); err == nil {
		t.Skip("this Windows lets a rename replace a file held open without delete sharing")
	} else if !transient(err) {
		t.Fatalf("held-open rename failed with %v, which Rename wouldn't retry", err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		syscall.CloseHandle(h)
	}()
	if err := Rename(tmp, dst); err != nil {
		t.Fatalf("Rename gave up: %v", err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "new" {
		t.Fatalf("dst = %q", b)
	}
}

func TestRenameGivesUpOnRealErrors(t *testing.T) {
	dir := t.TempDir()
	start := time.Now()
	if err := Rename(filepath.Join(dir, "missing"), filepath.Join(dir, "dst")); err == nil {
		t.Fatal("renamed a missing file")
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Error("retried an error that isn't transient")
	}
}
