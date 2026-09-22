package slack_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/slack"
)

func byField(reqs []setup.Requirement) map[string]setup.Requirement {
	out := map[string]setup.Requirement{}
	for _, r := range reqs {
		out[r.Field] = r
	}
	return out
}

// EVERY CREDENTIAL FIELD CARRIES THE WALK TO ITS VALUE.
//
// Nothing behind these two boxes can be provisioned: somebody has to go
// through Slack's own pages and come back with what they find, so the only
// thing this form can do to help is say exactly where to go. It described
// what the value WAS instead ("what this agent posts as", under a field
// labelled Bot token), then said where to find it in prose, then ended in a
// bare "Open Slack" that landed on an app list with no clue which of four
// pages was meant.
func TestEverySlackCredentialSaysWhereToGetIt(t *testing.T) {
	t.Parallel()
	reqs := byField(slack.Requirements("sre-lead", nil, nil))

	for _, field := range []string{"bot_token", "signing_secret"} {
		r, ok := reqs[field]
		if !ok {
			t.Fatalf("the form asks for no %s", field)
		}
		// THE PAGE, AS A LINK, and inside the sentence rather than after
		// it: the first thing a reader needs is the address, and a
		// trailing anchor makes them work out which page the words meant.
		if !strings.Contains(r.Help, "](https://api.slack.com/apps)") {
			t.Errorf("%s does not link the page that issues it: %q", field, r.Help)
		}
		// THE PATH, in the face this screen gives a literal, for the reason
		// Mattermost's administrator token carries one: a menu path is
		// followed exactly, and prose runs it into the sentence around it.
		if !strings.Contains(r.Help, "`") {
			t.Errorf("%s sets no part of the path apart: %q", field, r.Help)
		}
		// AND WHAT TO DO WITH WHAT YOU FIND. The line is a walk, so it
		// ends where the walk does, at this box.
		if !strings.Contains(r.Help, "paste") {
			t.Errorf("%s never says to paste the value here: %q", field, r.Help)
		}
		// NO TRAILING "Open Slack". A field whose sentence carries its own
		// link gets a second one appended from VendorURL, and two links to
		// one page in one line is the reader's question asked twice.
		if r.VendorURL != "" {
			t.Errorf("%s adds a trailing link beside the one in its sentence", field)
		}
		// ONE LINE. `Where` is rendered as a second sentence after the
		// help, which is how the old copy grew into three clauses.
		if r.Where != "" {
			t.Errorf("%s carries a second sentence: %q", field, r.Where)
		}
	}
}

// THE PATHS DIFFER, because the two values are on two different pages of the
// same app, and that is the whole reason a path beats a link to the app.
func TestTheTwoSlackCredentialsNameDifferentPages(t *testing.T) {
	t.Parallel()
	reqs := byField(slack.Requirements("sre-lead", nil, nil))

	bot, secret := reqs["bot_token"].Help, reqs["signing_secret"].Help
	// THE APP IS CREATED ON THIS LINE, and from the manifest rather than
	// from scratch: an app built by hand has no scopes, no events and no
	// request URL, so installing it hands back a token that reports success
	// and sees an empty workspace.
	if !strings.Contains(bot, "`Create New App > From an app manifest`") {
		t.Errorf("the bot token does not say to create the app from its manifest: %q", bot)
	}
	// AND IT WALKS THE WIZARD THIS FORM'S OWN MANIFEST OPENS.
	//
	// It named `OAuth & Permissions > Install to Workspace`, which is where
	// an app built FROM SCRATCH adds its scopes and installs. An app created
	// from a manifest is installed by the wizard — workspace, Next, Create
	// and Install, Allow — and ends on the page holding the token. Naming
	// the other path is worse than naming none: the pages are real, so
	// somebody follows it and concludes the value is missing rather than
	// that they are somewhere else.
	for _, step := range []string{
		"`Create New App > From an app manifest`",
		"`Create and Install`",
		"`Allow`",
		"`Your app credentials`",
	} {
		if !strings.Contains(bot, step) {
			t.Errorf("the bot token's directions skip %s: %q", step, bot)
		}
	}
	if strings.Contains(bot, "Install to Workspace") {
		t.Errorf("the bot token sends an operator down the from-scratch "+
			"path, which this form's manifest does not take: %q", bot)
	}
	if !strings.Contains(secret, "`Basic Information > App Credentials`") {
		t.Errorf("the signing secret does not name the page it is on: %q", secret)
	}
	if bot == secret {
		t.Error("both credentials give the same directions")
	}
}

// THE FORM ASKS FOR THE TWO CREDENTIALS AND NOTHING ELSE.
//
// A third box sat here, "Default channel", for as long as the transport had a
// `Send` whose empty-channel fallback was the seat's configured one — and that
// send had no caller anywhere in the tree. Every message an agent posts is the
// Slack MCP server's call on this same token, and it names its own channel, so
// the box collected a value nothing read: an operator typed a channel id, the
// dialog reported the seat configured, and the agent went on posting exactly
// where it was addressed. That is worse than not asking, because the answer
// looks like configuration and is stored in the company document as though it
// were. A seat's room is `units[].channel` on the org chart, which the
// executor prompt renders as the team channel.
//
// The whole list rather than "no channel field", so the next value added
// without a reader has to be argued for here first.
func TestTheSlackSeatFormAsksOnlyForValuesThatAreRead(t *testing.T) {
	t.Parallel()
	var fields []string
	for _, r := range slack.Requirements("sre-lead", nil, nil) {
		fields = append(fields, r.Field)
	}
	if !slices.Equal(fields, []string{"bot_token", "signing_secret"}) {
		t.Fatalf("the seat form asks for %v, and every field on it has to be "+
			"read by something", fields)
	}
}

// EVERY BOX ON THE FORM NAMES A FIELD THE SEAT ACTUALLY HAS.
//
// `ConfigPath` is where [setup] writes the answer, relative to the seat, and
// it is a STRING — nothing connects it to the struct it addresses. A path
// naming a field the seat does not carry is a box an operator can fill in and
// never submit. Walking the json tags is the only thing in the tree that ties
// the two together.
//
// THE RUNTIME SEAT, [org.Role], and not the authored [config.Role]. A
// per-seat write goes through the org chart, whose blob holds exactly this
// shape — so a vendor identity sits at the top level, and a path written
// against the authored document would resolve to nothing at the one moment it
// is used.
func TestEverySlackRequirementNamesAFieldTheSeatHas(t *testing.T) {
	t.Parallel()
	for _, r := range slack.Requirements("sre-lead", nil, nil) {
		if r.ConfigPath == "" {
			t.Errorf("%s says nowhere to write its answer", r.Field)
			continue
		}
		if err := resolvePath(reflect.TypeFor[org.Role](), r.ConfigPath); err != nil {
			t.Errorf("%s: %v", r.Field, err)
		}
	}
	// THE CONTROL, and it is the case this walk exists for: the AUTHORED
	// path these used to carry resolves in neither shape now, so a stale
	// one would be caught rather than quietly written into a seat nobody
	// reads.
	if err := resolvePath(reflect.TypeFor[org.Role](),
		"integrations.slack.bot_token"); err == nil {
		t.Error("the authored path still resolves against the runtime seat, " +
			"so this walk would not notice one left behind")
	}
}

// resolvePath walks a dotted config path through a struct's json tags, the
// same names the seat document is written in.
func resolvePath(t reflect.Type, path string) error {
	for _, segment := range strings.Split(path, ".") {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct {
			return fmt.Errorf("%q: %s is not a struct", path, t)
		}
		field, found := fieldByJSONName(t, segment)
		if !found {
			return fmt.Errorf("%q: %s has no %q", path, t, segment)
		}
		t = field
	}
	return nil
}

func fieldByJSONName(t reflect.Type, name string) (reflect.Type, bool) {
	for i := range t.NumField() {
		f := t.Field(i)
		if tag, _, _ := strings.Cut(f.Tag.Get("json"), ","); tag == name {
			return f.Type, true
		}
	}
	return nil, false
}

// THE MANIFEST IS THE ENGINE'S OWN, so the app a person builds by hand and
// the app the provisioning command pushes are the same app.
//
// Seventeen scopes and five event subscriptions decide whether an agent can
// hear anything, and every one of them is a decision that lives in Go. A
// screen that asked an operator to reproduce them from a table would be
// asking them to get one wrong, and the failure mode is a bot that reports
// success and sees an empty workspace.
func TestASeatsManifestIsTheOneTheProvisionerPushes(t *testing.T) {
	t.Parallel()
	const base = "https://engine.example.com"

	text, err := slack.ManifestJSON("SRE Lead", "sre-lead", base)
	if err != nil {
		t.Fatalf("ManifestJSON: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("the manifest is not JSON a person could paste: %v", err)
	}
	want, err := slack.Manifest("SRE Lead", "sre-lead", base)
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	// THROUGH JSON BOTH WAYS, because that is what the comparison is about:
	// the text an operator pastes has to decode to the structure the
	// command sends, not merely be built beside it.
	round, _ := json.Marshal(want)
	var expect map[string]any
	_ = json.Unmarshal(round, &expect)
	if !reflect.DeepEqual(got, expect) {
		t.Error("the pasted manifest and the pushed one are different apps")
	}

	// AND IT IS READ, not only pasted: an operator comparing it against an
	// app they already have is reading a diff, and one line of JSON is not
	// one a person can diff.
	if !strings.Contains(text, "\n") {
		t.Error("the manifest is one line, which nobody can read or diff")
	}

	// THIS SEAT'S OWN ADDRESS. A per-seat app has a route per agent, so a
	// manifest carrying somebody else's would deliver this agent's mentions
	// to another seat's inbox.
	if !strings.Contains(text, base+"/webhooks/slack/sre-lead") {
		t.Errorf("the manifest does not carry this seat's request URL:\n%s", text)
	}
}

// A NAME SLACK WOULD REFUSE IS REFUSED HERE, rather than pasted and rejected
// in a browser with a message about a field the operator never typed.
func TestAManifestForAnUnusableNameIsRefused(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a really long role name ", 10)
	if _, err := slack.ManifestJSON(long, "sre-lead", "https://engine.example.com"); err == nil {
		t.Error("a role name Slack caps produced a manifest anyway")
	}
}
