// Package config manages named connection profiles stored under
// $XDG_CONFIG_HOME/rdsh (default ~/.config/rdsh), the same location scheme
// gh uses. os.UserConfigDir is deliberately avoided: on darwin it points to
// ~/Library/Application Support instead.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

const (
	envURL    = "RDSH_URL"
	envAPIKey = "RDSH_API_KEY"

	fileName = "config.yml"
)

// Profile is one named Redash connection.
type Profile struct {
	URL        string `yaml:"url"`
	APIKey     string `yaml:"api_key"`
	DataSource string `yaml:"data_source,omitempty"`
}

// UnmarshalYAML accepts data_source written as an unquoted integer
// (e.g. `data_source: 3`); hand-editing the config file is a supported
// workflow and a plain string field would reject that.
func (p *Profile) UnmarshalYAML(node *yaml.Node) error {
	var raw struct {
		URL        string    `yaml:"url"`
		APIKey     string    `yaml:"api_key"`
		DataSource yaml.Node `yaml:"data_source"`
	}
	if err := node.Decode(&raw); err != nil {
		return err
	}
	p.URL = raw.URL
	p.APIKey = raw.APIKey
	switch raw.DataSource.Kind {
	case 0: // absent
	case yaml.ScalarNode:
		p.DataSource = raw.DataSource.Value
	default:
		return errors.New("data_source must be a scalar (ID or name)")
	}
	return nil
}

// Config is the on-disk configuration.
type Config struct {
	ActiveProfile string             `yaml:"active_profile,omitempty"`
	Profiles      map[string]Profile `yaml:"profiles,omitempty"`
}

// Connection is a resolved set of credentials for talking to one Redash
// instance. DataSource may be empty (the query command then requires
// --data-source).
type Connection struct {
	URL        string
	APIKey     string
	DataSource string
}

// Dir returns the rdsh config directory.
func Dir() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "rdsh"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "rdsh"), nil
}

// Path returns the config file path.
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, fileName), nil
}

// Load reads the config file. A missing file is not an error and yields an
// empty config, so first-run commands can give their own guidance.
func Load() (*Config, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &cfg, nil
}

// Save writes the config file with 0600 permissions (0700 for the
// directory), enforcing the mode even when the file already exists with a
// looser one.
func (c *Config) Save() error {
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	// os.WriteFile applies the mode only on creation; reassert it so a
	// pre-existing world-readable file does not keep leaking the key.
	return os.Chmod(path, 0o600)
}

// Resolve picks the connection to use. Precedence: the --profile flag
// (explicit, one invocation) > the RDSH_URL/RDSH_API_KEY pair > the profile
// set persistently by `profile use`. When --profile is given the environment
// is not consulted — an explicit flag must never lose silently to ambient
// state.
func Resolve(cfg *Config, profileFlag string) (Connection, error) {
	if profileFlag != "" {
		return fromProfile(cfg, profileFlag)
	}

	url, key := os.Getenv(envURL), os.Getenv(envAPIKey)
	switch {
	case url != "" && key != "":
		return Connection{URL: url, APIKey: key}, nil
	case url != "":
		return Connection{}, fmt.Errorf("%s is set but %s is not; the two must be set together", envURL, envAPIKey)
	case key != "":
		return Connection{}, fmt.Errorf("%s is set but %s is not; the two must be set together", envAPIKey, envURL)
	}

	if cfg.ActiveProfile == "" {
		return Connection{}, errors.New("no profile configured; run `rdsh auth login` or set RDSH_URL and RDSH_API_KEY")
	}
	return fromProfile(cfg, cfg.ActiveProfile)
}

func fromProfile(cfg *Config, name string) (Connection, error) {
	p, ok := cfg.Profiles[name]
	if !ok {
		return Connection{}, fmt.Errorf("profile %q not found; run `rdsh profile list` to see available profiles", name)
	}
	return Connection{URL: p.URL, APIKey: p.APIKey, DataSource: p.DataSource}, nil
}
