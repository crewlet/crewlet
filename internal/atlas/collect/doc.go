// Package collect is the atlas-collect duty: the fleet singleton that reads
// each connected integration's vendor whole, under the access the company's
// own credential already holds, and records on the atlas's log what the read
// found that the log does not hold yet (ADR-0129). Its first source reads
// Cloudflare: the account's name, every zone, every A, AAAA and CNAME record
// and every certificate pack. Its second reads Datadog: the organization,
// its monitors and their states, its service catalogue, its hosts and its
// Synthetic tests. Its third reads GitHub (ADR-0131): every repository the
// agents' installations cover, its workflows and its deployment environments.
// Its fourth reads the company's servers (ADR-0134): on each server that opts
// in, the ports it listens on and the containers it runs. That one is handed
// to the worker by the engine rather than registered, since it runs commands
// through the node's SSH runner. Its fifth reads GitLab (ADR-0135): every
// project of the company's group, its pipeline and its environments.
// Its sixth and seventh read Google Cloud and Scaleway (ADR-0137): every
// project as an account, and what a Google Cloud project's enabled APIs say
// it is used for. Its eighth reads Atlassian (ADR-0138): every site of the
// organization as an account, and each product as what its account does.
// Its ninth and tenth ask Slack and Mattermost (ADR-0139) for the workspace's
// own id and name, with a bot token one of the agents already holds. Its
// eleventh reads AWS (ADR-0148), as a role of its own that the company adds
// to its stack by choice: the Regions in use, instances, databases, load
// balancers, queues, buckets and public DNS. Google Cloud's source also
// searches what each project runs (ADR-0149), as a service account of its
// own, where the company asks.
//
// # A source may be the company's choice
//
// A source that reads under an identity the company adds by choice (AWS's
// atlas role) is [Optional]: while the company has not asked for it, it is
// not read and not listed among the sources, though its integration is
// connected ([Connected], the one rule the duty and the report both read).
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
// # A person may ask for a read, and the floor is the duty's
//
// A source is read on its interval, and sooner where a person asked
// ([Status.DueFor]): on the tick after the ask, once [RefreshFloor] has passed
// since the source's last attempt. The floor is the refresh interval's own, so
// no gesture reads a vendor more often than the smallest cadence a company
// may set, and it is held here as well as where the ask is made, since the
// ask is a record anybody with the store could write. The ask is dropped when
// its read is made, when a read already answered it, and when the company no
// longer connects its source.
//
// # A source that reads in scopes keeps one last whole read each
//
// The servers are read one by one, and a server that could not be reached
// says nothing of the others. Each scope the read visits ([Read.Scopes])
// keeps the instant it was last read whole and how many of its parts the last
// read was refused ([Scope]), counted from the read rather than from the
// findings, which are bounded.
//
// # Other voices use the same builder and the same diff
//
// A voice that is no integration's (a repository's files,
// internal/atlas/codescan) gathers its claims through [NewBuilderFor] and
// records them with [Replace], which withdraws at once what the voice no
// longer says: a reading that is exact at its instant needs no second miss.
//
// # A forge's repositories are a visit of their own
//
// A source whose vendor hosts repositories is also a [CodeHost] (ADR-0136):
// it lists them, and serves a tree and a blob. The duty asks its [CodePass]
// (codescan.Forges) about every such source after the source's own read; the
// pass keeps its own standing and comes due apart from the read, and the
// duty's lease is claimed again for it as for any visit. The seam is here so
// a vendor implements it without importing the reader of the files.
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
// apart from them. A person's ask that a source be read now ([Request]): the
// whole company, current value only, until the duty honours it, so it is a
// record of its own in the coordination store, never a field of the standing
// the duty rewrites whole. Which node reads: the `worker:atlas-collect` lease,
// claimed per tick and re-claimed before every source.
package collect
