package config

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// THE SAVE INTERVAL IS BOUNDED, AND ZERO TAKES THE DEFAULT. Unset (or 0)
// saves a running session every ten minutes; a value below five minutes or
// above an hour is refused, naming the field and the default, rather than
// clamped into the range, which would leave the document saying one thing
// and every node saving on another. The bounds themselves are admitted.
//
// The schema's maximum and the description state the same bounds and default,
// so moving a constant without them fails here rather than leaving them stale.
//
// Mutation: clamp instead of refusing, drop the floor, have SaveInterval
// answer 0 for an unset field, or let js or desc name another bound, and this
// fails.
func TestTheSaveIntervalIsBoundedAndZeroTakesTheDefault(t *testing.T) {
	t.Parallel()
	const path = "turn_engine.conversation_session.save_interval_seconds"
	doc := func(secs int) string {
		return "name: Acme\nturn_engine:\n  conversation_session:\n    save_interval_seconds: " +
			strconv.Itoa(secs) + "\n"
	}

	// Unset, in a block built in code and in a document that never names it,
	// and an explicit 0, all take the default: ten minutes, inside the bounds.
	if DefaultSaveInterval != 10*time.Minute {
		t.Errorf("the default is %s, want ten minutes", DefaultSaveInterval)
	}
	var unset ConversationSession
	if got := unset.SaveInterval(); got != DefaultSaveInterval {
		t.Errorf("an unset field saves every %s, want the default %s", got, DefaultSaveInterval)
	}
	for _, d := range []string{"name: Acme\n", doc(0)} {
		cs := mustCompany(t, d).TurnEngine.ConversationSession
		if got := cs.SaveInterval(); got != DefaultSaveInterval {
			t.Errorf("%q saves every %s, want the default %s", d, got, DefaultSaveInterval)
		}
	}

	// The bounds are values a company may choose, and are read as written.
	for _, secs := range []int{300, 3600} {
		cs := mustCompany(t, doc(secs)).TurnEngine.ConversationSession
		if got, want := cs.SaveInterval(), time.Duration(secs)*time.Second; got != want {
			t.Errorf("save_interval_seconds: %d saves every %s, want %s", secs, got, want)
		}
	}

	// The schema and the description an operator reads say the same numbers.
	inSecs := func(d time.Duration) int { return int(d / time.Second) }
	field, ok := reflect.TypeFor[ConversationSession]().FieldByName("SaveIntervalSeconds")
	if !ok {
		t.Fatal("ConversationSession has no SaveIntervalSeconds field")
	}
	if js, want := field.Tag.Get("js"), fmt.Sprintf("min=0;max=%d", inSecs(MaxSaveInterval)); js != want {
		t.Errorf("the schema bounds save_interval_seconds as %q, want %q", js, want)
	}
	stated := fmt.Sprintf("default %d, 0 takes it, %d..%d",
		inSecs(DefaultSaveInterval), inSecs(MinSaveInterval), inSecs(MaxSaveInterval))
	if desc := field.Tag.Get("desc"); !strings.Contains(desc, stated) {
		t.Errorf("the description of save_interval_seconds does not say %q: %q", stated, desc)
	}

	// Just past either bound, and a negative value, are refused by name and
	// told the default, never clamped to the nearest bound.
	for _, secs := range []int{299, 3601, -1} {
		t.Run(strconv.Itoa(secs), func(t *testing.T) {
			t.Parallel()
			err := rejects(t, doc(secs), path)
			if !errors.Is(err, ErrOutOfRange) {
				t.Fatalf("want %v, got:\n%v", ErrOutOfRange, err)
			}
			if !strings.Contains(err.Error(), "default of 600") {
				t.Errorf("the refusal does not say the default:\n%v", err)
			}
		})
	}
}
