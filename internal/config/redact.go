package config

import (
	"reflect"
	"strings"
)

// Redacted is what a masked credential reads as on every HTTP surface.
//
// A distinctive literal rather than an empty string, because the two mean
// opposite things in this document: an operator who deliberately stored an
// empty credential has said something, and a read that erased the difference
// would let a round trip turn "no credential" into "the credential I could
// not see". It is also what [RestoreRedacted] looks for, so the marker is a
// contract rather than decoration.
const Redacted = "__redacted__"

// secretTag marks a field that holds a credential.
//
// A TAG rather than a list of paths, and that is the whole point: a path list
// is maintained by whoever remembers it exists, so the day somebody adds
// integrations.newthing.token the config surface starts publishing it and
// nothing fails. A tag lives on the field, and [TestEveryCredentialFieldIsTagged]
// fails the build when a field that looks like a credential does not carry one.
const secretTag = "secret"

// Redact returns a copy of the company with every credential masked.
//
// A COPY: the caller's config is what a running engine reads, and masking in
// place would leave the process holding a company whose credentials are all the
// literal string "__redacted__" — an outage produced by looking at something.
//
// A ${VAR} REFERENCE IS NOT MASKED. It names a credential rather than being
// one, it is what an operator edits, and hiding it would make the document
// unreadable for the one purpose this surface exists to serve. The value it
// points at never enters this document at all — references are resolved where
// a provider is constructed, not at parse.
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
// This is what makes GET-edit-PUT safe. Without it a reader who fetched the
// config, changed one line and sent it back would replace every credential in
// the company with the mask — silently, and only discovered when each
// integration started failing to authenticate. The cheap answer is to document
// that the read is not round-trippable; a document that cannot be sent back is
// a document nobody can edit.
//
// Only the marker is substituted. A field the caller actually changed keeps
// their value, and a field they cleared stays cleared.
func (c *Company) RestoreRedacted(prior *Company) {
	if c == nil || prior == nil {
		return
	}
	restore(reflect.ValueOf(c).Elem(), reflect.ValueOf(*prior), false)
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
		for i := range src.NumField() {
			field := src.Type().Field(i)
			if !field.IsExported() {
				continue
			}
			copyMasking(src.Field(i), dst.Field(i),
				secret || field.Tag.Get(secretTag) == "true")
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

// restore walks a config beside its prior version, replacing masked
// credentials with what the prior held.
func restore(target, prior reflect.Value, secret bool) {
	if target.Type() != prior.Type() {
		return
	}
	switch target.Kind() {
	case reflect.String:
		if secret && target.String() == Redacted {
			target.SetString(prior.String())
		}
	case reflect.Pointer:
		if !target.IsNil() && !prior.IsNil() {
			restore(target.Elem(), prior.Elem(), secret)
		}
	case reflect.Struct:
		for i := range target.NumField() {
			field := target.Type().Field(i)
			if !field.IsExported() {
				continue
			}
			restore(target.Field(i), prior.Field(i),
				secret || field.Tag.Get(secretTag) == "true")
		}
	case reflect.Slice:
		// BY IDENTITY where the members have one, exactly as the map
		// branch below matches on its key: a seat's credentials belong to
		// the seat, not to the slot it happened to occupy.
		if restoreByIdentity(target, prior, secret) {
			return
		}
		// By POSITION otherwise, which is the only correspondence a list
		// of anonymous values has — a reordered `api_keys` is genuinely
		// ambiguous, so the mask is refused rather than guessed when the
		// lengths differ, and the validation that follows reports a
		// literal "__redacted__" where a credential should be.
		if target.Len() != prior.Len() {
			return
		}
		for i := range target.Len() {
			restore(target.Index(i), prior.Index(i), secret)
		}
	case reflect.Map:
		for _, key := range target.MapKeys() {
			previous := prior.MapIndex(key)
			if !previous.IsValid() {
				continue
			}
			element := reflect.New(target.Type().Elem()).Elem()
			element.Set(target.MapIndex(key))
			restore(element, previous, secret)
			target.SetMapIndex(key, element)
		}
	}
}

// identified is a collection member that knows WHO it is.
//
// Position is not an identity, and treating it as one is how a caller who
// merely REORDERED the roster had every seat's credentials resolved against
// its neighbour's — each agent then authenticating to its tools as somebody
// else, with nothing left holding the marker for [Company.UnresolvedMasks] to
// catch and no validation error to raise. The lengths matched, so the guard
// below never fired; the masks are anonymous, so the wrong answer looked
// exactly like the right one.
//
// A list of credentials genuinely has no identity — a reordered `api_keys`
// cannot be matched to what it hid — and stays positional. What separates the
// two is whether a member can name itself, so the member is what says.
type identified interface{ IdentityKey() string }

var identifiedType = reflect.TypeOf((*identified)(nil)).Elem()

// restoreByIdentity matches members by who they are and reports whether it
// could — false hands the slice back to positional matching.
//
// A target member whose identity the prior document does not carry is left
// alone: it is new, or it was renamed, and either way there is no value of
// ITS to restore. Its mask survives into [Company.Validate], which is the
// outcome that names the field rather than inventing a credential for it.
func restoreByIdentity(target, prior reflect.Value, secret bool) bool {
	if !target.Type().Elem().Implements(identifiedType) {
		return false
	}
	byKey := make(map[string]reflect.Value, prior.Len())
	for i := range prior.Len() {
		key := prior.Index(i).Interface().(identified).IdentityKey()
		if key == "" {
			// A member that cannot name itself makes the whole list
			// ambiguous again, so nothing here is matched by identity.
			return false
		}
		if _, duplicate := byKey[key]; duplicate {
			// TWO MEMBERS, ONE IDENTITY. The document is invalid and
			// will be refused, but not until after this runs — and an
			// identity that is not unique is not an identity, so this
			// falls back rather than picking one of them.
			return false
		}
		byKey[key] = prior.Index(i)
	}
	for i := range target.Len() {
		previous, found := byKey[target.Index(i).Interface().(identified).IdentityKey()]
		if !found {
			continue
		}
		restore(target.Index(i), previous, secret)
	}
	return true
}

// mask hides a literal credential and leaves a reference alone.
func mask(value string, secret bool) string {
	switch {
	case !secret || value == "":
		return value
	case strings.Contains(value, "${"):
		// A reference, not a credential. The engine resolves it where a
		// provider is built, so the value it names is not in this
		// document to leak — and it is the half an operator edits.
		return value
	default:
		return Redacted
	}
}

// UnresolvedMasks lists the credential fields still holding the redaction
// marker, by JSON path.
//
// A document reaches this state when [Company.RestoreRedacted] could not
// resolve a mask — a member that is new or renamed carries no prior value of
// its own, and a list of BARE credentials that changed length no longer says
// by position which one is which. Guessing would write one credential into
// another's place. Refusing to guess is right; storing the result silently is
// not. The literal "__redacted__" would be handed to a provider as an API key
// and fail at the first call, hours later, with an authentication error that
// names nothing about where it came from.
func (c *Company) UnresolvedMasks() []string {
	if c == nil {
		return nil
	}
	var found []string
	findMasks(reflect.ValueOf(*c), "", false, &found)
	return found
}

func findMasks(v reflect.Value, path string, secret bool, found *[]string) {
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
				secret || field.Tag.Get(secretTag) == "true", found)
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			findMasks(v.Index(i), idx(path, i), secret, found)
		}
	case reflect.Map:
		for _, key := range v.MapKeys() {
			findMasks(v.MapIndex(key), at(path, key.String()), secret, found)
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
