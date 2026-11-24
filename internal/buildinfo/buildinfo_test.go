package buildinfo

import (
	"runtime"
	"runtime/debug"
	"testing"
)

func TestInfoDefaultsToDev(t *testing.T) {
	i := Info()
	if i.Version != "dev" {
		t.Errorf("Version = %q, want dev (no release metadata in a development build)", i.Version)
	}
	if i.GoVersion != runtime.Version() || i.OS != runtime.GOOS || i.Arch != runtime.GOARCH {
		t.Errorf("platform = %s %s/%s, want %s %s/%s", i.GoVersion, i.OS, i.Arch, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	}
}

func TestInfoStringFormat(t *testing.T) {
	cases := []struct {
		name string
		in   Details
		want string
	}{
		{"release", Details{Version: "0.8.0-rc.0", Commit: "0123456789abcdef0123456789abcdef01234567", GoVersion: "go1.27.4", OS: "linux", Arch: "arm64"},
			"magnetico 0.8.0-rc.0 (commit 0123456789ab, go1.27.4, linux/arm64)"},
		{"dirty tree", Details{Version: "dev", Commit: "0123456789abcdef", Modified: true, GoVersion: "go1.27.4", OS: "linux", Arch: "amd64"},
			"magnetico dev (commit 0123456789ab+dirty, go1.27.4, linux/amd64)"},
		{"short commit is kept whole", Details{Version: "dev", Commit: "abc123", GoVersion: "go1.27.4", OS: "linux", Arch: "amd64"},
			"magnetico dev (commit abc123, go1.27.4, linux/amd64)"},
		{"no commit", Details{Version: "dev", GoVersion: "go1.27.4", OS: "linux", Arch: "amd64"},
			"magnetico dev (commit unknown, go1.27.4, linux/amd64)"},
	}
	for _, c := range cases {
		if got := c.in.String(); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestCommitFallsBackToVCSSetting(t *testing.T) {
	bi := &debug.BuildInfo{Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "fedcba9876543210fedcba9876543210fedcba98"},
		{Key: "vcs.modified", Value: "true"},
	}}
	d := resolve("0.8.0", "", bi)
	if d.Commit != "fedcba9876543210fedcba9876543210fedcba98" || !d.Modified {
		t.Fatalf("fallback = %q modified=%v, want the vcs.revision with the dirty flag", d.Commit, d.Modified)
	}
	if d.Version != "0.8.0" {
		t.Fatalf("version must come from the linker value, got %q", d.Version)
	}
}

func TestLinkerCommitWinsOverVCSSetting(t *testing.T) {
	bi := &debug.BuildInfo{Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "ffffffffffffffffffffffffffffffffffffffff"},
		{Key: "vcs.modified", Value: "true"},
	}}
	d := resolve("0.8.0-rc.0", "0123456789abcdef0123456789abcdef01234567", bi)
	if d.Commit != "0123456789abcdef0123456789abcdef01234567" || d.Modified {
		t.Fatalf("linker commit must win and carry no dirty flag, got %q modified=%v", d.Commit, d.Modified)
	}
}

func TestMissingBuildInfoIsUnknown(t *testing.T) {
	d := resolve("dev", "", nil)
	if d.Commit != "" {
		t.Fatalf("commit = %q, want empty", d.Commit)
	}
}
