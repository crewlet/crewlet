package types

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// A READ OF CODE IS ITS READER'S ACT: the actor is the seat that read, or the
// person a credential is bound to, and a credential bound to nobody is named
// by itself — never the envelope's source, which a fetch leaves empty and
// which would otherwise attribute every clone to the engine.
func TestACodeReadIsItsReadersAct(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		read CodeFetched
		want string
	}{
		{"a person's credential names the person",
			CodeFetched{PrincipalKind: CodeReaderOperator, Principal: "pat:7", Seat: "ceo"}, "ceo"},
		{"a seat names itself",
			CodeFetched{PrincipalKind: CodeReaderAgent, Principal: "dev", Seat: "dev"}, "dev"},
		{"a token bound to nobody names the token",
			CodeFetched{PrincipalKind: CodeReaderOperator, Principal: "token:ci"}, "token:ci"},
	} {
		if got := actorOf(tc.read, "engine"); got != tc.want {
			t.Errorf("%s: actor = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A feed line names the repository and what was read of it: a fetch by what
// it was sent, a server-side read by its kind and size — and, where the record
// stands for more than one read, how many times.
func TestACodeReadsSummaryNamesTheRepositoryAndTheRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		read CodeFetched
		want string
	}{
		{CodeFetched{Principal: "pat:7", Seat: "ceo", RepoName: "platform", Via: FetchedViaGit, Reads: 1,
			Bytes: 2048}, "ceo fetched platform (2048 bytes)"},
		{CodeFetched{Principal: "dev", Seat: "dev", RepoName: "platform", Via: FetchedViaTool, Kind: "blob",
			Reads: 1, Bytes: 900}, "dev read platform's blob (900 bytes)"},
		{CodeFetched{Principal: "token:ci", RepoName: "platform", Via: FetchedViaGit, Reads: 6,
			Bytes: 12288}, "token:ci fetched platform 6 times (12288 bytes)"},
		{CodeFetched{Principal: "dev", Seat: "dev", RepoName: "platform", Via: FetchedViaTool, Kind: "blob",
			Reads: 3, Bytes: 2700}, "dev read platform's blob 3 times (2700 bytes)"},
	} {
		if got := summaryOf(tc.read, ""); got != tc.want {
			t.Errorf("%s: summary = %q, want %q", tc.read.Via, got, tc.want)
		}
	}
}

// AN UNKNOWN WAY OR KIND OF READER IS A VALUE, NOT A FAILURE: a newer build's
// value decodes, and Valid is what tells a reader it is not one of this
// build's.
func TestAnUnknownCodeReadVia(t *testing.T) {
	t.Parallel()
	for _, via := range CodeFetchedVias() {
		if !via.Valid() {
			t.Errorf("%q is this build's own and reads as invalid", via)
		}
	}
	for _, kind := range CodeReaderKinds() {
		if !kind.Valid() {
			t.Errorf("%q is this build's own and reads as invalid", kind)
		}
	}
	const raw = `{"id":"6f1c3d2e-0000-4000-8000-000000000041","type":"code_fetched",` +
		`"timestamp":"2026-09-24T09:00:00Z","source":"","repo_id":"r1","repo_name":"platform",` +
		`"principal_kind":"system","principal":"expiry","via":"mirror","reads":2,"bytes":12}`
	var ev events.Event
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("decode: %v", err)
	}
	read, ok := events.DataAs[*CodeFetched](&ev)
	if !ok {
		t.Fatalf("decoded as %T", ev.Data)
	}
	if read.Via.Valid() || read.PrincipalKind.Valid() || read.Reads != 2 || read.Bytes != 12 {
		t.Errorf("decoded %+v; want the newer build's values, reported unknown", *read)
	}
}

// A MIRROR PUSH'S ACCOUNT IS BOUNDED, AND COUNTS THE WHOLE PUSH: a push
// setting far more refs than an account names — a first push sets every ref
// a mirror covers, up to gitlet.MaxRefsPerRepo — each a name at
// gitref.MaxRefNameBytes made of the byte JSON escapes longest, beside as
// many deletions and branches held back, announces the first
// CodeMirrorPushedRefs of each list in ref order and every one in its count,
// and its whole event, at that worst, encodes two orders of magnitude under
// the queue's payload ceiling, where an account naming every ref of a first
// push is refused at its publish and lost.
//
// Mutation: keep every ref in the lists, and the event outgrows the bound;
// count the cut lists rather than the push, and the counts say sixteen.
func TestAMirrorPushsAccountIsBoundedAndCountsThePush(t *testing.T) {
	t.Parallel()
	const refs = 1_000 // a push batch's worth: some fifty times the bound, named whole
	name := strings.Repeat("<", 255-len("refs/tags/")-len("000000-"))
	oid := strings.Repeat("a", 64)
	var updated, deleted, waiting []string
	for i := range refs {
		ref := fmt.Sprintf("refs/tags/%06d-%s", i, name)
		updated = append(updated, ref+" "+oid)
		deleted = append(deleted, ref)
		waiting = append(waiting, ref)
	}
	ev := CodeMirrorPushed{RepoID: "r1", RepoName: "web", URL: "https://example.com/acme/web.git",
		As: "ana"}.WithRefs(updated, deleted, waiting)
	for _, list := range []struct {
		name        string
		got, whole  []string
		count, want int
	}{
		{"updated", ev.Updated, updated, ev.UpdatedCount, refs},
		{"deleted", ev.Deleted, deleted, ev.DeletedCount, refs},
		{"waiting", ev.Waiting, waiting, ev.WaitingCount, refs},
	} {
		if !slices.Equal(list.got, list.whole[:CodeMirrorPushedRefs]) || list.count != list.want {
			t.Errorf("%s names %d refs counting %d; want the first %d counting %d", list.name, len(list.got),
				list.count, CodeMirrorPushedRefs, list.want)
		}
	}
	raw, err := json.Marshal(events.New(ev, events.TraceContext{}))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if limit := queue.MaxPayloadBytes / 100; len(raw) > limit {
		t.Errorf("the account of a push of %d refs encodes to %d bytes; want at most %d, two orders of "+
			"magnitude under queue.MaxPayloadBytes", refs, len(raw), limit)
	}
	if got, want := ev.SummaryFor("web"), "1000 ref(s) set, 1000 deleted, 1000 waiting"; !strings.Contains(
		got, want) {
		t.Errorf("SummaryFor = %q; want it to say %q", got, want)
	}
}
