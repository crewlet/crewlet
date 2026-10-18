package cliagent

import (
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
)

// A SENTENCE THE MODEL WROTE MUST NOT BENCH THE SEAT'S CREDENTIAL.
//
// Markers are matched against the model's own answer, and they have to be:
// Claude Code reports a spent plan on a ZERO exit with the vendor's sentence
// standing where the answer should be, so a marker confined to stderr would
// never fire and the fallback chain would never carry the seat onto a metered
// key. The cost is that a seat ASKED about rate limits answers in prose that
// trips a generic sentinel — a KindRateLimit on a perfectly good credential.
//
// A CLI whose failures only ever reach stderr says so, and then no sentence
// the model writes classifies anything.
func TestOnlyAVendorThatFailsIntoItsAnswerHasItSearched(t *testing.T) {
	t.Parallel()
	// What a seat asked about its own limits might legitimately say.
	const innocent = "Our plan's quota resets hourly. If you hit the rate limit " +
		"or see a usage limit message, the turn falls back to the metered key."

	for name, tc := range map[string]struct {
		scope     cliprofile.MarkerScope
		wantFired bool
		why       string
	}{
		"the default searches the answer": {
			scope: cliprofile.MarkerScopeAnswerAndStderr, wantFired: true,
			why: "a CLI that reports a spent plan AS its answer needs this, and " +
				"this is the false-positive it costs",
		},
		"stderr-scoped ignores the answer": {
			scope: cliprofile.MarkerScopeStderr, wantFired: false,
			why: "a CLI that reports failures only on stderr must not classify " +
				"on anything the model said",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := cliprofile.Profile{
				MarkerScope:  tc.scope,
				LimitMarkers: []cliprofile.LimitMarker{{Sentinel: "rate limit"}, {Sentinel: "quota"}},
			}
			_, fired := classifyMarkers(p, innocent, innocent, "")
			if fired != tc.wantFired {
				t.Errorf("fired = %v, want %v — %s", fired, tc.wantFired, tc.why)
			}
		})
	}

	// And a stderr-scoped profile still classifies a REAL failure, which is
	// the half that makes the narrowing safe rather than merely quiet.
	p := cliprofile.Profile{
		MarkerScope:  cliprofile.MarkerScopeStderr,
		LimitMarkers: []cliprofile.LimitMarker{{Sentinel: "provider.rate_limit"}},
		AuthMarkers:  []cliprofile.AuthMarker{{Sentinel: "provider.auth_error"}},
	}
	for _, tc := range []struct {
		stderr string
		want   llm.ErrorKind
	}{
		{"error: failed to run prompt: provider.rate_limit: 429 …", llm.KindRateLimit},
		{"error: failed to run prompt: provider.auth_error: 401 …", llm.KindAuth},
	} {
		hit, ok := classifyMarkers(p, "", "", tc.stderr)
		if !ok || hit.Kind != tc.want {
			t.Errorf("stderr %q: fired=%v kind=%v, want %v", tc.stderr, ok, hit.Kind, tc.want)
		}
	}
}

// THE THREE PROFILES THAT NARROW IT ARE THE THREE THAT MEASURED IT, and the
// ones that do NOT narrow it must keep the default: claude-code's spent plan
// arrives on a zero exit with the vendor's sentence as the answer, so
// confining its markers to stderr would silently stop the fallback chain from
// ever firing on a spent Claude plan.
func TestTheDefaultMarkerScopeSurvivesForTheVendorsThatNeedIt(t *testing.T) {
	t.Parallel()
	narrowed := map[string]cliprofile.MarkerScope{
		"kimi-code": cliprofile.MarkerScopeStderr,
		"hermes":    cliprofile.MarkerScopeStderr,
		"pi":        cliprofile.MarkerScopeStderr,
		// The other direction: OpenCode 2.0 reports a failure as an event of
		// its own on stdout, which may follow an answer it located.
		"opencode": cliprofile.MarkerScopeStdoutAndStderr,
	}
	for _, name := range cliprofile.BuiltinNames() {
		p, _ := cliprofile.Builtin(name)
		got := p.EffectiveMarkerScope()
		want := cliprofile.MarkerScopeAnswerAndStderr
		if scope, ok := narrowed[name]; ok {
			want = scope
		}
		if got != want {
			t.Errorf("%s: marker_scope = %q, want %q", name, got, want)
		}
	}
}

// A FAILURE EVENT IS FOUND WHEREVER IT FALLS IN THE STREAM, AND THE MODEL
// CANNOT FORGE ONE.
//
// OpenCode 2.0 prints a refused key or a spent plan as an `error` event on
// stdout — nothing on stderr — and the event can follow an answer the profile
// located, so neither the answer nor stderr holds it. The stdout-and-stderr
// scope searches every line; what keeps that from benching a credential over
// something the model wrote is that the sentinels are spelled with the
// event's own quotes, which a model's text only ever reaches the stream as
// escaped JSON. The error events are the ones 2.0.32 printed, verbatim, for a
// provider answering 429 with a quota body, 429 with a rate-limit body and
// 401.
func TestAStreamFailureIsFoundAndCannotBeSpokenByTheModel(t *testing.T) {
	t.Parallel()
	p, ok := cliprofile.Builtin("opencode")
	if !ok {
		t.Fatal("no built-in opencode profile")
	}
	text := func(said string) string {
		return `{"type":"text","sessionID":"ses_x","part":{"type":"text","text":` +
			strconv.Quote(said) + `}}`
	}
	answer := text("I fetched the page; the summary follows.")
	for name, tc := range map[string]struct {
		event string
		want  llm.ErrorKind
	}{
		"a spent plan": {
			event: `{"type":"error","sessionID":"ses_x","error":{"type":"provider.quota","message":"You exceeded your current quota, please check your plan and billing details.","status":429}}`,
			want:  llm.KindRateLimit,
		},
		"a rate limit": {
			event: `{"type":"error","sessionID":"ses_x","error":{"type":"provider.rate-limit","message":"Rate limit reached for requests","status":429}}`,
			want:  llm.KindRateLimit,
		},
		"a refused key": {
			event: `{"type":"error","sessionID":"ses_x","error":{"type":"provider.auth","message":"Incorrect API key provided: sk-...xyz.","status":401}}`,
			want:  llm.KindAuth,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stdout := answer + "\n" + tc.event + "\n"
			out := extract(p, stdout)
			if !out.located {
				t.Fatalf("the answer before the failure was not located — the case " +
					"this scope exists for is the one where it is")
			}
			hit, fired := classifyMarkers(p, nonEmpty(out.text, stdout), stdout, "")
			if !fired || hit.Kind != tc.want {
				t.Errorf("fired=%v kind=%v, want %v — the failure followed a located "+
					"answer, so only a search of the whole stream finds it", fired, hit.Kind, tc.want)
			}
		})
	}

	// The model writing every sentinel verbatim, quotes and all.
	var spoken []string
	for _, m := range p.LimitMarkers {
		spoken = append(spoken, m.Sentinel)
	}
	for _, m := range p.AuthMarkers {
		spoken = append(spoken, m.Sentinel)
	}
	stdout := text("A failure looks like "+strings.Join(spoken, " or ")+" in the stream.") + "\n"
	if hit, fired := classifyMarkers(p, extract(p, stdout).text, stdout, ""); fired {
		t.Errorf("the model's own words classified as %v: %q — a sentinel must be "+
			"spelled so that only the CLI's own event can carry it", hit.Kind, hit.Said)
	}
}

// The stdout-and-stderr scope is refused on a TEXT profile, whose stdout is the
// model's reply: there it would match sentinels against what the model wrote,
// which is the one thing the scope's quoting argument rules out.
func TestTheStreamScopeIsRefusedOnATextProfile(t *testing.T) {
	t.Parallel()
	_, err := cliprofile.Load("opencode", map[string]any{"output": "text"})
	if err == nil || !strings.Contains(err.Error(), "marker_scope") {
		t.Fatalf("err = %v, want a refusal naming marker_scope", err)
	}
}

// An unknown scope is refused rather than silently read as the default: an
// operator who narrowed it meant the narrowing, and a typo that widened the
// haystack back out is exactly the silent failure this field exists to close.
func TestAnUnknownMarkerScopeIsRefused(t *testing.T) {
	t.Parallel()
	_, err := cliprofile.Load("claude-code", map[string]any{"marker_scope": "stdout"})
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
	_, err := cliprofile.Load("kimi-code", map[string]any{
		"system_prompt_args": []any{"--agent-file", "{file}", "--system-prompt", "{system}"},
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
			// kimi-code ships a system_prompt_file, which is refused
			// without a file channel — a different rule, tested
			// elsewhere. Use a profile that ships neither.
			base = "grok"
		}
		if _, err := cliprofile.Load(base, map[string]any{"system_prompt_args": override}); err != nil {
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
			_, err := cliprofile.Load("hermes", map[string]any{"usage": usage})
			if err == nil || !strings.Contains(err.Error(), "usage.input and usage.output") {
				t.Fatalf("err = %v, want a refusal naming both counts", err)
			}
		})
	}
	// The shipped profile declares both, so the doctor's claim is true.
	p, _ := cliprofile.Builtin("hermes")
	if !p.ReadsUsage() {
		t.Error("hermes stopped reporting real usage")
	}
}
