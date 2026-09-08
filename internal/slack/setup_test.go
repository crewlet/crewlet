package slack_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

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
	// AND THE PAGE IT IS DISPLAYED ON. Install to Workspace lives there
	// too, but the value is what the operator came for: creating the app
	// from a manifest leaves them on Basic Information, and installing it
	// redirects the browser away from Slack entirely, so a line that names
	// no page leaves them hunting through four of them.
	if !strings.Contains(bot, "`OAuth & Permissions > Install to Workspace`") {
		t.Errorf("the bot token does not name the page it is displayed on: %q", bot)
	}
	if !strings.Contains(secret, "`Basic Information > App Credentials`") {
		t.Errorf("the signing secret does not name the page it is on: %q", secret)
	}
	if bot == secret {
		t.Error("both credentials give the same directions")
	}
}

// A CHANNEL IS A NAME SOMEBODY CHOOSES, so it gets no walk. What it does
// carry is the step that is invisible until the agent stays silent: a Slack
// bot reads and posts only in channels it has been invited to.
func TestTheDefaultChannelNamesTheInviteRatherThanAPath(t *testing.T) {
	t.Parallel()
	channel := byField(slack.Requirements("sre-lead", nil, nil))["channel"]

	if strings.Contains(channel.Help, "](") {
		t.Errorf("the channel sends a reader to a page that issues nothing: %q", channel.Help)
	}
	if !strings.Contains(channel.Help, "Invite") {
		t.Errorf("the channel never mentions the invite the bot needs: %q", channel.Help)
	}
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
