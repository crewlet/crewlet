package config

import (
	"reflect"
	"strings"

	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/secrets"
)

// Redacted is what a masked credential reads as on every HTTP surface.
//
// A distinctive literal rather than an empty string, because the two mean
// opposite things in this document: an operator who deliberately stored an
// empty credential has said something, and a read that erased the difference
// would let a round trip turn "no credential" into "the credential I could
// not see". It is also what [RestoreRedacted] looks for, so the marker is a
// contract rather than decoration.
//
// IT IS AN ALIAS, and the value lives in [redact.FieldMask], because
// [secrets.Mask] writes it from below this package. Spelled twice it would
// drift, and the drift is a working credential replaced by twelve characters.
const Redacted = redact.FieldMask

// WHICH FIELDS ARE CREDENTIALS is the `secret:"true"` tag (`secret:"content"`
// for a body read whole), read through [secrets.Field] and nothing else.
//
// A TAG, not a path list: a list is kept by whoever remembers it, so a new
// integrations.newthing.token would be published with nothing failing.
// [TestEveryCredentialFieldIsTagged] fails the build when a credential-looking
// field lacks the tag.
//
// TWO READERS, one predicate: the redaction below, over the company (Tier B,
// the tier a read serves), and the schema generator, which marks every covered
// position in BOTH tiers with [secretKeyword]. A tagged field covers every
// string beneath it; TestSchemaMarksExactlyWhatRedactionMasks holds the two.

// Redact returns a copy of the company with every credential masked.
//
// A COPY: the caller's config is what a running engine reads, and masking in
// place would leave the process holding a company whose credentials are all the
// literal string "__redacted__" — an outage produced by looking at something.
//
// A WHOLE ${VAR} REFERENCE IS NOT MASKED. It names a credential rather than
// being one, it is what an operator edits, and hiding it would make the
// document unreadable for the one purpose this surface exists to serve. The
// value it points at never enters this document at all, since references are
// resolved where a provider is constructed, not at parse. A value that only
// embeds a reference beside literal text is masked; see mask.
func (c *Company) Redact() *Company {
	if c == nil {
		return nil
	}
	out := reflect.New(reflect.TypeOf(*c))
	copyMasking(reflect.ValueOf(*c), out.Elem(), false)
	redacted, _ := out.Interface().(*Company)
	return redacted
}

// RestoreRedacted fills masked credentials in c from the values in prior.
//
// This makes GET-edit-PUT safe: without it a reader who changed one line and
// sent the config back would replace every credential with the mask,
// discovered only when each integration failed to authenticate. Only the
// marker is substituted: a field the caller changed keeps their value, and
// one they cleared stays cleared.
//
// Members that can name themselves are matched by identity, a seat or a unit
// ANYWHERE in the prior document ([documentIdentified]). A mask that cannot
// be matched to exactly one prior member is left standing, and
// [Company.Validate] names the field.
func (c *Company) RestoreRedacted(prior *Company) {
	if c == nil || prior == nil {
		return
	}
	r := restorer{documentWide: indexDocumentWide(reflect.ValueOf(*prior))}
	r.restore(reflect.ValueOf(c).Elem(), reflect.ValueOf(*prior), false)
}

// copyMasking deep-copies src into dst, replacing credential strings with the
// mask. secret says whether the enclosing field was tagged, so a map or slice
// of credentials masks its elements rather than needing a tag per element.
func copyMasking(src, dst reflect.Value, secret bool) {
	switch src.Kind() {
	case reflect.String:
		dst.SetString(mask(src.String(), secret))
	case reflect.Pointer:
		if src.IsNil() {
			return
		}
		p := reflect.New(src.Type().Elem())
		copyMasking(src.Elem(), p.Elem(), secret)
		dst.Set(p)
	case reflect.Struct:
		// THE WHOLE VALUE FIRST, then every exported field over it.
		// Reflection can neither read nor set an unexported field on its
		// own, so walking the exported fields alone left each one at its
		// zero value in the copy. That is not cosmetic: a [Toggle] keeps
		// its state unexported, so every explicit `enabled: false`,
		// `learning_enabled: false` and `shared: false` read as UNSET on
		// every config read, and a GET-edit-PUT round trip re-enabled a
		// disabled schedule without anybody touching it. Copying the
		// struct carries that state; the walk below then replaces every
		// exported field with its own deep, masked copy, so nothing a
		// credential can live in is shared with the original.
		dst.Set(src)
		for i := range src.NumField() {
			field := src.Type().Field(i)
			if !field.IsExported() {
				continue
			}
			copyMasking(src.Field(i), dst.Field(i),
				secret || secrets.Field(field))
		}
	case reflect.Slice:
		if src.IsNil() {
			return
		}
		s := reflect.MakeSlice(src.Type(), src.Len(), src.Len())
		for i := range src.Len() {
			copyMasking(src.Index(i), s.Index(i), secret)
		}
		dst.Set(s)
	case reflect.Array:
		// A fixed-length list is a value, not a reference, so dst is
		// already its own storage and each element is masked in place.
		for i := range src.Len() {
			copyMasking(src.Index(i), dst.Index(i), secret)
		}
	case reflect.Map:
		if src.IsNil() {
			return
		}
		m := reflect.MakeMapWithSize(src.Type(), src.Len())
		for _, key := range src.MapKeys() {
			// A map element is not addressable, so it is built in a
			// temporary and then set.
			element := reflect.New(src.Type().Elem()).Elem()
			copyMasking(src.MapIndex(key), element, secret)
			m.SetMapIndex(key, element)
		}
		dst.Set(m)
	default:
		dst.Set(src)
	}
}

// identified is a collection member that knows WHO it is.
//
// Position is not an identity: a caller who merely REORDERED the roster had
// every seat's credentials resolved against its neighbour's, each agent
// authenticating to its tools as somebody else, with no mask left for
// [Company.UnresolvedMasks] to catch and no error raised (the lengths matched
// and masks are anonymous).
//
// A member that cannot name itself has no identity and stays positional,
// which is why no anonymous list in the document may carry a credential
// (`TestEveryCredentialCarryingCollectionCanNameItself`).
type identified interface{ IdentityKey() string }

// documentIdentified is a member whose identity is unique across the WHOLE
// document rather than within its own list: a seat (its handle) and a unit
// (its key).
//
// Both MOVE: a seat between the root and units, a unit under another. A restore
// matching only within the list the member sits in left every moved member's
// masks standing, so these are matched against one index over the whole prior
// document.
//
// A marker method rather than a type list here, so the property is declared
// where the identity is.
type documentIdentified interface {
	identified
	identityIsDocumentWide()
}

var (
	identifiedType         = reflect.TypeOf((*identified)(nil)).Elem()
	documentIdentifiedType = reflect.TypeOf((*documentIdentified)(nil)).Elem()
)

// restorer walks a config beside its prior version, replacing masked
// credentials with what the prior held.
type restorer struct {
	// documentWide is, per [documentIdentified] member type, every identity
	// the prior document holds exactly once, mapped to that member.
	documentWide map[reflect.Type]map[string]reflect.Value
}

// indexDocumentWide collects every [documentIdentified] member of the prior
// document, at any depth, by identity.
//
// AN IDENTITY THAT IS EMPTY OR NOT UNIQUE IS NOT IN THE INDEX. Two units
// answering to the key "platform" in a prior — an identity shared by two
// members, which an apply admits since the key rule is an admission one, as an
// empty one is shared by every member without one — are two members with no
// identity between them: picking either hands one team's credentials to the
// other, and so does falling back to position. Left out, their masks stay
// standing and validation names the field, which is the one outcome that
// invents nothing.
func indexDocumentWide(prior reflect.Value) map[reflect.Type]map[string]reflect.Value {
	seen := map[reflect.Type]map[string]reflect.Value{}
	ambiguous := map[reflect.Type]map[string]bool{}
	var walk func(v reflect.Value)
	walk = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.Pointer, reflect.Interface:
			if !v.IsNil() {
				walk(v.Elem())
			}
		case reflect.Struct:
			for i := range v.NumField() {
				if v.Type().Field(i).IsExported() {
					walk(v.Field(i))
				}
			}
		case reflect.Slice:
			elem := v.Type().Elem()
			documentWide := elem.Implements(documentIdentifiedType)
			if documentWide && seen[elem] == nil {
				seen[elem], ambiguous[elem] = map[string]reflect.Value{}, map[string]bool{}
			}
			for i := range v.Len() {
				member := v.Index(i)
				if documentWide {
					key := member.Interface().(identified).IdentityKey()
					if _, twice := seen[elem][key]; twice || key == "" {
						ambiguous[elem][key] = true
					}
					seen[elem][key] = member
				}
				walk(member)
			}
		case reflect.Map:
			iter := v.MapRange()
			for iter.Next() {
				walk(iter.Value())
			}
		}
	}
	walk(prior)
	for elem, keys := range ambiguous {
		for key := range keys {
			delete(seen[elem], key)
		}
	}
	return seen
}

// restore replaces masks in target with the values at the same place in
// prior.
//
// An INVALID prior means "nothing corresponds here": a member that is new, was
// renamed, or whose identity is ambiguous. The walk still descends through it,
// restoring nothing of its own, because a new unit can hold seats that are not
// new at all and are matched by their own identity.
func (r *restorer) restore(target, prior reflect.Value, secret bool) {
	if prior.IsValid() && target.Type() != prior.Type() {
		return
	}
	switch target.Kind() {
	case reflect.String:
		if prior.IsValid() && secret && target.String() == Redacted {
			target.SetString(prior.String())
		}
	case reflect.Pointer:
		if target.IsNil() {
			return
		}
		var previous reflect.Value
		if prior.IsValid() && !prior.IsNil() {
			previous = prior.Elem()
		}
		r.restore(target.Elem(), previous, secret)
	case reflect.Struct:
		for i := range target.NumField() {
			field := target.Type().Field(i)
			if !field.IsExported() {
				continue
			}
			var previous reflect.Value
			if prior.IsValid() {
				previous = prior.Field(i)
			}
			r.restore(target.Field(i), previous, secret || secrets.Field(field))
		}
	case reflect.Slice, reflect.Array:
		r.restoreSlice(target, prior, secret)
	case reflect.Map:
		for _, key := range target.MapKeys() {
			var previous reflect.Value
			if prior.IsValid() {
				previous = prior.MapIndex(key)
			}
			// A map element is not addressable, so it is restored in a
			// temporary and set back.
			element := reflect.New(target.Type().Elem()).Elem()
			element.Set(target.MapIndex(key))
			r.restore(element, previous, secret)
			target.SetMapIndex(key, element)
		}
	}
}

// restoreSlice matches each member of a list to its prior value.
//
// Three correspondences, chosen by what the member can say about itself, and
// NEVER a fall back from one to another:
//   - A [documentIdentified] member (a seat, a unit) is matched in the index
//     over the whole prior document, so a move keeps its credentials.
//   - An [identified] member (an MCP server, a sandbox setup step) is matched
//     by identity within the prior list it sits in.
//   - Anything else is matched by position; a list that changed length is
//     refused: every slot's mask stays standing, into [Company.Validate].
//
// A duplicated or empty identity has no unambiguous prior: left unrestored.
func (r *restorer) restoreSlice(target, prior reflect.Value, secret bool) {
	elem := target.Type().Elem()
	var match func(i int) reflect.Value
	switch {
	case elem.Implements(documentIdentifiedType):
		index := r.documentWide[elem]
		match = func(i int) reflect.Value {
			return index[target.Index(i).Interface().(identified).IdentityKey()]
		}
	case elem.Implements(identifiedType):
		byKey := uniqueMembers(prior)
		match = func(i int) reflect.Value {
			return byKey[target.Index(i).Interface().(identified).IdentityKey()]
		}
	default:
		positional := prior.IsValid() && prior.Len() == target.Len()
		match = func(i int) reflect.Value {
			if !positional {
				return reflect.Value{}
			}
			return prior.Index(i)
		}
	}
	for i := range target.Len() {
		r.restore(target.Index(i), match(i), secret)
	}
}

// uniqueMembers indexes one prior list of [identified] members by identity,
// leaving out any identity that is empty or held by more than one member, for
// the reason [indexDocumentWide] gives.
func uniqueMembers(prior reflect.Value) map[string]reflect.Value {
	if !prior.IsValid() {
		return nil
	}
	byKey := make(map[string]reflect.Value, prior.Len())
	ambiguous := map[string]bool{}
	for i := range prior.Len() {
		key := prior.Index(i).Interface().(identified).IdentityKey()
		if _, twice := byKey[key]; twice || key == "" {
			ambiguous[key] = true
		}
		byKey[key] = prior.Index(i)
	}
	for key := range ambiguous {
		delete(byKey, key)
	}
	return byKey
}

// mask hides a literal credential and leaves a reference alone: [secrets.Mask]
// for a value under a credential field, and the value itself anywhere else.
//
// ONLY A WHOLE REFERENCE IS SHOWN, the shared rule's reason. The previous rule
// here showed any value containing "${" and so published exactly the literal
// half of "sk-live-SECRET-${ROTATION}"; a malformed "${line#host=}" is not a
// reference at all by the resolver's own grammar. The names an embedded
// reference carries are not lost to the operator: [References] reads the
// unredacted document and lists every one with its path, and a masked value
// is restored from the prior revision on a write.
func mask(value string, secret bool) string {
	if !secret {
		return value
	}
	return secrets.Mask(value)
}

// UnresolvedMasks lists the credential fields still holding the redaction
// marker, by JSON path.
//
// A document reaches this state when [Company.RestoreRedacted] could not
// resolve a mask — a member that is new or renamed carries no prior value of
// its own, and an identity the prior revision gives to more than one member
// names none of them. Guessing would write one credential into another's
// place. Refusing to guess is right; storing the result silently is
// not. The literal "__redacted__" would be handed to a provider as an API key
// and fail at the first call, hours later, with an authentication error that
// names nothing about where it came from.
func (c *Company) UnresolvedMasks() []Path {
	if c == nil {
		return nil
	}
	var found []Path
	findMasks(reflect.ValueOf(*c), nil, false, &found)
	return found
}

func findMasks(v reflect.Value, path Path, secret bool, found *[]Path) {
	switch v.Kind() {
	case reflect.String:
		if secret && v.String() == Redacted {
			*found = append(*found, path)
		}
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			findMasks(v.Elem(), path, secret, found)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			field := v.Type().Field(i)
			if !field.IsExported() {
				continue
			}
			findMasks(v.Field(i), at(path, jsonName(field)),
				secret || secrets.Field(field), found)
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			findMasks(v.Index(i), idx(path, i), secret, found)
		}
	case reflect.Map:
		for _, mapKey := range v.MapKeys() {
			findMasks(v.MapIndex(mapKey), entry(path, mapKey.String()), secret, found)
		}
	}
}

// jsonName is the field's wire name, so a reported path is one the operator
// can find in their own document rather than a Go identifier.
func jsonName(field reflect.StructField) string {
	tag := field.Tag.Get("json")
	if tag == "" {
		return field.Name
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "" || name == "-" {
		return field.Name
	}
	return name
}
