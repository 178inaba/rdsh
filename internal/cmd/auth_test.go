package cmd

import (
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/178inaba/rdsh/internal/config"
)

// runRdshIn is runRdsh with an explicit config dir so tests can seed or
// inspect the config file. srv == nil leaves the env pair unset.
func runRdshIn(t *testing.T, configDir string, srv *httptest.Server, stdin string, args ...string) (string, error) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", configDir)
	if srv != nil {
		t.Setenv("RDSH_URL", srv.URL)
		t.Setenv("RDSH_API_KEY", "test-key")
	} else {
		t.Setenv("RDSH_URL", "")
		t.Setenv("RDSH_API_KEY", "")
		os.Unsetenv("RDSH_URL")
		os.Unsetenv("RDSH_API_KEY")
	}
	return runRdshWithEnvSet(t, stdin, args...)
}

func TestAuthLoginSavesVerifiedProfile(t *testing.T) {
	f := &fakeServer{}
	srv := f.start(t)
	dir := t.TempDir()

	// Prompts answered in order: URL, API key, profile name, default data source.
	stdin := fmt.Sprintf("%s\nsecret-key\nstaging\n3\n", srv.URL)
	out, err := runRdshIn(t, dir, nil, stdin, "auth", "login")
	if err != nil {
		t.Fatalf("auth login error = %v (output: %s)", err, out)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	p, ok := cfg.Profiles["staging"]
	if !ok {
		t.Fatalf("profile staging not saved: %+v", cfg)
	}
	if p.URL != srv.URL || p.APIKey != "secret-key" || p.DataSource != "3" {
		t.Errorf("saved profile = %+v", p)
	}
	if cfg.ActiveProfile != "staging" {
		t.Errorf("first profile should become active, got %q", cfg.ActiveProfile)
	}
}

func TestAuthLoginDefaultsProfileNameAndDataSourceOptional(t *testing.T) {
	f := &fakeServer{}
	srv := f.start(t)
	dir := t.TempDir()

	stdin := fmt.Sprintf("%s\nsecret-key\n\n\n", srv.URL)
	if _, err := runRdshIn(t, dir, nil, stdin, "auth", "login"); err != nil {
		t.Fatalf("auth login error = %v", err)
	}

	cfg, _ := config.Load()
	p, ok := cfg.Profiles["default"]
	if !ok {
		t.Fatalf("profile default not saved: %+v", cfg)
	}
	if p.DataSource != "" {
		t.Errorf("DataSource = %q, want empty", p.DataSource)
	}
}

func TestAuthLoginRejectedCredentialsSaveNothing(t *testing.T) {
	f := &fakeServer{rejectSession: true}
	srv := f.start(t)
	dir := t.TempDir()

	stdin := fmt.Sprintf("%s\nbad-key\nstaging\n", srv.URL)
	_, err := runRdshIn(t, dir, nil, stdin, "auth", "login")
	if err == nil {
		t.Fatal("auth login with rejected credentials: want error")
	}

	path, _ := config.Path()
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("config file should not exist after failed login, stat err = %v", statErr)
	}
}

func TestProfileUseAndList(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	seed := &config.Config{
		ActiveProfile: "dev",
		Profiles: map[string]config.Profile{
			"dev":  {URL: "https://dev.example.com", APIKey: "k1"},
			"prod": {URL: "https://prod.example.com", APIKey: "k2"},
		},
	}
	if err := seed.Save(); err != nil {
		t.Fatal(err)
	}

	out, err := runRdshIn(t, dir, nil, "", "profile", "list")
	if err != nil {
		t.Fatalf("profile list error = %v", err)
	}
	if !strings.Contains(out, "* dev") || !strings.Contains(out, "prod") {
		t.Errorf("profile list output = %q, want active marker on dev", out)
	}

	if _, err := runRdshIn(t, dir, nil, "", "profile", "use", "prod"); err != nil {
		t.Fatalf("profile use error = %v", err)
	}
	cfg, _ := config.Load()
	if cfg.ActiveProfile != "prod" {
		t.Errorf("ActiveProfile = %q, want prod", cfg.ActiveProfile)
	}
}

func TestProfileUseUnknown(t *testing.T) {
	dir := t.TempDir()
	_, err := runRdshIn(t, dir, nil, "", "profile", "use", "nope")
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("error = %v, want unknown-profile error naming it", err)
	}
}

func TestDataSourceList(t *testing.T) {
	f := &fakeServer{}
	srv := f.start(t)

	out, err := runRdshIn(t, t.TempDir(), srv, "", "data-source", "list")
	if err != nil {
		t.Fatalf("data-source list error = %v", err)
	}
	if want := "7\twarehouse\n"; out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
}
