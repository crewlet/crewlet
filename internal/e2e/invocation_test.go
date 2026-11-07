package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE STAND-IN'S INVOCATION IS SEEN WHOLE OR NOT AT ALL.
//
// A case waits for the record's path to exist and then decodes it, as
// "the harness to start" and agentArgv after it do, so a record a case can
// see before it is written fails a healthy run with "unexpected end of JSON
// input" (crewlet/crewlet#681, twice on CI). Here a reader races the writer
// the way such a case does, on a record large enough that writing it takes
// many pages, so a cut one has every chance to show: it decodes every record
// it finds, and a record it cannot decode is the failure.
func TestTheStandInsInvocationIsSeenWholeOrNotAtAll(t *testing.T) {
	t.Parallel()
	want := codingInvocation{Argv: []string{"-p", "--output-format", "stream-json"},
		Stdin: strings.Repeat("the brief, at the length a long one runs to. ", 1<<15)}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for i := range 20 {
		path := filepath.Join(dir, fmt.Sprintf("argv-%d.txt", i))
		stop := make(chan struct{})
		seen := make(chan error, 1)
		go func() { seen <- readWhenThere(path, stop) }()
		if recordErr := recordWhole(path, raw); recordErr != nil {
			close(stop)
			t.Fatalf("recording the invocation: %v", recordErr)
		}
		if readErr := <-seen; readErr != nil {
			t.Fatalf("a case waiting for the record by its path read it before it was whole: %v", readErr)
		}
		close(stop)
	}
}

// readWhenThere waits, as a case does, until path exists, then reads and
// decodes it: nil for a record that decodes whole, the error for one that does
// not, and nil if stop closes first.
func readWhenThere(path string, stop <-chan struct{}) error {
	for {
		select {
		case <-stop:
			return nil
		default:
		}
		if _, err := os.Stat(path); err != nil {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var got codingInvocation
		if err := json.Unmarshal(raw, &got); err != nil {
			return fmt.Errorf("%d bytes of the record decoded: %w", len(raw), err)
		}
		return nil
	}
}
