package statelog_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// TestASpokenWaitIsNeverACountOfSeconds holds the engine's sentences to one
// spelling of a duration. A bulk edit refused while another held the lease
// said "retry in about 81155 seconds", a number a person has to divide; a
// wait is said through [statelog.Spoken] ("22h 33m") like every other
// duration the engine states.
func TestASpokenWaitIsNeverACountOfSeconds(t *testing.T) {
	if got := statelog.Spoken(81155 * time.Second); got != "22h 33m" {
		t.Fatalf("Spoken(81155s) = %q, want %q", got, "22h 33m")
	}
	if got := statelog.Spoken(125 * time.Second); got != "2m 5s" {
		t.Fatalf("Spoken(125s) = %q, want %q", got, "2m 5s")
	}

	// A WAIT a person is told to sit out, printed as a count: "retry in
	// about %d seconds", "try again in %v secs". A field whose unit is the
	// second ("an interval is %d to %d seconds") states a setting's unit and
	// is not a wait; a fixed deadline ("within %d seconds" of a ten-second
	// budget) is a constant, not a count the engine was handed.
	count := regexp.MustCompile(`(?i)again in (about )?%[-+ #0-9.]*[dvfg] ?(seconds|secs)\b|retry in (about )?%[-+ #0-9.]*[dvfg] ?(seconds|secs)\b`)
	root := filepath.Join("..", "..")
	var offenders []string
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if count.Match(body) {
				offenders = append(offenders, path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("a sentence prints a count of seconds; say it through statelog.Spoken: %v", offenders)
	}
}
