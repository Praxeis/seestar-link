package service

import "testing"

func TestQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/usr/local/bin/seestar":   "/usr/local/bin/seestar",
		"/home/j/My Tools/seestar": `"/home/j/My Tools/seestar"`,
		`/odd/"name"`:              `"/odd/\"name\""`,
		"/costs/$5/100%/seestar":   `"/costs/$$5/100%%/seestar"`,
		"":                         `""`,
	} {
		if got := quote(in); got != want {
			t.Errorf("quote(%q) = %s, want %s", in, got, want)
		}
	}
}
