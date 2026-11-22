package executor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// AN OPENCODE MESSAGE READS WHAT ITS STEP MEASURED AND THE FAILURE IT ENDED
// ON, off each driven release's export (testdata/opencode, described by
// openCodeExportFixture): a step that ended without a failure carries its
// prompt's uncached input and its cache reads; the step its provider refused
// and each step the session was interrupted in carry their failure.
//
// Mutation: spell `type`, `tokens`, `input`, `cache`, `read` or `error`
// otherwise than OpenCode 2 writes it, and a step reads as one that measured
// nothing, or as one that ended without its failure.
func TestAnOpenCodeMessageReadsWhatItsStepMeasuredAndTheFailureItEndedOn(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"2.3.0"} {
		raw, err := os.ReadFile(filepath.Join("testdata", "opencode", version, "export.json"))
		if err != nil {
			t.Fatalf("read the fixture: %v", err)
		}
		var export struct {
			Messages []openCodeMessage `json:"messages"`
		}
		if err = json.Unmarshal(raw, &export); err != nil {
			t.Fatalf("%s: the export does not decode: %v", version, err)
		}
		measured := 0
		failures := map[string]int{}
		for _, m := range export.Messages {
			switch {
			case m.Type != openCodeAssistant:
			case !absent(m.Error):
				var failure struct {
					Type string `json:"type"`
				}
				if err = json.Unmarshal(m.Error, &failure); err != nil {
					t.Errorf("%s: a step's failure does not decode: %v", version, err)
				}
				failures[failure.Type]++
			case m.Tokens == nil || m.Tokens.Input <= 0 || m.Tokens.Cache.Read <= 0:
				t.Errorf("%s: a step that ended without a failure reads as measuring %+v", version, m.Tokens)
			default:
				measured++
			}
		}
		if measured == 0 || failures["provider.invalid-request"] != 1 || failures["aborted"] != 2 {
			t.Errorf("%s: %d steps read as measured and the failures %v, want some measured, the one "+
				"the provider refused and the two the session was interrupted in", version, measured, failures)
		}
	}
}
