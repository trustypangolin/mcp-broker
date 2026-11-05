// Package update finds newer mcp-broker releases on GitHub and installs their
// executables next to the running ones.
//
// Installing swaps files by renaming: Windows lets a running exe be renamed but not
// overwritten, so each current exe becomes <name>.old-<version>.exe and the new one takes
// its name. The old files go at the next start (Cleanup), once nothing runs them.
//
// The archive is checked against the release's checksums.txt. That catches a corrupt or
// truncated download; it is not a signature, so it trusts whoever can publish releases on
// the repository.
package update

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Repo is where releases are published.
const Repo = "trustypangolin/mcp-broker"

// DefaultAPI is the GitHub API base for Repo.
const DefaultAPI = "https://api.github.com/repos/" + Repo

const (
	maxArchive   = 200 << 20
	maxChecksums = 1 << 20
	maxListing   = 4 << 20
)

// Release is a published release.
type Release struct {
	Version    string  `json:"version"` // the tag, e.g. v0.1.0-alpha.13
	URL        string  `json:"url"`     // its release page
	Prerelease bool    `json:"prerelease"`
	Assets     []Asset `json:"-"`
}

// Asset is a file attached to a release.
type Asset struct {
	Name string
	URL  string
	Size int64
}

// Checker looks for releases newer than the running version.
type Checker struct {
	Current     string       // the running version
	Prereleases bool         // offer pre-releases too
	API         string       // GitHub API base; DefaultAPI when empty
	Client      *http.Client // http.DefaultClient when nil

	mu      sync.Mutex
	latest  *Release // newer than Current, from the last successful check
	checked time.Time
	lastErr error
}

// NewChecker returns a checker for the running version, or nil when that version can't be
// compared (a "dev" build), since such a build can't tell whether a release is newer.
func NewChecker(current string, prereleases bool) *Checker {
	if _, err := parse(current); err != nil {
		return nil
	}
	return &Checker{Current: current, Prereleases: prereleases}
}

func (c *Checker) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return http.DefaultClient
}

// EnvAPI overrides DefaultAPI, for testing against a local fake of the releases API.
const EnvAPI = "MCP_BROKER_UPDATE_API"

func (c *Checker) api() string {
	if c.API != "" {
		return strings.TrimRight(c.API, "/")
	}
	if v := os.Getenv(EnvAPI); v != "" {
		return strings.TrimRight(v, "/")
	}
	return DefaultAPI
}

// Check asks GitHub for the newest release and returns it if it is newer than Current
// (nil when up to date).
func (c *Checker) Check(ctx context.Context) (*Release, error) {
	r, err := c.newest(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checked, c.lastErr = time.Now(), err
	if err != nil {
		return nil, err
	}
	c.latest = r
	return r, nil
}

// Available returns the newer release the last successful check found, or nil.
func (c *Checker) Available() *Release {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.latest
}

// Run checks after delay and then every interval until ctx ends, calling found the first
// time each newer release is seen.
func (c *Checker) Run(ctx context.Context, delay, interval time.Duration, found func(*Release)) {
	seen := ""
	t := time.NewTimer(delay)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if r, err := c.Check(ctx); err == nil && r != nil && r.Version != seen {
			seen = r.Version
			found(r)
		}
		t.Reset(interval)
	}
}

func (c *Checker) newest(ctx context.Context) (*Release, error) {
	cur, err := parse(c.Current)
	if err != nil {
		return nil, err
	}
	body, err := c.get(ctx, c.api()+"/releases?per_page=30", maxListing, "application/vnd.github+json")
	if err != nil {
		return nil, err
	}
	var list []struct {
		TagName    string `json:"tag_name"`
		HTMLURL    string `json:"html_url"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
		Assets     []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
			Size int64  `json:"size"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("reading the release list: %w", err)
	}
	var best *Release
	var bestV version
	for _, r := range list {
		if r.Draft || (r.Prerelease && !c.Prereleases) {
			continue
		}
		v, err := parse(r.TagName)
		if err != nil || v.commits > 0 || compare(v, cur) <= 0 {
			continue
		}
		if best != nil && compare(v, bestV) <= 0 {
			continue
		}
		rel := &Release{Version: r.TagName, URL: r.HTMLURL, Prerelease: r.Prerelease}
		for _, a := range r.Assets {
			rel.Assets = append(rel.Assets, Asset{Name: a.Name, URL: a.URL, Size: a.Size})
		}
		best, bestV = rel, v
	}
	return best, nil
}

func (c *Checker) get(ctx context.Context, url string, limit int64, accept string) ([]byte, error) {
	if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://127.0.0.1") {
		return nil, fmt.Errorf("refusing a non-https download: %s", url)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	req.Header.Set("User-Agent", "mcp-broker/"+c.Current)
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("GET %s: %s %s", url, resp.Status, strings.TrimSpace(string(msg)))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("GET %s: larger than %d bytes", url, limit)
	}
	return data, nil
}

// ---- install ----

// Exe is an executable's file name on this OS: "mcp-broker" → "mcp-broker.exe" on Windows.
func Exe(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// Programs are the executables a release ships (the tray only on Windows).
func Programs() []string {
	if runtime.GOOS == "windows" {
		return []string{"mcp-broker", "mcp-broker-tray"}
	}
	return []string{"mcp-broker"}
}

// ArchiveName is the release archive for an OS and architecture.
func ArchiveName(tag, goos, goarch string) string {
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	return fmt.Sprintf("mcp-broker_%s_%s_%s%s", strings.TrimPrefix(tag, "v"), goos, goarch, ext)
}

// Install downloads r's archive for this platform, checks it, and replaces the programs
// found in dir with the release's. It returns the files it replaced. On any failure
// nothing in dir changes.
func (c *Checker) Install(ctx context.Context, r *Release, dir string) ([]string, error) {
	name := ArchiveName(r.Version, runtime.GOOS, runtime.GOARCH)
	archive, sums := find(r.Assets, name), find(r.Assets, "checksums.txt")
	if archive == nil {
		return nil, fmt.Errorf("release %s has no %s", r.Version, name)
	}
	if sums == nil {
		return nil, fmt.Errorf("release %s has no checksums.txt", r.Version)
	}
	sumData, err := c.get(ctx, sums.URL, maxChecksums, "")
	if err != nil {
		return nil, err
	}
	want, err := checksum(sumData, name)
	if err != nil {
		return nil, err
	}
	data, err := c.get(ctx, archive.URL, maxArchive, "")
	if err != nil {
		return nil, err
	}
	got := sha256.Sum256(data)
	if hex.EncodeToString(got[:]) != want {
		return nil, fmt.Errorf("%s: checksum mismatch (download corrupt?)", name)
	}
	files, err := extract(data, name)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, p := range Programs() {
		names = append(names, Exe(p))
	}
	return swap(dir, names, files, c.Current)
}

func find(as []Asset, name string) *Asset {
	for i := range as {
		if as[i].Name == name {
			return &as[i]
		}
	}
	return nil
}

// checksum finds name's sha256 in a sha256sum listing.
func checksum(listing []byte, name string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(listing))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			if len(f[0]) != 64 {
				break
			}
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("checksums.txt has no entry for %s", name)
}

// extract returns the programs' contents from the archive, keyed by file name. Entries
// are matched by base name only, so a crafted path can't point outside dir.
func extract(data []byte, archiveName string) (map[string][]byte, error) {
	want := map[string]bool{}
	for _, p := range Programs() {
		want[Exe(p)] = true
	}
	out := map[string][]byte{}
	take := func(name string, r io.Reader, size int64) error {
		base := path.Base(strings.ReplaceAll(name, `\`, "/"))
		if !want[base] {
			return nil
		}
		if size > maxArchive {
			return fmt.Errorf("%s: too large", base)
		}
		b, err := io.ReadAll(io.LimitReader(r, maxArchive+1))
		if err != nil {
			return err
		}
		out[base] = b
		return nil
	}
	if strings.HasSuffix(archiveName, ".zip") {
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			if f.FileInfo().IsDir() {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			err = take(f.Name, rc, int64(f.UncompressedSize64))
			rc.Close()
			if err != nil {
				return nil, err
			}
		}
	} else {
		gz, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		tr := tar.NewReader(gz)
		for {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, err
			}
			if h.Typeflag == tar.TypeReg {
				if err := take(h.Name, tr, h.Size); err != nil {
					return nil, err
				}
			}
		}
	}
	for n := range want {
		if len(out[n]) == 0 {
			return nil, fmt.Errorf("%s is missing from the archive", n)
		}
	}
	return out, nil
}

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// oldName is where a replaced program goes: mcp-broker.exe → mcp-broker.old-v0.1.0.exe.
func oldName(file, from string) string {
	ext := filepath.Ext(file)
	if ext != ".exe" {
		ext = ""
	}
	return strings.TrimSuffix(file, ext) + ".old-" + unsafeChars.ReplaceAllString(from, "_") + ext
}

// swap puts files into dir in place of the named programs already there. Ones dir doesn't
// have are left out, so a CLI-only install doesn't grow a tray. Every step is undone if a
// later one fails.
func swap(dir string, names []string, files map[string][]byte, from string) ([]string, error) {
	type step struct{ cur, old, tmp string }
	var steps []step
	for _, name := range names {
		cur := filepath.Join(dir, name)
		if _, err := os.Stat(cur); err != nil {
			continue
		}
		steps = append(steps, step{cur: cur, old: filepath.Join(dir, oldName(name, from)), tmp: filepath.Join(dir, name+".new")})
	}
	if len(steps) == 0 {
		return nil, fmt.Errorf("no mcp-broker programs in %s", dir)
	}
	cleanTmp := func() {
		for _, s := range steps {
			os.Remove(s.tmp)
		}
	}
	for _, s := range steps {
		if err := os.WriteFile(s.tmp, files[filepath.Base(s.cur)], 0o755); err != nil {
			cleanTmp()
			return nil, err
		}
	}
	var done []step
	undo := func() {
		for i := len(done) - 1; i >= 0; i-- {
			s := done[i]
			os.Rename(s.cur, s.tmp)
			os.Rename(s.old, s.cur)
		}
		cleanTmp()
	}
	for _, s := range steps {
		os.Remove(s.old) // a leftover from installing this same version before
		if err := os.Rename(s.cur, s.old); err != nil {
			undo()
			return nil, err
		}
		if err := os.Rename(s.tmp, s.cur); err != nil {
			os.Rename(s.old, s.cur)
			undo()
			return nil, err
		}
		done = append(done, s)
	}
	var out []string
	for _, s := range steps {
		out = append(out, s.cur)
	}
	return out, nil
}

// Cleanup removes replaced programs (<name>.old-*) from dir. Ones still running can't be
// removed on Windows and stay until a later start.
func Cleanup(dir string) {
	for _, p := range Programs() {
		matches, _ := filepath.Glob(filepath.Join(dir, p+".old-*"))
		for _, m := range matches {
			os.Remove(m)
		}
	}
}

// ---- versions ----

// version is a semver tag, plus the commits since it when it comes from git describe
// (v0.1.0-alpha.12-3-gabc1234).
type version struct {
	core    [3]int
	pre     []string
	commits int
}

var describe = regexp.MustCompile(`-(\d+)-g[0-9a-f]+(-dirty)?$`)

func parse(s string) (version, error) {
	var v version
	t := strings.TrimPrefix(strings.TrimSpace(s), "v")
	if m := describe.FindStringSubmatch(t); m != nil {
		v.commits, _ = strconv.Atoi(m[1])
		t = strings.TrimSuffix(t, m[0])
	}
	t, _, _ = strings.Cut(t, "+")
	core, pre, hasPre := strings.Cut(t, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return v, fmt.Errorf("not a release version: %q", s)
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return v, fmt.Errorf("not a release version: %q", s)
		}
		v.core[i] = n
	}
	if hasPre {
		if pre == "" {
			return v, fmt.Errorf("not a release version: %q", s)
		}
		v.pre = strings.Split(pre, ".")
	}
	return v, nil
}

// compare orders versions by semver precedence; a git describe build comes after its tag.
func compare(a, b version) int {
	for i := range a.core {
		if a.core[i] != b.core[i] {
			return cmpInt(a.core[i], b.core[i])
		}
	}
	if c := comparePre(a.pre, b.pre); c != 0 {
		return c
	}
	return cmpInt(a.commits, b.commits)
}

func comparePre(a, b []string) int {
	switch {
	case len(a) == 0 && len(b) == 0:
		return 0
	case len(a) == 0:
		return 1 // a release outranks its pre-releases
	case len(b) == 0:
		return -1
	}
	for i := 0; i < len(a) && i < len(b); i++ {
		an, aErr := strconv.Atoi(a[i])
		bn, bErr := strconv.Atoi(b[i])
		switch {
		case aErr == nil && bErr == nil:
			if an != bn {
				return cmpInt(an, bn)
			}
		case aErr == nil:
			return -1 // numeric identifiers sort before alphanumeric ones
		case bErr == nil:
			return 1
		default:
			if c := strings.Compare(a[i], b[i]); c != 0 {
				return c
			}
		}
	}
	return cmpInt(len(a), len(b))
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Newer reports whether version a is newer than b. Unparseable versions are never newer.
func Newer(a, b string) bool {
	va, err1 := parse(a)
	vb, err2 := parse(b)
	return err1 == nil && err2 == nil && compare(va, vb) > 0
}
