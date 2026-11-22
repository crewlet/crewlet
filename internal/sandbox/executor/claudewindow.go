package executor

import (
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// claudeFallbackWindow is the window of a model this build does not know: the
// standard window of every Claude model, so the harness's clamp never lowers it on
// a current model, and room for a run's first request several times over.
const claudeFallbackWindow = 200_000

// claudeLongWindow is the window of every current Claude model and of a model
// argument that opts into the long window (`[1m]`).
const claudeLongWindow = 1_000_000

// claudeStandardWindow is the window Claude Code holds a model to that it does
// not serve its long window: claude-opus-4-6 and claude-sonnet-4-6 unless the
// account is served more or the argument opts in, every older model, and a
// model a release does not know (2.1.322's catalog and its default for a model
// it lacks).
const claudeStandardWindow = 200_000

// claudeReplyReserve is the most of a window Claude Code keeps back for a reply:
// min(maxOutputTokens, 20000) on 2.1.310 and 2.1.322, and every model in the
// table defaults to at least 32,000 output tokens (a lower
// CLAUDE_CODE_MAX_OUTPUT_TOKENS only raises the compaction point). A request
// past the rest cannot be sent whole: the harness compacts first, dropping its
// oldest messages where even the summary's request cannot carry them. The
// launch floor keeps a continued session below it ([sandbox.ExecutorWindow.Carries]);
// the compaction point is lower still ([claudeCompactBuffer]).
const claudeReplyReserve = 20_000

// claudeCompactBuffer is the room Claude Code keeps between its compaction
// point and the rest of its window less [claudeReplyReserve]: it compacts a
// conversation before a request where its own count of it reaches the window
// less the reserve less this (`let r=e-13000` in 2.1.310's and 2.1.322's
// threshold code; a percentage override can only lower it, and the run's
// environment and settings unset and blank that override). Measured: at a
// 200,000-token window the point is 167,000, a count of 165,922 sent whole and
// one of 167,834 compacted, on both releases (docs/concepts/code-sandbox.md).
const claudeCompactBuffer = 13_000

// claudeTurnAttachments is the most, in tokens at [claudeChargeBytes], a resumed run
// adds to its session beside its brief before its first request: the records a
// resume writes that the session's thread does not hold yet, and its turn's
// attachments.
//
// TWENTY THOUSAND, measured on 2.1.310 and 2.1.322: a resume's records beyond
// the archive came to at most 2,597 bytes (1,301 tokens), its turn's
// attachments to 2,232 bytes (1,118 tokens), and a whole first turn's set to
// 39,064 bytes (19,532 tokens). NOT COVERED: a box whose skills changed lists
// them again, up to 40,000 characters at a 1,000,000-token window (about
// 13,333 tokens) or 100,000 where a listing budget is served.
const claudeTurnAttachments = 20_000

// claudeChargeBytes is the most tokens n bytes of a Claude Code session can count
// for, by the harness's own estimate of what lies after its last counted
// reply: ⌈n/2⌉. The harness counts a text at its UTF-16 length over three or
// four (by the model), a JSON-titled text document at its length over two, a
// call at its JSON over three or four, so two bytes a token is at most every
// rate it charges, and a line's bytes are never fewer than its text's UTF-16
// length. A later release that counts text below three a character is still
// covered, which a tighter three bytes a token would not be.
const claudeChargeBytes sandbox.Charge = 2

// claudeCharge is n bytes at [claudeChargeBytes].
func claudeCharge(n int) int64 { return claudeChargeBytes.Tokens(int64(n)) }

// claudeRoute is how a Claude Code run reaches its model, as far as the windows
// it holds a model to differ by it ([claudeCodeWindowOn]): an alias names
// another model on a cloud platform or a gateway than on Anthropic's own API.
type claudeRoute string

const (
	// claudeRouteAnthropic is Anthropic's own API, or an endpoint the harness
	// reaches as it does that API (a base URL of its own and a token): what
	// an entry signed in with a login or a key reaches, and one whose env
	// names no cloud platform.
	claudeRouteAnthropic claudeRoute = "anthropic"
	// claudeRouteBedrock is Amazon Bedrock (CLAUDE_CODE_USE_BEDROCK).
	claudeRouteBedrock claudeRoute = "bedrock"
	// claudeRouteVertex is Google Cloud's Vertex AI (CLAUDE_CODE_USE_VERTEX).
	claudeRouteVertex claudeRoute = "vertex"
	// claudeRouteFoundry is Microsoft Foundry (CLAUDE_CODE_USE_FOUNDRY).
	claudeRouteFoundry claudeRoute = "foundry"
	// claudeRouteElsewhere is a route this build cannot name: a gateway the
	// harness's host signs into, another platform, or an entry whose route is
	// not read. Held to the least window of every route but Anthropic's own,
	// the smaller side.
	claudeRouteElsewhere claudeRoute = "elsewhere"
)

// claudeRoutes are the routes this build names.
var claudeRoutes = []claudeRoute{claudeRouteAnthropic, claudeRouteBedrock, claudeRouteVertex, claudeRouteFoundry,
	claudeRouteElsewhere}

// Valid reports whether this build knows r. [claudeCodeWindowOn] judges a
// route it does not know as [claudeRouteElsewhere].
func (r claudeRoute) Valid() bool { return slices.Contains(claudeRoutes, r) }

// claudeWindows is each model's own window at its largest, the pin, by the
// canonical id or the alias a providers.llm entry hands Claude Code.
var claudeWindows = map[string]int{
	"claude-fable-5-2":  claudeLongWindow,
	"claude-fable-5-1":  claudeLongWindow,
	"claude-fable-5":    claudeLongWindow,
	"claude-mythos-5-2": claudeLongWindow,
	"claude-mythos-5-1": claudeLongWindow,
	"claude-mythos-5":   claudeLongWindow,
	"claude-opus-5-5":   claudeLongWindow,
	"claude-opus-5":     claudeLongWindow,
	"claude-opus-4-8":   claudeLongWindow,
	"claude-opus-4-7":   claudeLongWindow,
	"claude-opus-4-6":   claudeLongWindow,
	"claude-sonnet-5-5": claudeLongWindow,
	"claude-sonnet-5":   claudeLongWindow,
	"claude-sonnet-4-6": claudeLongWindow,
	"claude-haiku-5-5":  claudeLongWindow,
	"claude-opus-4-5":   claudeStandardWindow,
	"claude-sonnet-4-5": claudeStandardWindow,
	"claude-haiku-4-5":  claudeStandardWindow,
	"default":           claudeLongWindow,
	"best":              claudeLongWindow,
	"opus":              claudeLongWindow,
	"opusplan":          claudeLongWindow,
	"sonnet":            claudeLongWindow,
	"haiku":             claudeLongWindow,
	"fable":             claudeLongWindow,
}

// claudeStandardWindows is, by route, the least window
// ([sandbox.ExecutorWindow.Standard]) of each model argument it is below the pin for
// on some route; every other known argument's is its pin. Read in Claude Code
// 2.1.310's and 2.1.322's model catalogs and alias tables:
//
//   - claude-opus-4-6 and claude-sonnet-4-6: 200,000 unless served more or `[1m]`;
//   - `opus` is claude-opus-5-5 but claude-opus-4-6 on Foundry; `best` is `opus`
//     on 2.1.322 and `fable` (long on every route) on 2.1.310;
//   - `sonnet` is claude-sonnet-5-5 on Anthropic's API, claude-sonnet-4-5 or 4-6
//     elsewhere; `haiku` is claude-haiku-4-5 off it; `opusplan` plans on `opus`
//     and works on `sonnet`;
//   - no model or `default` is the account's default, either 4-6 on any route.
var claudeStandardWindows = map[string]map[claudeRoute]int{
	"claude-opus-4-6":   allRoutes(claudeStandardWindow),
	"claude-sonnet-4-6": allRoutes(claudeStandardWindow),
	"default":           allRoutes(claudeStandardWindow),
	"opus":              offFoundry(claudeLongWindow),
	"best":              offFoundry(claudeLongWindow),
	"sonnet":            onAnthropicAlone(claudeLongWindow),
	"haiku":             onAnthropicAlone(claudeLongWindow),
	"opusplan":          onAnthropicAlone(claudeLongWindow),
}

// allRoutes is window on every route.
func allRoutes(window int) map[claudeRoute]int {
	out := map[claudeRoute]int{}
	for _, r := range claudeRoutes {
		out[r] = window
	}
	return out
}

// offFoundry is window on Anthropic's API, Bedrock and Vertex, and the standard
// window on Foundry and on a route this build cannot name.
func offFoundry(window int) map[claudeRoute]int {
	out := allRoutes(window)
	out[claudeRouteFoundry], out[claudeRouteElsewhere] = claudeStandardWindow, claudeStandardWindow
	return out
}

// onAnthropicAlone is window on Anthropic's API and the standard window on
// every other route.
func onAnthropicAlone(window int) map[claudeRoute]int {
	out := allRoutes(claudeStandardWindow)
	out[claudeRouteAnthropic] = window
	return out
}

// claudeModelDecorations are what a platform adds around a model's canonical id:
// a Bedrock region and vendor prefix, a Bedrock version suffix, and a dated
// snapshot written with `-` (Claude API, Bedrock) or `@` (Vertex AI).
var claudeModelDecorations = regexp.MustCompile(
	`^(?:(?:[a-z]{2,6}\.)?anthropic\.)?(.+?)(?:[-@]\d{8})?(?:-v\d+(?::\d+)?)?$`)

// The two rates Claude Code estimates text at, in UTF-16 characters a token
// ([claudeTextRateOf]).
const (
	claudeTextRate      = 3
	claudeOlderTextRate = 4
)

// claudeOlderTextModels is the fourteen canonical model ids Claude Code
// estimates text for at four characters a token, every other model at three
// (2.1.322's K$ @286842571 read by F_ @286842850; 2.1.310's eH and ry, the
// same fourteen).
var claudeOlderTextModels = []string{"claude-3-opus", "claude-3-sonnet", "claude-3-haiku", "claude-3-5-sonnet",
	"claude-3-5-haiku", "claude-3-7-sonnet", "claude-opus-4-0", "claude-opus-4-1", "claude-opus-4-5",
	"claude-opus-4-6", "claude-sonnet-4-0", "claude-sonnet-4-5", "claude-sonnet-4-6", "claude-haiku-4-5"}

// claudeCanonicalModels is how Claude Code names a model id canonically before
// it asks the rate of it (2.1.322's x$ @286839278, read by r_): lowercased, the
// first of these whose pattern the id holds, in this order, so an id holding a
// newer family's name is that family's whatever older name it also holds; an
// id holding none keeps its own name, less a dated suffix, which names none of
// the fourteen. A Bedrock region or vendor prefix, a dated or version suffix
// and a `[1m]` opt-in each leave the family's name in the id. Compiled at first
// use: at init its patterns alone put every push's hook past its bound.
var claudeCanonicalModels = sync.OnceValue(func() []claudeCanonical {
	return []claudeCanonical{
		{regexp.MustCompile(`claude-fable-5-2`), "claude-fable-5-2"},
		{regexp.MustCompile(`claude-fable-5-1`), "claude-fable-5-1"},
		{regexp.MustCompile(`claude-fable-5`), "claude-fable-5"},
		{regexp.MustCompile(`claude-mythos-5-2`), "claude-mythos-5-2"},
		{regexp.MustCompile(`claude-mythos-5-1`), "claude-mythos-5-1"},
		{regexp.MustCompile(`claude-mythos-5`), "claude-mythos-5"},
		{regexp.MustCompile(`claude-opus-5-5`), "claude-opus-5-5"},
		{regexp.MustCompile(`claude-opus-5`), "claude-opus-5"},
		{regexp.MustCompile(`claude-opus-4-8`), "claude-opus-4-8"},
		{regexp.MustCompile(`claude-opus-4-7`), "claude-opus-4-7"},
		{regexp.MustCompile(`claude-opus-4-6`), "claude-opus-4-6"},
		{regexp.MustCompile(`claude-opus-4-5`), "claude-opus-4-5"},
		{regexp.MustCompile(`claude-opus-4-1`), "claude-opus-4-1"},
		// `claude-opus-4` NOT FOLLOWED BY A SINGLE DIGIT'S PART: the harness's
		// lookahead (?!-\d(?!\d)), which Go's regexp has no form of, spelled as
		// what may follow instead: the end, a character that is no `-`, a `-`
		// at the end or that no digit follows, or a `-` and two digits (a dated
		// suffix).
		{regexp.MustCompile(`claude-opus-4(?:$|[^-]|-$|-[^0-9]|-[0-9][0-9])`), "claude-opus-4-0"},
		{regexp.MustCompile(`claude-sonnet-5-5`), "claude-sonnet-5-5"},
		{regexp.MustCompile(`claude-sonnet-5`), "claude-sonnet-5"},
		{regexp.MustCompile(`claude-sonnet-4-6`), "claude-sonnet-4-6"},
		{regexp.MustCompile(`claude-sonnet-4-5`), "claude-sonnet-4-5"},
		{regexp.MustCompile(`claude-sonnet-4(?:$|[^-]|-$|-[^0-9]|-[0-9][0-9])`), "claude-sonnet-4-0"},
		{regexp.MustCompile(`claude-haiku-5-5`), "claude-haiku-5-5"},
		{regexp.MustCompile(`claude-haiku-4-5`), "claude-haiku-4-5"},
		{regexp.MustCompile(`claude-3-7-sonnet`), "claude-3-7-sonnet"},
		{regexp.MustCompile(`claude-3-5-sonnet`), "claude-3-5-sonnet"},
		{regexp.MustCompile(`claude-3-5-haiku`), "claude-3-5-haiku"},
		{regexp.MustCompile(`claude-3-opus`), "claude-3-opus"},
		{regexp.MustCompile(`claude-3-sonnet`), "claude-3-sonnet"},
		{regexp.MustCompile(`claude-3-haiku`), "claude-3-haiku"},
	}
})

// claudeCanonical is one of [claudeCanonicalModels]: a pattern and the canonical
// id an id holding it is named.
type claudeCanonical struct {
	pattern   *regexp.Regexp
	canonical string
}

// claudeTextRateOf is how many UTF-16 characters Claude Code estimates a token of
// text at for model, as the model argument a launch hands it names it: four
// for the fourteen older models it names ([claudeOlderTextModels]), matched
// by the id it canonicalizes model to ([claudeCanonicalModels]), and three for
// every other, the current models among them.
//
// THREE FOR AN ALIAS AND FOR NO MODEL, whatever model they name: an alias
// (`sonnet`, `opus`, `default`) names a model only through the account's
// settings and the member's platform, and the rate a reader takes is the
// larger figure's, three ([sandbox.NextRequest.At]), so a model this build cannot
// place holds a launch to more and never less.
func claudeTextRateOf(model string) int {
	m := strings.ToLower(strings.TrimSpace(model))
	for _, c := range claudeCanonicalModels() {
		if c.pattern.MatchString(m) {
			if slices.Contains(claudeOlderTextModels, c.canonical) {
				return claudeOlderTextRate
			}
			return claudeTextRate
		}
	}
	return claudeTextRate
}

// claudeCodeWindow is the window a Claude Code run on model is pinned to: the
// model's own, or [claudeFallbackWindow] where this build does not know it,
// with its least window on Anthropic's own API ([claudeCodeWindowOn]). An empty
// model is the harness's default model.
func claudeCodeWindow(model string) sandbox.ExecutorWindow {
	return claudeCodeWindowOn(model, claudeRouteAnthropic)
}

// claudeCodeWindowOn is the window a Claude Code run on model, reaching it by
// route, is pinned to ([sandbox.ExecutorWindow.Tokens], the same on every route), and
// the least window the harness holds it to there ([sandbox.ExecutorWindow.Standard]):
// [claudeFallbackWindow] for both where this build does not know the model,
// and a route it does not know judged as [claudeRouteElsewhere]. A model
// argument that opts into the long window (`[1m]`) is held to it on every
// route.
func claudeCodeWindowOn(model string, route claudeRoute) sandbox.ExecutorWindow {
	if !route.Valid() {
		route = claudeRouteElsewhere
	}
	m := strings.ToLower(strings.TrimSpace(model))
	if strings.HasSuffix(m, "[1m]") {
		return claudeWindow(claudeLongWindow, claudeLongWindow, true)
	}
	if m == "" {
		m = "default"
	}
	if parts := claudeModelDecorations.FindStringSubmatch(m); parts != nil {
		m = parts[1]
	}
	tokens, ok := claudeWindows[m]
	if !ok {
		return claudeWindow(claudeFallbackWindow, claudeFallbackWindow, false)
	}
	standard := tokens
	if routes, held := claudeStandardWindows[m]; held {
		standard = routes[route]
	}
	return claudeWindow(tokens, standard, true)
}

// claudeWindow is a Claude Code window of tokens, held to standard, with the
// headroom the harness keeps below it.
func claudeWindow(tokens, standard int, known bool) sandbox.ExecutorWindow {
	return sandbox.ExecutorWindow{Tokens: tokens, Standard: standard, Known: known, Reserve: claudeReplyReserve,
		Buffer: claudeCompactBuffer, Attachments: claudeTurnAttachments, Charge: claudeChargeBytes}
}

// Window is the window a Claude Code run of ask's member is pinned to and held
// to, on the route the member reaches its model by: exactly off its env where
// ask says so ([claudePlatform]), else coarsely by its sign-in ([claudeRouteOf]).
func (ClaudeCode) Window(ask WindowAsk) sandbox.ExecutorWindow {
	route := claudeRouteOf(ask.SignIn)
	if ask.Exact {
		route = claudePlatform(ask.SignIn, ask.Env)
	}
	w := claudeCodeWindowOn(ask.Model, route)
	w.OlderText = claudeTextRateOf(ask.Model) == claudeOlderTextRate
	return w
}

// PinnedWindow is the window a Claude Code run on model is pinned to
// (CLAUDE_CODE_AUTO_COMPACT_WINDOW, [claudeSessionLayout]).
func (ClaudeCode) PinnedWindow(model string) int { return claudeCodeWindow(model).Tokens }

// claudeRouteOf is the route a Claude Code member reaches its model by, as far
// as a relaunch's telling reads it: Anthropic's own API for an entry signed in
// with a login, a key or the host's own environment, each of which hands the
// harness first-party credentials alone (config refuses a sign-in switch in
// such an entry's env); and a route this build cannot name for an entry signed
// in by its own env, and for a mode this build does not know, which is held to
// the least window off Anthropic's API, the side that tells more. The launch
// floor reads the entry's platform exactly ([claudePlatform]), since there a
// smaller window costs a session rather than words.
func claudeRouteOf(signIn SignIn) claudeRoute {
	if signIn == SignInFirstParty {
		return claudeRouteAnthropic
	}
	return claudeRouteElsewhere
}

// claudePlatform is the platform a Claude Code member reaches its model by,
// read exactly off its entry, for the launch floor: Anthropic's API for a
// login, key or host-environment entry ([claudeRouteOf]); for an env-signed
// one, the platform its env switches to, in Claude Code's own order (2.1.322:
// Bedrock, Foundry, unnamed platforms, Vertex, else Anthropic's API), a switch
// being on where Claude Code reads it as true (`1`, `true`, `yes`, `on`, any
// case). An unnamed platform, a gateway, or a switch neither true nor false is
// [claudeRouteElsewhere], the least window. The launch's later run
// environment is not read; a platform switched there is the floor's stated
// residual.
func claudePlatform(signIn SignIn, env map[string]string) claudeRoute {
	if route := claudeRouteOf(signIn); route == claudeRouteAnthropic || signIn != SignInOwnEnv {
		return route
	}
	unsure := false
	on := func(name string) bool {
		switch strings.ToLower(strings.TrimSpace(env[name])) {
		case "1", "true", "yes", "on":
			return true
		case "", "0", "false", "no", "off":
			return false
		}
		unsure = true
		return false
	}
	gateway, bedrock, foundry := on("CLAUDE_CODE_USE_GATEWAY"), on("CLAUDE_CODE_USE_BEDROCK"),
		on("CLAUDE_CODE_USE_FOUNDRY")
	unnamed := on("CLAUDE_CODE_USE_ANTHROPIC_AWS") || on("CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD") ||
		on("CLAUDE_CODE_USE_MANTLE")
	vertex := on("CLAUDE_CODE_USE_VERTEX")
	switch {
	case gateway || unsure:
		return claudeRouteElsewhere
	case bedrock:
		return claudeRouteBedrock
	case foundry:
		return claudeRouteFoundry
	case unnamed:
		return claudeRouteElsewhere
	case vertex:
		return claudeRouteVertex
	}
	return claudeRouteAnthropic
}
