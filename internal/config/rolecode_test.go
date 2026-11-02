package config_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/config"
)

// ONE TIER VOCABULARY, WRITTEN TWICE, WHICH MUST NOT DRIFT.
//
// role.code.access is the tier the engine's own code host caps a seat's grant
// by (internal/code's Tier, ADR-0081). config owns the vocabulary, since it is
// where a person writes and the validator reads it, and internal/code spells
// its tiers as config's constants; it is held here all the same, element by
// element, because a tier added on one side and not the other would validate
// here and be read as read-only there, or be refused here and be perfectly
// good there. A GitHub App reads no tier: its access is its own permissions.
func TestRoleCodeAccessIsTheOneVocabulary(t *testing.T) {
	t.Parallel()
	if tiers := code.Tiers(); len(tiers) != len(config.CodeAccessTiers) {
		t.Errorf("config knows %v, the native code host %v", config.CodeAccessTiers, tiers)
	} else {
		for i, tier := range config.CodeAccessTiers {
			if string(tiers[i]) != tier {
				t.Errorf("tier %d: config says %q, the native code host %q", i, tier, tiers[i])
			}
		}
	}
	var silent *config.RoleCode
	if got := silent.AccessOrDefault(); got != config.TierReadOnly {
		t.Errorf("a seat with no code block runs at %q, want the least tier %q",
			got, config.TierReadOnly)
	}
	// The native host reads an unset tier as the default tier's cap.
	if unset, def := code.Tier("").Cap(), code.Tier(silent.AccessOrDefault()).Cap(); unset != def {
		t.Errorf("an unset tier caps a seat at %s, and the default tier at %s", unset, def)
	}
}

// A SEAT NOBODY HAS THOUGHT ABOUT ASKS FOR MORE RATHER THAN ALREADY HOLDING
// IT. Silence is read-only, each tier reads as itself, and anything else —
// which validation refuses — reads read-only, never wider: a hyphen for the
// underscore is no spelling of a tier.
func TestASeatsCodeAccessDefaultsToTheLeastAccess(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{
		"":              config.TierReadOnly,
		"read_only":     config.TierReadOnly,
		"review":        config.TierReview,
		"full_access":   config.TierFullAccess,
		"administrator": config.TierReadOnly,
		// Not spellings of a tier: refused at validation, and read as the
		// least access by anything reading a document that was not.
		"full-access": config.TierReadOnly,
		"read-only":   config.TierReadOnly,
		"Review":      config.TierReadOnly,
		" review ":    config.TierReadOnly,
	} {
		if got := (&config.RoleCode{Access: raw}).AccessOrDefault(); got != want {
			t.Errorf("AccessOrDefault(%q) = %q, want %q", raw, got, want)
		}
	}
}

// THE FIELD TAKES ITS THREE TIERS, EACH IN ONE SPELLING, AND NOTHING ELSE,
// refused at the key it was written at with a sentence naming all three. No
// tag shipped role.code.access, so nothing was ever written in another
// spelling and nothing needs reading in one: a second spelling — `full-access`
// beside `full_access`, or the case and space the tier's reader under
// integrations.github folded — would be a compatibility path for a version
// that never existed. The schema lists the same three as the field's enum
// (TestSchemaEnumsMatchTheValidators), so an editor underlines exactly what
// the engine refuses.
func TestACodeAccessIsOneOfItsThreeTiers(t *testing.T) {
	t.Parallel()
	doc := func(access string) string {
		return "name: Acme\nroles:\n  - name: Dev\n    code: {access: \"" + access + "\"}\n"
	}
	for _, ok := range config.CodeAccessTiers {
		if err := parsed(t, doc(ok)).Validate(); err != nil {
			t.Errorf("code.access %q was refused: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"read-only", "full-access", "Review", "FULL_ACCESS", " review", "review ",
		"reviw", "read__only", "full_access_", "maintain",
	} {
		err := parsed(t, doc(bad)).Validate()
		if !errors.Is(err, config.ErrUnknownValue) {
			t.Errorf("code.access %q: Validate() = %v, want ErrUnknownValue", bad, err)
			continue
		}
		problems := config.Problems(err)
		got := locatedOf(problems, "is not an access tier")
		want := []located{{"roles[0].code.access", "unknown_value", "dev", ""}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("code.access %q refused at %+v, want %+v", bad, got, want)
		}
		for _, p := range problems {
			if !strings.Contains(p.Message, "is not an access tier") {
				continue
			}
			if !strings.Contains(p.Message, `"read_only", "review" or "full_access"`) {
				t.Errorf("code.access %q: the refusal does not name the three tiers: %s", bad, p.Message)
			}
			if strings.Contains(p.Message, "hyphen") {
				t.Errorf("code.access %q: the refusal offers a second spelling: %s", bad, p.Message)
			}
		}
	}
}

// A CODE TIER ON A HUMAN SEAT IS REFUSED AT `code`, AND ONLY ON ADMISSION. A
// person acts on the native code host as their own identity (their bound
// token holds maintain there), so a tier on their seat would cap nothing and
// read as a setting that does. A
// written document is refused for it; a block that says nothing is not, and a
// written read_only is.
//
// AN ADMISSION RULE, like a human seat's own GitHub App beside it, because
// nothing running reads a tier off a human seat: the native grant skips one. So a revision being APPLIED that
// carries one runs, which is what lets a later build relax the rule — a tier
// that caps a person is the plausible relaxation — without this build
// refusing, mid rolling upgrade, a revision its newer peer admitted and
// splitting the fleet's epoch.
func TestACodeAccessOnAHumanSeatIsRefused(t *testing.T) {
	t.Parallel()
	human := "name: Acme\nroles:\n  - name: Sarah\n    kind: human\n    contact: {slack_user_id: U0SARAH}\n"
	for _, tc := range []struct {
		name, block string
		refused     bool
	}{
		{"a written tier", "    code: {access: review}\n", true},
		{"the least tier, written", "    code: {access: read_only}\n", true},
		{"the widest tier", "    code: {access: full_access}\n", true},
		{"a block that says nothing", "    code: {}\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			company := parsed(t, human+tc.block)
			var want []located
			if tc.refused {
				want = []located{{"roles[0].code", "conflict", "sarah", ""}}
			}
			for _, check := range []struct {
				name string
				err  error
			}{
				{"Validate", company.Validate()},
				{"ValidateAdmission", company.ValidateAdmission()},
			} {
				problems := config.Problems(check.err)
				if got := locatedOf(problems, codeAccessOnHuman); !reflect.DeepEqual(got, want) {
					t.Errorf("%s: human-seat code refusals = %+v, want %+v\nall: %+v",
						check.name, got, want, problems)
				}
				if got := locatedOf(problems, agentOnlyOnHuman); len(got) != 0 {
					t.Errorf("%s: the code tier is refused as a runnable agent-only key too: %+v",
						check.name, got)
				}
			}
			// Applying it is not refused: nothing running reads it.
			if err := company.ValidateRunnable(); err != nil {
				t.Errorf("ValidateRunnable() = %v, want a human seat's code tier applied", err)
			}
		})
	}
}

// THE CHART CARRIES THE TIER AS IT WAS WRITTEN, AND NOTHING WHERE NOTHING
// WAS. The org model is what the native code host's grant reads a seat's tier
// from, so a seat whose copy the chart dropped would be capped at read_only
// on every repository whatever its document says. And silence stays silence:
// the native host reads it as read_only through its own reading, so a default
// written into the chart would be a second statement of that rule.
func TestTheChartCarriesASeatsCodeAccessAsWritten(t *testing.T) {
	t.Parallel()
	for block, want := range map[string]string{
		"":                                  "",
		"    code: {}\n":                    "",
		"    code: {access: read_only}\n":   config.TierReadOnly,
		"    code: {access: review}\n":      config.TierReview,
		"    code: {access: full_access}\n": config.TierFullAccess,
	} {
		company := parsed(t, "name: Acme\nroles:\n  - name: Dev\n"+block)
		if got := company.Roles[0].Seat().CodeAccess; got != want {
			t.Errorf("a seat written %q carries code access %q into the chart, want %q",
				block, got, want)
		}
	}
}
