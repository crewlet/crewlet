package main

import (
	"strings"
	"testing"
)

// `crewlet config scrub` — the one-time erasure of personal data from
// superseded revisions.
//
// The property under test is narrow and permanent: the addresses leave the
// superseded rows, the ACTIVE row is untouched, and a diff across the
// boundary shows the tombstone rather than looking like damage.

// THE COMPANY THAT NAMES SOMEBODY, which is every company with a human seat.
const scrubCompanyDoc = `
name: Nimbus
providers:
  llm:
    main:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${ANTHROPIC_API_KEY}"]
roles:
  - name: CEO
    handle: ceo
    llm: main
  - name: Sarah
    handle: sarah
    kind: human
    email: sarah@example.com
    contact:
      slack_user_id: U0FOUNDER
`

// THE SCRUB ERASES A SUPERSEDED REVISION AND REFUSES THE ACTIVE ONE.
//
// # What each half buys
//
// Erasing the superseded rows is the point: a human seat's address is
// otherwise archived in an append-only table on every node and in every
// backup, and removing the seat never reached it, because the removal writes
// a NEW revision and every older one still holds them.
//
// Refusing the active one is what keeps the erasure from becoming a silent
// configuration change. The fleet is serving that document and every node is
// holding it; rewriting it underneath them would be a change nothing
// activated — no epoch, no apply, no event.
func TestTheScrubErasesSupersededRevisionsAndRefusesTheActiveOne(t *testing.T) {
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)

	// TWO REVISIONS: the first names Sarah, the second is the same company
	// with her mission changed, so the first is superseded and still holds
	// her address.
	first := companyFile(t, dir, "first.yaml", func(string) string { return scrubCompanyDoc })
	if _, errs, err := configCmd(t, cfg, "import", first); err != nil {
		t.Fatalf("import the first revision: %v (%s)", err, errs)
	}
	second := companyFile(t, dir, "second.yaml", func(string) string {
		return scrubCompanyDoc + "mission: ship it\n"
	})
	if _, errs, err := configCmd(t, cfg, "import", second); err != nil {
		t.Fatalf("import the second revision: %v (%s)", err, errs)
	}

	// THE DRY RUN FIRST, because an operator making an irreversible change
	// to every superseded revision wants to know how many hold anything.
	out, errs, err := configCmd(t, cfg, "scrub", "-dry-run")
	if err != nil {
		t.Fatalf("dry run: %v (%s)", err, errs)
	}
	if !strings.Contains(out, "re-run without -dry-run") {
		t.Errorf("the dry run does not say how to proceed:\n%s", out)
	}
	// AND IT WROTE NOTHING: the address is still in the superseded
	// revision, which is what makes the next assertion mean something.
	if !holdsTheAddress(t, cfg) {
		t.Fatal("the dry run erased the address, so it is not a dry run")
	}

	out, errs, err = configCmd(t, cfg, "scrub")
	if err != nil {
		t.Fatalf("scrub: %v (%s)", err, errs)
	}
	if !strings.Contains(out, "scrubbed 1 revision(s)") {
		t.Errorf("the scrub did not report what it did:\n%s", out)
	}
	// THE WARNING IS NOT OPTIONAL. The two other places the data sits are
	// not reachable from here, and an operator who thinks this finished
	// the job has not finished it.
	for _, want := range []string{"every node", "backups"} {
		if !strings.Contains(out, want) {
			t.Errorf("the scrub does not say it only reached this node's "+
				"revisions (missing %q):\n%s", want, out)
		}
	}
	if holdsTheAddress(t, cfg) {
		t.Error("the address survived in a superseded revision, which is " +
			"where it was archived in the first place")
	}

	// THE ACTIVE REVISION IS UNTOUCHED, and it still names her — because
	// she is still in the company, and a scrub is not a way to remove
	// somebody from a chart.
	active, _, err := configCmd(t, cfg, "show")
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if !strings.Contains(active, "sarah@example.com") {
		t.Errorf("the ACTIVE revision lost the address, so the running "+
			"company changed with no epoch and no apply:\n%s", active)
	}

	// AND NAMING THE ACTIVE REVISION IS REFUSED, with the procedure.
	id := activeRevisionID(t, cfg)
	_, _, err = configCmd(t, cfg, "scrub", id)
	if err == nil {
		t.Fatal("scrubbing the active revision succeeded")
	}
	if !strings.Contains(err.Error(), "Edit the company") {
		t.Errorf("the refusal does not say what to do instead: %v", err)
	}
}

// A SECOND RUN IS A NO-OP AND SAYS SO.
//
// An operator works through a list and re-runs. A pass that counted the
// tombstones it had already written would report an erasure that did not
// happen — and rewrite a row that needed no write, stamping a scrub time over
// the one that recorded the real erasure.
func TestASecondScrubFindsNothing(t *testing.T) {
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)

	first := companyFile(t, dir, "first.yaml", func(string) string { return scrubCompanyDoc })
	if _, _, err := configCmd(t, cfg, "import", first); err != nil {
		t.Fatalf("import: %v", err)
	}
	second := companyFile(t, dir, "second.yaml", func(string) string {
		return scrubCompanyDoc + "mission: ship it\n"
	})
	if _, _, err := configCmd(t, cfg, "import", second); err != nil {
		t.Fatalf("import: %v", err)
	}
	if _, _, err := configCmd(t, cfg, "scrub"); err != nil {
		t.Fatalf("first scrub: %v", err)
	}

	out, _, err := configCmd(t, cfg, "scrub")
	if err != nil {
		t.Fatalf("second scrub: %v", err)
	}
	if !strings.Contains(out, "nothing to scrub") {
		t.Errorf("a second pass did not report a no-op:\n%s", out)
	}
}

// A DIFF ACROSS THE BOUNDARY SHOWS THE TOMBSTONE.
//
// This is the reading the scrub's own stamp exists to make answerable. A diff
// between a scrubbed revision and a later one shows an address turning into
// `__scrubbed__`, and a reader with no explanation would have to decide
// between "somebody ran the erasure" and "this row is damaged". The value is
// deliberately distinctive for that reason — and deliberately NOT the
// credential mask, which means the opposite: that a value is there and is
// being withheld.
func TestADiffAcrossAScrubShowsTheTombstone(t *testing.T) {
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)

	first := companyFile(t, dir, "first.yaml", func(string) string { return scrubCompanyDoc })
	if _, _, err := configCmd(t, cfg, "import", first); err != nil {
		t.Fatalf("import: %v", err)
	}
	old := activeRevisionID(t, cfg)
	second := companyFile(t, dir, "second.yaml", func(string) string {
		return scrubCompanyDoc + "mission: ship it\n"
	})
	if _, _, err := configCmd(t, cfg, "import", second); err != nil {
		t.Fatalf("import: %v", err)
	}
	if _, _, err := configCmd(t, cfg, "scrub"); err != nil {
		t.Fatalf("scrub: %v", err)
	}

	out, errs, err := configCmd(t, cfg, "diff", old)
	if err != nil {
		t.Fatalf("diff: %v (%s)", err, errs)
	}
	if !strings.Contains(out, "__scrubbed__") {
		t.Errorf("the diff across the scrub does not show the tombstone, so "+
			"a reader cannot tell an erasure from a corrupted row:\n%s", out)
	}
}

// holdsTheAddress reports whether any SUPERSEDED revision still carries it.
func holdsTheAddress(t *testing.T, cfg string) bool {
	t.Helper()
	active := activeRevisionID(t, cfg)
	for _, id := range revisionIDs(t, cfg) {
		if id == active {
			continue
		}
		out, _, err := configCmd(t, cfg, "export", "-revision", id)
		if err != nil {
			t.Fatalf("export %s: %v", id, err)
		}
		if strings.Contains(out, "sarah@example.com") {
			return true
		}
	}
	return false
}

// revisionIDs is every revision id the listing carries, in its own order.
func revisionIDs(t *testing.T, cfg string) []string {
	t.Helper()
	out, _, err := configCmd(t, cfg, "revisions")
	if err != nil {
		t.Fatalf("revisions: %v", err)
	}
	var ids []string
	for _, line := range strings.Split(out, "\n") {
		// THE ACTIVE ROW IS MARKED WITH A LEADING `*`, so the id is the
		// second field there and the first everywhere else — which is
		// also what [activeRevisionID] reads.
		for _, field := range strings.Fields(line) {
			if len(field) == 36 && strings.Count(field, "-") == 4 {
				ids = append(ids, field)
				break
			}
		}
	}
	if len(ids) == 0 {
		t.Fatalf("no revision ids in:\n%s", out)
	}
	return ids
}

// THE WALK COVERS THE WHOLE TABLE, NOT ITS FIRST PAGE.
//
// # Why this needs a case of its own
//
// The failure is silent in the direction that matters: a walk that stopped at
// one page would report a clean scrub over an archive it had read the newest
// few rows of, and every older revision would keep its addresses with an
// operator believing otherwise. It is also invisible on any realistic
// fixture, because a page is two hundred revisions and importing two hundred
// and one of them would take minutes to assert one thing — which is why the
// page size is a parameter of the walk rather than a constant read inside it.
func TestTheScrubWalksPastTheFirstPage(t *testing.T) {
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)

	// FIVE REVISIONS, so four are superseded and a page of two takes three
	// round trips — including a last one that comes back short, which is
	// the loop's own exit.
	for i := range 5 {
		path := companyFile(t, dir, "r.yaml", func(string) string {
			return scrubCompanyDoc + "mission: revision " + string(rune('a'+i)) + "\n"
		})
		if _, errs, err := configCmd(t, cfg, "import", path); err != nil {
			t.Fatalf("import %d: %v (%s)", i, err, errs)
		}
	}

	cs, closeStore, err := openConfigStore(t.Context(), cfg)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer closeStore()

	targets, err := scrubTargets(t.Context(), cs, "", 2)
	if err != nil {
		t.Fatalf("scrubTargets: %v", err)
	}
	if len(targets) != 4 {
		t.Errorf("the walk found %d superseded revisions, want 4 — a walk "+
			"that stops at a page boundary reports a clean scrub over an "+
			"archive it never read", len(targets))
	}
	for _, rev := range targets {
		if rev.Active {
			t.Errorf("the active revision %s is in the walk's targets", rev.ID)
		}
	}
}
