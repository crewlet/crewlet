// Package collect deletes the object store's garbage and counts what it has
// lost: the one fleet duty the object store needs (ADR-0026).
//
// # Why there is so little here
//
// The bytes live in ONE backend the whole fleet shares — the broker's own
// object store, replicated as every stream is, or an S3-compatible bucket
// that keeps its own copies. So nothing here decides where an object lives,
// fetches a missing copy or reads a disk for rot: the backend does all of
// that. What no backend can know is which objects the company still REFERS
// to, because that is a fact about the replicated estate — so the two jobs
// left are the two that need it, run by one data node at a time.
//
// # The inventory is the estate
//
// No list of objects is kept beside the estate. Every row that refers to an
// object names its key, and the declared tables (internal/objstore/
// references) are the whole inventory ([Sources]). A list kept beside it
// would be a second answer to which objects exist, and the two would drift
// the first time a write landed in one and not the other.
//
// Every declared table is REQUIRED, RETIRED or DERIVED (ADR-0074). All three
// keep the objects their rows name from collection; only a required table's
// are the company's to account for, so only those are audited, and only those
// a backup copies, counts or asks after (on `nats` a retired or derived
// object's bytes still ride the bucket's stream snapshot, uncounted). A
// retired row keeps a replaced object for a grace a reader still on it needs;
// the grace is the row's lifetime, and once its domain deletes the row the
// object goes at the next pass like any other nobody names. A derived row
// keeps an object the engine can build again for as long as it stands, and
// owes no grace when it goes. Nothing here reads when a reference was retired
// or replaced.
//
// # Collection: deletion is the only dangerous thing, and it needs no lock (ADR-0027)
//
// What a name in the store MEANS is read here, never by a backend: a backend
// lists every name it holds, verbatim, and this package sorts each into one
// of two kinds.
//
//   - AN OBJECT'S KEY under the engine's namespace (objstore.KeyOfName) is
//     the collector's to judge, by the rule below.
//   - ANYTHING ELSE is left where it is, and never counted: another
//     application's object under a shared prefix, something an operator
//     put in the bucket.
//
// A key's object is deleted only when ALL of these hold:
//
//   - It was stored more than [PendingGrace] ago, by the backend's own
//     clock (objstore.Info.Written). Bytes are uploaded BEFORE the record
//     naming them is written, so every object is unnamed for a while at the
//     start of its life; the grace outlasts the slowest upload from its key
//     to its record.
//   - Its key was MINTED more than [PendingGrace] ago, by its own instant
//     (objstore.Key.Minted). This is the half that makes the rule safe: a
//     write naming a key is refused once the key is older than
//     objstore.RecordWithin, which is shorter than the grace by more than
//     any two nodes' clocks disagree — so a key past the grace is one no
//     write can name any more, however late a record arrives.
//   - No declared table, of any standing, names it in an estate that is
//     CURRENT and COMPLETE.
//     Current: before it judges its first object, the pass waits for
//     everything each domain's log had committed by then, and so by the
//     moment it started ([Source.Barrier]). A pass with nothing past the
//     grace takes no barrier and writes nothing to any log. Complete: a
//     record this node could not decode might be the one naming the
//     object, so a pass that meets one stops judging. What it deleted
//     before is reported beside why it stopped.
//
// THE GRACE IS AN UPLOAD'S, NEVER A REPLACED VERSION'S. Both instants date
// the object's upload, and nothing records when a row stopped naming it: an
// object a file was rewritten off, or removed from, goes at the first pass
// after it stops being named once both are past the grace — within the hour
// for a file older than a day. The store is not where a replaced version is
// kept.
//
// NO LOCK, NO SECOND LOOK. A key is minted for one upload and named only by
// the write that uploaded it, so there is no writer re-using an object the
// collector could be deleting, and so nothing for a lock to guard. Two
// collectors at once are safe too: deleting an object that is gone is not an
// error.
//
// # The sweep: what no listing shows
//
// An upload that died part of the way through — a process killed mid-put, a
// delete interrupted between its halves — leaves bytes no name reaches: an S3
// multipart upload nobody completed or aborted, broker pieces no metadata
// names. A listing of the names never shows them, and each can be a gibibyte,
// so every collection also asks the backend for them (objstore.Backend's
// Pending) and abandons the ones begun more than [PendingGrace] ago — every
// upload still in flight is pending too, and none takes that long — that are
// named by no name or by a key also minted past the grace; never one under
// somebody else's name. It needs no estate: an upload that never finished is
// named by no row. A sweep the backend refuses — an S3 identity without the right to list or abort
// uploads — is reported beside the collection rather than failing it.
//
// # Audit: what the store has lost
//
// Every object a REQUIRED table names should be in the backend, whole — a
// file's record is written only after its object is stored. The audit asks the
// backend for each one and finds three things ([Finding]):
//
//   - MISSING: an object the store answers it does not hold, on two looks.
//   - DAMAGED: one it holds at another size, or under another digest where the
//     backend keeps one, or as no object it can read at all —
//     objstore.ErrCorrupt, a definite answer that needs no second look.
//   - UNANSWERED: one the store FAILED to answer about, on two looks, while it
//     answered the audit's other questions.
//
// All three are files the company cannot count on reading: the
// `objects_missing` alarm and the list `crewlet objects status` prints, each
// named as the file that holds it, since a person restores files rather than
// keys. A durable backend loses nothing, so a non-zero count is the backend
// failing at the one thing it is for. A retired reference is never asked
// after: the company has let its object go, and a store that lost it lost
// nothing anybody can miss. Nor is a derived one: a store that lost its object
// lost a rebuild, which the engine makes, and no file a person restores.
//
// THE SECOND LOOK comes at the end of the pass — or sooner, once
// [maxSuspects] others are waiting for theirs, so a store that lost everything
// never has the audit hold the whole inventory — for every object whose first
// look was not definite: "not found" from a store that may not have caught up
// with an upload yet, and a question the store failed. Only the second look's
// answer is the finding. The second looks wait in the order their first looks
// ended, a reference at a time or a range at a time ([lookAgain]).
//
// ONE OBJECT IS NEVER THE AUDIT'S FAILURE. A question the store fails — its
// client having asked again until the question's deadline — is set aside for
// its second look rather than failing the audit, and an object the store
// fails both times is UNANSWERED once the store answers a question after it:
// an S3-compatible gateway that answers one object with a server error every
// time, or refuses it, is one the company cannot read the file from, and an
// audit that failed instead never named it — every day it stopped there, and
// each retry asked the store about every object before it again. What fails
// an audit is the STORE failing: [failedInARow] questions failed with none
// answered between, which a store that has gone away does to the first
// window of questions it is sent — or, when the audit's last questions are
// failures nothing answered after, one more question about an object the
// store had answered for failing too ([auditor.confirm]), so a store that
// went away as the walk ended is never taken for one refusing each object it
// was asked about last.
//
// THE ESTATE IS READ A PAGE AT A TIME, BY ONE GOROUTINE, IN KEY ORDER — every
// required table's keyset walk merged into one order ([Source.Required],
// [objstore.MergeWalks]), holding a page of each — AND THE BACKEND IS ASKED
// OUTSIDE EVERY READ — up to [auditConcurrency] questions at once while the
// walk reads on, and a window of a listing by the walk itself between two of
// its reads, never inside one: a read of the estate holds one of the node's
// few reader connections and the snapshot under it, and a question to the
// backend is a network round trip — tens of thousands of them inside one read
// would hold the connection for the length of the audit.
//
// A STORE THAT KEEPS NO ORDER IS READ FROM ITS LISTING, A WINDOW AT A TIME —
// the broker's, which lists in the order it was written, and an S3 Express
// directory bucket, which lists in none. A listing of the whole store keeps
// the [auditWindow] smallest keys after the last window's
// ([auditor.listWindow]), and the walk judges each reference whose key falls
// in it from what it lists: held, or held wrong, as it says; listed holding
// nothing at all — no bytes and no digest, as the broker lists a name whose
// metadata does not decode or that only a delete marker holds — or not listed,
// it waits for a second look by name like any "not found". Once the walk
// passes a window's last key it lists the next, so each reference is looked at
// once, the audit holds a window of the inventory and never the whole of it,
// and a store of N objects is listed ceil(N / [auditWindow]) times — a listing
// request where there was a request an object, at the price [auditWindow]
// states. A LISTING THAT FAILS turns the rest of the audit to asking about
// each object by name, the way the audit asked before it had a listing: a
// window's extent is its listing's answer, so without one nothing says which
// references a listing again would cover, and a store that has gone away
// fails those questions too, which stops the audit ([failedInARow]).
//
// A STORE THAT LISTS IN ORDER IS ASKED A RANGE AT A TIME ([objstore.Ranged]):
// the walk is cut into chunks of references, and one listing of the range a
// chunk spans — the keys after the last chunk's and through its own — answers
// what a stat of each object in it would: an object it hands on is held, or
// held wrong, as it says; one it passes over waits for a second look like any
// "not found". On S3 that is a request per range rather than a HEAD per
// object, because a chunk is cut so that its range fills S3's page short of a
// room ([rangePageRoom]): [rangeChunk] references where the store keeps
// nothing between them, and fewer by the density of the objects between them
// that no required row names — a retired or a derived row's, garbage — which
// the walk MEASURES from the ranges it has listed, its first listed before it
// cuts the second ([auditor.cut]). So a range fits one page whatever the store
// keeps besides the references, and the ranges tile the key space, so the
// audit lists the engine's corner of the bucket once. Only because the walk is
// merged into ONE order: a range cut from one table's keys would leave out
// another's between them. One whose range comes back out of order
// ([objstore.ErrUnordered]) is read from its listing from then on, as a store
// that keeps no order is, the listing beginning after the last range the walk
// handed on and the ranges handed on but not yet listed asked about by name.
//
// A RANGE WHOSE LISTING FAILED TAKES ITS SECOND LOOK AS A RANGE: what the
// listing had not reached waits, with the key it comes after, for ONE listing
// more, and only an object that listing passes over is then asked about by
// name. A listing fails once its deadline passes with the bucket still
// throttling or failing it; asking by name there sent a HEAD for each of the
// range's hundreds of objects to the prefix that had just said to slow down.
// A re-listing that fails too leaves its objects UNANSWERED by the rule below,
// and what a re-listing that comes back out of order had not reached is asked
// about by name.
//
// WHAT AN AUDIT FOUND OUTLIVES THE AUDIT THAT FAILED AFTER IT, and the duty
// moving: the report keeps the last audit that ran to its end beside the
// last attempt ([AuditReport.Found]), and a node taking the duty picks both
// up from the fleet's record ([Collector.Restore]), so a broker that stopped
// answering mid-audit, or a holder that went away, never clears an alarm about
// files the store has lost. An audit its caller stopped has not run to its end
// either, however its questions fared: what it counted by then is a floor.
//
// # How hard the store is asked
//
// A collection makes a request per object it deletes — or, on a store that
// deletes in batches ([objstore.BatchDeleter]), one per judged batch of them.
// An audit makes, on a store that lists in order, one per range — about one
// per [rangeChunk] names among the objects it asks about, the store's others
// between them counted, once the walk has measured how many those are
// ([auditor.cut]); on one that keeps no order, one listing of the store per
// [auditWindow]; and a stat for each
// object its listings pass over, or for every object once a listing has
// failed. One at a time those stats are round trips in a row: an audit of two
// million objects by stat was two million of them, end to end. So the audit
// asks up to [auditConcurrency] questions at once — a stat, or a listing of a
// range — and a collection has up to [deleteConcurrency] deletions in flight
// beside its listing, or [batchesInFlight] batches of them. A window's listing
// is the walk's own, one at a time, since the walk needs it before it can
// judge the references in it.
//
// THE BOUNDS ARE CLOSED LOOPS, never a rate: a request goes out only when one
// in flight has been answered, so a store that slows down is asked more slowly
// by itself, and one that fails stops the pass with at most the bound
// outstanding — a collection at its first failed deletion, an audit at the
// store's [failedInARow]th failed question with none answered between — and
// the pass returns only once every request it made has ended. Neither is
// configurable: a closed loop already adapts to a store that answers slowly,
// and an operator has no better number than the one the bucket's transport
// keeps warm connections for (httpx.MaxIdleConnsPerHost — s3obj's clone of
// httpx's, over HTTP/1.1, keeps httpx's pool — which the audit's bound is
// held to by a test).
//
// ONLY THE STORE'S REQUESTS ARE PARALLEL. The estate is read by one goroutine
// — the walk, and each batch the collection judges — because a read holds one
// of the node's few reader connections and its snapshot, and one node runs one
// pass at a time; the bound is what one pass may ask of a store the whole
// fleet shares. The sweep of unfinished uploads stays one at a time: it
// abandons what a crash left, a handful a day.
//
// THE DEPTH IS MEASURED, NOT INFERRED (BenchmarkAudit, over a modelled store
// and estate rather than any real one; 2026-10-24, four vCPUs of a 2.3 GHz
// Xeon): asked by name, with a stat answered in 2 ms and a page of the estate
// read in 1 ms, the audit kept a mean of 31.5 of its 32 questions out across
// sixteen pages — a page read does not drain the bound. Read from the listing
// of a store that keeps no order, a name listed in 12.5 µs (eighty thousand
// a second, about the broker's pace), the same eight thousand references took
// one listing (0.000125 requests a reference) and went 63,700–63,900 a second
// against 13,300–13,400 by name — and cut into eight windows, eight listings
// of every name (0.001 a reference) and 9,360–9,380 a second, the listings'
// cost growing with the windows as [auditWindow] says. Listing ranges as S3
// does, a page of a thousand names a request answered in 10 ms (2026-10-26,
// four E-cores of an Intel Core Ultra 7 265U, Go 1.27.2, -benchtime 3x
// -count 1), over a store holding one object no required row names to every
// fifty referenced, the walk cut its ranges to 882 references, nine hundred
// names, and made 0.00115 requests a reference — about 3% more than a chunk
// of nine hundred references made there (0.00111), the price of cutting by
// names — keeping about four out: there the walk's own page reads, two a
// range, set the pace (320,000 references a second), and a slower listing —
// S3's — keeps more of the bound out rather than slowing the audit. With one
// to every five, as the last day's keys may hold, it made 0.00135 — where a
// chunk of nine hundred references spanned two pages, two requests a chunk
// (0.00222) — four and a half out and the same pace; and with one beside
// every reference, the most an index kept over packs puts among them,
// 0.00223, a page of names a range, the fewest those names allow, six and a
// half out and 290,000 references a second.
// These are the model's figures, not any store's, and are taken again with
//
//	go test -run '^$' -bench BenchmarkAudit -benchtime 3x ./internal/objstore/collect/
//
// What the audit names is chosen by NAME, the first [MissingShown] files by
// project and path, never by the order its questions were answered in, which
// running them at once leaves to chance.
package collect

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/objstore"
)

var log = logging.Get("objstore")

// PendingGrace is how long an object nothing refers to is kept, measured
// both from when the backend stored it and from when its key was minted.
//
// A DAY. The grace has to outlast the longest a key can go unnamed and still
// be named: objstore.RecordWithin, twelve hours, after which a write naming it
// is refused — and the twelve hours left over are what the clocks of the node
// deciding a write and the node running the collector may disagree by before
// the rule stops holding, which is hours more than any clock an engine runs
// on is ever off. It costs nothing but a day's worth of abandoned uploads in
// the store. It is not what covers a node whose estate is behind — the pass's
// barrier is.
const PendingGrace = 24 * time.Hour

// CollectInterval is how often garbage is collected.
//
// AN HOUR: a pass waits on a barrier in every domain that refers to objects
// and lists the whole store, and the only cost of running it less often is
// garbage kept a little longer beside a grace that is already a day.
const CollectInterval = time.Hour

// AuditInterval is how often the store is asked for every object a required
// table names.
//
// A DAY. What an audit looks for is a backend losing acknowledged bytes — an
// event, not a drift: a durable store does not lose a little more each hour —
// so how soon after one it pages is the trade, against what one audit costs.
// No longer the requests on the production store: on an S3 bucket that lists
// in order an audit of two million objects is some 2,200 listings where the
// bucket keeps little else among them — a listing per nine hundred names
// more for every other object it keeps there, up to twice as many where a
// derived object sits beside every live one — and a HEAD for each object
// they pass over, a minute or two of listing, and hourly would cost little
// more. What it costs is bounded instead by the two parts that
// grow with every object a live row names whatever the store:
//
//   - A WALK OF THE WHOLE REQUIRED ESTATE on the duty's data node, a page of
//     [objstore.WalkPage] rows a read on one of its few reader connections,
//     and a pin appending a barrier to every declaring domain's log
//     ([PinsPerDay]).
//   - A LISTING OF THE WHOLE STORE PER WINDOW on a store that keeps no order
//     ([auditWindow]): on `nats` two million objects are twenty listings of
//     every name, seven to eleven minutes of listing, and on an S3 directory
//     bucket about twenty minutes — or, once a listing has failed, a stat of
//     each object, two minutes of the bucket stream's leader on a member and
//     up to six from a follower, or 23 to 55 minutes of HEADs on a directory
//     bucket ([auditConcurrency]).
//
// Hourly, the directory bucket's audit would run a third of every hour and
// the broker's listing name nearly a billion objects a day for one company; a
// day keeps every backend's audit a small share of its day, and a loss is
// still paged within a day of happening.
const AuditInterval = 24 * time.Hour

// PinsPerDay is how many times the collector pins the estate in a day at its
// steady cadence — a collection every [CollectInterval] and an audit every
// [AuditInterval]. Each pin is a linearizable read, which appends one barrier
// to the log of every domain a declared table names; the census those logs'
// read rates are held against counts it (statelog.Census.Background).
const PinsPerDay = int(24*time.Hour/CollectInterval + 24*time.Hour/AuditInterval)

// JudgeBatch is how many listed objects one reference read judges: the
// widest [objstore.ReferenceTable.ObjectsAmong] a collection builds, and a
// pass's last batch any narrower one.
//
// FIVE HUNDRED: one statement's arguments, inside every SQL engine's variable
// ceiling with room to spare, and what the pass holds in memory between the
// listing and the read — never the store's whole inventory.
//
// EXPORTED FOR THE PLANS HELD TO IT. Each width is a statement text of its
// own, which the driver plans on its own, so a declaring domain's plan gate
// plans the question at this width — a gate that planned three keys would
// pass a driver that scans a table for five hundred.
const JudgeBatch = 500

// auditConcurrency is how many questions the audit has out to the store at
// once: a stat of one object, or — on a store that lists in order — a listing
// of a range of up to [rangeChunk] of them.
//
// THIRTY-TWO. Asked by name, an audit is one question per object a required
// row names — on a store that keeps no order once its listing has failed —
// and asked one at a time they are round trips in a row: two million objects
// are two million round trips end to end, most of a day at any latency a
// network store answers in, of a pass due daily that holds the duty's only
// turn for all of it. With thirty-two out the audit asks 32 divided by the
// store's mean latency a second — whatever the store's latency is; nothing
// here assumes a figure for it, and what each backend answers is its own
// benchmark's, below. On one that lists in order the same two million are some
// 2,200 listings and more by the bucket's other objects among them
// ([auditor.cut]), which the bound takes thirty-two at a time; on
// one that keeps no order its listing is the walk's own, a window at a time
// ([auditWindow]), and the bound carries the second looks.
//
// AND NO MORE, because the bound is what keeps the store from being flooded:
// a question goes out only when one of the thirty-two has been answered, so a
// store that slows down is asked more slowly by itself, and one that fails
// stops the audit at its [failedInARow]th failure in a row with at most
// thirty-two questions out.
//
//   - S3 documents at least 5,500 GET and HEAD requests a second per
//     partitioned prefix, and every object the engine writes is under one
//     (`files/`). Thirty-two out reach that only if a request averaged under
//     5.8 ms — a listing of a thousand keys or a HEAD of a second look alike
//     — which leaves the rest of the rate to the company's own downloads,
//     each a GET on the same prefix. A throttle slows the whole client
//     (internal/objstore/s3obj), every request of the node's waiting its
//     turn and the throttled one asked again until its deadline, so the
//     audit's thirty-two slow with everything else the node sends the
//     prefix. Against a bucket held 20 ms away, and 50, the backend's
//     benchmark listed 31,400–33,000 names a second and 15,600–16,900 one
//     listing at a time — two million objects in one to two minutes — and
//     answered 47 HEADs a second and 19 for each one out: 1,380–1,450 and
//     about 610 with thirty-two out, which is what a directory bucket costs
//     asked by HEAD, as it is once its listing has failed — 23–24 minutes and
//     55 for two million objects, rather than twelve hours and twenty-nine.
//   - It is the bucket's transport's warm connections to one host
//     (httpx.MaxIdleConnsPerHost: s3obj's clone of httpx's, over HTTP/1.1,
//     keeps httpx's pool), so every question rides a connection already open
//     rather than paying a TLS handshake of its own.
//   - On `nats` a question by name is one read from the bucket stream's
//     leader. The backend's benchmark answered a stat across ten thousand objects
//     4,800–8,600 times a second one at a time on a member's own client and
//     15,000–19,000 with thirty-two out; through a leaf 2,900–4,200 and
//     15,000–25,000; and from a follower at three replicas, whose every
//     question crosses a route to the leader, 2,100–2,800 and 5,600–23,400.
//     The depth buys a few times the stats a second, for a fraction as long:
//     two million objects are about two minutes on a member rather than four
//     to seven.
//
// Those figures are the backends' own benchmarks (`make bench`; four vCPUs of
// a 2.8 GHz Xeon, 2026-10-10), recorded in internal/objstore/natsobj's and
// s3obj's package docs — the S3 ones against an in-process fake held that far
// away, since no real bucket has been measured.
const auditConcurrency = 32

// deleteConcurrency is how many deletions a collection has in flight at once
// on a store that deletes one object a request — the broker's, an S3-compatible
// gateway that does not offer a batch.
//
// SIXTEEN, half the audit's, because a deletion is a WRITE. On S3 a prefix's
// writes — AWS documents at least 3,500 PUT, POST and DELETE requests a second
// — are what the company's uploads spend too, every part of a multipart upload
// included, and sixteen out reach a quarter of them only if a DELETE averaged
// under 18 ms. On `nats` a deletion is three requests to the bucket stream's
// leader — a read of the object's metadata, then a purge of it and a purge of
// the object's pieces, each purge a write replicated through the stream's
// group — the costliest thing the object store asks of the broker. Asked one at
// a time a collection's deletions are round trips in a row; sixteen out divide
// that by sixteen.
const deleteConcurrency = 16

// batchesInFlight is how many batch deletions ([objstore.BatchDeleter]) a
// collection has out at once on a store that deletes in batches: each judged
// batch's unnamed objects — up to [JudgeBatch] — in one DeleteObjects.
//
// ONE, and the reason is the prefix's write rate rather than the requests.
// AWS states that rate in requests and is silent on how a batch's keys count
// against it; read the way that cannot spend the uploads' share — each key a
// write — one batch of five hundred out reaches a quarter of the prefix's
// writes only if it is answered in under 0.57 s, about what the sixteen single
// deletions out reached, while a second batch out would take as much again
// from the company's uploads on the same prefix. What a
// batch buys is the requests: a bulk removal of a million objects is two
// thousand of them rather than a million, at the pace the prefix allowed
// anyway. Whether S3 counts a batch as one write is a measurement against a
// real bucket nobody has made; if it does, this is the number to raise.
const batchesInFlight = 1

// rangePage is how many names one request of a ranged listing answers on the
// store the ranged audit exists for: S3's ListObjectsV2, at most a thousand
// keys a page.
const rangePage = 1000

// rangePageRoom is how much of a page a range is cut short of, for the objects
// in it the walk could not count before it cut the range ([rangeChunk],
// [auditor.cut]).
//
// A range holds every object the store keeps between its references, and a
// page that fills before the range's last key costs a second request — one
// that lists most of the next chunk's range besides, which that chunk then
// lists again. So a chunk cut at the page itself took two requests whenever
// its range held one object more than its references: a bucket's everyday
// garbage doubled the audit (measured against the in-process S3 at a thousand
// references a chunk: nineteen listings for ten thousand references with one
// object in a thousand unnamed, against ten with none). What a range holds
// besides its references is what no required row names — an object a row
// stopped naming within the hour, which the next collection deletes; one a
// retired row keeps for its grace; one a derived row names, which an index
// kept over a repository's packs puts among the packs' keys at as many as one
// for every one to eight of them; and, among the last day's keys, every upload
// not yet named and every version of a file rewritten during the day
// ([PendingGrace]). That density is the store's, not a constant's — near
// nothing for a company that keeps files alone, up to as many objects again as
// references where its rows are mostly packs — so the walk MEASURES it from the
// ranges it has listed and cuts each range to fill the page less this room
// ([auditor.cut]). The room is for what the measurement cannot see: how much
// fuller a range is than the ones before it. A room for the density itself,
// as there was when every chunk was nine hundred references, is right for
// one density alone: a tenth of the page held a few objects in a thousand
// with an order of magnitude to spare, and any more than one in nine cost
// every chunk a second request — an index kept over packs at one object to
// every few of them doubled the audit's requests. Cut by the names, a store
// with a few objects in a hundred between its references pays up to a ninth
// more requests, where nine hundred references would still have fitted (3%
// at one in fifty, measured — the package doc), and every denser store fills
// its pages.
//
// A TENTH OF THE PAGE: placed independently, the objects a range of a whole
// page holds besides its references number at most nine hundred, so they vary
// from range to range by at most thirty, the root of that, and a hundred is
// three of those — about one range in two thousand overflows. Objects uploaded
// together — the parts of one large object a domain stores in pieces — sort
// together and widen that spread by the root of how many go at once: a tenth
// still holds a handful going at once, and a burst of a dozen or more is rare
// among the ranges.
// Less room pays a second request wherever a range is a little fuller than the
// last; more pays for part of a page on every chunk: at a tenth, an audit of a
// store that holds nothing but its references makes a ninth more listings than
// chunks of a whole page would.
const rangePageRoom = rangePage / 10

// rangeChunk is how many names a range is cut to hold, consecutive in key
// order, when the audit asks a store that lists in order about them with one
// ranged listing ([objstore.Ranged]): a page less the room the measured density
// cannot see ([rangePageRoom]), so a range costs one listing request where it
// would cost a HEAD of each object. It is also the MOST references a range
// holds — every one of them where the store keeps nothing between them, and
// fewer by the density it does ([auditor.cut]). Larger, each question out holds
// more references in memory and a range overflows its page; smaller, a chunk
// pays a request for less of a page.
const rangeChunk = rangePage - rangePageRoom

// rangeDensityWeight is how far the audit's estimate of a range's density
// moves toward each range it lists: an eighth of the way ([auditor.measured]).
//
// AN EIGHTH, so the estimate is a mean of about the last fifteen ranges
// listed: a range's own spread — the thirty names at worst that
// [rangePageRoom] holds — is cut to about a quarter of it in what the walk
// cuts by, while a change of density along the keys — a domain that began
// keeping derived objects among its references, or the last day's uploads —
// is followed within those fifteen ranges, behind the [auditConcurrency]
// ranges already out when it began. Weighted any heavier, one range crowded
// by a burst of uploads would shrink the next ranges for nothing; any
// lighter, a change of density would cost more ranges a second request each.
const rangeDensityWeight = 8

// failedInARow is how many questions in a row the store may fail, with none
// answered between, before the audit stops as a store failing rather than
// one refusing particular objects.
//
// THE AUDIT'S OWN BOUND, thirty-two. A store that has gone away fails every
// question the audit has out, so its first full window of answers stops the
// audit — with no more than the window asked in vain, and none of them taken
// for a finding: a failed audit's counts never reach [AuditReport.Found]. A
// store answering its other questions while it fails some breaks the run at
// every answer, and the objects it fails are set aside for their second look
// instead. Smaller, a run of failures as short as the requests that were in
// flight when a store hiccupped would stop an audit that could have run to its
// end; larger, a store that is down would be sent more than one window of
// questions it cannot answer.
const failedInARow = auditConcurrency

// MissingShown is how many unreadable files a status names: the counts are
// always whole, and the list — the first by name — is what an operator
// restores first.
const MissingShown = 100

// maxSuspects is how many references the audit holds at once for their second
// look ([Collector.audit]) — each waiting alone, or in a range whose listing
// failed ([lookAgain]).
//
// A HUNDRED THOUSAND, about twenty mebibytes of references at some two hundred
// bytes each, a queue entry's own included. The second look is for a store that
// had not caught up with an upload when it was asked — a handful of "not
// found" answers at a time — or failed a question, and a store that has given
// a hundred thousand of them has lost those objects rather than lagged. Past
// the bound the OLDEST is looked at again at once rather than at the end of
// the walk, so an audit of a store that lost everything holds twenty mebibytes
// rather than the company's whole inventory — two million references, four
// hundred mebibytes, on the one node whose alarm is the only thing saying the
// files are gone. Every object still gets its one second look, and the oldest
// gets it no sooner than a hundred thousand questions later — however fast the
// store answers, a hundred thousand of its answers after the one that missed,
// which is the room a store that lists a new object late is given to show it.
const maxSuspects = 100_000

// auditWindow is how many listed objects one window of the audit's listing of
// a store that keeps no order holds in memory ([auditor.listWindow]).
//
// A HUNDRED THOUSAND, about thirteen mebibytes: a window keeps 127 bytes a key
// — the key in its map and its heap, and the size and digest [judged] reads,
// the digest a string of its own as the broker's listing decodes one
// (BenchmarkAuditWindow; 2026-10-24, four vCPUs of a 2.3 GHz Xeon) — beside
// [maxSuspects]' twenty, on the one node that runs the duty.
//
// WHAT IT TRADES IS LISTINGS FOR MEMORY. Every window is a listing of the
// whole store, so an audit of N objects lists N × ceil(N / auditWindow) names,
// which grows with the square of the store past one window. On the broker,
// whose listing names 63,000–94,000 objects a second and whose stats answer
// 15,000–19,000 a second at [auditConcurrency] on a member's own client and
// 5,600–23,400 from a follower (internal/objstore/natsobj's package doc): a
// hundred thousand objects are one listing of one or two seconds against five
// to seven of stats; a million, ten listings and two to three minutes against
// about one; two million, twenty listings and seven to eleven minutes against
// two on a member and up to six from a follower. What the listing saves at
// every size is REQUESTS — its consumer pulls five hundred names a request,
// from whichever replica the broker places it on, so two million objects are
// eighty thousand pulls where a stat apiece was two million reads from the
// stream's leader — and on an S3 directory bucket, which lists a thousand
// names a request at 31,400–33,000 a second and answers HEADs at 1,380–1,450
// a second with thirty-two out from 20 ms away (s3obj's package doc, against
// an in-process fake held that far away), two million objects are forty
// thousand listing requests in about twenty minutes against two million HEADs
// in twenty-three or twenty-four. A larger window lists
// fewer times for 127 bytes a key more — a million keys is 127 MB on one
// node — and a smaller one the reverse.
const auditWindow = 100_000

// Status is what the collector last found, for the alarm, the status surface
// and the next duty holder.
type Status struct {
	// Collect is the last collection pass, and Audit the last audit.
	Collect CollectionReport `json:"collect"`
	Audit   AuditReport      `json:"audit"`
}

// CollectionReport is what one collection pass did.
type CollectionReport struct {
	// Completed is whether the pass listed the whole store and judged
	// every object old enough to be judged.
	Completed bool `json:"completed"`

	// Listed is how many objects the store held under the engine's
	// namespace; Aged how many of them were past the grace; Deleted how
	// many of those no row named and the pass deleted; Referenced how
	// many a row — live, retired or derived — still named.
	Listed     int `json:"listed"`
	Aged       int `json:"aged"`
	Deleted    int `json:"deleted"`
	Referenced int `json:"referenced"`

	// Abandoned is how many uploads begun and never finished the pass
	// abandoned.
	Abandoned int `json:"abandoned"`

	// Skipped says why the pass stopped judging, empty when it ran in
	// full. What it deleted before it stopped is counted above either way.
	Skipped string `json:"skipped,omitempty"`

	// SweepError is what stopped the sweep of unfinished uploads — which
	// the collection does not wait on, and so does not fail with.
	SweepError string `json:"sweep_error,omitempty"`

	// At is when the pass ended, and Error what stopped it.
	At    time.Time `json:"at"`
	Error string    `json:"error,omitempty"`
}

// AuditReport is the last audit attempt, and what the last one that ran to its
// end found.
//
// The attempt's fields describe that attempt whatever became of it — a failed
// one's counts are floors — and the findings that must outlive a failed
// attempt are [AuditReport.Found], beside them. The record is the fleet's, so
// it evolves additively: a successor build reads it during a rolling upgrade.
type AuditReport struct {
	// Completed is whether the attempt asked about every object the
	// estate's required tables name, in an estate that was complete;
	// Referenced how many it asked about, Missing how many the store does
	// not hold, Damaged how many it holds wrong and Unanswered how many it
	// failed to answer about on both looks ([Finding]) — floors, for an
	// attempt that did not complete.
	Completed  bool `json:"completed"`
	Referenced int  `json:"referenced"`
	Missing    int  `json:"missing"`
	Damaged    int  `json:"damaged"`
	Unanswered int  `json:"unanswered"`

	// At is when the attempt ended, and Error what stopped it.
	At    time.Time `json:"at"`
	Error string    `json:"error,omitempty"`

	// Found is what the last audit that ran to its end found — this one,
	// or the one before it when this one failed — and absent before any
	// has. It is what the alarm counts, so a failed attempt never clears
	// it.
	Found *AuditFindings `json:"found,omitempty"`
}

// AuditFindings is what an audit that ran to its end found.
type AuditFindings struct {
	// At is when that audit ended, and Completed whether its estate was
	// complete — when it was not, Missing and Damaged are floors.
	At        time.Time `json:"at"`
	Completed bool      `json:"completed"`

	// Referenced is how many objects its required tables named; Missing
	// how many of them the store does not hold, Damaged how many it holds
	// at another size, or under another digest where it keeps one, or as no
	// object it can read, and Unanswered how many it failed to answer
	// about on both looks ([Finding]).
	Referenced int `json:"referenced"`
	Missing    int `json:"missing"`
	Damaged    int `json:"damaged"`
	Unanswered int `json:"unanswered"`

	// MissingFiles names the [MissingShown] files that come first by name
	// (NamedBy, then the key) among those that cannot be read, whatever
	// was found of each — by name rather than by when the audit found
	// them, which its questions running at once leave to chance.
	MissingFiles []MissingFile `json:"missing_files,omitempty"`
}

// Lost is how many files the findings say cannot be read: what the
// `objects_missing` alarm counts and a status lists.
func (f *AuditFindings) Lost() int { return f.Missing + f.Damaged + f.Unanswered }

// Finding is what the audit found of a file it cannot count on reading.
//
// ONE OF THREE, never a pair of flags: each sends an operator somewhere
// different — a backup for a missing file, the store's own repair for a
// damaged one, the store's health for one it would not answer about — and two
// flags would spell a fourth case that means nothing.
type Finding string

const (
	// FindingMissing is an object the store answered, on both looks, that
	// it does not hold.
	FindingMissing Finding = "missing"
	// FindingDamaged is one it holds at another size, or under another
	// digest where it keeps one, or as no object it can read at all.
	FindingDamaged Finding = "damaged"
	// FindingUnanswered is one the store failed to answer about on both
	// looks, the walk apart, while it answered the audit's other
	// questions.
	FindingUnanswered Finding = "unanswered"
)

// Findings is every finding an audit makes, in the order the dashboard's copy
// lists them (`OBJECT_FINDINGS`) — one list, so the engine's idea of the set
// and the one a gate holds the screen to cannot part.
func Findings() []Finding {
	return []Finding{FindingMissing, FindingDamaged, FindingUnanswered}
}

// Valid reports whether f is one of the three findings.
func (f Finding) Valid() bool { return slices.Contains(Findings(), f) }

// MissingFile is one file whose bytes the store cannot give back.
type MissingFile struct {
	// Object is the object the file's row names.
	Object objstore.Key `json:"object"`
	// NamedBy is the file, as its row's owner columns spell it — a
	// project and a path, `ENG/reports/q3.md`.
	NamedBy string `json:"named_by"`
	// Finding is what the audit found of it.
	Finding Finding `json:"finding"`
}

// clone is f with a list of its own.
func (f *AuditFindings) clone() *AuditFindings {
	if f == nil {
		return nil
	}
	out := *f
	out.MissingFiles = slices.Clone(f.MissingFiles)
	return &out
}

// Options builds a [Collector].
type Options struct {
	// Store is the object store the collector lists and deletes from.
	Store *objstore.Store
	// References is every source of object references ([Sources]).
	References References
	// Now is the clock the grace is measured on, injected for tests.
	Now func() time.Time
}

// Collector runs collection and audits. ONE node runs it at a time, as a
// fleet duty: two collectors would each be safe — a deletion needs no lock
// (see the package doc), and deleting what another already deleted is not an
// error — but would pay every request twice.
type Collector struct {
	opts Options

	// suspectsHeld is [maxSuspects], rangeChunk [rangeChunk] and window
	// [auditWindow], held on the value so a test can overflow the first
	// without a hundred thousand references and cross the others without
	// thousands — every use reading the field, never the constant.
	suspectsHeld int
	rangeChunk   int
	window       int

	mu     sync.Mutex
	status Status
}

// New builds a collector.
func New(opts Options) (*Collector, error) {
	if opts.Store == nil {
		return nil, errors.New("objstore/collect: the collector needs the object store")
	}
	if len(opts.References) == 0 {
		// REFUSED, never run with nothing: a collector with no source
		// reads every object in the store as unreferenced and deletes the
		// lot a day later.
		return nil, errors.New("objstore/collect: the collector needs at least one source of references")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Collector{opts: opts, suspectsHeld: maxSuspects, rangeChunk: rangeChunk, window: auditWindow}, nil
}

// Status is what the last passes found.
func (c *Collector) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.status
	out.Audit.Found = out.Audit.Found.clone()
	return out
}

// Restore seeds the status from a record of passes another node ran — the
// fleet's, read when this node takes the duty — keeping each half that is
// newer than what this node has. Without it, the first pass a new holder
// recorded would carry the other half empty: the last audit's findings gone
// from the fleet's record, and its alarm with them, an hour after the duty
// moved.
func (c *Collector) Restore(s Status) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s.Collect.At.After(c.status.Collect.At) {
		c.status.Collect = s.Collect
	}
	found := c.status.Audit.Found
	if s.Audit.At.After(c.status.Audit.At) {
		c.status.Audit = s.Audit
		c.status.Audit.Found = found
	}
	if s.Audit.Found != nil && (found == nil || s.Audit.Found.At.After(found.At)) {
		c.status.Audit.Found = s.Audit.Found.clone()
	}
}

// Collect deletes the objects nothing refers to past their grace and abandons
// the uploads that never finished. See the package doc for why each of its
// rules is needed, and why none is a lock.
func (c *Collector) Collect(ctx context.Context) (CollectionReport, error) {
	var r CollectionReport
	cutoff := c.opts.Now().Add(-PendingGrace)
	err := c.collect(ctx, &r, cutoff)
	if ctx.Err() == nil {
		c.sweep(ctx, &r, cutoff)
	} else if err != nil {
		err = stopped(ctx, "pass")
	}
	r.At = c.opts.Now().UTC()
	if err != nil {
		r.Error = err.Error()
	}
	c.mu.Lock()
	c.status.Collect = r
	c.mu.Unlock()
	if r.Deleted > 0 || r.Abandoned > 0 || r.Skipped != "" ||
		r.SweepError != "" || err != nil {
		log.InfoContext(ctx, "objects_collected", "listed", r.Listed, "aged", r.Aged,
			"deleted", r.Deleted, "abandoned", r.Abandoned,
			"skipped", r.Skipped, "sweep_error", r.SweepError,
			"completed", r.Completed, "error", r.Error)
	}
	return r, err
}

// stopped is the error of a pass its caller stopped: why ctx ended, in place
// of the cancellation each of its calls to the store failed with. The engine
// ends a pass when its duty moves to another node, and a report and a log line
// reading "context canceled" would say nothing of that.
func stopped(ctx context.Context, pass string) error {
	return fmt.Errorf("objstore/collect: the %s was stopped: %w", pass, context.Cause(ctx))
}

// errPin stops a pass whose estate could not be brought current: it judges
// nothing, and reports why beside what it had deleted before.
type errPin struct{ err error }

func (e errPin) Error() string { return e.err.Error() }
func (e errPin) Unwrap() error { return e.err }

// errIncomplete stops a pass that met an estate it cannot call complete.
var errIncomplete = errors.New("a record this node could not apply may refer to objects in the store")

func (c *Collector) collect(ctx context.Context, r *CollectionReport, cutoff time.Time) error {
	// THE ESTATE IS PINNED AT THE FIRST JUDGEMENT, not before the listing:
	// a pass that finds nothing past the grace decides nothing, and a
	// barrier it took anyway would be a record on every domain's log each
	// hour, and on every log a node boots with, written for no reader. A
	// pin taken after the listing began is still at least as new as the
	// moment the pass began, which is all the rule asks: every name a key
	// past the grace will ever have was written before then.
	var (
		v      view
		pinned bool
	)
	// THE DELETIONS RUN BESIDE THE LISTING, at most [deleteConcurrency] at
	// once: the listing goes on judging the next batch while the last one's
	// are in flight, and waits only when all of them are. The estate is still
	// read by this goroutine alone — only the store's deletions are parallel —
	// and a deletion that fails ends dctx, which stops the listing.
	//
	// ON A STORE THAT DELETES IN BATCHES ([objstore.BatchDeleter]) a judged
	// batch's unnamed objects go in ONE request, [batchesInFlight] of them
	// out at once — a slot a batch waits for in its own goroutine, so the
	// listing still runs ahead — until the store refuses a batch as a request
	// ([objstore.ErrNoBatch]): a gateway without the verb, from which the
	// rest of the pass deletes each object on its own.
	deletes, dctx := errgroup.WithContext(ctx)
	deletes.SetLimit(deleteConcurrency)
	var (
		deleted atomic.Int64
		batched atomic.Bool
		refused sync.Once
		slot    = make(chan struct{}, batchesInFlight)
	)
	batched.Store(c.opts.Store.BatchDeletes())
	each := func(keys []objstore.Key) error {
		for _, k := range keys {
			if derr := c.opts.Store.Delete(dctx, k); derr != nil {
				return derr
			}
			deleted.Add(1)
		}
		return nil
	}
	inBatch := func(keys []objstore.Key) error {
		select {
		case slot <- struct{}{}:
		case <-dctx.Done():
			return dctx.Err()
		}
		if !batched.Load() {
			// A BATCH BEFORE IT WAS REFUSED, given its slot only now:
			// deleted one at a time, not offered to a store that just
			// refused the verb.
			<-slot
			return each(keys)
		}
		n, derr := c.opts.Store.DeleteKeys(dctx, keys)
		deleted.Add(int64(n))
		if errors.Is(derr, objstore.ErrNoBatch) {
			batched.Store(false) // before the slot goes, so no batch queued behind it is sent
			refused.Do(func() {
				log.WarnContext(ctx, "objects_collect_one_by_one", "error", derr,
					"detail", "the object store refused a batch deletion as a request, so the "+
						"collection deletes each object on its own: one request an object")
			})
		}
		<-slot
		if !errors.Is(derr, objstore.ErrNoBatch) {
			return derr
		}
		return each(keys)
	}
	var batch []objstore.Key
	judge := func() error {
		if len(batch) == 0 {
			return nil
		}
		defer func() { batch = batch[:0] }()
		if !pinned {
			at, perr := c.opts.References.pin(dctx)
			if perr != nil {
				return errPin{perr}
			}
			v, pinned = at, true
		}
		refs, complete, rerr := c.opts.References.referenced(dctx, batch, v)
		if rerr != nil {
			return rerr
		}
		if !complete {
			return errIncomplete
		}
		var unnamed []objstore.Key
		for _, k := range batch {
			if _, named := refs[k]; named {
				r.Referenced++
				continue
			}
			unnamed = append(unnamed, k)
		}
		if len(unnamed) == 0 {
			return nil
		}
		if derr := dctx.Err(); derr != nil {
			return derr // a deletion failed: judge nothing more
		}
		if batched.Load() {
			deletes.Go(func() error { return inBatch(unnamed) })
			return nil
		}
		for _, k := range unnamed {
			if derr := dctx.Err(); derr != nil {
				return derr // a deletion failed: judge nothing more
			}
			deletes.Go(func() error { return each([]objstore.Key{k}) })
		}
		return nil
	}
	backend := c.opts.Store.Backend()
	err := backend.List(dctx, func(info objstore.Info) error {
		// THE GRAMMAR IS READ HERE, not by the backend: see the package
		// doc for the two kinds of name.
		k, ours := keyNamed(info.Name)
		if !ours {
			return nil
		}
		r.Listed++
		// PAST THE GRACE BY BOTH CLOCKS: the backend's, which dates the
		// bytes, and the key's own, which is what the bound on the write
		// naming it is measured from.
		if !info.Written.Before(cutoff) || !k.Minted().Before(cutoff) {
			return nil
		}
		r.Aged++
		batch = append(batch, k)
		if len(batch) < JudgeBatch {
			return nil
		}
		return judge()
	})
	// A LISTING THAT LEFT UNDATED NAMES OUT still handed on every name it
	// could date, so the batch it filled is judged before the pass reports
	// what it could not list.
	if err == nil || errors.Is(err, objstore.ErrUndated) {
		if jerr := judge(); jerr != nil {
			err = errors.Join(err, jerr)
		}
	}
	// ON EVERY PATH, so no deletion outlives the pass, and what was deleted
	// before it stopped is counted beside why it stopped.
	derr := deletes.Wait()
	r.Deleted = int(deleted.Load())
	if derr != nil {
		// A DELETION FAILED, and that is the pass's failure. The listing's
		// own error rides beside it unless it is only the cancellation the
		// failed deletion caused; an estate it could not call complete is
		// reported where it always is.
		if errors.Is(err, errIncomplete) {
			r.Skipped = err.Error()
		} else if err != nil && (ctx.Err() != nil || !errors.Is(err, context.Canceled)) {
			derr = errors.Join(derr, err)
		}
		return fmt.Errorf("objstore/collect: %w", derr)
	}
	if pe := (errPin{}); errors.As(err, &pe) {
		r.Skipped = pe.err.Error()
		return pe.err
	}
	if errors.Is(err, errIncomplete) {
		r.Skipped = err.Error()
		return nil
	}
	if err != nil {
		return fmt.Errorf("objstore/collect: %w", err)
	}
	r.Completed = true
	return nil
}

// sweep abandons the uploads begun more than the grace ago and never
// finished: see the package doc. A failure is the report's, never the pass's.
func (c *Collector) sweep(ctx context.Context, r *CollectionReport, cutoff time.Time) {
	backend := c.opts.Store.Backend()
	err := backend.Pending(ctx, func(p objstore.Pending) error {
		if !p.Started.Before(cutoff) {
			return nil
		}
		k, ours := keyNamed(p.Name)
		switch {
		case p.Name == "":
			// NAMED BY NOTHING, so nothing could ever read or delete
			// it: the pieces of a put that died before naming them.
		case ours:
			// A KEY'S UPLOAD, past the grace by its key's own instant
			// too, for the rule a finished object is judged by.
			if !k.Minted().Before(cutoff) {
				return nil
			}
		default:
			// SOMEBODY ELSE'S upload, under a shared prefix: theirs.
			return nil
		}
		if err := backend.Abandon(ctx, p); err != nil {
			return err
		}
		r.Abandoned++
		return nil
	})
	if err != nil {
		r.SweepError = err.Error()
		log.WarnContext(ctx, "objects_sweep_failed", "error", err, "abandoned", r.Abandoned,
			"detail", "uploads that never finished are kept, and billed, until a sweep can "+
				"list and abandon them — on S3 the identity needs "+
				"s3:ListBucketMultipartUploads and s3:AbortMultipartUpload")
	}
}

// Audit asks the store for every object a required table names, and counts the
// ones it does not hold and the ones it holds wrong.
func (c *Collector) Audit(ctx context.Context) (AuditReport, error) {
	var r AuditReport
	files, err := c.audit(ctx, &r)
	if err != nil && ctx.Err() != nil {
		err = stopped(ctx, "audit")
	}
	r.At = c.opts.Now().UTC()
	c.mu.Lock()
	if err != nil {
		r.Error = err.Error()
		r.Found = c.status.Audit.Found.clone()
	} else {
		r.Found = &AuditFindings{At: r.At, Completed: r.Completed, Referenced: r.Referenced,
			Missing: r.Missing, Damaged: r.Damaged, Unanswered: r.Unanswered, MissingFiles: files}
	}
	c.status.Audit = r
	c.mu.Unlock()
	if r.Missing > 0 || r.Damaged > 0 || r.Unanswered > 0 || err != nil {
		log.WarnContext(ctx, "objects_audited", "referenced", r.Referenced,
			"missing", r.Missing, "damaged", r.Damaged, "unanswered", r.Unanswered,
			"completed", r.Completed, "error", r.Error,
			"detail", "objects the company's files are kept in are missing or damaged in the "+
				"object store, or the store would not answer for them")
	}
	return r, err
}

func (c *Collector) audit(ctx context.Context, r *AuditReport) ([]MissingFile, error) {
	v, err := c.opts.References.pin(ctx)
	if err != nil {
		return nil, err
	}
	a := &auditor{store: c.opts.Store, r: r, q: suspects{limit: c.suspectsHeld}, window: c.window,
		probed: make(chan struct{})}
	if !c.opts.Store.Ranged() {
		a.mode.Store(askListing)
	}
	m := mergeWalks(c.opts.References, v)

	// THE WALK READS THE ESTATE ON THIS GOROUTINE ALONE, in key order, and
	// hands the references on to questions, at most [auditConcurrency] of
	// them out at once: a hand-off waits for a free slot, so the walk reads
	// its next page only once the questions have caught up with this one,
	// and a question that stops the audit ends actx, which stops the walk at
	// its next reference. A question is a listing of a range of references
	// on a store that lists in order — as many as fill [rangeChunk] names at
	// the density the ranges before it listed ([auditor.cut]) — a second
	// look's stat, or a stat of one object once the audit asks by name. On a
	// store that keeps no order the walk itself takes each reference's first
	// look, from the window of the store's listing it falls in
	// ([auditor.listWindow]): a listing is a request between two reads of the
	// estate, never inside one, and the walk needs its window before it can
	// go on.
	asks, actx := errgroup.WithContext(ctx)
	asks.SetLimit(auditConcurrency)
	var (
		walkErr error
		chunk   []objstore.Reference
		// cut is how many references the chunk being gathered is cut at,
		// decided as it begins.
		cut int
		// after is the last key of the chunk before this one, so the
		// chunks' ranges tile the key space: (after, chunk's last]. The
		// listing's first window begins there too, so a walk the store
		// answered a range of out of order lists only what no range
		// already holds.
		after objstore.Key
		// probing is whether the walk has yet to hand its first range on.
		probing = true
		// win is the listing's window the walk is in, nil until it
		// needs one.
		win *window
	)
	ask := func(ref objstore.Reference) { asks.Go(func() error { return a.first(actx, ref) }) }
	list := func() {
		refs, from := chunk, after
		asks.Go(func() error { return a.firstRange(actx, from, refs) })
		after, chunk = refs[len(refs)-1].Object.Key, nil
		if !probing {
			return
		}
		probing = false
		// THE FIRST RANGE IS LISTED BEFORE THE SECOND IS CUT, so every range
		// but the first is cut from a density the store's own listing
		// measured rather than guessed: the walk would otherwise hand on up
		// to [auditConcurrency] ranges, thirty thousand references, before
		// any came back. It waits once an audit, for one listing, whatever
		// that listing answers — and for nothing once the audit is stopping.
		select {
		case <-a.probed:
		case <-actx.Done():
		}
	}
	// listed is ref's first look from the window of the store's listing its
	// key falls in, listing the next window first once the walk has passed
	// the last one's: a reference the window holds is held, or held wrong, as
	// the listing says, and one it does not waits for a second look by name.
	// A second look the queue hands back for one at once is asked as a
	// question of its own, since the walk holds no slot. A listing that fails
	// turns the audit to asking by name, this reference first.
	listed := func(ref objstore.Reference) error {
		for win == nil || (!win.last && ref.Object.Key.Compare(win.through) > 0) {
			from := after
			if win != nil {
				from = win.through
			}
			next, err := a.listWindow(actx, from)
			switch {
			case err != nil && actx.Err() != nil:
				return err
			case err != nil:
				if ferr := a.failed(fmt.Sprintf("listing the objects after %s", from), err); ferr != nil {
					return ferr
				}
				a.listingFailed(actx, err)
				ask(ref)
				return nil
			}
			a.answered()
			win = &next
		}
		// A NAME LISTED HOLDING NOTHING IS ASKED ABOUT BY NAME, as though
		// the listing had not shown it: the broker's listing hands on a
		// name whose metadata does not decode, and one only a delete
		// marker holds, as an object of no bytes and no digest — which
		// is also how an empty file's object reads from a listing that
		// keeps no digest. Judged from the listing, the marker's object
		// would be damaged where it is missing, and a spoiled empty
		// file's held where it is damaged; a stat tells the three apart,
		// at a stat for each empty file, which a company has few of.
		got := absent
		if info, ok := win.held[ref.Object.Key]; ok && (info.size != 0 || info.digest != "") {
			got = judged(ref, objstore.Info{Size: info.size, Digest: info.digest})
		}
		for _, l := range a.firstAnswer(ref, got) {
			asks.Go(func() error { return a.again(actx, l) })
		}
		return nil
	}
	// hand gives ref its first look as the audit now asks — what a chunk
	// gathered for a range before the audit stopped asking in ranges
	// included, since those references come before ref.
	hand := func(ref objstore.Reference) error {
		if a.mode.Load() == askRanges {
			// A CHUNK ENDS BETWEEN TWO KEYS, never inside one: the next
			// range begins after the last key of this one, so a key on
			// both sides of the cut would be left out of the second.
			// A CHUNK'S CUT IS DECIDED AS IT BEGINS, from the density
			// measured by then ([auditor.cut]).
			switch {
			case len(chunk) == 0:
				cut = a.cut(c.rangeChunk)
			case len(chunk) >= cut && ref.Object.Key != chunk[len(chunk)-1].Object.Key:
				list()
				cut = a.cut(c.rangeChunk)
			}
			chunk = append(chunk, ref)
			return nil
		}
		gathered := chunk
		chunk = nil
		gathered = append(gathered, ref)
		for _, g := range gathered {
			if a.mode.Load() == askByName {
				ask(g)
				continue
			}
			if err := listed(g); err != nil {
				return err
			}
		}
		return nil
	}
	for {
		if aerr := actx.Err(); aerr != nil {
			break // the audit is stopping: walk no further
		}
		ref, ok, err := m.Next(actx)
		if err != nil {
			walkErr = err
			break
		}
		if !ok {
			break
		}
		r.Referenced++
		if err := hand(ref); err != nil {
			walkErr = err
			break
		}
	}
	if walkErr == nil && actx.Err() == nil && len(chunk) > 0 {
		if a.mode.Load() == askRanges {
			list()
		} else {
			// THE AUDIT STOPPED ASKING IN RANGES after the walk's last
			// reference was gathered for one: the chunk is handed as it
			// asks now.
			last := chunk[len(chunk)-1]
			chunk = chunk[:len(chunk)-1]
			walkErr = hand(last)
		}
	}
	// ON EVERY PATH, so no question outlives the pass. A question that
	// stopped the audit is the root of a walk that stopped for it, so it is
	// the one reported.
	if err := asks.Wait(); err != nil {
		return nil, fmt.Errorf("objstore/collect: %w", err)
	}
	if walkErr != nil {
		return nil, fmt.Errorf("objstore/collect: %w", walkErr)
	}
	// THE SECOND LOOKS STILL WAITING, at the same bound, in a group of their
	// own: the walk's group ended its context with its Wait.
	again, gctx := errgroup.WithContext(ctx)
	again.SetLimit(auditConcurrency)
	a.mu.Lock()
	waiting := a.q.drain()
	a.mu.Unlock()
	for _, l := range waiting {
		if gctx.Err() != nil {
			break // the audit is stopping: ask no more
		}
		again.Go(func() error { return a.again(gctx, l) })
	}
	if err := again.Wait(); err != nil {
		return nil, fmt.Errorf("objstore/collect: %w", err)
	}
	if err := a.confirm(ctx); err != nil {
		return nil, fmt.Errorf("objstore/collect: %w", err)
	}
	// AN AUDIT WHOSE CONTEXT ENDED HAS NOT RUN TO ITS END, however its
	// questions fared. The walk and the drain both stop BETWEEN two questions
	// once it ends, so a stop that lands while nothing is out — routine on a
	// ranged walk, which asks nothing while it gathers a chunk — starts no
	// question that could fail, and every Wait above answers nil. What it
	// counted is then a floor: reported as the findings, it would replace the
	// last whole audit's and clear the alarm they raise.
	if ctx.Err() != nil {
		return nil, fmt.Errorf("objstore/collect: %w", context.Cause(ctx))
	}
	r.Completed = m.Complete()
	return a.shown, nil
}

// auditor is one audit's questions: what they found, and what the store's
// answers say of the store.
type auditor struct {
	store *objstore.Store

	// mode is how the audit takes each reference's first look: in ranges
	// ([askRanges]) on a store that lists in order, from the store's listing
	// a window at a time ([askListing]) on one that keeps no order and from
	// the first range one answered out of order ([objstore.ErrUnordered]),
	// and by name ([askByName]) from the first window whose listing failed.
	// It only ever moves down that list.
	mode atomic.Int32
	// window is how many listed objects a window holds ([auditWindow]).
	window int
	// unsorted and unlisted log each turn of the mode once an audit.
	unsorted, unlisted sync.Once

	// probed is closed once the first range handed on has come back,
	// whatever it answered, which the walk waits for before it cuts its
	// second ([auditor.probe]).
	probed    chan struct{}
	probeOnce sync.Once

	// mu guards everything below, written by up to [auditConcurrency]
	// questions at once.
	mu sync.Mutex
	// r is the attempt's report, whose counts the questions keep.
	r *AuditReport
	// shown is the files shown, in [compareShown]'s order.
	shown []MissingFile
	// q is the references and ranges waiting for their second look
	// ([suspects]).
	q suspects
	// failing is how many questions in a row the store has failed, with
	// none answered between ([failedInARow]).
	failing int
	// unconfirmed is the references whose second look the store failed
	// since it last answered a question: UNANSWERED once it answers one
	// after them, which says it was answering. From fewer than
	// [failedInARow] failed questions, since that many in a row stop the
	// audit — each a stat, or a re-listing of a range of up to
	// [rangeChunk].
	unconfirmed []objstore.Reference
	// settled is the last reference the store's answer finished with —
	// never one still waiting for a look, so never one of unconfirmed —
	// which [auditor.confirm] asks about again.
	settled    objstore.Reference
	hasSettled bool
	// density is the names a range lists for each reference it is cut
	// for, as the ranges listed so far measured it ([auditor.measured]);
	// zero until one has.
	density float64
}

// cut is how many references the walk gathers into its next range, the most
// being chunk ([rangeChunk]): as many as fill chunk names at the density the
// ranges listed so far held — every one of chunk where the store keeps
// nothing between them, or holds less than they name, and fewer, never none,
// the more it keeps between them. So a range fills a page short of its room
// ([rangePageRoom]) whatever the store holds besides the references: a
// derived row's objects, a retired row's, garbage.
func (a *auditor) cut(chunk int) int {
	a.mu.Lock()
	d := a.density
	a.mu.Unlock()
	if d <= 1 {
		return chunk
	}
	return max(1, int(float64(chunk)/d))
}

// measured folds one listed range into the density the walk cuts by: names
// is how many names its listing handed on, refs how many references it was
// cut for. The first range's density is taken as it is, each later one's a
// [rangeDensityWeight]th of the way.
func (a *auditor) measured(names, refs int) {
	if refs == 0 {
		return
	}
	d := float64(names) / float64(refs)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.density == 0 {
		a.density = d
		return
	}
	a.density += (d - a.density) / rangeDensityWeight
}

// probe says the first range handed on has come back ([auditor.probed]).
func (a *auditor) probe() { a.probeOnce.Do(func() { close(a.probed) }) }

// answer is what one question to the store found of one reference.
type answer int

const (
	// held: the store holds the object as its row records it.
	held answer = iota
	// damaged: the store holds it wrong, or as no object it can read.
	damaged
	// absent: the store answered that it holds nothing under the name.
	absent
	// unanswered: the store failed to answer.
	unanswered
)

// errStoreFailing is an audit the store stopped by failing rather than by
// refusing particular objects: [failedInARow] questions in a row with none
// answered between, or the questions at the audit's end and then one about an
// object it had answered for.
var errStoreFailing = errors.New("the object store is failing, rather than refusing particular objects")

// look asks the store about ref once. A failure is an answer like any other —
// [unanswered] — until it is the store's [failedInARow]th in a row, which is
// the audit's error; so is the audit's context ending, which is never the
// store's failure. Any other answer says the store is answering, which
// confirms every second look it failed since the last one.
func (a *auditor) look(ctx context.Context, ref objstore.Reference) (answer, error) {
	info, err := a.store.Stat(ctx, ref.Object.Key)
	var got answer
	switch {
	case errors.Is(err, objstore.ErrNotFound):
		got = absent
	case errors.Is(err, objstore.ErrCorrupt):
		// HELD, AND NO OBJECT ANYBODY CAN READ: a definite answer, as
		// damaged as a wrong digest.
		got = damaged
	case err != nil && ctx.Err() != nil:
		return 0, err
	case err != nil:
		if ferr := a.failed(fmt.Sprintf("asking for %s", ref.Object.Key), err); ferr != nil {
			return 0, ferr
		}
		return unanswered, nil
	default:
		got = judged(ref, info)
	}
	a.answered()
	return got, nil
}

// judged is what info, the store's account of ref's object, says of it.
func judged(ref objstore.Reference, info objstore.Info) answer {
	if info.Size != ref.Object.Size || (info.Digest != "" && info.Digest != ref.Object.Hash) {
		return damaged
	}
	return held
}

// answered records that the store answered a question: the run of failures is
// over, and every second look it failed since its last answer is UNANSWERED.
func (a *auditor) answered() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failing = 0
	for _, u := range a.unconfirmed {
		a.foundLocked(u, FindingUnanswered)
	}
	a.unconfirmed = a.unconfirmed[:0]
}

// failed records that the store failed a question — what, and with cause —
// and is the audit's error once that makes [failedInARow] in a row.
func (a *auditor) failed(what string, cause error) error {
	a.mu.Lock()
	a.failing++
	run := a.failing
	a.mu.Unlock()
	if run < failedInARow {
		return nil
	}
	return fmt.Errorf("%w: it failed %d questions in a row, answering none between — "+
		"the last, %s: %w", errStoreFailing, run, what, cause)
}

// first is ref's first look. Only a definite answer — held, or damaged — is
// taken as it stands; the rest wait for a second look, and what the queue hands
// back for one at once gets it here, in the slot this question already holds —
// never in a slot of its own, which the walk may be waiting on.
func (a *auditor) first(ctx context.Context, ref objstore.Reference) error {
	got, err := a.look(ctx, ref)
	if err != nil {
		return err
	}
	return a.againAll(ctx, a.firstAnswer(ref, got))
}

// firstAnswer takes got as ref's first look, and answers what the queue for
// second looks handed back for one at once ([auditor.wait]).
func (a *auditor) firstAnswer(ref objstore.Reference, got answer) []lookAgain {
	switch got {
	case held:
		a.settle(ref)
		return nil
	case damaged:
		a.settle(ref)
		a.found(ref, FindingDamaged)
		return nil
	}
	return a.wait(lookAgain{ref: ref})
}

// wait queues l for its second look, and answers what the queue hands back to
// be looked at once ([suspects.push]) — by the caller, after the request it is
// answering has ended.
func (a *auditor) wait(l lookAgain) []lookAgain {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.q.push(l)
}

// againAll is the second look of each of due, one after another.
func (a *auditor) againAll(ctx context.Context, due []lookAgain) error {
	for _, l := range due {
		if err := a.again(ctx, l); err != nil {
			return err
		}
	}
	return nil
}

// again is l's second look: a stat of a reference, or a listing of a range.
func (a *auditor) again(ctx context.Context, l lookAgain) error {
	if l.refs == nil {
		return a.second(ctx, l.ref)
	}
	return a.secondRange(ctx, l.after, l.refs)
}

// firstRange is the first look of every reference in chunk — consecutive in
// key order, the range (after, the last of them] — taken from ONE ranged
// listing of the store rather than a stat of each: a reference the listing
// hands on is held, or held wrong, as the listing says; one it passes over is
// one the store does not hold as far as the listing goes, and waits for a
// second look like any "not found". A chunk costs one request while its range
// fits a page ([rangeChunk]), and a range the store listed whole tells the walk
// how many names its next ranges may hold for each reference
// ([auditor.measured]).
//
// A LISTING IS ONE QUESTION to the store's account: one it answers ends a run
// of failures, and one that fails is a failure of the run, whose references it
// had not reached wait AS A RANGE for their second look — a listing of the
// range again ([auditor.secondRange]), never a stat of each. One the store
// cannot give in order turns the rest of the walk to the store's listing
// ([askListing]); this chunk's remaining references, which come before what the
// walk has reached, are asked about by name.
//
// NOTHING IS ASKED WHILE THE LISTING RUNS: what the queue hands back to be
// looked at once is looked at after it, so a question never holds another
// request open beneath it.
func (a *auditor) firstRange(ctx context.Context, after objstore.Key, chunk []objstore.Reference) error {
	if a.mode.Load() != askRanges {
		// HANDED ON BEFORE ANOTHER RANGE CAME BACK OUT OF ORDER, and only
		// now given its slot: asked by name, not listed in vain — the walk's
		// listing begins after it.
		a.probe()
		return a.eachByName(ctx, chunk)
	}
	i, names := 0, 0
	var due []lookAgain
	err := a.store.ListKeys(ctx, after, chunk[len(chunk)-1].Object.Key, func(k objstore.Key, info objstore.Info) error {
		names++
		for ; i < len(chunk) && chunk[i].Object.Key.Compare(k) < 0; i++ {
			due = append(due, a.firstAnswer(chunk[i], absent)...)
		}
		for ; i < len(chunk) && chunk[i].Object.Key == k; i++ {
			due = append(due, a.firstAnswer(chunk[i], judged(chunk[i], info))...)
		}
		return nil
	})
	// ONLY A RANGE LISTED TO ITS END is a density — one left out an
	// undated object or two included, which a stat dates — and the walk
	// goes on once this range has come back, whatever it answered.
	if err == nil || errors.Is(err, objstore.ErrUndated) {
		a.measured(names, len(chunk))
	}
	a.probe()
	switch {
	case errors.Is(err, objstore.ErrUnordered):
		a.unordered(ctx, err)
		if berr := a.eachByName(ctx, chunk[i:]); berr != nil {
			return berr
		}
		return a.againAll(ctx, due)
	case err != nil && ctx.Err() != nil:
		return err
	case err != nil && !errors.Is(err, objstore.ErrUndated):
		// THE LISTING FAILED: what it had not reached waits for its
		// second look as the range it is — after the last reference the
		// listing had reached, or where this one began.
		if ferr := a.failed(fmt.Sprintf("listing the objects after %s", after), err); ferr != nil {
			return ferr
		}
		if i < len(chunk) {
			from := after
			if i > 0 {
				from = chunk[i-1].Object.Key
			}
			// A COPY, so the range holds only what it waits with
			// rather than the whole chunk behind it.
			due = append(due, a.wait(lookAgain{after: from, refs: slices.Clone(chunk[i:])})...)
		}
		return a.againAll(ctx, due)
	}
	// ANSWERED — a listing that left an undated object out
	// ([objstore.ErrUndated]) included: what it passed over is asked about
	// by name, where a stat dates nothing.
	a.answered()
	for ; i < len(chunk); i++ {
		due = append(due, a.firstAnswer(chunk[i], absent)...)
	}
	return a.againAll(ctx, due)
}

// secondRange is the second look of refs — consecutive in key order, the range
// (after, the last of them] — whose first, a listing, failed: ONE listing of
// the range again, rather than a stat of each, which on a bucket that failed
// the listing because it is throttling the prefix is one request rather than
// nine hundred. What it lists is held, or held wrong, as it says — a definite
// answer, and the finding; what it passes over is asked about by name, and
// that answer is the finding, so no object is called missing on a listing
// alone. A re-listing that fails leaves every reference it had not reached
// UNANSWERED once the store answers after it ([auditor.unconfirmed]), as a
// second stat that failed would; one the store cannot give in order after all
// has each asked about by name.
func (a *auditor) secondRange(ctx context.Context, after objstore.Key, refs []objstore.Reference) error {
	i := 0
	var passed []objstore.Reference
	err := a.store.ListKeys(ctx, after, refs[len(refs)-1].Object.Key, func(k objstore.Key, info objstore.Info) error {
		for ; i < len(refs) && refs[i].Object.Key.Compare(k) < 0; i++ {
			passed = append(passed, refs[i])
		}
		for ; i < len(refs) && refs[i].Object.Key == k; i++ {
			if judged(refs[i], info) == damaged {
				a.found(refs[i], FindingDamaged)
			}
			a.settle(refs[i])
		}
		return nil
	})
	switch {
	case errors.Is(err, objstore.ErrUnordered):
		a.unordered(ctx, err)
		passed = append(passed, refs[i:]...)
	case err != nil && ctx.Err() != nil:
		return err
	case err != nil && !errors.Is(err, objstore.ErrUndated):
		if ferr := a.failed(fmt.Sprintf("listing the objects after %s again", after), err); ferr != nil {
			return ferr
		}
		a.mu.Lock()
		a.unconfirmed = append(a.unconfirmed, refs[i:]...)
		a.mu.Unlock()
	default:
		a.answered()
		passed = append(passed, refs[i:]...)
	}
	for _, ref := range passed {
		if err := a.second(ctx, ref); err != nil {
			return err
		}
	}
	return nil
}

// unordered turns the rest of the walk to the store's listing ([askListing]),
// the store having answered a range out of order with err.
func (a *auditor) unordered(ctx context.Context, err error) {
	if !a.mode.CompareAndSwap(askRanges, askListing) {
		return
	}
	a.unsorted.Do(func() {
		log.WarnContext(ctx, "objects_audit_listed", "error", err,
			"detail", "the object store cannot list a range of its objects in order, "+
				"so the audit reads its whole listing a window at a time instead, as "+
				"for a store that keeps no order")
	})
}

// listingFailed turns the rest of the audit to asking about each object by
// name ([askByName]), the store having failed a window's listing with err.
//
// NOT THE AUDIT'S FAILURE: a listing the store fails is one failed question,
// which the run of them counts ([failedInARow]) — a store that went away fails
// the stats that follow too, and stops the audit there. And never a second
// listing of the window: a window's extent is its listing's answer, so without
// one nothing says which references the next listing would cover.
func (a *auditor) listingFailed(ctx context.Context, err error) {
	if !a.mode.CompareAndSwap(askListing, askByName) {
		return
	}
	a.unlisted.Do(func() {
		log.WarnContext(ctx, "objects_audit_by_name", "error", err,
			"detail", "the object store failed a listing of its objects, so the audit "+
				"asks about each object it has not reached by name instead: one "+
				"request an object rather than a listing a window of them")
	})
}

// second is ref's second look, whose answer is the finding — but for a
// failure, which is UNANSWERED only once the store answers after it
// ([auditor.unconfirmed]).
func (a *auditor) second(ctx context.Context, ref objstore.Reference) error {
	got, err := a.look(ctx, ref)
	switch {
	case err != nil:
		return err
	case got == unanswered:
		a.mu.Lock()
		a.unconfirmed = append(a.unconfirmed, ref)
		a.mu.Unlock()
		return nil
	case got == damaged:
		a.found(ref, FindingDamaged)
	case got == absent:
		a.found(ref, FindingMissing)
	}
	a.settle(ref)
	return nil
}

// settle records that the store's answer finished with ref.
func (a *auditor) settle(ref objstore.Reference) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.settled, a.hasSettled = ref, true
}

// confirm settles the second looks the store failed after its last answer,
// once every question has ended: it asks again about the last object the
// store's answer finished with — never one of those waiting, whose own failure
// would say nothing of the store — and an answer says the store was answering,
// so those objects are UNANSWERED, while a failure, or a store that finished
// none all audit, says it is failing, which is the audit's error rather than
// a finding about each object.
//
// ONE QUESTION, ASKED ONLY WHEN SOMETHING WAITS ON IT: without it, the second
// looks are the audit's last questions, and a store that went away just then
// would have every one of them called unanswered for a day — while an
// object the store refuses every time would, as the last second look, never
// be confirmed at all.
func (a *auditor) confirm(ctx context.Context) error {
	a.mu.Lock()
	waiting, probe, ok := len(a.unconfirmed), a.settled, a.hasSettled
	a.mu.Unlock()
	switch {
	case waiting == 0:
		return nil
	case !ok:
		return fmt.Errorf("%w: it failed the audit's last %d questions, and gave no answer "+
			"that finished with any object", errStoreFailing, waiting)
	}
	if _, err := a.look(ctx, probe); err != nil {
		return err
	}
	a.mu.Lock()
	still := len(a.unconfirmed)
	a.mu.Unlock()
	if still > 0 {
		return fmt.Errorf("%w: it failed the audit's last %d questions, and then one about %s, "+
			"which it had answered", errStoreFailing, waiting, probe.Object.Key)
	}
	return nil
}

// found counts what the audit found of ref, and shows ref among the files that
// cannot be read if it is among the first [MissingShown] by name.
func (a *auditor) found(ref objstore.Reference, f Finding) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.foundLocked(ref, f)
}

// foundLocked is [auditor.found] with mu held.
func (a *auditor) foundLocked(ref objstore.Reference, f Finding) {
	switch f {
	case FindingMissing:
		a.r.Missing++
	case FindingDamaged:
		a.r.Damaged++
	case FindingUnanswered:
		a.r.Unanswered++
	}
	a.shown = showFirst(a.shown, MissingFile{Object: ref.Object.Key, NamedBy: ref.NamedBy, Finding: f})
}

// eachByName is the first look of each of refs, asked by name one after
// another in the slot of the range they were gathered for.
func (a *auditor) eachByName(ctx context.Context, refs []objstore.Reference) error {
	for _, ref := range refs {
		if err := a.first(ctx, ref); err != nil {
			return err
		}
	}
	return nil
}

// mergeWalks merges the required walks of every source into one key order
// ([objstore.MergeWalks]), each read no earlier than the view pinned for its
// source.
func mergeWalks(sources References, v view) *objstore.Merge {
	var walks []objstore.NamedWalk
	for i, s := range sources {
		for _, w := range s.Required(v[i]) {
			walks = append(walks, objstore.NamedWalk{Name: s.Name(), Walk: w})
		}
	}
	return objstore.MergeWalks(walks...)
}

// compareShown is the order the files that cannot be read are shown in: by the
// file, then by its object.
func compareShown(a, b MissingFile) int {
	if c := strings.Compare(a.NamedBy, b.NamedBy); c != 0 {
		return c
	}
	return strings.Compare(a.Object.String(), b.Object.String())
}

// showFirst is shown, kept in [compareShown]'s order, with f in it if f is among
// the first [MissingShown] — so a status names the same files whichever order
// the audit's questions were answered in, and never holds more than it shows.
func showFirst(shown []MissingFile, f MissingFile) []MissingFile {
	i, _ := slices.BinarySearchFunc(shown, f, compareShown)
	if i >= MissingShown {
		return shown
	}
	shown = slices.Insert(shown, i, f)
	if len(shown) > MissingShown {
		shown = shown[:MissingShown]
	}
	return shown
}

// lookAgain is one entry waiting for its second look: a reference whose first
// look was not definite — the store answered "not found", or failed to answer
// — or a RANGE whose listing failed, its references consecutive in key order
// after the key it is listed from.
type lookAgain struct {
	// ref is the reference, where refs is nil.
	ref objstore.Reference
	// after and refs are a range: the references (after, the last of
	// them].
	after objstore.Key
	refs  []objstore.Reference
}

// size is how many references l holds.
func (l lookAgain) size() int {
	if l.refs == nil {
		return 1
	}
	return len(l.refs)
}

// suspects is the queue of second looks ([lookAgain]), oldest first, holding
// at most limit references — a limit below one holds one, and so does a lone
// range larger than the limit — so the oldest entries are the ones handed back
// when a push would pass it.
//
// AN ENTRY HANDED BACK IS CLEARED from the array behind the queue, and the
// array is compacted once half of it is spent, so what the queue holds is what
// it counts: a slice re-sliced from the front would keep the references it
// handed back reachable from the array until the next append happened to copy.
type suspects struct {
	queue []lookAgain
	head  int // the oldest entry
	held  int // references across the entries from head
	limit int
}

// push queues l, and hands back the oldest entries while what is held passes
// the limit — the caller looks at them at once. Never the entry it was given
// alone: one range larger than the limit is held rather than looked at again
// the moment its first look failed.
func (q *suspects) push(l lookAgain) (due []lookAgain) {
	q.queue = append(q.queue, l)
	q.held += l.size()
	for q.held > max(q.limit, 1) && len(q.queue)-q.head > 1 {
		due = append(due, q.pop())
	}
	return due
}

// pop hands back the oldest entry.
func (q *suspects) pop() lookAgain {
	l := q.queue[q.head]
	q.queue[q.head] = lookAgain{}
	q.head++
	q.held -= l.size()
	if q.head*2 >= len(q.queue) {
		n := copy(q.queue, q.queue[q.head:])
		clear(q.queue[n:])
		q.queue, q.head = q.queue[:n], 0
	}
	return l
}

// drain hands back every entry still held, oldest first, and holds nothing.
func (q *suspects) drain() []lookAgain {
	out := slices.Clone(q.queue[q.head:])
	q.queue, q.head, q.held = nil, 0, 0
	return out
}

// How the audit takes each reference's first look ([auditor.mode]).
const (
	// askRanges: a ranged listing of each chunk of references, on a store
	// that lists in order ([objstore.Ranged]).
	askRanges int32 = iota
	// askListing: the store's whole listing, a window of keys at a time,
	// the references walked beside it ([auditor.listWindow]).
	askListing
	// askByName: a stat of each reference.
	askByName
)

// window is one window of the audit's listing of a store that keeps no order:
// what the store holds under every key in it, the last key in it, and whether
// it is the last window.
type window struct {
	held    map[objstore.Key]listedObject
	through objstore.Key
	last    bool
}

// listedObject is what a window keeps of one listed object: what [judged]
// reads, and nothing else — its name is its key, and when it was written the
// audit never asks.
type listedObject struct {
	size   int64
	digest objstore.Hash
}

// listWindow lists the store once and keeps the [auditor.window] smallest keys
// after `after` (the zero key: from the first) — the window. It is the last
// window when the listing held no more keys than that; otherwise it runs
// through the largest key kept, and the next window starts after it.
//
// ONE LISTING A WINDOW, AND THE WHOLE STORE EACH TIME: a store that keeps no
// order hands its names in an order of its own, so the only way to know which
// keys come first is to see them all. An audit of N objects is so
// ceil(N / [auditWindow]) listings, each naming all N, where a stat apiece was
// N requests; what that costs, and what it saves, is [auditWindow]'s to say.
// What the window bounds is the memory: a window of keys and what [judged]
// reads of each, never the company's inventory.
//
// AN UNDATED LISTING IS NO FAILURE HERE. An entry the backend could not date
// is left out of the listing ([objstore.ErrUndated]), so the reference naming
// it reads as unlisted and is asked about by name, which answers whether the
// object is there; the collection, which deletes, is the pass that must refuse
// to judge what it cannot date.
func (a *auditor) listWindow(ctx context.Context, after objstore.Key) (window, error) {
	held := map[objstore.Key]listedObject{}
	var largest keyHeap
	truncated := false
	err := a.store.Backend().List(ctx, func(info objstore.Info) error {
		k, ours := keyNamed(info.Name)
		if !ours || (!after.IsZero() && k.Compare(after) <= 0) {
			return nil
		}
		held[k] = listedObject{size: info.Size, digest: info.Digest}
		heap.Push(&largest, k)
		if largest.Len() > a.window {
			delete(held, heap.Pop(&largest).(objstore.Key))
			truncated = true
		}
		return nil
	})
	if err != nil && !errors.Is(err, objstore.ErrUndated) {
		return window{}, err
	}
	w := window{held: held, last: !truncated}
	if truncated {
		w.through = largest[0]
	}
	return w, nil
}

// keyHeap is a max-heap of keys: a window keeps the smallest keys it has
// seen, so the one it gives up is always the largest.
type keyHeap []objstore.Key

func (h keyHeap) Len() int           { return len(h) }
func (h keyHeap) Less(i, j int) bool { return h[i].Compare(h[j]) > 0 }
func (h keyHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *keyHeap) Push(x any)        { *h = append(*h, x.(objstore.Key)) }
func (h *keyHeap) Pop() any {
	old := *h
	k := old[len(old)-1]
	*h = old[:len(old)-1]
	return k
}

// keyNamed is the key a listed name stores, and whether it is one at all —
// false for every name the collector does not judge as a key
// ([objstore.KeyOfName]).
func keyNamed(name string) (objstore.Key, bool) {
	k, err := objstore.KeyOfName(name)
	return k, err == nil
}
