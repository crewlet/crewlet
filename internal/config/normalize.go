package config

import (
	"reflect"
	"strings"
)

// normalize collapses Tier A onto the value the engine will actually run:
// every string trimmed of the whitespace around it, in place, before any
// rule reads one.
//
// # Why it exists
//
// Because validation and the runtime were reading two different values.
// Every rule in this file that cares about a string's CONTENT already
// trimmed one — an emptiness check on `stream.url`, an address parse on
// `stream.cluster.advertise`, a key check over `node.labels` — while every
// consumer downstream read the field RAW. So the validated value and the
// used value differed by exactly the whitespace, and each field failed its
// own way:
//
//   - `stream.cluster.advertise: " "` trims to empty, skips validation
//     ENTIRELY, and reaches server.ClusterOpts.Advertise as " ". The broker
//     validates an advertise address while starting, logs it and shuts
//     down — a node that boots, dies, and leaves an operator reading broker
//     logs for a typo in their own config file, which is the one outcome
//     [validateAdvertise] exists to prevent.
//   - `node.labels: {" zone": eu}` passes the non-empty key check and then
//     matches no `zone` selector any seat could write, because
//     [Node.Profile] copies the key through verbatim.
//   - `store.path` with a trailing newline is validated as one path and
//     opened as another.
//
// # Why whitespace arrives at all
//
// ${VAR} references resolve BEFORE this, in [ParseBootstrap], and what they
// carry comes from files, from `--from-file` secrets, from a .env line,
// from a shell that captured a command's output — every one of which
// routinely keeps a trailing newline. Naming the broker's address as
// `${NATS_URL}` is the shape the docs recommend, and it is the shape that
// breaks.
//
// # Why a reflection walk rather than a list of fields
//
// The same reason [StreamCluster]'s own "is anything else set" question is
// asked through IsZero: a list names the fields somebody thought of, and a
// field added later sits silently outside it. Tier A holds thirty-odd
// strings across nine blocks, and the walk reaches whichever ones the
// struct has — so a field is covered by having been added to it.
//
// # Why Tier A and not Tier B
//
// Every string here ADDRESSES something: an id, a path, a URL, a host, a
// port, a token, a label a placement selector matches exactly. None of them
// is prose. Tier B carries the company's prompts, personas and skill text,
// where the whitespace around a fragment belongs to whoever wrote it rather
// than to the transport that delivered it.
//
// # What still trims after this, and why
//
// Every rule under [Bootstrap.Validate] now reads its field directly: the
// pair (validated value, used value) is one value, and a second TrimSpace
// inside a validator would be the very drift this removes, written in the
// one place it cannot be seen. Three kinds of caller keep theirs, because
// none of them is downstream of this pass:
//
//   - The EXPORTED accessors — [Stream.SyncAlways], [Stream.SyncInterval],
//     [Retention.IsZero] — which a caller may hold over a value nothing
//     validated, and which have to be correct on their own.
//   - [ResolveNodeID], which trims what the ENVIRONMENT answered. That
//     value arrives after this pass and never passes through it, and
//     $CREWLET_NODE_ID read from a file is the trailing newline this whole
//     doc is about.
//   - The two parses that trim a SUBSTRING rather than a field: the
//     keyring's base64 and [sameHostCluster]'s per-peer host. What they
//     cut is inside the value, not around it.
//
// # The one thing it refuses rather than decides
//
// Two map keys that are the same key once trimmed. Collapsing them would
// pick a winner by Go's random range order, and a config whose meaning
// changes between two runs of the same binary is the failure this package
// exists to prevent, not one it may introduce.
func (b *Bootstrap) normalize() error {
	var p problems
	_, collisions := mapStrings(reflect.ValueOf(b), nil, func(_ Path, s string) string {
		return strings.TrimSpace(s)
	})
	for _, path := range collisions {
		p.add(path, ErrConflict,
			"two keys here are the same key once the whitespace around them is "+
				"removed, and which one survived would be decided by map order. "+
				"Write the one you meant")
	}
	return p.err()
}

// mapStrings rewrites every string reachable from v.
//
// It reports whether any of them changed, and the paths at which a rewrite
// collided with a key the map already had — see [mapEntries].
//
// THE ONE REACHABILITY WALK in this package — [walkStrings] is the read-only
// half of it — for the reason its doc gives: two walks are two chances to
// disagree about which fields a config payload actually has, and a
// disagreement here is silent on both sides. A field the reference index
// cannot see is a ${VAR} nothing reports; a field the trim cannot see keeps
// the drift [Bootstrap.normalize] describes.
//
// v MUST BE ADDRESSABLE to be rewritten — pass a pointer. A value handed in
// by copy still traverses, but a struct field of it cannot be set, so the
// only caller that rewrites anything passes &cfg. Map ENTRIES are the
// exception in both directions and are handled by [mapEntries].
//
// A string behind an INTERFACE is read but never rewritten: the dynamic
// value an interface holds is not addressable, and reseating it would mean
// rebuilding whatever holds the interface. That costs the read-only caller
// nothing — it walks the map[string]any a store row decodes to — but it
// would make a Tier A field of interface type silently unreachable by the
// trim, so a test asserts Tier A has none rather than leaving it to be
// remembered.
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

// mapEntries rewrites a map's keys and values.
//
// SEPARATE FROM THE WALK because a map entry is not addressable: the range
// hands back copies of both halves, so the only way to change either is to
// write the entry again. Rewrites are collected first and applied after the
// range, because adding a key to a map being ranged over is undefined — and
// a rewritten KEY is an add, not an update.
//
// Keys as well as values, for the reason the reference index gives for
// reading them: a key is text an operator typed, and a `node.labels` key is
// matched EXACTLY by a seat's placement selector, so one carrying a stray
// space matches nothing any seat could write. An entry is reported at its
// OWN path rather than the map's, because that is where the operator finds
// the text.
//
// A rewritten key that lands on an entry the map already has, or on
// another rewrite's target, is REPORTED rather than applied: `{"zone": a,
// "zone ": b}` has no answer this can pick, since which survived would
// follow Go's randomized range order. Every collision is found before any
// rewrite lands, and finding one abandons the whole map — so it is left
// exactly as it was, both entries intact, and the caller refusing it is
// describing something the operator can still see in their own file.
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

	// EVERY collision is found before ANY rewrite is applied, so the
	// outcome does not depend on the order the range happened to produce.
	// Applied as they were found, `{"zone ": a, "zone  ": b}` would rename
	// whichever came first and report the second — leaving a map that
	// differs between two runs of one binary, which is the failure this is
	// here to refuse rather than to relocate.
	//
	// A target already IN the map is always a real collision: it can only
	// be an entry that is not itself pending, since a pending entry's own
	// key is by definition one the trim CHANGED, and a target is already
	// trimmed.
	// THIS map's own collisions, kept apart from the ones a nested map
	// reported through the loop above: a collision two levels down refuses
	// the config either way, but it is not a reason to abandon a rewrite
	// here that is perfectly well defined.
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
