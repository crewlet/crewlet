package pulsar

import (
	"errors"
	"regexp"
	"testing"

	"github.com/crewlet/crewlet/internal/queue/topics"
)

// testCfg is the tenant/namespace pair the naming tests translate through.
// Deliberately not public/default: a translation bug that only shows up under
// a non-default tenant is exactly the bug this backend must not have.
func testCfg() Config { return Config{URL: "pulsar://broker:6650", Tenant: "acme", Namespace: "prod"} }

func TestFullTopicScopesEverySubjectToTheTenant(t *testing.T) {
	t.Parallel()
	cfg := testCfg()
	// The tenant IS the boundary between companies on one estate, so a
	// subject that reached the broker without it would land in whichever
	// namespace happened to be there.
	got := cfg.fullTopic(topics.AgentInbox("alice"))
	want := "persistent://acme/prod/crewlet.agent.alice.inbox"
	if got != want {
		t.Fatalf("fullTopic = %q, want %q", got, want)
	}
	if back := cfg.localSubject(got); back != topics.AgentInbox("alice") {
		t.Fatalf("localSubject(%q) = %q, want %q", got, back, topics.AgentInbox("alice"))
	}
}

func TestLocalSubjectRecoversTheSubjectAStreamHandlerIsGiven(t *testing.T) {
	t.Parallel()
	cfg := testCfg()
	for _, tc := range []struct{ name, full, want string }{
		{"fully qualified", "persistent://acme/prod/crewlet.events.task_created", "crewlet.events.task_created"},
		{"a dead-letter subject", "persistent://acme/prod/" + topics.DeadLetter("t", "g"), topics.DeadLetter("t", "g")},
		// A name this backend did not build is handed back untouched
		// rather than truncated into a different subject: a stream
		// handler routing on a mangled subject is worse than one routing
		// on something it does not recognise.
		{"not a topic name at all", "nonsense", "nonsense"},
	} {
		if got := cfg.localSubject(tc.full); got != tc.want {
			t.Errorf("%s: localSubject(%q) = %q, want %q", tc.name, tc.full, got, tc.want)
		}
	}
}

func TestCheckSubjectRefusesWhatWouldSilentlyMisroute(t *testing.T) {
	t.Parallel()
	// Every case here is a publish that would otherwise succeed and be
	// read by nobody — no dead letter, no producer error, nothing to
	// alert on.
	for _, tc := range []struct {
		name    string
		subject string
		ok      bool
	}{
		{"an ordinary subject", "crewlet.events.task_created", true},
		{"a seat inbox", topics.AgentInbox("alice"), true},
		{"a dead-letter subject", topics.DeadLetter("crewlet.events.x", "grp"), true},
		{"empty", "", false},
		{"an empty middle segment, what an unroutable handle produces", "crewlet.agent..inbox", false},
		{"a leading dot", ".crewlet.events.x", false},
		{"a trailing dot", "crewlet.events.x.", false},
		{"a slash, which names a namespace", "acme/prod/x", false},
		{"whitespace", "crewlet.events. x", false},
		{"a wildcard, which only SubscribeStream takes", "crewlet.events.*", false},
		{"the trailing wildcard", "crewlet.events.>", false},
	} {
		err := checkSubject(tc.subject)
		if tc.ok && err != nil {
			t.Errorf("%s: checkSubject(%q) = %v, want nil", tc.name, tc.subject, err)
		}
		if !tc.ok {
			if err == nil {
				t.Errorf("%s: checkSubject(%q) accepted it", tc.name, tc.subject)
			} else if !errors.Is(err, ErrSubject) {
				t.Errorf("%s: checkSubject(%q) = %v, want an ErrSubject", tc.name, tc.subject, err)
			}
		}
	}
}

func TestPatternRegexMirrorsTheSubjectWildcardGrammar(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, pattern, want string }{
		{"trailing remainder", "crewlet.events.>", `crewlet\.events\..+$`},
		{"one segment", "crewlet.events.*", `crewlet\.events\.[^.]+$`},
		{"no wildcard at all", "crewlet.events.task_created", `crewlet\.events\.task_created$`},
		{"a wildcard in the middle", "crewlet.agent.*.inbox", `crewlet\.agent\.[^.]+\.inbox$`},
		{"everything in the namespace", ">", `.+$`},
	} {
		got, err := patternRegex(tc.pattern)
		if err != nil {
			t.Errorf("%s: patternRegex(%q): %v", tc.name, tc.pattern, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: patternRegex(%q) = %q, want %q", tc.name, tc.pattern, got, tc.want)
		}
	}
}

func TestPatternRegexRefusesPatternsWithNoSafeReading(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, pattern string }{
		{"empty", ""},
		{"`>` before the end", "crewlet.>.inbox"},
		{"an empty segment", "crewlet..inbox"},
		{"a namespace path", "acme/prod/crewlet.events.>"},
	} {
		if _, err := patternRegex(tc.pattern); !errors.Is(err, ErrSubject) {
			t.Errorf("%s: patternRegex(%q) = %v, want an ErrSubject", tc.name, tc.pattern, err)
		}
	}
}

// TestPatternRegexMatchesTheWayThePatternConsumerWill runs the translated
// regex the way the client does: unanchored, against FULLY-QUALIFIED topic
// names inside the namespace.
//
// This is the case that catches the two failures a per-domain dashboard
// filter dies of — `*` over-matching a deeper subject (turning a filter into
// a firehose) and the namespace prefix letting a neighbouring tenant's topic
// through. Both are silent: the subscriber just receives the wrong set.
func TestPatternRegexMatchesTheWayThePatternConsumerWill(t *testing.T) {
	t.Parallel()
	cfg := testCfg()
	for _, tc := range []struct {
		name    string
		pattern string
		subject string
		full    string // set to override the tenant/namespace
		want    bool
	}{
		{name: "`*` takes exactly one segment", pattern: "crewlet.events.*", subject: "crewlet.events.task_created", want: true},
		{name: "`*` refuses two", pattern: "crewlet.events.*", subject: "crewlet.events.task.created", want: false},
		{name: "`*` refuses none", pattern: "crewlet.events.*", subject: "crewlet.events", want: false},
		{name: "`>` takes one", pattern: "crewlet.events.>", subject: "crewlet.events.task_created", want: true},
		{name: "`>` takes many", pattern: "crewlet.events.>", subject: "crewlet.events.task.created", want: true},
		{name: "`>` needs at least one", pattern: "crewlet.events.>", subject: "crewlet.events", want: false},
		{name: "a different domain does not match", pattern: "crewlet.events.>", subject: "crewlet.notifications.inbound", want: false},
		{name: "dead letters stay outside the events feed", pattern: "crewlet.events.>", subject: topics.DeadLetter("crewlet.events.x", "g"), want: false},
		{
			name:    "another tenant's identically-named topic does not match",
			pattern: "crewlet.events.>",
			full:    "persistent://other/prod/crewlet.events.task_created",
			want:    false,
		},
	} {
		local, err := patternRegex(tc.pattern)
		if err != nil {
			t.Errorf("%s: patternRegex(%q): %v", tc.name, tc.pattern, err)
			continue
		}
		// What the client compiles: the tenant/namespace prefix plus the
		// local regex, matched unanchored against each topic it finds.
		re, err := regexp.Compile(regexp.QuoteMeta(cfg.nsPath()+"/") + local)
		if err != nil {
			t.Errorf("%s: the translated pattern does not compile: %v", tc.name, err)
			continue
		}
		full := tc.full
		if full == "" {
			full = cfg.fullTopic(tc.subject)
		}
		if got := re.MatchString(full); got != tc.want {
			t.Errorf("%s: %q against %q = %v, want %v", tc.name, tc.pattern, full, got, tc.want)
		}
	}
}
