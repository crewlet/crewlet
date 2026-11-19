package config

import (
	"reflect"
	"strings"
)

// normalize collapses Tier A onto the value the engine will run: every string
// trimmed in place, before any rule reads one.
//
// Validation trimmed to judge while consumers read the field RAW, so the two
// differed by the whitespace: `stream.cluster.advertise: " "` skipped
// validation and reached the broker as " "; `node.labels: {" zone": eu}`
// matched no `zone` selector. ${VAR} values (files, .env lines, shell output)
// keep a trailing newline. A reflection walk, not a field list, so a field is
// covered by being added. Tier A only: its strings ADDRESS things, while Tier B
// prose keeps its author's whitespace. Validators read fields directly; still
// trimming are the exported accessors, [ResolveNodeID] (the environment arrives
// after this pass) and parses of a SUBSTRING (keyring base64, [sameHostCluster]).
func (b *Bootstrap) normalize() error {
	var p problems
	_, collisions := mapStrings(reflect.ValueOf(b), nil, func(_ Path, s string) string {
		return strings.TrimSpace(s)
	})
	// Two keys equal once trimmed are REFUSED, not collapsed: a winner picked by
	// Go's random range order would mean different things per run.
	for _, path := range collisions {
		p.add(path, ErrConflict,
			"two keys here are the same key once the whitespace around them is "+
				"removed, and which one survived would be decided by map order. "+
				"Write the one you meant")
	}
	// An empty type is how a file says "unset" (or a ${VAR} resolved to nothing):
	// set it to the default here once, since readers disagreed (the topology rule
	// took empty as CLUSTERED, the engine as embedded).
	defaults := DefaultBootstrap()
	if b.Stream.Type == "" {
		b.Stream.Type = defaults.Stream.Type
	}
	if b.Coordination.Type == "" {
		b.Coordination.Type = defaults.Coordination.Type
	}
	return p.err()
}

// mapStrings rewrites every string reachable from v, reporting whether any
// changed and the paths where a rewrite collided with an existing map key
// ([mapEntries]).
//
// THE ONE REACHABILITY WALK in this package ([walkStrings] is its read-only
// half): two walks could disagree about a config's fields, silently on both
// sides.
//
// v MUST BE ADDRESSABLE to be rewritten (pass a pointer); a copy traverses but
// its fields cannot be set. A string behind an INTERFACE is read, never
// rewritten (its dynamic value is not addressable), so a test asserts Tier A has
// no interface field rather than leaving the trim to miss one.
func mapStrings(v reflect.Value, path Path, replace func(path Path, s string) string) (bool, []Path) {
	if !v.IsValid() {
		return false, nil
	}
	switch v.Kind() {
	case reflect.String:
		s := v.String()
		out := replace(path, s)
		if out == s {
			return false, nil
		}
		if v.CanSet() {
			v.SetString(out)
		}
		return true, nil
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return false, nil
		}
		return mapStrings(v.Elem(), path, replace)
	case reflect.Slice, reflect.Array:
		changed, collisions := false, []Path(nil)
		for i := range v.Len() {
			hit, collided := mapStrings(v.Index(i), idx(path, i), replace)
			changed = hit || changed
			collisions = append(collisions, collided...)
		}
		return changed, collisions
	case reflect.Map:
		return mapEntries(v, path, replace)
	case reflect.Struct:
		// A yaml.Node's own Value carries the scalar text; its Content
		// carries the children. Walking the struct generically reaches
		// both, so there is no special case to keep in sync.
		//
		// Unexported fields are skipped: reflection cannot read them, and
		// nothing in a config payload hides a string behind one — the one
		// type with unexported state (org.Toggle) holds none.
		changed, collisions := false, []Path(nil)
		t := v.Type()
		for i := range v.NumField() {
			if t.Field(i).PkgPath != "" {
				continue // unexported
			}
			hit, collided := mapStrings(v.Field(i), at(path, jsonName(t.Field(i))), replace)
			changed = hit || changed
			collisions = append(collisions, collided...)
		}
		return changed, collisions
	}
	return false, nil
}

// mapEntries rewrites a map's keys and values, for keys too: a `node.labels` key
// is matched EXACTLY by a placement selector, so a stray space matches nothing.
// An entry is reported at its OWN path, where the operator finds the text.
//
// SEPARATE FROM THE WALK because entries are not addressable: ranging yields
// copies, so rewrites are collected and applied after the range (a rewritten
// KEY is an add, undefined mid-range).
//
// A rewritten key landing on an existing entry or another rewrite's target is
// REPORTED, not applied (`{"zone": a, "zone ": b}` has no answer; Go's range
// order would pick one). All collisions are found before any rewrite lands and
// one abandons the whole map, leaving both entries intact in the operator's file.
func mapEntries(v reflect.Value, path Path, replace func(path Path, s string) string) (bool, []Path) {
	type rewrite struct{ old, key, val reflect.Value }
	var pending []rewrite
	var collisions []Path

	iter := v.MapRange()
	for iter.Next() {
		// THE KEY IS THE OPERATOR'S OWN TEXT, so it extends the path as
		// one segment through [entry] rather than through [at]: a
		// `node.labels` key or an `mcp_env` variable may itself hold a
		// dot, and splitting it would report the collision at a place
		// nobody can find in their file.
		keyPath := entry(path, iter.Key().String())

		key := reflect.New(v.Type().Key()).Elem()
		key.Set(iter.Key())
		keyChanged, keyCollided := mapStrings(key, keyPath, replace)

		val := reflect.New(v.Type().Elem()).Elem()
		val.Set(iter.Value())
		valChanged, valCollided := mapStrings(val, keyPath, replace)

		collisions = append(collisions, keyCollided...)
		collisions = append(collisions, valCollided...)
		if !keyChanged && !valChanged {
			continue
		}
		pending = append(pending, rewrite{old: iter.Key(), key: key, val: val})
	}

	// EVERY collision is found before ANY rewrite, so the outcome does not
	// depend on range order: applied as found, `{"zone ": a, "zone  ": b}` would
	// rename whichever came first. A target already IN the map is always a real
	// collision: a pending entry's own key is one the trim CHANGED, a target is
	// already trimmed.
	// `own` is THIS map's collisions, apart from nested maps' reported above: those
	// refuse the config but are no reason to abandon a well-defined rewrite here.
	var own []Path
	taken := make(map[any]bool, len(pending))
	for _, r := range pending {
		target := r.key.Interface()
		if target == r.old.Interface() {
			continue // the value moved, not the key
		}
		if taken[target] || v.MapIndex(r.key).IsValid() {
			own = append(own, path)
		}
		taken[target] = true
	}
	if len(own) > 0 {
		// NOTHING WAS WRITTEN, so nothing changed — the map is reported
		// exactly as the operator wrote it, which is what the refusal
		// above it describes.
		return false, append(collisions, own...)
	}

	for _, r := range pending {
		if r.key.Interface() != r.old.Interface() {
			v.SetMapIndex(r.old, reflect.Value{})
		}
		v.SetMapIndex(r.key, r.val)
	}
	return len(pending) > 0, collisions
}
