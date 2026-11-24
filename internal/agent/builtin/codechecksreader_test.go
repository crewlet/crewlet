package builtin_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
	"github.com/crewlet/crewlet/internal/tools"
)

// gitletChecks is site's Gitlet with its checks read by Gitlet's own reader,
// as a data node answers them, its query validated and all; every other
// question is the host's. A checks read is answered at `stale`: the estate
// has no log to prove a seat's linearizable read against
// (statelogtest.LocalReader), and the level is no part of what is judged here;
// every other field is the tool's own.
type gitletChecks struct {
	*gitletHost
	reader *gitlet.Reader
}

func (h *gitletChecks) Read(ctx context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
	if q.Kind != gitlet.ReadChecks && q.Kind != gitlet.ReadCheck {
		return h.gitletHost.Read(ctx, q)
	}
	h.mu.Lock()
	h.reads = append(h.reads, q)
	h.mu.Unlock()
	q.Fresh = statelog.Freshness{Level: statelog.ReadStale}
	return h.reader.Read(ctx, q)
}

// engChart is the chart a seat's checks read is judged by: agent-cto, eng's,
// at full access.
type engChart struct{}

func (engChart) Seat(handle string) (gitlet.RosterSeat, bool) {
	return gitlet.RosterSeat{Handle: handle, Tier: gitlet.TierFullAccess, Unit: "eng"}, handle == "agent-cto"
}
func (engChart) Lead(string) string                { return "" }
func (engChart) Leads(string) []string             { return nil }
func (engChart) Within(unit, ancestor string) bool { return unit == ancestor }

// newGitletChecks is [newGitletHost] whose checks are rows of an estate a
// real reader reads: site, company-visible, and the errored attempt 1 of its
// `unit` check of main, for main, applied through the host's checkpoint.
func newGitletChecks(t *testing.T) *gitletChecks {
	t.Helper()
	host := newGitletHost()
	node, db := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() { _ = node.Close() })
	at, ended := host.at, store.EncodeTime(time.Date(2031, 4, 2, 3, 14, 0, 0, time.UTC))
	for _, row := range []struct {
		statement string
		args      []any
	}{
		{`INSERT INTO code_repos (id, name, status, managed_by, owner_kind, owner, visibility, default_branch,
			created_at) VALUES (?, 'site', 'active', 'config', 'unit', 'eng', 'company', 'main', 0)`,
			[]any{siteID}},
		{`INSERT INTO code_names (token, name, repo_id) VALUES (?, 'site', ?)`,
			[]any{gitlet.NameToken("site"), siteID}},
		{`INSERT INTO code_checks (repo_id, commit_oid, target_ref, name, attempt, state, purpose, change_id,
			revision, candidate_id, entry_id, base_oid, defs_oid, required, timeout_seconds, reason,
			runner_node, placement, box_handle, reporter, queued_at, started_at, finished_at, duration_ms,
			exit_code, failures_omitted, queued_position, finished_position, row_bytes, overdue_at, url,
			summary, def, failures, tail)
			VALUES (?, ?, 'refs/heads/main', 'unit', 1, 'errored', 'tip', '', 0, '', '', '', '', 1, 600,
				'box_failed', 'node-a', 'container', '', '', ?, ?, ?, 1000, NULL, 0, 1, 2, 4, ?, '', '', '',
				'', '')`,
			[]any{siteID, mainOID, ended, ended, ended, ended}},
		{`INSERT INTO statelog_cursor (stream, generation, seq, stream_created_at, updated_at, applied_version)
			VALUES (?, ?, ?, 1, 0, 0)`, []any{at.Stream, int64(at.Generation), int64(at.Seq)}},
	} {
		if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(t.Context(), row.statement, row.args...)
			return err
		}); err != nil {
			t.Fatalf("seed site's checks: %v", err)
		}
	}
	authority, err := statelogtest.LocalReader(gitlet.Domain{}, db.Reader(), at)
	if err != nil {
		t.Fatalf("the code log's read authority: %v", err)
	}
	reader, err := gitlet.NewReader(gitlet.ReaderOptions{Log: authority,
		Roster: func() gitlet.Roster { return engChart{} }})
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	return &gitletChecks{gitletHost: host, reader: reader}
}

// THE CHECK TOOLS' READ OF A COMMIT'S ATTEMPTS IS ONE GITLET ANSWERS. Each
// tool that finds an attempt by listing the commit's checks (a rerun, a
// report, a failure read naming no attempt) asks it of Gitlet's own reader,
// which refuses a page past the one its kind carries
// (gitlet.ReadQuery.PageMax), so each call is answered from site's rows: the
// rerun asks for unit's attempt 2, the report records lint's first, the read
// answers unit's errored attempt.
//
// Mutation: list the attempts a page of gitlet.MaxSnapshotItems again, and
// every call is refused invalid, naming the limit, with nothing sent.
func TestTheCheckToolsReadTheirAttemptsFromGitletsReader(t *testing.T) {
	t.Parallel()
	host, writer := newGitletChecks(t), &codeWrites{}
	reg := codeRegistry(t, writeDeps(host, writer))
	answered := func(what string, got tools.Result) {
		t.Helper()
		if got.Failed {
			t.Fatalf("%s answered %s %q, want it read from site's checks", what, tools.RefusalOf(got), got.Output)
		}
	}

	answered("a seat's rerun of unit", callWrite(everyGrant(), t, reg, writerTurn(t), builtin.CodeRerunChecksTool,
		map[string]any{"repo": "site", "sha": mainOID, "checks": []any{map[string]any{"name": "unit",
			"attempt": 1}}}))
	rerun := gitlet.CheckKey{RepoID: siteID, OID: mainOID, Target: "refs/heads/main", Name: "unit", Attempt: 2}
	if sent := writer.checksAsked(); len(sent) != 1 || sent[0].Rerun == nil || sent[0].Rerun.CheckKey != rerun {
		t.Fatalf("a seat's rerun of unit sent %+v, want a rerun of %+v", sent, rerun)
	}

	deps := writeDeps(host, writer)
	deps.Actor = builtin.PrincipalActor
	report := operatorToolOver(t, builtin.OperatorDeps{Code: deps, Authorize: builtin.Decide(chartLeads)},
		builtin.CodeReportCheckTool)
	got, err := report.CallForTurn(asBoundPerson(), nil, map[string]any{"repo": "site", "sha": mainOID,
		"name": "lint", "status": "failed", "summary": "2 lint errors", "url": "https://ci.example.com/runs/1"})
	if err != nil {
		t.Fatalf("a person's report of lint: %v", err)
	}
	answered("a person's report of lint", got)
	reported := gitlet.CheckKey{RepoID: siteID, OID: mainOID, Target: "refs/heads/main", Name: "lint", Attempt: 1}
	if sent := writer.checksAsked(); len(sent) != 2 || sent[1].Report == nil || sent[1].Report.CheckKey != reported {
		t.Fatalf("a person's report of lint sent %+v, want a report of %+v", sent, reported)
	}

	got, out := callCode(t, reg, codeTurn(t), builtin.CodeGetCheckFailuresTool,
		map[string]any{"repo": "site", "sha": mainOID, "name": "unit"})
	answered("a seat's read of unit's failures", got)
	if out["state"] != string(gitlet.CheckStateErrored) || out["attempt"] != float64(1) {
		t.Errorf("a seat's read of unit's failures answered %s, want its errored attempt 1", got.Output)
	}

	reads, _, _ := host.asked()
	if listed := readsOf(reads, gitlet.ReadChecks); listed != 3 {
		t.Errorf("the three calls listed site's checks %d times, want once each", listed)
	}
}
