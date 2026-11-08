package queries

import (
	"context"
	"errors"
	"testing"
	"time"
)

// historyFake is an access history: the SRE's access changed at changedAt,
// the PM's history cannot be read, and every read is counted.
type historyFake struct {
	changedAt time.Time
	reads     map[string]int
}

func (h *historyFake) AccessSince(_ context.Context, handle string) (func(time.Time) ([]string, bool), error) {
	h.reads[handle]++
	if handle == "pm" {
		return nil, errors.New("the access log is out of service")
	}
	return func(at time.Time) ([]string, bool) {
		if at.Before(h.changedAt) {
			return []string{"Cloudflare: your level is now Read (was Operate)."}, true
		}
		return nil, false
	}, nil
}

// A QUESTION ASKED BEFORE ITS AGENT'S ACCESS CHANGED IS NOTED, AND ONE ASKED
// AGAIN AFTER IT IS NOT. Each seat's history is read once per answer, however
// many of its questions the answer lists, and a history that cannot be read
// notes nothing rather than refusing the person their list.
//
// Mutation: note every question of a seat whose access changed and the one
// asked again carries the note it should have lost.
func TestAQuestionAskedBeforeAnAccessChangeIsNotedUntilAskedAgain(t *testing.T) {
	t.Parallel()
	changedAt := time.Date(2026, 11, 8, 9, 30, 0, 0, time.UTC)
	h := &historyFake{changedAt: changedAt, reads: map[string]int{}}
	changes := &accessChanges{history: h, read: map[string]func(time.Time) ([]string, bool){}}
	before := changes.of(t.Context(), "sre", changedAt.Add(-time.Hour))
	if before == nil || len(before.Lines) != 1 {
		t.Fatalf("a question asked before the change notes %+v", before)
	}
	if after := changes.of(t.Context(), "sre", changedAt.Add(time.Hour)); after != nil {
		t.Errorf("the question asked again after the change notes %+v", after)
	}
	if unread := changes.of(t.Context(), "pm", changedAt.Add(-time.Hour)); unread != nil {
		t.Errorf("a seat whose history cannot be read notes %+v", unread)
	}
	_ = changes.of(t.Context(), "pm", changedAt)
	if h.reads["sre"] != 1 || h.reads["pm"] != 1 {
		t.Errorf("the histories were read %v times, want once a seat", h.reads)
	}
	if none := (&accessChanges{}).of(t.Context(), "sre", changedAt); none != nil {
		t.Errorf("an answer with no history notes %+v", none)
	}
}
