package artifactsdomain_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A CREATE OF WHAT IS ALREADY THERE IS A COLLISION, in this domain's words
// wherever it is found: by the framework before the publish — the guard row a
// create's subject holds, which is what refuses the same create made again once
// its operation ledger has forgotten it — and by the applier after it. The
// framework's bare sentinel reached the artifacts API's fallback and was
// answered as this node's fault, and the applier's verdict was told as a
// malformed request; a source address keeps its own collision, which names the
// address rather than an id nobody chose.
//
// AND IT IS THE FRAMEWORK'S REFUSAL TOO, in its chain and not its words: a
// person is told a create of what is there in the state log's sentence on
// every domain ([statelog.Sentence]), and the framework's own text names the
// subject's broker path.
//
// Mutation: drop the ErrExists arm from publishError, or tell RefusedExists as
// ErrInvalid again, or take statelog.ErrExists out of the chain, and a case
// goes red.
func TestACreateOfWhatIsThereIsACollision(t *testing.T) {
	t.Parallel()
	guard := fmt.Errorf("%w: artifact.x", statelog.ErrExists)
	for name, c := range map[string]struct {
		err       error
		want, not error
	}{
		"an artifact the framework found made": {
			err:  artifactsdomain.PublishError(statelog.Subject{Kind: "artifact", ID: "x"}, guard),
			want: artifactsdomain.ErrExists, not: artifactsdomain.ErrSourceTaken},
		"a source address the framework found held": {
			err: artifactsdomain.PublishError(statelog.Subject{Kind: "source",
				ID: "jane.report"}, guard),
			want: artifactsdomain.ErrSourceTaken, not: artifactsdomain.ErrExists},
		"an artifact the applier found made": {
			err: artifactsdomain.RefusalError(t.Context(), &artifactsdomain.Writer{},
				artifacts.IDFor("op").String(), artifactsdomain.RefusedExists,
				artifactsdomain.HistorySummary{}, 0),
			want: artifactsdomain.ErrExists, not: artifactsdomain.ErrInvalid},
	} {
		if !errors.Is(c.err, c.want) || errors.Is(c.err, c.not) {
			t.Errorf("%s is told %v, want %v and not %v", name, c.err, c.want, c.not)
		}
		if !errors.Is(c.want, artifactsdomain.ErrExists) {
			continue
		}
		if said, _ := statelog.Sentence(c.err); said != statelog.AlreadyThere ||
			strings.Contains(c.err.Error(), "statelog") {
			t.Errorf("%s is told %q to a person and %q to a log, want %q and no "+
				"framework words", name, said, c.err, statelog.AlreadyThere)
		}
	}
	// AND A LOST RACE STAYS THE DOMAIN'S CONFLICT, carrying the framework's
	// sentinel and neither its words nor the subject, which a caller
	// printing it would show a person; its sentence is the state log's.
	// Mutations: wrap the framework's error, or name the subject, and the
	// text row goes red; drop the statelog arm from Sentence and the
	// sentence rows do.
	race := artifactsdomain.PublishError(statelog.Subject{Kind: "artifact", ID: "x"},
		fmt.Errorf("%w: crewlet.artifacts.artifact.x kept changing under this write",
			statelog.ErrConflict))
	if !errors.Is(race, artifactsdomain.ErrConflict) || !errors.Is(race, statelog.ErrConflict) {
		t.Errorf("a lost race is told %v", race)
	}
	if strings.Contains(race.Error(), "artifact.x") || strings.Contains(race.Error(), ": x") {
		t.Errorf("a lost race's error is %q, which names the subject", race)
	}
	if said := artifactsdomain.Sentence(race); said != statelog.LostRace {
		t.Errorf("a lost race is told %q, want %q", said, statelog.LostRace)
	}
	if said := artifactsdomain.Sentence(&artifactsdomain.AlreadyExists{What: "x"}); said !=
		statelog.AlreadyThere {
		t.Errorf("a create of what is there is told %q, want %q", said, statelog.AlreadyThere)
	}
}
