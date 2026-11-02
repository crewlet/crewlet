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
// # Required, retired and derived (ADR-0074)
//
// Every declaration states its STANDING ([objstore.Standing]), and the gate
// below refuses one that states none of the three, and holds each entry's to
// a reviewed row naming why (TestEveryDeclarationStatesItsStanding). A
// REQUIRED table's rows are live references: the object is part of something
// the company holds, so the collector keeps it, the audit asks the store for
// it, a backup accounts for it — copied on `s3`, asked after once the bucket's
// stream snapshot is taken on `nats`, recorded lost where the store does not
// hold it — and a store move copies it into the store the fleet moves to. A
// RETIRED table's rows keep an object the company has let go of from the
// collector for a grace — a replaced object a reader that has not caught up
// may still be fetching — and do nothing else: never audited, never copied,
// counted or asked after by a backup (on `nats` its bytes ride the bucket's
// stream snapshot like any other object the bucket holds, uncounted), and left
// behind by a store move, after whose restart onto the new store no such
// reader is left. A DERIVED table's rows keep an object the engine can build
// again from what Required rows name for as long as they stand, and have a
// store move copy it as it copies a Required one, and do nothing else: a lost
// one costs a rebuild and no data, so it raises no `objects_missing`, and a
// restore without it is a company whose engine builds it again. A backup and
// a move part over it for what each costs: a backup is taken again and again,
// so carrying it would cost its bytes at every backup to spare a rebuild after
// a rare restore, while a move is made once, and leaving it behind would have
// every node of the moved fleet build it again.
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
// rows name and what sweeps them — a job of the engine's maintenance worker,
// or a duty whose records delete them, as the code host's retire_sweep records
// delete its retired packs — and holds that grace above the longest read of
// that object ([objstore.ReadBudget]) by more than the clocks stamping and
// sweeping a row can disagree.
//
// A DERIVED ROW OWES NO GRACE, and its declaration no entry: its domain owes
// every reader of the object a way on without it — reading the rows again for
// what replaced it, or building it — so it may delete a derived row the moment
// it replaces what the row names, and the collector's next pass deletes the
// object as it deletes anything nothing names.
//
// NO DEFAULT, because each is a silent failure: a live table read as retired
// or derived is audited by nobody and accounted for by no backup; a retired or
// derived table read as live pages somebody about bytes nobody needs and has
// every backup copy them, or list them lost; and a retired table read as
// derived is held to no grace, so its object can go under a reader still
// fetching it.
//
// # The one input, not one of several
//
// The collector, its audit, the backup and a store move build their
// statements FROM this list ([objstore.ReferenceTable.ObjectsAmong],
// [objstore.ReferenceTable.ReferencesAfter]); none carries a query of its own.
// Each reads the standing too: the collector's batch question is asked of
// every table, and each walk is built for its pass ([objstore.Pass]) — the
// audit's and a backup's read Required tables alone and refuse the other two,
// a store move's reads Required and Derived ones and refuses a retired one. A
// domain supplies only what a declaration cannot — a barrier on its log and a
// read of its rows (collect.Estate) — and the engine's own test builds the
// passes from this list against the estates it hands them, so a table declared
// in a domain the engine reads no estate of fails the build rather than the
// boot.
//
// # The log half (ADR-0075)
//
// A backup reads more than rows: a restore brings its store copy back and
// replays the declaring domain's log from the copy's position — and a newer
// build applies what the copy's applier deferred — so a Required declaration
// also names its log ([objstore.ReferenceTable.Log]), the table its domain
// defers that log's records into ([objstore.ReferenceTable.Deferred]) and the
// domain's own reader of one record ([objstore.ReferenceTable.Records]). The
// gate here refuses a Required entry missing any of them, or naming a deferred
// table the replicated schema does not hold; the engine's gate holds the log
// and the table to the registered domain's own, which this package cannot
// import.
package references

import (
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/worklet"
)

// All is every replicated table whose rows keep objects alive, whatever its
// standing: a project's files, the pack bundles the company's repositories'
// pushes stored and, for a grace, the ones a record retired, the parts of each
// repository's published multi-pack-index layers, the logs its failed checks
// kept, an artifact's version files and assets, the files of the company's
// adopted brand, and the files a person's assistant conversations keep.
var All = []objstore.ReferenceTable{
	worklet.FileObjectReferences,
	code.PackReferences,
	code.RetiredPackReferences,
	code.LayerReferences,
	code.CheckLogReferences,
	artifactsdomain.ObjectReferences,
	artifactsdomain.AssetReferences,
	artifactsdomain.BrandFileReferences,
	assistantdomain.FileObjectReferences,
}

// ObjectColumn is the name every referencing table gives its key column, and
// what the schema gate looks for. One name, so a new table cannot name its
// objects something the gate does not recognise.
const ObjectColumn = "object"
