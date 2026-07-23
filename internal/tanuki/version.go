package tanuki

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Build metadata, injected at link time via -ldflags "-X ...tanuki.version=…".
// Defaults are what an unstamped local `go build` reports.
var (
	version   = "dev"
	commit    = ""
	buildDate = "unknown"
)

// versionString returns the release version, falling back to the VCS revision
// the go tool embeds when the binary was not stamped by CI.
func versionString() string {
	if version != "dev" {
		return version
	}

	if rev := vcsRevision(); rev != "" {
		return "dev+" + rev
	}

	return version
}

func vcsRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}

	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && len(s.Value) >= 7 {
			return s.Value[:7]
		}
	}

	return ""
}

func commitString() string {
	if commit != "" {
		return commit
	}

	if rev := vcsRevision(); rev != "" {
		return rev
	}

	return "unknown"
}

func cmdVersion() {
	fmt.Printf("tanuki %s\n", versionString())
	fmt.Printf("  commit:  %s\n", commitString())
	fmt.Printf("  built:   %s\n", buildDate)
	fmt.Printf("  go:      %s\n", runtime.Version())
	fmt.Printf("  platform: %s/%s\n", runtime.GOOS, runtime.GOARCH)
}
