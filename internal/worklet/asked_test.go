package worklet_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// fakeThreads answers one Thread read and keeps the query it was asked.
type fakeThreads struct {
	answer worklet.ResolvedThread
	err    error
	asked  []worklet.ThreadQuery
	fresh  []statelog.Freshness
}

func (f *fakeThreads) Thread(_ context.Context, q worklet.ThreadQuery,
	fresh statelog.Freshness) (worklet.ResolvedThread, error) {

	f.asked = append(f.asked, q)
	f.fresh = append(f.fresh, fresh)
	return f.answer, f.err
}

// ASKED-ON SEES ONLY AN OPEN ASK NAMING THEM.
//
// The probe is the one read behind the authority table's `asked` arm, which
// admits somebody without Worklet to an item and to answering on it. Over
// the real reader: nobody is asked on an item with no ask; the person an open
// ask names is; the asker and a bystander are not; a second open ask to the
// same person keeps them asked (the inference's ambiguity is MORE asked, not
// less); and once both are answered nobody is — open asks only. Over a fake,
// the shape of the read: the task and the author and nothing else, at the
// freshness the caller asked, and an error that is neither an answer nor an
// ambiguity is returned with false rather than read as "not asked". Mutation:
// answer true whenever the read succeeds and the bystander and answered cases
// go red; read ErrAmbiguousAnswer as false and the second ask case does.
func TestAskedOnSeesOnlyAnOpenAskNamingThem(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	fresh := statelog.Freshness{Level: statelog.ReadStale}
	assign(t, r, "asked", "dev")
	asked := func(handle string) bool {
		t.Helper()
		got, err := worklet.AskedOn(t.Context(), r.reader, "asked", handle, fresh)
		if err != nil {
			t.Fatalf("AskedOn(%s): %v", handle, err)
		}
		return got
	}
	if asked("legal") {
		t.Error("somebody is asked on an item nobody asked anything on")
	}

	askOn(t, r, "op-ask-1", "asked", worklet.Comment{
		ID: "c-ask-1", Task: "asked", Author: "dev", AuthorKind: worklet.AuthorAgent,
		Body: "may we ship this under the current licence?", Ask: "legal",
		CreatedAt: wednesday,
	})
	if !asked("legal") {
		t.Error("the person an open ask names is not asked")
	}
	for _, other := range []string{"dev", "finance"} {
		if asked(other) {
			t.Errorf("%s is asked on an item whose only ask names somebody else", other)
		}
	}

	askOn(t, r, "op-ask-2", "asked", worklet.Comment{
		ID: "c-ask-2", Task: "asked", Author: "dev", AuthorKind: worklet.AuthorAgent,
		Body: "and the bundled fonts?", Ask: "legal", CreatedAt: wednesday,
	})
	if !asked("legal") {
		t.Error("two open asks naming the same person left them not asked")
	}

	for i, ask := range []string{"c-ask-1", "c-ask-2"} {
		answers := ask
		askOn(t, r, fmt.Sprintf("op-answer-%d", i), "asked", worklet.Comment{
			ID: fmt.Sprintf("c-answer-%d", i), Task: "asked", Author: "legal",
			AuthorKind: worklet.AuthorHuman, Body: "yes", Answers: &answers,
			CreatedAt: wednesday,
		})
	}
	if asked("legal") {
		t.Error("a person whose every ask is answered is still asked — the " +
			"arm grants answering what is open, not the item for ever")
	}

	// THE SHAPE OF THE READ, and what each answer means.
	ambiguous := &worklet.ErrAmbiguousAnswer{Task: "t", Actor: "legal"}
	broken := errors.New("the store did not answer")
	for _, c := range []struct {
		name    string
		answer  worklet.ResolvedThread
		err     error
		want    bool
		wantErr error
	}{
		{"an inferred answer", worklet.ResolvedThread{Answers: "c-1"}, nil, true, nil},
		{"nothing inferred", worklet.ResolvedThread{}, nil, false, nil},
		{"several open asks", worklet.ResolvedThread{}, ambiguous, true, nil},
		{"several open asks, wrapped", worklet.ResolvedThread{},
			fmt.Errorf("read: %w", ambiguous), true, nil},
		{"an unreadable item", worklet.ResolvedThread{}, broken, false, broken},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			threads := &fakeThreads{answer: c.answer, err: c.err}
			want := statelog.Freshness{Level: statelog.ReadSession}
			got, err := worklet.AskedOn(t.Context(), threads, "t", "legal", want)
			if got != c.want || !errors.Is(err, c.wantErr) || (c.wantErr == nil && err != nil) {
				t.Fatalf("AskedOn = %v, %v; want %v, %v", got, err, c.want, c.wantErr)
			}
			if len(threads.asked) != 1 {
				t.Fatalf("read the thread %d times, want once", len(threads.asked))
			}
			if q := threads.asked[0]; q != (worklet.ThreadQuery{Task: "t", Author: "legal"}) {
				t.Errorf("asked %+v, want the task and the author alone", q)
			}
			if threads.fresh[0] != want {
				t.Errorf("read at %+v, want the caller's %+v", threads.fresh[0], want)
			}
		})
	}
	// NOBODY IS ASKED ON NOTHING, and nothing is read to say so.
	threads := &fakeThreads{answer: worklet.ResolvedThread{Answers: "c-1"}}
	for _, empty := range [][2]string{{"", "legal"}, {"t", ""}} {
		if got, err := worklet.AskedOn(t.Context(), threads, empty[0], empty[1],
			fresh); got || err != nil {
			t.Errorf("AskedOn(%q, %q) = %v, %v; want false", empty[0], empty[1], got, err)
		}
	}
	if len(threads.asked) != 0 {
		t.Errorf("an empty task or handle read the thread: %+v", threads.asked)
	}
}
