// Package trashreap is the trash reaper: the fleet duty that purges what has
// been trashed past its term, as the system, for every domain that keeps a
// trash.
//
// # Why it exists
//
// A trashed page was documented as recoverable for thirty days and then kept
// for ever, because nothing purged it: every page anybody deleted stayed in
// every data node's copy and in every backup. The reaper keeps that promise for
// pages and, through the same duty, for artifacts (ADR-0008: one engine-wide
// rule rather than one per domain). A domain joins by handing the engine a
// [Source]; the reaper knows nothing of any domain's rows.
//
// # What a pass does
//
// A pass runs once per [PassInterval] slot on whichever node holds the
// `trash-reaper` duty. It asks each source what it holds in the trash since
// before the cutoff (the slot's start less the term), oldest first, and purges
// each item through the domain's own purge record, at most [MaxPerPass] a
// source a pass. It then records a [Report] in the coordination store, which is
// what the next holder of the duty resumes the schedule from.
//
// A source may keep a term of its own ([Termed]), asked at every pass: the
// Dashboard Assistant's conversations are purged their retention after they were
// last used rather than thirty days after a trash. The floor ([MinTerm]) holds
// for every term, whichever source states it, and a term the source cannot
// state at a pass (the assistant's, on a node that does not run the revision
// the fleet's activation pointer names) purges nothing of that source rather
// than a guess at it.
//
// # Why nothing is purged twice, or wrongly
//
// The list only nominates. Each source decides its purge again in the snapshot
// its record is decided in, where the broker's arbitration on the item holds
// the answer to the append: an item restored, or trashed again (which starts
// its term over), after it was listed is refused ([ErrNotEligible]), and an
// item another reaper already purged is gone. So two reapers that overlap while
// the duty moves between nodes cannot destroy anything a person took back.
//
// THE OPERATION ID IS DERIVED from the slot, the source and the item
// ([OpID]), never minted per attempt. Two reapers in the same slot, and a pass
// retried after a failure, publish the same purge under the same id, which
// the operation ledger and the broker's duplicate window collapse into one
// record. The slot's start is the id's instant, which keeps it inside the
// ledger's horizon however old the trashed item is: an id is used only within
// the slot it names, so it never needs the rebase ADR-0029 decides for work
// older than the ledger.
//
// # Who it acts as
//
// The engine, under the name [Name], recorded in each domain's own vocabulary
// for a write the engine makes (`iam.ActorSystem` where the domain has that
// kind, the knowledge base's operator kind where it has not). An automatic
// purge is housekeeping nobody asked for in the moment, and putting a person's
// or a credential's name on it would be an audit row that lies. A purge a
// person makes is the domain's admin route, decided by the authority table;
// the reaper is not a way around it, since it purges only what somebody
// already trashed and left there past the term.
package trashreap
