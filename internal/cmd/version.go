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

// moduleVersion reports the version the toolchain recorded: the module
// version for `go install …@vX.Y.Z`, and either (devel) or a VCS-stamped
// pseudo-version for a local `go build`.
func moduleVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	return info.Main.Version
}

// resolveVersion reports the version the root command prints, preferring
// what the linker embedded over what the toolchain recorded. GoReleaser's
// {{.Version}} carries no v prefix, so neither does what this returns —
// the fallback is stripped to match, since a module version does carry
// one.
//
// Both are parameters so this stays a pure function: what
// debug.ReadBuildInfo actually answers is a property of how the calling
// binary was built, which a test cannot choose.
func resolveVersion(embedded, module string) string {
	if embedded != "" {
		return strings.TrimPrefix(embedded, "v")
	}
	if module != "" {
		return strings.TrimPrefix(module, "v")
	}
	return "unknown"
}
