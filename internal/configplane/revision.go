package configplane

import (
	"encoding/json"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/store"
)

// RevisionOf is the revision the activation pointer names, as a node's own
// history keeps it, from the pointer and the sealed body it carries.
//
// ONE MAPPING for every reader of the pointer — the reconciler opening the
// fleet's revision, the config API answering it, the boot keeping it — because
// each used to build its own and they disagreed on whose record a revision was.
//
// THE ORIGIN'S RECORD, NOT THIS NODE'S. The pointer carries who wrote the
// revision ([coord.RevisionOrigin]) — the author, what sort of author, the
// credential it was written through — and how and when, and a peer that stored
// `peer` from `fleet` at the activation instant instead gave one revision a
// different author on every node: the audit screen's answer depended on which
// node served it, and no node but the origin could say who had written it.
// Every pointer carries a complete origin — the coordination store refuses one
// that does not ([coord.RevisionOrigin.Check]) — so it maps straight onto the
// row.
//
// AT THE POINTER'S INSTANT, never its creation: the two differ on every
// re-activation of an older revision, and the instant is what an applied
// revision stamps the chart with.
//
// The body is the caller's to authenticate: it is whatever the coordination
// bucket holds, so nothing reads it as this fleet's until it opens under the
// fleet's keyring.
func RevisionOf(target coord.Activation, payload json.RawMessage) store.Revision {
	origin := target.Origin
	return store.Revision{
		ID: target.RevisionID, Source: origin.Source, CreatedBy: origin.Author,
		CreatedByKind: origin.AuthorKind, OperatorID: origin.OperatorID,
		Summary: target.Summary, Payload: payload, CreatedAt: origin.CreatedAt,
		ActivatedAt: target.At,
	}
}

// WithheldOfferReason is why a node does not publish an offline change, said the
// same way wherever a node reports one: at boot, and when the reconciler
// replaces it with the fleet's revision.
//
// AN OFFER AN EARLIER START SENT IS NOT SAID TO BE UNPUBLISHED. Its planned
// instant ([store.Offer.PlannedAt]) says that start published it or tried to,
// and one that landed and was then moved past before this node recorded the
// landing looks, from here, exactly like one that never landed. Saying it was
// never published would send an operator to redo a change the fleet already
// took.
func WithheldOfferReason(offer store.Offer) string {
	const moved = "the fleet moved on from the revision this offline change was built on"
	if offer.PlannedAt.IsZero() {
		return moved
	}
	return moved + "; an earlier start of this node published it, or tried to, and " +
		"whether the fleet ran it before moving on is not recorded here"
}

// WithheldOfferRemedy is what an operator does about an offline change the
// fleet moved on from (an offer withheld at boot, or replaced by the fleet's
// revision once applied), said the same way wherever a node reports one.
//
// NOT "IMPORT IT THROUGH THE API". The change was built on builtOn and the
// fleet has moved past it, so its whole document stood up as the company would
// undo everything the fleet changed since — which is exactly what withholding
// it avoided. What the operator needs is the change itself, read from THIS
// node (the offer is in its history alone) against what it was built on, and
// made again on the fleet's document; the revert is named last, as what puts
// the document up as it stands for an operator who means that.
func WithheldOfferRemedy(revisionID, builtOn string) string {
	against, what := builtOn, "what it was built on"
	if against == "" {
		against, what = "active", "the fleet's revision"
	}
	return "making it the company as it stands would undo what the fleet changed " +
		"since; GET /config/revisions/" + revisionID + "/diff?against=" + against +
		" on this node shows how it differs from " + what + ", to make that change on " +
		"the fleet's document (the dashboard, or crewlet config import -api); POST " +
		"/config/revisions/" + revisionID + "/revert on this node makes it the company " +
		"as it stands"
}
