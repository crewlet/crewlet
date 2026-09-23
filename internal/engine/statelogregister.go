package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// THE REGISTER: every domain this build runs, and everything the engine has to
// know to run one, in ONE fixed-order table.
//
// # Why one table rather than a list plus four switches
//
// A domain used to be declared in five places: the list itself, the applier
// switch, the write-authority switch, the barrier switch and the Tier A
// ceiling switch. Three of those refuse a domain they do not know, naming it,
// so forgetting them is a boot failure with an explanation. THE BARRIER SWITCH
// DOES NOT: a domain missing from it silently gets no read index, and
// therefore no `linearizable`, which is a correct answer for one domain
// (the vectors are derived and compacted, so "as of a position" is not a
// question about them) and a silent wrong one for every other. A reviewer
// cannot tell the two apart by reading the switch, because absent and
// deliberately-absent look identical.
//
// So the barrier decision moves into the entry and becomes IMPOSSIBLE TO
// OMIT: an entry states an encoder or states [registration.NoBarrier], and
// [checkRegister] refuses one that states neither or both. That is the whole
// reason the table exists — the other four collapse into it because a domain
// declared in one place is read in one place.
//
// # The order is still load-bearing
//
// The register is a SLICE and its order is the order every operator surface
// reports in, the order streams are provisioned in and the order an offer's
// terms are compared in. A map would reorder all of them on a whim of the
// runtime, which is why this is a table rather than a registry each domain
// writes itself into from an init.
//
// # Why the constructors are functions on the engine rather than methods on
// the domain
//
// [statelog.Domain] is the DECLARATION — what a snapshot, a claim and a sweep
// read — and it must be answerable by a build that cannot construct the
// applier or the seams at all. An applier holds this node's id, its store
// handle and, for the knowledge base, the skill detector and its nudge; none
// of those belong to a value a manifest carries. So the constructors take the
// engine's own state log and live here, next to the declaration that names
// them, rather than inside it.

// registration is one domain's complete entry in the register.
//
// Every field but [registration.Barrier] and [registration.NoBarrier] is
// required, and [checkRegister] is what says so: an entry that omits one is a
// boot failure naming the domain and the field, never a node that runs a
// domain it cannot apply, cannot write to or cannot size a stream for.
type registration struct {
	// Domain is the declaration itself.
	Domain statelog.Domain

	// NewApplier builds the state machine that turns this domain's records
	// into rows. Required: a domain with no applier would have its records
	// consumed and produce nothing on this node.
	NewApplier func(s *stateLog) (statelog.Applier, error)

	// NewSeams builds the write authority's three seams and the domain's
	// eviction reader. Required: a domain with no write authority is one
	// nothing could ever append to.
	NewSeams func(s *stateLog, runner *statelog.Runner) (writeSeams, error)

	// Barrier encodes the framework's barrier append as one of this
	// domain's own records. A domain that declares one gets a read index
	// and therefore `linearizable`; one that declares [NoBarrier] instead
	// gets neither, and its reads make no freshness claim beyond `session`.
	//
	// EXACTLY ONE of Barrier and NoBarrier is stated, which is what makes
	// "this domain grants no linearizable read" a decision in the diff
	// rather than a line nobody wrote.
	Barrier func(statelog.Envelope) ([]byte, error)

	// NoBarrier is the explicit form of a nil Barrier. See Barrier.
	NoBarrier bool

	// Ceiling is where Tier A puts this domain's stream ceiling, and what
	// a refusal to reserve it names. Required: a domain Tier A declares no
	// ceiling for would reserve its own default outside the budget every
	// other state log is sized into.
	Ceiling func(stream config.Stream, free int64) domainCeiling

	// OpsRetention is how long this domain's operation ledger keeps a row.
	//
	// ON THE ENTRY rather than one constant every domain shares, because
	// the question the ledger answers is "did my operation land", asked by
	// a RETRYING CLIENT — a seat told to carry an op id forward and re-ask
	// on its next wake, after a weekend. Every domain's writers re-ask on
	// the same rhythm today, so every entry takes the framework's default;
	// what the field buys is that a domain whose writers do not says so
	// where it is declared, rather than by moving a constant the others
	// read.
	//
	// Zero takes [statelog.OpsRetention].
	OpsRetention time.Duration

	// Participates reports whether a node with these roles runs this
	// domain. Required, and stated per domain rather than defaulted,
	// because "every node runs everything" is an answer rather than an
	// absence: a domain whose rows only an ingress node reads still costs
	// every seats-only satellite its disk, its applier and its share of
	// the stream budget, and a satellite that holds a directory of people
	// it authenticates nobody against is the specific thing this exists to
	// prevent.
	//
	// EVERY DOMAIN A NODE DECLARES OR NONE. The set is one fact with five
	// readers — the applier set, the snapshot manifest, an offer's
	// usability, the trim's counted set and the store's pinned writers —
	// and a node running half of what it declared serves rows derived from
	// one log while another's records pile up unapplied, which nothing
	// above it can tell from a node that is merely behind.
	Participates func(placement.RoleSet) bool
}

// writeSeams is what one domain's write authority is built from: the three
// seams [statelog.Deps] takes, and the eviction reader readiness asks through.
//
// EVICTED MAY BE NIL, and that is the domain rather than an omission — the
// vectors are derived and compacted, so there is no tombstone table to read
// and nothing an evicted node could serve that a re-embed would not replace.
// It is not paired with a "no eviction reader" flag the way the barrier is,
// because a nil one refuses nothing silently: readiness asks the fence it was
// built from, and a domain with no fence has no answer to give either way.
type writeSeams struct {
	Rows    statelog.Rows
	Fence   statelog.Fence
	Gates   statelog.Gates
	Evicted func(context.Context) (bool, error)
}

// register is every domain this build runs, in a FIXED order.
func register() []registration {
	return []registration{
		{
			Domain: tracker.Domain{},
			NewApplier: func(s *stateLog) (statelog.Applier, error) {
				return tracker.NewApplier(s.nodeID, nil), nil
			},
			NewSeams: func(s *stateLog, runner *statelog.Runner) (writeSeams, error) {
				rows, err := tracker.NewRows(s.db)
				if err != nil {
					return writeSeams{}, err
				}
				fence := tracker.NewFence(s.db, s.nodeID)
				fence.Cursor = runner.Committed
				fence.Floor = s.trimFloor(tracker.Domain{}.Name(),
					func() uint32 { return runner.Committed().Generation })
				return writeSeams{Rows: rows, Fence: fence,
					Gates: tracker.NewGates(s.db), Evicted: fence.Evicted}, nil
			},
			Barrier:      tracker.EncodeBarrier,
			OpsRetention: statelog.OpsRetention,
			Participates: everyNode,
			Ceiling: func(stream config.Stream, free int64) domainCeiling {
				bytes, derived := stream.LogMaxBytes(free)
				return domainCeiling{Bytes: bytes,
					Field: "stream.tracker_log_max_bytes", Explicit: !derived}
			},
		},
		{
			Domain: search.Domain{},
			NewApplier: func(*stateLog) (statelog.Applier, error) {
				return search.NewApplier(), nil
			},
			NewSeams: func(s *stateLog, _ *statelog.Runner) (writeSeams, error) {
				rows, err := search.NewRows(s.db)
				if err != nil {
					return writeSeams{}, err
				}
				return writeSeams{Rows: rows, Fence: search.NewFence(),
					Gates: search.NewGates()}, nil
			},
			// NO BARRIER, DECLARED: the vectors are derived from sources
			// another domain owns and compacted to one message per
			// source, so a read of them makes no claim about a position
			// and there is nothing a barrier could prove.
			NoBarrier:    true,
			OpsRetention: statelog.OpsRetention,
			Participates: everyNode,
			Ceiling: func(stream config.Stream, free int64) domainCeiling {
				bytes, _ := stream.VectorsMaxBytes(free)
				return domainCeiling{Bytes: bytes,
					Field:    "stream.tracker_vectors_max_bytes",
					Explicit: stream.TrackerVectorsMaxBytes > 0}
			},
		},
		{
			Domain: pages.Domain{},
			NewApplier: func(s *stateLog) (statelog.Applier, error) {
				// THE PARSER AND THE NUDGE COME FROM HERE, because the
				// apply is what notices a tool-skill page arriving or
				// leaving and there is no other delivery to hang the
				// resync off. The nudge is safe to take before the
				// native runtime exists: it is a non-blocking send that
				// returns when there is nothing to send to.
				return pages.NewApplier(s.nodeID, s.skills, s.nudgeSkills), nil
			},
			NewSeams: func(s *stateLog, runner *statelog.Runner) (writeSeams, error) {
				rows, err := pages.NewRows(s.db)
				if err != nil {
					return writeSeams{}, err
				}
				fence := pages.NewFence(s.db, s.nodeID)
				fence.Cursor = runner.Committed
				fence.Floor = s.trimFloor(pages.Domain{}.Name(),
					func() uint32 { return runner.Committed().Generation })
				return writeSeams{Rows: rows, Fence: fence,
					Gates: pages.NewGates(s.db), Evicted: fence.Evicted}, nil
			},
			Barrier:      pages.EncodeBarrier,
			OpsRetention: statelog.OpsRetention,
			Participates: everyNode,
			Ceiling: func(stream config.Stream, free int64) domainCeiling {
				bytes, derived := stream.PagesMaxBytes(free)
				return domainCeiling{Bytes: bytes,
					Field: "stream.pages_log_max_bytes", Explicit: !derived}
			},
		},
		{
			Domain: chart.Domain{},
			NewApplier: func(s *stateLog) (statelog.Applier, error) {
				// NO REBUILD LISTENER YET, and nil is what the applier
				// documents as "nobody is listening" rather than a wire
				// somebody forgot. The company view a turn reads is
				// derived from these rows and is what will take this
				// seam; until the engine holds one, the applier's own
				// change set is drained and dropped, which is precisely
				// what an unobserved domain should cost.
				//
				// It cannot be the change feed instead: a feed relays a
				// record to ONE node, and a derived view is held by
				// every node — so the rest would go on serving a chart
				// they had already applied and could not see they had.
				// THE VIEW'S TRIGGER. Every committed batch
				// nudges the rebuild — on this node, which is
				// the only node whose view these rows are.
				// The object list is not read: a rebuild
				// derives the whole tree, because lead
				// inheritance and manages expansion make one
				// seat's move a fact about its descendants.
				return chart.NewApplier(s.nodeID, func([]chart.ObjectRef) {
					if s.nudgeChart != nil {
						s.nudgeChart()
					}
				}), nil
			},
			NewSeams: func(s *stateLog, runner *statelog.Runner) (writeSeams, error) {
				rows, err := chart.NewRows(s.db)
				if err != nil {
					return writeSeams{}, err
				}
				fence := chart.NewFence(s.db, s.nodeID)
				fence.Cursor = runner.Committed
				fence.Floor = s.trimFloor(chart.Domain{}.Name(),
					func() uint32 { return runner.Committed().Generation })
				return writeSeams{Rows: rows, Fence: fence,
					Gates: chart.NewGates(s.db), Evicted: fence.Evicted}, nil
			},
			Barrier:      chart.EncodeBarrier,
			OpsRetention: statelog.OpsRetention,
			Participates: everyNode,
			// THE ONE CEILING THAT IGNORES THE FREE BYTES, and the
			// signature keeps the parameter so the table stays one
			// shape: a chart is sized from the corpus rather than from
			// the operator's disk — see [config.Stream.ChartMaxBytes].
			Ceiling: func(stream config.Stream, _ int64) domainCeiling {
				bytes, derived := stream.ChartMaxBytes()
				return domainCeiling{Bytes: bytes,
					Field: "stream.chart_log_max_bytes", Explicit: !derived}
			},
		},
		{
			Domain: iamdomain.Domain{},
			NewApplier: func(s *stateLog) (statelog.Applier, error) {
				// THE SHREDDER, because removing a person is a key
				// deletion rather than a row deletion and the apply is
				// the only thing that sees every removal on every node.
				// Nil is legal and means a node with no keyring: it
				// deletes the rows and the key is a peer's to destroy.
				//
				// AND THE DIRECTORY'S SIGNAL, the party registry's
				// second rebuild trigger beside the chart view's: a
				// suspension withdraws a seat's contact identities
				// with no chart record, so nothing on the publish path
				// would ever see it. See directory.go.
				return iamdomain.NewApplier(s.nodeID, s.shredder,
					s.nudgeDirectory), nil
			},
			NewSeams: func(s *stateLog, runner *statelog.Runner) (writeSeams, error) {
				rows, err := iamdomain.NewRows(s.db)
				if err != nil {
					return writeSeams{}, err
				}
				fence := iamdomain.NewFence(s.db, s.nodeID)
				fence.Cursor = runner.Committed
				fence.Floor = s.trimFloor(iamdomain.Domain{}.Name(),
					func() uint32 { return runner.Committed().Generation })
				return writeSeams{Rows: rows, Fence: fence,
					Gates: iamdomain.NewGates(s.db), Evicted: fence.Evicted}, nil
			},
			Barrier:      iamdomain.EncodeBarrier,
			OpsRetention: statelog.OpsRetention,
			// THE FIRST DOMAIN THAT NARROWS, which is what
			// participationIn's own comment anticipated.
			Participates: servesPeople,
			// NOT DERIVED FROM THE DISK either, like the org chart's and
			// for a different reason: this log grows with the company's
			// HEADCOUNT and how often people sign in, and a volume has
			// nothing to say about either. The parameter stays so the
			// table is one shape — see [config.Stream.IamMaxBytes].
			Ceiling: func(stream config.Stream, _ int64) domainCeiling {
				bytes, derived := stream.IamMaxBytes()
				return domainCeiling{Bytes: bytes,
					Field: "stream.iam_log_max_bytes", Explicit: !derived}
			},
		},
	}
}

// checkRegister refuses a register whose entries are not complete, BEFORE
// anything is provisioned or started.
//
// Three of these were already boot failures, each raised at the moment its
// switch was consulted and naming only itself. Taken together and taken first,
// they say what is actually wrong: this build declares a domain it cannot run.
// The fourth, the barrier, was not a failure at all.
func checkRegister(entries []registration) error {
	seen := map[string]bool{}
	for i, entry := range entries {
		if entry.Domain == nil {
			return fmt.Errorf("engine: the state-log register's entry %d names no domain", i)
		}
		name := entry.Domain.Name()
		if seen[name] {
			return fmt.Errorf("engine: the state-log register declares domain %q twice, "+
				"so which applier, write authority and ceiling it runs would depend "+
				"on which entry a lookup reached first", name)
		}
		seen[name] = true
		if entry.NewApplier == nil {
			return fmt.Errorf("engine: the state-log register's entry for %q declares no "+
				"applier, so its records would be consumed and produce no rows on "+
				"this node", name)
		}
		if entry.NewSeams == nil {
			return fmt.Errorf("engine: the state-log register's entry for %q declares no "+
				"write authority, so nothing could ever append to its log", name)
		}
		if entry.Ceiling == nil {
			return fmt.Errorf("engine: the state-log register's entry for %q declares no "+
				"Tier A ceiling for its stream, so it would reserve its own default "+
				"outside the budget every other state log is sized into", name)
		}
		if entry.OpsRetention <= 0 {
			return fmt.Errorf("engine: the state-log register's entry for %q declares an "+
				"operation-ledger horizon of %s — a domain's ledger row answers a "+
				"retrying client, and a horizon of zero or less would sweep a row "+
				"the client has not had a chance to re-ask with",
				name, entry.OpsRetention)
		}
		if entry.Participates == nil {
			return fmt.Errorf("engine: the state-log register's entry for %q says "+
				"nothing about which nodes run it, and an absent predicate is "+
				"read as running NOWHERE — every node would strip its rows out "+
				"of an adopted artefact and apply none of its records, silently. "+
				"Declare everyNode, or the roles it needs", name)
		}
		if (entry.Barrier == nil) == !entry.NoBarrier {
			return fmt.Errorf("engine: the state-log register's entry for %q must state "+
				"either a barrier encoder or NoBarrier, and states %s — a domain "+
				"that grants no linearizable read says so, because an entry that "+
				"merely omits the encoder is indistinguishable from one that "+
				"forgot it", name, statedBarrier(entry))
		}
	}
	if len(entries) == 0 {
		return errors.New("engine: the state-log register is empty, so this node would " +
			"provision no log, apply no record and serve no domain's rows")
	}
	return nil
}

// statedBarrier is what an entry said about its barrier, for the refusal.
func statedBarrier(entry registration) string {
	if entry.Barrier != nil {
		return "both"
	}
	return "neither"
}

// registrationFor is one domain's entry.
//
// It cannot fail on an unknown name where the caller walked the register to
// get there, which every caller does; the boolean is for the one that did not.
func registrationFor(name string) (registration, bool) {
	for _, entry := range register() {
		if entry.Domain.Name() == name {
			return entry, true
		}
	}
	return registration{}, false
}

// registeredDomains is every domain this BUILD knows, in the register's order,
// whatever this node runs.
//
// Kept as a derivation rather than folded into every caller, because most of
// them want exactly this — the list — and reading it off the table is what
// makes the table the single place a domain is declared.
//
// NOT THE SET A NODE APPLIES. That is [participationOf], and the difference
// matters wherever the answer is about this process rather than about this
// binary: what to start an applier for, what a snapshot may claim, what a
// pinned writer is reserved for. What stays on this list is everything a
// domain's EXISTENCE decides — the streams a maintenance window excludes, the
// ceilings the broker is sized to — because a stream belongs to the fleet
// whether or not the node reading this line applies it.
func registeredDomains() []statelog.Domain {
	entries := register()
	domains := make([]statelog.Domain, 0, len(entries))
	for _, entry := range entries {
		domains = append(domains, entry.Domain)
	}
	return domains
}

// participation is which registered domains a node runs and which it does not.
//
// BOTH HALVES, because the two have opposite dispositions everywhere they are
// read and a caller holding one cannot derive the other without the register.
// The run set decides what starts an applier, what a snapshot claims and what
// an artefact must name; the unrun set decides what is STRIPPED out of an
// artefact that carries it.
type participation struct {
	// Run is this node's own set, in the register's order.
	Run []registration

	// Unrun is the rest, as bare declarations: nothing here has an
	// applier, a publisher or a ceiling on this node, and the only thing
	// asked of it is what tables and which stream to scrub.
	Unrun []statelog.Domain
}

// Domains is the run set as a plain list, for the callers that want the
// declarations rather than the registrations.
func (p participation) Domains() []statelog.Domain {
	out := make([]statelog.Domain, 0, len(p.Run))
	for _, entry := range p.Run {
		out = append(out, entry.Domain)
	}
	return out
}

// Runs reports whether this node applies the named domain.
func (p participation) Runs(name string) bool {
	for _, entry := range p.Run {
		if entry.Domain.Name() == name {
			return true
		}
	}
	return false
}

// DomainsForRoles is which state-log domains a node with these roles would
// run, by name, in the register's order.
//
// EXPORTED FOR `crewlet validate`, which is the one surface that answers this
// question about a configuration NOBODY IS RUNNING. Everything else reads it
// off a live engine, and must: the engine decides it once at boot and a
// second derivation is how a screen comes to name a set the appliers do not
// match. This one has no engine to ask.
//
// It exists because the consequence is otherwise invisible until boot. An
// operator narrowing node.roles narrows what that node applies, and the only
// other symptom is a peer answering a question this node's copy cannot.
func DomainsForRoles(roles placement.RoleSet) []string {
	return domainNames(participationOf(roles).Domains())
}

// domainNames is a list of declarations as their names, which is the form
// everything crossing a package boundary takes: a peer's participation
// travels as strings, because the far side holds declarations of its own and
// comparing two builds' Domain values would compare two different types.
func domainNames(domains []statelog.Domain) []string {
	out := make([]string, 0, len(domains))
	for _, domain := range domains {
		out = append(out, domain.Name())
	}
	return out
}

// participationOf divides the register by what a node with these roles runs.
//
// DERIVED FROM THE ROLES rather than configured beside them, so there is one
// place an operator states it and no second list to keep in step. A node that
// declares no roles runs every role, which [placement.RoleSet] already reads
// an empty set as — so the default deployment is unchanged, and it is the
// operator who narrowed the roles who narrows the domains.
func participationOf(roles placement.RoleSet) participation {
	return participationIn(register(), roles)
}

// participationIn is participationOf over a given register, which is what
// lets a test hand it a NARROWING predicate. Every shipped domain runs
// everywhere, so the rules below are unreachable through [register] alone and
// a case that could only call that would be asserting nothing.
func participationIn(entries []registration, roles placement.RoleSet) participation {
	// AN EMPTY SET IS EVERY ROLE, resolved HERE rather than left to each
	// predicate. [placement.RoleSet] settles that convention — "declared
	// nothing" and "does nothing" must never be the same answer — and a
	// predicate is exactly where it would be forgotten: a rolling upgrade
	// puts a peer's presence row with no roles on it in front of this
	// node, and a predicate reading that as "no roles" would leave the
	// peer out of every domain it narrows on. It would then not be counted
	// for that domain's trim, and the fleet would trim past a node that is
	// still applying.
	//
	// It costs nothing today, because every shipped domain runs
	// everywhere. It is here now because the first domain that narrows is
	// the one that would have found out.
	if len(roles) == 0 {
		roles = placement.DefaultRoles()
	}
	var out participation
	for _, entry := range entries {
		// A NIL PREDICATE READS AS "NOWHERE", and [checkRegister]
		// refuses one at boot for that reason: the safe-looking
		// default, running everywhere, would make a domain nobody
		// declared a participation for indistinguishable from one
		// somebody decided runs on every node.
		if entry.Participates != nil && entry.Participates(roles) {
			out.Run = append(out.Run, entry)
			continue
		}
		out.Unrun = append(out.Unrun, entry.Domain)
	}
	return out
}

// signerFor and verifierFor are one domain's halves of the record signature.
//
// PER DOMAIN, because the derivation binds the domain's own name in: a record
// replayed from one log onto another must not verify, and a signer that did
// not know which log it was writing to could not promise that.
func (s *stateLog) signerFor(domain statelog.Domain) (*statelog.Signer, error) {
	return statelog.NewSigner(domain.Name(), s.ring)
}

func (s *stateLog) verifierFor(domain statelog.Domain) (*statelog.Verifier, error) {
	return statelog.NewVerifier(domain.Name(), s.ring)
}

// recordKeyring is the Tier A keyring as the state log's signatures read it.
//
// THE SAME MATERIAL THE PER-RUN TOKENS DERIVE FROM, and deliberately: a
// deployment has one keyring, and a second one for records would be a second
// thing to rotate, a second thing to get wrong and a second thing an operator
// has to know exists. The two derive different keys from it, each binding its
// own label in, so a record MAC and a token MAC over one key are never the
// same bytes.
//
// THE REFERENCES ARE NOT RESOLVED, for [config.Secrets.TokenMaterial]'s
// reason: this is read before the secret store whose own key would resolve
// them is open, and what matters is that two nodes agree rather than that the
// material is plaintext.
func recordKeyring(boot *config.Bootstrap) statelog.Keyring {
	if boot == nil {
		return statelog.Keyring{}
	}
	ring := statelog.Keyring{
		ActiveID: boot.Secrets.ActiveKeyID,
		Keys:     make([]statelog.Key, 0, len(boot.Secrets.Keys)),
	}
	for _, key := range boot.Secrets.Keys {
		ring.Keys = append(ring.Keys, statelog.Key{ID: key.ID, Material: key.Material})
	}
	return ring
}

// everyNode is the participation of a domain every role needs.
//
// All three shipped domains take it, and each for the same reason: an ingress
// node serves the board, the knowledge base and search over the API; a seats
// node reads and writes all three inside a turn; and a workers node sweeps
// them and runs the embedding duty. There is no role that can do its job
// without them, so filtering would only mean a node refusing its own work.
//
// It is a named function rather than a nil check because a domain that runs
// everywhere is a DECISION, and the next domain's entry is where somebody
// decides differently.
func everyNode(placement.RoleSet) bool { return true }

// servesPeople is the participation of the IDENTITY estate, and it is the
// first predicate in this register that says no to anybody.
//
// AN AGENT SEAT NEVER READS THIS DOMAIN. A seat's principal is its own handle,
// its authority is decided by internal/authz from the ORG CHART, and its work
// arrives on its mailbox — so a seats-only satellite gains nothing from these
// rows and pays for them three times over: the disk, the applier, and its
// share of a stream budget every other log is sized into. A satellite holding
// a directory of the company's people, which it authenticates nobody against,
// is the specific thing the Participates field was added for.
//
// INGRESS BECAUSE IT SERVES REQUESTS: resolving who is asking, validating a
// session bearer against a revocation epoch, and refusing one that is over.
// WORKERS BECAUSE IT SWEEPS: the per-bucket retention sweep, the
// duplicate-claim report, and the retry that destroys a key a removal could
// not reach.
//
// # What a node that does NOT run it must never do
//
// Answer a question about people from an empty table. `iam_people` on a
// satellite is empty because the domain is not running, not because the
// company has nobody — and the two are the same rows. Every reader in this
// estate is three-valued for that reason: "nobody holds this seat" and "this
// node does not hold that answer" send a caller to opposite places, and the
// second is a 503.
func servesPeople(roles placement.RoleSet) bool {
	return roles.Has(placement.RoleIngress) || roles.Has(placement.RoleWorkers)
}
