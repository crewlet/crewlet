package digitalocean_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/digitalocean"
)

// A TEAM'S NAME MAY HOLD A SPACE, as the control panel shows it and as the
// document's own rule allows: the form takes it as the save path would store
// it. Declared as an identifier, the field refused every inner space, so a
// team called "Acme Labs" could not be connected from the dashboard.
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
