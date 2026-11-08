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
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE COMPOSED INBOX (inbox.go's composedInbox): every native domain's notices
// in one list, whose own verb decides whose inbox it is and whose halves are
// each read under their own permission.

// inboxSources are the viewer fixture's sources with Gitlet beside the
// tracker: founder's inbox holds one notice of each, the tracker's at 9:10 and
// Gitlet's at 9:20.
func inboxSources(t *testing.T) (queries.Sources, *stubWork, *stubCode) {
	t.Helper()
	at := time.Date(2031, 4, 2, 9, 0, 0, 0, time.UTC)
	work := &stubWork{inbox: worklet.InboxAnswer{Notices: []worklet.InboxNotice{{RecordID: "t-1",
		LogStream: "CREWLET_TRACKER_LOG", LogGeneration: 1, LogSeq: 4, At: at.Add(10 * time.Minute),
		Reason: worklet.ReasonMention, Kind: worklet.ChangeKind("comment"), SubjectID: "task-1",
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
// reads their own inbox — Gitlet's notices of the changes they are
// asked about, interleaved with nothing they may not read — and asking for a
// colleague's is the lead relation's, refused to a peer.
//
// Mutation: register `inbox` behind the worklet permission, as `work_inbox`
// is — the person without it is refused their own inbox, red.
func TestTheInboxOpensToEverySignedInPerson(t *testing.T) {
	t.Parallel()
	s, _, host := inboxSources(t)
	page := composed(t, s, "ana")
	if got := sourcesOf(page); !slices.Equal(got, []string{"code"}) {
		t.Errorf("a person with no app read the sources %v, want Gitlet's alone", got)
	}
	if len(page.Notices) != 1 || page.Notices[0].Source != "code" || page.Notices[0].Subject.Key != "web!1" {
		t.Errorf("a person with no app reads %+v, want Gitlet's notice of web!1", page.Notices)
	}
	if len(host.asked) != 1 || host.asked[0].Inbox == nil || host.asked[0].Inbox.Handle != "ana" ||
		host.asked[0].Asker.Seat != "ana" {
		t.Errorf("Gitlet was asked %+v, want ana's inbox as ana", host.asked)
	}
	if _, err := askHolding(t, s, "ana", "inbox", map[string]any{"handle": "bo"}); !errors.Is(err,
		queries.ErrUnauthorized) {
		t.Errorf("a peer read bo's inbox = %v, want unauthorized", err)
	}
}

// WORKLET'S NOTICES NEED WORKLET: a person without the permission is asked
// nothing of Worklet — not even a page it then drops — and one holding it reads
// both domains interleaved by when each happened, Gitlet's newer notice
// first.
//
// Mutation: read Worklet's half for everybody the composed verb admits — the
// person without the permission reads Worklet's notice, red.
func TestWorkletNoticesNeedWorklet(t *testing.T) {
	t.Parallel()
	s, work, _ := inboxSources(t)
	composed(t, s, "ana")
	if work.inboxQuery.Handle != "" {
		t.Errorf("the tracker was asked %+v for a person without its app", work.inboxQuery)
	}
	s, work, _ = inboxSources(t)
	page := composed(t, s, "ana", iam.GrantWorklet)
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

// GITLET IS ASKED THE CARDS OF THE NOTICES A PAGE KEEPS, AND ONLY FOR A
// READ THAT DRAWS THEM: founder's Gitlet inbox holds two notices of web!1 and
// one of web!2, its page read at code log position 9. Read with `cards`, Gitlet
// is asked one `notice_cards` read of the two changes, each once, as the
// caller and no older than its page, and each notice's change is its card;
// read without, it is asked the inbox alone and the notices carry what the
// page said.
//
// Mutations: read the cards whatever `cards` says — the bare read asks them,
// red; floor the card read at the caller's own ask — it is asked with no
// floor, red; leave the card out of its notice — the page carries the bare
// change, red.
func TestGitletIsAskedTheCardsOfANoticesPageOnlyWhereDrawn(t *testing.T) {
	t.Parallel()
	at := time.Date(2031, 4, 2, 9, 0, 0, 0, time.UTC)
	page := statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: 9}
	notice := func(record, change, ref string, minute int) code.InboxNotice {
		return code.InboxNotice{RecordID: record, LogStream: topics.CodeLogStream, LogGeneration: 1,
			LogSeq: uint64(minute), At: at.Add(time.Duration(minute) * time.Minute),
			Reason: code.ReasonReviewRequested, Repo: "web", ChangeID: change, ChangeRef: ref,
			Change: &code.NoticeChange{Title: "bare " + ref}}
	}
	read := func(params map[string]any) (inbox.Page, *stubCode) {
		t.Helper()
		host := &stubCode{answer: func(q code.ReadQuery) (code.ReadAnswer, error) {
			switch q.Kind {
			case code.ReadInbox:
				return code.ReadAnswer{Level: q.Fresh.Level, Position: page, Inbox: &code.InboxPage{
					Handle: q.Inbox.Handle, Notices: []code.InboxNotice{notice("c-3", "change-1", "web!1", 3),
						notice("c-2", "change-2", "web!2", 2), notice("c-1", "change-1", "web!1", 1)}}}, nil
			case code.ReadNoticeCards:
				cards := map[string]*code.NoticeChange{}
				for _, id := range q.NoticeCards {
					cards[id] = &code.NoticeChange{Title: "card " + id, Body: "why " + id}
				}
				return code.ReadAnswer{Level: q.Fresh.Level, Position: page, NoticeCards: cards}, nil
			}
			return code.ReadAnswer{}, nil
		}}
		s := viewerSources(t, &stubWork{})
		s.Code = host
		got, err := askHolding(t, s, "ana", "inbox", params)
		if err != nil {
			t.Fatalf("inbox %v: %v", params, err)
		}
		composed, ok := got.(inbox.Page)
		if !ok || len(composed.Notices) != 3 {
			t.Fatalf("inbox %v answered %+v, want founder's three code notices", params, got)
		}
		return composed, host
	}

	composed, host := read(map[string]any{"cards": true})
	var kinds []code.ReadKind
	for _, q := range host.asked {
		kinds = append(kinds, q.Kind)
	}
	if want := []code.ReadKind{code.ReadInbox, code.ReadNoticeCards}; !slices.Equal(kinds, want) {
		t.Fatalf("a read drawing cards asked Gitlet %v, want %v", kinds, want)
	}
	cards := host.asked[1]
	if !slices.Equal(cards.NoticeCards, []string{"change-1", "change-2"}) || cards.Asker.Seat != "ana" ||
		!slices.Equal(cards.Fresh.Floors, []statelog.Position{page}) {
		t.Errorf("the cards were asked %+v, want web!1's and web!2's once each, as ana, no older than the "+
			"page at %s", cards, page)
	}
	for _, n := range composed.Notices {
		said, ok := n.Extra.(code.InboxNotice)
		if !ok || said.Change == nil || said.Change.Body != "why "+said.ChangeID ||
			n.Subject.Title != "card "+said.ChangeID {
			t.Errorf("the notice %s carries %+v titled %q, want its change's card", n.RecordID, n.Extra,
				n.Subject.Title)
		}
	}

	composed, host = read(nil)
	if len(host.asked) != 1 || host.asked[0].Kind != code.ReadInbox {
		t.Errorf("a read drawing no cards asked Gitlet %+v, want its inbox alone", host.asked)
	}
	for _, n := range composed.Notices {
		said, ok := n.Extra.(code.InboxNotice)
		if !ok || said.Change == nil || said.Change.Body != "" || n.Subject.Title != "bare "+said.ChangeRef {
			t.Errorf("the notice %s of a read drawing no cards carries %+v, want what its page said", n.RecordID,
				n.Extra)
		}
	}
}
