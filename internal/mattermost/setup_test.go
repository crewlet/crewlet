package mattermost_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/setup"
)

func byField(reqs []setup.Requirement) map[string]setup.Requirement {
	out := map[string]setup.Requirement{}
	for _, r := range reqs {
		out[r.Field] = r
	}
	return out
}

// THE LINK IS BUILT FROM THE INSTANCE BOX, because a self-hosted app has no
// address this engine could know.
//
// `{url}` is filled from the field above it, so the link follows what is
// typed and is dropped entirely until something is. Hard-coding a host would
// send every operator to somebody else's server, and a help line with no link
// at all left them to find a setting most people do not know exists.
func TestTheAdminTokenHelpLinksTheOperatorsOwnInstance(t *testing.T) {
	t.Parallel()
	reqs := byField(mattermost.Requirements(nil, nil))

	admin, ok := reqs["admin_token"]
	if !ok {
		t.Fatal("the form asks for no administrator token")
	}
	if !strings.Contains(admin.Help, "{url}") {
		t.Errorf("the help names no instance to link to: %q", admin.Help)
	}
	// A MARKDOWN LINK, which is what the screen renders as an anchor inside
	// the sentence rather than as "Open Mattermost" after a full stop.
	if !strings.Contains(admin.Help, "[turning them on](") {
		t.Errorf("the help carries no link: %q", admin.Help)
	}
	// THE FIELD THE TEMPLATE READS has to be one this form actually has,
	// or the link is dropped for ever and nothing says why.
	if _, has := reqs["url"]; !has {
		t.Error("the help templates {url}, and the form has no url field to fill it from")
	}

	// AND IT NAMES THE PAGE A PERSON GOES TO. Creating the token is a modal
	// on the admin's own account rather than an address, so the sentence
	// says where that is and links the setting that has to be on first.
	if !strings.Contains(admin.Help, "Profile > Security") {
		t.Errorf("the help does not say where a token is made: %q", admin.Help)
	}
}
