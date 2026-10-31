// Package secret keeps the ZWO account password between runs, keyed by account email.
//
// Each platform uses what it has without extra dependencies:
//   - Windows: DPAPI-encrypted file in the config directory (user scope, or machine scope
//     for a Windows service, which runs as LocalSystem).
//   - macOS: the login Keychain, through the security command.
//   - Linux and others: a file readable only by its owner in the config directory.
package secret

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/Praxeis/seestar-link/internal/config"
)

// ErrNotFound is returned by Get when no password is stored for the account.
var ErrNotFound = errors.New("no saved password")

// service names the Keychain item and keys the stored files.
const service = config.AppName

// key derives a stable file-safe name from an email, so the address itself is not exposed
// in a file listing.
func key(email string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	return hex.EncodeToString(sum[:8])
}

// dir is where file-backed secrets live.
func dir() (string, error) {
	d, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "secrets"), nil
}

// writeFile writes b to the secrets directory with owner-only permissions.
func writeFile(name string, b []byte) error {
	d, err := dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(d, name+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(d, name))
}

// readFile reads a file-backed secret, mapping a missing file to ErrNotFound.
func readFile(name string) ([]byte, error) {
	d, err := dir()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(d, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return b, err
}

// removeFile deletes a file-backed secret; a missing file is not an error.
func removeFile(name string) error {
	d, err := dir()
	if err != nil {
		return err
	}
	err = os.Remove(filepath.Join(d, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Set stores the password for email.
func Set(email, password string) error { return set(email, password) }

// Get returns the stored password for email, or ErrNotFound.
func Get(email string) (string, error) { return get(email) }

// Delete removes any stored password for email.
func Delete(email string) error { return del(email) }

// Backend describes where passwords are kept on this platform, for user-facing messages.
func Backend() string { return backend() }
