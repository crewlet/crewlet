package executor_test

import (
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// spend is a result's charge, compared field by field.
type spend struct {
	input, output, cacheRead, cacheWrite int
	cost                                 float64
}

func spendOf(r sandbox.Result) spend {
	return spend{input: r.InputTokens, output: r.OutputTokens,
		cacheRead: r.CacheReadTokens, cacheWrite: r.CacheWriteTokens, cost: r.CostUSD}
}

func (s spend) near(o spend) bool {
	return s.input == o.input && s.output == o.output && s.cacheRead == o.cacheRead &&
		s.cacheWrite == o.cacheWrite && math.Abs(s.cost-o.cost) < 1e-9
}

// claudeResultLine is a Claude Code result line stating the session's totals
// (usage, total_cost_usd) and, where since is not "", the run's own figures
// since its previous result.
func claudeResultLine(input, output, read, write int, cost float64, since string) string {
	line := `{"type":"result","subtype":"success","is_error":false,"result":"done",` +
		`"session_id":"5e55f1d0-0000-4000-8000-00000000c1a0","num_turns":3,` +
		`"total_cost_usd":` + ftoa(cost) + `,"usage":{"input_tokens":` + itoa(input) +
		`,"output_tokens":` + itoa(output) + `,"cache_read_input_tokens":` + itoa(read) +
		`,"cache_creation_input_tokens":` + itoa(write) + `}`
	if since != "" {
		line += `,"model_usage_since_last_result":` + since
	}
	return line + "}"
}

func itoa(n int) string { return strconv.Itoa(n) }

func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// A CLAUDE CODE RUN IS CHARGED WHAT ITS OWN RESULTS STATED. From 2.1.310 each
// result states, per model, what the calls this process made since its
// previous result cost; nothing another process counted is carried into it, so
// the sum over the run's results is the run's own spend — however many results
// it printed (a note handed to it as it finished carries its loop on to
// another), and whatever the session's totals beside it say after a resume.
//
// Mutation: charge the last result's usage, or only its per-run figures, and
// the run pays for the session's earlier jobs, or loses its first loop's
// calls.
func TestAClaudeRunIsChargedWhatItsOwnResultsStated(t *testing.T) {
	t.Parallel()
	runner := executor.NewClaudeCode()
	b := box(t, runner)
	p := paths(b)
	first := claudeResultLine(5000, 900, 70000, 4000, 9.5,
		`{"claude-opus-5-5":{"inputTokens":10,"outputTokens":5,"cacheReadInputTokens":100,`+
			`"cacheCreationInputTokens":1,"costUSD":0.10}}`)
	last := claudeResultLine(5030, 912, 70300, 4003, 9.8,
		`{"claude-opus-5-5":{"inputTokens":15,"outputTokens":4,"cacheReadInputTokens":150,`+
			`"cacheCreationInputTokens":1,"costUSD":0.12},`+
			`"claude-haiku-5-5":{"inputTokens":5,"outputTokens":3,"cacheReadInputTokens":50,`+
			`"cacheCreationInputTokens":1,"costUSD":0.08}}`)
	b.Put(p.Stream(), first+"\n"+last+"\n")
	b.Put(p.Result(), last)
	b.Put(p.ExitCode(), "0")

	res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	want := spend{input: (10 + 100 + 1) + (15 + 150 + 1) + (5 + 50 + 1), output: 5 + 4 + 3,
		cacheRead: 100 + 150 + 50, cacheWrite: 1 + 1 + 1, cost: 0.10 + 0.12 + 0.08}
	if got := spendOf(res); !got.near(want) {
		t.Fatalf("the run was charged %+v, want what its results stated, %+v", got, want)
	}
}

// A RESULT THAT STATES NO PER-RUN FIGURES IS CHARGED AS IT IS, where the run
// did not continue a session: its totals are the run's own on every release.
//
// Mutation: read the per-run figures as zero when absent, and the run is free.
func TestAFreshClaudeRunOnAnOlderReleaseIsChargedItsResult(t *testing.T) {
	t.Parallel()
	runner := executor.NewClaudeCode()
	b := box(t, runner)
	p := paths(b)
	line := claudeResultLine(40, 20, 300, 7, 0.42, "")
	b.Put(p.Stream(), line+"\n")
	b.Put(p.Result(), line)
	b.Put(p.ExitCode(), "0")

	res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got, want := spendOf(res), (spend{input: 347, output: 20, cacheRead: 300, cacheWrite: 7, cost: 0.42}); !got.near(want) {
		t.Fatalf("a fresh run was charged %+v, want its result as stated, %+v", got, want)
	}
}

// A CONTINUED RUN ON A RELEASE THAT RESTORES THE SESSION'S TOTALS IS CHARGED
// THE DIFFERENCE. From 2.1.277 a continued session starts from the totals the
// job before it saved, and before 2.1.310 its result states no per-run
// figures: the run keeps the job before's result as it starts, before its own
// artefacts are cleared, and the collection takes it off. A release before
// 2.1.277 starts the session's totals at zero, so its result is the run's own.
//
// Mutation: keep no base at Start, or subtract it whatever the release, and a
// case below is charged the session's earlier jobs, or a negative spend.
func TestAContinuedClaudeRunIsChargedOnlyWhatItAddedToTheSession(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		version string
		want    spend
	}{
		"a release that restores the totals": {version: "2.1.290 (Claude Code)",
			want: spend{input: (130 + 1600 + 70) - (100 + 1000 + 50), output: 15, cacheRead: 600,
				cacheWrite: 20, cost: 0.60}},
		"a release that starts them at zero": {version: "2.1.263 (Claude Code)",
			want: spend{input: 130 + 1600 + 70, output: 25, cacheRead: 1600, cacheWrite: 70, cost: 1.60}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner := executor.NewClaudeCode()
			b := box(t, runner)
			p := paths(b)
			// The job before: parked on a question, its result left in the box.
			b.Put(p.Result(), claudeResultLine(100, 10, 1000, 50, 1.00, ""))

			if _, err := runner.Start(t.Context(), b, sandbox.RunRequest{
				Brief: "the answer: main", Resume: "5e55f1d0-0000-4000-8000-00000000c1a0"}); err != nil {
				t.Fatalf("Start: %v", err)
			}
			kept, err := b.ReadFile(t.Context(), p.BaseResult())
			if err != nil || !strings.Contains(string(kept), `"input_tokens":100`) {
				t.Fatalf("the continued run kept no base (%v): %s", err, kept)
			}

			// The continued job's end: the session's totals, no per-run figures.
			line := claudeResultLine(130, 25, 1600, 70, 1.60, "")
			b.Put(p.Stream(), line+"\n")
			b.Put(p.Result(), line)
			b.Put(p.CLIVersion(), tc.version)
			b.Put(p.ExitCode(), "0")

			res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{})
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if got := spendOf(res); !got.near(tc.want) {
				t.Fatalf("the continued run was charged %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A RUN THAT STARTS A SESSION OF ITS OWN KEEPS NO BASE: there is nothing it
// took up, and a base left from an earlier job would take that job's spend off
// a run that never continued it.
//
// Mutation: keep the base whatever the request, and the fresh run's base is
// the job before's.
func TestAFreshRunKeepsNoBase(t *testing.T) {
	t.Parallel()
	runner := executor.NewClaudeCode()
	b := box(t, runner)
	p := paths(b)
	b.Put(p.Result(), claudeResultLine(100, 10, 1000, 50, 1.00, ""))
	if _, err := runner.Start(t.Context(), b, sandbox.RunRequest{Brief: "a new task"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if kept, _ := b.ReadFile(t.Context(), p.BaseResult()); len(kept) != 0 {
		t.Fatalf("a fresh run kept a base: %s", kept)
	}
}

// A CONTINUED CLAUDE CODE RUN RECORDS ITS HARNESS'S RELEASE before the harness starts,
// asked of the harness the box's PATH finds, which the collection reads to know
// whether the result states the session's spend. A fresh run records none.
//
// Mutation: drop the record, and a continued run on 2.1.277–2.1.309 is read as
// a current release with nothing to say otherwise — or, recorded for every
// run, a fresh run asks its harness twice for nothing.
func TestAContinuedClaudeRunRecordsItsRelease(t *testing.T) {
	t.Parallel()
	p := executor.Paths{Home: "/box"}
	continued := scriptOf(t, claude().Command(sandbox.RunRequest{Brief: "x", Resume: "sess"}, p, ""))
	if want := "claude --version < /dev/null > '" + p.CLIVersion() + "' 2> /dev/null; "; !strings.HasPrefix(continued, want) {
		t.Errorf("a continued run does not record its release first:\n%s", continued)
	}
	if !strings.Contains(continued, "exec claude -p ") || !strings.Contains(continued, "--resume 'sess'") {
		t.Errorf("a continued run does not then resume its session:\n%s", continued)
	}
	if fresh := claude().Command(sandbox.RunRequest{Brief: "x"}, p, ""); strings.Contains(fresh, "--version") {
		t.Errorf("a fresh run records a release:\n%s", fresh)
	}
}

// THE RELEASE THAT RESTORES A CONTINUED SESSION'S TOTALS is read off what the
// harness printed: from 2.1.277 on, and a release this code cannot read is taken
// for a current one, as the notes gate takes it.
func TestTheReleaseThatRestoresTotalsIsReadOffTheHarness(t *testing.T) {
	t.Parallel()
	for version, want := range map[string]bool{
		"2.1.276 (Claude Code)": false,
		"2.1.277 (Claude Code)": true,
		"2.1.310 (Claude Code)": true,
		"2.2.0 (Claude Code)":   true,
		"1.9.400":               false,
		"":                      true,
		"Claude Code nightly":   true,
	} {
		if got := (executor.ClaudeCode{}).TotalsRestored(version); got != want {
			t.Errorf("TotalsRestored(%q) = %v, want %v", version, got, want)
		}
	}
}

// AN OPENCODE RUN THAT CONTINUED A SESSION IS CHARGED WHAT THE SESSION GAINED
// DURING IT. OpenCode's report is the session's lifetime totals — every step
// it ever ran, and what an import restored — so the run's script reads the
// report before the run takes the session up too, and the run is charged the
// difference.
//
// Mutation: ignore the report before, and the run pays for every step the
// session ever ran.
func TestAContinuedOpenCodeRunIsChargedWhatItsSessionGained(t *testing.T) {
	t.Parallel()
	stream := strings.Join([]string{
		`{"type":"crewlet.usage_before","report":{"data":{"id":"ses_abc","cost":0.40,` +
			`"tokens":{"input":80,"output":15,"reasoning":4,"cache":{"read":200,"write":5}}}}}`,
		`{"type":"text","part":{"text":"done"}}`,
		`{"type":"crewlet.usage","report":{"data":{"id":"ses_abc","cost":0.70,` +
			`"tokens":{"input":100,"output":20,"reasoning":5,"cache":{"read":300,"write":7}}}}}`,
	}, "\n")
	res := opencode().Parse(stream)
	want := spend{input: (100 + 300 + 7) - (80 + 200 + 5), output: 25 - 19, cacheRead: 100, cacheWrite: 2, cost: 0.30}
	if got := spendOf(res); !got.near(want) {
		t.Fatalf("the continued run was charged %+v, want what its session gained, %+v", got, want)
	}
	if res.SessionID != "ses_abc" || res.Text != "done" {
		t.Errorf("session %q answer %q: the report before leaked into the result", res.SessionID, res.Text)
	}
}

// A CONTINUED OPENCODE RUN'S SCRIPT ASKS FOR THE REPORT BEFORE THE RUN, of the
// session it continues and from the server that runs it; a fresh run's does
// not, since its session's totals begin at the run.
//
// Mutation: ask after the run, or for every run, and the case goes red.
func TestAContinuedOpenCodeRunReadsItsSessionBeforeTheRun(t *testing.T) {
	t.Parallel()
	const session = "0123456789abcdef0123456789abcdef"
	continued := scriptOf(t, opencode().Command(sandbox.RunRequest{Brief: "x", Resume: "ses_" + session},
		executor.Paths{}, ""))
	before := strings.Index(continued, `"type":"crewlet.usage_before"`)
	run := strings.Index(continued, `opencode run --server "$url"`)
	if before < 0 || run < 0 || before > run {
		t.Fatalf("a continued run does not read its session's report before the run:\n%s", continued)
	}
	if !strings.Contains(continued[:run], "session.get --param sessionID='ses_"+session+"'") {
		t.Errorf("the report before is not the continued session's:\n%s", continued[:run])
	}
	fresh := scriptOf(t, opencode().Command(sandbox.RunRequest{Brief: "x"}, executor.Paths{}, ""))
	if strings.Contains(fresh, "crewlet.usage_before") {
		t.Errorf("a fresh run reads a report before its run:\n%s", fresh)
	}
}
