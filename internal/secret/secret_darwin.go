//go:build darwin

package secret

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// UseMachineScope is a no-op outside Windows.
func UseMachineScope(bool) {}

func set(email, password string) error {
	// -U updates an existing item in place. The password is passed as an argument, which
	// the security tool requires for a non-interactive add; it is visible to the same user's
	// processes for the instant the command runs.
	cmd := exec.Command("security", "add-generic-password", "-U", "-s", service, "-a", email, "-w", password)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("save to Keychain: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func get(email string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("security", "find-generic-password", "-s", service, "-a", email, "-w")
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		// Exit status 44 is errSecItemNotFound.
		if errors.As(err, &ee) && ee.ExitCode() == 44 {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("read from Keychain: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimRight(stdout.String(), "\r\n"), nil
}

func del(email string) error {
	out, err := exec.Command("security", "delete-generic-password", "-s", service, "-a", email).CombinedOutput()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 44 {
			return nil
		}
		return fmt.Errorf("remove from Keychain: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func backend() string { return "macOS Keychain" }
