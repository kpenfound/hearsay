// Package version reports the build identity of the hearsay binary.
package version

import (
	"fmt"
	"runtime/debug"
	"sync"
)

var (
	version = ""
	commit  = ""
	date    = ""
)

// BuildInfo describes the binary that is running.
type BuildInfo struct {
	Version string
	Commit  string
	Date    string
}

// String renders the build info as one line.
func (b BuildInfo) String() string {
	return fmt.Sprintf("hearsay %s (commit %s, built %s)", b.Version, b.Commit, b.Date)
}

var info = sync.OnceValue(func() BuildInfo {
	b := BuildInfo{Version: version, Commit: commit, Date: date}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if b.Commit == "" {
					b.Commit = s.Value
				}
			case "vcs.time":
				if b.Date == "" {
					b.Date = s.Value
				}
			}
		}
		if b.Version == "" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			b.Version = bi.Main.Version
		}
	}
	if b.Version == "" {
		b.Version = "devel"
	}
	if b.Commit == "" {
		b.Commit = "unknown"
	}
	if b.Date == "" {
		b.Date = "unknown"
	}
	return b
})

// Info returns the build identity of this binary.
func Info() BuildInfo { return info() }
