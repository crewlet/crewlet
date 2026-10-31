package queries_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/tracker"
)

// THE COMPOSED INBOX (inbox.go's composedInbox): every native domain's notices
// in one list, whose own verb decides whose inbox it is and whose halves are
// each read under their own permission.

// inboxSources are the viewer fixture's sources with a code host beside the
// tracker: founder's inbox holds one notice of each, the tracker's at 9:10 and
// the code host's at 9:20.
func inboxSources(t *testing.T) (queries.Sources, *stubWork, *stubCode) {
	t.Helper()
	at := time.Date(2031, 4, 2, 9, 0, 0, 0, time.UTC)
	work := &stubWork{inbox: tracker.InboxAnswer{Notices: []tracker.InboxNotice{{RecordID: "t-1",
		LogStream: "CREWLET_TRACKER_LOG", LogGeneration: 1, LogSeq: 4, At: at.Add(10 * time.Minute),
		Reason: tracker.ReasonMention, Kind: tracker.ChangeKind("comment"), SubjectID: "task-1",
		SubjectKey: "ENG-1"}}}}
	host := &stubCode{answer: func(q code.ReadQuery) (code.ReadAnswer, error) {
		return code.ReadAnswer{Level: q.Fresh.Level, Inbox: &code.InboxPage{Handle: q.Inbox.Handle,
			Notices: []code.InboxNotice{{RecordID: "c-1", LogStream: "CREWLET_CODE_LOG", LogGeneration: 1,
				LogSeq: 9, At: at.Add(20 * time.Minute), Reason: code.ReasonReviewRequested, Repo: "web",
				ChangeID: "change-1", ChangeRef: "web!1", Change: &code.NoticeChange{Title: "Retry once"}}}}}, nil
	}}
	s := viewerSources(t, work)
	s.Code = host
	return s, work, host
}

// composed is handle's composed inbox as they read it holding grants.
func composed(t *testing.T, s queries.Sources, handle string, grants ...iam.Grant) inbox.Page {
	t.Helper()
	got, err := askHolding(t, s, handle, "inbox", nil, grants...)
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	page, ok := got.(inbox.Page)
	if !ok {
		t.Fatalf("inbox answered %T, want an inbox.Page", got)
	}
	return page
}

// sourcesOf is the names of the sources a page read.
func sourcesOf(p inbox.Page) []string {
	var out []string
	for _, s := range p.Sources {
		out = append(out, s.Name)
	}
	return out
}

// THE INBOX OPENS TO EVERY SIGNED-IN PERSON: a person holding no app at all
// reads their own inbox — the code host's notices of the changes they are
// asked about, interleaved with nothing they may not read — and asking for a
// colleague's is the lead relation's, refused to a peer.
//
// Mutation: register `inbox` behind the tracker app, as `work_inbox` is — the
// person without it is refused their own inbox, red.
func TestTheInboxOpensToEverySignedInPerson(t *testing.T) {
	t.Parallel()
	s, _, host := inboxSources(t)
	page := composed(t, s, "ana")
	if got := sourcesOf(page); !slices.Equal(got, []string{"code"}) {
		t.Errorf("a person with no app read the sources %v, want the code host's alone", got)
	}
	if len(page.Notices) != 1 || page.Notices[0].Source != "code" || page.Notices[0].Subject.Key != "web!1" {
		t.Errorf("a person with no app reads %+v, want the code host's notice of web!1", page.Notices)
	}
	if len(host.asked) != 1 || host.asked[0].Inbox == nil || host.asked[0].Inbox.Handle != "ana" ||
		host.asked[0].Asker.Seat != "ana" {
		t.Errorf("the code host was asked %+v, want ana's inbox as ana", host.asked)
	}
	if _, err := askHolding(t, s, "ana", "inbox", map[string]any{"handle": "bo"}); !errors.Is(err,
		queries.ErrUnauthorized) {
		t.Errorf("a peer read bo's inbox = %v, want unauthorized", err)
	}
}

// TRACKER NOTICES NEED THE TRACKER APP: a person without it is asked nothing
// of the tracker — not even a page it then drops — and one holding it reads
// both domains interleaved by when each happened, the code host's newer notice
// first.
//
// Mutation: read the tracker's half for everybody the composed verb admits —
// the person without the app reads the tracker's notice, red.
func TestTrackerNoticesNeedTheTrackerApp(t *testing.T) {
	t.Parallel()
	s, work, _ := inboxSources(t)
	composed(t, s, "ana")
	if work.inboxQuery.Handle != "" {
		t.Errorf("the tracker was asked %+v for a person without its app", work.inboxQuery)
	}
	s, work, _ = inboxSources(t)
	page := composed(t, s, "ana", iam.GrantTracker)
	if work.inboxQuery.Handle != "ana" {
		t.Errorf("the tracker was asked for %q's inbox, want ana's", work.inboxQuery.Handle)
	}
	var got []string
	for _, n := range page.Notices {
		got = append(got, n.Source+":"+n.RecordID)
	}
	if want := []string{"code:c-1", "tracker:t-1"}; !slices.Equal(got, want) {
		t.Errorf("a person with the tracker reads %v, want %v", got, want)
	}
	if want := []string{"tracker", "code"}; !slices.Equal(sourcesOf(page), want) {
		t.Errorf("the page read the sources %v, want %v", sourcesOf(page), want)
	}
}
