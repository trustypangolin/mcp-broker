package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCompare(t *testing.T) {
	order := []string{
		"v0.1.0-alpha.2",
		"v0.1.0-alpha.9",
		"v0.1.0-alpha.12",
		"v0.1.0-alpha.12-1-g26ac1c9",
		"v0.1.0-alpha.12-4-gabcdef0-dirty",
		"v0.1.0-alpha.13",
		"v0.1.0-beta",
		"v0.1.0-beta.1",
		"v0.1.0",
		"v0.1.1",
		"v0.2.0",
		"v1.0.0",
	}
	for i := range order {
		for j := range order {
			a, b := order[i], order[j]
			want := cmpInt(i, j)
			if got := cmpParsed(t, a, b); got != want {
				t.Errorf("compare(%s, %s) = %d, want %d", a, b, got, want)
			}
		}
	}
	for _, bad := range []string{"dev", "", "v1.2", "v1.2.x", "v1.2.3-"} {
		if _, err := parse(bad); err == nil {
			t.Errorf("parse(%q) succeeded", bad)
		}
	}
	if NewChecker("dev", true) != nil {
		t.Error("a dev build got a checker")
	}
	if !Newer("v0.1.0-alpha.13", "v0.1.0-alpha.12-1-g26ac1c9") || Newer("v0.1.0-alpha.12", "v0.1.0-alpha.12-1-g26ac1c9") {
		t.Error("Newer disagrees with compare")
	}
}

func cmpParsed(t *testing.T, a, b string) int {
	t.Helper()
	va, err := parse(a)
	if err != nil {
		t.Fatal(err)
	}
	vb, err := parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return compare(va, vb)
}

// fakeGitHub serves a release list and the assets of each release.
type fakeGitHub struct {
	srv      *httptest.Server
	releases []map[string]any
	files    map[string][]byte
}

func newFake(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{files: map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/releases" {
			json.NewEncoder(w).Encode(f.releases)
			return
		}
		b, ok := f.files[strings.TrimPrefix(r.URL.Path, "/dl/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) add(tag string, pre bool, files map[string][]byte) {
	var assets []map[string]any
	for name, b := range files {
		f.files[tag+"/"+name] = b
		assets = append(assets, map[string]any{"name": name, "browser_download_url": f.srv.URL + "/dl/" + tag + "/" + name, "size": len(b)})
	}
	f.releases = append(f.releases, map[string]any{"tag_name": tag, "html_url": "https://example.test/" + tag, "prerelease": pre, "assets": assets})
}

func (f *fakeGitHub) checker(cur string, pre bool) *Checker {
	c := NewChecker(cur, pre)
	c.API = f.srv.URL
	return c
}

func TestCheckPicksNewest(t *testing.T) {
	f := newFake(t)
	f.add("v0.1.0-alpha.13", true, nil)
	f.add("v0.1.0-alpha.15", true, nil)
	f.add("v0.1.0-alpha.14", true, nil)
	f.add("v0.0.9", false, nil)
	f.releases = append(f.releases, map[string]any{"tag_name": "v9.9.9", "draft": true})

	r, err := f.checker("v0.1.0-alpha.12-1-g26ac1c9", true).Check(context.Background())
	if err != nil || r == nil || r.Version != "v0.1.0-alpha.15" {
		t.Fatalf("got %+v, %v; want alpha.15", r, err)
	}
	if r, _ := f.checker("v0.1.0-alpha.12", false).Check(context.Background()); r != nil {
		t.Errorf("stable channel offered %s", r.Version)
	}
	c := f.checker("v0.1.0-alpha.15", true)
	if r, err := c.Check(context.Background()); r != nil || err != nil || c.Available() != nil {
		t.Errorf("up to date, got %+v, %v", r, err)
	}
}

// release builds a release archive for this platform holding the given programs.
func release(t *testing.T, tag string, programs map[string]string) (string, []byte) {
	t.Helper()
	name := ArchiveName(tag, runtime.GOOS, runtime.GOARCH)
	dir := strings.TrimSuffix(strings.TrimSuffix(name, ".zip"), ".tar.gz") + "/"
	var buf bytes.Buffer
	if strings.HasSuffix(name, ".zip") {
		zw := zip.NewWriter(&buf)
		for n, body := range programs {
			w, _ := zw.Create(dir + n)
			w.Write([]byte(body))
		}
		zw.Create(dir + "README.md")
		zw.Close()
	} else {
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		for n, body := range programs {
			tw.WriteHeader(&tar.Header{Name: dir + n, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
			tw.Write([]byte(body))
		}
		tw.Close()
		gz.Close()
	}
	return name, buf.Bytes()
}

func sums(name string, data []byte) []byte {
	h := sha256.Sum256(data)
	return []byte(fmt.Sprintf("%s  other.zip\n%s  %s\n", strings.Repeat("0", 64), hex.EncodeToString(h[:]), name))
}

func programsWith(body string) map[string]string {
	out := map[string]string{}
	for _, p := range Programs() {
		out[Exe(p)] = body + " " + p
	}
	return out
}

func TestInstall(t *testing.T) {
	const cur, next = "v0.1.0-alpha.12", "v0.1.0-alpha.13"
	f := newFake(t)
	name, archive := release(t, next, programsWith("new"))
	f.add(next, true, map[string][]byte{name: archive, "checksums.txt": sums(name, archive)})

	dir := t.TempDir()
	for _, p := range Programs() {
		os.WriteFile(filepath.Join(dir, Exe(p)), []byte("old "+p), 0o755)
	}
	c := f.checker(cur, true)
	r, err := c.Check(context.Background())
	if err != nil || r == nil {
		t.Fatal(r, err)
	}
	replaced, err := c.Install(context.Background(), r, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(replaced) != len(Programs()) {
		t.Errorf("replaced %v", replaced)
	}
	for _, p := range Programs() {
		if b, _ := os.ReadFile(filepath.Join(dir, Exe(p))); string(b) != "new "+p {
			t.Errorf("%s = %q", p, b)
		}
		if b, _ := os.ReadFile(filepath.Join(dir, oldName(Exe(p), cur))); string(b) != "old "+p {
			t.Errorf("%s old copy = %q", p, b)
		}
	}
	Cleanup(dir)
	if left, _ := filepath.Glob(filepath.Join(dir, "*.old-*")); len(left) != 0 {
		t.Errorf("Cleanup left %v", left)
	}
}

func TestInstallRefusesBadDownloads(t *testing.T) {
	const cur = "v0.1.0-alpha.12"
	f := newFake(t)
	// Checksum of different bytes.
	name, archive := release(t, "v0.1.0-alpha.13", programsWith("new"))
	f.add("v0.1.0-alpha.13", true, map[string][]byte{name: archive, "checksums.txt": sums(name, []byte("something else"))})
	// A program missing from the archive.
	name2, archive2 := release(t, "v0.1.0-alpha.14", map[string]string{"README.md": "x"})
	f.add("v0.1.0-alpha.14", true, map[string][]byte{name2: archive2, "checksums.txt": sums(name2, archive2)})
	// No archive for this platform.
	f.add("v0.1.0-alpha.15", true, map[string][]byte{"checksums.txt": nil})

	dir := t.TempDir()
	for _, p := range Programs() {
		os.WriteFile(filepath.Join(dir, Exe(p)), []byte("old "+p), 0o755)
	}
	c := f.checker(cur, true)
	for i, tag := range []string{"v0.1.0-alpha.13", "v0.1.0-alpha.14", "v0.1.0-alpha.15"} {
		var r *Release
		for _, raw := range f.releases {
			if raw["tag_name"] == tag {
				r = &Release{Version: tag}
				for _, a := range raw["assets"].([]map[string]any) {
					r.Assets = append(r.Assets, Asset{Name: a["name"].(string), URL: a["browser_download_url"].(string)})
				}
			}
		}
		if _, err := c.Install(context.Background(), r, dir); err == nil {
			t.Errorf("case %d (%s): install succeeded", i, tag)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != len(Programs()) {
		t.Errorf("dir changed: %v", entries)
	}
	for _, p := range Programs() {
		if b, _ := os.ReadFile(filepath.Join(dir, Exe(p))); string(b) != "old "+p {
			t.Errorf("%s = %q", p, b)
		}
	}
}

func TestSwapRollsBack(t *testing.T) {
	names := []string{"a.exe", "b.exe"}
	dir := t.TempDir()
	files := map[string][]byte{}
	for _, n := range names {
		os.WriteFile(filepath.Join(dir, n), []byte("old "+n), 0o755)
		files[n] = []byte("new " + n)
	}
	// A non-empty directory where b's old copy must go makes its rename fail, after a's
	// swap has already happened.
	os.MkdirAll(filepath.Join(dir, oldName("b.exe", "v1.0.0"), "block"), 0o755)
	if _, err := swap(dir, names, files, "v1.0.0"); err == nil {
		t.Fatal("swap succeeded")
	}
	for _, n := range names {
		if b, _ := os.ReadFile(filepath.Join(dir, n)); string(b) != "old "+n {
			t.Errorf("%s = %q after rollback", n, b)
		}
		if _, err := os.Stat(filepath.Join(dir, n+".new")); err == nil {
			t.Errorf("%s.new left behind", n)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, oldName("a.exe", "v1.0.0"))); err == nil {
		t.Error("a's old copy left behind")
	}
}

func TestChecksumListing(t *testing.T) {
	h := strings.Repeat("ab", 32)
	got, err := checksum([]byte(h+" *mcp-broker_1.0.0_linux_amd64.tar.gz\n"), "mcp-broker_1.0.0_linux_amd64.tar.gz")
	if err != nil || got != h {
		t.Errorf("got %q, %v", got, err)
	}
	if _, err := checksum([]byte("short  x.zip\n"), "x.zip"); err == nil {
		t.Error("accepted a short hash")
	}
}
