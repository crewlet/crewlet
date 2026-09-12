package pages_test

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The applier's own guards, over a real replicated estate.
//
// [statelogtest] certifies what the FRAMEWORK relies on — determinism,
// idempotency, the deferral contract, the table classes. What it cannot know
// is what this domain's records mean, and every case here is one of those: the
// address rule, the two gates, the divergent skill row, and the prune that
// rides the commit.

type harness struct {
	t       *testing.T
	db      *store.DB
	applier *pages.Applier
	seq     uint64
}

func newHarness(t *testing.T, skills pages.SkillDetector) *harness {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node.db"),
		store.Options{PinnedWriters: 1})
	if err != nil {
		t.Fatalf("open a store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	return &harness{t: t, db: db, applier: pages.NewApplier("node-a", skills, nil)}
}

var brokerAt = time.Date(2031, 4, 2, 3, 14, 0, 0, time.UTC)

// apply runs one record at the next position, returning the gate that dropped
// it when one did.
func (h *harness) apply(rec pages.MutationRecord) (rows int, gate statelog.Reason, err error) {
	h.t.Helper()
	h.seq++
	return h.applyAt(rec, h.seq)
}

func (h *harness) applyAt(rec pages.MutationRecord, seq uint64) (
	rows int, gate statelog.Reason, err error) {

	h.t.Helper()
	body, encodeErr := pages.Encode(rec)
	if encodeErr != nil {
		h.t.Fatalf("encode the record: %v", encodeErr)
	}
	record := statelog.Record{
		Envelope: statelog.Envelope{
			V: rec.V, Kind: string(rec.Subject.Kind),
			Subject: statelog.Subject{
				Kind: string(rec.Subject.Kind), ID: rec.Subject.ID,
			},
			Op: string(rec.Op), OpID: rec.OpID, Gen: rec.Gen,
			Writer: rec.Writer, Scope: rec.Scope.Resolve(rec.Subject),
		},
		Position: statelog.Position{
			Stream: "CREWLET_PAGES_LOG", Generation: rec.Gen, Seq: seq,
		},
		Payload:  body,
		StoredAt: brokerAt,
	}
	err = h.db.Replicated().Tx(h.t.Context(), func(tx *sql.Tx) error {
		reason, gated, gErr := h.applier.Gated(h.t.Context(), tx, record)
		if gErr != nil {
			return gErr
		}
		if gated {
			gate = reason
			return nil
		}
		n, aErr := h.applier.Apply(h.t.Context(), tx, record,
			statelog.ApplyOptions{Now: brokerAt, StoredAt: brokerAt})
		rows = n
		return aErr
	})
	return rows, gate, err
}

func (h *harness) count(table string) int {
	h.t.Helper()
	var n int
	if err := h.db.Replicated().Read(h.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(h.t.Context(),
			`SELECT COUNT(*) FROM `+table).Scan(&n)
	}); err != nil {
		h.t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func (h *harness) scalar(query string, args ...any) string {
	h.t.Helper()
	var out sql.NullString
	if err := h.db.Replicated().Read(h.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(h.t.Context(), query, args...).Scan(&out)
	}); err != nil {
		return ""
	}
	return out.String
}

// create builds a create record for one page.
func create(pageID, container, title, body string) pages.MutationRecord {
	return record(pages.TitleSubject(container, title), pages.OpCreate,
		"op-create-"+pageID, pages.CreatePayload{
			V: pages.DocumentVersion, PageID: pageID, Container: container,
			Title: title, Body: body, Status: pages.StatusPublished,
			Author: "ada",
		}, pages.ScopeSet{Subject: true, Container: container})
}

// record builds one record with its payload encoded.
func record(subject pages.Subject, op pages.OpKind, opID string, payload any,
	scope pages.ScopeSet) pages.MutationRecord {

	rec := pages.MutationRecord{
		RecordEnvelope: pages.RecordEnvelope{
			V: pages.RecordVersion, OpID: opID, Subject: subject, Op: op,
			CreatedAt: brokerAt, Gen: 1, Writer: "node-a", Scope: scope,
		},
		Actor: "ada", ActorKind: pages.AuthorHuman,
	}
	if payload != nil {
		body, err := json.Marshal(payload)
		if err != nil {
			panic(err)
		}
		rec.Mutation = body
	}
	return rec
}

// TestACreateWritesTheClaimTheHeadTheRevisionAndTheHistoryTogether.
//
// THE WHOLE REASON THIS DOMAIN EXISTS. On the bucket a create was a three-key
// sequence — title claim, page, change — with an orphan claim as its crash
// state and a grace rule for stepping over the debris. Here it is one record
// and one transaction, so there is no window in which a title is held by a
// page that was never written.
func TestACreateWritesTheClaimTheHeadTheRevisionAndTheHistoryTogether(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	if _, gate, err := h.apply(create("page-1", "ENG", "Deploy Runbook",
		"# Deploy\n")); err != nil || gate != "" {
		t.Fatalf("apply a create: %v (gate %q)", err, gate)
	}
	for table, want := range map[string]int{
		"pages_titles": 1, "pages_heads": 1, "pages_revisions": 1,
		"pages_history": 1, "pages_skills": 1,
	} {
		if got := h.count(table); got != want {
			t.Errorf("%s holds %d rows, want %d — a create is ONE transaction "+
				"and a half-written one is the crash state this domain removes",
				table, got, want)
		}
	}
	// THE CLAIM CARRIES THE NORMALISED TITLE BESIDE ITS TOKEN, because a
	// digest has no inverse and an operator asking "why can this name not
	// be used" needs the name.
	if got := h.scalar(
		`SELECT title_norm FROM pages_titles WHERE container = ?`, "ENG"); got == "" {
		t.Error("the claim holds no readable title, so nothing can say what " +
			"address is taken")
	}
}

// TestARecordThatArbitratedOneAddressAndClaimsAnotherIsRefused.
//
// THE SUBJECT IS THE ADDRESS, and this is what makes that true rather than
// conventional. Without it a writer takes one title at the broker — where
// exactly one writer can — and writes a different one into every node's row,
// leaving the arbitrated name held by nothing and the written name held by two.
func TestARecordThatArbitratedOneAddressAndClaimsAnotherIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	rec := create("page-1", "ENG", "Deploy Runbook", "# Deploy\n")
	// The subject stays; the payload's title moves.
	var payload pages.CreatePayload
	if err := json.Unmarshal(rec.Mutation, &payload); err != nil {
		t.Fatal(err)
	}
	payload.Title = "Something Else Entirely"
	body, _ := json.Marshal(payload)
	rec.Mutation = body

	if _, _, err := h.apply(rec); err == nil {
		t.Fatal("a record took one address at the broker and wrote another — " +
			"the arbitrated name is now held by nothing")
	}
	if got := h.count("pages_heads"); got != 0 {
		t.Errorf("%d head rows were written anyway", got)
	}

	// AND THE CONTAINER IS HALF OF THE ADDRESS: one title in two spaces is
	// two addresses, so a payload that moved only the container is the
	// same failure.
	rec = create("page-2", "ENG", "Deploy Runbook", "# Deploy\n")
	if err := json.Unmarshal(rec.Mutation, &payload); err != nil {
		t.Fatal(err)
	}
	payload.Container = "PROD"
	body, _ = json.Marshal(payload)
	rec.Mutation = body
	if _, _, err := h.apply(rec); err == nil {
		t.Fatal("a record arbitrated ENG's address and claimed PROD's")
	}
}

// TestAPurgedPageStaysPurgedHoweverLateARecordArrives.
//
// A redelivery months later must not resurrect a page an operator deliberately
// destroyed — and the record that WROTE the marker must not be gated by it,
// or a purge whose acknowledgement was lost resolves as "applied nowhere" and
// its caller is told the destruction did not happen when it did.
func TestAPurgedPageStaysPurgedHoweverLateARecordArrives(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	if _, _, err := h.apply(create("page-1", "ENG", "Deploy Runbook", "# a\n")); err != nil {
		t.Fatalf("create: %v", err)
	}
	purge := record(pages.PageSubject("page-1"), pages.OpPurge, "op-purge",
		pages.StatusPayload{V: pages.DocumentVersion, Reason: "wrong space"},
		pages.ScopeSet{Subject: true, Container: "ENG"})
	if _, gate, err := h.apply(purge); err != nil || gate != "" {
		t.Fatalf("purge: %v (gate %q)", err, gate)
	}
	if got := h.count("pages_heads"); got != 0 {
		t.Fatalf("%d heads survive a purge", got)
	}
	if got := h.count("pages_deletions"); got != 1 {
		t.Fatalf("a purge wrote %d deletion markers", got)
	}

	// A LATER RECORD ABOUT THE SAME PAGE IS DROPPED.
	save := record(pages.PageSubject("page-1"), pages.OpPatch, "op-save",
		pages.PagePatch{V: pages.DocumentVersion, Labels: []string{"x"}},
		pages.ScopeSet{Subject: true, Container: "ENG"})
	_, gate, err := h.apply(save)
	if err != nil {
		t.Fatalf("apply a record about a purged page: %v", err)
	}
	if gate != statelog.ReasonDeleted {
		t.Fatalf("a record about a purged page was applied (gate %q) — a "+
			"redelivery months later would resurrect it", gate)
	}

	// AND A REDELIVERY OF THE PURGE ITSELF IS NOT GATED BY ITS OWN MARKER.
	if _, gate, err := h.applyAt(purge, h.seq+1); err != nil {
		t.Fatalf("redeliver the purge: %v", err)
	} else if gate != "" {
		t.Fatalf("the purge was gated by the marker it wrote (%q), so a lost "+
			"acknowledgement tells its caller the destruction never happened",
			gate)
	}
}

// TestAnEvictedNodesRecordsApplyNowhere, and a readmission takes it back.
//
// The fence depends on nothing but the log's own order, which is what makes it
// hold when coordination cannot be reached at all — and a wedged coordination
// path is a precondition of an eviction being permitted.
func TestAnEvictedNodesRecordsApplyNowhere(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	evict := record(pages.EvictionSubject("node-b"), pages.OpEviction, "op-evict",
		pages.Eviction{
			V: pages.GateRecordVersion, NodeID: "node-b", EvictedBy: "ops",
			EvictedAt: brokerAt,
		}, pages.ScopeSet{Subject: true})
	if _, _, err := h.apply(evict); err != nil {
		t.Fatalf("evict: %v", err)
	}

	from := create("page-1", "ENG", "Deploy Runbook", "# a\n")
	from.Writer = "node-b"
	_, gate, err := h.apply(from)
	if err != nil {
		t.Fatalf("apply an evicted node's record: %v", err)
	}
	if gate != statelog.ReasonEvicted {
		t.Fatalf("an evicted node's record applied (gate %q)", gate)
	}

	// A READMISSION IS AN INVERSE COMMIT rather than a delete, so the
	// eviction's whole history survives a replay.
	readmit := record(pages.EvictionSubject("node-b"), pages.OpEviction,
		"op-readmit", pages.Eviction{
			V: pages.GateRecordVersion, NodeID: "node-b", EvictedBy: "ops",
			EvictedAt: brokerAt, Readmitted: true,
		}, pages.ScopeSet{Subject: true})
	if _, _, err := h.apply(readmit); err != nil {
		t.Fatalf("readmit: %v", err)
	}
	if got := h.count("pages_evictions"); got != 1 {
		t.Errorf("a readmission left %d eviction rows — the history is the "+
			"point, and a delete would lose it", got)
	}
	after := create("page-2", "ENG", "Second Page", "# b\n")
	after.Writer = "node-b"
	if _, gate, err := h.apply(after); err != nil || gate != "" {
		t.Fatalf("a readmitted node's record was still dropped: %v (gate %q)",
			err, gate)
	}
}

// TestTheSkillFlagIsThisBuildsAnswerAndNobodyElses.
//
// It is DIVERGENT by construction: two nodes on different builds legitimately
// disagree, so it lives in its own table outside the identity claim. What must
// hold is that it is recomputed on every apply rather than carried on the
// record — which is what lets a parser fix reach every existing page on the
// next rebuild rather than only the pages edited since.
func TestTheSkillFlagIsThisBuildsAnswerAndNobodyElses(t *testing.T) {
	t.Parallel()
	body := "<!--skill-->\n# How to deploy\n"

	blind := newHarness(t, nil)
	if _, _, err := blind.apply(create("page-1", "ENG", "Deploy", body)); err != nil {
		t.Fatalf("apply on a build with no parser: %v", err)
	}
	if got := blind.scalar(
		`SELECT skill FROM pages_skills WHERE page_id = ?`, "page-1"); got != "0" {
		t.Errorf("a build with no skill parser recorded skill=%q", got)
	}

	seeing := newHarness(t, skillWhenBody("<!--skill-->"))
	if _, _, err := seeing.apply(create("page-1", "ENG", "Deploy", body)); err != nil {
		t.Fatalf("apply on a build with a parser: %v", err)
	}
	if got := seeing.scalar(
		`SELECT skill FROM pages_skills WHERE page_id = ?`, "page-1"); got != "1" {
		t.Errorf("a build whose parser sees the marker recorded skill=%q", got)
	}
	// THE HEAD ROWS ARE THE SAME on both, which is what the Divergent
	// class buys: the disagreement is confined to the one table the
	// identity claim excludes.
	if a, b := blind.scalar(`SELECT body FROM pages_heads WHERE id = ?`, "page-1"),
		seeing.scalar(`SELECT body FROM pages_heads WHERE id = ?`, "page-1"); a != b {
		t.Errorf("two builds wrote different page rows (%q vs %q) — the "+
			"divergence must not reach a table inside the identity claim", a, b)
	}
}

// TestThePruneRidesTheCommitRatherThanBeingRecomputed.
//
// Every node must delete exactly the same rows at exactly the same position. A
// "keep the last hundred" rule evaluated per node deletes on that node's own
// authority, and two nodes that saw a different set delete different rows —
// permanently, inside the identity claim.
func TestThePruneRidesTheCommitRatherThanBeingRecomputed(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	if _, _, err := h.apply(create("page-1", "ENG", "Deploy", "# v1\n")); err != nil {
		t.Fatalf("create: %v", err)
	}
	second := "# v2\n"
	save := record(pages.PageSubject("page-1"), pages.OpPatch, "op-save-1",
		pages.PagePatch{V: pages.DocumentVersion, Body: &second},
		pages.ScopeSet{Subject: true, Container: "ENG"})
	if _, _, err := h.apply(save); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got := h.count("pages_revisions"); got != 2 {
		t.Fatalf("%d revisions after a create and one save, want 2", got)
	}

	third := "# v3\n"
	pruning := record(pages.PageSubject("page-1"), pages.OpPatch, "op-save-2",
		pages.PagePatch{
			V: pages.DocumentVersion, Body: &third, RetiredRevisions: []int{1},
		}, pages.ScopeSet{Subject: true, Container: "ENG"})
	if _, _, err := h.apply(pruning); err != nil {
		t.Fatalf("save with a prune: %v", err)
	}
	if got := h.count("pages_revisions"); got != 2 {
		t.Fatalf("%d revisions after a save that retired one, want 2", got)
	}
	if got := h.scalar(
		`SELECT body FROM pages_revisions WHERE page_id = ? AND edit_version = ?`,
		"page-1", 1); got != "" {
		t.Errorf("revision 1 survives a record that retired it: %q", got)
	}
}

// TestAPatchForAPageThisNodeHasNoRowForStopsTheLoop.
//
// Under a STRICT replay the create is BELOW this position on the same ordered
// log, so a node that applied that position and has no row applied it wrong.
// Carrying on would serve a page that silently lost every edit before the gap.
func TestAPatchForAPageThisNodeHasNoRowForStopsTheLoop(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	body := "# orphan\n"
	save := record(pages.PageSubject("page-missing"), pages.OpPatch, "op-save",
		pages.PagePatch{V: pages.DocumentVersion, Body: &body},
		pages.ScopeSet{Subject: true, Container: "ENG"})
	if _, _, err := h.apply(save); err == nil {
		t.Fatal("a patch for a page with no row was applied — under a strict " +
			"replay that is a record this build applied incorrectly, not one " +
			"that has not arrived")
	}
}

// skillWhenBody is a parser that sees a marker.
type skillWhenBody string

func (m skillWhenBody) IsSkill(body string) bool {
	return len(body) > 0 && len(string(m)) > 0 &&
		containsSubstring(body, string(m))
}

func containsSubstring(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
