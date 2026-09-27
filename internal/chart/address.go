package chart

import (
	"context"
	"database/sql"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/iam"
)

// WHICH ADDRESSES AN OBJECT MAY BE GIVEN, decided in ONE place for every path
// that gives one.
//
// # Why one function
//
// Four paths put an object on an address — a structural batch's create, a
// rename, an import's placement of an object the chart does not hold yet, and
// (for records a peer already wrote) a content record for an absent row — and
// the rules each asked had drifted apart: the reserved names were checked by
// the batch's create alone, so `tree`, `barrier`, `root` and Datadog's `none`
// all landed through a rename, a content write and an import; the seat-handle
// grammar was asked by nothing on the chart's side at all, so `jane.doe`,
// `ci:release` and `token:ops` became seats — names every lookup
// ([iam.OwnerOf]) sends to the identity directory rather than to the chart. A
// rule written per path is a rule one path forgets.
//
// So [refuseCreate] is the only question, and it is asked of an [addressBook]:
// the decide's working copy, whose answers include what the batch's own
// earlier operations did, or the apply's transaction, which is the one place an
// import — deciding nothing at its decide — can be held to the same rules.
//
// # A rename creates an address too
//
// Not an object: the object keeps its identity. But it is given an address, and
// every rule about which addresses exist applies to it — plus one a creation
// does not have, below.

// ReservedKeys are the addresses this engine will not let a company give an
// object of kind, because something already means each of them.
//
// FOR EVERY KIND: `root` is the unit a seat names when it sits at the org root
// ([RootUnit]), so a real unit keyed on it would make "at the root" and "in the
// root team" the same string in every scope path and every routing decision;
// `tree` and `barrier` are this log's own subject kinds, kept out of the
// address space so an operator reading a subject never has to ask which of the
// two a word is.
//
// AND FOR A SEAT, `none`: it is what `integrations.datadog.route_to` means by
// nobody, so a seat of that name would have every alert meant for it dismissed
// by its own handle. The config layer asks this set rather than keeping a list
// of its own — the chart cannot import the config package, which is why the
// word is spelled here and a config test holds it equal to that package's
// constant.
//
// ONE SET, returned per kind as a fresh slice so no caller can edit it.
func ReservedKeys(kind ObjectKind) []string {
	keys := []string{RootUnit, string(KindTree), string(KindBarrier)}
	if kind == KindSeat {
		keys = append(keys, reservedHandleNone)
	}
	return keys
}

// reservedHandleNone is the seat-only reserved handle. See [ReservedKeys].
const reservedHandleNone = "none"

// addressRefusal is why an address may not be given, in the vocabulary a
// refusal and a decline both render.
type addressRefusal struct {
	// Rule is a batch rule's name ([RuleKeyTaken] and its siblings), so a
	// decide can hand it straight to a [RefusalError].
	Rule string

	// Reason is the same answer as a closed word, for the apply's counter
	// and log line: a rule's name is a sentence, and a sentence is not a
	// metric attribute.
	Reason string

	// Detail is the sentence a person reads.
	Detail string
}

// holding is how an address is held, in the order the rules read it.
type holding int

const (
	// heldByNothing is an address nothing answers to.
	heldByNothing holding = iota

	// heldAsKey is an object's live key.
	heldAsKey

	// heldAsAlias is one of an object's capped retired keys.
	heldAsAlias

	// heldAsIdentity is the key an object was CREATED under and has since
	// been renamed away from — its identity (ADR-0019, ADR-0020).
	heldAsIdentity
)

// addressBook is what [refuseCreate] asks: whether an address is tombstoned,
// and which object answers to it and how.
//
// CONSUMER-DEFINED, with two implementations that must agree: the decide's
// working copy and the apply's own transaction ([txBook]). They answer from
// different states on purpose — the decide from its snapshot advanced by the
// batch's own earlier operations, the apply from the rows the log's order has
// produced — and the RULES are the same because both go through this one
// function.
type addressBook interface {
	removed(ctx context.Context, kind ObjectKind, key string) (bool, error)
	holder(ctx context.Context, kind ObjectKind, key string) (string, holding, error)
}

// refuseCreate reports why key may not be given to an object of kind, or nil
// where it may.
//
// SELF is the object being RENAMED onto key, named by the address it answers to
// now, and empty for a creation. The two differ in exactly one rule, and it is
// the one the difference in their identities forces:
//
//   - A CREATION MAY TAKE A RETIRED ALIAS. A new object's identity is the
//     address it is created under, and an alias carries no identity — only
//     the references somebody wrote with it, which the claimant then wins.
//   - A RENAME MAY NOT, unless the alias is its own. The renamed object keeps
//     its identity, so taking another object's alias would re-point every
//     reference written before that object moved, at an object that never had
//     them. Its OWN retired address is not a collision: renaming back claims
//     something that already resolves to it.
//
// And neither may take another object's IDENTITY, however long ago it was
// retired: a second object on one identity shares the first one's mailbox,
// lease and diary, and every person bound to it.
//
// THE ORDER IS THE REMEDY'S ORDER. A malformed or reserved address is refused
// before anything is read — no chart could ever hold it — and a REMOVED one
// before a held one, because a taken key needs a different name and a removed
// one can never be used again at all.
func refuseCreate(ctx context.Context, book addressBook, kind ObjectKind,
	key, self string) (*addressRefusal, error) {

	if refused := addressShape(kind, key); refused != nil {
		return refused, nil
	}
	return refuseHeld(ctx, book, kind, key, self)
}

// refuseHeld is [refuseCreate] without the address's shape: whether the
// address was removed and who answers to it, which is every rule a version-1
// record was held to when it applied — a version-1 placement, content record
// and rekey all declined an address a removal took or somebody's identity (a
// rekey, any address somebody else answered to), and none asked whether the
// address was reserved or a well-formed handle. The apply asks this of a
// version-1 record ([Applier.refuseAddress]) so a replay derives the rows the
// first apply did.
func refuseHeld(ctx context.Context, book addressBook, kind ObjectKind,
	key, self string) (*addressRefusal, error) {

	gone, err := book.removed(ctx, kind, key)
	if err != nil {
		return nil, err
	}
	if gone {
		return &addressRefusal{Rule: RuleKeyRemoved, Reason: "removed",
			Detail: fmt.Sprintf("%q was removed from the chart, and a removed "+
				"address never resolves again — its history, its references "+
				"and the tombstone that stops its old records applying are all "+
				"keyed on it", key)}, nil
	}
	holder, how, err := book.holder(ctx, kind, key)
	if err != nil {
		return nil, err
	}
	if self != "" && holder == self {
		// ITS OWN ADDRESS, current or retired: renaming onto what already
		// resolves to this object collides with nothing. The current one
		// is a rename that changes nothing, which the caller refuses with
		// better words than a collision.
		return nil, nil
	}
	switch how {
	case heldAsKey:
		return &addressRefusal{Rule: RuleKeyTaken, Reason: "taken",
			Detail: fmt.Sprintf("%s %q is already in the chart", kind, key)}, nil
	case heldAsIdentity:
		return &addressRefusal{Rule: RuleKeyTaken, Reason: "identity",
			Detail: fmt.Sprintf("%q is the address %s %q was created under — "+
				"its identity, which everything durable it owns and every "+
				"person bound to it is keyed on, and which it keeps however "+
				"often it is renamed. An identity is never issued twice; pick "+
				"another address", key, kind, holder)}, nil
	case heldAsAlias:
		if self == "" {
			// A CREATION TAKES A RETIRED ALIAS, and the claimant then
			// wins every reference written with it — see above.
			return nil, nil
		}
		return &addressRefusal{Rule: RuleKeyTaken, Reason: "alias",
			Detail: fmt.Sprintf("%q still answers to %s %q, which was renamed "+
				"from it — every reference written with it before that rename "+
				"still reaches %q, and a second object taking it would re-point "+
				"them all. Rename or remove %q first, or pick another address",
				key, kind, holder, holder, holder)}, nil
	}
	return nil, nil
}

// addressShape refuses an address no chart could ever hold, before anything is
// read: a reserved word, or a value outside its kind's grammar.
//
// A SEAT'S GRAMMAR IS THE IDENTITY LEAF'S ([iam.ValidSeatHandle]) — one
// segment, no `.` and no `:` — because those two characters are how a person's
// login and a machine's are spelled, and a seat carrying either is one the
// three namespaces were built never to produce. A UNIT'S is this domain's own
// key rule ([checkKey]): a unit key is minted from a display name, so it is
// looser than a handle, and what it may never carry is what would split a
// subject token or a scope path.
func addressShape(kind ObjectKind, key string) *addressRefusal {
	if slices.Contains(ReservedKeys(kind), key) {
		return &addressRefusal{Rule: RuleReservedKey, Reason: "reserved",
			Detail: fmt.Sprintf("%q is reserved for a %s: each of %v already "+
				"means something — the org root, this log's own subject kinds "+
				"and, for a seat, the word integrations.datadog.route_to uses "+
				"for nobody — so an object keyed on one would collide with it "+
				"in every scope path and every routing decision",
				key, kind, ReservedKeys(kind))}
	}
	switch kind {
	case KindSeat:
		if !iam.ValidSeatHandle(key) {
			return &addressRefusal{Rule: RuleBadKey, Reason: "shape",
				Detail: fmt.Sprintf("%q is not a seat handle: a handle is one run "+
					"of lowercase letters, digits and hyphens, starting with a "+
					"letter or a digit, at most %d bytes. A `.` is how a "+
					"person's login is spelled and a `:` a machine's, so a seat "+
					"called that would be one every name lookup sends to the "+
					"identity directory instead", key, iam.MaxLogin)}
		}
	case KindUnit:
		if err := checkKey("key", key); err != nil {
			return &addressRefusal{Rule: RuleBadKey, Reason: "shape",
				Detail: err.Error()}
		}
	default:
		return &addressRefusal{Rule: RuleUnknownKind, Reason: "shape",
			Detail: fmt.Sprintf("%s is not an object in the chart — only a "+
				"unit and a seat have an address", kind)}
	}
	return nil
}

// txBook is [addressBook] over the apply's own transaction.
type txBook struct{ tx *sql.Tx }

func (b txBook) removed(ctx context.Context, kind ObjectKind, key string) (bool, error) {
	return objectRemoved(ctx, b.tx, ObjectRef{Kind: kind, ID: key})
}

// holder reads the address the way [resolveUnit] does — the live key first,
// then the renamed objects, the identity outranking an alias — and says which
// of the three it matched.
func (b txBook) holder(ctx context.Context, kind ObjectKind, key string) (
	string, holding, error) {

	switch kind {
	case KindUnit:
		if unit, found, err := readUnit(ctx, b.tx, key); err != nil || found {
			return unit.Key, heldAs(found, heldAsKey), err
		}
		unit, match, err := byRetiredAddress(ctx, b.tx, "chart_units", key,
			DecodeUnit, Unit.Origin)
		return unit.Key, retiredHolding(match), err
	case KindSeat:
		if seat, found, err := readSeat(ctx, b.tx, key); err != nil || found {
			return seat.Handle, heldAs(found, heldAsKey), err
		}
		seat, match, err := byRetiredAddress(ctx, b.tx, "chart_seats", key,
			DecodeSeat, Seat.Origin)
		return seat.Handle, retiredHolding(match), err
	}
	return "", heldByNothing, nil
}

// heldAs is how, when found, and nothing otherwise.
func heldAs(found bool, how holding) holding {
	if found {
		return how
	}
	return heldByNothing
}

// retiredHolding maps a retired-address match onto the book's vocabulary.
func retiredHolding(match retiredMatch) holding {
	switch match {
	case retiredOrigin:
		return heldAsIdentity
	case retiredAlias:
		return heldAsAlias
	}
	return heldByNothing
}
