package cmd

import (
	"runtime/debug"
	"testing"
)

// stubBuildInfo answers as the linked-in build info would. A nil info is
// the absent case, which is what a binary built without module
// information gives.
func stubBuildInfo(info *debug.BuildInfo) func() (*debug.BuildInfo, bool) {
	return func() (*debug.BuildInfo, bool) {
		return info, info != nil
	}
}

func mainVersion(v string) *debug.BuildInfo {
	return &debug.BuildInfo{Main: debug.Module{Version: v}}
}

func TestResolveVersion(t *testing.T) {
	tests := []struct {
		name      string
		embedded  string
		buildInfo func() (*debug.BuildInfo, bool)
		want      string
	}{
		{
			name:     "the embedded value wins",
			embedded: "1.2.3",
			// Never reached, so anything it answers would be wrong to report.
			buildInfo: stubBuildInfo(mainVersion("v9.9.9")),
			want:      "1.2.3",
		},
		{
			name:      "the embedded value keeps no v prefix",
			embedded:  "v1.2.3",
			buildInfo: stubBuildInfo(nil),
			want:      "1.2.3",
		},
		{
			name:      "go install falls back to the module version",
			buildInfo: stubBuildInfo(mainVersion("v1.2.3")),
			want:      "1.2.3",
		},
		{
			name:      "go build falls back to what it was stamped with",
			buildInfo: stubBuildInfo(mainVersion("(devel)")),
			want:      "(devel)",
		},
		{
			name:      "build info carrying no version reports unknown",
			buildInfo: stubBuildInfo(mainVersion("")),
			want:      "unknown",
		},
		{
			name:      "absent build info reports unknown",
			buildInfo: stubBuildInfo(nil),
			want:      "unknown",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveVersion(tt.embedded, tt.buildInfo); got != tt.want {
				t.Errorf("resolveVersion(%q) = %q, want %q", tt.embedded, got, tt.want)
			}
		})
	}
}
