// Package update checks for and installs newer cheep releases. It knows two
// install shapes: a Homebrew formula (delegate to `brew upgrade`) and a raw
// GitHub Release binary (download the archive, verify its checksum, and swap
// the running binary in place).
package update

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const (
	repo      = "TedHaley/cheep"
	tapName   = "TedHaley/homebrew-tap/cheep"
	apiLatest = "https://api.github.com/repos/" + repo + "/releases/latest"
)

// userAgent is required by the GitHub API — unauthenticated requests without
// one are rejected with 403.
var userAgent = "cheep-updater"

// Release is the slice of a GitHub release we care about.
type Release struct {
	Version string // the tag, e.g. "v0.2.0"
}

// Result describes a completed upgrade.
type Result struct {
	Via    string // "brew" or "binary"
	Output string // command output, for the brew path
}

// Latest returns the newest published (non-draft, non-prerelease) release.
func Latest(ctx context.Context) (Release, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", apiLatest, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Release{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("github api: http %d", resp.StatusCode)
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return Release{}, err
	}
	if body.TagName == "" {
		return Release{}, fmt.Errorf("github api: no tag_name in latest release")
	}
	return Release{Version: body.TagName}, nil
}

// IsNewer reports whether latest is a strictly newer version than current.
// A current version that isn't valid semver (e.g. "dev") is treated as older
// than any real release, so an upgrade is always offered for dev builds.
func IsNewer(current, latest string) bool {
	return compareVersions(current, latest) < 0
}

// Upgrade installs `to`. It delegates to Homebrew when cheep was installed that
// way; otherwise it self-replaces the running binary from the GitHub Release.
func Upgrade(ctx context.Context, to string) (Result, error) {
	exe, err := os.Executable()
	if err != nil {
		return Result{}, err
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	if viaHomebrew(exe) {
		out, err := brewUpgrade(ctx)
		return Result{Via: "brew", Output: out}, err
	}
	if err := selfReplace(ctx, to, exe); err != nil {
		return Result{}, err
	}
	return Result{Via: "binary"}, nil
}

// --- Homebrew ---------------------------------------------------------------

func viaHomebrew(exe string) bool {
	if strings.Contains(exe, "/Cellar/") || strings.Contains(exe, "/homebrew/") {
		return true
	}
	if out, err := exec.Command("brew", "--prefix").Output(); err == nil {
		if prefix := strings.TrimSpace(string(out)); prefix != "" {
			return isBrewPath(exe, prefix)
		}
	}
	return false
}

// isBrewPath reports whether exe lives under the Homebrew prefix.
func isBrewPath(exe, prefix string) bool {
	return exe == prefix || strings.HasPrefix(exe, prefix+string(os.PathSeparator))
}

func brewUpgrade(ctx context.Context) (string, error) {
	c := exec.CommandContext(ctx, "brew", "upgrade", tapName)
	out, err := c.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("brew upgrade: %w", err)
	}
	return string(out), nil
}

// --- Self-replace from a GitHub Release archive -----------------------------

func selfReplace(ctx context.Context, tag, exe string) error {
	archive := archiveName(runtime.GOOS, runtime.GOARCH)

	sums, err := download(ctx, releaseURL(tag, "checksums.txt"))
	if err != nil {
		return fmt.Errorf("fetch checksums: %w", err)
	}
	want, ok := pickChecksum(string(sums), archive)
	if !ok {
		return fmt.Errorf("no checksum for %s in this release", archive)
	}

	data, err := download(ctx, releaseURL(tag, archive))
	if err != nil {
		return fmt.Errorf("download %s: %w", archive, err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != want {
		return fmt.Errorf("checksum mismatch for %s (got %s, want %s)", archive, got, want)
	}

	bins, err := extractBinaries(data, "cheep", "cheep-claude-mcp")
	if err != nil {
		return fmt.Errorf("extract %s: %w", archive, err)
	}
	if bins["cheep"] == nil {
		return fmt.Errorf("archive %s did not contain the cheep binary", archive)
	}
	if err := replaceFile(exe, bins["cheep"]); err != nil {
		return err
	}
	// Replace the optional Claude bridge only if it already sits alongside cheep.
	if mcp := bins["cheep-claude-mcp"]; mcp != nil {
		sibling := filepath.Join(filepath.Dir(exe), "cheep-claude-mcp")
		if _, err := os.Stat(sibling); err == nil {
			_ = replaceFile(sibling, mcp)
		}
	}
	return nil
}

// archiveName mirrors GoReleaser's name_template: cheep_<os>_<arch>[.ext].
func archiveName(goos, goarch string) string {
	ext := "tar.gz"
	if goos == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("cheep_%s_%s.%s", goos, goarch, ext)
}

func releaseURL(tag, file string) string {
	return fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", repo, tag, file)
}

// pickChecksum finds the sha256 for file in a GoReleaser checksums.txt
// ("<hex>  <filename>" per line).
func pickChecksum(sums, file string) (string, bool) {
	for _, line := range strings.Split(sums, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == file {
			return fields[0], true
		}
	}
	return "", false
}

func download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 200<<20))
}

// extractBinaries pulls the named files (matched by base name) out of a
// gzip-compressed tarball.
func extractBinaries(targz []byte, names ...string) (map[string][]byte, error) {
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	gz, err := gzip.NewReader(strings.NewReader(string(targz)))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	out := make(map[string][]byte)
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		base := filepath.Base(h.Name)
		if want[base] {
			b, err := io.ReadAll(io.LimitReader(tr, 200<<20))
			if err != nil {
				return nil, err
			}
			out[base] = b
		}
	}
	return out, nil
}

// replaceFile atomically swaps target's contents for data. It writes a temp
// file in the same directory (so the rename stays on one filesystem) and
// renames over the target — which works even while the old binary is running,
// since the running process keeps its already-open inode.
func replaceFile(target string, data []byte) error {
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".cheep-upgrade-*")
	if err != nil {
		return fmt.Errorf("write to %s: %w (is it writable? a brew install upgrades via `brew upgrade`)", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmpName, target); err != nil {
		return fmt.Errorf("replace %s: %w", target, err)
	}
	return nil
}

// --- Version comparison -----------------------------------------------------

// compareVersions returns -1, 0, or 1 comparing two semver-ish tags. Unparseable
// versions sort before any parseable one (so "dev" < "v1.0.0").
func compareVersions(a, b string) int {
	pa, oka := parseVersion(a)
	pb, okb := parseVersion(b)
	switch {
	case !oka && !okb:
		return 0
	case !oka:
		return -1
	case !okb:
		return 1
	}
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// parseVersion parses "v1.2.3" (or "1.2.3", with an optional "-prerelease"
// suffix that is ignored) into major/minor/patch.
func parseVersion(v string) ([3]int, bool) {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}
