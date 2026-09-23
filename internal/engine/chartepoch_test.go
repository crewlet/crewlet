package engine_test

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// chartDoc is a company whose one unit names a project, with its purpose left
// as a hole a case fills — the one chart-owned field a case can change
// without renaming anything.
const chartDoc = `
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${K}"]
units:
  - name: Engineering
    project: ENG
    purpose: PURPOSE
    roles:
      - name: CEO
        handle: ceo
        llm: zulu
`

func chartCompany(purpose string) string {
	return strings.Replace(chartDoc, "PURPOSE", purpose, 1)
}

// A BOOT STAMPS THE CHART WITH THE INSTANT ITS COMPANY WAS ACTIVATED.
//
// It used to be the boot's own clock, so a node that restarted on a revision
// the fleet had since replaced stamped NOW — later than the newer revision's
// apply — and walked every project's names back to its own old ones. The
// activation instant is the same on every node and at every boot.
func TestABootStampsTheChartWithItsCompanysActivation(t *testing.T) {
	t.Parallel()
	activated := time.Date(2026, 3, 2, 10, 0, 0, 250_000_000, time.UTC)
	e := newEngine(t, engine.Options{
		Company: parsedCompany(t, chartCompany("builds it")), ActivatedAt: activated,
	})
	purpose, epoch := eventuallyProject(t, e, "ENG")
	if purpose != "builds it" {
		t.Errorf("the project's purpose is %q", purpose)
	}
	if want := tracker.ChartEpochOf(activated); epoch != want {
		t.Errorf("the project is stamped %d, want the activation's own %d — "+
			"a boot's clock is later than every newer revision's apply", epoch, want)
	}
}

// A COMPANY NO ACTIVATION HAS NAMED IS NOT CHARTED AT BOOT, and the activation
// that does name it is what stamps its projects.
//
// A Tier B file a node booted with has no instant: its reconciler publishes
// it a moment later and applies it with the pointer's. A boot that stamped its
// own clock instead would sit ABOVE that activation, and the apply carrying
// the fleet's instant would be refused as an older chart.
func TestAnUnactivatedCompanyIsChartedByItsActivation(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{Company: parsedCompany(t, chartCompany("builds it"))})
	activated := time.Now().Add(-time.Minute)
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, chartCompany("builds it")),
		activated); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	_, epoch := eventuallyProject(t, e, "ENG")
	if want := tracker.ChartEpochOf(activated); epoch != want {
		t.Errorf("the project is stamped %d, want the activation's %d", epoch, want)
	}
}

// AN OLDER ACTIVATION APPLIED LATE DOES NOT WALK THE CHART BACK.
//
// Two nodes applying two revisions is ordinary during a rollout, and so is a
// node applying the older one second. Stamped with each apply's own clock the
// later APPLY won whatever it carried; stamped with the activation, the later
// ACTIVATION does.
func TestAnOlderActivationAppliedLateDoesNotWalkTheChartBack(t *testing.T) {
	t.Parallel()
	newer := time.Date(2026, 3, 2, 10, 0, 1, 0, time.UTC)
	older := newer.Add(-500 * time.Millisecond)
	e := newEngine(t, engine.Options{
		Company: parsedCompany(t, chartCompany("ships it")), ActivatedAt: newer,
	})
	if purpose, _ := eventuallyProject(t, e, "ENG"); purpose != "ships it" {
		t.Fatalf("the boot charted %q", purpose)
	}
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, chartCompany("builds it")),
		older); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	purpose, epoch := projectRow(t, e, "ENG")
	if purpose != "ships it" || epoch != tracker.ChartEpochOf(newer) {
		t.Errorf("after the older activation the project is (%q, %d), want the "+
			"newer one's (\"ships it\", %d)", purpose, epoch, tracker.ChartEpochOf(newer))
	}
}

// REAPPLYING ONE ACTIVATION WRITES NOTHING — which is every boot of every node
// on a company nobody has edited.
func TestReapplyingOneActivationPutsNoRecordOnTheLog(t *testing.T) {
	t.Parallel()
	activated := time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)
	e := newEngine(t, engine.Options{
		Company: parsedCompany(t, chartCompany("builds it")), ActivatedAt: activated,
	})
	eventuallyProject(t, e, "ENG")
	before := projectHistory(t, e)
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, chartCompany("builds it")),
		activated); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if after := projectHistory(t, e); after != before {
		t.Errorf("reapplying the activation the boot applied wrote %d project "+
			"record(s) — every restart of every node would", after-before)
	}
}

// THE RECONCILER HANDS THE ENGINE THE POINTER'S OWN INSTANT, which is the one
// every node reads identically — not its own clock, which is this node's
// alone and later on every node that applies later.
func TestTheReconcilerAppliesTheChartAtThePointersInstant(t *testing.T) {
	t.Parallel()
	p := planeFor(t, newEngine(t, engine.Options{
		Company: parsedCompany(t, chartCompany("builds it")),
	}))
	// AN HOUR BEFORE THE RECONCILER'S OWN CLOCK, so the two cannot be
	// mistaken for each other.
	activated := pinnedNow.Add(-time.Hour)
	document := yamlToJSON(t, chartCompany("ships it"))
	id, err := p.store.Configs().InsertActive(t.Context(), store.Revision{
		Source: "test", CreatedBy: "operator", Summary: "revision",
		Payload: document, CreatedAt: activated,
	})
	if err != nil {
		t.Fatalf("store the revision: %v", err)
	}
	if _, err := p.fleet.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: id, Summary: "revision", Payload: document, At: activated,
	}); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	purpose, epoch := eventuallyProject(t, p.engine, "ENG")
	if purpose != "ships it" || epoch != tracker.ChartEpochOf(activated) {
		t.Errorf("the project is (%q, %d), want (\"ships it\", %d) — the "+
			"activation's own instant", purpose, epoch, tracker.ChartEpochOf(activated))
	}
}

// eventuallyProject waits for a project's row to reach this node, and reads
// its chart-owned purpose and its epoch.
func eventuallyProject(t *testing.T, e *engine.Engine, key string) (string, int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		purpose, epoch, found, err := readProject(t, e, key)
		if err != nil {
			t.Fatalf("read project %s: %v", key, err)
		}
		if found {
			return purpose, epoch
		}
		if time.Now().After(deadline) {
			t.Fatalf("project %s never reached this node — the chart was not "+
				"applied", key)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// projectRow reads a project that must already be there.
func projectRow(t *testing.T, e *engine.Engine, key string) (string, int64) {
	t.Helper()
	purpose, epoch, found, err := readProject(t, e, key)
	if err != nil || !found {
		t.Fatalf("read project %s: found=%v err=%v", key, found, err)
	}
	return purpose, epoch
}

func readProject(t *testing.T, e *engine.Engine, key string) (string, int64, bool, error) {
	t.Helper()
	var purpose string
	var epoch int64
	err := e.Backends().Store.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(),
			`SELECT purpose, chart_epoch FROM tracker_projects WHERE key = ?`,
			key).Scan(&purpose, &epoch)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, nil
	}
	return purpose, epoch, err == nil, err
}

// projectHistory counts the project records this node has applied.
func projectHistory(t *testing.T, e *engine.Engine) int {
	t.Helper()
	var n int
	if err := e.Backends().Store.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM tracker_history
			WHERE kind IN (?, ?)`, string(tracker.ChangeProjectCreated),
			string(tracker.ChangeProjectUpdated)).Scan(&n)
	}); err != nil {
		t.Fatalf("count the project history: %v", err)
	}
	return n
}
