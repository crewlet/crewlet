package engine

import (
	"errors"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/org"
)

// A TURN'S PROMPT NAMES THE ATLAS ONLY WHERE WHOEVER WOKE IT MAY READ IT
// (ADR-0109, ADR-0127): an agent seat holds the atlas, so a turn nobody woke
// is shown the systems its work names; a turn a person woke is shown them only
// if that person holds the atlas, and a turn a stranger woke never. A
// directory this node cannot read is unknown, which shows nothing.
//
// Mutation: decide as the seat alone, without the turn's ceiling, and the
// person without the atlas reads it through the agent they woke.
func TestATurnsPromptNamesTheAtlasOnlyWhereWhoeverWokeItMayReadIt(t *testing.T) {
	t.Parallel()
	o := &org.Organization{Name: "Acme", Roles: []*org.Role{
		{Name: "SRE", DeclaredHandle: "sre"},
		{Name: "Jane", DeclaredHandle: "jane", Kind: org.KindHuman},
		{Name: "Priya", DeclaredHandle: "priya", Kind: org.KindHuman},
	}}
	o.Normalize()
	served := &directoryCopy{}
	served.set(oneSeatDirectory{whole: &atomic.Int64{}, holders: map[string]iamdomain.Requester{
		"jane": {SeatHolder: iamdomain.SeatHolder{Seat: "jane", Person: uuid.NewString(), Stage: iam.StageActive},
			Grants: []iam.Grant{iam.GrantWorklet}},
		"priya": {SeatHolder: iamdomain.SeatHolder{Seat: "priya", Person: uuid.NewString(), Stage: iam.StageActive},
			Grants: []iam.Grant{iam.GrantAtlas}},
	}})
	e := statelessWithDirectory(t, served)
	c := &Company{Org: o}
	e.epoch.current.Store(c)

	stranger := []types.CanonicalIdentity{{ExternalID: "U9", Platform: "chat", DisplayName: "Stranger"}}
	for _, tc := range []struct {
		woke  string
		tel   turnTelemetry
		reads bool
	}{
		{"nobody", turnTelemetry{}, true},
		{"a person holding the atlas", turnTelemetry{requester: "priya"}, true},
		{"a person without it", turnTelemetry{requester: "jane"}, false},
		{"a stranger", turnTelemetry{senders: stranger}, false},
	} {
		if got := e.turnReadsAtlas(t.Context(), c, "sre", tc.tel); got != tc.reads {
			t.Errorf("a turn %s woke reads the atlas: %v, want %v", tc.woke, got, tc.reads)
		}
	}
	if e.turnReadsAtlas(t.Context(), c, "jane", turnTelemetry{}) {
		t.Error("a person's seat was read as an agent's turn")
	}

	served.set(oneSeatDirectory{whole: &atomic.Int64{}, err: errors.New("the directory is unreachable")})
	if e.turnReadsAtlas(t.Context(), c, "sre", turnTelemetry{requester: "priya"}) {
		t.Error("a turn whose requester could not be read was shown the atlas")
	}
}
