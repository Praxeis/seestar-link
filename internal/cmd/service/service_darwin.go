//go:build darwin

package service

import (
	"context"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Praxeis/seestar-link/internal/cmd/proxy"
)

func label(t target) string { return "com.praxeis.seestar-link." + t.profile }

func plistPath(t target) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", label(t)+".plist"), nil
}

func logPath(t target) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Logs", "seestar-link", t.profile+".log")
}

func domain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

func launchctl(args ...string) (string, error) {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func install(t target) error {
	if _, err := savedPassword(t); err != nil {
		return err
	}
	exe, err := executable()
	if err != nil {
		return err
	}
	warnIfTransient(exe)
	pp, err := plistPath(t)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(pp), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(logPath(t)), 0o755); err != nil {
		return err
	}
	args := []string{exe, "service", "run", "-profile", t.profile}
	var b strings.Builder
	for _, a := range args {
		fmt.Fprintf(&b, "\t\t<string>%s</string>\n", html.EscapeString(a))
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
%s	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>10</integer>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`, label(t), b.String(), html.EscapeString(logPath(t)), html.EscapeString(logPath(t)))
	if err := os.WriteFile(pp, []byte(plist), 0o644); err != nil {
		return err
	}
	launchctl("bootout", domain(), pp) // replace a running copy; fails harmlessly if none
	if out, err := launchctl("bootstrap", domain(), pp); err != nil {
		return fmt.Errorf("launchctl bootstrap: %v: %s", err, out)
	}
	fmt.Printf("Installed and started %q (runs while you are logged in).\nLog: %s\nCheck it with: seestar status -profile %s\n",
		t.display, logPath(t), t.profile)
	return nil
}

func uninstall(t target) error {
	pp, err := plistPath(t)
	if err != nil {
		return err
	}
	launchctl("bootout", domain(), pp)
	if err := os.Remove(pp); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Printf("Removed %q.\n", t.display)
	return nil
}

func start(t target) error {
	pp, err := plistPath(t)
	if err != nil {
		return err
	}
	if _, err := os.Stat(pp); err != nil {
		return fmt.Errorf("%s is not installed (seestar service install -profile %s)", t.display, t.profile)
	}
	if out, err := launchctl("bootstrap", domain(), pp); err != nil {
		// Already loaded: kick it instead.
		if out2, err2 := launchctl("kickstart", domain()+"/"+label(t)); err2 != nil {
			return fmt.Errorf("start: %s %s", out, out2)
		}
	}
	fmt.Printf("Started %q.\n", t.display)
	return nil
}

func stop(t target) error {
	pp, err := plistPath(t)
	if err != nil {
		return err
	}
	// KeepAlive would restart a killed job, so stopping means unloading it until the next
	// start or login.
	launchctl("bootout", domain(), pp)
	fmt.Printf("Stopped %q until 'seestar service start' or your next login.\n", t.display)
	return nil
}

func status(t target) error {
	out, err := launchctl("print", domain()+"/"+label(t))
	if err != nil {
		pp, _ := plistPath(t)
		if _, serr := os.Stat(pp); serr != nil {
			fmt.Printf("%s: not installed\n", t.display)
		} else {
			fmt.Printf("%s: installed, not running\n", t.display)
		}
		return nil
	}
	state := "unknown"
	for _, line := range strings.Split(out, "\n") {
		if l := strings.TrimSpace(line); strings.HasPrefix(l, "state = ") {
			state = strings.TrimPrefix(l, "state = ")
			break
		}
	}
	fmt.Printf("%s: %s\nLog: %s\n", t.display, state, logPath(t))
	return nil
}

func run() error { return proxy.RunService(context.Background()) }
