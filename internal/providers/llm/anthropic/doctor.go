package anthropic

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/crewlet/crewlet/internal/providers/credential"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/anthropic/claudemodel"
	"github.com/crewlet/crewlet/internal/textcut"
)

// What `crewlet llm doctor` reports about an anthropic entry.
//
// Two measurements, each answering a question nothing offline can:
//
//   - DOES THE TABLE STILL DESCRIBE THE MODEL? Every request is shaped from
//     [claudemodel], which is compiled into this build; the vendor's Models
//     API says what the model accepts today. The two are compared on the
//     three facts the API reports and the shape turns on — the thinking
//     types, the effort levels and the output ceiling. A disagreement that
//     makes a request this entry SENDS a 400 is a problem, because every
//     call on it fails and the chain does not retry a 400. One where the
//     table is merely more cautious than the model is a note: the entry
//     works, and nothing the operator controls would change.
//   - DOES A ROUND COME BACK AS A CALL? One request in the shape a phase
//     sends — the tool offered, no choice forced, the instruction naming it,
//     the entry's own thinking, effort lowered to `low`, streamed — and it is
//     certified by the tool_use it returns. No tool_choice is ever sent (it
//     is a 400 on Opus 5.5, Sonnet 5.5, Fable 5.1 and Mythos 5.1), so "the
//     model calls a tool it was asked to call" is exactly what every phase
//     that finishes by a call rests on, and a forced call would certify a
//     request no phase makes.
//
// THE MODELS API IS NOT REQUIRED. A gateway `base_url` may not serve
// /v1/models at all, and that is "not served" rather than a failure: the
// entry's requests do not depend on it, and the round is what says whether
// the model answers. It also runs under -no-smoke, because it bills nothing.

// modelsTimeout bounds the Models API read.
//
// A metadata GET does no model work, so the completion timeout — minutes, for
// a round that thinks — would make the doctor look wedged on an endpoint that
// is simply not answering. Thirty seconds covers a cold gateway's first
// request with room to spare and is still short enough to read as a hang.
const modelsTimeout = 30 * time.Second

// smokeTool is the one tool the round offers, and smokePrompt names it in the
// conversation, as a phase's instruction names the call that finishes it.
const (
	smokeTool   = "crewlet_smoke"
	smokePrompt = "Call the " + smokeTool + " tool with ok set to true."
)

// DiagnoseOptions are what the doctor is asked to do and the facts a provider
// cannot see about itself.
type DiagnoseOptions struct {
	// Key is the providers.llm key the entry is configured under, for the
	// report. A provider is built from an entry and never learns its name.
	Key string

	// Smoke sends the real round. It is billed, so a scripted health check
	// that runs often turns it off; the Models API read is free and runs
	// either way.
	Smoke bool
}

// Diagnosis is what `crewlet llm doctor` reports about one anthropic entry.
//
// A struct rather than printed text so the command renders it and a test
// asserts on it, as the cli-agent doctor's is.
type Diagnosis struct {
	Provider string
	Model    string
	// Profile is the capability-table row the requests are shaped from, and
	// how it was chosen.
	Profile  string
	Endpoint string
	Keys     string
	// Shape is what a phase's request carries on this entry.
	Shape string
	// ModelsAPI is what the endpoint said about the model.
	ModelsAPI string
	// Drift is every disagreement between the table and the Models API,
	// problems and notes alike.
	Drift []string
	// Smoke is the result of the real round.
	Smoke string

	Problems []string
}

// Healthy reports whether the diagnosis found nothing wrong.
func (d Diagnosis) Healthy() bool { return len(d.Problems) == 0 }

// Diagnose measures one entry.
//
// It never returns an error: every failure is a LINE in the report, because an
// operator running the doctor wants the whole picture, and stopping at the
// Models API would hide whether the model answers at all.
func (p *Provider) Diagnose(ctx context.Context, opts DiagnoseOptions) Diagnosis {
	d := Diagnosis{
		Provider: opts.Key,
		Model:    p.model,
		Profile:  p.profileLine(),
		Endpoint: p.baseURL,
		Keys:     p.keysLine(),
		Shape:    p.shapeLine(),
	}

	id := claudemodel.Normalize(p.model)
	info, err := p.modelInfo(ctx, id)
	switch {
	case notServed(err):
		d.ModelsAPI = fmt.Sprintf(
			"not served — the endpoint has no model record for %q (a gateway that does "+
				"not serve /v1/models answers the same way), so the table was not checked; "+
				"the round below is what says whether the model answers", id)
	case err != nil:
		d.ModelsAPI = "failed — " + err.Error()
		d.Problems = append(d.Problems, fmt.Sprintf(
			"the Models API read for %q failed, so neither the table nor the endpoint "+
				"was verified: %v", id, err))
	case !info.JSON.ID.Valid():
		// A 200 that is not a model record — a catch-all route answering
		// every path — says nothing about the model either way.
		d.ModelsAPI = fmt.Sprintf(
			"not served — the endpoint answered /v1/models/%s without a model record, so "+
				"the table was not checked", id)
	default:
		d.ModelsAPI = servedLine(info)
		breaking := 0
		for _, f := range drift(p.profile, p.effort, p.budget, info) {
			d.Drift = append(d.Drift, f.String())
			if f.breaks {
				breaking++
			}
		}
		// ONE problem however many findings break, carrying the remedy
		// once: the findings are listed under drift, and they share a
		// cause — the profile — so they share what to do about it.
		if breaking > 0 {
			d.Problems = append(d.Problems, fmt.Sprintf(
				"the Models API says this model refuses what this entry sends (%d of the "+
					"drift findings), so calls on it will be answered with a 400: %s",
				breaking, p.driftRemedy()))
		}
	}

	switch {
	case !opts.Smoke:
		d.Smoke = "skipped (-no-smoke)"
	default:
		d.Smoke = p.smokeTest(ctx)
		if strings.HasPrefix(d.Smoke, "failed") {
			d.Problems = append(d.Problems, d.Smoke)
		}
	}
	return d
}

// modelInfo reads the model's record, through the pool like any call: a key
// the endpoint refuses is benched and the next one is tried.
func (p *Provider) modelInfo(ctx context.Context, id string) (*sdk.ModelInfo, error) {
	return credential.Rotate(ctx, p.pool,
		credential.Identity{Provider: providerName, Model: p.model},
		p.classify,
		func(key string) (*sdk.ModelInfo, error) {
			// Escaped, because an id the table does not know is whatever
			// the entry wrote — a gateway alias may carry anything — and
			// the SDK puts it into the path as given.
			return p.client.Models.Get(ctx, url.PathEscape(id), sdk.ModelGetParams{},
				option.WithAPIKey(key), option.WithRequestTimeout(modelsTimeout))
		})
}

// notServed reports a Models API read the endpoint answered with "no such
// route or record": 404 is both an unknown model at the vendor and a gateway
// that does not serve /v1/models, and 405 and 501 are a gateway that knows the
// path and does not implement it. None of them is a fault of the entry.
func notServed(err error) bool {
	var e *llm.Error
	if !errors.As(err, &e) {
		return false
	}
	switch e.Status {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return true
	default:
		return false
	}
}

// servedLine is the model record, as the report prints it.
func servedLine(info *sdk.ModelInfo) string {
	name := info.ID
	if info.DisplayName != "" {
		name = fmt.Sprintf("%s (%s)", info.DisplayName, info.ID)
	}
	parts := []string{"served — " + name}
	if info.JSON.MaxTokens.Valid() {
		parts = append(parts, fmt.Sprintf("max_tokens %d", info.MaxTokens))
	}
	if info.JSON.MaxInputTokens.Valid() {
		parts = append(parts, fmt.Sprintf("input %d", info.MaxInputTokens))
	}
	return strings.Join(parts, ", ")
}

// finding is one disagreement between the table and the Models API.
type finding struct {
	text string
	// breaks is whether a request this entry sends is one the API says the
	// model refuses — a problem, rather than a note about a table that is
	// more cautious than it needs to be.
	breaks bool
}

func (f finding) String() string {
	if f.breaks {
		return "PROBLEM: " + f.text
	}
	return "note: " + f.text
}

// drift compares what this entry sends against the model's record.
//
// PURE, over the profile, the entry's dials and the record, so every branch is
// exercised without a server. A field the record does not carry is not
// compared: an absent capability is the API saying nothing, and reading it as
// "unsupported" would report drift against a gateway's thinner record.
func drift(profile claudemodel.Profile, effort llm.Effort, budget int64, info *sdk.ModelInfo) []finding {
	var out []finding
	caps := info.Capabilities

	// Thinking: what the entry sends, against the two types the record
	// reports.
	adaptive := caps.Thinking.Types.Adaptive
	enabled := caps.Thinking.Types.Enabled
	switch profile.Thinking {
	case claudemodel.ThinkingAdaptive:
		if adaptive.JSON.Supported.Valid() && !adaptive.Supported {
			out = append(out, finding{breaks: true, text: "every call sends adaptive thinking, " +
				"and the Models API says this model does not take it"})
		}
	case claudemodel.ThinkingBudget:
		if budget > 0 && enabled.JSON.Supported.Valid() && !enabled.Supported {
			out = append(out, finding{breaks: true, text: fmt.Sprintf("every call sends thinking "+
				"with a %d-token budget, and the Models API says this model does not take one", budget)})
		}
		if adaptive.JSON.Supported.Valid() && adaptive.Supported {
			out = append(out, finding{text: "the Models API says this model thinks adaptively; " +
				"this build's table shapes it with a budget, which it still accepts"})
		}
	}

	// Effort: a level the entry can SEND is one at or below its own, since a
	// call only ever lowers it; a level above it is never on the wire.
	if caps.JSON.Effort.Valid() {
		for _, level := range claudemodel.Efforts {
			api, reported := effortSupport(caps.Effort, level)
			if !reported {
				continue
			}
			inTable := profile.Takes(level)
			sendable := inTable && effort != "" && effort.AtMost(llm.Effort(level)) == llm.Effort(level)
			switch {
			case sendable && !api:
				out = append(out, finding{breaks: true, text: fmt.Sprintf("this entry sends effort %q "+
					"(its own level or a call's lower one), and the Models API says this model "+
					"does not take it", level)})
			case inTable && !api:
				out = append(out, finding{text: fmt.Sprintf("the table lists effort %q, which the "+
					"Models API says this model does not take; this entry never sends it at %q", level, effort)})
			case !inTable && api:
				out = append(out, finding{text: fmt.Sprintf("the Models API offers effort %q, "+
					"which this build's table does not", level)})
			}
		}
	}

	// The output ceiling: every call that thinks sends the table's own.
	if info.JSON.MaxTokens.Valid() && info.MaxTokens > 0 {
		switch ceiling := int64(profile.MaxOutput); {
		case ceiling > info.MaxTokens:
			out = append(out, finding{breaks: true, text: fmt.Sprintf("calls send max_tokens %d, "+
				"and the Models API says this model's ceiling is %d", ceiling, info.MaxTokens)})
		case ceiling < info.MaxTokens:
			out = append(out, finding{text: fmt.Sprintf("the Models API allows max_tokens %d; "+
				"this build's table caps the model at %d", info.MaxTokens, ceiling)})
		}
	}
	return out
}

// effortSupport is whether the record says the model takes level, and whether
// it says anything about it at all. A record whose effort capability is
// reported unsupported takes no level, whatever the per-level leaves say.
func effortSupport(e sdk.EffortCapability, level claudemodel.Effort) (supported, reported bool) {
	var leaf sdk.CapabilitySupport
	switch level {
	case claudemodel.EffortLow:
		leaf = e.Low
	case claudemodel.EffortMedium:
		leaf = e.Medium
	case claudemodel.EffortHigh:
		leaf = e.High
	case claudemodel.EffortXHigh:
		leaf = e.Xhigh
	case claudemodel.EffortMax:
		leaf = e.Max
	}
	if e.JSON.Supported.Valid() && !e.Supported {
		return false, true
	}
	if !leaf.JSON.Supported.Valid() {
		return false, false
	}
	return leaf.Supported, true
}

// driftRemedy is what an operator does about a problem drift, which depends on
// where the profile came from.
func (p *Provider) driftRemedy() string {
	if p.profile.ID == "" {
		return fmt.Sprintf("the capability table does not know %q and shapes it as the current "+
			"generation; if it is an alias for an older Claude model, name that model with "+
			"claude_model", p.model)
	}
	if _, known := claudemodel.Lookup(p.model); !known {
		return fmt.Sprintf("claude_model names %s; check that it is the model %q serves",
			p.profile.ID, p.model)
	}
	return fmt.Sprintf("this build's capability table is out of date for %s, and every call "+
		"on this entry will be refused; move it to a model the table describes, or run a "+
		"Crewlet release whose table matches the API", p.profile.ID)
}

// profileLine names the table row and how it was chosen.
func (p *Provider) profileLine() string {
	switch _, known := claudemodel.Lookup(p.model); {
	case p.profile.ID == "":
		return "none — not in the capability table, so shaped as the current generation " +
			"(set claude_model if this is an alias for an older model)"
	case !known:
		return p.profile.ID + " (claude_model)"
	default:
		return p.profile.ID
	}
}

// keysLine is the credential bag, by hint — never a key.
func (p *Provider) keysLine() string {
	stats := p.pool.Stats()
	if len(stats) == 1 && stats[0].Hint == "" {
		return "none — every call goes out without a key; set api_keys, or ANTHROPIC_API_KEY " +
			"(a gateway that needs none will still answer)"
	}
	hints := make([]string, len(stats))
	for i, s := range stats {
		hints[i] = s.Hint
	}
	return fmt.Sprintf("%d (%s)", len(stats), strings.Join(hints, ", "))
}

// shapeLine is what a phase's request carries on this entry, from the same
// fields [Provider.params] reads.
func (p *Provider) shapeLine() string {
	var parts []string
	switch {
	case p.profile.Thinking == claudemodel.ThinkingAdaptive:
		parts = append(parts, "thinking adaptive (summarized)")
	case p.budget > 0:
		parts = append(parts, fmt.Sprintf("thinking budget %d", p.budget))
	default:
		parts = append(parts, "no thinking (no reasoning_budget_tokens)")
	}
	if p.effort != "" {
		parts = append(parts, "effort "+string(p.effort))
	} else {
		parts = append(parts, "no effort (the model takes none)")
	}
	parts = append(parts, fmt.Sprintf("max_tokens %d", p.profile.MaxOutput))
	if !p.profile.Sampling {
		parts = append(parts, "never a temperature")
	}
	if p.profile.PrefixBinding {
		// Not a field, but what the conversation the request carries
		// looks like after an activate_tool: said here so an operator
		// reading a phase that reasoned less after one has the reason.
		parts = append(parts, "reasoning before a tool change shed")
	}
	return strings.Join(parts, ", ")
}

// smokeTest sends one production-shaped round and certifies the call.
func (p *Provider) smokeTest(ctx context.Context) string {
	comp, err := p.Complete(ctx, smokeRequest())
	if err != nil {
		return "failed — " + err.Error()
	}
	for _, call := range comp.ToolCalls {
		if call.Name != smokeTool {
			continue
		}
		how := "streamed"
		if p.noStream.Load() {
			how = "the endpoint does not stream, so phases get each round whole"
		}
		return fmt.Sprintf("ok — called %s (%s), %d in / %d out",
			smokeTool, how, comp.InputTokens, comp.OutputTokens)
	}
	said := strings.TrimSpace(comp.Content)
	if said == "" {
		return fmt.Sprintf(
			"failed — the round ended (stop reason %q) with no call to %s and no text, "+
				"so every phase that finishes by a call on this entry will spend corrective "+
				"rounds asking again", comp.StopReason, smokeTool)
	}
	return fmt.Sprintf(
		"failed — the model answered in prose instead of calling %s (stop reason %q), so "+
			"every phase that finishes by a call on this entry will spend corrective rounds "+
			"asking again. It said: %q", smokeTool, comp.StopReason, textcut.Ellipsis(said, 200))
}

// smokeRequest is the round [Provider.smokeTest] sends: what a phase sends,
// with the effort lowered to the least a one-call answer needs.
//
// STREAMED, because the phases that finish by a call are, and the streamed
// path is the one with its own failure modes — the idle bound, an endpoint
// that answers it unary.
func smokeRequest() llm.Request {
	return llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: smokePrompt}},
		Tools: []llm.ToolDef{{
			Name:        smokeTool,
			Description: "Confirm the tool channel works.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{"ok": map[string]any{"type": "boolean"}},
				"required":   []any{"ok"},
			},
		}},
		Effort:  llm.EffortLow,
		OnDelta: func(llm.Delta) {},
	}
}

// Render writes the report in the fixed-width form the docs show, the same
// form as a cli-agent entry's.
func (d Diagnosis) Render(w io.Writer) {
	line := func(label, value string) {
		fmt.Fprintf(w, "%-14s: %s\n", label, value)
	}
	line("provider", d.Provider)
	line("type", providerName)
	line("model", d.Model)
	line("profile", d.Profile)
	line("endpoint", d.Endpoint)
	line("keys", d.Keys)
	line("request", d.Shape)
	line("models api", d.ModelsAPI)
	for i, entry := range d.Drift {
		label := ""
		if i == 0 {
			label = "drift"
		}
		line(label, entry)
	}
	line("smoke test", d.Smoke)
	if d.Healthy() {
		line("problems", "none")
		return
	}
	fmt.Fprintln(w, "problems:")
	for _, problem := range d.Problems {
		fmt.Fprintf(w, "  - %s\n", problem)
	}
}
