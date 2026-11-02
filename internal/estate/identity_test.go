package estate

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/signedask"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// identityStream is the identity estate's log, as a floor names it.
var identityStream = floorStreams[identityDomain]

// directory is a data node's identity directory: one person holding one seat,
// one seat a removed person left, one machine token, and a lag and a deferral
// to vouch with.
type directory struct {
	err   error
	vouch iamdomain.Vouch

	// token is the row held under [machineID], or nil for [liveToken]'s.
	token *credential.TokenRow

	// tokenReads, where set, counts the machine-token rows read.
	tokenReads *atomic.Int64
}

// verifier is the secret-shaped value the row holds, which no answer may carry.
const verifier = "$argon2id$v=19$m=65536,t=3,p=1$c2FsdA$aGFzaA"

func (d directory) PersonByLoginVouched(_ context.Context, login string) (iamdomain.Sighting,
	iamdomain.Vouch, error) {
	if d.err != nil {
		return iamdomain.Sighting{}, iamdomain.Vouch{}, d.err
	}
	if login != "jane.doe" {
		return iamdomain.Sighting{}, d.vouch, nil
	}
	return iamdomain.Sighting{
		ID: "p-1", Kind: iam.KindPerson, Stage: iam.StageActive, Login: "jane.doe", Seat: "jane",
		Credentials: []iamdomain.Credential{{ID: "c-1", Method: iamdomain.MethodPassword,
			Verifier: verifier}},
		Grants: []iam.Grant{iam.GrantAdmin},
	}, d.vouch, nil
}

func (d directory) SeatHolders(context.Context) ([]iamdomain.SeatHolder, error) {
	if d.err != nil {
		return nil, d.err
	}
	return []iamdomain.SeatHolder{
		{Seat: "jane", Person: "p-1", Stage: iam.StageSuspended},
		{Seat: "sam", Person: "p-2", Removed: true},
	}, nil
}

// SeatHolder is the one seat's entry of [directory.SeatHolders], and nobody for
// a seat it does not list.
func (d directory) SeatHolder(ctx context.Context, seat string) (iamdomain.SeatHolder, bool, error) {
	holders, err := d.SeatHolders(ctx)
	if err != nil {
		return iamdomain.SeatHolder{}, false, err
	}
	for _, h := range holders {
		if h.Seat == seat {
			return h, true, nil
		}
	}
	return iamdomain.SeatHolder{}, false, nil
}

// MachineToken is the row held under id: [machineID]'s token, or the absent
// row — this node's applied position and vouch, and nothing found — for any
// other id, as the directory answers one.
func (d directory) MachineToken(_ context.Context, id string) (credential.TokenRow, error) {
	if d.tokenReads != nil {
		d.tokenReads.Add(1)
	}
	if d.err != nil {
		return credential.TokenRow{}, d.err
	}
	if id != machineID {
		return credential.TokenRow{Applied: tokenApplied, Lag: d.vouch.Lag,
			Deferred: d.vouch.Deferred}, nil
	}
	if d.token != nil {
		return *d.token, nil
	}
	return liveToken(), nil
}

// machineID is the one machine token the directory holds, minted at
// [tokenMinted] with the secret [machineSecret]; tokenApplied is how far the
// serving node has applied the identity log, past the mint.
const (
	machineID     = "018f3a9c-0000-7000-8000-0000000000f1"
	tokenMinted   = 40
	tokenApplied  = 50
	machineSecret = "5d2c0f3a9b8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c5b4a392817061f2e"
)

// tokenNow is the instant a case checks a token at.
var tokenNow = time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)

// presentedToken is the machine token as its holder presents it.
func presentedToken() credential.Token {
	return credential.Token{ID: machineID, Position: tokenMinted, Secret: machineSecret}
}

// liveToken is the row a good token is checked against: found, a token, the
// verifier its secret implies, live for a day, and its owner active.
func liveToken() credential.TokenRow {
	return credential.TokenRow{
		Applied: tokenApplied, Found: true, IsToken: true,
		Verifier:  credential.TokenVerifier(machineID, machineSecret),
		ExpiresAt: tokenNow.Add(24 * time.Hour),
		Grants:    []iam.Grant{iam.GrantWorklet},
		Epoch:     2,
		Owner: credential.TokenOwner{
			Found: true, ID: "018f3a9c-0000-7000-8000-00000000a11c", Kind: iam.KindPerson,
			Stage: iam.StageActive, Login: "sarah.chen", Grants: []iam.Grant{iam.GrantWorklet},
			Epoch: 2,
		},
	}
}

// A LOGIN'S BINDING CROSSES, AND ITS CREDENTIALS DO NOT: a node holding no data
// resolves whose record a login names from the row a data node answers, with
// how far that node could vouch for it — and nothing of the row's verifiers or
// grants reaches the wire, because the asker decides nothing on either and a
// verifier on the broker is a value an observer would want.
func TestABindingCrossesWithoutItsCredentials(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	vouch := iamdomain.Vouch{Lag: 3 * time.Second, Deferred: true}
	f.nodes["data-a"].set(func(n *fakeNode) { n.directory = directory{vouch: vouch} })

	binding, vouched, err := f.client.Identity().BindingByLogin(t.Context(), "jane.doe")
	if err != nil {
		t.Fatalf("binding: %v", err)
	}
	want := Binding{ID: "p-1", Kind: iam.KindPerson, Stage: iam.StageActive,
		Login: "jane.doe", Seat: "jane"}
	if binding != want || vouched != vouch {
		t.Fatalf("binding = (%+v, %+v), want (%+v, %+v)", binding, vouched, want, vouch)
	}

	// THE WIRE ITSELF, not the decoded answer: what the serving node sent.
	srv := server{self: "data-a", local: f.nodes["data-a"], env: fleetEnvelopes,
		now: time.Now, seen: signedask.NewGuard()}
	req, sealed := signed(t, fleetEnvelopes, request{Op: opIdentityBinding.spec.name,
		Args: json.RawMessage(`{"Login":"jane.doe"}`)})
	rep, why, err := fleetEnvelopes.openReply(srv.answer(t.Context(), laneShared, sealed), req.ID)
	if err != nil || why != "" || rep.Err != nil || rep.Unserved != "" {
		t.Fatalf("the binding was answered (%+v, %q, %v)", rep, why, err)
	}
	for _, leaked := range []string{verifier, string(iam.GrantAdmin), "Credentials", "Grants"} {
		if strings.Contains(string(rep.Result), leaked) {
			t.Errorf("the binding's answer carries %q: %s", leaked, rep.Result)
		}
	}

	// A LOGIN NOBODY HOLDS is the zero binding and no error, as the
	// directory answers it.
	if nobody, _, err := f.client.Identity().BindingByLogin(t.Context(), "nobody"); err != nil ||
		nobody != (Binding{}) {
		t.Fatalf("a login nobody holds = (%+v, %v), want the zero binding", nobody, err)
	}
}

// WHO HOLDS EACH SEAT CROSSES WHOLE, a removal's tombstone included: the party
// registry is rebuilt from it whole, and a removed holder read as "nobody"
// would hand their seat — still pointed at their own accounts — back to the
// chart.
func TestSeatHoldersCrossWhole(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	f.nodes["data-a"].set(func(n *fakeNode) { n.directory = directory{} })
	holders, err := f.client.Identity().SeatHolders(t.Context())
	if err != nil {
		t.Fatalf("seat holders: %v", err)
	}
	want, _ := directory{}.SeatHolders(t.Context())
	if !reflect.DeepEqual(holders, want) {
		t.Fatalf("seat holders = %+v, want %+v", holders, want)
	}
}

// ONE SEAT'S HOLDER CROSSES AS ITS OWN QUESTION — the seat named on the wire
// and the three answers kept apart: a holder at their stage, a removal's
// tombstone, and nobody. A turn composes the person it acts for from it on
// every tool call, so it asks the serving node about that seat alone, and an
// unreadable directory is UNKNOWN rather than a seat nobody holds, which the
// turn would read as one the chart alone speaks for.
func TestASeatHolderCrossesAsOneSeatsAnswer(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	f.nodes["data-a"].set(func(n *fakeNode) { n.directory = directory{} })
	for _, tc := range []struct {
		seat string
		want iamdomain.SeatHolder
		held bool
	}{
		{"jane", iamdomain.SeatHolder{Seat: "jane", Person: "p-1", Stage: iam.StageSuspended}, true},
		{"sam", iamdomain.SeatHolder{Seat: "sam", Person: "p-2", Removed: true}, true},
		{"nobody", iamdomain.SeatHolder{}, false},
	} {
		holder, held, err := f.client.Identity().SeatHolder(t.Context(), tc.seat)
		if err != nil || held != tc.held || holder != tc.want {
			t.Errorf("seat %s's holder = (%+v, %v, %v), want (%+v, %v)", tc.seat,
				holder, held, err, tc.want, tc.held)
		}
	}

	f.nodes["data-a"].set(func(n *fakeNode) {
		n.directory = directory{err: store.ErrNoEstate}
	})
	if holder, held, err := f.client.Identity().SeatHolder(t.Context(), "jane"); !errors.Is(err,
		store.ErrNoEstate) || held || holder != (iamdomain.SeatHolder{}) {
		t.Fatalf("an unreadable directory's holder = (%+v, %v, %v), want its error",
			holder, held, err)
	}
}

// A DIRECTORY THAT CANNOT BE READ IS UNKNOWN, NEVER EMPTY: "nobody is bound to
// anything" is a real reading, and an asker that took an outage for it would
// hand every suspended person's seat back to the chart. The error keeps its
// identity across the wire; a node whose core runtime is not up runs nothing,
// and the estate is unserved rather than empty.
func TestAnUnreadableDirectoryIsUnknownAndNeverEmpty(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	f.nodes["data-a"].set(func(n *fakeNode) {
		n.directory = directory{err: store.ErrNoEstate}
	})
	holders, err := f.client.Identity().SeatHolders(t.Context())
	if !errors.Is(err, store.ErrNoEstate) || holders != nil {
		t.Fatalf("an unreadable directory = (%+v, %v), want its error", holders, err)
	}

	f.nodes["data-a"].set(func(n *fakeNode) { n.directory = nil })
	_, _, err = f.client.Identity().BindingByLogin(t.Context(), "jane.doe")
	var unserved *ErrUnserved
	if !errors.As(err, &unserved) || !strings.Contains(unserved.Detail, "no native backend") {
		t.Fatalf("a node with no directory = %v, want the estate unserved", err)
	}
}

// A DIRECTORY READ WAITS FOR THE IDENTITY LOG, and for no other: a rebind this
// node has been told landed is not answered from before it, and a floor on the
// tracker's log — which no identity row is derived from — holds nothing.
func TestADirectoryReadIsFlooredOnTheIdentityLog(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	node := f.nodes["data-a"]
	node.set(func(n *fakeNode) { n.directory = directory{} })
	identity := statelog.Position{Stream: identityStream, Generation: 1, Seq: 12}
	f.client.Observe(identity)
	f.client.Observe(statelog.Position{Stream: trackerStream, Generation: 1, Seq: 40})

	if _, err := f.client.Identity().SeatHolders(t.Context()); err != nil {
		t.Fatalf("seat holders: %v", err)
	}
	node.mu.Lock()
	waited := slices.Clone(node.floors)
	node.mu.Unlock()
	if !slices.Equal(waited, []statelog.Position{identity}) {
		t.Fatalf("the directory read waited on %v, want only the identity log's %v",
			waited, identity)
	}

	// AND A NODE BEHIND IT SAYS SO rather than answer from before it — its
	// floor wait ends a reply's margin inside its asker's attempt
	// (statelog.WithReplyMargin), so it says so while the asker is still
	// listening.
	node.set(func(n *fakeNode) { n.behindOn = identityStream })
	_, _, err := f.client.Identity().BindingByLogin(t.Context(), "jane.doe")
	var unserved *ErrUnserved
	if !errors.As(err, &unserved) || !strings.Contains(unserved.Detail, "has not applied") {
		t.Fatalf("a directory read behind the identity floor = %v, want it unserved as behind", err)
	}
}
