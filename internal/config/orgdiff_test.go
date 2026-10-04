package config_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// diffBase is the document every case edits: a root CEO, an Engineering
// division the CTO leads holding a Platform team, and a Design team beside it.
const diffBase = `
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${K}"]
roles:
  - name: CEO
    handle: ceo
    llm: zulu
units:
  - name: Engineering
    id: engineering
    lead: cto
    project: ENG
    roles:
      - name: CTO
        handle: cto
        llm: zulu
    children:
      - name: Platform
        id: platform
        purpose: keep the lights on
        roles:
          - name: Staff Engineer
            handle: staff-eng
            llm: zulu
            mcp_env:
              github:
                GITHUB_TOKEN: ${PLATFORM_GITHUB}
          - name: SRE
            handle: sre
            llm: zulu
  - name: Design
    id: design
    lead: designer
    project: DSN
    roles:
      - name: Designer
        handle: designer
        llm: zulu
`

// parse is one document, as a write's prepared company would be: read and
// its identities minted, and not yet validated — a write is judged before
// it is validated.
func parse(t *testing.T, doc string) *config.Company {
	t.Helper()
	c, err := config.ParseCompanyDocument([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return c
}

// edit is the base document with one textual replacement, which the case
// asserts actually happened.
func edit(t *testing.T, old, replacement string) *config.Company {
	t.Helper()
	if !strings.Contains(diffBase, old) {
		t.Fatalf("the fixture has no %q to replace", old)
	}
	return parse(t, strings.Replace(diffBase, old, replacement, 1))
}

// only is the one change a diff holds, failing when it holds any other count.
func only(t *testing.T, d config.OrgDiff) config.OrgChange {
	t.Helper()
	if len(d.Changes) != 1 {
		t.Fatalf("changes = %+v, want exactly one", d.Changes)
	}
	return d.Changes[0]
}

// touches renders a change's places as side/unit/why/value, for comparison.
func touches(c config.OrgChange) []string {
	var out []string
	for _, t := range c.Touches {
		out = append(out, string(t.Side)+"/"+t.Unit+"/"+t.Why+"/"+t.Value)
	}
	return out
}

// AN UNCHANGED DOCUMENT CHANGES NOTHING.
//
// The control every other case rests on: a diff that reported something for
// the same document twice would refuse a lead every write.
func TestTheSameDocumentDiffsToNothing(t *testing.T) {
	t.Parallel()
	d := config.DiffOrg(parse(t, diffBase), parse(t, diffBase))
	if len(d.Changes) != 0 || len(d.Settings) != 0 {
		t.Errorf("an unchanged document diffed to %+v, settings %v", d.Changes, d.Settings)
	}
	if d.Before == nil || d.After == nil {
		t.Error("the diff carries no organization to decide its places in")
	}
}

// EVERY OP REACHES THE PLACES ITS SIDE OF THE WRITE HOLDS IT IN.
//
// An addition is placed where it lands, a removal where it was, a move at
// both ends, and an edit of a seat in its unit on both sides. A unit is placed
// in its PARENT, which is what keeps a lead from removing or moving the top
// unit they lead.
func TestEachOpReachesItsPlaces(t *testing.T) {
	t.Parallel()
	base := parse(t, diffBase)
	for _, c := range []struct {
		name    string
		after   *config.Company
		id      string
		op      config.OrgOp
		touches []string
	}{
		{"a seat added", edit(t, "          - name: SRE\n", "          - name: Intern\n            handle: intern\n            llm: zulu\n          - name: SRE\n"),
			"intern", config.OrgAdded, []string{"after/platform/place/"}},
		{"a seat removed", edit(t, "          - name: SRE\n            handle: sre\n            llm: zulu\n", ""),
			"sre", config.OrgRemoved, []string{"before/platform/place/"}},
		{"a seat edited", edit(t, "            handle: sre\n", "            handle: sre\n            goal: keep it up\n"),
			"sre", config.OrgChanged, []string{"before/platform/place/", "after/platform/place/"}},
		{"a unit removed is placed in its parent", edit(t, "  - name: Design\n    id: design\n    lead: designer\n    project: DSN\n    roles:\n      - name: Designer\n        handle: designer\n        llm: zulu\n", ""),
			"design", config.OrgRemoved, []string{"before//place/"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d := config.DiffOrg(base, c.after)
			var change config.OrgChange
			for _, ch := range d.Changes {
				if ch.ID == c.id {
					change = ch
				}
			}
			if change.Op != c.op || !slices.Equal(touches(change), c.touches) {
				t.Errorf("%s = %s %v, want %s %v (all changes: %+v)",
					c.id, change.Op, touches(change), c.op, c.touches, d.Changes)
			}
		})
	}
}

// A MOVE REACHES BOTH ENDS, and a unit's seats move with it without changing.
//
// Moving Platform from Engineering to Design is one change, placed in its
// parent on each side; the seats inside it sit where they sat — in Platform —
// so they are not changes at all.
func TestAMoveReachesBothEnds(t *testing.T) {
	t.Parallel()
	before := parse(t, diffBase)
	after := parse(t, `
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${K}"]
roles:
  - name: CEO
    handle: ceo
    llm: zulu
units:
  - name: Engineering
    id: engineering
    lead: cto
    project: ENG
    roles:
      - name: CTO
        handle: cto
        llm: zulu
  - name: Design
    id: design
    lead: designer
    project: DSN
    roles:
      - name: Designer
        handle: designer
        llm: zulu
    children:
      - name: Platform
        id: platform
        purpose: keep the lights on
        roles:
          - name: Staff Engineer
            handle: staff-eng
            llm: zulu
            mcp_env:
              github:
                GITHUB_TOKEN: ${PLATFORM_GITHUB}
          - name: SRE
            handle: sre
            llm: zulu
`)
	change := only(t, config.DiffOrg(before, after))
	want := []string{"before/engineering/place/", "after/design/place/"}
	if change.ID != "platform" || change.Op != config.OrgMoved ||
		!slices.Equal(touches(change), want) {
		t.Errorf("the move diffed to %s %s %v, want platform moved %v",
			change.ID, change.Op, touches(change), want)
	}
}

// A UNIT'S SEATS AND CHILD UNITS ARE NOT ITS FIELDS, and a seat's display name
// is a field like any other rather than its identity.
//
// Adding a seat to Platform changes the seat and leaves Platform alone, so a
// lead who may add to a team is not judged as editing it. And renaming the SRE
// for display is an edit of the seat its handle names — never a removal and an
// addition, which would judge a different, wider pair of places.
func TestAnObjectsFieldsAreItsOwn(t *testing.T) {
	t.Parallel()
	base := parse(t, diffBase)
	added := config.DiffOrg(base,
		edit(t, "          - name: SRE\n", "          - name: Intern\n            handle: intern\n            llm: zulu\n          - name: SRE\n"))
	if change := only(t, added); change.Kind != config.OrgSeat {
		t.Errorf("adding a seat changed the %s %s", change.Kind, change.ID)
	}
	renamed := only(t, config.DiffOrg(base, edit(t, "- name: SRE\n", "- name: Site Reliability\n")))
	if renamed.Op != config.OrgChanged || !slices.Equal(renamed.Fields, []string{"name"}) {
		t.Errorf("a display rename diffed to %s %v, want a change of name", renamed.Op,
			renamed.Fields)
	}
	purpose := only(t, config.DiffOrg(base, edit(t, "keep the lights on", "ship the platform")))
	want := []string{"before/platform/self/", "after/platform/self/"}
	if purpose.ID != "platform" || !slices.Equal(touches(purpose), want) {
		t.Errorf("a unit's own edit reached %v, want the unit itself on both sides %v",
			touches(purpose), want)
	}
}

// A NEW REFERENCE REACHES WHAT IT NAMES, after the write; one already there
// is not judged again.
//
// A `manages:` entry reaches the unit the named seat sits in, a unit key the
// unit itself, a root seat the root, and a name nothing answers to the root.
// A unit's `lead:` reaches the unit the named seat sits in.
func TestANewReferenceReachesWhatItNames(t *testing.T) {
	t.Parallel()
	base := parse(t, diffBase)
	for _, c := range []struct {
		name     string
		replaced string
		want     string
	}{
		{"a seat in another team", "            handle: sre\n", "after/design/manages/designer"},
		{"a unit by its key", "            handle: sre\n", "after/design/manages/design"},
		{"a seat at the root", "            handle: sre\n", "after//manages/ceo"},
		{"a name nothing answers to", "            handle: sre\n", "after//manages/ghost"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			value := c.want[strings.LastIndex(c.want, "/")+1:]
			change := only(t, config.DiffOrg(base, edit(t, c.replaced,
				c.replaced+"            manages: ["+value+"]\n")))
			if !slices.Contains(touches(change), c.want) {
				t.Errorf("manages %s reached %v, want %s", value, touches(change), c.want)
			}
		})
	}
	lead := only(t, config.DiffOrg(base, edit(t, "        purpose: keep the lights on\n",
		"        purpose: keep the lights on\n        lead: designer\n")))
	if !slices.Contains(touches(lead), "after/design/lead/designer") {
		t.Errorf("a new lead reached %v, want the unit the lead sits in", touches(lead))
	}
	// AND A REFERENCE THE OBJECT ALREADY CARRIED IS NOT JUDGED AGAIN: the
	// Design team's lead is unchanged when its purpose is edited.
	again := only(t, config.DiffOrg(base, edit(t, "    lead: designer\n",
		"    lead: designer\n    purpose: draw\n")))
	for _, touch := range again.Touches {
		if touch.Why == "lead" {
			t.Errorf("an unchanged lead was judged again: %v", touches(again))
		}
	}
}

// A NEW CLAIM REACHES EVERY OTHER OBJECT ALREADY CLAIMING THE KEY, before the
// write — whatever the spelling.
func TestANewClaimReachesItsOtherClaimants(t *testing.T) {
	t.Parallel()
	base := parse(t, diffBase)
	change := only(t, config.DiffOrg(base, edit(t, "        purpose: keep the lights on\n",
		"        purpose: keep the lights on\n        project: dsn\n")))
	if !slices.Contains(touches(change), "before/design/project/dsn") {
		t.Errorf("claiming Design's project reached %v, want Design itself", touches(change))
	}
	free := only(t, config.DiffOrg(base, edit(t, "        purpose: keep the lights on\n",
		"        purpose: keep the lights on\n        project: PLAT\n")))
	for _, touch := range free.Touches {
		if touch.Why == "project" {
			t.Errorf("a key nobody claims reached %v", touches(free))
		}
	}
	// AND A CLAIM THE OBJECT ALREADY MADE IS NOT JUDGED AGAIN: Platform and
	// Design sharing a channel somebody else wired is no reason to refuse an
	// edit of Platform's purpose.
	shared := strings.Replace(strings.Replace(diffBase,
		"        purpose: keep the lights on\n",
		"        purpose: keep the lights on\n        channel: shared\n", 1),
		"    project: DSN\n", "    project: DSN\n    channel: shared\n", 1)
	edited := strings.Replace(shared, "keep the lights on", "ship it", 1)
	again := only(t, config.DiffOrg(parse(t, shared), parse(t, edited)))
	for _, touch := range again.Touches {
		if touch.Why == "channel" {
			t.Errorf("a claim the unit already made was judged again: %v", touches(again))
		}
	}
}

// A CREDENTIAL CHANGE IS LISTED APART, and an unchanged one is not.
//
// Setting, altering and clearing a credential are all changes; carrying it
// through untouched is not, and neither is a credential block with nothing in
// it. A `${VAR}` anywhere in the object is listed as one too.
//
// Mutation: list only the credential fields and the address naming a
// variable is no change a lead is refused.
func TestACredentialChangeIsListedApart(t *testing.T) {
	t.Parallel()
	base := parse(t, diffBase)
	altered := only(t, config.DiffOrg(base, edit(t, "${PLATFORM_GITHUB}", "${CEO_GITHUB}")))
	if !slices.Equal(altered.Credentials, []string{"mcp_env.github.GITHUB_TOKEN"}) {
		t.Errorf("an altered credential listed %v", altered.Credentials)
	}
	goal := only(t, config.DiffOrg(base, edit(t, "            handle: staff-eng\n",
		"            handle: staff-eng\n            goal: ship\n")))
	if len(goal.Credentials) != 0 {
		t.Errorf("a goal edit listed credentials %v", goal.Credentials)
	}
	added := config.DiffOrg(base, edit(t, "          - name: SRE\n",
		"          - name: Intern\n            handle: intern\n            llm: zulu\n"+
			"            mcp_env:\n              github:\n                GITHUB_TOKEN: ${ANY}\n"+
			"          - name: SRE\n"))
	if change := only(t, added); !slices.Equal(change.Credentials,
		[]string{"mcp_env.github.GITHUB_TOKEN"}) {
		t.Errorf("a seat added with a credential listed %v", change.Credentials)
	}
	// A BLOCK WHOSE CREDENTIAL FIELDS ARE UNSET HOLDS NONE: a seat's own
	// GitHub App declared before it has a key is no credential change.
	unset := config.DiffOrg(base, edit(t, "          - name: SRE\n",
		"          - name: Intern\n            handle: intern\n            llm: zulu\n"+
			"            integrations:\n              github:\n                tier: review\n"+
			"          - name: SRE\n"))
	if change := only(t, unset); len(change.Credentials) != 0 {
		t.Errorf("a seat added with no credential listed %v", change.Credentials)
	}
	// AND A `${VAR}` IN ANY FIELD IS ONE, credential field or not: a seat's
	// address and contact ids resolve a whole reference from the engine's
	// own environment, and are recited back to whoever asks about the seat.
	// A literal in the same field is not.
	ref := only(t, config.DiffOrg(base, edit(t, "            handle: sre\n",
		"            handle: sre\n            email: ${CREWLET_KEYRING}\n")))
	if !slices.Equal(ref.Credentials, []string{"email"}) {
		t.Errorf("an address naming a variable listed %v, want email", ref.Credentials)
	}
	literal := only(t, config.DiffOrg(base, edit(t, "            handle: sre\n",
		"            handle: sre\n            email: sre@example.com\n")))
	if len(literal.Credentials) != 0 {
		t.Errorf("a literal address listed credentials %v", literal.Credentials)
	}
}

// A SETTING IS LISTED BY ITS TOP-LEVEL KEY, and the org chart is not one.
func TestASettingIsListedByItsKey(t *testing.T) {
	t.Parallel()
	d := config.DiffOrg(parse(t, diffBase), edit(t, "model: claude-sonnet-5", "model: claude-opus-5"))
	if len(d.Changes) != 0 || !slices.Equal(d.Settings, []string{"providers"}) {
		t.Errorf("a provider edit diffed to %+v, settings %v", d.Changes, d.Settings)
	}
	chart := config.DiffOrg(parse(t, diffBase), edit(t, "keep the lights on", "ship it"))
	if len(chart.Settings) != 0 {
		t.Errorf("an edit of the org chart listed the settings %v", chart.Settings)
	}
}

// TWO OBJECTS ANSWERING TO ONE ID REACH THE ROOT, which nobody's subtree
// holds: which of them a change is about is not a fact either side states.
func TestTwoUnitsOnOneKeyReachTheRoot(t *testing.T) {
	t.Parallel()
	twice := edit(t, "  - name: Design\n    id: design\n",
		"  - name: Ops\n    id: design\n  - name: Design\n    id: design\n")
	d := config.DiffOrg(parse(t, diffBase), twice)
	var found bool
	for _, change := range d.Changes {
		if change.ID == "design" {
			found = slices.Contains(touches(change), "after//duplicate/design")
		}
	}
	if !found {
		t.Errorf("a duplicated key diffed to %+v, want it placed at the root", d.Changes)
	}
}
