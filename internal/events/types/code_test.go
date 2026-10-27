package types

import (
	"encoding/json"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
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
// it was sent, a server-side read by its kind and size.
func TestACodeReadsSummaryNamesTheRepositoryAndTheRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		read CodeFetched
		want string
	}{
		{CodeFetched{Principal: "pat:7", Seat: "ceo", RepoName: "platform", Via: FetchedViaGit, Bytes: 2048},
			"ceo fetched platform (2048 bytes)"},
		{CodeFetched{Principal: "dev", Seat: "dev", RepoName: "platform", Via: FetchedViaTool, Kind: "blob",
			Bytes: 900}, "dev read platform's blob (900 bytes)"},
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
		`"principal_kind":"system","principal":"expiry","via":"mirror","bytes":12}`
	var ev events.Event
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("decode: %v", err)
	}
	read, ok := events.DataAs[*CodeFetched](&ev)
	if !ok {
		t.Fatalf("decoded as %T", ev.Data)
	}
	if read.Via.Valid() || read.PrincipalKind.Valid() || read.Bytes != 12 {
		t.Errorf("decoded %+v; want the newer build's values, reported unknown", *read)
	}
}
