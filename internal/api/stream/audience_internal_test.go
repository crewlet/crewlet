package stream

import (
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// EVERY KIND SAYS WHO MAY READ IT, exactly once.
//
// A kind in neither table is received by nobody — the closed end, and a push
// that silently never arrives. A kind in both is two answers to one question.
// And a grant this build does not know is one no principal can hold, so it
// would be the same silent nothing as an absent row.
func TestEveryPushKindDeclaresWhoMayReadIt(t *testing.T) {
	t.Parallel()
	for _, kind := range slices.Sorted(maps.Keys(routes)) {
		grant, gated := needs[kind]
		switch {
		case gated && answers[kind]:
			t.Errorf("%q is both gated on %s and an answer to the socket's own "+
				"exchange; it is one or the other", kind, grant)
		case !gated && !answers[kind]:
			t.Errorf("%q declares no reader: add it to needs with the grant its "+
				"question takes, or to answers if it says nothing about the "+
				"company", kind)
		case gated && !grant.Valid():
			t.Errorf("%q is gated on %q, which no principal can hold", kind, grant)
		}
	}
	for kind := range needs {
		if _, routed := routes[kind]; !routed {
			t.Errorf("needs names %q, which is not a push kind", kind)
		}
	}
	for kind := range answers {
		if _, routed := routes[kind]; !routed {
			t.Errorf("answers names %q, which is not a push kind", kind)
		}
	}
}

// AN EVENT IS THE `events` QUESTION'S ROW, so it reaches only a reader who may
// ask that question — and the roster beside it reaches both.
func TestAnEventReachesOnlyAReaderHoldingAuditRead(t *testing.T) {
	t.Parallel()
	h := NewHub()
	defer h.Close()
	stateOnly := NewClient(AudienceOf([]iam.Grant{iam.GrantStateRead}))
	auditor := NewClient(AudienceOf([]iam.Grant{iam.GrantStateRead, iam.GrantAuditRead}))
	nobody := NewClient(Audience{})
	for _, c := range []*Client{stateOnly, auditor, nobody} {
		h.Register(c)
	}
	h.Broadcast(Push(KindEvent, map[string]any{"type": "agent_phase_completed"}, time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)))
	h.Broadcast(Push(KindAgents, []any{}, time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)))

	for _, tc := range []struct {
		name   string
		client *Client
		want   []string
	}{
		{"state:read alone", stateOnly, []string{KindAgents}},
		{"state:read and audit:read", auditor, []string{KindEvent, KindAgents}},
		{"the zero audience", nobody, nil},
	} {
		var got []string
		for _, f := range drainClient(tc.client) {
			got = append(got, f.kind)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s received %v, want %v", tc.name, got, tc.want)
		}
	}
}

// THE SNAPSHOT IS WHAT THE PUSHES WOULD BE, key by key.
//
// Every key the service builds names the push it stands in for, and a reader
// receives in the handshake exactly what it would receive afterwards.
func TestTheSnapshotCarriesOnlyWhatItsAudienceMayRead(t *testing.T) {
	t.Parallel()
	svc, err := NewService(livestate.New(), Options{
		Health:    func() Health { return Health{Status: "ok"} },
		Posture:   func(Health) FramePosture { return FrameLive },
		Handles:   func() map[string]string { return map[string]string{} },
		Roster:    func() []map[string]any { return nil },
		Org:       func() any { return map[string]any{} },
		Tools:     func() []map[string]any { return nil },
		Schedules: func() any { return []any{} },
		Chart:     authz.NoChart{},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Stop)

	every := svc.Snapshot(AudienceOf(iam.AllGrants))
	for key := range every {
		if _, ok := snapshotKinds[key]; !ok {
			t.Errorf("the snapshot builds %q, which names no push kind — no "+
				"reader can ever receive it", key)
		}
	}
	if len(every) != len(snapshotKinds) {
		t.Errorf("an audience holding every grant received %d of the %d "+
			"snapshot keys", len(every), len(snapshotKinds))
	}

	stateOnly := svc.Snapshot(AudienceOf([]iam.Grant{iam.GrantStateRead}))
	if _, ok := stateOnly["events"]; ok {
		t.Error("a state:read-only snapshot carries the event feed, which the " +
			"events question refuses without audit:read")
	}
	if _, ok := stateOnly["agents"]; !ok {
		t.Error("a state:read snapshot is missing the roster")
	}
	if got := svc.Snapshot(Audience{}); len(got) != 0 {
		t.Errorf("the zero audience received %v; it is the closed end", slices.Sorted(maps.Keys(got)))
	}
}

// drainClient reads everything currently queued for a client.
func drainClient(c *Client) []*Frame {
	var out []*Frame
	for {
		select {
		case frame, ok := <-c.Out():
			if !ok {
				return out
			}
			out = append(out, frame)
		default:
			return out
		}
	}
}

// A REPLY NEVER CARRIES A FACT ITS CLIENT MAY NOT READ, even from a path that
// forgot to build it for the client's audience: the direct path is gated by
// the same table as the broadcast, while the socket's own answers pass.
func TestAReplyNeverCarriesAFactItsClientMayNotRead(t *testing.T) {
	t.Parallel()
	c := NewClient(AudienceOf([]iam.Grant{iam.GrantStateRead}))
	c.Reply(Push(KindEvent, map[string]any{"type": "x"}, time.Now()))
	c.Reply(Envelope{Kind: KindResult, ID: 1, What: "anything"})
	var got []string
	for _, f := range drainClient(c) {
		got = append(got, f.kind)
	}
	if !slices.Equal(got, []string{KindResult}) {
		t.Fatalf("a state:read-only client was replied %v, want only its own answer", got)
	}
}
