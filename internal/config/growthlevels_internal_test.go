package config

import (
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/authz/orgchart"
	"github.com/crewlet/crewlet/internal/growth"
)

func init() {
	growth.RegisterFunc("seatads", func() growth.Catalog {
		return growth.Catalog{Kind: "seatads", API: "Seat Ads API", Version: "v1"}
	})
}

// fakeSeat is a growth seat block as a product's would be, written under a
// key that is not its kind.
type fakeSeat struct {
	On    bool
	Level string
}

func (f *fakeSeat) GrowthKind() string              { return "seatads" }
func (f *fakeSeat) SeatAccess() GrowthSeatAccess    { return GrowthSeatAccess{} }
func (f *fakeSeat) SetSeatAccess(GrowthSeatAccess)  {}
func (f *fakeSeat) seatScopeKey() string            { return "accounts" }
func (f *fakeSeat) seatEnabled() **bool             { return nil }
func (f *fakeSeat) check(*Company, *problems, Path) {}
func (f *fakeSeat) GrowthSeatLevel() string {
	if !f.On {
		return ""
	}
	return f.Level
}

// A SEAT'S GROWTH LEVEL REACHES THE CHART UNDER ITS PRODUCT'S KIND, whatever
// key its block is written under, so the grants the authority asks for
// (`seatads_read`) find it: a seat on at operate carries read and operate,
// one whose block is off or absent carries none.
//
// Mutation: key the levels by the field's JSON name, and no seat carries
// anything.
func TestASeatsGrowthLevelReachesTheChartUnderItsKind(t *testing.T) {
	t.Parallel()
	type blocks struct {
		Ads *fakeSeat `json:"seat_ads"`
	}
	for _, c := range []struct {
		name    string
		blocks  blocks
		levels  map[string]string
		carried []growth.Level
	}{
		{"on at operate", blocks{&fakeSeat{On: true, Level: "operate"}}, map[string]string{"seatads": "operate"},
			[]growth.Level{growth.LevelRead, growth.LevelOperate}},
		{"off", blocks{&fakeSeat{Level: "administer"}}, nil, nil},
		{"absent", blocks{}, nil, nil},
	} {
		levels := growthLevelsIn(&c.blocks)
		if !maps.Equal(levels, c.levels) {
			t.Errorf("%s: levels %v, want %v", c.name, levels, c.levels)
		}
		o, err := mustCompany(t, "name: Acme\nroles:\n  - name: Analyst\n").Organization()
		if err != nil {
			t.Fatal(err)
		}
		seat := o.AgentSeatByHandle("analyst")
		if seat == nil {
			t.Fatal("the company holds no agent seat analyst")
		}
		seat.GrowthLevels = levels
		var carried []growth.Level
		for _, l := range growth.Levels() {
			if slices.Contains(orgchart.GrantedSeats(o, authz.GrowthSeatGrant("seatads", l)), "analyst") {
				carried = append(carried, l)
			}
		}
		if !slices.Equal(carried, c.carried) {
			t.Errorf("%s: the seat carries %v, want %v", c.name, carried, c.carried)
		}
	}
}

// partialSeat has a seat level and names no kind.
type partialSeat struct{}

func (p *partialSeat) GrowthSeatLevel() string { return "read" }

// EVERY GROWTH SEAT BLOCK IS A WHOLE ONE: a block with a seat level that names
// no kind is not a growth seat to the walks, so its seats would carry no grant
// and nothing would say so. The control is a struct holding such a block.
func TestEveryGrowthSeatBlockIsAWholeOne(t *testing.T) {
	t.Parallel()
	if partial := partialGrowthSeats(reflect.TypeFor[RoleIntegrations]()); len(partial) != 0 {
		t.Errorf("roles[].integrations.%v have a seat level but are not growth seat blocks: give each "+
			"GrowthKind", partial)
	}
	type blocks struct {
		Whole   *fakeSeat    `json:"whole"`
		Partial *partialSeat `json:"partial"`
	}
	if got := partialGrowthSeats(reflect.TypeFor[blocks]()); !slices.Equal(got, []string{"partial"}) {
		t.Errorf("the partial blocks found are %v, want [partial]", got)
	}
}
