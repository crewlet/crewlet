package slack_test

import (
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
	if !strings.Contains(bot, "`OAuth & Permissions > Install to Workspace`") {
		t.Errorf("the bot token does not name the page it is minted on: %q", bot)
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
