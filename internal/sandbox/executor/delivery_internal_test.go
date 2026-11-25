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
		"a poll its record of text confirms": {textPoll{}, true},
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

// recordedPoll is a poll whose session record shows the notes it admitted by
// id: one inbox id a line ([recordLines]).
type recordedPoll struct{ unconfirmedPoll }

func (recordedPoll) notesHeld(ctx context.Context, box sandbox.Sandbox, _ Session, takes []ServedTake,
	_ *summaryRead) ([]string, error) {
	lines, err := recordLines(ctx, box)
	if err != nil {
		return nil, err
	}
	var held []string
	for _, take := range takes {
		if slices.Contains(lines, take.InboxID) {
			held = append(held, take.InboxID)
		}
	}
	return held, nil
}

// textPoll is a poll whose session record shows the notes it admitted by
// their text alone: one item's text a line ([recordLines]), matched by its
// digest.
type textPoll struct{ unconfirmedPoll }

func (textPoll) notesHeld(ctx context.Context, box sandbox.Sandbox, _ Session, takes []ServedTake,
	_ *summaryRead) ([]string, error) {
	lines, err := recordLines(ctx, box)
	if err != nil {
		return nil, err
	}
	digests := make([]string, 0, len(lines))
	for _, line := range lines {
		digests = append(digests, ItemDigest(line))
	}
	var held []string
	for _, take := range takes {
		if slices.Contains(digests, take.Digest) {
			held = append(held, take.InboxID)
		}
	}
	return held, nil
}

// recordSummary is false: a test poll's record holds no summary, and its
// harness reads none.
func (recordedPoll) recordSummary(sandbox.MainThreadAsk) bool { return false }

func (textPoll) recordSummary(sandbox.MainThreadAsk) bool { return false }

// recordFile is where a test poll's record lies in a box.
const recordFile = "/notes.record"

// recordLines is the whole lines of the record in box, a line counted only
// once its break is written.
func recordLines(ctx context.Context, box sandbox.Sandbox) ([]string, error) {
	raw, err := box.ReadFile(ctx, recordFile)
	if err != nil {
		return nil, err
	}
	return strings.Split(string(raw[:max(bytes.LastIndexByte(raw, '\n'), 0)]), "\n"), nil
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
	if err := box.WriteFile(t.Context(), recordFile, []byte("msg_other\nmsg_held\nmsg_torn")); err != nil {
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

// A RECORD OF TEXT CONFIRMS A TAKE BY ITS ITEM'S DIGEST, the SHA-256 the poll
// recorded of the text it handed over: a line holding that very text confirms
// it, one a byte off does not, and a torn last line holds nothing.
//
// Mutation: match a record of text by inbox id, digest anything but the whole
// text, read a torn line as held, or change ItemDigest's hash or encoding, and
// a case goes red.
func TestATakeIsConfirmedByItsItemsDigestOffARecordOfText(t *testing.T) {
	t.Parallel()
	const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got := ItemDigest(""); got != emptySHA256 {
		t.Fatalf("ItemDigest of nothing is %q, want SHA-256 in hex, %q", got, emptySHA256)
	}
	ask := sandbox.MainThreadAsk{Session: "s-1", Takes: []sandbox.AskedTake{
		{Take: "msg_held", Digest: ItemDigest("the note held")},
		{Take: "msg_near", Digest: ItemDigest("the note near")},
		{Take: "msg_torn", Digest: ItemDigest("the note torn")},
	}}
	box := sandbox.NewFakeSandbox("box-text")
	record := "msg_near\nthe note held\nthe note neaR\nthe note torn"
	if err := box.WriteFile(t.Context(), recordFile, []byte(record)); err != nil {
		t.Fatal(err)
	}
	got := New(textPoll{}).ReadMainThread(t.Context(), box, ask).Receipts
	if !slices.Equal(got.Held, []string{"msg_held"}) || len(got.Absent) > 0 {
		t.Errorf("the record of text's receipts are %+v, want msg_held alone held and nothing absent", got)
	}
}

// servedOrder is a record confirmer that keeps the takes it was handed and
// holds every one of them.
type servedOrder struct{ handed *[]ServedTake }

func (s servedOrder) notesHeld(_ context.Context, _ sandbox.Sandbox, _ Session, takes []ServedTake,
	_ *summaryRead) ([]string, error) {
	*s.handed = takes
	held := make([]string, 0, len(takes))
	for _, take := range takes {
		held = append(held, take.InboxID)
	}
	return held, nil
}

func (servedOrder) recordSummary(sandbox.MainThreadAsk) bool { return false }

// A RECORD IS MATCHED AGAINST EVERY SERVE AND ANSWERS THE TAKES ASKED ALONE:
// the record confirmer is handed the job's serves in order, its read notes'
// in their places, and the receipts name only the takes asked; an ask naming
// no served order hands its takes.
//
// Mutation: hand the confirmer the asked takes alone, or answer a read note's
// take, and a case goes red.
func TestARecordIsMatchedAgainstEveryServeAndAnswersTheAskedAlone(t *testing.T) {
	t.Parallel()
	read := sandbox.AskedTake{Take: "msg_read", Digest: "d-1", Read: true}
	asked := sandbox.AskedTake{Take: "msg_asked", Digest: "d-1"}
	var handed []ServedTake
	got := recordReceipts(t.Context(), servedOrder{&handed}, sandbox.NewFakeSandbox("b"),
		sandbox.MainThreadAsk{Session: "s-1", Takes: []sandbox.AskedTake{asked}, Served: []sandbox.AskedTake{read, asked}},
		nil)
	if want := []ServedTake{{"msg_read", "d-1"}, {"msg_asked", "d-1"}}; !slices.Equal(handed, want) {
		t.Errorf("the confirmer was handed %+v, want %+v", handed, want)
	}
	if !slices.Equal(got.Held, []string{"msg_asked"}) {
		t.Errorf("the receipts hold %+v, want the asked take alone", got.Held)
	}
	recordReceipts(t.Context(), servedOrder{&handed}, sandbox.NewFakeSandbox("b"),
		sandbox.MainThreadAsk{Session: "s-1", Takes: []sandbox.AskedTake{asked}}, nil)
	if want := []ServedTake{{"msg_asked", "d-1"}}; !slices.Equal(handed, want) {
		t.Errorf("an ask naming no served order handed %+v, want %+v", handed, want)
	}
}
