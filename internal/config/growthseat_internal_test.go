package config

import (
	"reflect"
	"slices"
	"testing"
)

// EVERY GROWTH SEAT BLOCK JOINS SEAT ACCESS BY ITS KEY ALONE: written through
// SetGrowthSeatAccess it reads back whole, it is a lane that needs a scope, its
// `enabled` is where seat access flips it, its level reaches the chart, and a
// company block of the same key is its product's.
func TestEveryGrowthSeatBlockJoinsSeatAccessByItsKey(t *testing.T) {
	t.Parallel()
	if len(growthSeatKeys()) == 0 {
		t.Fatal("no growth seat block was found, so this sweep holds nothing")
	}
	for _, kind := range growthSeatKeys() {
		on := true
		want := GrowthSeatAccess{On: &on, Level: "operate", Scope: []string{"1"}}
		var r Role
		if _, ok := r.GrowthSeatAccess(kind); ok {
			t.Errorf("%s: a seat with no block reads as holding one", kind)
		}
		if !r.SetGrowthSeatAccess(kind, want) {
			t.Fatalf("%s: the seat block was not written", kind)
		}
		got, ok := r.GrowthSeatAccess(kind)
		if !ok || got.On != &on || got.Level != want.Level || !slices.Equal(got.Scope, want.Scope) {
			t.Errorf("%s: read back %+v, want %+v", kind, got, want)
		}
		if key, scope := r.SeatScope(kind); key == "" || !slices.Equal(scope, want.Scope) {
			t.Errorf("%s: the scope reads as %q %v", kind, key, scope)
		}
		if !slices.Contains(SeatAccessLanes(), kind) || !SeatScopeRequired(kind) {
			t.Errorf("%s: not a seat access lane that needs a scope", kind)
		}
		if f := r.seatEnabledField(kind); f == nil || *f != &on {
			t.Errorf("%s: seat access cannot reach the block's enabled", kind)
		}
		if r.Integrations.IsZero() || r.GrowthLevels()[kind] != "operate" {
			t.Errorf("%s: the block drops out of a round trip or the chart", kind)
		}
		if !slices.Contains(growthBlockKeys(), kind) {
			t.Errorf("%s: no company growth block has this key", kind)
		}
	}
	var r Role
	if r.SetGrowthSeatAccess("slack", GrowthSeatAccess{}) || r.Integrations.Slack != nil {
		t.Error("a lane that is no growth product was written as one")
	}
}

// A GROWTH BLOCK'S KEY IS ITS KIND, which every lookup by kind relies on, and
// a company declares the product exactly where it holds the block.
func TestEveryGrowthBlockIsKeyedByItsKind(t *testing.T) {
	t.Parallel()
	block := reflect.TypeFor[growthBlock]()
	for f := range reflect.TypeFor[Integrations]().Fields() {
		if !f.Type.Implements(block) {
			continue
		}
		made := reflect.New(f.Type.Elem())
		if kind := made.Interface().(growthBlock).GrowthKind(); kind != jsonName(f) {
			t.Errorf("integrations.%s holds a block of kind %q", jsonName(f), kind)
		}
		c := &Company{Name: "Acme"}
		if c.DeclaresIntegration(jsonName(f)) {
			t.Errorf("%s: a company without the block declares it", jsonName(f))
		}
		reflect.ValueOf(&c.Integrations).Elem().FieldByIndex(f.Index).Set(made)
		if !c.DeclaresIntegration(jsonName(f)) {
			t.Errorf("%s: a company holding the block does not declare it", jsonName(f))
		}
	}
}
