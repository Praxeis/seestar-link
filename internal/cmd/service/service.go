// Package service installs the proxy as a background service that starts on its own and
// restarts if it ever exits: a Windows service, a macOS LaunchAgent, or a systemd user
// unit on Linux. Each profile gets its own service, so two scopes can run side by side.
package service

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Praxeis/seestar-link/internal/cli"
	"github.com/Praxeis/seestar-link/internal/config"
	"github.com/Praxeis/seestar-link/internal/secret"
)

const help = `%s keeps the proxy running in the background for a profile.

Usage:
  %[1]s install   [-profile NAME]   install, start, and start again at every boot/login
  %[1]s uninstall [-profile NAME]
  %[1]s start     [-profile NAME]
  %[1]s stop      [-profile NAME]
  %[1]s status    [-profile NAME]

Run 'seestar login' and 'seestar profile add' first: the service uses the profile's ports
and the saved login, and never prompts. 'seestar status' shows the tunnel state.

Platform notes:
  Windows  a Windows service ("Seestar Link (NAME)"), installed from an Administrator
           terminal. Starts at boot, before anyone logs in. The program is copied to
           Program Files and the profile and login to ProgramData.
  macOS    a LaunchAgent for your user. Runs while you are logged in.
           Log: ~/Library/Logs/seestar-link/NAME.log
  Linux    a systemd user unit. Log: journalctl --user -u seestar-link-NAME
           To keep it running when you are logged out: loginctl enable-linger $USER
`

// target is the resolved service to act on.
type target struct {
	profile string
	prof    config.Profile
	name    string // service / unit name
	display string
}

func resolve() (target, error) {
	c, err := config.Load()
	if err != nil {
		return target{}, err
	}
	name, p, ok, err := c.Pick(cli.ProfileName())
	if err != nil {
		return target{}, err
	}
	if !ok {
		return target{}, errors.New("no profiles yet: run 'seestar profile add NAME' first")
	}
	return target{
		profile: name,
		prof:    p,
		name:    config.AppName + "-" + name,
		display: "Seestar Link (" + name + ")",
	}, nil
}

// savedPassword checks the profile can log in unattended and returns its password.
func savedPassword(t target) (string, error) {
	pw, err := secret.Get(t.prof.Email)
	if errors.Is(err, secret.ErrNotFound) {
		return "", fmt.Errorf("no saved login for %s: run 'seestar login -profile %s' first", t.prof.Email, t.profile)
	}
	return pw, err
}

// runArgs is the command line a service manager starts: the run command for the profile,
// plus the configuration directory if it is not the default one.
func runArgs(exe string, t target) []string {
	args := []string{exe, "service", "run", "-profile", t.profile}
	if d, ok := config.CustomDir(); ok {
		if abs, err := filepath.Abs(d); err == nil {
			d = abs
		}
		args = append(args, "-config-dir", d)
	}
	return args
}

// executable returns the absolute, symlink-resolved path of the running binary.
func executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return filepath.Abs(exe)
}

// warnIfTransient flags a binary run from somewhere it is likely to be moved or deleted.
func warnIfTransient(exe string) {
	l := strings.ToLower(exe)
	for _, s := range []string{"downloads", string(filepath.Separator) + "tmp" + string(filepath.Separator), "go-build"} {
		if strings.Contains(l, s) {
			fmt.Fprintf(os.Stderr, "note: the service will run %s; if you move or delete it, run 'seestar service install' again from its new place.\n", exe)
			return
		}
	}
}

// Main is 'seestar service'.
func Main() {
	usage := func() {
		fmt.Fprintf(os.Stderr, help, "seestar service")
		flag.PrintDefaults()
	}
	// The action may come before or after the flags: "install -profile s30" or
	// "-profile s30 install".
	cli.ParseFlags(usage)
	pos := cli.ParseRest(usage)
	if len(pos) != 1 {
		usage()
		os.Exit(2)
	}
	action := pos[0]
	cli.ProgName += " " + action

	if action == "run" {
		// Started by the service manager; settings come from the flags it was given.
		if err := run(); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	t, err := resolve()
	if err != nil {
		fail(err)
	}
	switch action {
	case "install":
		err = install(t)
	case "uninstall", "remove":
		err = uninstall(t)
	case "start":
		err = start(t)
	case "stop":
		err = stop(t)
	case "status":
		err = status(t)
	default:
		fmt.Fprintf(os.Stderr, help, "seestar service")
		os.Exit(2)
	}
	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
