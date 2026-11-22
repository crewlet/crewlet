package orgchart

import (
	"testing"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/org"
)

func init() {
	growth.RegisterFunc("chartgrowth", func() growth.Catalog {
		return growth.Catalog{Kind: "chartgrowth", API: "Chart Growth API", Version: "v1"}
	})
}

// AN AGENT CARRIES A GROWTH GRANT AT ITS LANE'S LEVEL OR BELOW: a seat at
// operate carries read and operate and never administer, a seat whose lane is
// off carries none, and a person carries none whatever their document says.
//
// Mutation: compare levels by equality, and the operate seat cannot read.
func TestAnAgentCarriesAGrowthGrantAtItsLevelOrBelow(t *testing.T) {
	t.Parallel()
	agent := &org.Role{Kind: org.KindAgent, GrowthLevels: map[string]string{"chartgrowth": "operate"}}
	human := &org.Role{Kind: org.KindHuman, GrowthLevels: map[string]string{"chartgrowth": "administer"}}
	off := &org.Role{Kind: org.KindAgent}
	grant := func(l growth.Level) authz.SeatGrant { return authz.GrowthSeatGrant("chartgrowth", l) }
	for _, c := range []struct {
		name string
		role *org.Role
		g    authz.SeatGrant
		want bool
	}{
		{"operate reads", agent, grant(growth.LevelRead), true},
		{"operate operates", agent, grant(growth.LevelOperate), true},
		{"operate does not administer", agent, grant(growth.LevelAdminister), false},
		{"a person", human, grant(growth.LevelRead), false},
		{"a lane off", off, grant(growth.LevelRead), false},
		{"an unknown product", agent, authz.SeatGrant("nosuch_read"), false},
	} {
		if got := carries(c.role, c.g); got != c.want {
			t.Errorf("%s: carries = %v, want %v", c.name, got, c.want)
		}
	}
}
