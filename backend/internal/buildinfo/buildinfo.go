// Package buildinfo exposes the version metadata baked into the binary.
//
// Release builds set Version, Commit and Date through linker flags, e.g.
//
//	go build -ldflags "-X github.com/kshitijv97/chat-analyzer/backend/internal/buildinfo.Version=v0.1.0"
//
// Development builds fall back to the VCS information that the Go toolchain
// embeds automatically (see runtime/debug.ReadBuildInfo), so `chatanalyzer
// version` is meaningful even without a release pipeline.
package buildinfo

import (
	"runtime"
	"runtime/debug"
	"sync"
)

// Linker-injected values. They are variables (not constants) so -ldflags -X can
// overwrite them; do not assign to them at runtime.
var (
	// Version is the semantic version of the release, or "dev".
	Version = "dev"
	// Commit is the full git commit SHA the binary was built from.
	Commit = ""
	// Date is the RFC 3339 build timestamp.
	Date = ""
)

// Info is the JSON-serialisable build description served by GET /version
// (NFR-OBS-004) and printed by `chatanalyzer version`.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	GoVersion string `json:"go_version"`
	Platform  string `json:"platform"`
	// Modified reports uncommitted changes in the working tree at build time
	// (only known for development builds).
	Modified bool `json:"modified"`
}

var (
	once   sync.Once
	cached Info
)

// Get returns the build information, resolving VCS fallbacks once.
func Get() Info {
	once.Do(func() {
		cached = Info{
			Version:   Version,
			Commit:    Commit,
			Date:      Date,
			GoVersion: runtime.Version(),
			Platform:  runtime.GOOS + "/" + runtime.GOARCH,
		}
		bi, ok := debug.ReadBuildInfo()
		if !ok {
			return
		}
		// Only fill gaps: explicit linker flags always win over VCS stamps.
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if cached.Commit == "" {
					cached.Commit = s.Value
				}
			case "vcs.time":
				if cached.Date == "" {
					cached.Date = s.Value
				}
			case "vcs.modified":
				cached.Modified = s.Value == "true"
			}
		}
	})
	return cached
}

// ShortCommit returns the first 12 characters of the commit SHA, or "unknown".
func (i Info) ShortCommit() string {
	switch {
	case i.Commit == "":
		return "unknown"
	case len(i.Commit) > 12:
		return i.Commit[:12]
	default:
		return i.Commit
	}
}
