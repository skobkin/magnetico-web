// Package buildinfo carries the identity of a build: application version, Git
// commit, Go toolchain and target platform. The same two linker values feed
// every kind of build (local, OCI image, native release archive):
//
//	-ldflags "-X github.com/skobkin/magnetico-web/internal/buildinfo.Version=<version>
//	          -X github.com/skobkin/magnetico-web/internal/buildinfo.Commit=<full commit>"
//
// A development build needs neither: the version is "dev" and the commit is read
// from the VCS stamp Go embeds when building inside a Git checkout.
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Version and Commit are set by the linker (see the package comment). They are
// variables, not constants, only because -X cannot set constants.
var (
	Version = "dev"
	Commit  = ""
)

// shortCommit is the number of commit characters printed.
const shortCommit = 12

// Details is the resolved identity of the running executable.
type Details struct {
	Version   string
	Commit    string // full commit when known, empty otherwise
	Modified  bool   // the working tree was dirty (VCS fallback only)
	GoVersion string
	OS        string
	Arch      string
}

// Info resolves the identity of the running executable.
func Info() Details {
	bi, _ := debug.ReadBuildInfo()
	return resolve(Version, Commit, bi)
}

// resolve combines the linker values with the embedded build information. The
// linker commit wins; the VCS stamp is only a fallback for development builds.
func resolve(version, commit string, bi *debug.BuildInfo) Details {
	d := Details{Version: version, Commit: commit, GoVersion: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH}
	if d.Commit == "" && bi != nil {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				d.Commit = s.Value
			case "vcs.modified":
				d.Modified = s.Value == "true"
			}
		}
		if d.Commit == "" {
			d.Modified = false
		}
	}
	return d
}

// String renders the one-line form printed by `magnetico --version`:
//
//	magnetico 0.8.0-rc.0 (commit 0123456789ab, go1.27.4, linux/arm64)
func (d Details) String() string {
	commit := "unknown"
	if d.Commit != "" {
		commit = d.Commit
		if len(commit) > shortCommit {
			commit = commit[:shortCommit]
		}
		if d.Modified {
			commit += "+dirty"
		}
	}
	return fmt.Sprintf("magnetico %s (commit %s, %s, %s/%s)", d.Version, commit, d.GoVersion, d.OS, d.Arch)
}
