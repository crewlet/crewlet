package code_test

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// AN EXPUNGE THROUGH THE WHOLE PATH: its records proposed to a committer,
// decided in one snapshot, published on a real embedded broker, applied by the
// shipped applier and answered from the rows they wrote.

// beginFor is an expunge's begin of repo on n at epoch, rewriting retire into a
// pack it keeps holding kept objects and a pack it leaves dropping holding
// dropping objects — either left out at zero — its id minted now and its
// outputs' keys at the harness's clock, planned at n's own position.
func beginFor(n *node, repo string, epoch int64, retire []string, kept, dropping int64) code.ExpungeBegin {
	xid := statelog.NewOpID(time.Now(), "")
	b := code.ExpungeBegin{RepoID: repo, ExpungeID: xid, Node: n.id, Seen: n.waiter.Committed(),
		DropEpoch: epoch, Grace: code.PruneGrace, Retire: slices.Clone(retire)}
	if kept > 0 {
		b.Add = append(b.Add, compactOutput(xid, 0, kept, harnessAt))
	}
	if dropping > 0 {
		b.Dropping = append(b.Dropping, compactOutput(xid, len(b.Add), dropping, harnessAt))
	}
	return b
}

// commitFor is the commit of b on n, retiring its dropping packs and keeping a
// rescue pack holding rescue objects — none at zero.
func commitFor(n *node, b code.ExpungeBegin, rescue int64) code.ExpungeCommit {
	c := code.ExpungeCommit{RepoID: b.RepoID, ExpungeID: b.ExpungeID, CommitID: statelog.NewOpID(time.Now(), ""),
		RequestID: b.RequestID, Node: n.id, Seen: n.waiter.Committed(), DropEpoch: b.DropEpoch}
	for _, out := range b.Dropping {
		c.Retire = append(c.Retire, out.PackID)
	}
	slices.Sort(c.Retire)
	if rescue > 0 {
		c.Add = append(c.Add, compactOutput(c.CommitID, 0, rescue, harnessAt))
	}
	return c
}

// purgeOf is a purge of repo, asked now by the harness's person.
func purgeOf(repo string) code.ExpungeRequest {
	return code.ExpungeRequest{RepoID: repo, RequestID: statelog.NewOpID(time.Now(), "code_purge_unreachable"),
		By: asker}
}

// requireExpunged fails t unless res is an expunge record applied at a
// position.
func requireExpunged(t *testing.T, res code.ExpungeResult, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("ProposeExpunge: %v", err)
	}
	if res.Outcome != statelog.OutcomeApplied || res.Position.IsZero() || res.Refusal != "" {
		t.Fatalf("the expunge record answered %+v, want applied at a position", res)
	}
}

// AN EXPUNGE IS DECIDED IN ITS REPOSITORY'S QUEUE, record by record: a
// person's request applied at its position; a second request applied at its
// own, the repository's open request widened to it; a begin applied and
// answered from the rows of the packs it added; a second begin while it is
// open refused stale; and its commit applied, serving the request — every
// pack the repository held when it was asked rewritten, by a scan past it. A
// request asked again under its own id is the same request, answered at its
// record by the ledger.
//
// Mutation: answer a second request with the open one, and it appends no
// record of its own and the repository holds the first; publish an expunge
// record under an operation id minted for its batch, and the request asked
// again is decided again.
func TestAnExpungeIsDecidedInItsRepositorysQueue(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, ids := packedRepo(t, n, "web", 3)
	c, _ := committerOn(t, n, n.pub)
	ctx := t.Context()

	asked := purgeOf(repo)
	first, err := c.ProposeExpunge(ctx, code.ExpungeProposal{Request: &asked})
	requireExpunged(t, first, err)
	if first.Request != asked.RequestID {
		t.Fatalf("the request was answered as %q, want its own %s", first.Request, asked.RequestID)
	}
	second := purgeOf(repo)
	widened, err := c.ProposeExpunge(ctx, code.ExpungeProposal{Request: &second})
	requireExpunged(t, widened, err)
	if widened.Request != second.RequestID || widened.Position.Packed() <= first.Position.Packed() {
		t.Fatalf("a second request answered %+v; want its own %s, at a record of its own past the first's %s",
			widened, second.RequestID, first.Position)
	}
	if got := n.count(`SELECT COUNT(*) FROM code_repos WHERE id = ? AND expunge_request = ?
		AND expunge_request_position = ?`, repo, second.RequestID, widened.Position.Packed()); got != 1 {
		t.Fatalf("the repository does not hold the second request open at its record, %s", widened.Position)
	}

	b := beginFor(n, repo, 1, ids, 2, 1)
	b.RequestID, b.Grace = second.RequestID, 0
	res, err := c.ProposeExpunge(ctx, code.ExpungeProposal{Begin: &b})
	requireExpunged(t, res, err)
	other := beginFor(n, repo, 2, []string{b.Add[0].PackID}, 2, 0)
	if res, err := c.ProposeExpunge(ctx, code.ExpungeProposal{Begin: &other}); err != nil ||
		res.Refusal != code.RefusalStale || !strings.Contains(res.Detail, "expunge "+b.ExpungeID+" is open") {
		t.Fatalf("a second begin answered %+v, %v; want stale naming the open expunge", res, err)
	}

	cm := commitFor(n, b, 1)
	res, err = c.ProposeExpunge(ctx, code.ExpungeProposal{Commit: &cm})
	requireExpunged(t, res, err)
	if got := n.count(`SELECT COUNT(*) FROM code_repos WHERE id = ? AND expunge_id = '' AND expunge_request = ''
		AND drop_epoch = 1`, repo); got != 1 {
		t.Fatalf("after the commit the repository holds an expunge or a request open, or another epoch")
	}
	if got := n.livePacks(repo); !slices.Equal(got, sorted(b.Add[0].PackID, cm.Add[0].PackID)) {
		t.Fatalf("the repository's packs are %v, want the kept pack and the rescue", got)
	}

	before := n.end()
	again, err := c.ProposeExpunge(ctx, code.ExpungeProposal{Request: &asked})
	requireExpunged(t, again, err)
	if again.Position != first.Position || again.Request != asked.RequestID || n.end() != before {
		t.Fatalf("the request asked again answered %+v, and %d records were appended; want its record at %s",
			again, n.end()-before, first.Position)
	}
}

// A PUSH CHECKED BEFORE A BEGIN IS REFUSED EXPUNGING: once an expunge's begin
// has moved its repository's drop epoch, a push its receiver checked at the
// epoch before — against a copy holding live what the begin left dropping — is
// refused WHOLE: every command the receiver did not refuse, a deletion among
// them, under the one sentence naming both epochs, and a command it refused
// keeping its own refusal; nothing of it decided and nothing appended. The
// same push checked at the begin's epoch lands. A push the repository's status
// refuses is told that first, whatever epoch it was checked at.
//
// Mutation: drop the decide's comparison of the epochs, and the push checked
// before the begin lands a ref the begin's commit could leave naming an object
// it retired.
func TestAPushCheckedBeforeABeginIsRefusedExpunging(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo := repoOn(t, n, "web")
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set("refs/heads/old", code.ZeroOID, oidOf("O"))))
	c, _ := committerOn(t, n, n.pub)
	ctx := t.Context()
	b := beginFor(n, repo, 1, n.livePacks(repo), 1, 1)
	res, err := c.ProposeExpunge(ctx, code.ExpungeProposal{Begin: &b})
	requireExpunged(t, res, err)

	tag := code.Command{Ref: "refs/tags/v1", Old: oidOf("T"), New: oidOf("U"), Result: code.ResultNG,
		Reason: code.RefusalTagMoved, Detail: "a tag does not move: delete it and push it again"}
	checked := proposal(repo, update("refs/heads/feature", code.ZeroOID, oidOf("F")),
		update("refs/heads/old", oidOf("O"), code.ZeroOID), tag)
	before := n.end()
	got, err := c.Propose(ctx, checked)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	detail := code.ExpungingDetail("web", 0, 1)
	if got.Outcome != "" || !got.Position.IsZero() || got.Answer.Refused != code.RefusalExpunging ||
		got.Answer.Detail != detail || !strings.Contains(detail, "(drop epoch 0, now 1)") {
		t.Fatalf("a push checked at drop epoch 0 after the begin answered %+v, want refused expunging, "+
			"undecided, saying %q", got, detail)
	}
	for _, cmd := range got.Answer.Commands {
		want := code.Command{Ref: cmd.Ref, Old: cmd.Old, New: cmd.New, Result: code.ResultNG,
			Reason: code.RefusalExpunging, Detail: detail}
		if cmd.Ref == tag.Ref {
			want = tag
		}
		if cmd != want {
			t.Errorf("command %s answered %+v, want %+v", cmd.Ref, cmd, want)
		}
	}
	if len(got.Answer.Commands) != 3 {
		t.Fatalf("the refusal answered %d commands, want the push's 3", len(got.Answer.Commands))
	}
	if end := n.end(); end != before {
		t.Fatalf("the refused push appended %d records, want none", end-before)
	}
	if n.target(repo, "refs/heads/feature") != "" || n.target(repo, "refs/heads/old") != oidOf("O") {
		t.Fatal("the refused push moved a ref")
	}

	checked.DropEpoch = 1
	got, err = c.Propose(ctx, checked)
	if err != nil || got.Outcome != statelog.OutcomeApplied || got.Answer.Refused != "" {
		t.Fatalf("the push checked at the begin's epoch answered %+v, %v; want applied", got, err)
	}
	if n.target(repo, "refs/heads/feature") != oidOf("F") || n.target(repo, "refs/heads/old") != "" {
		t.Fatal("the push checked at the begin's epoch did not move its refs")
	}

	s := suiteSettings()
	s.Archived = true
	n.ensure("web", s, t2)
	late := proposal(repo, update("refs/heads/late", code.ZeroOID, oidOf("L")))
	got, err = c.Propose(ctx, late)
	if err != nil || got.Answer.Refused != code.RefusalRepoArchived {
		t.Fatalf("a push to the archived repository checked at another epoch answered %+v, %v; want "+
			"repo_archived, its status first", got, err)
	}
}

// A SNAPSHOT SERVES WHEN EACH PACK'S CONTENT IS FROM ([code.PackRow.Content]),
// the date a purge's cycle chooses its inputs by and a request is judged
// served by: a push's pack its own position, and a compaction's output its
// oldest input's — never the position its row was added at, which the
// snapshot serves beside it and a rewrite moves past every request it judged
// nothing for.
//
// Mutation: serve a pack's added position as its content's, and the
// compaction's output reads as newer than what it merged.
func TestASnapshotServesWhenEachPacksContentIsFrom(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo := n.ensure("web", suiteSettings(), t1).RepoID
	var ids []string
	var at []statelog.Position
	for i := range 3 {
		e := entry(uuid.Must(uuid.NewV7()).String(),
			set(fmt.Sprintf("refs/heads/b%d", i), code.ZeroOID, oidOf(fmt.Sprint("b", i))))
		ids = append(ids, e.Pack.PackID)
		at = append(at, n.push(repo, e).Position)
	}
	c, _ := committerOn(t, n, n.pub)
	merge := compactionOf(n, repo, sorted(ids[0], ids[1]), 2, harnessAt)
	res, err := c.ProposeCompaction(t.Context(), merge)
	requireApplied(t, res, err)

	want := map[string][2]statelog.Position{
		merge.Compaction.Add[0].PackID: {res.Position, at[0]},
		ids[2]:                         {at[2], at[2]},
	}
	served := 0
	for _, page := range snapshot(t, n.reader, repo, statelog.Position{}) {
		for _, p := range page.Packs {
			w, ok := want[p.PackID]
			if !ok {
				t.Fatalf("the snapshot serves pack %s, which the repository no longer holds", p.PackID)
			}
			if p.Added != w[0] || p.Content != w[1] {
				t.Errorf("pack %s is served added at %s with its content from %s, want %s and %s", p.PackID,
					p.Added, p.Content, w[0], w[1])
			}
			served++
		}
	}
	if served != len(want) {
		t.Fatalf("the snapshot served %d packs, want %d", served, len(want))
	}
}

// AN EXPUNGE IS ANSWERED ONLY FROM THE ROWS OF THE PACKS IT ADDED: a publish
// that answers applied for a begin that wrote no row is answered unknown,
// never applied — which would hand its builder packs no row names — and logged
// at ERROR, since no committer of this domain forms such a record.
//
// Mutation: answer a begin applied from the publish's word, and it is applied
// with none of its packs in the rows.
func TestAnExpungeIsAnsweredOnlyFromTheRowsOfThePacksItAdded(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, ids := packedRepo(t, n, "web", 2)
	logs := &lineLog{}
	c, _ := committerOn(t, n, decideOnly{n}, func(o *code.CommitterOptions) {
		o.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	})
	b := beginFor(n, repo, 1, ids, 1, 1)
	res, err := c.ProposeExpunge(t.Context(), code.ExpungeProposal{Begin: &b})
	if err != nil || res.Outcome != statelog.OutcomeUnknown || !strings.Contains(res.Detail, "not in the rows") {
		t.Fatalf("a begin whose record wrote no row answered %+v, %v; want unknown", res, err)
	}
	if lines := logs.lines("code_expunge_unanswerable"); len(lines) != 1 || lines[0]["level"] != "ERROR" {
		t.Fatalf("logged %v, want code_expunge_unanswerable once at ERROR", lines)
	}
}

// THE EXPUNGE LISTING NAMES THE EXPUNGES OWED: the active and archived
// repositories with an expunge open or a purge requested, by id, each with
// the open expunge, the request and its position, its live packs and bytes
// and the version its row was read at — and no repository with neither, nor
// a deleted one, whose request waits for its restore — paged by id.
//
// Mutation: drop the listing's status rule, and the deleted repository's
// request is listed; page by name, and the cursor skips a repository.
func TestTheExpungeListingNamesTheExpungesOwed(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	c, _ := committerOn(t, n, n.pub)
	ctx := t.Context()
	requested, _ := packedRepo(t, n, "requested", 2)
	ask := purgeOf(requested)
	asked, err := c.ProposeExpunge(ctx, code.ExpungeProposal{Request: &ask})
	requireExpunged(t, asked, err)
	expunging, ids := packedRepo(t, n, "expunging", 2)
	b := beginFor(n, expunging, 1, ids, 1, 1)
	res, err := c.ProposeExpunge(ctx, code.ExpungeProposal{Begin: &b})
	requireExpunged(t, res, err)
	packedRepo(t, n, "idle", 2)
	gone, _ := packedRepo(t, n, "gone", 1)
	goneAsk := purgeOf(gone)
	res, err = c.ProposeExpunge(ctx, code.ExpungeProposal{Request: &goneAsk})
	requireExpunged(t, res, err)
	if _, err := n.store.DeleteRepo(ctx, n.activate(t2), gone); err != nil {
		t.Fatalf("delete %s: %v", gone, err)
	}
	n.drain()

	want := map[string]code.ExpungeCandidate{
		requested: {ID: requested, Name: "requested", Request: ask.RequestID, Requested: asked.Position,
			LivePacks: 2},
		expunging: {ID: expunging, Name: "expunging", Open: b.ExpungeID, LivePacks: 1},
	}
	order := slices.Sorted(maps.Keys(want))
	code.SetPageLimits(n.reader, 1, 1<<20)
	var listed []string
	cursor := ""
	for page := 0; ; page++ {
		got, err := n.reader.Read(ctx, code.ReadQuery{Kind: code.ReadExpungeDue, Cursor: cursor,
			Fresh: statelog.Freshness{Level: statelog.ReadStale}})
		if err != nil {
			t.Fatalf("list page %d: %v", page, err)
		}
		for _, cand := range got.Expunge {
			w, ok := want[cand.ID]
			if !ok {
				t.Fatalf("the listing names %s (%s), which owes no expunge", cand.ID, cand.Name)
			}
			if cand.Name != w.Name || cand.Open != w.Open || cand.Request != w.Request ||
				cand.Requested != w.Requested || cand.LivePacks != w.LivePacks || cand.LiveBytes == 0 ||
				cand.Version.IsZero() {
				t.Fatalf("the listing names %+v, want %+v with its live bytes and version", cand, w)
			}
			listed = append(listed, cand.ID)
		}
		if got.Next == "" {
			break
		}
		cursor = got.Next
	}
	if !slices.Equal(listed, order) {
		t.Fatalf("the listing named %v, want %v by id", listed, order)
	}
}

// A REPOSITORY MID-EXPUNGE IS LISTED FOR NO PUBLICATION: rule 6g refuses every
// publication between an expunge's begin and its commit, so the publication
// due listing leaves out a repository with an expunge open — however many of
// its packs no layer covers, the kept pack its begin wrote among them — and
// names it again once the commit lands. A publisher polling it builds no
// layer the committer would refuse stale at every poll until the commit.
//
// Mutation: drop the listing's open-expunge predicate, and the repository is
// listed between its begin and its commit.
func TestARepositoryMidExpungeIsListedForNoPublication(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, ids := packedRepo(t, n, "web", 3)
	c, _ := committerOn(t, n, n.pub)
	listed := func() bool {
		t.Helper()
		page, err := n.reader.Read(t.Context(), code.ReadQuery{Kind: code.ReadPublicationDue, Fresh: stale})
		if err != nil {
			t.Fatalf("list the publications due: %v", err)
		}
		return slices.ContainsFunc(page.Publish, func(p code.PublicationCandidate) bool { return p.ID == repo })
	}
	if !listed() {
		t.Fatal("a repository of three packs no layer covers is not listed")
	}
	b := beginFor(n, repo, 1, ids, 2, 1)
	res, err := c.ProposeExpunge(t.Context(), code.ExpungeProposal{Begin: &b})
	requireExpunged(t, res, err)
	if listed() {
		t.Fatalf("the repository is listed with expunge %s open, whose publication rule 6g refuses", b.ExpungeID)
	}
	cm := commitFor(n, b, 0)
	res, err = c.ProposeExpunge(t.Context(), code.ExpungeProposal{Commit: &cm})
	requireExpunged(t, res, err)
	if !listed() {
		t.Fatal("once its expunge committed, the repository whose kept pack no layer covers is not listed")
	}
}

// seedDropsLeaving writes one ref drop of repoID for each of olds straight
// into a node's estate, all left by one record — at the packed position on the
// code log and its broker instant at — the n-th of them dropping
// refs/heads/dropped-<n>.
func (n *node) seedDropsLeaving(repoID string, position int64, at time.Time, olds ...string) {
	n.t.Helper()
	ctx := n.t.Context()
	if err := n.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := store.InsertRows(ctx, tx, n.db.Caps().MaxVariables,
			`INSERT INTO code_ref_drops (repo_id, position, ref, old, at) VALUES `, `(?, ?, ?, ?, ?)`, ``,
			len(olds), func(i int) []any {
				return []any{repoID, position, fmt.Sprintf("refs/heads/dropped-%06d", i), olds[i],
					store.EncodeTime(at)}
			})
		return err
	}); err != nil {
		n.t.Fatalf("seed %s's ref drops: %v", repoID, err)
	}
}

// AN EXPUNGE'S TIPS ARE WHAT ITS DROPS LEFT SINCE ITS INSTANT: the
// `expunge_tips` read answers the object of every ref drop of the repository
// whose record's broker instant is at or after the one it names — a drop at the
// instant itself among them, none before it and none of another repository —
// in the order of the drops' key, each object once a page, paged by a keyset
// that resumes after the last drop the page read, so every page's drops are
// read once; a cursor from before the instant is refused, and a repository
// with no row is not found.
//
// Mutation: seek `at > ?` and the drops left at the instant go unread; resume
// a page after the last drop's position alone, and a page that ends inside a
// record's drops skips the rest of them.
func TestAnExpungesTipsAreWhatItsDropsLeftSinceItsInstant(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo := n.ensure("web", suiteSettings(), t1).RepoID
	other := n.ensure("app", suiteSettings(), t1).RepoID
	since := harnessAt
	n.seedDropsLeaving(repo, 1, since.Add(-time.Second), oidOf("before"))
	n.seedDropsLeaving(repo, 2, since, oidOf("x"), oidOf("y"), oidOf("x"))
	n.seedDropsLeaving(repo, 3, since.Add(time.Minute), oidOf("z"), oidOf("y"))
	n.seedDropsLeaving(other, 4, since, oidOf("theirs"))
	read := func(id, cursor string) (code.ReadAnswer, error) {
		return n.reader.Read(t.Context(), code.ReadQuery{Kind: code.ReadExpungeTips,
			ExpungeTips: &code.ExpungeTipsQuery{RepoID: id, Since: since}, Cursor: cursor,
			Fresh: statelog.Freshness{Level: statelog.ReadStale}})
	}
	one, err := read(repo, "")
	if err != nil {
		t.Fatalf("the tips: %v", err)
	}
	if want := []string{oidOf("x"), oidOf("y"), oidOf("z")}; !slices.Equal(one.Tips, want) || one.Next != "" {
		t.Fatalf("one page of the tips is %v (next %q), want %v whole: every drop since the instant, each "+
			"object once", one.Tips, one.Next, want)
	}

	code.SetPageLimits(n.reader, 2, 1<<20)
	var tips []string
	cursor := ""
	for page := 0; ; page++ {
		got, err := read(repo, cursor)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		tips = append(tips, got.Tips...)
		if got.Next == "" {
			break
		}
		cursor = got.Next
	}
	// Pages of two drops each: (2,x)(2,y), (2,x)(3,z), (3,y) — an object
	// once a page, and every drop read once.
	if want := []string{oidOf("x"), oidOf("y"), oidOf("x"), oidOf("z"), oidOf("y")}; !slices.Equal(tips, want) {
		t.Fatalf("the tips in pages of two are %v, want %v", tips, want)
	}

	early := fmt.Sprintf("drops:%d:2:%s:refs/heads/dropped-000000", store.EncodeTime(since.Add(-time.Hour)),
		oidOf("x"))
	if _, err := read(repo, early); !errors.Is(err, code.ErrBadCursor) {
		t.Errorf("a cursor from before the instant: %v, want ErrBadCursor", err)
	}
	if _, err := read(uuid.NewString(), ""); !errors.Is(err, code.ErrRepoNotFound) {
		t.Errorf("the tips of a repository with no row: %v, want ErrRepoNotFound", err)
	}
}

// AN EXPUNGE IS CHECKED UNSTORED AS IT WILL BE STORED: ValidateUnstored
// answers what Validate answers once every output's put has filled in its key
// and digest — a begin and a commit both take, a commit whose rescue's sizes
// lay out no bundle both refuse — so the maintainer that built one holds it to
// every rule its committer will before a byte of it is uploaded
// (internal/code/compact). An output already stored is refused, and so is a
// request, which stores nothing: Validate is the check of those.
//
// Mutation: hold the outputs to Validate as they stand — no key, no digest —
// and the begin both take is refused unstored.
func TestAnUnstoredExpungeIsCheckedAsItWillBeStored(t *testing.T) {
	t.Parallel()
	seen := statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: 9}
	xid := statelog.NewOpID(time.Now(), "")
	begin := code.ExpungeBegin{RepoID: uuid.NewString(), ExpungeID: xid, Node: "node-a", Seen: seen, DropEpoch: 1,
		Grace: code.PruneGrace, Retire: sorted(uuid.NewString(), uuid.NewString()),
		Add:      []code.PackRef{compactOutput(xid, 0, 3, harnessAt)},
		Dropping: []code.PackRef{compactOutput(xid, 1, 2, harnessAt)}}
	cid := statelog.NewOpID(time.Now(), "")
	commit := code.ExpungeCommit{RepoID: begin.RepoID, ExpungeID: xid, CommitID: cid, Node: "node-a", Seen: seen,
		DropEpoch: 1, Retire: []string{begin.Dropping[0].PackID},
		Add: []code.PackRef{compactOutput(cid, 0, 1, harnessAt)}}
	unstored := func(outs []code.PackRef) []code.PackRef {
		out := slices.Clone(outs)
		for i := range out {
			out[i].Object, out[i].Hash = objstore.Key{}, ""
		}
		return out
	}
	for name, stored := range map[string]code.ExpungeProposal{
		"a begin": {Begin: &begin}, "a commit": {Commit: &commit},
	} {
		if err := stored.Validate(); err != nil {
			t.Fatalf("%s the case builds is refused stored: %v", name, err)
		}
		if err := stored.ValidateUnstored(); !errors.Is(err, code.ErrInvalidProposal) {
			t.Errorf("ValidateUnstored of %s already stored: %v, want ErrInvalidProposal", name, err)
		}
	}
	b := begin
	b.Add, b.Dropping = unstored(begin.Add), unstored(begin.Dropping)
	if err := (code.ExpungeProposal{Begin: &b}).ValidateUnstored(); err != nil {
		t.Errorf("an unstored begin whose stored form validates: %v", err)
	}
	c := commit
	c.Add = unstored(commit.Add)
	if err := (code.ExpungeProposal{Commit: &c}).ValidateUnstored(); err != nil {
		t.Errorf("an unstored commit whose stored form validates: %v", err)
	}
	c.Add[0].IndexBytes++
	if err := (code.ExpungeProposal{Commit: &c}).ValidateUnstored(); !errors.Is(err, code.ErrInvalidProposal) {
		t.Errorf("an unstored rescue whose sizes lay out no bundle: %v, want ErrInvalidProposal", err)
	}
	asked := purgeOf(begin.RepoID)
	if err := (code.ExpungeProposal{Request: &asked}).ValidateUnstored(); !errors.Is(err, code.ErrInvalidProposal) {
		t.Errorf("ValidateUnstored of a request: %v, want ErrInvalidProposal", err)
	}
}
