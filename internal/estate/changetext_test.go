package estate

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
)

// A CHANGES LISTING NARROWED BY WORDS IS READ, OR REFUSED UNREAD: a `changes` read naming its
// words reaches the code half; one naming a key this build does not declare (text_later, standing
// for words this build cannot read) is refused naming it, the code half unasked, so the router
// asks another.
// Mutation: drop Text from gitlet.ReadQuery, and the first read is refused naming args.text.
func TestAChangesListingNarrowedByWordsIsReadOrRefusedUnread(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	node := f.nodes["data-a"]
	start := f.start(t)
	ask := func(key string) reply {
		return askSigned(t, start, "data-a", request{Op: opCodeRead.spec.name, Args: json.RawMessage(
			`{"kind":"changes","asker":{"kind":"operator","operator_id":"token:founder"},"` + key +
				`":"login","fresh":{"level":"stale"}}`)})
	}
	rep := ask("text_later")
	var refused *ErrUnknownKeys
	if err := decodeError(rep.Err); rep.Unserved != "" || rep.Result != nil || !errors.As(err, &refused) ||
		refused.Op != opCodeRead.spec.name || !slices.Equal(refused.Keys, []string{"args.text_later"}) {
		t.Fatalf("a changes read naming words this build does not declare was answered %+v, want it refused "+
			"naming args.text_later", rep)
	}
	if node.askedFor(opCodeRead.spec.name) {
		t.Fatal("the code half was asked a changes read naming words this build does not declare")
	}
	if rep = ask("text"); rep.Err != nil {
		t.Fatalf("a changes read narrowed by its words was refused: %v", decodeError(rep.Err))
	}
	if !node.askedFor(opCodeRead.spec.name) {
		t.Fatal("the code half was never asked a changes read narrowed by its words")
	}
}
