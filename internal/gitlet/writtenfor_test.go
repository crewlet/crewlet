package gitlet_test

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// WHOM A SEAT'S CODE WRITE WAS MADE FOR, ON ITS RECORD (ADR-0109): beside the
// actor a routing names where the routing may wake a seat, and on an open, a
// reopen, a push's entry, a merge and an enqueue whatever they wake — each
// decided by the shipped committer or change writer, read back off the log.

// carriedRecord is the record at p on n's log: its version, its payload, and
// whom its routings carry, one entry a routing.
func carriedRecord(t *testing.T, n *node, p statelog.Position) (int, any, []*types.WorkingFor) {
	t.Helper()
	rec := n.recordAt(p)
	payload, err := gitlet.DecodeMutation(rec)
	if err != nil {
		t.Fatalf("decode the payload at %s: %v", p, err)
	}
	var carried []*types.WorkingFor
	for _, r := range gitlet.RoutingsOf(payload) {
		carried = append(carried, r.WorkingFor)
	}
	return rec.V, payload, carried
}

// audience is whom a case's write tells: an agent's seat, or persons alone.
type audience bool

const (
	toASeat   audience = true
	toPersons audience = false
)

// told is seat where a write's routing tells an agent's seat, and person where
// it tells persons alone.
func told[T any](to audience, seat, person T) T {
	if to == toASeat {
		return seat
	}
	return person
}

// A CODE RECORD CARRIES WHOM ITS AGENT WROTE IT FOR WHERE IT WAKES A SEAT
// (ADR-0109): a comment, a review, an ask to review, a close told to the
// change's watchers, a dequeue, a merge and a push that merges a change, each
// written by the seat dev in a turn founder woke, carry founder on the routing
// beside dev — and are stamped version 2, where a build that cannot read the
// carrier retains them — when the routing tells an agent's seat: cto named,
// cto's change reviewed, plat-lead asked or watching, cto's enqueue taken
// out, cto's change merged. The same writes telling persons alone — founder
// named, asked, watching, founder's change reviewed, founder's enqueue taken
// out, founder's change merged — carry nobody on the routing: a person reads
// a notice, and no turn they would start carries it. And the writes of a turn
// nobody woke carry nobody, at version 1. A merge's and a push's own payload
// carries founder whatever its routing tells
// (TestAnOpenAPushAMergeAndAnEnqueueCarryWhomTheyWereMadeForWhateverTheyWake),
// so their records are at version 2 while they tell persons alone.
//
// Mutations: stamp every routing, whatever it wakes — the writes telling
// persons alone carry founder, red; never stamp — the writes waking a seat
// carry nobody, red.
func TestACodeRecordCarriesWhomItsAgentWroteItForWhereItWakesASeat(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	founder := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "founder"}
	repos := 0
	name := func() string {
		repos++
		return fmt.Sprintf("carried-%d", repos)
	}
	// fresh is a fresh repository and a change on it from feature into main,
	// opened by owner.
	fresh := func(owner gitlet.Principal) (repo, change string) {
		t.Helper()
		repo = changeRepo(t, n, name())
		opened, err := c.ProposeChange(t.Context(), opening(repo, featureRef, mainRef, owner))
		requireChanged(t, opened, err)
		return repo, opened.ChangeID
	}
	for _, write := range []struct {
		name string
		// make is the write, made for f and telling to, whose record's
		// position it answers.
		make func(f types.WrittenFor, to audience) statelog.Position
		// carries is whether the write's own payload carries whom it was
		// made for, which stamps its record whatever its routing carries.
		carries bool
	}{
		{"a comment naming somebody", func(f types.WrittenFor, to audience) statelog.Position {
			repo, change := fresh(dev)
			said := saying(repo, change, dev, "", "Look at the retry loop.")
			said.Comment.Mentions, said.WrittenFor = []string{told(to, "cto", "founder")}, f
			return converse(t, w, said).Position
		}, false},
		{"a review of somebody's change", func(f types.WrittenFor, to audience) statelog.Position {
			repo, change := fresh(told(to, cto, ada))
			review := reviewing(repo, change, dev, 1, gitlet.VerdictComment, nil)
			review.WrittenFor = f
			return converse(t, w, review).Position
		}, false},
		{"an ask to review", func(f types.WrittenFor, to audience) statelog.Position {
			repo, change := fresh(dev)
			ask := asking(repo, change, dev, told(to, "plat-lead", "founder"))
			ask.WrittenFor = f
			return converse(t, w, ask).Position
		}, false},
		{"a close told to the change's watchers", func(f types.WrittenFor, to audience) statelog.Position {
			repo, change := fresh(dev)
			converse(t, w, asking(repo, change, dev, told(to, "plat-lead", "founder")))
			closed := closing(repo, change, dev, n.change(change).version)
			closed.WrittenFor = f
			res, err := c.ProposeChange(t.Context(), closed)
			requireChanged(t, res, err)
			return res.Position
		}, false},
		{"a dequeue of somebody's enqueue", func(f types.WrittenFor, to audience) statelog.Position {
			repo, change := queueRepo(t, n, c, name())
			ready(t, n, w, repo, change)
			enqueue := enqueueing(repo, change, told(to, cto, ada), oidOf("B"))
			queued, err := c.ProposeQueue(t.Context(), enqueue)
			requireQueued(t, queued, err, gitlet.QueueWaiting, 1)
			out := dequeueing(repo, enqueue.Enqueue.EntryID, dev)
			out.WrittenFor = f
			res, err := c.ProposeQueue(t.Context(), out)
			if err != nil || res.Outcome != statelog.OutcomeApplied || res.Refusal != "" {
				t.Fatalf("dev's dequeue answered %+v %v, want applied", res, err)
			}
			return res.Position
		}, false},
		{"a merge of somebody's change", func(f types.WrittenFor, to audience) statelog.Position {
			repo, change := fresh(told(to, cto, ada))
			approved(t, n, w, repo, change)
			merge := merging(repo, change, dev, gitlet.MergeMethodFastForward, oidOf("A"), oidOf("B"), oidOf("B"))
			merge.WrittenFor = f
			res, err := c.ProposeMerge(t.Context(), merge)
			requireMerged(t, res, err, oidOf("B"))
			return res.Position
		}, true},
		{"a push merging somebody's change", func(f types.WrittenFor, to audience) statelog.Position {
			repo := changeRepo(t, n, name())
			opened, err := c.ProposeChange(t.Context(), opening(repo, featureRef, releaseRef, told(to, cto, ada)))
			requireChanged(t, opened, err)
			push := proposal(repo, update(releaseRef, oidOf("R"), oidOf("B")))
			push.Principal = gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "dev",
				Run: gitlet.RunProvenance{TurnID: uuid.Must(uuid.NewV7()).String()}}
			push.Via, push.WrittenFor = gitlet.ViaEngine, f
			res, err := c.Propose(t.Context(), push)
			requireOK(t, answer{res, err})
			return res.Position
		}, true},
	} {
		for _, tc := range []struct {
			name    string
			f       types.WrittenFor
			to      audience
			routing *types.WorkingFor
		}{
			{"made for founder, waking a seat", types.ForTurn(founder), toASeat, founder},
			{"made for founder, telling persons alone", types.ForTurn(founder), toPersons, nil},
			{"in a turn nobody woke", types.ForTurn(nil), toASeat, nil},
		} {
			v, _, carried := carriedRecord(t, n, write.make(tc.f, tc.to))
			want := 1
			if tc.routing != nil || (write.carries && tc.f.Carrier() != nil) {
				want = gitlet.WorkingForVersion
			}
			switch {
			case len(carried) != 1:
				t.Errorf("%s %s carries %d routings, want the one the case reads", write.name, tc.name,
					len(carried))
			case !reflect.DeepEqual(carried[0], tc.routing):
				t.Errorf("%s %s: its routing carries %+v, want %+v", write.name, tc.name, carried[0], tc.routing)
			}
			if v != want {
				t.Errorf("%s %s is stamped version %d, want %d", write.name, tc.name, v, want)
			}
		}
	}
}

// AN OPEN, A REOPEN, A PUSH, A MERGE AND AN ENQUEUE CARRY WHOM THEY WERE MADE
// FOR WHATEVER THEY WAKE (ADR-0109): the engine's later records read each one
// back — a revision's facts its open or its push, the home's landing the push
// that merged a change, a queue's landing and its close the entry's enqueue —
// so each of dev's, written for founder, carries founder on its own payload
// and is stamped version 2, even where it has no routing — an open naming
// nobody, a reopen nobody watches, a push moving dev's change's head, and an
// enqueue, which routes nobody — or one that tells nobody: dev's merge of
// dev's own change, whose one author merged it. Each made for nobody — the
// seat's own work — carries nothing, at version 1.
//
// Mutation: an open's payload left without its carrier — the open made for
// founder carries nobody, at version 1, red.
func TestAnOpenAPushAMergeAndAnEnqueueCarryWhomTheyWereMadeForWhateverTheyWake(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	founder := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "founder"}
	repos := 0
	name := func() string {
		repos++
		return fmt.Sprintf("made-for-%d", repos)
	}
	for _, write := range []struct {
		name string
		// make is the write made for f, and the position of its record.
		make func(f types.WrittenFor) statelog.Position
		// carrier is whom the record's own payload carries.
		carrier func(payload any) *types.WorkingFor
	}{
		{"an open naming nobody", func(f types.WrittenFor) statelog.Position {
			open := opening(changeRepo(t, n, name()), featureRef, mainRef, dev)
			open.WrittenFor = f
			res, err := c.ProposeChange(t.Context(), open)
			requireChanged(t, res, err)
			return res.Position
		}, func(payload any) *types.WorkingFor {
			p, ok := payload.(gitlet.ChangeOpen)
			if !ok || p.Routing != nil {
				t.Fatalf("the open's record carries %T, want an open naming nobody", payload)
			}
			return p.WorkingFor
		}},
		{"a reopen nobody watches", func(f types.WrittenFor) statelog.Position {
			repo := changeRepo(t, n, name())
			opened, err := c.ProposeChange(t.Context(), opening(repo, featureRef, mainRef, dev))
			requireChanged(t, opened, err)
			closed, err := c.ProposeChange(t.Context(), closing(repo, opened.ChangeID, dev, opened.Version))
			requireChanged(t, closed, err)
			reopen := reopening(repo, opened.ChangeID, dev, closed.Version)
			reopen.WrittenFor = f
			res, err := c.ProposeChange(t.Context(), reopen)
			requireChanged(t, res, err)
			return res.Position
		}, func(payload any) *types.WorkingFor {
			p, ok := payload.(gitlet.ChangeReopen)
			if !ok || p.Routing != nil {
				t.Fatalf("the reopen's record carries %T, want a reopen telling nobody", payload)
			}
			return p.WorkingFor
		}},
		{"a push moving a change's head", func(f types.WrittenFor) statelog.Position {
			repo, _ := mergeRepo(t, n, c, name(), gitlet.DirectPersons, mergeGate())
			push := proposal(repo, update(seatRef, oidOf("B"), oidOf("C")))
			push.Principal = gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "dev",
				Run: gitlet.RunProvenance{TurnID: uuid.Must(uuid.NewV7()).String()}}
			push.Via, push.WrittenFor = gitlet.ViaEngine, f
			res, err := c.Propose(t.Context(), push)
			requireOK(t, answer{res, err})
			return res.Position
		}, func(payload any) *types.WorkingFor {
			p, ok := payload.(gitlet.PushBatch)
			if !ok || len(p.Pushes) != 1 || len(p.Pushes[0].Routing) != 0 {
				t.Fatalf("the push's record carries %+v, want one entry moving a head, which routes nobody",
					payload)
			}
			return p.Pushes[0].WorkingFor
		}},
		{"a merge of dev's own change", func(f types.WrittenFor) statelog.Position {
			repo, change := mergeRepo(t, n, c, name(), gitlet.DirectPersons, mergeGate())
			approved(t, n, w, repo, change)
			merge := merging(repo, change, dev, gitlet.MergeMethodFastForward, oidOf("A"), oidOf("B"), oidOf("B"))
			merge.WrittenFor = f
			res, err := c.ProposeMerge(t.Context(), merge)
			requireMerged(t, res, err, oidOf("B"))
			return res.Position
		}, func(payload any) *types.WorkingFor {
			// ITS ROUTING TELLS NOBODY: its one author is who merged it.
			p, ok := payload.(gitlet.Merge)
			if !ok || p.Routing == nil || p.Routing.WorkingFor != nil {
				t.Fatalf("the merge's record carries %+v, want a merge whose routing tells nobody, and so "+
					"carries nobody", payload)
			}
			return p.WorkingFor
		}},
		{"an enqueue", func(f types.WrittenFor) statelog.Position {
			repo, change := queueRepo(t, n, c, name())
			ready(t, n, w, repo, change)
			enqueue := enqueueing(repo, change, dev, oidOf("B"))
			enqueue.WrittenFor = f
			res, err := c.ProposeQueue(t.Context(), enqueue)
			requireQueued(t, res, err, gitlet.QueueWaiting, 1)
			return res.Position
		}, func(payload any) *types.WorkingFor {
			p, ok := payload.(gitlet.Enqueue)
			if !ok {
				t.Fatalf("the enqueue's record carries %T, want an enqueue", payload)
			}
			return p.WorkingFor
		}},
	} {
		for _, tc := range []struct {
			name string
			f    types.WrittenFor
			want *types.WorkingFor
			v    int
		}{
			{"made for founder", types.ForTurn(founder), founder, gitlet.WorkingForVersion},
			{"the seat's own work", types.ForTurn(nil), nil, 1},
		} {
			v, payload, _ := carriedRecord(t, n, write.make(tc.f))
			if got := write.carrier(payload); !reflect.DeepEqual(got, tc.want) || v != tc.v {
				t.Errorf("%s %s carries %+v at version %d, want %+v at %d", write.name, tc.name, got, v, tc.want,
					tc.v)
			}
		}
	}
}
