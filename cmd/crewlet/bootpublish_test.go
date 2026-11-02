package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
)

// THE BOOT PUBLISH IS A TABLE, and every row of it is held here: the request
// each set of facts makes — what it publishes, the compare-and-set it publishes
// under, the instant it asks for and whether it restores or chooses — or that it
// makes none.
//
// Mutation: delete any one rule's guard in [decideBootPublish] and a row below
// lands on another rule.
func TestTheBootPublishDecision(t *testing.T) {
	t.Parallel()
	t1 := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)
	t3 := t2.Add(time.Hour)
	now := t3.Add(time.Hour)
	planned := t1.Add(30 * time.Minute)
	const epoch = 7

	p := store.Revision{ID: "p", ActivatedAt: t1}
	r := store.Revision{ID: "r", ActivatedAt: t2}
	landed := store.Revision{ID: "o", ActivatedAt: planned}
	unconfirmed := store.Revision{ID: "x", ActivatedAt: planned}
	never := store.Revision{ID: "q"}
	offer := func(id, base string, plannedAt time.Time) store.Revision {
		return store.Revision{ID: id, Offer: &store.Offer{Base: base, PlannedAt: plannedAt}}
	}
	// over is an offer built on the base and on an offer an earlier start
	// sent and never confirmed.
	over := func(id, base, unconfirmed string) store.Revision {
		return store.Revision{ID: id, Offer: &store.Offer{Base: base, Unconfirmed: unconfirmed}}
	}
	pointer := func(id string, at time.Time) coord.Activation {
		return coord.Activation{RevisionID: id, At: at, Epoch: epoch}
	}
	restored := func(id string, at time.Time) coord.Activation {
		a := pointer(id, at)
		a.Restored = true
		return a
	}

	for _, tc := range []struct {
		name  string
		facts bootFacts
		want  bootPublish
	}{
		{
			name: "1b: an offer the pointer names at its planned instant landed before a crash",
			facts: bootFacts{boot: offer("o", "p", planned), fleets: &p,
				target: pointer("o", planned), pointed: true, targetHeld: true},
			want: bootPublish{rule: ruleOfferLanded, confirm: true},
		},
		{
			// The pointer has moved past the offer, and this node's
			// reconciler saw it name the offer before it did: the
			// publish landed, and only its confirmation was lost.
			name: "1b: an offer the fleet's row names landed, however far the pointer moved since",
			facts: bootFacts{boot: offer("o", "p", planned), fleets: &landed,
				target: pointer("w", t3), pointed: true, targetHeld: true},
			want: bootPublish{rule: ruleOfferLanded, confirm: true},
		},
		{
			// #318: the pointer names the offer because the offer IS the
			// fleet's revision, re-activated. Nothing planned, so nothing
			// landed: it is published, and mints an epoch.
			name: "1a: a re-activation of the pointer's own revision is published",
			facts: bootFacts{boot: offer("p", "p", time.Time{}), fleets: &p,
				target: pointer("p", t1), pointed: true, targetHeld: true},
			want: bootPublish{rule: ruleOfferOverBase, publish: offer("p", "p", time.Time{}),
				offer: true, over: "p", expect: "p", expectEpoch: epoch, at: now},
		},
		{
			name: "1a: a plan the pointer never carried is published again",
			facts: bootFacts{boot: offer("p", "p", planned), fleets: &p,
				target: pointer("p", t1), pointed: true, targetHeld: true},
			want: bootPublish{rule: ruleOfferOverBase, publish: offer("p", "p", planned),
				offer: true, over: "p", expect: "p", expectEpoch: epoch, at: now},
		},
		{
			name: "1a: an offer over the pointer's revision is published over it",
			facts: bootFacts{boot: offer("o", "p", time.Time{}), fleets: &p,
				target: pointer("p", t1), pointed: true, targetHeld: true},
			want: bootPublish{rule: ruleOfferOverBase, publish: offer("o", "p", time.Time{}),
				offer: true, over: "p", expect: "p", expectEpoch: epoch, at: now},
		},
		{
			name: "1a: a clock behind the pointer still publishes later than it",
			facts: bootFacts{boot: offer("o", "p", time.Time{}), fleets: &p,
				target: pointer("p", now.Add(time.Hour)), pointed: true, targetHeld: true},
			want: bootPublish{rule: ruleOfferOverBase, publish: offer("o", "p", time.Time{}),
				offer: true, over: "p", expect: "p", expectEpoch: epoch,
				at: now.Add(time.Hour + time.Millisecond)},
		},
		{
			// #6: an earlier start sent x and died before confirming it,
			// and an offline command built y on it. x landed, so y goes
			// over x — the base alone would withhold a change made on
			// the very company the fleet runs.
			name: "1a: an offer over an unconfirmed one the fleet took is published over it",
			facts: bootFacts{boot: over("y", "p", "x"), fleets: &p,
				target: pointer("x", planned), pointed: true, targetHeld: true},
			want: bootPublish{rule: ruleOfferOverBase, publish: over("y", "p", "x"),
				offer: true, over: "x", expect: "x", expectEpoch: epoch, at: now},
		},
		{
			name: "1a: an offer over an unconfirmed one the fleet never took is published over the base",
			facts: bootFacts{boot: over("y", "p", "x"), fleets: &p,
				target: pointer("p", t1), pointed: true, targetHeld: true},
			want: bootPublish{rule: ruleOfferOverBase, publish: over("y", "p", "x"),
				offer: true, over: "p", expect: "p", expectEpoch: epoch, at: now},
		},
		{
			// Somebody chose the base again after the fleet moved past
			// it: the company is the base, so the change built on it is
			// published.
			name: "1a: a base chosen again after the fleet moved past it",
			facts: bootFacts{boot: offer("o", "p", time.Time{}), fleets: &r,
				target: pointer("p", t3), pointed: true, targetHeld: true},
			want: bootPublish{rule: ruleOfferOverBase, publish: offer("o", "p", time.Time{}),
				offer: true, over: "p", expect: "p", expectEpoch: epoch, at: now},
		},
		{
			name: "1c: an offer over a revision the fleet moved on from is withheld",
			facts: bootFacts{boot: offer("o", "p", time.Time{}), fleets: &r,
				target: pointer("r", t2), pointed: true, targetHeld: true},
			want: bootPublish{rule: ruleOfferWithheld, withheld: true},
		},
		{
			name: "1c: an offer under a restore newer than the fleet's row is withheld",
			facts: bootFacts{boot: offer("o", "p", time.Time{}), fleets: &r,
				target: restored("w", t3), pointed: true, targetHeld: true},
			want: bootPublish{rule: ruleOfferWithheld, withheld: true},
		},
		{
			name:  "1d: an offer on a node that never saw a pointer is published create-only",
			facts: bootFacts{boot: offer("o", "", time.Time{})},
			want: bootPublish{rule: ruleOfferFirst, publish: offer("o", "", time.Time{}),
				offer: true, expectAbsent: true, at: now},
		},
		{
			name:  "1d: an offer over the fleet's last revision, with the pointer gone",
			facts: bootFacts{boot: offer("o", "p", time.Time{}), fleets: &p},
			want: bootPublish{rule: ruleOfferFirst, publish: offer("o", "p", time.Time{}),
				offer: true, over: "p", expectAbsent: true, at: now},
		},
		{
			name:  "1d: an offer over the unconfirmed one the fleet's row names, with the pointer gone",
			facts: bootFacts{boot: over("y", "p", "x"), fleets: &unconfirmed},
			want: bootPublish{rule: ruleOfferFirst, publish: over("y", "p", "x"),
				offer: true, over: "x", expectAbsent: true, at: now},
		},
		{
			name:  "1e: with the pointer gone, a fleet that moved on is restored and the offer withheld",
			facts: bootFacts{boot: offer("o", "p", time.Time{}), fleets: &r},
			want: bootPublish{rule: ruleOfferRestore, withheld: true, publish: r, restore: true,
				expectAbsent: true, at: t2},
		},
		{
			// #0: another node restored the base this node saw the fleet
			// move past. The base is not the company, so the change built
			// on it is not published; the fleet's revision is put back.
			name: "1e: a restore of the offer's base the fleet moved past is replaced",
			facts: bootFacts{boot: offer("o", "p", time.Time{}), fleets: &r,
				target: restored("p", t1), pointed: true, targetHeld: true},
			want: bootPublish{rule: ruleOfferRestore, withheld: true, publish: r, restore: true,
				expect: "p", expectEpoch: epoch, at: t2},
		},
		{
			name:  "2a: a lost pointer is restored from the fleet's revision, not the boot row",
			facts: bootFacts{boot: p, fleets: &r},
			want:  bootPublish{rule: ruleRestore, publish: r, restore: true, expectAbsent: true, at: t2},
		},
		{
			name:  "2a: a node that never saw a pointer restores what it boots on",
			facts: bootFacts{boot: p},
			want:  bootPublish{rule: ruleRestore, publish: p, restore: true, expectAbsent: true, at: t1},
		},
		{
			name:  "2a: a revision nothing activated is restored now",
			facts: bootFacts{boot: never},
			want: bootPublish{rule: ruleRestore, publish: never, restore: true,
				expectAbsent: true, at: now},
		},
		{
			name: "2b: a restore older than the fleet's revision is replaced",
			facts: bootFacts{boot: p, fleets: &r, target: restored("p", t1), pointed: true,
				targetHeld: true},
			want: bootPublish{rule: ruleRestoreNewer, publish: r, restore: true, expect: "p",
				expectEpoch: epoch, at: t2},
		},
		{
			// #4: instants come from writers' clocks. A choice whose
			// writer's clock ran behind reads older, and is still the
			// company.
			name: "2c: a chosen pointer older than the fleet's revision is the company",
			facts: bootFacts{boot: p, fleets: &r, target: pointer("p", t1), pointed: true,
				targetHeld: true},
			want: bootPublish{rule: ruleFleetPublishes},
		},
		{
			// #3: the restore names the revision this node knows, at an
			// older instant. Replacing it would publish the same company.
			name: "2c: a restore of the fleet's own revision at an older instant",
			facts: bootFacts{boot: p, fleets: &r, target: restored("r", t1), pointed: true,
				targetHeld: true},
			want: bootPublish{rule: ruleFleetPublishes},
		},
		{
			name:  "2c: an older restore naming a revision this node never held is the fleet's",
			facts: bootFacts{boot: p, fleets: &r, target: restored("w", t1), pointed: true},
			want:  bootPublish{rule: ruleFleetPublishes},
		},
		{
			name: "2c: a restore newer than the fleet's revision this node saw is the fleet's",
			facts: bootFacts{boot: p, fleets: &r, target: restored("w", t3), pointed: true,
				targetHeld: true},
			want: bootPublish{rule: ruleFleetPublishes},
		},
		{
			name: "2c: a pointer naming the fleet's revision this node saw",
			facts: bootFacts{boot: p, fleets: &r, target: pointer("r", t2), pointed: true,
				targetHeld: true},
			want: bootPublish{rule: ruleFleetPublishes},
		},
		{
			name:  "a node with nothing to boot on publishes nothing",
			facts: bootFacts{target: pointer("w", t1), pointed: true},
			want:  bootPublish{rule: ruleNothingToBoot},
		},
	} {
		facts := tc.facts
		facts.now = now
		if got := decideBootPublish(facts); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s:\n got %+v\nwant %+v", tc.name, got, tc.want)
		}
	}
}

// A CRASH AFTER THE BOOT'S PUBLISH LANDED IS CONFIRMED, NOT PUBLISHED AGAIN.
//
// The node planned the instant, the pointer took the offer at it, and the node
// died before recording that it had. Its next boot finds the pointer naming its
// own offer at the very instant it planned: a publish now would mint a second
// epoch for one change, and withholding it — the pointer no longer names the
// base — would leave a change the fleet runs marked as one it does not.
//
// Mutation: drop rule 1b and the offer is withheld as one the fleet moved on
// from.
func TestACrashAfterTheBootPublishLandsConfirmsInsteadOfPublishingAgain(t *testing.T) {
	t.Parallel()
	db := seedStore(t)
	fleet := coordmemory.NewFleet()
	if err := bootSeed(t, db, fleet, seedOf(parse(t, companyYAML)), fixtureCipher); err != nil {
		t.Fatalf("seed: %v", err)
	}
	offered := offlineImport(t, db, strings.Replace(companyYAML, "name: Acme",
		"name: Acme Offline", 1))
	base, _, err := fleet.Target(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	planned := coord.ActivationAt(time.Now().UTC().Truncate(time.Microsecond), base.At)
	if err := db.Configs().PlanOffer(t.Context(), offered, planned, base.RevisionID); err != nil {
		t.Fatalf("plan: %v", err)
	}
	booted := bootRow(t, db)
	landed, err := fleet.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: offered, Summary: booted.Summary, Payload: booted.Payload, At: planned,
		Origin: originOf(booted), Expect: base.RevisionID,
	})
	if err != nil {
		t.Fatalf("the publish before the crash: %v", err)
	}

	if err := bootOnly(t, db, fleet, quiet()); err != nil {
		t.Fatalf("boot: %v", err)
	}
	assertFleetOn(t, fleet, landed)
	if after := bootRow(t, db); after.ID != offered || after.Offer != nil {
		t.Errorf("this node boots on %s with offer %+v, want %s confirmed as the "+
			"fleet's", after.ID, after.Offer, offered)
	}
}

// RE-ACTIVATING THE REVISION THIS NODE BOOTS ON, OFFLINE, MINTS AN EPOCH (#318).
//
// `crewlet config activate <the company's own revision>` is how an operator
// asks the fleet to re-resolve its ${VAR} references after a credential was
// rotated. It used to be published only when this node's clock put it later
// than the pointer, so after a peer whose clock ran ahead had activated last,
// the gesture did nothing at all and said nothing. It is published over the
// pointer now, later than it whatever this node's clock says — and a plan an
// earlier boot made that the pointer never carried is published too.
//
// Mutation: match rule 1b by the revision's id alone, and the offer is
// confirmed as landed with no epoch minted.
func TestReactivatingTheBootRevisionOfflineMintsAnEpoch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		planned bool
	}{
		{"a re-activation", false},
		{"a re-activation an earlier boot planned and never published", true},
	} {
		db := seedStore(t)
		fleet := coordmemory.NewFleet()
		if err := bootSeed(t, db, fleet, seedOf(parse(t, companyYAML)), fixtureCipher); err != nil {
			t.Fatalf("%s: seed: %v", tc.name, err)
		}
		company := bootRow(t, db)
		// A PEER WHOSE CLOCK RUNS TWO HOURS AHEAD activated it last.
		ahead, err := fleet.Activate(t.Context(), coord.ActivationRequest{
			RevisionID: company.ID, Payload: company.Payload,
			At: time.Now().Add(2 * time.Hour), Origin: originOf(company), Expect: company.ID,
		})
		if err != nil {
			t.Fatalf("%s: peer activate: %v", tc.name, err)
		}
		applies(t, db, fleet, fixtureCipher)

		cs := &configStore{configs: db.Configs(), cipher: fixtureCipher, boot: &config.Bootstrap{}}
		if err := activateRevision(t.Context(), cs, company.ID, io.Discard); err != nil {
			t.Fatalf("%s: config activate: %v", tc.name, err)
		}
		if tc.planned {
			if err := db.Configs().PlanOffer(t.Context(), company.ID,
				ahead.At.Add(time.Minute), company.ID); err != nil {
				t.Fatalf("%s: plan: %v", tc.name, err)
			}
		}
		if err := bootOnly(t, db, fleet, quiet()); err != nil {
			t.Fatalf("%s: boot: %v", tc.name, err)
		}
		after, _, err := fleet.Target(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if after.RevisionID != company.ID || after.Epoch <= ahead.Epoch ||
			!after.At.After(ahead.At) {
			t.Errorf("%s: the pointer names %s at epoch %d and %s, want %s re-activated "+
				"at an epoch after %d and later than %s", tc.name, after.RevisionID,
				after.Epoch, after.At, company.ID, ahead.Epoch, ahead.At)
		}
		if booted := bootRow(t, db); booted.Offer != nil {
			t.Errorf("%s: the published re-activation is still an offer: %+v",
				tc.name, booted.Offer)
		}
	}
}

// AN OFFLINE CHANGE IS NEVER PUBLISHED OVER A FLEET THAT MOVED ON (#321).
//
// The change was made on a node that was behind: built on A while the fleet
// ran R. It used to be published at the next start whenever this node's clock
// put it later than the pointer, and every node rolled back to the stale A
// underneath it, plus the edit. It is withheld now, said at warn on every boot
// it stands — a change somebody made that the fleet is not running is not
// something to find out by accident — and kept, as what this node runs until
// it applies the fleet's revision, which then replaces it.
//
// Mutation: publish on rule 1c, or say it only once, and the case fails.
func TestAnOfflineChangeIsNeverPublishedOverAFleetThatMovedOn(t *testing.T) {
	t.Parallel()
	db := seedStore(t)
	fleet := coordmemory.NewFleet()
	if err := bootSeed(t, db, fleet, seedOf(parse(t, companyYAML)), fixtureCipher); err != nil {
		t.Fatalf("seed: %v", err)
	}
	built := bootRow(t, db)
	offered := offlineImport(t, db, strings.Replace(companyYAML, "name: Acme",
		"name: Acme Offline", 1))
	moved := activateAsPeer(t, fleet, "peer-revision", []byte(`{"name":"Moved On"}`))

	for boot := range 2 {
		var logged bytes.Buffer
		if err := bootOnly(t, db, fleet, slog.New(slog.NewTextHandler(&logged, nil))); err != nil {
			t.Fatalf("boot %d: %v", boot, err)
		}
		assertFleetOn(t, fleet, moved)
		// AND WHAT TO DO ABOUT IT: the change itself, read against what it
		// was built on — never the whole stale document put up as the company.
		for _, want := range []string{"offline_revision_not_published", offered,
			"built_on=" + built.ID, "fleet_runs=peer-revision",
			"/config/revisions/" + offered + "/diff?against=" + built.ID} {
			if !strings.Contains(logged.String(), want) {
				t.Errorf("boot %d did not say the withheld change with %q: %s",
					boot, want, logged.String())
			}
		}
		if booted := bootRow(t, db); booted.ID != offered || booted.Offer == nil ||
			booted.Offer.Base != built.ID {
			t.Errorf("boot %d: this node boots on %s (offer %+v), want the withheld "+
				"change %s over %s kept", boot, booted.ID, booted.Offer, offered, built.ID)
		}
		// THE TICK OF A NODE THAT REFUSES THE FLEET'S REVISION records it as
		// the fleet's and leaves the offer in place.
		refuses(t, db, fleet)
	}

	applies(t, db, fleet, fixtureCipher)
	if booted := bootRow(t, db); booted.ID != moved.RevisionID || booted.Offer != nil {
		t.Errorf("once it applies the fleet's revision this node boots on %s (offer %+v), "+
			"want the fleet's %s", booted.ID, booted.Offer, moved.RevisionID)
	}
	if _, held, err := db.Configs().Get(t.Context(), offered); err != nil || !held {
		t.Errorf("the replaced offline change is not kept as history: held=%v err=%v", held, err)
	}
}

// landsFirst is a fleet on which another write lands just before this node's
// first activation: a peer's write between the boot's read of the pointer and
// its publish.
type landsFirst struct {
	coord.Plane
	write  func() coord.Activation
	landed atomic.Bool
	won    coord.Activation
}

func (p *landsFirst) Activate(ctx context.Context, req coord.ActivationRequest) (coord.Activation, error) {
	if p.landed.CompareAndSwap(false, true) {
		p.won = p.write()
	}
	return p.Plane.Activate(ctx, req)
}

// A WRITE BETWEEN THE BOOT'S READ AND ITS PUBLISH IS NOT OVERWRITTEN (#319).
//
// The boot read the pointer, decided, and published with no compare-and-set,
// so a write landing in between was replaced by the boot's revision. It
// publishes now under the expectation it decided on, and a lost race reads the
// pointer again and decides again: an offer over a base the fleet has left is
// withheld, and a restore of a lost pointer defers to the one that appeared.
//
// Mutation: publish either with no expectation and the write is replaced.
func TestAWriteBetweenTheBootReadAndPublishIsNotOverwritten(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, db *store.DB) coord.Plane
	}{
		{"an offline change over the fleet's revision", func(t *testing.T, db *store.DB) coord.Plane {
			fleet := coordmemory.NewFleet()
			if err := bootSeed(t, db, fleet, seedOf(parse(t, companyYAML)), fixtureCipher); err != nil {
				t.Fatalf("seed: %v", err)
			}
			offlineImport(t, db, strings.Replace(companyYAML, "name: Acme", "name: Acme Offline", 1))
			return fleet
		}},
		{"the restore of a lost pointer", func(t *testing.T, db *store.DB) coord.Plane {
			if err := bootSeed(t, db, coordmemory.NewFleet(), seedOf(parse(t, companyYAML)),
				fixtureCipher); err != nil {
				t.Fatalf("seed: %v", err)
			}
			return coordmemory.NewFleet()
		}},
	} {
		db := seedStore(t)
		real := tc.setup(t, db)
		racing := &landsFirst{Plane: real, write: func() coord.Activation {
			return activateAsPeer(t, real, "written-meanwhile", []byte(`{"name":"Meanwhile"}`))
		}}
		var logged bytes.Buffer
		if err := bootOnly(t, db, racing, slog.New(slog.NewTextHandler(&logged, nil))); err != nil {
			t.Fatalf("%s: boot: %v", tc.name, err)
		}
		if !racing.landed.Load() {
			t.Fatalf("%s: the boot published nothing, so nothing raced it", tc.name)
		}
		assertFleetOn(t, real, racing.won)
		if !strings.Contains(logged.String(), "boot_publish_raced") {
			t.Errorf("%s: the lost race was not said: %s", tc.name, logged.String())
		}
	}
}

// A LOST POINTER IS RESTORED FROM THE FLEET'S LAST REVISION, NOT THE BOOT ROW.
//
// This node refused the fleet's R — its engine could not build it — and runs
// the P it served before. The coordination store is rebuilt. What it restores
// is the company, R, which every node that could run R runs: restoring P would
// roll each of them back to it.
//
// Mutation: restore the boot row and the pointer names P.
func TestALostPointerIsRestoredFromTheFleetsLastRevisionNotTheBootRow(t *testing.T) {
	t.Parallel()
	db := seedStore(t)
	first := coordmemory.NewFleet()
	if err := bootSeed(t, db, first, seedOf(parse(t, companyYAML)), fixtureCipher); err != nil {
		t.Fatalf("seed: %v", err)
	}
	served := bootRow(t, db)
	fleets := activateAsPeer(t, first, "the-fleets-revision", []byte(`{"name":"Refused Here"}`))
	refuses(t, db, first)

	fleet := coordmemory.NewFleet()
	if err := bootOnly(t, db, fleet, quiet()); err != nil {
		t.Fatalf("restart: %v", err)
	}
	target, found, err := fleet.Target(t.Context())
	if err != nil || !found {
		t.Fatalf("no pointer restored: found=%v err=%v", found, err)
	}
	if target.RevisionID != fleets.RevisionID ||
		store.EncodeTime(target.At) != store.EncodeTime(fleets.At) {
		t.Errorf("the restored pointer names %s at %s, want the fleet's %s at %s — not "+
			"%s, which only this node runs", target.RevisionID, target.At,
			fleets.RevisionID, fleets.At, served.ID)
	}
}

// A RECREATED POINTER OLDER THAN THE FLEET'S REVISION THIS NODE SAW IS REPLACED.
//
// The coordination store was rebuilt, and a node that was down while the fleet
// moved on restored the old revision it knew first. This node saw the newer
// one, and publishes it over the old — expecting that very restore, so a write
// landing in between is not replaced — as a restore itself. A pointer naming a
// revision this node never held is not one it can call older: it is somebody's
// write, and the fleet's. And a pointer somebody CHOSE is the company however
// old its instant reads (#4): instants come from writers' clocks, and an
// operator who reverted to the old revision on a node whose clock runs behind
// meant it.
//
// Mutation: drop rule 2b and the old revision stays; drop its held guard and a
// write this node never saw is replaced; drop its restored guard and the
// operator's choice is reverted.
func TestARecreatedOlderPointerIsReplacedByTheNewerRevisionThisNodeSaw(t *testing.T) {
	t.Parallel()
	db := seedStore(t)
	first := coordmemory.NewFleet()
	if err := bootSeed(t, db, first, seedOf(parse(t, companyYAML)), fixtureCipher); err != nil {
		t.Fatalf("seed: %v", err)
	}
	old := bootRow(t, db)
	newer := activateAsPeer(t, first, "the-newer-revision", []byte(`{"name":"Newer"}`))
	applies(t, db, first, fixtureCipher)

	t.Run("an older restore this node holds", func(t *testing.T) {
		recreated := coordmemory.NewFleet()
		if _, err := recreated.Activate(t.Context(), coord.ActivationRequest{
			RevisionID: old.ID, Summary: old.Summary, Payload: old.Payload,
			At: old.ActivatedAt, Origin: originOf(old), ExpectAbsent: true, Restore: true,
		}); err != nil {
			t.Fatalf("the down node's restore: %v", err)
		}
		if err := bootOnly(t, db, recreated, quiet()); err != nil {
			t.Fatalf("boot: %v", err)
		}
		target, _, err := recreated.Target(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if target.RevisionID != newer.RevisionID || !target.Restored {
			t.Errorf("the recreated pointer names %s (restored=%v), want the newer %s "+
				"this node saw, restored", target.RevisionID, target.Restored, newer.RevisionID)
		}
	})

	t.Run("an older revision somebody chose", func(t *testing.T) {
		chosen := coordmemory.NewFleet()
		written, err := chosen.Activate(t.Context(), coord.ActivationRequest{
			RevisionID: old.ID, Summary: "revert", Payload: old.Payload,
			At: old.ActivatedAt, Origin: originOf(old), ExpectAbsent: true,
		})
		if err != nil {
			t.Fatalf("the operator's choice: %v", err)
		}
		if err := bootOnly(t, db, chosen, quiet()); err != nil {
			t.Fatalf("boot: %v", err)
		}
		assertFleetOn(t, chosen, written)
	})

	t.Run("an older restore naming a revision this node never held", func(t *testing.T) {
		elsewhere := coordmemory.NewFleet()
		sealed, err := secrets.Seal(fixtureCipher, []byte(`{"name":"Elsewhere"}`))
		if err != nil {
			t.Fatal(err)
		}
		written, err := elsewhere.Activate(t.Context(), coord.ActivationRequest{
			RevisionID: "never-held", Payload: sealed, At: newer.At.Add(-time.Hour),
			Origin: coord.RevisionOrigin{Author: "maya", AuthorKind: iam.ActorOperator,
				Source: "api", CreatedAt: newer.At.Add(-time.Hour)},
			ExpectAbsent: true, Restore: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := bootOnly(t, db, elsewhere, quiet()); err != nil {
			t.Fatalf("boot: %v", err)
		}
		assertFleetOn(t, elsewhere, written)
	})
}

// A STALE RESTORE OF AN OFFLINE CHANGE'S BASE DOES NOT PUBLISH THE CHANGE.
//
// This node holds an offline change built on P, and saw the fleet move on to R
// before it could publish it. The coordination store was rebuilt, and a node
// that was down restored P. The pointer naming P again is not the company
// coming back to P — it is another node's stale memory — so the change built
// on P is not published over it, which would roll every node back to P's
// document plus the change. R is restored instead, and the change is withheld
// as it was.
//
// Mutation: drop the restored guard on rule 1a and the offline change is
// published over the stale restore.
func TestAStaleRestoreOfAnOfflineChangesBaseDoesNotPublishTheChange(t *testing.T) {
	t.Parallel()
	db := seedStore(t)
	first := coordmemory.NewFleet()
	if err := bootSeed(t, db, first, seedOf(parse(t, companyYAML)), fixtureCipher); err != nil {
		t.Fatalf("seed: %v", err)
	}
	built := bootRow(t, db)
	offered := offlineImport(t, db, strings.Replace(companyYAML, "name: Acme",
		"name: Acme Offline", 1))
	moved := activateAsPeer(t, first, "the-fleets-revision", []byte(`{"name":"Moved On"}`))
	refuses(t, db, first)

	rebuilt := coordmemory.NewFleet()
	if _, err := rebuilt.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: built.ID, Summary: built.Summary, Payload: built.Payload,
		At: built.ActivatedAt, Origin: originOf(built), ExpectAbsent: true, Restore: true,
	}); err != nil {
		t.Fatalf("the down node's restore: %v", err)
	}
	var logged bytes.Buffer
	if err := bootOnly(t, db, rebuilt, slog.New(slog.NewTextHandler(&logged, nil))); err != nil {
		t.Fatalf("boot: %v", err)
	}
	target, _, err := rebuilt.Target(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if target.RevisionID != moved.RevisionID || !target.Restored {
		t.Errorf("the pointer names %s (restored=%v), want the fleet's %s restored over "+
			"the stale %s", target.RevisionID, target.Restored, moved.RevisionID, built.ID)
	}
	if !strings.Contains(logged.String(), "offline_revision_not_published") {
		t.Errorf("the withheld change was not said: %s", logged.String())
	}
	if booted := bootRow(t, db); booted.ID != offered || booted.Offer == nil {
		t.Errorf("this node boots on %s (offer %+v), want the withheld change %s kept",
			booted.ID, booted.Offer, offered)
	}
}

// AN OFFLINE CHANGE MADE ON ONE AN EARLIER START SENT IS PUBLISHED OVER IT (#6).
//
// A start published X and died before recording that it landed, and an
// operator then made another offline change, Y, on X — what the node boots on.
// Whether the fleet took X is not known offline, so Y is built on X's base or
// on X, and the boot publishes it over whichever the pointer names. Built on
// the base alone, Y was withheld as a change the fleet had moved on from: the
// fleet had moved on to X, the very company Y was made on.
//
// Mutation: drop the unconfirmed offer from [store.Offer.Over] and Y is
// withheld.
func TestAnOfflineChangeOnAnUnconfirmedPublishIsPublishedOverIt(t *testing.T) {
	t.Parallel()
	db := seedStore(t)
	fleet := coordmemory.NewFleet()
	if err := bootSeed(t, db, fleet, seedOf(parse(t, companyYAML)), fixtureCipher); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sent := offlineImport(t, db, strings.Replace(companyYAML, "name: Acme", "name: Acme X", 1))
	base, _, err := fleet.Target(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	planned := coord.ActivationAt(time.Now().UTC().Truncate(time.Microsecond), base.At)
	if err := db.Configs().PlanOffer(t.Context(), sent, planned, base.RevisionID); err != nil {
		t.Fatalf("plan: %v", err)
	}
	booted := bootRow(t, db)
	landed, err := fleet.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: sent, Summary: booted.Summary, Payload: booted.Payload, At: planned,
		Origin: originOf(booted), Expect: base.RevisionID, ExpectEpoch: base.Epoch,
	})
	if err != nil {
		t.Fatalf("the publish before the crash: %v", err)
	}

	made := offlineImport(t, db, strings.Replace(companyYAML, "name: Acme", "name: Acme Y", 1))
	if offer := bootRow(t, db).Offer; offer == nil || offer.Base != base.RevisionID ||
		offer.Unconfirmed != sent {
		t.Fatalf("the change on the sent offer is offered over %+v, want %s or the "+
			"unconfirmed %s", offer, base.RevisionID, sent)
	}
	if err := bootOnly(t, db, fleet, quiet()); err != nil {
		t.Fatalf("boot: %v", err)
	}
	target, _, err := fleet.Target(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if target.RevisionID != made || target.Epoch != landed.Epoch+1 {
		t.Errorf("the fleet is on %s at epoch %d, want the change %s published over %s's "+
			"epoch %d", target.RevisionID, target.Epoch, made, sent, landed.Epoch)
	}
	if after := bootRow(t, db); after.ID != made || after.Offer != nil {
		t.Errorf("this node boots on %s with offer %+v, want %s confirmed", after.ID,
			after.Offer, made)
	}
}

// THE BOOT PUBLISHES WHAT THE ENGINE BOOTED ON, and an offer the store no
// longer holds is a DEFECT, not a race — whether the boot meets it planning
// the offer's publish (1a, 1d) or confirming one that landed (1b).
//
// `crewlet run` used to read the store, close it, and let the engine open it
// again seconds later; an offline command in that gap replaced the offer, the
// boot published what it had read, and the first tick then cleared the
// operator's change with a false "the fleet moved on" warning. The boot now
// holds this node's store from the read it decides on until the engine has
// opened the same file ([bootEngine], whose own case stands a second process
// in that gap), so nothing can replace the offer there, and the one online
// writer of the boot row does not exist until the publish returns. An offer
// gone when the publish plans or confirms it is therefore a store that
// contradicts the company the engine built — a defect — and the boot is
// refused naming the revision, with nothing published, rather than publishing
// past it or walking on.
//
// Mutations, each run red: make [noLongerOffered] return nil on
// [store.ErrNotOffered], as the old "leave it for the next start" did, and the
// plan case reports success having published nothing; make [confirmOffer]
// return nil on it, and the confirm case does.
func TestTheBootPublishesWhatTheEngineBootedOn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// landed is whether the offer's planned publish reached the pointer
		// before the boot: rule 1b, which confirms it, rather than 1a, which
		// plans it. doing is the step the refusal names.
		landed bool
		doing  string
	}{
		{name: "planning the publish", doing: "plan the publish of"},
		{name: "confirming a publish that landed", landed: true,
			doing: "confirm the landed publish of"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := seedStore(t)
			fleet := coordmemory.NewFleet()
			if err := bootSeed(t, db, fleet, seedOf(parse(t, companyYAML)), fixtureCipher); err != nil {
				t.Fatalf("seed: %v", err)
			}
			booted := offlineImport(t, db, strings.Replace(companyYAML, "name: Acme",
				"name: Acme One", 1))
			if tc.landed {
				// THE PUBLISH AN EARLIER START MADE, which landed at the
				// instant it planned before that start crashed.
				base, _, err := fleet.Target(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				planned := coord.ActivationAt(time.Now().UTC().Truncate(time.Microsecond), base.At)
				if err := db.Configs().PlanOffer(t.Context(), booted, planned,
					base.RevisionID); err != nil {
					t.Fatalf("plan: %v", err)
				}
				row := bootRow(t, db)
				if _, err := fleet.Activate(t.Context(), coord.ActivationRequest{
					RevisionID: booted, Summary: row.Summary, Payload: row.Payload,
					At: planned, Origin: originOf(row), Expect: base.RevisionID,
				}); err != nil {
					t.Fatalf("the publish before the crash: %v", err)
				}
			}
			before, _, err := fleet.Target(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			plan, err := decideSeed(t.Context(), &config.Bootstrap{},
				tierBSeed{Path: "company.yaml"}, storeOf(db, fixtureCipher), quiet())
			if err != nil {
				t.Fatal(err)
			}
			// A STORE THE BOOT NO LONGER ACCOUNTS FOR. In production the held
			// claim makes this unreachable; recreated here, it must be refused
			// rather than silently published past, which is the behaviour the
			// claim lets the code insist on.
			replaced := offlineImport(t, db, strings.Replace(companyYAML, "name: Acme",
				"name: Acme Two", 1))

			err = seedCompany(t.Context(), db, fleet, nil, plan, fixtureCipher, testNode,
				builds, quiet())
			if err == nil || !strings.Contains(err.Error(), tc.doing+" revision "+booted) ||
				!strings.Contains(err.Error(), "defect") {
				t.Fatalf("a boot whose offer was gone = %v, want a refusal to %s %s, as "+
					"a defect", err, tc.doing, booted)
			}
			// NOTHING PUBLISHED: the pointer is where the fleet left it, and the
			// store is left exactly as the (impossible) replacement made it,
			// for the next start to decide on.
			assertFleetOn(t, fleet, before)
			if row := bootRow(t, db); row.ID != replaced || row.Offer == nil {
				t.Errorf("this node boots on %s (offer %+v), want the store untouched at %s",
					row.ID, row.Offer, replaced)
			}
		})
	}
}

// THE FIRST TICK SERVES THE OFFER THE BOOT PUBLISHED, and says nothing of a
// fleet that moved on.
//
// An offline import makes an offer over the company the node runs. The boot
// publishes it (1d: the fleet has no pointer, and last ran the offer's base),
// and the first reconcile — `startReconciler`'s own tick, which `crewlet run`
// always runs — applies that epoch, so the node serves the offer and boots on
// it confirmed, with no `offline_revision_not_published`. This is the path an
// offer takes when nothing interferes. What once sent the tick down the other
// one — an offline command run while the node started, whose offer the tick
// then cleared under that warning — is excluded by the boot's claim on the
// store, which [TestTheBootHoldsItsStoreUntilTheEngineHasOpenedIt] stands a
// second process against.
//
// Mutation, run red: suppress rule 1d in [decideBootPublish], so the offer is
// withheld as one the fleet moved on from, and the boot puts the fleet back on
// the base instead of on the offer.
func TestTheFirstTickServesTheBootsOffer(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)
	boot := storedBoot(t, cfg)

	// A COMPANY THE NODE ALREADY RUNS, then an offline import over it — the
	// offer the next start publishes.
	serveStored(t, cfg, runnableRevisionDoc)
	offered := companyFile(t, dir, "company.yaml", func(string) string {
		return strings.Replace(runnableRevisionDoc, `"Nimbus"`, `"Nimbus Offline"`, 1)
	})
	if _, errs, err := configCmd(t, cfg, "import", offered); err != nil {
		t.Fatalf("offline import: %v (%s)", err, errs)
	}
	offerID := bootRevisionID(t, cfg)

	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, nil))
	e, plan := bootDecided(t, cfg, boot, tierBSeed{})
	if plan.boot.ID != offerID || plan.boot.Offer == nil {
		t.Fatalf("the engine booted on %s (offer %+v), want the offline offer %s",
			plan.boot.ID, plan.boot.Offer, offerID)
	}
	if _, err := startReconciler(t.Context(), e, boot, plan, e.Cipher(), log); err != nil {
		t.Fatalf("startReconciler: %v", err)
	}

	// THE FLEET RUNS THE OFFER, and this node boots on it, confirmed.
	target, found, err := e.Backends().Fleet.Target(t.Context())
	if err != nil || !found || target.RevisionID != offerID {
		t.Fatalf("the fleet is on %+v (found=%v err=%v), want the published offer %s",
			target, found, err, offerID)
	}
	if row := bootRow(t, e.Backends().Store); row.ID != offerID || row.Offer != nil {
		t.Errorf("this node boots on %s (offer %+v), want the offer %s served and confirmed",
			row.ID, row.Offer, offerID)
	}
	if strings.Contains(logged.String(), "offline_revision_not_published") {
		t.Errorf("the first tick warned that the fleet moved on from an offer it in fact "+
			"published and served:\n%s", logged.String())
	}
}

// AN OFFLINE ACTIVATION OF A REVISION THIS BUILD CANNOT RUN IS REFUSED, with
// nothing written: what it would write is what the next start boots on, and
// that start would be refused for a document nobody asked to change.
//
// Mutation: drop the runnable check and the revision becomes the boot row.
func TestAnOfflineActivationOfARevisionThisBuildCannotRunIsRefused(t *testing.T) {
	t.Parallel()
	cfg := bootstrapForStore(t, t.TempDir())
	served := serveStored(t, cfg, runnableRevisionDoc)
	cs, closeStore, err := openConfigStore(t.Context(), cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := secrets.Seal(cs.cipher, []byte(storedRevisionDoc))
	if err != nil {
		t.Fatal(err)
	}
	unrunnable, err := cs.configs.Insert(t.Context(), store.Revision{
		Source: "fleet", CreatedBy: "peer", CreatedByKind: iam.ActorOperator,
		Summary: "from a newer build", Payload: sealed,
	})
	closeStore()
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := configCmd(t, cfg, "activate", unrunnable); err == nil ||
		!strings.Contains(err.Error(), "cannot run on this build") ||
		!strings.Contains(err.Error(), unrunnable) {
		t.Errorf("config activate of a revision this build cannot run = %v, want a "+
			"refusal naming it", err)
	}
	if booted := bootRevisionID(t, cfg); booted != served {
		t.Errorf("this node boots on %s after a refused activation, want %s as before",
			booted, served)
	}
}

// A NODE THAT REFUSED THE FLEET'S REVISION BOOTS ON WHAT IT SERVED.
//
// Its reconciler opened R and could not build it, and recorded R as the
// fleet's: that is what the node knows the company to be. It used to mark R
// active as it fetched it, before validating it, and its next start booted on
// the revision it had just refused — and refused the boot. The boot row moves
// only once the engine applied a revision, so the restart boots on P, at the
// instant of the activation P was applied from.
//
// Mutation: boot on the fleet's row and the node starts on R.
func TestANodeThatRefusedTheFleetsRevisionBootsOnWhatItServed(t *testing.T) {
	t.Parallel()
	cfg := bootstrapForStore(t, t.TempDir())
	applied := time.Date(2026, 4, 1, 9, 30, 0, 0, time.UTC)
	served := serveStoredAt(t, cfg, runnableRevisionDoc, applied)
	refused := seeStored(t, cfg, strings.Replace(runnableRevisionDoc, `"Nimbus"`,
		`"Refused Here"`, 1))

	plan, err := bootOn(t, cfg, tierBSeed{}, quiet())
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	if plan.company == nil || plan.company.Name != "Nimbus" || plan.boot.ID != served {
		t.Errorf("the node boots on %s (%s), want %s, the revision it served — not the "+
			"fleet's %s it refused", companyLabel(plan.company), plan.boot.ID, served, refused)
	}
	if !plan.activatedAt.Equal(applied) {
		t.Errorf("the boot carries activation %s, want the served revision's %s",
			plan.activatedAt, applied)
	}
	if plan.fleets == nil || plan.fleets.ID != refused {
		t.Errorf("the plan carries the fleet's revision %+v, want %s for the boot publish",
			plan.fleets, refused)
	}
}

// THE BOOT CARRIES THE INSTANT IT APPLIED, NOT THE FLEET'S LATEST ACTIVATION.
//
// A node runs P, applied at T1, so its boot row's served instant is T1. The
// fleet re-activates P at T2 to pick up a rotated credential, and this node's
// reconciler records that sighting — raising the row's `activated_at` to T2 —
// but refuses the apply, lacking the credential. Its next boot still stamps the
// chart with T1, the activation it applied, not T2, which it never ran. An
// offline `config activate` is the other way the two columns part: it keeps
// `activated_at` and nulls `served_at`, so the offer it makes boots with no
// instant, because nothing has applied the choice.
//
// Mutation: boot on `activated_at` (seed.go's `booted.revision.ServedAt` back
// to `.ActivatedAt`) and the first case stamps T2, an activation this node
// never applied; the second stamps the offer with the activation it replaced.
func TestTheBootCarriesTheInstantItAppliedNotTheFleetsLatest(t *testing.T) {
	t.Parallel()

	t.Run("a refused re-activation raised activated_at past served_at", func(t *testing.T) {
		t.Parallel()
		cfg := bootstrapForStore(t, t.TempDir())
		t1 := time.Date(2026, 4, 1, 9, 30, 0, 0, time.UTC)
		t2 := t1.Add(2 * time.Hour)
		served := serveStoredAt(t, cfg, runnableRevisionDoc, t1)

		// THE FLEET'S RE-ACTIVATION THIS NODE REFUSED: its reconciler saw the
		// pointer name the same revision at a later instant and recorded it,
		// which raises activated_at and leaves served_at where the apply put
		// it.
		cs, closeStore, err := openConfigStore(t.Context(), cfg, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := cs.configs.SawFleet(t.Context(), store.Revision{
			ID: served, Source: "fleet", CreatedBy: "peer", CreatedByKind: iam.ActorOperator,
			Summary: "re-activated", Payload: json.RawMessage(`{"name":"Nimbus"}`),
			CreatedAt: t1, ActivatedAt: t2,
		}); err != nil {
			closeStore()
			t.Fatalf("record the fleet's re-activation: %v", err)
		}
		boot, _, err := cs.configs.Boot(t.Context())
		closeStore()
		if err != nil {
			t.Fatal(err)
		}
		// The case only separates the columns if the store really holds them
		// apart: activated_at raised to T2, served_at left at T1.
		if !boot.ActivatedAt.Equal(t2) || !boot.ServedAt.Equal(t1) {
			t.Fatalf("the boot row reads activated %s served %s, want the fleet's %s "+
				"and the applied %s — the case does not separate the columns",
				boot.ActivatedAt, boot.ServedAt, t2, t1)
		}

		plan, err := bootOn(t, cfg, tierBSeed{}, quiet())
		if err != nil {
			t.Fatal(err)
		}
		if !plan.activatedAt.Equal(t1) {
			t.Errorf("the boot carries activation %s, want the applied %s and never the "+
				"fleet's later %s, which this node refused", plan.activatedAt, t1, t2)
		}
	})

	t.Run("an offline activation nulls served_at", func(t *testing.T) {
		t.Parallel()
		cfg := bootstrapForStore(t, t.TempDir())
		served := serveStoredAt(t, cfg, runnableRevisionDoc, time.Now().UTC())

		// `crewlet config activate` of the running revision keeps its
		// activated_at and nulls its served_at, so the offer has no applied
		// instant.
		cs, closeStore, err := openConfigStore(t.Context(), cfg, "")
		if err != nil {
			t.Fatal(err)
		}
		err = activateRevision(t.Context(), cs, served, io.Discard)
		closeStore()
		if err != nil {
			t.Fatalf("offline activate: %v", err)
		}

		plan, err := bootOn(t, cfg, tierBSeed{}, quiet())
		if err != nil {
			t.Fatal(err)
		}
		if !plan.activatedAt.IsZero() {
			t.Errorf("the boot carries activation %s, want none: an offer has none until "+
				"the first apply of the activation that publishes it", plan.activatedAt)
		}
	})
}

// A JOINER THAT SAW THE FLEET'S COMPANY IGNORES ITS BOOTSTRAP FILE.
//
// It joined, opened the fleet's revision and refused it, so it boots on
// nothing. A -company file beside it is no bootstrap: the company exists.
// Booting on the file would run it until the first tick, and importing it
// would publish it over a fleet that never asked. A file that IS the fleet's
// revision is what every node of a uniform fleet carries, and is no news.
//
// Mutation: read a store with no boot row as a store with no company, and the
// file is bootstrapped.
func TestAJoinerThatSawTheFleetIgnoresItsBootstrapFile(t *testing.T) {
	t.Parallel()
	file := parse(t, companyYAML)

	t.Run("another file", func(t *testing.T) {
		t.Parallel()
		cfg := bootstrapForStore(t, t.TempDir())
		fleets := seeStored(t, cfg, runnableRevisionDoc)
		var logged bytes.Buffer
		plan, err := bootOn(t, cfg, seedOf(file), slog.New(slog.NewTextHandler(&logged, nil)))
		if err != nil {
			t.Fatal(err)
		}
		if plan.company != nil || plan.act != seedNone {
			t.Errorf("a joiner that saw the fleet's company boots on %s to %q, want "+
				"nothing and nothing imported", companyLabel(plan.company), plan.act)
		}
		for _, want := range []string{"company_seed_ignored", "fleet_revision=" + fleets,
			"crewlet config import company.yaml"} {
			if !strings.Contains(logged.String(), want) {
				t.Errorf("the ignored file was not said with %q: %s", want, logged.String())
			}
		}
	})

	t.Run("the fleet's own revision", func(t *testing.T) {
		t.Parallel()
		cfg := bootstrapForStore(t, t.TempDir())
		document, err := json.Marshal(file)
		if err != nil {
			t.Fatal(err)
		}
		seeStored(t, cfg, string(document))
		var logged bytes.Buffer
		plan, err := bootOn(t, cfg, seedOf(file), slog.New(slog.NewTextHandler(&logged, nil)))
		if err != nil {
			t.Fatal(err)
		}
		if plan.act != seedNone {
			t.Errorf("a file that is the fleet's revision is %q, want nothing imported", plan.act)
		}
		if logged.Len() != 0 {
			t.Errorf("a file that is the fleet's revision was reported: %s", logged.String())
		}
	})
}

// THE REVISION LIST SAYS WHAT THIS NODE BOOTS ON AND WHAT THE FLEET RUNS.
//
// One marker said "active", and on a node that refused the fleet's revision
// "which is running" has two answers. The STATE column gives both, and an
// offline change says what it was built on, which is what the next start
// publishes it over.
func TestTheRevisionListSaysWhatThisNodeBootsOnAndWhatTheFleetRuns(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)
	served := serveStored(t, cfg, runnableRevisionDoc)
	assertStates(t, cfg, map[string]string{served: "boots, fleet's"})

	refused := seeStored(t, cfg, strings.Replace(runnableRevisionDoc, `"Nimbus"`,
		`"Refused Here"`, 1))
	assertStates(t, cfg, map[string]string{served: "boots", refused: "fleet's"})

	if _, errs, err := configCmd(t, cfg, "import", companyFile(t, dir, "company.yaml", nil)); err != nil {
		t.Fatalf("import: %v (%s)", err, errs)
	}
	offered := bootRevisionID(t, cfg)
	assertStates(t, cfg, map[string]string{served: "-", refused: "fleet's",
		offered: "boots, offered over " + refused})

	// A START SENT THAT OFFER AND NEVER CONFIRMED IT, and the operator changed
	// the company again: the next start publishes this change over either, and
	// the listing names both, or the operator reads a base the fleet may have
	// moved past and takes the change for one that will be withheld.
	cs, closeStore, err := openConfigStore(t.Context(), cfg, "")
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	err = cs.configs.PlanOffer(t.Context(), offered, time.Now().UTC(), refused)
	closeStore()
	if err != nil {
		t.Fatalf("plan the offer's publish: %v", err)
	}
	if _, errs, err := configCmd(t, cfg, "import", companyFile(t, dir, "company.yaml",
		func(doc string) string {
			return strings.Replace(doc, "name: Nimbus", "name: Changed Again", 1)
		})); err != nil {
		t.Fatalf("import over the unconfirmed offer: %v (%s)", err, errs)
	}
	again := bootRevisionID(t, cfg)
	assertStates(t, cfg, map[string]string{served: "-", refused: "fleet's", offered: "-",
		again: "boots, offered over " + refused + " or the unconfirmed " + offered})
}

// assertStates fails a case whose revision listing does not give each revision
// the STATE want names, and exactly those revisions.
func assertStates(t *testing.T, cfg string, want map[string]string) {
	t.Helper()
	out, _, err := configCmd(t, cfg, "revisions")
	if err != nil {
		t.Fatalf("revisions: %v", err)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	// THE COLUMNS BY THE HEADER'S OFFSETS: a STATE such as "offered over <id>"
	// holds spaces, so splitting on them would cut it.
	header := lines[0]
	from, to := strings.Index(header, "STATE"), strings.Index(header, "CREATED")
	if from < 0 || to < from {
		t.Fatalf("the listing has no STATE column:\n%s", out)
	}
	got := map[string]string{}
	for _, line := range lines[1:] {
		got[strings.TrimSpace(line[:from])] = strings.TrimSpace(line[from:to])
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the listing says %v, want %v:\n%s", got, want, out)
	}
}

// offlineImport is `crewlet config import` with the engine stopped, over db: a
// revision this node boots on, offered over the company it knows. It returns
// the offer's id.
func offlineImport(t *testing.T, db *store.DB, document string) string {
	t.Helper()
	cs := &configStore{configs: db.Configs(), cipher: fixtureCipher, boot: &config.Bootstrap{}}
	if err := importConfig(t.Context(), cs, "company.yaml", parse(t, document),
		"imported offline", io.Discard); err != nil {
		t.Fatalf("offline import: %v", err)
	}
	return bootRow(t, db).ID
}

// bootOnly is a boot of a data node with no file, decided and carried out
// ([bootSeed]) with no tick after it: the boot publish alone, which is what a
// node whose engine refuses the fleet's revision does at every start.
func bootOnly(t *testing.T, db *store.DB, plane coord.Plane, log *slog.Logger) error {
	t.Helper()
	plan, err := decideSeed(t.Context(), &config.Bootstrap{}, tierBSeed{Path: "company.yaml"},
		storeOf(db, fixtureCipher), log)
	if err != nil {
		return err
	}
	return seedCompany(t.Context(), db, plane, nil, plan, fixtureCipher, testNode, builds, log)
}

// refuses is the reconcile tick of a node whose engine refuses the fleet's
// revision: it is recorded as the fleet's, and this node boots on what it
// served before ([applies] is the tick that applies it).
func refuses(t *testing.T, db *store.DB, plane coord.Plane) {
	t.Helper()
	target, pointed, err := plane.Target(t.Context())
	if err != nil || !pointed {
		t.Fatalf("the tick's read of the pointer: pointed=%v err=%v", pointed, err)
	}
	payload, found, err := plane.Payload(t.Context(), target.RevisionID)
	if err != nil || !found {
		t.Fatalf("the tick's read of revision %s: found=%v err=%v", target.RevisionID, found, err)
	}
	if err := db.Configs().SawFleet(t.Context(), configplane.RevisionOf(target, payload)); err != nil {
		t.Fatalf("the tick's record of the fleet's revision: %v", err)
	}
}

// seeStored records raw bytes as the fleet's revision in the store cfg names,
// the way a tick that opened it and could not build it leaves it, and returns
// its id.
func seeStored(t *testing.T, cfg, payload string) string {
	t.Helper()
	cs, closeStore, err := openConfigStore(t.Context(), cfg, "")
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer closeStore()
	sealed, err := secrets.Seal(cs.cipher, []byte(payload))
	if err != nil {
		t.Fatalf("seal the revision: %v", err)
	}
	seen := time.Now().UTC()
	fleets := store.Revision{
		ID: uuid.NewString(), Source: "api", CreatedBy: "peer",
		CreatedByKind: iam.ActorOperator, Summary: "from a peer", Payload: sealed,
		CreatedAt: seen, ActivatedAt: seen,
	}
	if err := cs.configs.SawFleet(t.Context(), fleets); err != nil {
		t.Fatalf("record the fleet's revision: %v", err)
	}
	return fleets.ID
}

// storeAndActivate is a write the fleet took, as the config API makes one: r
// stored in db's history, and the pointer moved to it over what it named, or
// created where it named nothing.
func storeAndActivate(t *testing.T, db *store.DB, plane coord.Plane, r store.Revision) store.Revision {
	t.Helper()
	id, err := db.Configs().Insert(t.Context(), r)
	if err != nil {
		t.Fatalf("store the revision: %v", err)
	}
	stored, _, err := db.Configs().Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	target, pointed, err := plane.Target(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	published, err := plane.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: id, Summary: stored.Summary, Payload: stored.Payload, At: time.Now(),
		Origin: originOf(stored), Expect: target.RevisionID, ExpectAbsent: !pointed,
	})
	if err != nil {
		t.Fatalf("activate the revision: %v", err)
	}
	if err := db.Configs().NoteActivation(t.Context(), id, published.At); err != nil {
		t.Fatal(err)
	}
	stored.ActivatedAt = published.At
	return stored
}

// fleetsRevision is the revision the fleet's pointer names, from db's history,
// failing a case where there is no pointer or db does not hold it.
func fleetsRevision(t *testing.T, db *store.DB, plane coord.Plane) store.Revision {
	t.Helper()
	target, pointed, err := plane.Target(t.Context())
	if err != nil || !pointed {
		t.Fatalf("the fleet's pointer: pointed=%v err=%v", pointed, err)
	}
	revision, held, err := db.Configs().Get(t.Context(), target.RevisionID)
	if err != nil || !held {
		t.Fatalf("the fleet's revision %s: held=%v err=%v", target.RevisionID, held, err)
	}
	return revision
}
