// Package collect is the atlas-collect duty: the fleet singleton that reads
// each connected integration's vendor whole, under the access the company's
// own credential already holds, and records on the atlas's log what the read
// found that the log does not hold yet (ADR-0129). Its first source reads
// Cloudflare: the account's name, every zone, every A, AAAA and CNAME record
// and every certificate pack. Its second reads Datadog: the organization,
// its monitors and their states, its service catalogue, its hosts and its
// Synthetic tests. Its third reads GitHub (ADR-0131): every repository the
// agents' installations cover, its workflows and its deployment environments.
//
// # Its own duty, never the reconcile loop
//
// The reconcile loop's contract forbids a write on a converged pass, its
// cadence is tuned to catching access somebody revoked, and its failures are
// findings about seats. An inventory read has a different cost, a different
// failure meaning and a different cadence (`atlas.refresh_interval_seconds`,
// hourly by default), so it is a duty of its own. Where the two read the same
// list (Cloudflare's zones), this one repeats one cheap call an hour, which is
// the price of keeping the two contracts apart.
//
// # A read is level-triggered, and records only what changed
//
// A [Source] reads the world and reports it whole; it is never told what
// changed. The duty reads every live claim the atlas log holds in that
// source's voice ([Log.LiveOf]), diffs the two ([Plan]) and publishes one
// record per entry whose claims moved: the claims that are new or changed,
// and the slots the read no longer holds, which the log withdraws. An
// unchanged read writes nothing, so an hourly read of a few thousand DNS
// records costs the log nothing an hour.
//
// # Retirement takes two complete reads a day apart
//
// A complete read that does not list an entry the voice holds is one miss,
// never a removal: a vendor's paginated listing can skip a row while the list
// changes under it. The first miss is remembered in the source's standing;
// an entry a later complete read at least [RetireAfter] after the first also
// misses is retired, with one denied exists claim and every other slot of the
// voice withdrawn. A read that failed or was cut short retires nothing and
// forgets no miss, and one that lists the entry again clears its miss.
//
// # A source reads in parts, and denies only what it said exists
//
// A vendor puts each listing behind a permission of its own, and a
// credential may hold some and not others (ADR-0130). A listing the vendor
// refuses is [Read.Unread]: the read is still whole, [Decide] carries every
// claim that part holds as the log has it, and the standing names the part.
// A source claims an entry ([Builder.Ref]) only where the entry lives at its
// vendor, and otherwise only names it ([Builder.Names]); an entry the voice
// never claimed is released by its retirement, never denied, since a denial
// would retire what another source holds.
//
// # Other voices use the same builder and the same diff
//
// A voice that is no integration's (a repository's files,
// internal/atlas/codescan) gathers its claims through [NewBuilderFor] and
// records them with [Replace], which withdraws at once what the voice no
// longer says: a reading that is exact at its instant needs no second miss.
//
// # Values are typed, and a credential has nowhere to go
//
// Every value a source reports goes through the vocabulary's grammars
// ([Builder]), which refuse a value shaped like a credential and every key
// so shaped; what is refused is counted and placed ([Read.Withheld]), never
// shown. A source maps a vendor's answer into closed fields and reads no
// field nobody mapped: Cloudflare's TXT records, comments and tags are never
// decoded.
//
// # Who has to agree on each piece of state
//
// What a read found: every data node, identically, so it is a record on the
// atlas log, written through [Log.Read] by the one node holding the duty.
// Each source's standing (its last complete read, that read's digest, the
// calls and withheld values it cost, the parts it was refused, and the
// entries it has stopped seeing since when): the whole company, current value only, so it is a record in
// the coordination store ([CoordStore]), beside the integration statuses and
// apart from them. Which node reads: the `worker:atlas-collect` lease, claimed
// per tick and re-claimed before every source.
package collect
