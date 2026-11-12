package host_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/host"
	"github.com/crewlet/crewlet/internal/statelog"
)

// WHOM AN ENGINE WRITE IS MADE FOR, THROUGH THE GITLET NODE (ADR-0109): the
// node holds the carrier a seat's write states to its ceiling on its own chart
// before anything is built, and hands it on, as the write stated it, to what
// it lands the write as.

// carriers is a host's estate that records whom each push, merge and enqueue
// it is asked is made for — by the push's id, the merge's push id, the entry
// an enqueue makes — and asks each of the estate it wraps.
type carriers struct {
	host.Estate

	mu    sync.Mutex
	asked map[string]types.WrittenFor
}

// wrap is a WrapEstate that records into e.
func (e *carriers) wrap(inner host.Estate) host.Estate {
	e.Estate = inner
	return e
}

func (e *carriers) note(id string, f types.WrittenFor) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.asked == nil {
		e.asked = map[string]types.WrittenFor{}
	}
	e.asked[id] = f
}

// of is whom the request under id was made for, and whether one was asked.
func (e *carriers) of(id string) (types.WrittenFor, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	f, ok := e.asked[id]
	return f, ok
}

func (e *carriers) Propose(ctx context.Context, p gitlet.Proposal) (gitlet.ProposeResult, error) {
	e.note(p.PushID, p.WrittenFor)
	return e.Estate.Propose(ctx, p)
}

func (e *carriers) Merge(ctx context.Context, p gitlet.MergeProposal) (gitlet.MergeResult, error) {
	e.note(p.Merge.MergeID, p.WrittenFor)
	return e.Estate.Merge(ctx, p)
}

func (e *carriers) Queue(ctx context.Context, p gitlet.QueueProposal) (gitlet.QueueResult, error) {
	if p.Enqueue != nil {
		e.note(p.Enqueue.EntryID, p.WrittenFor)
	}
	return e.Estate.Queue(ctx, p)
}

// applied is a data node's applier that keeps each record it applies — its
// version and its payload, and for a merge what its push row keeps of whom the
// merge was made for, read in the record's own transaction.
type applied struct {
	statelog.Applier

	mu      sync.Mutex
	records []appliedRecord
}

// appliedRecord is one record [applied] kept.
type appliedRecord struct {
	v       int
	payload any
	row     sql.NullString
}

// wrap is a WrapApplier that keeps into a.
func (a *applied) wrap(inner statelog.Applier) statelog.Applier {
	a.Applier = inner
	return a
}

func (a *applied) Apply(ctx context.Context, tx *sql.Tx, rec statelog.Record,
	opts statelog.ApplyOptions) (int, error) {

	rows, err := a.Applier.Apply(ctx, tx, rec, opts)
	if err != nil {
		return rows, err
	}
	record, err := gitlet.Decode(rec.Payload)
	if err != nil {
		return rows, nil
	}
	payload, err := gitlet.DecodeMutation(record)
	if err != nil || payload == nil {
		return rows, nil
	}
	kept := appliedRecord{v: record.V, payload: payload}
	if m, ok := payload.(gitlet.Merge); ok {
		switch err := tx.QueryRowContext(ctx, `SELECT working_for FROM code_pushes WHERE push_id = ?`,
			m.MergeID).Scan(&kept.row); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return rows, err
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.records = append(a.records, kept)
	return rows, nil
}

// find is the first record a kept whose payload is what is asks after, and
// whom that payload says it was made for.
func (a *applied) find(is func(payload any) (*types.WorkingFor, bool)) (appliedRecord, *types.WorkingFor, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range a.records {
		if f, ok := is(r.payload); ok {
			return r, f, true
		}
	}
	return appliedRecord{}, nil, false
}

// AN ENGINE WRITE LANDS CARRYING WHOM IT WAS MADE FOR: the Gitlet node hands
// whom a seat's engine write is made for (gitlet.EngineWrite.WrittenFor) on to
// what it lands the write as — the push a commit lands as, the merge record a
// merge lands as, and the enqueue a merge onto a queue target is — as the write
// stated it, never stated anew. alice's commit, merge and enqueue made for pat's
// seat each reach the committer made for pat, and each lands carrying pat: the
// commit's push entry, the merge record and the push row it lands, and the
// enqueue record, each stamped version 2. Each made for nobody reaches it made
// for nobody and lands carrying nothing, at version 1. The estate's refusal
// before asking catches a request that states nothing, and the committer a
// carrier its asker may not state; neither sees a node that states nobody in
// place of the carrier it was handed, which only this does.
//
// Mutations: the landing's proposal, the merge's or the enqueue's stating
// nobody in place of the write's carrier, red.
func TestAnEngineWriteLandsCarryingWhomItWasMadeFor(t *testing.T) {
	t.Parallel()
	pat := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: gitlettest.FixturePat}
	for _, tc := range []struct {
		name       string
		writtenFor types.WrittenFor
	}{
		{"made for pat's seat", types.ForTurn(pat)},
		{"made for nobody", types.ForTurn(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			want := tc.writtenFor.Carrier()
			// version is what every record that carries it is stamped, and
			// row what a push row keeps of it: the carrier's own version and
			// its JSON, or the base and NULL for nobody.
			version, row := 1, sql.NullString{}
			if want != nil {
				encoded, err := json.Marshal(want)
				if err != nil {
					t.Fatal(err)
				}
				version, row = gitlet.WorkingForVersion, sql.NullString{String: string(encoded), Valid: true}
			}
			carried := func(e *carriers, what, id string) {
				t.Helper()
				got, ok := e.of(id)
				if !ok || !got.Stated() || !reflect.DeepEqual(got.Carrier(), want) {
					t.Errorf("the %s reached its committer made for %+v (stated %v, asked %v), want %+v", what,
						got.Carrier(), got.Stated(), ok, want)
				}
			}
			// landedCarrying fails the case unless the record a kept that is
			// asks after carries whom the write was made for, at its version.
			landedCarrying := func(a *applied, what string, is func(payload any) (*types.WorkingFor, bool)) {
				t.Helper()
				rec, got, ok := a.find(is)
				if !ok || !reflect.DeepEqual(got, want) || rec.v != version {
					t.Errorf("the %s landed carrying %+v at version %d (applied %v), want %+v at %d", what, got,
						rec.v, ok, want, version)
				}
			}

			// A COMMIT, AND A MERGE onto a target whose gate builds its merges.
			built, kept := &carriers{}, &applied{}
			c := newMergeCase(t, gitlettest.SoloOptions{Shape: gitlettest.Shape{WrapEstate: built.wrap},
				WrapApplier: kept.wrap}, gitlettest.Settings(), map[string]string{"README.md": "one\n"})
			commit := files(c.repo, alice(), gitlet.SeatNamespace(gitlettest.FixtureAlice)+"carried", "", c.head,
				text("carried.md", "carried\n"))
			commit.WrittenFor = tc.writtenFor
			landed := c.land(commit)
			carried(built, "commit's push", landed.PushID)
			landedCarrying(kept, "commit's push entry", func(payload any) (*types.WorkingFor, bool) {
				if b, ok := payload.(gitlet.PushBatch); ok {
					for _, e := range b.Pushes {
						if e.PushID == landed.PushID {
							return e.WorkingFor, true
						}
					}
				}
				return nil, false
			})
			n := c.open(featureRef, map[string]string{"x.md": "x\n"}, "Merge me")
			c.meetGate(n, lead())
			m := merge(c.repo, alice(), n, c.change(n).Head, gitlet.MergeMethodMerge)
			m.WrittenFor = tc.writtenFor
			res, err := c.write(m)
			c.requireMerged(n, res, err)
			carried(built, "merge", gitlet.WritePushID(m.OpID))
			isMerge := func(payload any) (*types.WorkingFor, bool) {
				merged, ok := payload.(gitlet.Merge)
				return merged.WorkingFor, ok && merged.MergeID == gitlet.WritePushID(m.OpID)
			}
			landedCarrying(kept, "merge record", isMerge)
			if rec, _, _ := kept.find(isMerge); rec.row != row {
				t.Errorf("the merge's push row keeps %+v of whom it was made for, want %+v", rec.row, row)
			}

			// AN ENQUEUE, a merge onto a target whose gate requires a check.
			queued, keptQueued := &carriers{}, &applied{}
			settings := gitlettest.Settings()
			settings.Protections[0].Gate = &gitlet.MergeGate{MergeBy: gitlet.MergeByWriters,
				Approvals: gitlet.ApprovalsNone, Checks: []string{"unit"}, Landing: gitlet.Landing{Depth: 4, Batch: 4},
				DeleteSource: gitlet.DeleteNone}
			q := newMergeCase(t, gitlettest.SoloOptions{Shape: gitlettest.Shape{WrapEstate: queued.wrap},
				WrapApplier: keptQueued.wrap}, settings, map[string]string{"README.md": "one\n"})
			n = q.open(featureRef, map[string]string{"y.md": "y\n"}, "Queue me")
			q.meetGate(n, lead())
			head := q.change(n).Head
			unit := gitlet.CheckKey{RepoID: q.repo, OID: head, Target: "refs/heads/main", Name: "unit", Attempt: 1}
			queueCheck(t, q.solo(), unit)
			endCheck(t, q.solo(), unit)
			e := merge(q.repo, alice(), n, head, gitlet.MergeMethodSquash)
			e.WrittenFor = tc.writtenFor
			res, err = q.write(e)
			if err != nil || res.Outcome != statelog.OutcomeApplied || res.Merge == nil || !res.Merge.Queued {
				t.Fatalf("the merge onto a queue target answered %+v %v, want it enqueued", res, err)
			}
			carried(queued, "enqueue", gitlet.QueueEntryID(e.OpID))
			landedCarrying(keptQueued, "enqueue record", func(payload any) (*types.WorkingFor, bool) {
				enqueue, ok := payload.(gitlet.Enqueue)
				return enqueue.WorkingFor, ok && enqueue.EntryID == gitlet.QueueEntryID(e.OpID)
			})
		})
	}
}

// A GITLET NODE REFUSES A WRITE WHOSE CARRIER ITS CEILING DOES NOT BOUND, ON
// ITS OWN CHART, BEFORE ANYTHING IS BUILT (ADR-0109; gitlet.EngineWrite's
// WrittenForOn): only the chart tells a person's seat, which bounds nothing,
// from a colleague's, whose ceiling the write must carry. So alice's commit
// made for the seat lead — an agent's — with no ceiling, and one made for pat's
// seat — a person's — under pat's ceiling, are each refused naming written_for,
// with no push recorded and no object stored; made for pat with no ceiling,
// and for lead under lead's, each lands. And a Gitlet node holding no chart
// yet judges no carrier: a merge made for somebody there — which reads no
// chart of its own where it carries no ceiling — is answered unready, about
// that node, so a Gitlet node holding one builds it; made for nobody it is
// judged as ever, and refused for a change the repository does not hold.
//
// Mutations: the Gitlet node skipping the chart's half — the commit for lead
// with no ceiling, and the one for pat under pat's, land, red; a node with no
// chart judging the carrier on none — the merge for pat is judged there, red.
func TestAGitletNodeRefusesAWriteWhoseCarrierItsCeilingDoesNotBound(t *testing.T) {
	t.Parallel()
	c := newWriteCase(t, gitlettest.SoloOptions{}, map[string]string{"README.md": "one\n"})
	forSeat := func(handle string) types.WrittenFor {
		return types.ForTurn(&types.WorkingFor{Kind: types.WorkingForSeat, Seat: handle})
	}
	seat := func(handle string) *gitlet.WriteCeiling {
		return &gitlet.WriteCeiling{Kind: gitlet.WriteCeilingSeat, Seat: handle}
	}
	for i, tc := range []struct {
		name       string
		ceiling    *gitlet.WriteCeiling
		writtenFor types.WrittenFor
		refused    bool
	}{
		{"for a colleague, with no ceiling", nil, forSeat(gitlettest.FixtureLead), true},
		{"for a person's seat, under its ceiling", seat(gitlettest.FixturePat), forSeat(gitlettest.FixturePat), true},
		{"for a person's seat, with no ceiling", nil, forSeat(gitlettest.FixturePat), false},
		{"for a colleague, under its ceiling", seat(gitlettest.FixtureLead), forSeat(gitlettest.FixtureLead), false},
	} {
		w := files(c.repo, alice(), gitlet.SeatNamespace(gitlettest.FixtureAlice)+fmt.Sprintf("made-for-%d", i), "",
			c.head, text("made-for.md", tc.name+"\n"))
		w.Ceiling, w.WrittenFor = tc.ceiling, tc.writtenFor
		if !tc.refused {
			c.land(w)
			continue
		}
		objects := len(c.objects())
		_, err := c.write(w)
		if !errors.Is(err, gitlet.ErrInvalid) || !strings.Contains(err.Error(), "written_for") {
			t.Errorf("alice's commit %s answered %v, want it refused, ErrInvalid naming written_for", tc.name, err)
		}
		if _, made := c.Rows().Refs[w.Ref]; made || len(c.objects()) != objects {
			t.Errorf("alice's refused commit %s made its branch (%v) and stored %d objects, want neither",
				tc.name, made, len(c.objects())-objects)
		}
	}

	// A GITLET NODE HOLDING NO CHART YET.
	chartless := newWriteCase(t, gitlettest.SoloOptions{Roster: func() gitlet.Roster { return nil }},
		map[string]string{"README.md": "one\n"})
	for _, tc := range []struct {
		name       string
		writtenFor types.WrittenFor
		unready    bool
	}{
		{"made for pat's seat", forSeat(gitlettest.FixturePat), true},
		{"made for nobody", types.ForTurn(nil), false},
	} {
		m := merge(chartless.repo, alice(), 99, chartless.head, gitlet.MergeMethodMerge)
		m.WrittenFor = tc.writtenFor
		res, err := chartless.write(m)
		var unready *gitlet.ErrGitUnready
		switch {
		case tc.unready && (!errors.As(err, &unready) || unready.Reason != gitlet.UnreadyNoChart):
			t.Errorf("a merge %s on a Gitlet node holding no chart answered %+v %v, want it unready there: "+
				"no chart", tc.name, res, err)
		case !tc.unready && (err != nil || res.Merge == nil || res.Merge.Refusal != gitlet.MergeRefusalChangeNotFound):
			t.Errorf("a merge %s on a Gitlet node holding no chart answered %+v %v, want it judged: "+
				"change_not_found", tc.name, res, err)
		}
	}
}
