//go:build !windows && !darwin

package secret

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// UseMachineScope is a no-op outside Windows.
func UseMachineScope(bool) {}

// The file is owner-only (0600) in an owner-only directory. Base64 only keeps the password
// from being read at a glance; the file permissions are the protection.
func set(email, password string) error {
	return writeFile(key(email)+".pw", []byte(base64.StdEncoding.EncodeToString([]byte(password))+"\n"))
}

func get(email string) (string, error) {
	b, err := readFile(key(email) + ".pw")
	if err != nil {
		return "", err
	}
	pw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil {
		return "", fmt.Errorf("saved password file is damaged; run 'seestar login' again: %w", err)
	}
	return string(pw), nil
}

func del(email string) error { return removeFile(key(email) + ".pw") }

func backend() string { return "an owner-only file in the config directory" }
