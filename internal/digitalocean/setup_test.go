package digitalocean_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/digitalocean"
)

// A TEAM'S NAME MAY HOLD A SPACE, as the document's own rule allows; as an
// identifier the field refused one, so "Acme Labs" could not be connected.
func TestATeamNameWithASpaceIsTakenAsTheDocumentTakesIt(t *testing.T) {
	t.Parallel()
	for _, r := range digitalocean.Requirements(nil, nil) {
		if r.Field != "team" {
			continue
		}
		if problem := r.ValueProblem("Acme Labs"); problem != "" {
			t.Errorf("the form refuses a team the document takes: %s", problem)
		}
		return
	}
	t.Fatal("DigitalOcean asks for no team")
}
