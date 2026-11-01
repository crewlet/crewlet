package harness

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// A SENTENCE THE MODEL WROTE MUST NOT BENCH THE SEAT'S CREDENTIAL.
//
// What a harness says on stdout is searched in the default scope, and it has to be:
// Claude Code reports a spent plan on a ZERO exit with the vendor's sentence
// standing where the answer should be, so a marker confined to stderr would
// never fire and the entry would never sit out. But on a healthy run what it
// says is the MODEL's words, so there a sentinel counts only where it begins
// the first line — the answer IS the vendor's report — and anywhere only in a
// run the harness itself reported failed. A seat ASKED about rate limits answers
// in prose that mentions one, and that is an answer.
//
// A harness whose failures only ever reach stderr says so, and then no sentence
// the model writes classifies anything, failed run or not.
//
// Mutation: match the answer anywhere on a clean run, and the innocent answer
// classifies.
func TestOnlyAVendorsOwnReportInTheAnswerClassifies(t *testing.T) {
	t.Parallel()
	// What a seat asked about its own limits might legitimately say.
	const innocent = "Our plan's quota resets hourly. If you hit the rate limit " +
		"or see a usage limit message, the turn falls back to the metered key."
	// What a vendor's own report looks like on a clean exit: the sentinel
	// opens what was said, after any blank lines.
	const report = "\n  rate limit exceeded for this account · try again later\nDetails follow."

	for name, tc := range map[string]struct {
		scope      harnessprofile.MarkerScope
		said       string
		failed     bool
		wantSource markerSource // "" when nothing may fire
		why        string
	}{
		"the default does not take the model's prose for a report": {
			scope: harnessprofile.MarkerScopeAnswerAndStderr, said: innocent,
			why: "a healthy answer that mentions a limit is an answer",
		},
		"the default takes an answer that opens with the sentinel as the report": {
			scope: harnessprofile.MarkerScopeAnswerAndStderr, said: report, wantSource: sourceReport,
			why: "a harness that reports a spent plan AS its answer needs this",
		},
		"the default reads a failed run's words anywhere": {
			scope: harnessprofile.MarkerScopeAnswerAndStderr, said: innocent, failed: true,
			wantSource: sourceFailedRun,
			why:        "the harness said the run failed, so what it printed is its account of why",
		},
		"stderr-scoped ignores the answer": {
			scope: harnessprofile.MarkerScopeStderr, said: report, failed: true,
			why: "a harness that reports failures only on stderr must not classify " +
				"on anything the model said",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := harnessprofile.Profile{
				MarkerScope:  tc.scope,
				LimitMarkers: []harnessprofile.LimitMarker{{Sentinel: "rate limit"}, {Sentinel: "quota"}},
			}
			hit, fired := classifyMarkers(p, harnessOutput{said: tc.said, stdout: tc.said, failed: tc.failed})
			if fired != (tc.wantSource != "") || hit.Source != tc.wantSource {
				t.Errorf("fired = %v from %q, want from %q — %s", fired, hit.Source, tc.wantSource, tc.why)
			}
		})
	}

	// And a stderr-scoped profile still classifies a REAL failure, which is
	// the half that makes the narrowing safe rather than merely quiet.
	p := harnessprofile.Profile{
		MarkerScope:  harnessprofile.MarkerScopeStderr,
		LimitMarkers: []harnessprofile.LimitMarker{{Sentinel: "provider.rate_limit"}},
		AuthMarkers:  []harnessprofile.AuthMarker{{Sentinel: "provider.auth_error"}},
	}
	for _, tc := range []struct {
		stderr string
		want   llm.ErrorKind
	}{
		{"error: failed to run prompt: provider.rate_limit: 429 …", llm.KindRateLimit},
		{"error: failed to run prompt: provider.auth_error: 401 …", llm.KindAuth},
	} {
		hit, ok := classifyMarkers(p, harnessOutput{stderr: tc.stderr, failed: true})
		if !ok || hit.Kind != tc.want || hit.Source != sourceStderr {
			t.Errorf("stderr %q: fired=%v kind=%v, want %v", tc.stderr, ok, hit.Kind, tc.want)
		}
	}
}

// THE PROFILES THAT NARROW IT ARE THE ONES THAT MEASURED IT, and the ones that
// do NOT narrow it must keep the default: claude-code's spent plan arrives on a
// zero exit with the vendor's sentence as the answer, and hermes prints a
// refused key or a spent plan on STDOUT with exit 2, so confining either's
// markers to stderr would silently stop the fallback chain from ever firing.
func TestTheDefaultMarkerScopeSurvivesForTheVendorsThatNeedIt(t *testing.T) {
	t.Parallel()
	narrowed := map[string]harnessprofile.MarkerScope{
		"kimi-code": harnessprofile.MarkerScopeStderr,
		"pi":        harnessprofile.MarkerScopeStderr,
		// The other direction: OpenCode 2.0 reports a failure as an event of
		// its own on stdout, which may follow an answer it located.
		"opencode": harnessprofile.MarkerScopeEvents,
	}
	for _, name := range harnessprofile.BuiltinNames() {
		p, _ := harnessprofile.Builtin(name)
		got := p.EffectiveMarkerScope()
		want := harnessprofile.MarkerScopeAnswerAndStderr
		if scope, ok := narrowed[name]; ok {
			want = scope
		}
		if got != want {
			t.Errorf("%s: marker_scope = %q, want %q", name, got, want)
		}
	}
}

// THE HERMES SENTINELS NAME WHAT HERMES PRINTS, and where it prints it: what
// 0.21.5 printed for each failure, through `-z` under the profile's own argv,
// against a stub answering 401, 429 with a quota body and 429 with a
// rate-limit body — each on STDOUT with exit 2 and nothing on stderr — and for
// a start with no credential, which never reaches a model and is the one
// failure written to stderr, with exit 1. Each must classify, and an answer
// that merely quotes one must not.
//
// Mutation: scope the profile's markers to stderr, or go back to the generic
// sentinels, and a case here is red.
func TestTheHermesSentinelsNameWhatHermesPrints(t *testing.T) {
	t.Parallel()
	p, ok := harnessprofile.Builtin("hermes")
	if !ok {
		t.Fatal("no built-in hermes profile")
	}
	for name, tc := range map[string]struct {
		stdout, stderr string
		exit           int
		want           llm.ErrorKind
		fires          bool
	}{
		"a refused key": {
			stdout: "custom rejected your API key, so the model can't be reached. Update it in " +
				"Settings → Providers, or run `hermes setup` in a terminal.\n\n" +
				"Provider said: HTTP 401: Incorrect API key provided: sk-test.\n",
			exit: 2, want: llm.KindAuth, fires: true,
		},
		"a spent account": {
			stdout: "Billing or credits exhausted: HTTP 429: You exceeded your current quota, please " +
				"check your plan and billing details.\n\nCustom reported that billing, credits, or " +
				"account entitlement is exhausted for stub-model.\nAdd credits or update billing with " +
				"that provider, then retry.\nYou can switch providers temporarily with /model <model> " +
				"--provider <provider>.\n",
			exit: 2, want: llm.KindRateLimit, fires: true,
		},
		"a rate limit its retries could not wait out": {
			stdout: "custom rate-limited every one of 3 attempts — it looks temporarily unavailable. " +
				"Wait a minute and send /retry, or switch models with /model. To avoid this in future, " +
				"add a backup provider with `hermes fallback add`.\n",
			exit: 2, want: llm.KindRateLimit, fires: true,
		},
		"no credential at all": {
			stderr: "hermes -z: agent failed: Hermes is not connected to any AI provider yet. Run " +
				"`hermes model` to pick one (the free Nous tier needs no API key), type `/login` in " +
				"chat, or add a key with `hermes auth add <provider>`. (Advanced: put an API key such " +
				"as OPENROUTER_API_KEY in ~/.hermes/.env.)\n",
			exit: 1, want: llm.KindAuth, fires: true,
		},
		"a provider named with no credential": {
			stderr: "hermes -z: agent failed: provider 'custom' resolved without credentials (no " +
				"endpoint or API key configured). If this is a named custom provider, use its real " +
				"name (see providers: in config.yaml).\n",
			exit: 1, want: llm.KindAuth, fires: true,
		},
		"an answer that quotes one": {
			stdout: "If the provider answers with \"Billing or credits exhausted\", the seat falls " +
				"back to its next model.\n",
		},
	} {
		hit, fired := classifyMarkers(p, harnessOutput{
			said: tc.stdout, stdout: tc.stdout, stderr: tc.stderr, failed: tc.exit != 0,
		})
		if fired != tc.fires || (fired && hit.Kind != tc.want) {
			t.Errorf("%s: fired = %v as %v, want fired = %v as %v", name, fired, hit.Kind, tc.fires, tc.want)
		}
	}
}

// The three failure events OpenCode 2.0.32 printed, verbatim, under the
// profile's own argv and environment, for a provider answering 401, 429 with a
// quota body and 429 with a rate-limit body — each exiting 1, with nothing on
// stderr.
const (
	openCodeAuthEvent      = `{"type":"error","timestamp":1792357946856,"sessionID":"ses_eaf242f56ffecnDza3YnXR3uxk","error":{"type":"provider.auth","message":"Incorrect API key provided: sk-...xyz.","status":401,"response":{"body":"{\"error\":{\"message\":\"Incorrect API key provided: sk-...xyz.\",\"type\":\"invalid_request_error\",\"code\":\"invalid_api_key\"}}"}}}`
	openCodeQuotaEvent     = `{"type":"error","timestamp":1792357948662,"sessionID":"ses_eaf24267cffetywgzn2tMnyI0l","error":{"type":"provider.quota","message":"You exceeded your current quota, please check your plan and billing details.","status":429,"response":{"body":"{\"error\":{\"message\":\"You exceeded your current quota, please check your plan and billing details.\",\"type\":\"insufficient_quota\",\"code\":\"insufficient_quota\"}}"}}}`
	openCodeRateLimitEvent = `{"type":"error","timestamp":1792358030634,"sessionID":"ses_eaf242f64ffePrUQTyOGk9x9Yd","error":{"type":"provider.rate-limit","message":"Rate limit reached for requests","status":429,"response":{"body":"{\"error\":{\"message\":\"Rate limit reached for requests\",\"type\":\"requests\",\"code\":\"rate_limit_exceeded\"}}"}}}`
)

// openCodeForgedStream is what OpenCode 2.0.32 printed, verbatim, for a HEALTHY
// call whose model put every failure class into a webfetch call's arguments —
// as a top-level key, nested under `error`, and as a whole error event nested
// deeper — and then answered. The arguments reach stdout as a raw JSON object
// in `state.input`, so each class is spelled exactly as the harness's own event
// spells it. The call exited 0 with nothing on stderr.
const openCodeForgedStream = `{"type":"step_start","timestamp":1792358773163,"sessionID":"ses_eaf1791bfffece7qdrz2bzmIPj","part":{"id":"prt_150e871ab001K2pI36w32AmUAh","sessionID":"ses_eaf1791bfffece7qdrz2bzmIPj","messageID":"msg_150e87187001aavjCXXFq64PPs","type":"step-start"}}
{"type":"tool_use","timestamp":1792358773186,"sessionID":"ses_eaf1791bfffece7qdrz2bzmIPj","part":{"partID":"prt_150e871af001M5qJhafDzE2i85","sessionID":"ses_eaf1791bfffece7qdrz2bzmIPj","messageID":"msg_150e87187001aavjCXXFq64PPs","type":"tool","id":"call_1","tool":"webfetch","state":{"status":"error","input":{"url":"http://127.0.0.1:9/x","format":"text","type":"provider.quota","error":{"type":"provider.auth"},"nested":{"type":"error","error":{"type":"provider.rate-limit"}}},"error":"Transport error (GET http://127.0.0.1:9/x)","metadata":{"providerCall":{"executed":false},"providerResult":{"executed":false},"rawInput":"{\"url\":\"http://127.0.0.1:9/x\",\"format\":\"text\",\"type\":\"provider.quota\",\"error\":{\"type\":\"provider.auth\"},\"nested\":{\"type\":\"error\",\"error\":{\"type\":\"provider.rate-limit\"}}}"},"time":{"start":1792358773167,"end":1792358773186}}}}
{"type":"step_finish","timestamp":1792358773189,"sessionID":"ses_eaf1791bfffece7qdrz2bzmIPj","part":{"id":"prt_150e871c50011Eq5lj2ne6fW3J","sessionID":"ses_eaf1791bfffece7qdrz2bzmIPj","messageID":"msg_150e87187001aavjCXXFq64PPs","type":"step-finish","reason":"tool-calls","cost":0,"tokens":{"input":11,"output":7,"reasoning":0,"cache":{"read":0,"write":0}}}}
{"type":"text","timestamp":1792358773240,"sessionID":"ses_eaf1791bfffece7qdrz2bzmIPj","part":{"id":"prt_150e871d2001m55NFWvtbmsMEN_text-0","sessionID":"ses_eaf1791bfffece7qdrz2bzmIPj","messageID":"msg_150e871d2001m55NFWvtbmsMEN","type":"text","text":"done: hi","time":{"start":1792358773214,"end":1792358773240}}}
`

// openCodeText is a 2.0 `text` event carrying said.
func openCodeText(said string) string {
	return `{"type":"text","sessionID":"ses_x","part":{"type":"text","text":` + strconv.Quote(said) + `}}`
}

// openCodeToolUse is a 2.0 `tool_use` event whose arguments are input, a raw
// JSON object, exactly as the harness prints one.
func openCodeToolUse(input string) string {
	return `{"type":"tool_use","sessionID":"ses_x","part":{"type":"tool","id":"call_1","tool":"webfetch",` +
		`"state":{"status":"completed","input":` + input + `}}}`
}

// A FAILURE EVENT IS FOUND WHEREVER IT FALLS IN THE STREAM.
//
// OpenCode 2.0 prints a refused key or a spent plan as an `error` event on
// stdout — nothing on stderr — and the event can follow an answer the profile
// located, so neither the answer nor stderr holds it. The events scope reads
// the harness's own failure events and compares the class in each with the
// profile's sentinels.
func TestAStreamFailureIsFoundAfterALocatedAnswer(t *testing.T) {
	t.Parallel()
	p, ok := harnessprofile.Builtin("opencode")
	if !ok {
		t.Fatal("no built-in opencode profile")
	}
	answer := openCodeText("I fetched the page; the summary follows.")
	for name, tc := range map[string]struct {
		event string
		want  llm.ErrorKind
	}{
		"a spent plan":  {openCodeQuotaEvent, llm.KindRateLimit},
		"a rate limit":  {openCodeRateLimitEvent, llm.KindRateLimit},
		"a refused key": {openCodeAuthEvent, llm.KindAuth},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stdout := answer + "\n" + tc.event + "\n"
			out := extract(p, stdout)
			if !out.located {
				t.Fatalf("the answer before the failure was not located — the case " +
					"this scope exists for is the one where it is")
			}
			hit, fired := classifyMarkers(p, harnessOutput{said: nonEmpty(out.text, stdout), stdout: stdout})
			if !fired || hit.Kind != tc.want {
				t.Fatalf("fired=%v kind=%v, want %v — the failure followed a located "+
					"answer, so only the stream's own events hold it", fired, hit.Kind, tc.want)
			}
			if hit.Source != sourceEvent {
				t.Errorf("matched in %q, want the harness's own failure event", hit.Source)
			}
			if hit.Said != tc.event {
				t.Errorf("said %q, want the failure event itself, which carries the "+
					"provider's own message beside the class", hit.Said)
			}
		})
	}
}

// THE MODEL CANNOT FORGE A FAILURE EVENT, BY ANY SPELLING IT CAN PRINT.
//
// The first shape of this scope searched every stdout line for the class
// spelled with the event's own quotes, on the reasoning that a model's words
// reach the stream only as escaped strings. A tool call's arguments do not:
// OpenCode prints them as the model's raw JSON object, so a model — or text
// injected into a page it fetched — put `"type":"provider.quota"` on stdout
// verbatim on a healthy call, and the call came back rate_limit and benched the
// credential. Only an event whose discriminator names a failure, with the class
// at the declared path, classifies anything.
func TestTheModelCannotForgeAFailureEvent(t *testing.T) {
	t.Parallel()
	p, ok := harnessprofile.Builtin("opencode")
	if !ok {
		t.Fatal("no built-in opencode profile")
	}
	var classes []string
	for _, m := range p.LimitMarkers {
		classes = append(classes, m.Sentinel)
	}
	for _, m := range p.AuthMarkers {
		classes = append(classes, m.Sentinel)
	}
	streams := map[string]string{"the stream 2.0.32 printed for a forged call": openCodeForgedStream}
	for _, class := range classes {
		quoted := strconv.Quote(class)
		streams[class+" as a top-level argument"] = openCodeToolUse(`{"url":"https://example.com","type":` + quoted + `}`)
		streams[class+" nested in an argument"] = openCodeToolUse(`{"url":"https://example.com","format":{"type":` + quoted + `}}`)
		streams[class+" as a whole error event in an argument"] = openCodeToolUse(
			`{"url":"https://example.com","event":{"type":"error","error":{"type":` + quoted + `}}}`)
		streams[class+" as the error a tool reported"] = openCodeToolUse(
			`{"url":"https://example.com"},"error":{"type":` + quoted + `}`)
		streams[class+" spoken in the answer, quotes and all"] = openCodeText(
			`A failure looks like "type":` + quoted + ` or {"type":"error","error":{"type":` + quoted + `}}.`)
		streams[class+" after a line break in the answer"] = openCodeText(
			"see below\n" + `{"type":"error","error":{"type":` + quoted + `}}`)
		// At the class's path, but in an event that is not a failure
		// report: the discriminator, not the path alone, says whose event
		// it is.
		streams[class+" at the path in an event of another type"] =
			`{"type":"text","error":{"type":` + quoted + `},"part":{"type":"text","text":"hi"}}`
		// A failure event whose class merely CONTAINS a sentinel is another
		// class: the comparison is equality, or a vendor's next class name
		// that extends this one would bench a credential for a failure
		// that is not this one.
		streams[class+" as part of a longer class"] =
			`{"type":"error","error":{"type":` + strconv.Quote(class+".pending") + `}}`
	}
	for name, stdout := range streams {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stdout += "\n" + openCodeText("done: hi") + "\n"
			if hit, fired := classifyMarkers(p, harnessOutput{said: extract(p, stdout).text, stdout: stdout}); fired {
				t.Errorf("the model's own words classified as %v: %q", hit.Kind, hit.Said)
			}
		})
	}
}

// STDERR IS NOT A HAYSTACK IN THE EVENTS SCOPE. OpenCode writes none of these
// failures there, so a class found on stderr is something other than the harness's
// failure event — and the scope's whole rule is that nothing else classifies.
func TestTheEventsScopeDoesNotSearchStderr(t *testing.T) {
	t.Parallel()
	p, _ := harnessprofile.Builtin("opencode")
	stdout := openCodeText("done: hi") + "\n"
	if hit, fired := classifyMarkers(p, harnessOutput{said: "done: hi", stdout: stdout,
		stderr: "provider.quota provider.auth " + openCodeQuotaEvent}); fired {
		t.Errorf("stderr classified the call as %v: %q", hit.Kind, hit.Said)
	}
}

// A FAILED RUN WITH TOOLS IS CLASSIFIED BY ITS OWN FAILURE EVENTS. Its answer
// is its record's last prose, which a failed run never wrote, and a harness whose
// stderr says only that it failed says why in a failure event alone (Muse
// Code's run.terminal.failed reason, beside "run ended with Failed"): read
// beside stderr, by the profile's fields, a spent plan there sits the entry
// out. Only a failed run's — a run that went on past a failure event did not
// fail of it.
//
// Mutation: leave the failure events out of the default scope, and the spent
// plan is a server failure; read them on a run that did not fail, and its
// recovered retry benches the entry.
func TestAFailedRunWithToolsIsClassifiedByItsOwnFailureEvents(t *testing.T) {
	t.Parallel()
	p, err := harnessprofile.Load("claude-code", map[string]any{
		"limit_markers": []any{map[string]any{"sentinel": "rate_limit_error"}},
		"auth_markers":  []any{map[string]any{"sentinel": "authentication_error"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	events := "payload.reason=API error 429: slow down (rate_limit_error) (after 4 provider attempts)"
	hit, fired := classifyMarkers(p, harnessOutput{stderr: "run ended with Failed\n", failed: true, events: events})
	if !fired || hit.Kind != llm.KindRateLimit || hit.Source != sourceEvent || hit.Said != events {
		t.Errorf("classified %+v (fired %v), want the spent plan the failure event names", hit, fired)
	}
	if hit, fired := classifyMarkers(p, harnessOutput{said: "done", events: events}); fired {
		t.Errorf("a run that did not fail was classified %v by a failure event it went on past", hit.Kind)
	}
}

// THE PRODUCTION CALL SITE CLASSIFIES A STREAM FAILURE, AND ONLY A STREAM
// FAILURE. Every case above asks classifyMarkers directly; this asks the frame
// that turns a finished OpenCode call into an answer or an error, so a call
// site that handed the located answer over as stdout — which is the shape
// that once classified a spent OpenCode plan as fatal and never moved the
// chain — fails here. Exit 0 throughout: a marker is checked before the exit
// code.
func TestACompletionClassifiesOpenCodesFailureEvents(t *testing.T) {
	t.Parallel()
	p, ok := harnessprofile.Builtin("opencode")
	if !ok {
		t.Fatal("no built-in opencode profile")
	}
	prov := &Provider{profile: p, model: "anthropic/claude-sonnet-5", harness: "opencode", key: "sub", now: time.Now}
	answer := openCodeText("I fetched the page; the summary follows.")
	for name, tc := range map[string]struct {
		event string
		want  llm.ErrorKind
	}{
		"a spent plan":  {openCodeQuotaEvent, llm.KindRateLimit},
		"a rate limit":  {openCodeRateLimitEvent, llm.KindRateLimit},
		"a refused key": {openCodeAuthEvent, llm.KindAuth},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := prov.completion(t.Context(), "prompt",
				&rawResult{stdout: answer + "\n" + tc.event + "\n"}, "")
			var classified *llm.Error
			if !errors.As(err, &classified) || classified.Kind != tc.want {
				t.Fatalf("err = %v, want an %v provider error — the event followed a "+
					"located answer on a zero exit", err, tc.want)
			}
		})
	}
	// And a forged call is an ANSWER, not a benched credential.
	comp, err := prov.completion(t.Context(), "prompt", &rawResult{stdout: openCodeForgedStream}, "")
	if err != nil {
		t.Fatalf("a healthy call whose model wrote every class into a tool's arguments "+
			"failed: %v", err)
	}
	if comp.Content != "done: hi" {
		t.Errorf("Content = %q, want the answer", comp.Content)
	}
}

// An unknown scope is refused rather than silently read as the default: an
// operator who narrowed it meant the narrowing, and a typo that widened the
// haystack back out is exactly the silent failure this field exists to close.
func TestAnUnknownMarkerScopeIsRefused(t *testing.T) {
	t.Parallel()
	_, err := harnessprofile.Load("claude-code", map[string]any{"marker_scope": "stdout"})
	if err == nil || !strings.Contains(err.Error(), "marker_scope") {
		t.Fatalf("err = %v, want a refusal naming marker_scope", err)
	}
}

// BOTH PLACEHOLDERS IN ONE TEMPLATE WROTE THE PROMPT TWICE, and the second
// copy went somewhere every account on the machine can read.
//
// The renderer writes the private file for `{file}` and substitutes
// `{system}` into argv on the SAME pass, so an override naming both produced
// a 0600 file AND /proc/<pid>/cmdline carrying the seat's whole identity. The
// shipped profiles were held to one channel by a test; operator overrides —
// the one place a hand-written argv actually appears — were held to nothing.
func TestAnArgvTemplateNamingBothPlaceholdersIsRefused(t *testing.T) {
	t.Parallel()
	_, err := harnessprofile.Load("kimi-code", map[string]any{
		"system_prompt": map[string]any{
			"args": []any{"--agent-file", "{file}", "--system-prompt", "{system}"},
		},
	})
	if err == nil {
		t.Fatal("an override naming both {file} and {system} was accepted — it writes " +
			"the seat's system prompt to a private file and then puts the same text on argv")
	}
	for _, want := range []string{"{file}", "{system}", "/proc/<pid>/cmdline"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
	// Either alone stays legitimate — this must not become "no overrides".
	for name, override := range map[string][]any{
		"file only":   {"--agent-file", "{file}"},
		"system only": {"--system-prompt", "{system}"},
	} {
		base := "kimi-code"
		if name == "system only" {
			// kimi-code's channel ships a file spec, which is refused
			// without a file to write — a different rule, tested
			// elsewhere. Use a profile whose channel has none.
			base = "grok"
		}
		overrides := map[string]any{"system_prompt": map[string]any{"args": override}}
		if _, err := harnessprofile.Load(base, overrides); err != nil {
			t.Errorf("%s: a single-placeholder override was refused: %v", name, err)
		}
	}
}

// A USAGE-FILE PROFILE MISSING EITHER PROMPT COUNT REPORTS FIGURES IT NEVER
// READS. [extracted.applyUsageReport] takes the report only when input AND
// output resolve — a partial overlay would pair one source's input with
// another's output, and the sum is what a budget is charged. So a profile
// declaring one of them estimates on every call while ReadsUsage, and
// `crewlet llm doctor` with it, says the vendor's own figures are in use.
func TestAUsageFileProfileMustDeclareBothPromptCounts(t *testing.T) {
	t.Parallel()
	for name, usage := range map[string]map[string]any{
		"only input": {"input": []any{[]any{"input_tokens"}}, "output": []any{}},
		"only output": {
			"input": []any{}, "output": []any{[]any{"output_tokens"}},
		},
		"only the cache counts": {
			"input": []any{}, "output": []any{},
			"cache_read": []any{[]any{"cache_read_tokens"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := harnessprofile.Load("hermes", map[string]any{"usage": usage})
			if err == nil || !strings.Contains(err.Error(), "usage.input and usage.output") {
				t.Fatalf("err = %v, want a refusal naming both counts", err)
			}
		})
	}
	// The shipped profile declares both, so the doctor's claim is true.
	p, _ := harnessprofile.Builtin("hermes")
	if !p.ReadsUsage() {
		t.Error("hermes stopped reporting real usage")
	}
}
