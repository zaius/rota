// Package version reports the build's version.
package version

import "runtime/debug"

// The build sets Version with
//
//	-ldflags "-X github.com/alpkeskin/rota/core/internal/version.Version=v1.2.3"
//
// Release images and binaries pass the git tag. Builds without it report the
// VCS revision Go embedded, or "dev" when there is none (e.g. a Docker build,
// whose context excludes .git).
var Version = "dev"

func init() {
	if Version == "dev" {
		if rev := vcsRevision(); rev != "" {
			Version = rev
		}
	}
}

// vcsRevision returns the short commit hash Go stamped into the binary, with a
// "-dirty" suffix for uncommitted changes, or "" without VCS information.
func vcsRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	var rev string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if rev != "" && dirty {
		rev += "-dirty"
	}
	return rev
}
