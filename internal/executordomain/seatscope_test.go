package executordomain_test

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/executorread"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A SEAT READS NO OTHER SEAT'S SESSION. A turn id is the fleet's, one row of
// one seat whoever minted it, so what holds a seat's reads to its own sessions
// is the seat every statement asks for beside it — the row's, the page's and
// the search's — and nothing else: no reader above it checks a row's seat
// again. Over the real write path, two seats each keeping a session — its
// object in the store, its turn ended, both asks holding the same words — ana's
// read of ben's turn finds no row, her page and her search list her own turn
// alone, and the one reader every surface reads a kept session by answers her
// read of ben's turn in the very words it answers a turn id nobody has, while
// her read of her own turn opens it.
//
// Mutation: read a turn's row by the turn and any seat, or page a seat's rows
// past its own, and a check goes red.
func TestASeatReadsNoOtherSeatsSession(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openExecutorLog(t), "n1")
	store, _ := sessionStore(t, "k1")
	turns := map[string]string{}
	for _, seat := range []string{"ana", "ben"} {
		turn := newTurnID()
		turns[seat] = turn
		put, err := store.Put(t.Context(), seat, turn, io.NopCloser(bytes.NewReader([]byte(seat+"'s session"))))
		if err != nil {
			t.Fatalf("keep %s's session: %v", seat, err)
		}
		capture := newCaptureOf(seat, "chat:"+seat, turn, 1, put.Object.Size, 64<<20)
		capture.Object, capture.ContentKey = put.Object, put.ContentKey
		n.recorded(capture)
		if _, err := n.writer.End(t.Context(), engineCall(), executordomain.NewEnd{Handle: seat, Turn: turn,
			ConversationKey: "chat:" + seat, Ask: "the zebra deploy, for " + seat,
			Ending: executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}}); err != nil {
			t.Fatalf("end %s's turn: %v", seat, err)
		}
	}
	reader, stale := n.reader(), statelog.Freshness{Level: statelog.ReadStale}

	if read, err := reader.Session(t.Context(), "ana", turns["ben"], stale); !errors.Is(err,
		executordomain.ErrNotFound) {
		t.Errorf("ana's read of ben's turn answered (%+v, %v), want no row", read.Session, err)
	}
	if read, err := reader.Session(t.Context(), "ana", turns["ana"], stale); err != nil ||
		read.Session.Turn != turns["ana"] || read.Session.Handle != "ana" {
		t.Fatalf("ana's read of her own turn answered (%+v, %v), want her row", read.Session, err)
	}
	page, err := reader.Sessions(t.Context(), "ana", "", 0, stale)
	if err != nil {
		t.Fatalf("ana's page: %v", err)
	}
	listed := make([]string, len(page.Sessions))
	for i, s := range page.Sessions {
		listed[i] = s.Turn
	}
	found, err := reader.Search(t.Context(), "ana", executordomain.SessionSearch{Query: "zebra deploy"}, stale)
	if err != nil {
		t.Fatalf("ana's search: %v", err)
	}
	searched := make([]string, len(found.Hits))
	for i, hit := range found.Hits {
		searched[i] = hit.Session.Turn
	}
	if want := []string{turns["ana"]}; !slices.Equal(listed, want) || !slices.Equal(searched, want) {
		t.Errorf("ana's page lists %v and her search finds %v, want her own turn alone, %v", listed,
			searched, want)
	}

	read := func(turn string) (executorread.Page, error) {
		return executorread.Read(t.Context(), executorread.Ask{Handle: "ana", Turn: turn, Fresh: stale,
			By: executor.ReadBySeat}, executorread.Source{Rows: reader, Objects: store})
	}
	stranger := newTurnID()
	_, nobodys := read(stranger)
	_, bens := read(turns["ben"])
	if !errors.Is(nobodys, executorread.ErrNoSession) || !errors.Is(bens, executorread.ErrNoSession) ||
		strings.ReplaceAll(bens.Error(), turns["ben"], "T") != strings.ReplaceAll(nobodys.Error(), stranger, "T") {
		t.Errorf("ana's read of ben's turn answered %v, and of a turn nobody has %v: want the same words",
			bens, nobodys)
	}
	if own, err := read(turns["ana"]); err != nil || own.Read.Session.Turn != turns["ana"] ||
		own.State == executorread.TranscriptNone {
		t.Errorf("ana's read of her own turn answered (%s, %v), want her session opened", own.State, err)
	}
}
