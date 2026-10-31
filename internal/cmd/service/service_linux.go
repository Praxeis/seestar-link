//go:build linux

package service

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Praxeis/seestar-link/internal/cmd/proxy"
)

func unitName(t target) string { return t.name + ".service" }

func unitPath(t target) (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "systemd", "user", unitName(t)), nil
}

func systemctl(args ...string) error {
	cmd := exec.Command("systemctl", append([]string{"--user"}, args...)...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// quote renders an argument for a systemd ExecStart line.
func quote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"'\\$%") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `$$`, `%`, `%%`)
	return `"` + r.Replace(s) + `"`
}

func install(t target) error {
	if _, err := savedPassword(t); err != nil {
		return err
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return fmt.Errorf("systemctl not found; run 'seestar proxy -profile %s' under your own process supervisor instead", t.profile)
	}
	exe, err := executable()
	if err != nil {
		return err
	}
	warnIfTransient(exe)
	up, err := unitPath(t)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(up), 0o755); err != nil {
		return err
	}
	var args []string
	for _, a := range runArgs(exe, t) {
		args = append(args, quote(a))
	}
	unit := fmt.Sprintf(`[Unit]
Description=%s
Wants=network-online.target
After=network-online.target

[Service]
ExecStart=%s
Restart=always
RestartSec=10

[Install]
WantedBy=default.target
`, t.display, strings.Join(args, " "))
	if err := os.WriteFile(up, []byte(unit), 0o644); err != nil {
		return err
	}
	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	if err := systemctl("enable", "--now", unitName(t)); err != nil {
		return err
	}
	if err := systemctl("restart", unitName(t)); err != nil { // pick up a changed binary or unit
		return err
	}
	fmt.Printf(`Installed and started %s.
Log:   journalctl --user -u %s -f
Check: seestar status -profile %s
To keep it running while you are logged out and start it at boot:
  loginctl enable-linger $USER
`, unitName(t), t.name, t.profile)
	return nil
}

func uninstall(t target) error {
	up, err := unitPath(t)
	if err != nil {
		return err
	}
	systemctl("disable", "--now", unitName(t))
	if err := os.Remove(up); err != nil && !os.IsNotExist(err) {
		return err
	}
	systemctl("daemon-reload")
	fmt.Printf("Removed %s.\n", unitName(t))
	return nil
}

func start(t target) error { return systemctl("start", unitName(t)) }
func stop(t target) error  { return systemctl("stop", unitName(t)) }
func status(t target) error {
	systemctl("status", "--no-pager", unitName(t)) // non-zero when stopped; the output says so
	return nil
}

func run() error { return proxy.RunService(context.Background()) }
