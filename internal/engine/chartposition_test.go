package engine_test

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// WHAT A UNIT'S PROJECT AND KNOWLEDGE SPACE ARE STAMPED WITH, AND WHY IT IS NOT
// A CLOCK.
//
// A project and a container are DERIVED from the org chart: the chart names
// them, and every node writes them from the chart it composed. Two nodes can
// compose two charts at once — one of them behind — so each write carries an
// order the apply compares against the row's, and the one that wins is the
// later CHART, never the later write. That order is the position on the
// chart's own log the composition read ([engine.Company.ChartAt]): every node
// reads the same log, so the same chart is the same position everywhere, and
// a node behind composes at a smaller one. A wall clock — the boot's own, or
// an activation's instant — ordered the settings revision rather than the
// chart, which is the wrong thing now that the chart is a log of its own.
//
// The comparison itself is the tracker's and the knowledge base's, and their
// own suites hold it. What these cases hold is the engine's half: that the
// stamp a converge writes IS the position its company was composed at.

// chartDoc is a company whose one unit names a project and a knowledge
// space, with its purpose left as a hole a case fills — the one chart-owned
// field a case can change without renaming anything, and one both the project
// and the container carry.
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
    space: ENGDOCS
    purpose: PURPOSE
    roles:
      - name: CEO
        handle: ceo
        llm: zulu
`

func chartCompany(purpose string) string {
	return strings.Replace(chartDoc, "PURPOSE", purpose, 1)
}

// A BOOT STAMPS THE CHART'S PROJECT AND SPACE WITH THE POSITION ITS VIEW READ.
func TestABootStampsTheChartsProjectAndSpaceWithTheViewsPosition(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{Company: parsedCompany(t, chartCompany("builds it"))})
	purpose, at := eventuallyStampedAtView(t, e, "ENG", "ENGDOCS")
	if purpose != "builds it" {
		t.Errorf("the project's purpose is %q", purpose)
	}
	if at <= 0 {
		t.Fatalf("the project is stamped %d — a company composed from the "+
			"chart's rows is composed at a position on its log", at)
	}
}

// A CHART WRITE RESTAMPS THEM AT THE LATER POSITION IT PUBLISHED.
//
// No config apply is involved, which is the point: the unit's purpose is
// chart content, so the change arrives as a record on the chart's log and the
// view's own rebuild is what carries it to the project and the space.
func TestAChartWriteRestampsTheProjectAndSpaceAtItsPosition(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{Company: parsedCompany(t, chartCompany("builds it"))})
	_, before := eventuallyStampedAtView(t, e, "ENG", "ENGDOCS")

	unit := unitRow(t, e, "ENG")
	written, err := e.ChartWriter().WriteUnit(t.Context(),
		statelog.NewOpID(time.Now(), "unit-purpose"), chart.UnitContent{
			Key: unit.Key, Name: unit.Name, Type: unit.Type,
			Purpose: "ships it", Goals: unit.Goals, Channel: unit.Channel,
			Project: unit.Project, Space: unit.Space,
			KnowledgeRefs: unit.KnowledgeRefs,
		})
	if err != nil || written.Outcome != statelog.OutcomeApplied {
		t.Fatalf("write the unit's purpose: (%+v, %v)", written.Result, err)
	}
	if _, err := engine.RefreshChartForTest(t.Context(), e); err != nil {
		t.Fatalf("refresh the chart view: %v", err)
	}
	purpose, after := eventuallyStampedAtView(t, e, "ENG", "ENGDOCS")
	if purpose != "ships it" {
		t.Errorf("after the chart write the project's purpose is %q", purpose)
	}
	if after <= before {
		t.Errorf("after the chart write the project is stamped %d, not past the "+
			"boot's %d — a peer still composing the boot's chart would win", after,
			before)
	}
	if space := containerRow(t, e, "ENGDOCS"); space.Purpose != "ships it" {
		t.Errorf("after the chart write the unit's knowledge space reads %q",
			space.Purpose)
	}
}

// CONVERGING AGAIN ON ONE CHART WRITES NOTHING — which is every re-apply of an
// unchanged revision and every periodic rebuild on every node.
func TestConvergingAgainOnOneChartPutsNoRecordOnTheLog(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{Company: parsedCompany(t, chartCompany("builds it"))})
	eventuallyStampedAtView(t, e, "ENG", "ENGDOCS")
	before := projectHistory(t, e)
	if _, _, err := e.Apply(t.Context(),
		parsedCompany(t, chartCompany("builds it"))); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, err := engine.RefreshChartForTest(t.Context(), e); err != nil {
		t.Fatalf("refresh the chart view: %v", err)
	}
	if after := projectHistory(t, e); after != before {
		t.Errorf("converging again on the chart the boot converged on wrote %d "+
			"project record(s) — every apply and every rebuild on every node "+
			"would", after-before)
	}
}

// eventuallyStampedAtView waits until a unit's project and its knowledge space
// both carry the position the published company was composed at, and answers
// the project's purpose and that position.
func eventuallyStampedAtView(t *testing.T, e *engine.Engine, project, space string) (string, int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		at := e.Company().ChartAt
		purpose, stamped, found, err := readProject(t, e, project)
		if err != nil {
			t.Fatalf("read project %s: %v", project, err)
		}
		container, held := readContainer(t, e, space)
		if found && held && stamped == at && container.ChartPosition == at {
			return purpose, at
		}
		if time.Now().After(deadline) {
			t.Fatalf("project %s (found %v, stamped %d) and space %s (found %v, "+
				"stamped %d) never both carried the published company's chart "+
				"position %d", project, found, stamped, space, held,
				container.ChartPosition, at)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// unitRow is the chart's row for the unit whose project is project.
func unitRow(t *testing.T, e *engine.Engine, project string) chart.Unit {
	t.Helper()
	for _, unit := range readChart(t, e).Units {
		if unit.Project == project {
			return unit
		}
	}
	t.Fatalf("no chart unit names project %s", project)
	return chart.Unit{}
}

func readProject(t *testing.T, e *engine.Engine, key string) (string, int64, bool, error) {
	t.Helper()
	var purpose string
	var at int64
	err := e.Backends().Store.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(),
			`SELECT purpose, chart_position FROM tracker_projects WHERE key = ?`,
			key).Scan(&purpose, &at)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, nil
	}
	return purpose, at, err == nil, err
}

// containerRow reads a container that must already be there.
func containerRow(t *testing.T, e *engine.Engine, key string) pages.Container {
	t.Helper()
	c, found := readContainer(t, e, key)
	if !found {
		t.Fatalf("container %s is not on this node", key)
	}
	return c
}

func readContainer(t *testing.T, e *engine.Engine, key string) (pages.Container, bool) {
	t.Helper()
	var document []byte
	err := e.Backends().Store.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(),
			`SELECT document FROM pages_containers WHERE key = ?`, key).Scan(&document)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return pages.Container{}, false
	}
	if err != nil {
		t.Fatalf("read container %s: %v", key, err)
	}
	c, err := pages.DecodeContainer(document)
	if err != nil {
		t.Fatalf("decode container %s: %v", key, err)
	}
	return c, true
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
