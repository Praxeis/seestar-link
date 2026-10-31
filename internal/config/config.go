// Package config stores named device profiles. Each profile remembers which ZWO account
// and which Seestar it uses, plus the proxy settings for that scope, so switching between
// scopes is a matter of naming the profile instead of exporting a set of variables.
//
// Passwords are never written here; they live in the secret store (internal/secret),
// keyed by account email.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

// AppName names the configuration directory and the service.
const AppName = "seestar-link"

// DefaultStatusAddr is where a proxy serves its status page when the profile sets none.
const DefaultStatusAddr = "127.0.0.1:4799"

// Profile is one named scope.
type Profile struct {
	Email  string   `json:"email"`
	Sn     string   `json:"sn"`
	Model  string   `json:"model,omitempty"`
	Alias  string   `json:"alias,omitempty"`  // the name shown in the Seestar app, for display
	Master string   `json:"master,omitempty"` // Kalay rendezvous override; empty uses the built-in
	Ports  []string `json:"ports,omitempty"`  // proxy mappings, "local:device"; empty means 4700:4700
	Listen string   `json:"listen,omitempty"` // proxy bind address; empty means 127.0.0.1
	Status string   `json:"status,omitempty"` // proxy status address; empty means DefaultStatusAddr
}

// StatusAddr returns the profile's status address, falling back to the default.
func (p Profile) StatusAddr() string {
	if p.Status != "" {
		return p.Status
	}
	return DefaultStatusAddr
}

// Label renders a profile for listings.
func (p Profile) Label() string {
	name := p.Alias
	if name == "" {
		name = p.Model
	}
	if name == "" {
		return p.Sn
	}
	return fmt.Sprintf("%s (%s)", name, p.Sn)
}

// Config is the whole file.
type Config struct {
	Default  string             `json:"default,omitempty"`
	Profiles map[string]Profile `json:"profiles"`

	dir string
}

var dirOverride string

// SetDir points the package at a specific configuration directory, for the -config-dir flag
// and for a Windows service, which runs as a different account than the one that set it up.
func SetDir(d string) { dirOverride = d }

// Dir returns the configuration directory: an explicit SetDir, then SEESTAR_CONFIG_DIR, then
// the platform's per-user config location.
func Dir() (string, error) {
	if dirOverride != "" {
		return dirOverride, nil
	}
	if d := os.Getenv("SEESTAR_CONFIG_DIR"); d != "" {
		return d, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find config directory: %w", err)
	}
	return filepath.Join(base, AppName), nil
}

// CustomDir returns the configuration directory when it was set explicitly (by SetDir or
// SEESTAR_CONFIG_DIR), so a service can be started with the same one.
func CustomDir() (string, bool) {
	if dirOverride != "" {
		return dirOverride, true
	}
	if d := os.Getenv("SEESTAR_CONFIG_DIR"); d != "" {
		return d, true
	}
	return "", false
}

// Path returns the config file path.
func Path() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "config.json"), nil
}

// Load reads the config file. A missing file is an empty config, not an error.
func Load() (*Config, error) {
	d, err := Dir()
	if err != nil {
		return nil, err
	}
	c := &Config{Profiles: map[string]Profile{}, dir: d}
	b, err := os.ReadFile(filepath.Join(d, "config.json"))
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(d, "config.json"), err)
	}
	if c.Profiles == nil {
		c.Profiles = map[string]Profile{}
	}
	c.dir = d
	return c, nil
}

// Save writes the config file atomically, owner-readable only.
func (c *Config) Save() error {
	if c.dir == "" {
		d, err := Dir()
		if err != nil {
			return err
		}
		c.dir = d
	}
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(c.dir, "config.json.tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(c.dir, "config.json"))
}

var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,31}$`)

// ValidName reports whether s can be used as a profile name. Names end up in service names
// and file names, so they are kept to a safe character set.
func ValidName(s string) error {
	if !nameRE.MatchString(s) {
		return fmt.Errorf("profile name %q: use letters, digits, '-', '_' or '.', up to 32 characters", s)
	}
	return nil
}

// Names returns the profile names in sorted order.
func (c *Config) Names() []string {
	var out []string
	for n := range c.Profiles {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Pick returns the profile to use: the named one, else the default, else the only one.
// ok is false when nothing is configured. Naming a profile that does not exist is an error.
func (c *Config) Pick(name string) (string, Profile, bool, error) {
	if name != "" {
		p, found := c.Profiles[name]
		if !found {
			return "", Profile{}, false, fmt.Errorf("no profile named %q (have: %v)", name, c.Names())
		}
		return name, p, true, nil
	}
	if c.Default != "" {
		if p, found := c.Profiles[c.Default]; found {
			return c.Default, p, true, nil
		}
	}
	if len(c.Profiles) == 1 {
		for n, p := range c.Profiles {
			return n, p, true, nil
		}
	}
	if len(c.Profiles) > 1 {
		return "", Profile{}, false, fmt.Errorf("several profiles are set up and none is the default; pick one with -profile or 'seestar profile use NAME' (have: %v)", c.Names())
	}
	return "", Profile{}, false, nil
}
