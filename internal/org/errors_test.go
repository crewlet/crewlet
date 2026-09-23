package org

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// leaves flattens a joined error into its leaves.
func leaves(err error) []error {
	if err == nil {
		return nil
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return []error{err}
	}
	var out []error
	for _, inner := range joined.Unwrap() {
		out = append(out, leaves(inner)...)
	}
	return out
}

// located is one leaf reduced to what a caller placing it needs: the entity
// it names, by pointer, and the field inside it.
type located struct {
	seat  *Role
	unit  *Unit
	field []any
}

func locate(t *testing.T, err, sentinel error) located {
	t.Helper()
	var found []located
	for _, leaf := range leaves(err) {
		if !errors.Is(leaf, sentinel) {
			continue
		}
		var seatErr *SeatError
		var unitErr *UnitError
		switch {
		case errors.As(leaf, &seatErr):
			if seatErr.Error() != seatErr.Err.Error() {
				t.Errorf("SeatError renders %q, want the wrapped text %q", seatErr.Error(), seatErr.Err.Error())
			}
			found = append(found, located{seat: seatErr.Seat, field: seatErr.Field})
		case errors.As(leaf, &unitErr):
			if unitErr.Error() != unitErr.Err.Error() {
				t.Errorf("UnitError renders %q, want the wrapped text %q", unitErr.Error(), unitErr.Err.Error())
			}
			found = append(found, located{unit: unitErr.Unit, field: unitErr.Field})
		default:
			t.Fatalf("%v is neither a SeatError nor a UnitError", leaf)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d errors wrap %v in %v, want exactly one", len(found), sentinel, err)
	}
	return found[0]
}

// EVERY RULE A SEAT BREAKS NAMES THE SEAT AND THE FIELD. The config layer puts
// a problem back where it was written by the seat's pointer, because the
// mistakes a person makes while building a chart (a blank name, a shared name,
// a name that slugifies to nothing) are exactly the ones a name cannot tell
// apart; and a form marks the input a problem is about by its field.
func TestASeatErrorCarriesItsSeatAndField(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		seat     *Role
		sentinel error
		field    []any
	}{
		{"missing name", &Role{Name: " "}, ErrMissingName, []any{"name"}},
		{"unknown kind", &Role{Name: "Dev", Kind: "robot"}, ErrUnknownKind, []any{"kind"}},
		{"invalid handle", &Role{Name: "Dev", DeclaredHandle: "Dev!"}, ErrInvalidHandle, []any{"handle"}},
		{"a name that yields no handle", &Role{Name: "!!!"}, ErrInvalidHandle, []any{"name"}},
		{"one agent-only field on a person", human(func(r *Role) {
			r.Slack = SlackIdentity{BotToken: "xoxb-1"}
		}), ErrHumanSeatField, []any{"integrations", "slack"}},
		{"several agent-only fields on a person", human(func(r *Role) {
			r.TokenBudget = 10
			r.Workers = []string{"researcher"}
		}), ErrHumanSeatField, nil},
		{"a human-only field on an agent", &Role{Name: "Dev", Availability: "mornings"},
			ErrAgentSeatField, []any{"availability"}},
		{"an embedded reference", human(func(r *Role) {
			r.Contact = &HumanContact{SlackUserID: "U${SUFFIX}"}
		}), ErrEmbeddedEnvRef, []any{"contact", "slack_user_id"}},
		{"a schedule field", &Role{Name: "Dev", Schedules: []Schedule{
			{Name: "standup", Cron: "0 9 * * *", Task: "post"},
			{Name: "report", Cron: "0 9 * *", Task: "post"},
		}}, ErrInvalidSchedule, []any{"schedules", 1, "cron"}},
		{"a duplicate schedule name", &Role{Name: "Dev", Schedules: []Schedule{
			{Name: "standup", Cron: "0 9 * * *", Task: "post"},
			{Name: "standup", Cron: "0 10 * * *", Task: "post"},
		}}, ErrInvalidSchedule, []any{"schedules"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := locate(t, tc.seat.Validate(), tc.sentinel)
			if got.seat != tc.seat {
				t.Errorf("the error names seat %p, want %p", got.seat, tc.seat)
			}
			if !reflect.DeepEqual(got.field, tc.field) {
				t.Errorf("field = %#v, want %#v", got.field, tc.field)
			}
		})
	}
}

// EVERY RULE A UNIT BREAKS NAMES THE UNIT AND THE FIELD, the lead-targeted
// schedule the organization checks included.
func TestAUnitErrorCarriesItsUnitAndField(t *testing.T) {
	t.Parallel()
	nameless := &Unit{Name: ""}
	fanOut := &Unit{Name: "Humans", Roles: []*Role{human()}, Schedules: []Schedule{
		{Name: "standup", Cron: "0 9 * * *", Task: "post"},
	}}
	ledByAPerson := &Unit{Name: "Design", Lead: "sarah-chen", Roles: []*Role{human(), {Name: "Dev"}},
		Schedules: []Schedule{
			{Name: "digest", Cron: "0 9 * * *", Task: "post", Target: TargetEach},
			{Name: "roundup", Cron: "0 17 * * 5", Task: "post", Target: TargetLead},
		}}
	for _, tc := range []struct {
		name     string
		err      error
		unit     *Unit
		sentinel error
		field    []any
	}{
		{"missing name", nameless.Validate(), nameless, ErrMissingName, []any{"name"}},
		{"a fan-out with no agent", fanOut.Validate(), fanOut, ErrUnrunnableSchedule, []any{"schedules", 0}},
		{"a lead schedule on a human lead",
			normalized(&Organization{Name: "T", Units: []*Unit{ledByAPerson}}).Validate(),
			ledByAPerson, ErrUnrunnableSchedule, []any{"schedules", 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := locate(t, tc.err, tc.sentinel)
			if got.unit != tc.unit {
				t.Errorf("the error names unit %p, want %p", got.unit, tc.unit)
			}
			if !reflect.DeepEqual(got.field, tc.field) {
				t.Errorf("field = %#v, want %#v", got.field, tc.field)
			}
		})
	}
}

// A DUPLICATE CARRIES EVERY ENTITY THAT SHARES THE KEY, in the order they were
// met, so a caller can put a problem beside each one rather than beside the
// first. Three seats on one name are one error holding three seats.
func TestADuplicateErrorCarriesEveryEntity(t *testing.T) {
	t.Parallel()
	a, b, c := &Role{Name: "Engineer", DeclaredHandle: "a"}, &Role{Name: "Engineer", DeclaredHandle: "b"},
		&Role{Name: "Engineer", DeclaredHandle: "c"}
	x, y := &Role{Name: "Dev"}, &Role{Name: "dev", DeclaredHandle: "dev"}
	platformA, platformB := &Unit{Name: "Platform"}, &Unit{Name: "Platform"}
	o := normalized(&Organization{Name: "T", Roles: []*Role{a, x}, Units: []*Unit{
		{Name: "Engineering", Roles: []*Role{b, y}, Children: []*Unit{platformA}},
		{Name: "Product", Roles: []*Role{c}, Children: []*Unit{platformB}},
	}})

	for _, tc := range []struct {
		name  string
		err   error
		kind  DuplicateKind
		key   string
		seats []*Role
		units []*Unit
	}{
		{"handle", o.Validate(), DuplicateHandle, "dev", []*Role{x, y}, nil},
		{"seat name", o.ValidateAdmission(), DuplicateSeatName, "Engineer", []*Role{a, b, c}, nil},
		{"unit name", o.ValidateAdmission(), DuplicateUnitName, "Platform", nil, []*Unit{platformA, platformB}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var found []*DuplicateError
			for _, leaf := range leaves(tc.err) {
				var dup *DuplicateError
				if errors.As(leaf, &dup) && dup.Kind == tc.kind {
					found = append(found, dup)
				}
			}
			if len(found) != 1 {
				t.Fatalf("%d %s duplicates in %v, want one", len(found), tc.kind, tc.err)
			}
			dup := found[0]
			if !dup.Kind.Valid() || dup.Key != tc.key {
				t.Errorf("kind %q key %q, want %q %q", dup.Kind, dup.Key, tc.kind, tc.key)
			}
			if !reflect.DeepEqual(dup.Seats, tc.seats) || !reflect.DeepEqual(dup.Units, tc.units) {
				t.Errorf("entities = %v %v, want %v %v", dup.Seats, dup.Units, tc.seats, tc.units)
			}
			if dup.Error() != dup.Err.Error() {
				t.Errorf("DuplicateError renders %q, want the grouped message", dup.Error())
			}
		})
	}
}

// A THIRD SEAT ON ONE ACCOUNT IS STILL ONE MISTAKE.
//
// The pairwise reading is the one to guard against, because it looks right
// until a third seat arrives: three seats on one Slack id are three PAIRS, so
// a validator comparing them two at a time reports two problems, and NEITHER
// message names all three. Whoever reads it edits one line, revalidates, and
// meets the collision again — and [internal/config] can only place a problem
// beside the seats the error names, so the third seat gets no marker at all.
//
// It is also the case the table above cannot reach: every other duplicate
// kind is keyed on a name or a handle, and only an identity is keyed on one
// value of one contact FIELD, which is what the grouping has to be per.
func TestThreeSeatsOnOneAccountAreOneMessageNamingEveryOne(t *testing.T) {
	t.Parallel()
	ada := &Role{Name: "Ada", Kind: KindHuman,
		Contact: &HumanContact{SlackUserID: "U0FOUNDER"}}
	rui := &Role{Name: "Rui", Kind: KindHuman,
		Contact: &HumanContact{SlackUserID: "U0FOUNDER"}}
	mei := &Role{Name: "Mei", Kind: KindHuman,
		Contact: &HumanContact{SlackUserID: "U0FOUNDER"}}
	o := normalized(&Organization{Name: "T", Roles: []*Role{ada}, Units: []*Unit{
		{Name: "Engineering", Roles: []*Role{rui}},
		{Name: "Product", Roles: []*Role{mei}},
	}})

	var found []*DuplicateError
	err := o.Validate()
	for _, leaf := range leaves(err) {
		var dup *DuplicateError
		if errors.As(leaf, &dup) && dup.Kind == DuplicateIdentity {
			found = append(found, dup)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d identity duplicates in %v, want ONE naming all three "+
			"rather than one per pair", len(found), err)
	}
	dup := found[0]
	if !dup.Kind.Valid() || dup.Key != "U0FOUNDER" {
		t.Errorf("kind %q key %q, want %q %q", dup.Kind, dup.Key,
			DuplicateIdentity, "U0FOUNDER")
	}
	// IN THE ORDER MET, walking the chart, so the problems a caller places
	// land in document order rather than in map order.
	if want := []*Role{ada, rui, mei}; !reflect.DeepEqual(dup.Seats, want) {
		t.Errorf("seats = %v, want %v", roleNames(dup.Seats), roleNames(want))
	}
	for _, name := range []string{"Ada", "Rui", "Mei"} {
		if !strings.Contains(dup.Error(), name) {
			t.Errorf("the message %q does not name %s, so that seat is the "+
				"one nobody is told to edit", dup.Error(), name)
		}
	}
}
