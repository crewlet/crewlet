package engine

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/statelog"
)

// tokenDirectory is a data node's identity directory as a request's bearer
// reads it: one machine token's row under its id, one machine bound under its
// login, the vouch of the node holding them — or a directory that cannot be
// read.
type tokenDirectory struct {
	token credential.TokenRow
	bound iamdomain.Sighting
	vouch iamdomain.Vouch
	err   error
}

func (d tokenDirectory) PersonByLoginVouched(_ context.Context, login string) (
	iamdomain.Sighting, iamdomain.Vouch, error) {

	if d.err != nil {
		return iamdomain.Sighting{}, iamdomain.Vouch{}, d.err
	}
	if login != d.bound.Login {
		return iamdomain.Sighting{}, d.vouch, nil
	}
	return d.bound, d.vouch, nil
}

func (d tokenDirectory) SeatHolders(context.Context) ([]iamdomain.SeatHolder, error) {
	return nil, d.err
}

func (d tokenDirectory) SeatHolder(context.Context, string) (iamdomain.SeatHolder, bool, error) {
	return iamdomain.SeatHolder{}, false, d.err
}

func (d tokenDirectory) MachineToken(_ context.Context, id string) (credential.TokenRow, error) {
	if d.err != nil {
		return credential.TokenRow{}, d.err
	}
	if id != routedTokenID {
		return credential.TokenRow{Applied: d.token.Applied}, nil
	}
	return d.token, nil
}

// directoryCopy is a data node's copy serving whichever identity directory a
// case hands it — none at all for the zero copy.
type directoryCopy struct {
	mu  sync.Mutex
	dir estate.IdentityReader
}

func (c *directoryCopy) set(dir estate.IdentityReader) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dir = dir
}

func (c *directoryCopy) For(context.Context, string) (estate.Backend, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return estate.Backend{Identity: c.dir}, true
}

// statelessWithDirectory is a node holding no data whose router asks one data
// node, data-1, serving the directory a case hands served through the estate's
// own server — over a broker both share, under one keyring.
func statelessWithDirectory(t *testing.T, served *directoryCopy) *Engine {
	t.Helper()
	broker := memory.NewBroker()
	started := func() *memory.Queue {
		q := broker.Client()
		if err := q.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
		return q
	}
	envelopes := routerEnvelopes(t)
	stop, err := estate.Serve(t.Context(), started(), estate.ServerOptions{
		Self: "data-1", Local: served, Envelopes: envelopes,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.WithoutCancel(t.Context())) })
	router, err := estate.NewRouter(estate.RouterOptions{
		Self: "git-1", Queue: started(), Placement: oneDataNode{}, Git: noGitNodes{},
		Session: estate.NewSession(), Timing: estate.Production(), Envelopes: envelopes,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &Engine{router: router}
}

// routedTokenID is the machine token a data node's directory holds, minted at
// position 40 with routedTokenSecret.
const (
	routedTokenID     = "018f3a9c-0000-7000-8000-0000000000f1"
	routedTokenSecret = "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"
)

// routedNow is the instant a case checks a token at.
var routedNow = time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)

// routedToken is the row a good token is checked against: found, a token, the
// verifier its secret implies, live for a day and its owner active.
func routedToken() credential.TokenRow {
	return credential.TokenRow{
		Applied: 50, Found: true, IsToken: true,
		Verifier:  credential.TokenVerifier(routedTokenID, routedTokenSecret),
		ExpiresAt: routedNow.Add(24 * time.Hour),
		Grants:    []iam.Grant{iam.GrantWorklet},
		Epoch:     2,
		Owner: credential.TokenOwner{
			Found: true, ID: "018f3a9c-0000-7000-8000-00000000a11c", Kind: iam.KindPerson,
			Stage: iam.StageActive, Login: "sarah.chen", Grants: []iam.Grant{iam.GrantWorklet},
			Epoch: 2,
		},
	}
}

// A MACHINE TOKEN RESOLVES ON A NODE WITHOUT DATA — and a node that cannot say
// says so.
//
// A node without `data` holds no identity rows, so the row a presented machine
// token is checked against is a data node's, asked through the estate's
// router. A good token's row comes back with the stored verifier confirmed,
// and the one table decides it valid exactly as a data node would; a wrong
// secret's comes back with no verifier, and is refused as a data node refuses
// it. And a directory the data node cannot read, or a data node holding none,
// is an ERROR — the unknown arm, which the guard answers 503 — never the zero
// row, which the table reads as "no such token". Mutation: read a router
// error as no token (the zero row and no error) and the last two cases fail.
func TestAMachineTokenResolvesOnANodeWithoutData(t *testing.T) {
	t.Parallel()
	served := &directoryCopy{}
	e := statelessWithDirectory(t, served)
	stored := routedToken()
	served.set(tokenDirectory{token: stored})
	presented := credential.Token{ID: routedTokenID, Position: 40, Secret: routedTokenSecret}

	row, err := e.MachineToken(t.Context(), presented)
	if err != nil {
		t.Fatalf("a good token on a node without data: %v", err)
	}
	if row.Verifier != stored.Verifier {
		t.Fatalf("the row came back with verifier %q, want the stored one confirmed",
			row.Verifier)
	}
	if check := credential.CheckToken(presented, row, routedNow, statelog.StallGrace); check.Answer !=
		credential.TokenValid {
		t.Fatalf("a good token checked %s (%s), want valid", check.Answer, check.Detail)
	}

	wrong := presented
	wrong.Secret = strings.Repeat("ab", credential.TokenBytes)
	row, err = e.MachineToken(t.Context(), wrong)
	if err != nil || row.Verifier != "" {
		t.Fatalf("a wrong secret = (verifier %q, %v), want the row and no verifier",
			row.Verifier, err)
	}
	if check := credential.CheckToken(wrong, row, routedNow, statelog.StallGrace); check.Answer !=
		credential.TokenRefused {
		t.Fatalf("a wrong secret checked %s, want refused", check.Answer)
	}

	for name, dir := range map[string]estate.IdentityReader{
		"a directory the data node cannot read": tokenDirectory{err: errors.New("the replicated estate is not open")},
		"a data node holding no directory":      nil,
	} {
		served.set(dir)
		row, err = e.MachineToken(t.Context(), presented)
		if err == nil {
			t.Errorf("%s answered %+v and no error, so a node that cannot say is read "+
				"as one that said", name, row)
			continue
		}
		if !reflect.DeepEqual(row, credential.TokenRow{}) {
			t.Errorf("%s answered a row beside its error: %+v", name, row)
		}
	}
}

// A TIER A TOKEN'S BINDING RESOLVES ON A NODE WITHOUT DATA, by the rule its own
// rows would be weighed by: a data node's row for the token's login, narrowed
// to the binding, binds its seat; a bound row that node cannot vouch for is
// refused in the widening direction, as a data node refuses its own; and a
// directory it cannot read is unknown. Mutation: answer errNoDirectory on an
// engine without a core, the rule before a node without `data` could ask, and
// the first case fails; skip boundRowOf's vouch and the second does.
func TestATierATokensBindingResolvesOnANodeWithoutData(t *testing.T) {
	t.Parallel()
	served := &directoryCopy{}
	e := statelessWithDirectory(t, served)

	served.set(tokenDirectory{bound: boundMachine()})
	row, err := e.BoundSeat(t.Context(), "token:ops")
	if err != nil || !row.Found || row.Seat != "platform-lead" {
		t.Fatalf("a bound token on a node without data = (%+v, %v), want its seat", row, err)
	}

	served.set(tokenDirectory{bound: boundMachine(),
		vouch: iamdomain.Vouch{Lag: statelog.StallGrace + time.Second}})
	row, err = e.BoundSeat(t.Context(), "token:ops")
	if err == nil || row.Found {
		t.Fatalf("a binding the data node cannot vouch for = (%+v, %v), want unknown", row, err)
	}

	served.set(tokenDirectory{err: errors.New("disk")})
	row, err = e.BoundSeat(t.Context(), "token:ops")
	if err == nil || row.Found {
		t.Fatalf("a directory the data node cannot read = (%+v, %v), want unknown", row, err)
	}
}

// A NODE THAT CAN READ NO DIRECTORY CANNOT SAY: a nil engine, one built with
// neither a core nor a router, and a DATA node whose core is not up yet — which
// holds the directory it cannot read yet, so a peer's answer must not stand in
// for the rows it is about to apply. Each answers errNoDirectory, which the
// guard turns into 503.
func TestAnEngineThatCanReadNoDirectoryCannotSayWhoseTokenItIs(t *testing.T) {
	t.Parallel()
	served := &directoryCopy{}
	served.set(tokenDirectory{token: routedToken(), bound: boundMachine()})
	routed := statelessWithDirectory(t, served)
	presented := credential.Token{ID: routedTokenID, Position: 40, Secret: routedTokenSecret}
	var none *Engine
	for name, e := range map[string]*Engine{
		"a nil engine":                     none,
		"an engine with no core or router": {},
		"a data node whose core is not up": {router: routed.router, local: &localEstate{}},
	} {
		if _, err := e.MachineToken(t.Context(), presented); !errors.Is(err, errNoDirectory) {
			t.Errorf("%s: MachineToken = %v, want %v", name, err, errNoDirectory)
		}
		if _, err := e.BoundSeat(t.Context(), "token:ops"); !errors.Is(err, errNoDirectory) {
			t.Errorf("%s: BoundSeat = %v, want %v", name, err, errNoDirectory)
		}
	}
}
