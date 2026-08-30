package cmd

import (
	"runtime/debug"
	"strings"
)

// version is what GoReleaser writes into a release build, through
// -ldflags -X on this variable. It is empty in every other build. A
// package-level var is the only thing the linker can write to, which is
// why this one exists despite the no-package-level-state convention the
// commands themselves follow: nothing mutates it after link time.
var version string

// resolveVersion reports the version the root command prints, from the
// value the linker embedded and, failing that, the build information the
// toolchain recorded. GoReleaser's {{.Version}} carries no v prefix, so
// neither does what this returns — the fallback is stripped to match,
// since a module version does carry one.
//
// buildInfo is a parameter rather than a direct debug.ReadBuildInfo call
// so that each branch can be exercised: what the real one answers is a
// property of how the test binary itself was built.
func resolveVersion(embedded string, buildInfo func() (*debug.BuildInfo, bool)) string {
	if embedded != "" {
		return strings.TrimPrefix(embedded, "v")
	}
	// The module version for `go install …@vX.Y.Z`, and either (devel) or
	// a VCS-stamped pseudo-version for a local `go build`.
	if info, ok := buildInfo(); ok && info.Main.Version != "" {
		return strings.TrimPrefix(info.Main.Version, "v")
	}
	return "unknown"
}
