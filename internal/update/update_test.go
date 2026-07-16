package update

import "testing"

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"v1.0.0", "v1.0.1", -1},
		{"v1.0.1", "v1.0.0", 1},
		{"v1.2.0", "v1.2.0", 0},
		{"1.2.0", "v1.2.0", 0}, // leading v optional
		{"v1.2.3", "v1.10.0", -1},
		{"v2.0.0", "v1.9.9", 1},
		{"v1.2.3-rc1", "v1.2.3", 0},   // prerelease suffix ignored
		{"dev", "v1.0.0", -1},         // unparseable current sorts first
		{"v1.0.0", "dev", 1},          // and any real release beats it
		{"dev", "also-not-semver", 0}, // both unparseable
	}
	for _, tc := range tests {
		if got := compareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestIsNewer(t *testing.T) {
	if !IsNewer("v0.1.0", "v0.2.0") {
		t.Error("v0.2.0 should be newer than v0.1.0")
	}
	if IsNewer("v0.2.0", "v0.2.0") {
		t.Error("same version is not newer")
	}
	if IsNewer("v0.3.0", "v0.2.0") {
		t.Error("older release is not newer")
	}
	if !IsNewer("dev", "v0.1.0") {
		t.Error("a dev build should always see a real release as newer")
	}
}

func TestArchiveName(t *testing.T) {
	if got := archiveName("darwin", "arm64"); got != "cheep_darwin_arm64.tar.gz" {
		t.Errorf("darwin/arm64 archive = %q", got)
	}
	if got := archiveName("linux", "amd64"); got != "cheep_linux_amd64.tar.gz" {
		t.Errorf("linux/amd64 archive = %q", got)
	}
	if got := archiveName("windows", "amd64"); got != "cheep_windows_amd64.zip" {
		t.Errorf("windows archive = %q", got)
	}
}

func TestPickChecksum(t *testing.T) {
	sums := "abc123  cheep_linux_amd64.tar.gz\n" +
		"def456  cheep_darwin_arm64.tar.gz\n" +
		"ghi789  checksums.txt\n"
	if got, ok := pickChecksum(sums, "cheep_darwin_arm64.tar.gz"); !ok || got != "def456" {
		t.Errorf("pickChecksum darwin = %q, %v", got, ok)
	}
	if _, ok := pickChecksum(sums, "cheep_windows_arm64.zip"); ok {
		t.Error("expected miss for an absent archive")
	}
}

func TestIsBrewPath(t *testing.T) {
	if !isBrewPath("/opt/homebrew/bin/cheep", "/opt/homebrew") {
		t.Error("binary under the brew prefix should be detected")
	}
	if isBrewPath("/usr/local/bin/cheep", "/opt/homebrew") {
		t.Error("binary outside the brew prefix should not match")
	}
	if isBrewPath("/opt/homebrew-extra/bin/cheep", "/opt/homebrew") {
		t.Error("prefix match must be path-segment aware, not a substring")
	}
}
