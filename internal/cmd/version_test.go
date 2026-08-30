package cmd

import "testing"

func TestResolveVersion(t *testing.T) {
	tests := []struct {
		name     string
		embedded string
		module   string
		want     string
	}{
		{
			name:     "the embedded value wins",
			embedded: "1.2.3",
			// Never reached, so reporting this would be wrong.
			module: "v9.9.9",
			want:   "1.2.3",
		},
		{
			name:     "the embedded value keeps no v prefix",
			embedded: "v1.2.3",
			want:     "1.2.3",
		},
		{
			name:   "go install falls back to the module version",
			module: "v1.2.3",
			want:   "1.2.3",
		},
		{
			name:   "go build falls back to what it was stamped with",
			module: "(devel)",
			want:   "(devel)",
		},
		{
			name: "neither source has a version",
			want: "unknown",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveVersion(tt.embedded, tt.module); got != tt.want {
				t.Errorf("resolveVersion(%q, %q) = %q, want %q", tt.embedded, tt.module, got, tt.want)
			}
		})
	}
}
