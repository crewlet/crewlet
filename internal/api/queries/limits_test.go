package queries_test

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
)

// askLimits is `limits` as a caller holding grants reads it.
func askLimits(t *testing.T, c *config.Company, grants ...iam.Grant) (queries.LimitsAnswer, error) {
	t.Helper()
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{Company: companySource(t, c)})
	got, err := r.Answer(asGrants(t, "", grants...), "limits", nil)
	if err != nil {
		return queries.LimitsAnswer{}, err
	}
	return got.(queries.LimitsAnswer), nil
}

// EVERY ROW'S KEY IS THE DOCUMENT PATH ITS VALUE IS READ FROM.
//
// A document writes a distinct value at every key the answer names, and each
// row must answer exactly that value: a key naming one setting while its
// getter reads another would put the wrong number under a correct-looking
// label, and a key that is not a path at all is refused by the parser here.
// The schedule timeout has no company-wide key and is answered as the
// engine's constant.
//
// Mutation: swap two getters in `limits` and their rows answer each other's
// values.
func TestEveryLimitIsReadFromTheKeyItNames(t *testing.T) {
	t.Parallel()
	// A value per key that no default holds and every validator admits,
	// so a row answering its default rather than the document fails.
	written := map[string]string{
		"turn_engine.max_iterations":                  "5",
		"turn_engine.onboarding_max_tool_rounds":      "7",
		"turn_engine.delegation_depth_limit":          "4",
		"turn_engine.delegation.max_parallel":         "2",
		"turn_engine.delegation.max_tasks_per_call":   "6",
		"turn_engine.delegation.max_turns":            "17",
		"turn_engine.delegation.budget_fraction":      "0.35",
		"turn_engine.delegation.task_timeout_seconds": "240",
		"turn_engine.delegation.call_timeout_seconds": "720",
		"notification_rate_limit":                     "9",
		"notification_coalesce_max_batch":             "33",
	}
	doc := "name: Acme\n" + nest(written)
	c, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatalf("parse:\n%s\n%v", doc, err)
	}
	got, err := askLimits(t, c, iam.GrantAdmin)
	if err != nil {
		t.Fatalf("limits: %v", err)
	}
	seen := map[string]bool{}
	for _, row := range got.Limits {
		seen[row.Key] = true
		if row.Key == "schedules.timeout_seconds" {
			if want := float64(org.DefaultScheduleTimeout / time.Second); row.Value != want ||
				row.Default != want {
				t.Errorf("the schedule timeout is %v (default %v), want the engine's %v",
					row.Value, row.Default, want)
			}
			continue
		}
		want, ok := written[row.Key]
		if !ok {
			t.Errorf("the answer names %q, which this case does not write: add it", row.Key)
			continue
		}
		if fmt.Sprint(row.Value) != want {
			t.Errorf("%s answers %v, and the document writes %s", row.Key, row.Value, want)
		}
		if row.Off {
			t.Errorf("%s = %v is reported off", row.Key, row.Value)
		}
		// THE DEFAULT IS JUDGED ON ITS OWN, not on the value written over
		// it: the rate limit's default is still no limit.
		if wantOff := row.Key == "notification_rate_limit"; row.DefaultOff != wantOff {
			t.Errorf("%s default_off = %v under a written %v, want %v",
				row.Key, row.DefaultOff, row.Value, wantOff)
		}
	}
	for key := range written {
		if !seen[key] {
			t.Errorf("the document writes %s and the answer has no row for it", key)
		}
	}
}

// nest writes dotted keys as one YAML document, so a key that is not a path
// the parser knows is refused rather than ignored.
func nest(flat map[string]string) string {
	type node struct {
		value    string
		children map[string]*node
		order    []string
	}
	root := &node{children: map[string]*node{}}
	for _, key := range slices.Sorted(maps.Keys(flat)) {
		n := root
		for _, part := range strings.Split(key, ".") {
			child, ok := n.children[part]
			if !ok {
				child = &node{children: map[string]*node{}}
				n.children[part] = child
				n.order = append(n.order, part)
			}
			n = child
		}
		n.value = flat[key]
	}
	var b strings.Builder
	var write func(n *node, depth int)
	write = func(n *node, depth int) {
		for _, name := range n.order {
			child := n.children[name]
			b.WriteString(strings.Repeat("  ", depth) + name + ":")
			if child.value != "" {
				b.WriteString(" " + child.value + "\n")
				continue
			}
			b.WriteString("\n")
			write(child, depth+1)
		}
	}
	write(root, 0)
	return b.String()
}

// A DOCUMENT THAT SAYS NOTHING IS HELD TO THE ENGINE'S DEFAULTS, and the
// answer says so with the values the engine enforces rather than zeros: the
// value of every row is its default, and the two limits a zero switches off
// are reported as such only where the value is zero, for the value and for
// the default alike.
//
// Mutation: read the default from anywhere but [config.DefaultCompany], or
// mark a row off whatever its value, and a row here disagrees.
func TestADocumentThatSaysNothingIsHeldToTheDefaults(t *testing.T) {
	t.Parallel()
	c, err := config.ParseCompany([]byte("name: Acme\n"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := askLimits(t, c, iam.GrantAdmin)
	if err != nil {
		t.Fatalf("limits: %v", err)
	}
	if len(got.Limits) == 0 {
		t.Fatal("no rows, so this case asserts nothing")
	}
	defaults := config.DefaultTurnEngine()
	for _, row := range got.Limits {
		if row.Value != row.Default {
			t.Errorf("%s answers %v against a default of %v on a document that writes nothing",
				row.Key, row.Value, row.Default)
		}
		if row.Key == "turn_engine.max_iterations" && row.Value != float64(defaults.MaxIterations) {
			t.Errorf("max_iterations answers %v, and the engine's default is %d",
				row.Value, defaults.MaxIterations)
		}
		// The rate limit's default is 0, no limit; onboarding's is a
		// positive budget.
		if wantOff := row.Key == "notification_rate_limit"; row.Off != wantOff ||
			row.DefaultOff != wantOff {
			t.Errorf("%s off = %v and default_off = %v, want both %v",
				row.Key, row.Off, row.DefaultOff, wantOff)
		}
	}
}

// THE LIMITS ARE AN ADMINISTRATOR'S, as the company document they are read
// from is, and a node running no company answers none: no turn runs there.
func TestTheLimitsAreAnAdministratorsAndNeedACompany(t *testing.T) {
	t.Parallel()
	c, err := config.ParseCompany([]byte("name: Acme\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := askLimits(t, c, iam.Apps...); !errors.Is(err, queries.ErrUnauthorized) {
		t.Errorf("a caller holding every app and not admin = %v, want unauthorized", err)
	}
	got, err := askLimits(t, nil, iam.GrantAdmin)
	if err != nil {
		t.Fatalf("limits with no company: %v", err)
	}
	if got.Limits == nil || len(got.Limits) != 0 {
		t.Errorf("a node running no company answers %+v, want an empty list", got.Limits)
	}
}
