package prompts

import (
	"strings"
	"testing"
	"time"
)

// A SESSION SAVED WHILE ITS RUN WORKED SAYS WHEN AND THAT IT WENT ON. A run
// continuing a capture taken while its job still worked is told the session
// above was saved then, at the instant it was saved (in UTC, whatever zone the
// instant was read in), and that the run went on after it, so the session ends
// where it was saved and not where the run stopped; that it may end mid-call;
// that any call through the seat's tools after it is told below; and that what
// else the run did is in no record, so the working tree is read first. With no
// instant stated it names none, never the zero time. It names no checkpoint,
// which no other text of the engine calls a capture.
//
// Mutation: leave the instant out, and the run cannot tell where its session
// ends; say it however it was read, or say the zero time, and the run is told
// another instant, or a session saved in the year one.
func TestASessionSavedWhileItsRunWorkedSaysWhenAndThatItWentOn(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 8, 9, 20, 0, 0, time.UTC)
	said := []string{"## The session above was saved while its run still worked\n",
		"while the run that worked in it was still working, and that run went on after it: the session above " +
			"ends where it was saved, not where the run stopped.",
		"So it may end in the middle of a call: a tool call with no result, which may or may not have run.",
		"Any call the run made through your tools after it is told below.",
		"What else it did after it, the commands it ran and the files it read or changed, is in no record: " +
			"check the working tree, and what any such call was meant to do, before doing any of it again."}
	for _, c := range []struct {
		name        string
		at          time.Time
		says, never []string
	}{
		{name: "saved at an instant", at: at,
			says:  append([]string{"It was saved at 2026-11-08T09:20:00Z, while the run"}, said...),
			never: []string{"checkpoint", "0001-01-01"}},
		{name: "saved at an instant read in another zone", at: at.In(time.FixedZone("UTC+3", 3*60*60)),
			says:  []string{"It was saved at 2026-11-08T09:20:00Z, while the run"},
			never: []string{"12:20:00", "+03:00"}},
		{name: "saved at no instant stated", at: time.Time{},
			says:  append([]string{"It was saved while the run"}, said...),
			never: []string{"saved at", "0001-01-01", "checkpoint"}},
	} {
		text := SavedSession(c.at)
		for _, want := range c.says {
			if !strings.Contains(text, want) {
				t.Errorf("%s: the text does not say %q:\n%s", c.name, want, text)
			}
		}
		for _, not := range c.never {
			if strings.Contains(text, not) {
				t.Errorf("%s: the text says %q:\n%s", c.name, not, text)
			}
		}
	}
}
