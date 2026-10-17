package chat_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/statelog"
)

// AN ERASE NAMES ONE SET, whatever order or repetition it was asked in.
//
// The operation is the caller's and arbitrates on the ROOM, so the publisher
// collapses a retry onto whatever record that operation already holds there:
// a caller that bound its operation to the list as typed would mint two
// operations for one erase asked in two orders, and a store that published
// the list as typed would carry a record the caller's binding never named.
// [chat.EraseSet] is the one spelling both read, and this holds the store to
// publishing exactly it. Mutation: publish the trimmed list without sorting
// or deduplicating it, and the record below carries the order it was asked
// in.
func TestAnEraseNamesOneSet(t *testing.T) {
	t.Parallel()

	// THE SET ITSELF: trimmed, empty names dropped, one of each, sorted.
	if got, want := chat.EraseSet([]string{" m-2", "m-1", "", "m-2 ", "m-3", "m-1"}),
		[]string{"m-1", "m-2", "m-3"}; !slices.Equal(got, want) {
		t.Fatalf("EraseSet = %q, want %q", got, want)
	}
	// AND TWO ORDERS OF ONE ASK ARE ONE SET.
	if a, b := chat.EraseSet([]string{"b", "a"}), chat.EraseSet([]string{"a", "b", "a"}); !slices.Equal(a, b) {
		t.Fatalf("one ask in two orders named %q and %q", a, b)
	}

	// THE STORE PUBLISHES THE SET, not the list it was handed.
	r := newWriteRound(t, agents().withPeople("jane"))
	room := r.room(person("jane"), "launch")
	first := r.say(person("jane"), room.Channel.ID, "first", "k-1")
	second := r.say(person("jane"), room.Channel.ID, "second", "k-2")
	want := chat.EraseSet([]string{first.Message.ID, second.Message.ID})

	if _, err := r.store.Erase(t.Context(), viaToken("jane"), statelog.NewOpID(time.Now(), ""),
		room.Channel.ID, []string{want[1], want[0], want[1]}, "leaked"); err != nil {
		t.Fatalf("erase: %v", err)
	}
	payload, err := chat.DecodeMutation(r.lastRecord())
	if err != nil {
		t.Fatalf("decode the erase: %v", err)
	}
	erase, ok := payload.(chat.MessageErase)
	if !ok {
		t.Fatalf("the last record carries a %T, want the erase", payload)
	}
	if !slices.Equal(erase.MessageIDs, want) {
		t.Fatalf("the erase record names %q, want the set %q", erase.MessageIDs, want)
	}
}
