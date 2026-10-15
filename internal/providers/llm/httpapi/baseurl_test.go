package httpapi

import (
	"errors"
	"strings"
	"testing"
)

// THE ENDPOINT A BACKEND HANDS ITS SDK IS TRIMMED AND JUDGED AT CONSTRUCTION.
// Both SDKs' WithBaseURL defer a parse failure to every request, and the
// failure prints the raw URL — userinfo included — so a value that does not
// parse is refused here by a sentence that repeats none of it, and a value
// with surrounding space is sent without it.
//
// Mutation: drop the trim, and the spaced and CR cases are refused; return
// the parse error itself, and the password reaches the refusal.
func TestBaseURLIsTrimmedAndRefusedWithoutRepeatingIt(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{
		"":                             "https://default.example.com",
		"   ":                          "https://default.example.com",
		"https://gw.example.com/v1":    "https://gw.example.com/v1",
		" https://gw.example.com/v1\r": "https://gw.example.com/v1",
		"\thttp://127.0.0.1:8317\n":    "http://127.0.0.1:8317",
	} {
		got, err := BaseURL(raw, "https://default.example.com")
		if err != nil || got != want {
			t.Errorf("BaseURL(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	const password = "gw-password"
	for _, raw := range []string{
		"https://gw-user:" + password + "@gw.example.com/v1\x7f",
		"https://gw-user:" + password + "@gw example.com/v1",
		"gw-user:" + password + "@gw.example.com/v1",
		"ftp://gw-user:" + password + "@gw.example.com",
		"https:///v1",
	} {
		_, err := BaseURL(raw, "https://default.example.com")
		if !errors.Is(err, ErrBaseURL) {
			t.Errorf("BaseURL(%q) = %v, want ErrBaseURL", raw, err)
			continue
		}
		if strings.Contains(err.Error(), password) || strings.Contains(err.Error(), "gw.example.com") {
			t.Errorf("the refusal of %q repeats the value: %v", raw, err)
		}
	}
}
