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

// A MENU PATH IS A THING TO FOLLOW EXACTLY, so it wears the face this screen
// gives a literal rather than running into the prose around it.
//
// It is not a link, and cannot be: Mattermost opens user settings as a modal
// and serves its whole web app from one route, so there is no address for
// this page and an anchor claiming otherwise would land somewhere else.
func TestTheAdminTokenHelpSetsThePathApart(t *testing.T) {
	t.Parallel()
	reqs := byField(mattermost.Requirements(nil, nil))

	admin, ok := reqs["admin_token"]
	if !ok {
		t.Fatal("the form asks for no administrator token")
	}
	// THE PATH A PERSON FOLLOWS, in the face this screen gives a literal.
	// Mattermost opens user settings as a modal and serves its whole web
	// app from one route, so there is no address for this page: an anchor
	// claiming to go there would land somewhere else and say nothing about
	// it, and prose runs a menu path into the sentence around it.
	const path = "`Profile > Security > Personal Access Tokens`"
	if !strings.Contains(admin.More, path) {
		t.Errorf("the field does not set the path apart: %q", admin.More)
	}
	var stepped bool
	for _, step := range mattermost.Guide().Prepare {
		if step.Yields == "admin_token" && strings.Contains(step.Text, path) {
			stepped = true
		}
		if strings.Contains(step.Text, "](") || step.Link != "" {
			t.Errorf("a step links a page this app has no address for: %+v", step)
		}
	}
	if !stepped {
		t.Errorf("no step sets the path apart: %+v", mattermost.Guide().Prepare)
	}
	if strings.Contains(admin.Hint+admin.More, "](") || admin.VendorURL != "" {
		t.Errorf("the field links a page this app has no address for: %q %q", admin.Hint, admin.More)
	}
}
