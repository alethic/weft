// Package version carries build information stamped in at link time.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

// Set with -ldflags "-X github.com/alethic/weft/internal/version.Version=...".
//
// The defaults are what a plain "go build" produces, and they are honest about
// it rather than claiming a release that was never cut.
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

// Info is the resolved build information.
type Info struct {
	Version   string
	Commit    string
	Date      string
	GoVersion string
	Platform  string
}

// Get resolves the build information, falling back to what the Go toolchain
// embedded when the linker flags were not supplied.
func Get() Info {
	i := Info{
		Version:   Version,
		Commit:    Commit,
		Date:      Date,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}

	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				if i.Commit == "" {
					i.Commit = s.Value
				}
			case "vcs.time":
				if i.Date == "" {
					i.Date = s.Value
				}
			case "vcs.modified":
				// git describe --dirty already says so; saying it twice reads
				// like a bug in the version string.
				if s.Value == "true" && !strings.HasSuffix(i.Version, "-dirty") {
					i.Version += "-dirty"
				}
			}
		}
	}
	return i
}

// String renders one line, as a --version flag would print it.
func (i Info) String() string {
	s := "weft " + i.Version
	if i.Commit != "" {
		short := i.Commit
		if len(short) > 12 {
			short = short[:12]
		}
		s += fmt.Sprintf(" (%s)", short)
	}
	if i.Date != "" {
		s += " built " + i.Date
	}
	return s + fmt.Sprintf(", %s %s", i.GoVersion, i.Platform)
}
