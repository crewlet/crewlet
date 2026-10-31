package code_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE REF LIFECYCLE (M3-decisions M3-O54, M3-O62, M3-O64): a seat's branch
// nobody has set for code.RefExpiry is deleted by the code-retention pass,
// through a push on the engine's own account to its repository's committer —
// unless a live turn holds it, or its committer finds it no longer idle. Each
// case runs the real committer and applier of the harness's node, its pass at
// the harness's clock.

// idleSince is when a case's idle seat branches were set: a day past
// code.RefExpiry before the pass's clock.
var idleSince = harnessAt.Add(-code.RefExpiry - 24*time.Hour)

// seatBranch is the seat branch every case expires or keeps.
const seatBranch = "refs/heads/seats/dev/x"

// pushAt publishes one record of entries at the broker instant at — the `at`
// every ref it sets carries.
func (n *node) pushAt(at time.Time, repo string, entries ...code.PushEntry) {
	n.t.Helper()
	n.storedAt = func(uint64, time.Time) time.Time { return at }
	defer func() { n.storedAt = nil }()
	n.push(repo, entries...)
}

// newPushID is a fresh push id.
func newPushID() string { return uuid.Must(uuid.NewV7()).String() }

// seatSets is dev's push setting ref to oid from nothing.
func seatSets(ref, oid string) code.PushEntry {
	return seatPush(newPushID(), "dev", set(ref, code.ZeroOID, oid))
}

// deletion is an undecided command deleting ref off old.
func deletion(ref, old string) code.Command {
	return code.Command{Ref: ref, Old: old, New: code.ZeroOID}
}

// expiryOf is the ref lifecycle's push of cmds to repo, formed from a page
// read at seen, at the pass's clock.
func expiryOf(repo string, seen statelog.Position, cmds ...code.Command) code.Proposal {
	return code.Proposal{PushID: newPushID(), RepoID: repo, Principal: code.Principal{Kind: code.PrincipalSystem},
		Via: code.ViaExpiry, Node: "node-a", Commands: cmds, ReceivedAt: harnessAt, Seen: seen}
}

// listed is the position of the expiry's first page over n at the pass's
// clock: what every proposal formed from it carries as its Seen.
func (n *node) listed() statelog.Position {
	n.t.Helper()
	_, seen, err := code.ExpiryPage(n.t.Context(), n.reader, harnessAt.Add(-code.RefExpiry))
	if err != nil {
		n.t.Fatalf("list the idle seat branches: %v", err)
	}
	return seen
}

// targetOf is ref's target in repository id, "" for a tombstone or no row.
func (n *node) targetOf(id, ref string) string {
	n.t.Helper()
	var target string
	err := n.db.Reader().Read(n.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(n.t.Context(), `SELECT target FROM code_refs WHERE repo_id = ? AND ref = ?`, id,
			ref).Scan(&target)
	})
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		n.t.Fatalf("read %s of %s: %v", ref, id, err)
	}
	return target
}

// AN EXPIRY PROPOSAL IS VALIDATED AS ONE: the engine's own account and the
// expiry pair both ways; and the push deletes seats' branches and nothing
// else — no create, no other ref, nothing decided by anybody but its
// committer, no pack, no introduced commit, no --atomic, no change — naming
// the page it was listed from. Each refused naming its field.
//
// Mutation: accept a create in an expiry → the create row passes, red.
func TestAnExpiryProposalIsValidatedAsOne(t *testing.T) {
	t.Parallel()
	seen := statelog.Position{Stream: "CREWLET_CODE_LOG", Generation: 1, Seq: 9}
	repo := uuid.NewString()
	good := expiryOf(repo, seen, deletion(seatBranch, oidOf("x")))
	if err := good.Validate(); err != nil {
		t.Fatalf("the ref lifecycle's push: %v, want it valid", err)
	}
	for _, c := range []struct {
		name  string
		edit  func(p *code.Proposal)
		field string
	}{
		{"a seat's expiry", func(p *code.Proposal) { p.Principal = agent("dev") }, "principal.kind:"},
		{"the engine's git push", func(p *code.Proposal) { p.Via = code.ViaGit }, "via:"},
		{"the engine for a seat", func(p *code.Proposal) { p.Principal.Seat = "dev" }, "principal.seat:"},
		{"the engine with a credential", func(p *code.Proposal) { p.Principal.OperatorID = "tok" },
			"principal.operator_id:"},
		{"a create", func(p *code.Proposal) {
			p.Commands = []code.Command{{Ref: seatBranch, Old: code.ZeroOID, New: oidOf("y")}}
		}, "commands[0].new:"},
		{"an update", func(p *code.Proposal) {
			p.Commands = []code.Command{{Ref: seatBranch, Old: oidOf("x"), New: oidOf("y")}}
		}, "commands[0].new:"},
		{"main", func(p *code.Proposal) { p.Commands = []code.Command{deletion("refs/heads/main", oidOf("x"))} },
			"commands[0].ref:"},
		{"a seat's namespace itself", func(p *code.Proposal) {
			p.Commands = []code.Command{deletion("refs/heads/seats/dev", oidOf("x"))}
		}, "commands[0].ref:"},
		{"a receiver's refusal", func(p *code.Proposal) {
			p.Commands[0].Result, p.Commands[0].Reason = code.ResultNG, code.RefusalStale
		}, "commands[0].result:"},
		{"a pack", func(p *code.Proposal) { p.Pack = suitePack(p.PushID) }, "pack:"},
		{"an introduced commit", func(p *code.Proposal) {
			p.Introduced, p.IntroducedCount = []string{oidOf("c")}, 1
		}, "introduced:"},
		{"--atomic", func(p *code.Proposal) { p.Atomic = true }, "atomic:"},
		{"a change", func(p *code.Proposal) { p.Change = &code.ChangeOption{Title: "x"} }, "change:"},
		{"no page", func(p *code.Proposal) { p.Seen = statelog.Position{} }, "seen:"},
	} {
		p := expiryOf(repo, seen, deletion(seatBranch, oidOf("x")))
		c.edit(&p)
		err := p.Validate()
		if !errors.Is(err, code.ErrInvalidProposal) || !strings.Contains(err.Error(), c.field) {
			t.Errorf("%s: Validate() = %v, want ErrInvalidProposal naming %s", c.name, err, c.field)
		}
	}
}

// THE DECIDE KEEPS A BRANCH THAT IS NO LONGER IDLE (rule 4b), answering each
// command stale with the reason, and deletes the one that still is:
//
//   - pushed again since its page — rule 4's own answer, "fetch first", since
//     rule 4b runs after it;
//   - set within code.RefExpiry by the pass's clock;
//   - force-pushed away and back by a person: its target is the same, and no
//     seat set it last;
//   - held by a live turn RECORDED after its page — and one recorded before
//     the page is deleted, the pass's own read of the holds having answered it
//     (M3-decisions M3-O61);
//   - its repository's default branch (M3-decisions M3-O62), and the same
//     branch of a repository whose default is main deleted;
//   - the source of a live change, whose deletion would close it;
//   - under any protection that matches it: every rule refuses the engine's
//     push protected_direct (direct_push persons takes a person's alone, none
//     nobody's), so allow_delete never comes to be asked.
//
// Mutations: drop ExpiryRule's held term → the held branch is deleted, red;
// compare it with >= → the branch held before its page is kept, red; drop the
// default term → the default branch is deleted, red; drop the change term →
// the change's source is deleted, red; run 4b before rule 4 → the moved
// branch is answered with 4b's sentence, red; skip 4b → the person's branch is
// deleted, red.
func TestTheDecideKeepsABranchThatIsNoLongerIdle(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	old := oidOf("idle")
	branch := func(name string, at time.Time, e code.PushEntry) string {
		id := n.made(lcPerson, name, unitOwned("eng"))
		n.pushAt(at, id, e)
		return id
	}
	idle := func(name string) string { return branch(name, idleSince, seatSets(seatBranch, old)) }
	var (
		deleted    = idle("deleted")
		moved      = idle("moved")
		recent     = branch("recent", harnessAt.Add(-code.RefExpiry+time.Hour), seatSets(seatBranch, old))
		person     = idle("person")
		held       = idle("held")
		heldBefore = idle("held-before")
		deflt      = idle("default")
		otherDeflt = idle("default-main")
		protected  = idle("protected")
		guarded    = idle("guarded")
	)
	opened := seatSets(seatBranch, old)
	opened.Change = &code.ChangeOption{Target: "refs/heads/main", Title: "the work under review"}
	change := branch("change", idleSince, opened)

	// BEFORE THE PAGE: a hold, the default branch, the protections.
	_, read := n.readRef("held-before", seatBranch)
	n.lands(lcRefHold(agent("dev"), heldBefore, "turn-0", seatBranch, read))
	n.lands(lcSettings(lcPerson, deflt, code.RepoChange{DefaultBranch: ptr("seats/dev/x")}))
	n.lands(lcProtect(lcPerson, protected, "refs/heads/seats/dev/*",
		&code.Protection{PushRule: code.PushRule{DirectPush: code.DirectPersons}}))
	n.lands(lcProtect(lcPerson, guarded, "refs/heads/seats/dev/*",
		&code.Protection{PushRule: code.PushRule{DirectPush: code.DirectNone, AllowDelete: true}}))
	seen := n.listed()

	// AFTER IT: a push, a person's force-push away and back, a hold.
	n.pushAt(idleSince, moved, seatPush(newPushID(), "dev", set(seatBranch, old, oidOf("moved"))))
	n.push(person, entry(newPushID(), set(seatBranch, old, oidOf("away"))),
		entry(newPushID(), set(seatBranch, oidOf("away"), old)))
	_, read = n.readRef("held", seatBranch)
	n.lands(lcRefHold(agent("dev"), held, "turn-1", seatBranch, read))

	for _, tc := range []struct {
		name, id string
		reason   code.Refusal
		detail   string
	}{
		{"an idle branch", deleted, "", ""},
		{"pushed since its page", moved, code.RefusalStale, "fetch first"},
		{"set within the window", recent, code.RefusalStale, "pushed within code.RefExpiry"},
		{"a person pushed last", person, code.RefusalStale, "somebody else pushed it last"},
		{"held since its page", held, code.RefusalStale, "held by a live turn since the expiry listed it"},
		{"held before its page", heldBefore, "", ""},
		{"the default branch", deflt, code.RefusalStale, "the repository's default branch"},
		{"not the default branch", otherDeflt, "", ""},
		{"a live change's source", change, code.RefusalStale, "the source or the target of a live change"},
		{"under direct_push persons", protected, code.RefusalProtectedDirect, ""},
		{"under direct_push none, deletable", guarded, code.RefusalProtectedDirect, ""},
	} {
		res, err := c.Propose(t.Context(), expiryOf(tc.id, seen, deletion(seatBranch, old)))
		if err != nil || len(res.Answer.Commands) != 1 {
			t.Errorf("%s: the expiry was answered %+v, %v", tc.name, res, err)
			continue
		}
		got := res.Answer.Commands[0]
		switch {
		case tc.reason == "" && (got.Result != code.ResultOK || res.Outcome != statelog.OutcomeApplied):
			t.Errorf("%s: answered %+v (%s), want it deleted", tc.name, got, res.Outcome)
		case tc.reason == "" && n.targetOf(tc.id, seatBranch) != "":
			t.Errorf("%s: answered deleted and the branch is still live", tc.name)
		case tc.reason != "" && (got.Result != code.ResultNG || got.Reason != tc.reason ||
			!strings.Contains(got.Detail, tc.detail)):
			t.Errorf("%s: answered %+v, want %s %q", tc.name, got, tc.reason, tc.detail)
		case tc.reason != "" && (res.Outcome != "" || n.targetOf(tc.id, seatBranch) == ""):
			t.Errorf("%s: kept, and a record was written (%s) or the branch is gone", tc.name, res.Outcome)
		}
	}
}

// AN IDLE SEAT BRANCH EXPIRES THROUGH ITS PUSH ROW: the deletion is a push on
// the engine's own account — via expiry, actor_kind system, no actor, no seat,
// no run — whose command keeps the commit the branch was at, which is what a
// restore reads; the ref is a tombstone; and the repository's last push and
// idle clock stay where the seat's last push left them (M3-decisions M3-O54):
// the engine's housekeeping is not anybody working there. The pushes ever and
// the version move as for every record.
//
// Mutation: drop the CASE from the counters' statement → the clocks move to
// the expiry's instant, red.
func TestAnIdleSeatBranchExpires(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	old := oidOf("idle")
	id := n.made(lcPerson, "web", unitOwned("eng"))
	n.pushAt(idleSince, id, seatSets(seatBranch, old))
	before := n.activityOf(id)
	p := expiryOf(id, n.listed(), deletion(seatBranch, old))
	res, err := c.Propose(t.Context(), p)
	if err != nil || res.Outcome != statelog.OutcomeApplied || res.Answer.Commands[0].Result != code.ResultOK {
		t.Fatalf("the expiry = %+v, %v; want the branch deleted", res, err)
	}
	if got := n.targetOf(id, seatBranch); got != "" {
		t.Errorf("%s is at %s after its expiry", seatBranch, got)
	}
	var via, actor, kind, seat, turn, commands string
	if err := n.db.Reader().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT via, actor, actor_kind, actor_seat, turn_id, commands
			FROM code_pushes WHERE push_id = ?`, p.PushID).Scan(&via, &actor, &kind, &seat, &turn, &commands)
	}); err != nil {
		t.Fatalf("read the expiry's push row: %v", err)
	}
	if via != string(code.ViaExpiry) || actor != "" || kind != string(code.PrincipalSystem) || seat != "" ||
		turn != "" || !strings.Contains(commands, old) {
		t.Errorf("the expiry's row is via %q actor %q kind %q seat %q turn %q commands %s, want the engine's "+
			"expiry keeping %s", via, actor, kind, seat, turn, commands, old)
	}
	after := n.activityOf(id)
	if after.lastActivity != before.lastActivity || after.lastPush != before.lastPush ||
		after.version <= before.version {
		t.Errorf("the expiry moved the activity %+v to %+v, want the clocks kept and the version moved", before,
			after)
	}
}

// AN EXPIRY DOES NOT MOVE A REPOSITORY'S ACTIVITY — but only a record whose
// every entry is one (M3-decisions M3-O54): the committer batches an expiry
// with whatever else is queued, and a record carrying a person's push beside
// it moves both clocks to its instant, as any push does.
//
// Mutations: drop the CASE → the expiry alone moves the clocks, red; condition
// it on "any expiry entry in the record" → the mixed record keeps them, red.
func TestAnExpiryDoesNotMoveARepositorysActivity(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	id := n.made(lcPerson, "web", unitOwned("eng"))
	n.pushAt(idleSince, id, seatSets(seatBranch, oidOf("x")), seatSets("refs/heads/seats/dev/y", oidOf("y")))
	expiry := func(ref, old string) code.PushEntry {
		return code.PushEntry{PushID: newPushID(), Principal: code.Principal{Kind: code.PrincipalSystem},
			Via: code.ViaExpiry, Node: "node-a", ReceivedAt: harnessAt,
			Commands: []code.Command{{Ref: ref, Old: old, New: code.ZeroOID, Result: code.ResultOK}}}
	}
	before := n.activityOf(id)
	n.push(id, expiry(seatBranch, oidOf("x")))
	alone := n.activityOf(id)
	if alone.lastActivity != before.lastActivity || alone.lastPush != before.lastPush {
		t.Errorf("a record of an expiry alone moved the activity %+v to %+v", before, alone)
	}
	n.push(id, expiry("refs/heads/seats/dev/y", oidOf("y")), entry(newPushID(), set("refs/heads/main",
		code.ZeroOID, oidOf("main"))))
	mixed := n.activityOf(id)
	if mixed.lastActivity == alone.lastActivity || mixed.lastPush == alone.lastPush ||
		mixed.lastActivity != mixed.lastPush {
		t.Errorf("a record of an expiry and a person's push moved the activity %+v to %+v, want both clocks at "+
			"its instant", alone, mixed)
	}
}

// proposals records every expiry a pass hands its committer, and hands it on.
type proposals struct {
	mu   sync.Mutex
	c    *code.Committer
	seen []code.Proposal
	// answer, when set, answers in the committer's place.
	answer func(code.Proposal) (code.ProposeResult, error)
}

func (p *proposals) Propose(ctx context.Context, prop code.Proposal) (code.ProposeResult, error) {
	p.mu.Lock()
	p.seen = append(p.seen, prop)
	answer := p.answer
	p.mu.Unlock()
	if answer != nil {
		return answer(prop)
	}
	return p.c.Propose(ctx, prop)
}

// to is every proposal made to repository id.
func (p *proposals) to(id string) []code.Proposal {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []code.Proposal
	for _, prop := range p.seen {
		if prop.RepoID == id {
			out = append(out, prop)
		}
	}
	return out
}

// expiryPass is a retention pass at the harness's clock whose ref expiry
// proposes through props and asks h which keys live turns hold.
func expiryPass(n *node, props *proposals, h func(context.Context, []string) (map[string]bool, error)) *code.Retention {
	var p purged
	r := passOver(n.store, n.reader, &p)
	r.Propose, r.Holds = props.Propose, h
	return r
}

// THE RETENTION PASS EXPIRES IDLE SEAT BRANCHES, and only those no live turn
// holds: a seat branch idle past code.RefExpiry is deleted by a push of the
// engine's own; one set within the window is kept; a branch a person set in
// the namespace, a tag the seat pushed and main are never listed; a branch a
// live turn holds is kept and counted held — and deleted by the next pass once
// nobody holds it; an archived repository's branch is kept and proposed
// nothing, and so is a repository's default branch. Every proposal passes
// Proposal.Validate.
//
// Mutations: ignore what the holds answer → the held branch is deleted, red;
// skip the default-branch filter → the default is proposed (and kept by
// ExpiryRule, so the assertion of no proposal turns red); drop the status
// filter → the archived repository is proposed, red.
func TestRetentionExpiresIdleSeatBranches(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	web := n.made(lcPerson, "web", unitOwned("eng"))
	docs := n.made(lcPerson, "docs", unitOwned("eng"))
	old := n.made(lcPerson, "old", unitOwned("eng"))
	const (
		recent   = "refs/heads/seats/dev/recent"
		mine     = "refs/heads/seats/dev/persons"
		heldRef  = "refs/heads/seats/dev/held"
		tag      = "refs/tags/seats-dev-v1"
		mainline = "refs/heads/main"
	)
	n.pushAt(idleSince, web, seatSets(seatBranch, oidOf("x")), seatSets(heldRef, oidOf("h")),
		seatSets(tag, oidOf("t")), entry(newPushID(), set(mine, code.ZeroOID, oidOf("p")),
			set(mainline, code.ZeroOID, oidOf("m"))))
	n.pushAt(harnessAt.Add(-code.RefExpiry+time.Hour), web, seatSets(recent, oidOf("r")))
	n.pushAt(idleSince, docs, seatSets(seatBranch, oidOf("d")))
	n.lands(lcSettings(lcPerson, docs, code.RepoChange{DefaultBranch: ptr("seats/dev/x")}))
	n.pushAt(idleSince, old, seatSets(seatBranch, oidOf("o")))
	n.lands(lcOn(code.LifecycleArchive, lcPerson, old))

	props := &proposals{c: c}
	h := &holding{held: map[string]bool{code.RefHoldKey(web, heldRef): true}}
	report, err := expiryPass(n, props, h.Holds).Pass(t.Context())
	if err != nil || report.RefsExpired != 1 || report.RefsHeld != 1 || report.RefsKept != 0 ||
		len(report.ExpiryFailed) != 0 || report.ExpiryUnread != "" {
		t.Fatalf("the pass = %+v, %v; want one branch expired and one held", report, err)
	}
	for ref, live := range map[string]bool{seatBranch: false, recent: true, mine: true, heldRef: true, tag: true,
		mainline: true} {
		if got := n.targetOf(web, ref) != ""; got != live {
			t.Errorf("%s of web is live %v after the pass, want %v", ref, got, live)
		}
	}
	for name, id := range map[string]string{"docs": docs, "old": old} {
		if n.targetOf(id, seatBranch) == "" {
			t.Errorf("%s's %s was expired", name, seatBranch)
		}
		if got := props.to(id); len(got) != 0 {
			t.Errorf("%s was proposed %d expiries, want none", name, len(got))
		}
	}
	for _, p := range props.to(web) {
		if err := p.Validate(); err != nil {
			t.Errorf("the pass proposed %+v: %v", p, err)
		}
	}
	// THE HOLD ENDS, and the next pass expires the branch it kept.
	h.held = nil
	report, err = expiryPass(n, props, h.Holds).Pass(t.Context())
	if err != nil || report.RefsExpired != 1 || n.targetOf(web, heldRef) != "" {
		t.Fatalf("the pass after the hold ended = %+v, %v; want %s expired", report, err, heldRef)
	}
}

// A PAGE IS PROPOSED IN PUSHES THE DOMAIN TAKES: 2 500 idle branches of one
// repository are three proposals, one a page of code.MaxCommandsPerPush; and
// 700 whose names JSON spells six bytes a character are cut by the entry's
// room too — two proposals, each one Proposal.Validate admits — so no page is
// refused for its width on every pass.
//
// Mutation: cut by MaxCommandsPerPush alone → the wide page is one proposal,
// refused TooWideError, nothing expired, red.
func TestAnExpiryIsProposedInPushesTheDomainTakes(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// refs is how many idle branches, each name's bytes at width; parts
		// how many records seed them; want how many proposals expire them.
		refs, width, parts, want int
	}{
		{"2 500 plain branches", 2500, 32, 3, 3},
		{"700 of the longest branches", 700, code.MaxRefNameBytes, 2, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			n := newNode(t)
			n.chart = testChart()
			committer, _ := committerOn(t, n, n.pub)
			id := n.made(lcPerson, "web", unitOwned("eng"))
			per := (c.refs + c.parts - 1) / c.parts
			for part := range c.parts {
				var cmds []code.Command
				for i := part * per; i < min(c.refs, (part+1)*per); i++ {
					name := code.SeatNamespace("dev") + fmt.Sprintf("%05d", i)
					cmds = append(cmds, set(name+strings.Repeat("<", c.width-len(name)), code.ZeroOID,
						oidOf(fmt.Sprint(i))))
				}
				n.pushAt(idleSince, id, seatPush(newPushID(), "dev", cmds...))
			}
			props := &proposals{c: committer}
			report, err := expiryPass(n, props, holdsNone).Pass(t.Context())
			if err != nil || report.RefsExpired != c.refs || len(report.ExpiryFailed) != 0 {
				t.Fatalf("the pass = expired %d failed %v (%v), want all %d branches expired", report.RefsExpired,
					report.ExpiryFailed, err, c.refs)
			}
			got := props.to(id)
			if len(got) != c.want {
				t.Errorf("%d idle branches were %d proposals, want %d", c.refs, len(got), c.want)
			}
			for _, p := range got {
				if err := p.Validate(); err != nil {
					t.Errorf("a proposal of %d branches: %v", len(p.Commands), err)
				}
			}
		})
	}
}

// A PAGE WHOSE EVERY BRANCH WAS SET SINCE ITS LISTING IS KEPT, NOT FAILED: its
// proposal is answered with no record and no refusal of the whole push, and
// each branch is counted kept.
//
// Mutation: read an answer with no outcome as a failure → expiry_failed, red.
func TestAnExpiryOfNothingIdleIsCountedKept(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	id := n.made(lcPerson, "web", unitOwned("eng"))
	n.pushAt(idleSince, id, seatSets(seatBranch, oidOf("x")))
	props := &proposals{c: c}
	// THE SEAT PUSHES IT AGAIN AFTER THE PAGE: the holds are read after the
	// page, so the push lands between the listing and the proposal.
	pushed := func(ctx context.Context, keys []string) (map[string]bool, error) {
		n.pushAt(idleSince, id, seatPush(newPushID(), "dev", set(seatBranch, oidOf("x"), oidOf("x2"))))
		return holdsNone(ctx, keys)
	}
	report, err := expiryPass(n, props, pushed).Pass(t.Context())
	if err != nil || report.RefsKept != 1 || report.RefsExpired != 0 || len(report.ExpiryFailed) != 0 {
		t.Fatalf("the pass = %+v, %v; want the branch counted kept", report, err)
	}
	if n.targetOf(id, seatBranch) != oidOf("x2") {
		t.Error("the branch pushed since its page was deleted")
	}
}

// AN EXPIRY YIELDS TO A BUSY COMMITTER: busy ends the step, the pass reports
// Remain, and the next pass resumes from the rows.
//
// Mutation: read busy as a failure and go on → Remain false, red.
func TestAnExpiryYieldsToABusyCommitter(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	a := n.made(lcPerson, "a", unitOwned("eng"))
	b := n.made(lcPerson, "b", unitOwned("eng"))
	n.pushAt(idleSince, a, seatSets(seatBranch, oidOf("a")))
	n.pushAt(idleSince, b, seatSets(seatBranch, oidOf("b")))
	props := &proposals{c: c, answer: func(p code.Proposal) (code.ProposeResult, error) {
		return code.ProposeResult{Answer: code.PushAnswer{PushID: p.PushID, Refused: code.RefusalBusy,
			Commands: p.Commands}}, nil
	}}
	report, err := expiryPass(n, props, holdsNone).Pass(t.Context())
	if err != nil || !report.Remain || len(props.seen) != 1 || len(report.ExpiryFailed) != 0 {
		t.Fatalf("a busy committer's pass = %+v (%d proposals), %v; want it stopped at the first, Remain",
			report, len(props.seen), err)
	}
	props.answer = nil
	report, err = expiryPass(n, props, holdsNone).Pass(t.Context())
	if err != nil || report.RefsExpired != 2 || report.Remain {
		t.Fatalf("the next pass = %+v, %v; want both branches expired", report, err)
	}
}

// AN UNREAD HOLD SKIPS THE REF EXPIRY: the live turns' holds that cannot be
// read expire nothing — no proposal, ExpiryUnread saying why, and the WARN
// line naming the page.
//
// Mutation: read the error as "nothing held" → the branch is deleted, red.
func TestAnUnreadHoldSkipsTheRefExpiry(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	id := n.made(lcPerson, "web", unitOwned("eng"))
	n.pushAt(idleSince, id, seatSets(seatBranch, oidOf("x")))
	var logged strings.Builder
	props := &proposals{c: c}
	r := expiryPass(n, props, func(context.Context, []string) (map[string]bool, error) {
		return nil, errors.New("the run store is unreachable")
	})
	r.Logger = slog.New(slog.NewJSONHandler(&logged, nil))
	report, err := r.Pass(t.Context())
	if err != nil || !strings.Contains(report.ExpiryUnread, "the run store is unreachable") || len(props.seen) != 0 {
		t.Fatalf("the pass = %+v (%d proposals), %v; want nothing proposed and the reason said", report,
			len(props.seen), err)
	}
	if !strings.Contains(logged.String(), "code_ref_expiry_skipped") {
		t.Errorf("no code_ref_expiry_skipped line was logged:\n%s", logged.String())
	}
	if n.targetOf(id, seatBranch) == "" {
		t.Error("a branch whose holds could not be read was expired")
	}
}

// A PAGE OF ARCHIVED BRANCHES ASKS NO HOLDS: nothing of it is proposed, so the
// run store and the seat holders are not asked about it.
//
// Mutation: ask for every page → the fake records a call, red.
func TestAPageOfArchivedBranchesAsksNoHolds(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	id := n.made(lcPerson, "old", unitOwned("eng"))
	n.pushAt(idleSince, id, seatSets(seatBranch, oidOf("x")))
	n.lands(lcOn(code.LifecycleArchive, lcPerson, id))
	h := &holding{}
	report, err := expiryPass(n, &proposals{c: c}, h.Holds).Pass(t.Context())
	if err != nil || len(h.asked) != 0 || report.RefsExpired != 0 {
		t.Fatalf("a page of archived branches = %+v, %v, asked %v; want no hold asked", report, err, h.asked)
	}
}

// A REF HOLD TAKEN DURING A PASS IS NEVER LOST: the pass reads the holds after
// its page, so a turn that holds the branch after that read records its hold
// on the repository's subject past the page — and the decide keeps the branch
// stale. And an archive in between changes nothing: a hold on an archived
// repository's branch is recorded all the same, so the branch is kept once it
// is unarchived before the proposal.
//
// Mutations: drop ExpiryRule's held term → the branch is deleted, red; read the
// holds before the page → the record lands below the page's position and the
// branch is deleted, red; answer a hold in an archived repository with no
// record → the arm's branch is deleted, red.
func TestARefHoldTakenDuringAPassIsNeverLost(t *testing.T) {
	t.Parallel()
	for _, archived := range []bool{false, true} {
		t.Run(fmt.Sprintf("archived %v", archived), func(t *testing.T) {
			t.Parallel()
			n := newNode(t)
			n.chart = testChart()
			c, _ := committerOn(t, n, n.pub)
			id := n.made(lcPerson, "web", unitOwned("eng"))
			n.pushAt(idleSince, id, seatSets(seatBranch, oidOf("x")))
			// THE TURN HOLDS THE BRANCH JUST AFTER THE PASS READ ITS HOLDS.
			late := func(ctx context.Context, keys []string) (map[string]bool, error) {
				_, read := n.readRef("web", seatBranch)
				if archived {
					n.lands(lcOn(code.LifecycleArchive, lcPerson, id))
				}
				n.lands(lcRefHold(agent("dev"), id, "turn-late", seatBranch, read))
				if archived {
					n.lands(lcOn(code.LifecycleUnarchive, lcPerson, id))
				}
				return holdsNone(ctx, keys)
			}
			report, err := expiryPass(n, &proposals{c: c}, late).Pass(t.Context())
			if err != nil || report.RefsKept != 1 || report.RefsExpired != 0 {
				t.Fatalf("the pass = %+v, %v; want the branch kept", report, err)
			}
			if n.targetOf(id, seatBranch) == "" {
				t.Error("a branch a live turn held during the pass was expired")
			}
		})
	}
}

// AN EXPIRY THAT LANDS FIRST ANSWERS THE REF HOLD DELETED: a hold decided at a
// read before the expiry finds the branch gone and refuses ref_deleted, and
// writes nothing.
//
// Mutation: publish the hold whatever the ref's row holds → a record follows
// the deletion, red.
func TestAnExpiryThatLandsFirstAnswersTheRefHoldDeleted(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	id := n.made(lcPerson, "web", unitOwned("eng"))
	n.pushAt(idleSince, id, seatSets(seatBranch, oidOf("x")))
	_, read := n.readRef("web", seatBranch)
	report, err := expiryPass(n, &proposals{c: c}, holdsNone).Pass(t.Context())
	if err != nil || report.RefsExpired != 1 {
		t.Fatalf("the pass = %+v, %v; want the branch expired", report, err)
	}
	end := n.end()
	got := n.life(lcRefHold(agent("dev"), id, "turn-late", seatBranch, read))
	if got.Refusal != code.LifecycleRefDeleted || !strings.Contains(got.Detail, "ref lifecycle") {
		t.Fatalf("a hold after the expiry = %+v, want ref_deleted naming the ref lifecycle", got)
	}
	if n.end() != end {
		t.Error("a hold on an expired branch wrote a record")
	}
}
