package account

import (
	"testing"

	"github.com/Praxeis/seestar-link/internal/config"
)

func TestSecondProfileGetsItsOwnPorts(t *testing.T) {
	c := &config.Config{Profiles: map[string]config.Profile{}}
	if got := freeLocal(c, 4700); got != 4700 {
		t.Fatalf("first profile local port %d", got)
	}
	if got := freeStatus(c); got != config.DefaultStatusAddr {
		t.Fatalf("first profile status %s", got)
	}

	c.Profiles["s30"] = config.Profile{} // no ports saved means the 4700:4700 default
	if got := freeLocal(c, 4700); got != 4710 {
		t.Fatalf("second profile local port %d, want 4710", got)
	}
	if got := freeStatus(c); got != "127.0.0.1:4800" {
		t.Fatalf("second profile status %s", got)
	}

	c.Profiles["s50"] = config.Profile{Ports: []string{"4710:4700", "32333:32323"}, Status: "127.0.0.1:4800"}
	if got := freeLocal(c, 4700); got != 4720 {
		t.Fatalf("third profile local port %d, want 4720", got)
	}
	if got := freeStatus(c); got != "127.0.0.1:4801" {
		t.Fatalf("third profile status %s", got)
	}
}

func TestHuman(t *testing.T) {
	for n, want := range map[int64]string{0: "0B", 1023: "1023B", 1024: "1.0KB", 5 << 20: "5.0MB"} {
		if got := human(n); got != want {
			t.Errorf("human(%d) = %s, want %s", n, got, want)
		}
	}
}
