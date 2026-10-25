// Package version holds build metadata, set at link time:
//
//	go build -ldflags "-X github.com/trustypangolin/mcp-broker/internal/version.Version=v0.1.0 ..."
package version

import "fmt"

var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String returns a one-line version description.
func String() string {
	return fmt.Sprintf("mcp-broker %s (commit %s, built %s)", Version, Commit, Date)
}
