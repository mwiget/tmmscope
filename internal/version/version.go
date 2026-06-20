// Package version holds build metadata, injected at link time by the Makefile /
// goreleaser (-X flags). Defaults are for `go run` / `go build` without ldflags.
package version

import "fmt"

var (
	Version   = "dev"
	Commit    = "none"
	BuildDate = "unknown"
)

// String renders a one-line version banner.
func String() string {
	return fmt.Sprintf("tmmscope %s (commit %s, built %s)", Version, Commit, BuildDate)
}
