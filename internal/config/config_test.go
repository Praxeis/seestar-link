package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveLoadAndPick(t *testing.T) {
	SetDir(t.TempDir())
	t.Cleanup(func() { SetDir("") })

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := c.Pick(""); ok || err != nil {
		t.Fatalf("empty config: ok=%v err=%v", ok, err)
	}

	c.Profiles["s30"] = Profile{Email: "a@b.c", Sn: "111", Ports: []string{"4700:4700"}}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	c, _ = Load()
	if n, p, ok, err := c.Pick(""); !ok || err != nil || n != "s30" || p.Sn != "111" {
		t.Fatalf("single profile is picked without a default: %q %+v %v %v", n, p, ok, err)
	}

	c.Profiles["s50"] = Profile{Email: "a@b.c", Sn: "222"}
	if _, _, _, err := c.Pick(""); err == nil {
		t.Fatal("two profiles and no default should ask the user to choose")
	}
	c.Default = "s50"
	if n, _, _, _ := c.Pick(""); n != "s50" {
		t.Fatalf("default not used: %q", n)
	}
	if n, _, _, _ := c.Pick("s30"); n != "s30" {
		t.Fatalf("explicit name not used: %q", n)
	}
	if _, _, _, err := c.Pick("nope"); err == nil {
		t.Fatal("unknown profile accepted")
	}

	fi, err := os.Stat(filepath.Join(dirOverride, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if os.PathSeparator == '/' && fi.Mode().Perm() != 0o600 {
		t.Fatalf("config.json mode %v, want 0600", fi.Mode().Perm())
	}
}

func TestValidName(t *testing.T) {
	for _, ok := range []string{"s30", "S50-pro", "back_yard.2"} {
		if err := ValidName(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-x", "has space", "a/b", `a\b`, "x;rm", "0123456789012345678901234567890123"} {
		if ValidName(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
