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

// A turn's prompt names the atlas only where whoever woke it may read it
// (ADR-0109, ADR-0127): a turn nobody woke, yes; one a person woke, only if
// they hold the atlas; one a stranger woke, never; an unreadable directory
// shows nothing. Mutation: decide without the turn's ceiling, and the person
// without the atlas reads it through the agent they woke.
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
