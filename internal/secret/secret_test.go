//go:build !darwin

// The macOS backend talks to the real login Keychain, so it is not exercised in tests.

package secret

import (
	"errors"
	"testing"

	"github.com/Praxeis/seestar-link/internal/config"
)

func TestRoundTrip(t *testing.T) {
	config.SetDir(t.TempDir())
	t.Cleanup(func() { config.SetDir("") })

	if _, err := Get("me@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get before Set: %v", err)
	}
	const pw = `p@ss "word" with ünïcode`
	if err := Set("Me@Example.com ", pw); err != nil {
		t.Fatal(err)
	}
	got, err := Get("me@example.com") // case and spacing do not matter
	if err != nil || got != pw {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if err := Set("me@example.com", "new"); err != nil {
		t.Fatal(err)
	}
	if got, _ := Get("me@example.com"); got != "new" {
		t.Fatalf("overwrite: got %q", got)
	}
	if err := Delete("me@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := Get("me@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete: %v", err)
	}
	if err := Delete("me@example.com"); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
}
