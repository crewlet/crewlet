package executor

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// EVERY HARNESS THAT READS NOTES CAN CONFIRM A TAKE, by one of three: its
// hooks' receipts on its main thread, its session record of the notes it
// admitted, or its plugin's own delivery event. One that reads notes but cannot
// confirm leaves its wakes unclaimed, run again as turns of their own, and the
// requester answered twice: such a harness takes no notes until it has one.
//
// Mutation: take a case out of confirms, or let a poll with none of them
// confirm, and a case goes red.
func TestEveryHarnessThatReadsNotesCanConfirmATake(t *testing.T) {
	t.Parallel()
	for _, h := range Registered() {
		if _, reads := h.(noteReader); reads && !confirms(h) {
			t.Errorf("%s reads notes but can confirm no take: give it hook receipts, a session record or a "+
				"plugin that confirms, or take its notes away", h.Name())
		}
	}
	for name, tc := range map[string]struct {
		h    Harness
		want bool
	}{
		"hooks whose thread shows receipts":  {ClaudeCode{}, true},
		"a poll its session record confirms": {recordedPoll{}, true},
		"a poll its plugin confirms":         {OpenCode{}, true},
		"hooks whose thread shows nothing":   {unconfirmedHooks{}, false},
		"a poll nothing confirms":            {unconfirmedPoll{}, false},
	} {
		if got := confirms(tc.h); got != tc.want {
			t.Errorf("%s: confirms = %v, want %v", name, got, tc.want)
		}
	}
}

// unconfirmedHooks is Claude Code's hooks with a thread that shows no receipts.
type unconfirmedHooks struct{ ClaudeCode }

func (unconfirmedHooks) threadTraits() sandbox.ThreadTraits { return sandbox.ThreadTraits{} }

// unconfirmedPoll is OpenCode's poll with a plugin that confirms nothing.
type unconfirmedPoll struct{ OpenCode }

func (unconfirmedPoll) pluginConfirms() bool { return false }

// recordedPoll is a poll whose session record shows the notes it admitted: one
// inbox id a line, a line counted only once whole (its break written).
type recordedPoll struct{ unconfirmedPoll }

// recordedPollFile is where recordedPoll's record lies in a box.
const recordedPollFile = "/notes.record"

func (recordedPoll) notesHeld(ctx context.Context, box sandbox.Sandbox, _ Session, inboxIDs []string) ([]string,
	error) {
	raw, err := box.ReadFile(ctx, recordedPollFile)
	if err != nil {
		return nil, err
	}
	whole := string(raw[:max(bytes.LastIndexByte(raw, '\n'), 0)])
	var held []string
	for _, line := range strings.Split(whole, "\n") {
		if slices.Contains(inboxIDs, line) {
			held = append(held, line)
		}
	}
	return held, nil
}

// A TAKE IS CONFIRMED OFF THE SESSION RECORD THROUGH THE ONE RECEIPTS PATH: a
// record confirmer's runner states Receipts, and its main-thread read holds
// the inbox ids the record shows whole. A torn last line holds nothing, an id
// the record does not show is not proved absent, and a record that cannot be
// read decides nothing, saying why.
//
// Mutation: read a torn line as held, call an unread id absent, or take an
// unreadable record for one holding nothing, and a case goes red.
func TestATakeIsConfirmedOffTheSessionRecordThroughTheReceipts(t *testing.T) {
	t.Parallel()
	runner := New(recordedPoll{})
	if !runner.ThreadTraits().Receipts {
		t.Fatal("a record confirmer's runner states no receipts, so no read asks it")
	}
	ask := sandbox.MainThreadAsk{Session: "s-1", Takes: []sandbox.AskedTake{{Take: "msg_held"}, {Take: "msg_torn"},
		{Take: "msg_never"}}}
	box := sandbox.NewFakeSandbox("box-record")
	if err := box.WriteFile(t.Context(), recordedPollFile, []byte("msg_other\nmsg_held\nmsg_torn")); err != nil {
		t.Fatal(err)
	}
	got := runner.ReadMainThread(t.Context(), box, ask).Receipts
	if !slices.Equal(got.Held, []string{"msg_held"}) || len(got.Restarted) > 0 || len(got.Absent) > 0 {
		t.Errorf("the record's receipts are %+v, want msg_held alone held and nothing absent", got)
	}
	unread := runner.ReadMainThread(t.Context(), sandbox.NewFakeSandbox("box-none"), ask).Receipts
	if len(unread.Held) > 0 || len(unread.Absent) > 0 || unread.Why == "" {
		t.Errorf("an unreadable record answered %+v, want nothing decided and why", unread)
	}
}
