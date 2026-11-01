package harness

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// toolCompletion turns a finished run with tools into an answer or a
// classified failure — after reporting, on EVERY outcome, what the run cost
// that nothing has reported yet ([recordReader.settle]).
//
// # What is different from a text function's ([Provider.completion])
//
//   - A run the host ended is an answer of what it did, not a failure: the
//     host decided, and the run's record is the tool loop's to read.
//   - A run its CLI's own turn cap ended is an answer stopped at the cap
//     ([llm.StopCap]), where the profile can tell one ([harnessprofile.Cap]).
//   - A run that never listed the host's tools had none, which is the CLI or
//     its configuration failing rather than a model that called none.
//   - The answer is the model's LAST response's prose where the record shows
//     the requests — the earlier ones were the host's, observed as the run
//     went — and the profile's answer path otherwise.
//   - NOTHING THE RUN PRINTED IS QUOTED IN AN ERROR. Its output carries every
//     call's arguments and every tool's result, and an error is shown and
//     logged wherever the failure goes; what a failure says is stderr and the
//     CLI's own failure events by field ([harnessprofile.MCP.FailureEvents]).
func (p *Provider) toolCompletion(
	ctx context.Context, res *rawResult, usageDoc string, t *toolRun, connected bool,
) (*llm.Completion, error) {
	out, read := t.reader.stdoutResult()
	if !read {
		out = extract(p.profile.Routed(), res.stdout)
	}
	failed := res.exitCode != 0 || out.failed
	if outcome := out.applyUsageReport(p.profile, usageDoc); outcome.unreadable() && !failed {
		log.WarnContext(ctx, "cli_agent_usage_report_unreadable", "provider", p.key,
			"agent", p.harness, "source", usageSource(p.profile), "reason", outcome.String(),
			"input_paths", harnessprofile.PathList(p.profile.Usage.Input),
			"output_paths", harnessprofile.PathList(p.profile.Usage.Output),
			"counts_from", "the record, or an estimate")
	}

	last, model, observed := t.reader.last()
	answer := out.text
	if observed {
		answer = last
	}
	var total *llm.Usage
	if out.reported {
		u := llm.Usage{
			Input:      p.profile.Usage.PromptTokens(out.input, out.cacheRead, out.cacheWrite) + out.toolPrompt,
			Output:     out.output + out.reasoning,
			CacheRead:  out.cacheRead,
			CacheWrite: out.cacheWrite,
		}
		total = &u
	}
	spent := t.reader.settle(total, answer)
	completion := func(stop llm.StopReason) *llm.Completion {
		return &llm.Completion{
			Model: nonEmpty(model, p.model), Content: answer, StopReason: stop,
			InputTokens: spent.Input, OutputTokens: spent.Output,
			CacheRead: spent.CacheRead, CacheWrite: spent.CacheWrite,
		}
	}
	events := t.reader.failureDetail()
	if !connected {
		events = joinDetail("it never listed the seat's tools", events)
	}

	switch {
	case res.failureMarker != "":
		return nil, p.confinementFailed(res.failureMarker, res.stderr)
	case res.timedOut:
		return nil, p.fail(llm.KindTimeout, fmt.Errorf("the model worked longer than %s without "+
			"calling a tool or answering — raise timeout_seconds if this model legitimately "+
			"reasons for longer%s", p.timeout, failureDetail(res.stderr, events)))
	case res.stopped:
		// The host ended the run: whatever the CLI's exit says is the
		// signal's doing. No stop reason — the run ending was the host's.
		return completion(""), nil
	case t.lost:
		// The run's loop was its CLI's server's, and the run was aborted
		// when it went: whatever the run's own exit says is that.
		return nil, p.fail(llm.KindServer, fmt.Errorf("the %s CLI's server exited %d during the run%s",
			p.harness, t.server.exitCode, failureDetail(res.stderr, events)))
	}
	if res.droppedStdout > 0 && !read {
		return nil, p.fail(llm.KindServer, fmt.Errorf(
			"the CLI wrote more than %d bytes to stdout and %d were dropped, so the answer is "+
				"incomplete: run `crewlet llm doctor %s` to see what it printed",
			maxOutput, res.droppedStdout, p.key))
	}
	if p.capped(res, t.reader) {
		return completion(llm.StopCap), nil
	}
	// The CLI's own words where they stand: the answer it located and
	// stderr — never the whole of stdout, which is the tools' words too.
	if hit, ok := classifyMarkers(p.profile, harnessOutput{
		said: out.text, stdout: res.stdout, stderr: res.stderr, failed: failed,
		events: t.reader.failureLines(), at: p.now(),
	}); ok {
		return nil, p.classified(ctx, hit, nonEmpty(hit.Said, firstLine(out.text, res.stderr)))
	}
	if !connected {
		return nil, p.fail(llm.KindServer, fmt.Errorf("the %s CLI never reached the seat's tools: "+
			"it did not list the crewlet server's tools, so the profile's mcp block does not "+
			"match the installed CLI or the relay could not start — run `crewlet llm doctor %s`%s",
			p.harness, p.key, failureDetail(res.stderr, t.reader.failureDetail())))
	}
	if failed {
		what := fmt.Sprintf("the %s CLI exited %d", p.harness, res.exitCode)
		if res.exitCode == 0 {
			what = fmt.Sprintf("the %s CLI exited 0 and reported that the run failed", p.harness)
		}
		return nil, p.fail(llm.KindServer, fmt.Errorf("%s%s", what, failureDetail(res.stderr, events)))
	}
	if why := res.unfinished(); why != "" {
		return nil, p.fail(llm.KindServer, fmt.Errorf("the %s CLI exited 0, but %s%s",
			p.harness, why, failureDetail(res.stderr, events)))
	}
	if !observed && !out.located {
		// THE PROFILE HAS DRIFTED FROM THE CLI — see [Provider.completion] —
		// said by the record's shape, never its lines.
		if strings.TrimSpace(res.stdout) == "" {
			return nil, p.fail(llm.KindServer, fmt.Errorf(
				"the %s CLI exited 0 but printed nothing at all%s", p.harness, stderrDetail(res.stderr)))
		}
		return nil, p.fail(llm.KindServer, fmt.Errorf(
			"the %s CLI's record showed no model request and none of the text_paths this "+
				"profile looks in resolved (%s) — the profile no longer matches the installed "+
				"CLI. Run `crewlet llm doctor %s` and set providers.llm.%s.overrides.requests "+
				"or .text_paths. Its output was %s",
			p.harness, harnessprofile.PathList(p.profile.TextPaths), p.key, p.key, t.reader.shape()))
	}
	if strings.TrimSpace(answer) == "" {
		// A model outcome, the tool loop's to correct — see
		// [Provider.completion] — said here, where the counts are in hand.
		log.WarnContext(ctx, "cli_agent_answered_nothing", "provider", p.key, "agent", p.harness,
			"model", model, "requests", t.reader.observed(), "output_tokens", spent.Output)
	}
	return completion(llm.StopEnd), nil
}

// confinementFailed is the failure of a CLI that said its own confinement did
// not load — FAILED CLOSED: it was stopped before it could act without it.
func (p *Provider) confinementFailed(line, stderr string) error {
	return p.fail(llm.KindServer, fmt.Errorf("the %s CLI reported that its confinement "+
		"failed to load (%q) and was stopped — the files or flags the profile gives it no "+
		"longer match the installed CLI; run `crewlet llm doctor %s`%s",
		p.harness, line, p.key, failureDetail(stderr, "")))
}

// capped reports a run its CLI's own turn cap ended, by the profile's
// signature: the exit status, and what it printed.
func (p *Provider) capped(res *rawResult, reader *recordReader) bool {
	c := p.profile.Cap
	if c == nil {
		return false
	}
	if c.ExitCode != nil && res.exitCode != *c.ExitCode {
		return false
	}
	if len(c.Match) == 0 {
		return true
	}
	if p.profile.RouteOutput() == harnessprofile.OutputJSONL {
		return reader.hitCap()
	}
	doc, ok := decodeObject(strings.TrimSpace(res.stdout))
	if !ok {
		if bare, found := outermostObject(res.stdout); found {
			doc, ok = decodeObject(bare)
		}
	}
	return ok && matches(doc, c.Match)
}

// failureDetail is what a failed run with tools says beyond its first
// sentence: the CLI's own failure events and stderr's tail, each where there
// is one.
func failureDetail(stderr, events string) string {
	var parts []string
	if strings.TrimSpace(events) != "" {
		parts = append(parts, events)
	}
	if strings.TrimSpace(stderr) != "" {
		parts = append(parts, "it wrote on stderr:\n"+tail(stderr))
	}
	if len(parts) == 0 {
		return ""
	}
	return " — " + strings.Join(parts, "; ")
}

func joinDetail(a, b string) string {
	if strings.TrimSpace(b) == "" {
		return a
	}
	return a + "; " + b
}
