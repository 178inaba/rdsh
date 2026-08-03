package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/178inaba/rdsh/internal/config"
)

func setConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	return dir
}

// clearEnvPair removes the env pair: t.Setenv first so the original value
// is restored on cleanup, then Unsetenv because empty and unset are
// different for the atomic-pair validation.
func clearEnvPair(t *testing.T) {
	t.Helper()
	t.Setenv("RDSH_URL", "")
	t.Setenv("RDSH_API_KEY", "")
	os.Unsetenv("RDSH_URL")
	os.Unsetenv("RDSH_API_KEY")
}

func TestLoadReturnsEmptyConfigWhenFileMissing(t *testing.T) {
	setConfigDir(t)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.ActiveProfile != "" || len(cfg.Profiles) != 0 {
		t.Errorf("Load() = %+v, want empty config", cfg)
	}
}

func TestSaveLoadRoundtrip(t *testing.T) {
	setConfigDir(t)

	want := &config.Config{
		ActiveProfile: "default",
		Profiles: map[string]config.Profile{
			"default": {URL: "https://redash.example.com", APIKey: "secret", DataSource: "3"},
		},
	}
	if err := want.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Load() = %+v, want %+v", got, want)
	}
}

func TestSaveSetsFileAndDirPermissions(t *testing.T) {
	setConfigDir(t)

	cfg := &config.Config{Profiles: map[string]config.Profile{"p": {URL: "u", APIKey: "k"}}}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	path, err := config.Path()
	if err != nil {
		t.Fatalf("Path() error = %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%s) error = %v", path, err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("config file permissions = %o, want 600", perm)
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("Stat(dir) error = %v", err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("config dir permissions = %o, want 700", perm)
	}
}

func TestSaveEnforcesPermissionsOnExistingFile(t *testing.T) {
	setConfigDir(t)

	path, err := config.Path()
	if err != nil {
		t.Fatalf("Path() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("profiles: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Profiles: map[string]config.Profile{"p": {URL: "u", APIKey: "k"}}}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("config file permissions after overwrite = %o, want 600", perm)
	}
}

func TestDirFallsBackToHomeConfigWithoutXDG(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	os.Unsetenv("XDG_CONFIG_HOME")

	dir, err := config.Dir()
	if err != nil {
		t.Fatalf("Dir() error = %v", err)
	}
	if want := filepath.Join(home, ".config", "rdsh"); dir != want {
		t.Errorf("Dir() = %s, want %s", dir, want)
	}
}

func TestLoadAcceptsUnquotedIntDataSource(t *testing.T) {
	setConfigDir(t)

	path, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	yml := "active_profile: default\nprofiles:\n  default:\n    url: https://r.example.com\n    api_key: k\n    data_source: 3\n"
	if err := os.WriteFile(path, []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := cfg.Profiles["default"].DataSource; got != "3" {
		t.Errorf("DataSource = %q, want %q", got, "3")
	}
}

func TestResolve(t *testing.T) {
	base := &config.Config{
		ActiveProfile: "dev",
		Profiles: map[string]config.Profile{
			"dev":  {URL: "https://dev.example.com", APIKey: "dev-key", DataSource: "1"},
			"prod": {URL: "https://prod.example.com", APIKey: "prod-key"},
		},
	}

	tests := []struct {
		name        string
		cfg         *config.Config
		profileFlag string
		envURL      string
		envKey      string
		want        config.Connection
		wantErr     string
	}{
		{
			name: "active profile by default",
			cfg:  base,
			want: config.Connection{URL: "https://dev.example.com", APIKey: "dev-key", DataSource: "1"},
		},
		{
			name:        "profile flag wins over env pair",
			cfg:         base,
			profileFlag: "prod",
			envURL:      "https://env.example.com",
			envKey:      "env-key",
			want:        config.Connection{URL: "https://prod.example.com", APIKey: "prod-key"},
		},
		{
			name:   "env pair wins over active profile",
			cfg:    base,
			envURL: "https://env.example.com",
			envKey: "env-key",
			want:   config.Connection{URL: "https://env.example.com", APIKey: "env-key"},
		},
		{
			name:   "env pair works without any config",
			cfg:    &config.Config{},
			envURL: "https://env.example.com",
			envKey: "env-key",
			want:   config.Connection{URL: "https://env.example.com", APIKey: "env-key"},
		},
		{
			name:    "only RDSH_URL set is an error",
			cfg:     base,
			envURL:  "https://env.example.com",
			wantErr: "RDSH_API_KEY",
		},
		{
			name:    "only RDSH_API_KEY set is an error",
			cfg:     base,
			envKey:  "env-key",
			wantErr: "RDSH_URL",
		},
		{
			name:        "unknown profile flag is an error",
			cfg:         base,
			profileFlag: "nope",
			wantErr:     "nope",
		},
		{
			name:    "active profile pointing to a missing profile is an error",
			cfg:     &config.Config{ActiveProfile: "gone", Profiles: map[string]config.Profile{"dev": {URL: "u", APIKey: "k"}}},
			wantErr: "gone",
		},
		{
			name:    "no profile and no env is an error",
			cfg:     &config.Config{},
			wantErr: "rdsh auth login",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnvPair(t)
			if tt.envURL != "" {
				t.Setenv("RDSH_URL", tt.envURL)
			}
			if tt.envKey != "" {
				t.Setenv("RDSH_API_KEY", tt.envKey)
			}

			got, err := config.Resolve(tt.cfg, tt.profileFlag)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Resolve() = %+v, want error containing %q", got, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("Resolve() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("Resolve() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
