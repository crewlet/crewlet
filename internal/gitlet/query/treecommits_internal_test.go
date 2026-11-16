package query

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// A TREE WINDOW'S RULE IS ITS FORMAT: which names begin a window (FNV-1a 64 of
// the name, 0 modulo the size), where a run of names that begin none is cut,
// and a window's bytes are what tree_window/1 names, so a kept window is never
// read by another rule. A window no answer could carry is never kept.
// Mutation: hash another way, move the forced start, or drop decodeWindow's checks.
func TestATreeWindowsRuleIsItsFormat(t *testing.T) {
	t.Parallel()
	golden := map[string]bool{"README.md": false, "LICENSE": false, "Makefile": false, "go.mod": false,
		"go.sum": false, "cmd": false, "internal": true, "docs": false, "dashboard": false, "static": false,
		".github": false, ".gitignore": false, "main.go": false, "server.go": false, "client.go": false,
		"api": false, "web": false, "file201.go": true, "file308.go": true, "file344.go": true}
	for name, want := range golden {
		if got := windowStart(name, 64); got != want {
			t.Errorf("windowStart(%q, 64) = %t, want %t: the rule moved without %s", name, got, want,
				treeWindowFormat)
		}
	}
	starts := func(names []string) []int {
		rule := windowRule{size: 64}
		var out []int
		for i, n := range names {
			if _, begins := rule.next(i, n, false); begins {
				out = append(out, i)
			}
		}
		return out
	}
	var none []string
	for i := 0; len(none) < 300; i++ {
		if n := fmt.Sprintf("n%05d", i); !windowStart(n, 64) {
			none = append(none, n)
		}
	}
	if got := starts(none); !slices.Equal(got, []int{0, 256}) {
		t.Errorf("300 names beginning no window begin windows at %v, want the first and the forced 256", got)
	}
	tenth := slices.Clone(none)
	tenth[10] = "internal"
	if got := starts(tenth); !slices.Equal(got, []int{0, 10, 266}) {
		t.Errorf("with entry 10 beginning one, windows begin at %v, want 0, 10 and 266", got)
	}
	first := windowRule{size: 64}
	if start, _ := first.next(0, "n00000", false); start != "" {
		t.Errorf("the first window, its first name beginning none, is keyed %q, want \"\"", start)
	}
	if start, _ := first.next(0, "internal", false); start != "internal" {
		t.Errorf("a first entry beginning a window keys it %q, want its own name", start)
	}

	names, commits := []string{"a", "caf\xe9", "z"}, []string{strings.Repeat("1", 40), "", strings.Repeat("2", 40)}
	body := encodeWindow(names, commits)
	got, err := decodeWindow(body)
	if err != nil || len(got) != 3 || got["caf\xe9"] != "" || got["a"] != commits[0] || got["z"] != commits[2] {
		t.Errorf("a window read back %v (%v), want its three answers", got, err)
	}
	if _, held := got["caf\xe9"]; !held {
		t.Error("a window's answer of no commit read back absent")
	}
	for why, b := range map[string][]byte{"cut short": body[:len(body)-1], "a byte past its end": append(
		slices.Clone(body), 0), "a name twice": encodeWindow([]string{"a", "a"}, []string{"", ""})} {
		if _, err := decodeWindow(b); err == nil {
			t.Errorf("a window %s was read", why)
		}
	}

	long := func(n int) []dirEntry {
		out := make([]dirEntry, n)
		for i := range out {
			out[i] = dirEntry{name: fmt.Sprintf("%03d%s", i, strings.Repeat("x", 247)), window: ""}
		}
		return out
	}
	if w := cutWindows(long(256)); len(w) != 1 || w[0].keepable {
		t.Errorf("a window of 256 names of 250 bytes is keepable: %+v", w)
	}
	if w := cutWindows(long(64)); len(w) != 1 || !w[0].keepable {
		t.Error("a window of 64 names of 250 bytes is not keepable")
	}
	withLong := long(3)
	withLong[1].long = true
	if w := cutWindows(withLong); w[0].keepable {
		t.Error("a window holding a long entry is keepable")
	}
}
