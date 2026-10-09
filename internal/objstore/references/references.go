// Package references is the one list of replicated tables whose rows name
// objects of the object store — ADR-0026's cross-package half.
//
// # Why a package of one list
//
// The collector deletes every object no row names, so a table that names
// objects and is missing from this list is a table whose files are deleted a
// day after they were written. The consumer that owns such a table cannot
// hold the list (the collector would have to import every consumer), and the
// object store cannot hold it (every consumer imports the object store). So
// each consumer declares its table beside its own schema, and this package
// collects the declarations — and its test holds the list against the
// replicated schema in BOTH directions: a column named `object` in a table not
// listed here fails the build, and so does a listed table the schema does not
// have, or one whose key column no index leads with.
//
// # Required and retired (ADR-0033)
//
// Every declaration states its STANDING ([objstore.Standing]), and the gate
// below refuses one that states neither. A REQUIRED table's rows are live
// references: the object is part of something the company holds, so the
// collector keeps it, the audit asks the store for it and a backup accounts
// for it — copied on `s3`, asked after once the bucket's stream snapshot is
// taken on `nats`, recorded lost where the store does not hold it. A RETIRED
// table's rows keep an object the company has let go of from the collector for
// a grace — a replaced object a reader that has not caught up may still be
// fetching — and do nothing else: never audited, and never copied, counted or
// asked after by a backup (on `nats` its bytes ride the bucket's stream
// snapshot like any other object the bucket holds, uncounted).
//
// THE GRACE IS THE ROW'S LIFETIME, and bounding it is the declaring domain's
// obligation: it deletes a retired row once its grace has passed, and the
// collector's next pass deletes the object as it deletes anything nothing
// names. No timestamp of a retirement ever reaches the collector, so
// ADR-0027's judgement — is it named, and is it past the upload's grace — is
// unchanged. A retired table nobody sweeps keeps its objects for ever; a grace
// shorter than the longest read of what it names deletes the object under
// that read. So a retired declaration never lands alone: the gate refuses one
// without a reviewed entry naming its domain's grace, the largest object its
// rows name and the job that sweeps them, and holds that grace above the
// longest read of that object ([objstore.ReadBudget]) by more than the clocks
// stamping and sweeping a row can disagree.
//
// NO DEFAULT, because each is a silent failure: a live table read as retired
// is audited by nobody and accounted for by no backup; a retired table read as
// live pages somebody about bytes nobody needs and has every backup copy them,
// or list them lost.
//
// # The one input, not one of several
//
// The collector, its audit and the backup build their statements FROM this
// list ([objstore.ReferenceTable.ObjectsAmong],
// [objstore.ReferenceTable.ReferencesAfter]); none carries a query of its own.
// Each reads the standing too: the collector's batch question is asked of
// every table, and the walk the audit and the backup take is built for
// Required tables alone and refused for a retired one. A domain supplies only
// what a declaration cannot — a barrier on its log and a read of its rows
// (collect.Estate) — and the engine's own test builds the passes from this
// list against the estates it hands them, so a table declared in a domain the
// engine reads no estate of fails the build rather than the boot.
package references

import (
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/tracker"
)

// All is every replicated table whose rows keep objects alive — required and
// retired alike.
var All = []objstore.ReferenceTable{
	tracker.FileObjectReferences,
}

// ObjectColumn is the name every referencing table gives its key column, and
// what the schema gate looks for. One name, so a new table cannot name its
// objects something the gate does not recognise.
const ObjectColumn = "object"
