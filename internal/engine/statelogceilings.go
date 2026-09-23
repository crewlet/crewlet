package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/bits"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// THE STATE LOGS' STREAM CEILINGS: what each log's stream is created with, and
// what a boot that cannot reserve one says.
//
// # One budget, over the REGISTERED set
//
// A ceiling is a reservation the broker grants in full when it creates the
// stream, so every log competes for one number and they have to be sized
// together. They are sized over [registeredDomains] rather than over a list
// written here, because a list written here is how the pages log came to sit
// outside the budget: the tracker's and the vectors' ceilings were scaled to
// fill it, the pages log reserved a fixed 4 GiB on top, and on a host with
// 7.6 GiB free that was the one reservation the broker refused. A registered
// domain that Tier A declares no ceiling for fails the boot naming itself, the
// way one with no applier does.
//
// # A stream that already exists keeps its ceiling
//
// Sizing decides what a MISSING stream is created with and nothing else. A
// stream's configuration has one writer and a booting node is not it, and the
// broker never re-checks a reservation it has already granted, so a log
// created larger than today's arithmetic would make it (a pages log from
// before it joined the budget) boots as it is and is reported rather than
// rewritten. Changing it is the capacity operation's, which runs where no
// publisher can move the number it is decided against.
//
// And it COUNTS at that ceiling. What the budget promises is a bound on what
// the logs reserve between them, and an existing log reserves what it holds,
// not what today's arithmetic would give it — so a log created beside it is
// sized from what it leaves ([fitCeilings]).

// domainCeiling is one domain's stream ceiling and where Tier A puts it.
type domainCeiling struct {
	// Bytes is what the stream is created with.
	Bytes int64

	// Field is the Tier A key that governs it, which is what a refusal
	// names.
	Field string

	// Explicit reports that the operator set Field. An explicit ceiling is
	// never scaled: the operator named a limit for a broker they can see,
	// and silently lowering it would be the engine deciding a limit an
	// emergency grant had just raised. A refused boot naming the field is
	// the honest answer there.
	Explicit bool
}

// StreamBudgetShare is how much of what the broker can grant the state logs
// their ceilings may reserve between them — the ceilings their existing
// streams hold included, whatever Tier A would size those at today.
//
// HALF. The same broker holds every seat's mailbox, every coordination bucket
// (the leases, the ledgers, the counters, the company's sealed credentials)
// and the snapshot a joining node reads. None of those reserves anything, so
// the broker would hand the logs all of it; a log allowed to reserve the whole
// account starves the estate it is part of, and the failure is not a full log,
// it is a company that cannot claim a seat.
const StreamBudgetShare = 0.5

// MinDomainCeiling is the floor a scaled-down ceiling never goes below.
//
// A gibibyte, which is the same floor Tier A's own validation enforces on an
// explicit value: below it a log is not a log, it is a window that refuses
// appends within a week of a company starting work.
const MinDomainCeiling int64 = 1 << 30

// tierACeiling is where Tier A puts one domain's ceiling on a volume with free
// bytes of headroom.
//
// A SWITCH RATHER THAN A METHOD on the domain, for [stateLog.applierFor]'s
// reason: the Tier A field is the engine's vocabulary rather than the domain's,
// and what this costs is that a new domain fails here, at boot, naming itself,
// rather than reserving a default outside the budget.
//
// # Why the mutation log's is DERIVED and the vector changelog's is capped
//
// A fixed default for the mutation log is wrong in both directions: the same
// number is five years of history at the modelled write rate and one boot on a
// small disk. So an unset value takes a quarter of the stream volume's free
// space, clamped, and the knowledge base's log takes a quarter of that for the
// ratio its corpus grows at.
//
// The vector changelog's default is fixed because its peak is not a function
// of the disk: the stream keeps one message per source and bounds their age, so
// a week's minting is small, but changing the embedding model rewrites every
// source in a few hours, and for the following week every source's current
// message is inside the window. The default is sized for that operation and
// capped only by the disk, because sizing it from the steady state would refuse
// the one operation it exists to survive.
func tierACeiling(stream config.Stream, domain statelog.Domain, free int64) (domainCeiling, error) {
	switch domain.Name() {
	case tracker.Domain{}.Name():
		bytes, derived := stream.LogMaxBytes(free)
		return domainCeiling{Bytes: bytes, Field: "stream.tracker_log_max_bytes", Explicit: !derived}, nil
	case search.Domain{}.Name():
		bytes, _ := stream.VectorsMaxBytes(free)
		return domainCeiling{Bytes: bytes, Field: "stream.tracker_vectors_max_bytes",
			Explicit: stream.TrackerVectorsMaxBytes > 0}, nil
	case pages.Domain{}.Name():
		bytes, derived := stream.PagesMaxBytes(free)
		return domainCeiling{Bytes: bytes, Field: "stream.pages_log_max_bytes", Explicit: !derived}, nil
	}
	return domainCeiling{}, fmt.Errorf("engine: domain %q is registered and Tier A "+
		"declares no ceiling for its stream, so it would reserve its own default "+
		"outside the budget every other state log is sized into", domain.Name())
}

// ceilingsFor sizes every registered domain's stream ceiling from Tier A and
// the broker's own budget.
func ceilingsFor(ctx context.Context, host domainHost, boot *config.Bootstrap) (map[string]domainCeiling, error) {
	if boot == nil {
		return nil, errors.New("engine: a state log's ceilings are sized from Tier A, and this node was given none")
	}
	volume := streamVolume(boot)
	free, err := volumeFree(volume)
	if err != nil {
		// LOGGED AND CARRIED with a zero, which every derivation clamps
		// up to its floor. A node that cannot measure its own disk still
		// has to boot, and the floor is a value that fits on any volume
		// this engine runs on.
		log.WarnContext(ctx, "statelog_free_space_unmeasured",
			"path", volume, "error", err.Error(),
			"detail", "every derived state-log ceiling falls back to its floor; "+
				"set stream.tracker_log_max_bytes, stream.tracker_vectors_max_bytes "+
				"and stream.pages_log_max_bytes to choose them")
	}
	return sizeCeilings(ctx, host, boot.Stream, free, volume)
}

// sizeCeilings is [ceilingsFor] on a volume already measured.
//
// # The budget
//
// [StreamBudgetShare] of what the broker can grant the state logs, read from
// the broker ([jetstream.Queue.StreamBudget]) and counting the ceilings the
// logs' existing streams already hold as the logs' own. That second clause is
// what makes the pool stable: a restart divides the pool the boot that created
// the logs divided, rather than whatever their own reservations left, and a log
// an upgrade adds is sized beside the ones already there. What each log ASKS
// still follows the volume's free space, which the logs' own records spend.
//
// UNSTATED IS NOT UNBOUNDED. An external broker whose account states no limit
// still refuses a reservation its servers cannot back, and a client cannot
// read their caps; the volume this node measured is the one figure it has, so
// the same share of that bounds it.
//
// # Why the held ceilings are added back to the one figure and not the other
//
// The broker's figure is what it has LEFT, and a reservation spends it: once
// the logs exist their ceilings are gone from it, so adding them back is what
// hands a restart the number the creating boot had. Free space is spent by
// bytes written and never by a reservation, so it still contains every ceiling
// the logs hold. Adding them to it as well counted each one twice — a restart
// sizing from its own disk divided a pool larger than its first boot's by half
// of what the logs hold, and a log created beside them was handed that half on
// top of the share.
func sizeCeilings(ctx context.Context, host domainHost, stream config.Stream,
	free int64, volume string) (map[string]domainCeiling, error) {

	asked := map[string]domainCeiling{}
	held := map[string]int64{}
	var holding int64
	for _, domain := range registeredDomains() {
		ceiling, err := tierACeiling(stream, domain, free)
		if err != nil {
			return nil, err
		}
		asked[domain.Name()] = ceiling
		holds, found, err := host.DomainStreamCeiling(ctx, domain.Stream().Name)
		switch {
		case err != nil:
			// CARRIED AS ABSENT. The provision that follows asks the
			// same broker the same question and fails with its own
			// answer if it still cannot give one; counted here as
			// absent, the stream is merely sized as though this boot
			// were creating it.
			log.WarnContext(ctx, "statelog_ceiling_unread",
				"domain", domain.Name(), "error", err.Error())
		case found:
			held[domain.Name()] = holds
			holding += holds
		}
	}

	budget, err := host.StreamBudget(ctx)
	if err != nil {
		log.WarnContext(ctx, "statelog_broker_budget_unread", "error", err.Error(),
			"detail", "the state logs' ceilings are sized from this node's free "+
				"space instead, and the broker still refuses one it cannot reserve")
	}
	grantable := free
	if available := budget.Available(); err == nil && available >= 0 {
		grantable = available + holding
	}
	// A LIMIT OF ZERO FROM AN ACCOUNT THAT DECLARES NONE FOR THIS NODE IS
	// SAID OUT LOUD, because the boot that follows does not say it.
	//
	// It is the ONE reading that is a misconfiguration rather than a
	// condition: the broker refuses every create on such an account before
	// it compares a byte, so a node whose objects all already exist starts
	// and works, sized against a pool of nothing but what those logs
	// already hold, until the first time it has to make something new.
	// Then a create is refused and the operator meets a fact that has been
	// true since this line. Every other zero is a full broker, which the
	// refusal itself explains when it arrives.
	if err == nil && noApplicableLimit(budget.Source) {
		log.WarnContext(ctx, "statelog_broker_states_no_limit",
			"broker_source", string(budget.Source),
			"detail", limitSource(budget.Source, volume)+
				". This node will start if every stream, consumer and bucket it "+
				"needs already exists, and the first create it has to make will "+
				"be refused")
	}
	pool := int64(float64(grantable) * StreamBudgetShare)
	sized := fitCeilings(asked, held, pool)

	ceilings := make(map[string]int64, len(sized))
	var explicit []string
	for name, ceiling := range sized {
		ceilings[name] = ceiling.Bytes
		if ceiling.Explicit {
			explicit = append(explicit, name)
		}
	}
	slices.Sort(explicit)
	log.InfoContext(ctx, "statelog_ceilings",
		"ceilings", ceilings, "explicit", explicit, "free", free,
		"broker_limit", budget.Limit, "broker_committed", budget.Committed,
		"broker_source", string(budget.Source), "held", held, "pool", pool)
	// A LOG CREATED BELOW ITS FIT IS SAID OUT LOUD, because nothing else
	// says it: the boot succeeds, the ceiling is the only number that
	// moved, and the first sign would be a log refusing appends early. The
	// logs whose holdings did it are reported against their fit by the
	// broker's capacity line; this names the one they took it from, and
	// the gesture that moves both.
	if fit := shortOfFit(asked, held, pool, sized); len(fit) > 0 {
		created := make(map[string]int64, len(fit))
		for name := range fit {
			created[name] = sized[name].Bytes
		}
		log.WarnContext(ctx, "statelog_ceiling_short_of_fit",
			"created", created, "fit", fit, "held", held, "pool", pool,
			"detail", "the state logs that already exist hold more of their "+
				"share than this sizing would give them, so a log created now "+
				"is sized from what they leave rather than from what it asked "+
				"for; once this node is up, `crewlet retention set-capacity` "+
				"gives an existing log's reservation back and raises this one")
	}
	return sized, nil
}

// fitCeilings sizes every log inside pool: what a MISSING log is created with,
// and the value an EXISTING one is reported against, which is never applied.
// held is the ceiling each existing log's stream holds, by domain.
//
// # A log that exists counts at what it HOLDS
//
// Its reservation is the ceiling the broker granted it, and the pool bounds
// what the logs reserve between them — so the logs being created divide what
// the existing ones leave of the pool, whatever Tier A would give those today.
//
// The two numbers differ whenever a log was created at something other than
// today's arithmetic: a knowledge-base log from before it joined the budget,
// an explicit ceiling later unset, a log created while its volume had more
// room, one a capacity operation resized. Dividing the pool by the ASKS
// counted such a log at a reservation it does not have. Measured: a tracker
// log holding 17179869184 bytes, created at an explicit ceiling later unset,
// beside a pool of 8789273088, was counted at its ask, and the boot was
// refused the vector changelog's reservation. On a 16 GiB volume that
// arithmetic sizes the changelog at 3857765632 in bytes the tracker already
// holds, and the logs ask for 22111376640 between them.
//
// So the missing logs divide what the existing ones leave of the pool, and
// together with them reserve at most the pool, past it only by the floors and
// by a ceiling set for a log being created. What the existing logs ALREADY
// hold is not something this can give back: where they hold more than the
// pool — a ceiling set and later unset, a log created while its volume had
// more room, one a capacity operation raised — the missing ones get their
// floors, and the total exceeds the pool by that excess as well. The cost is
// stated rather than hidden: a log created beside logs that already hold the
// whole share gets the floor ([shortOfFit] says so), and is refused by name
// ([stateLog.storageRefused]) when not even that fits.
//
// # An existing log's own value is its fit on an EMPTY broker
//
// What this sizing would create it with if every log were missing, which is
// the figure a restart reproduces: the boot that created the logs sized them
// exactly so, so a log still holding what it was created with draws no report,
// and one holding anything else is reported with both numbers.
//
// # And a missing log is created at NO MORE than that fit
//
// Because the fit is what every later boot reports its stream against, a log
// created above it is a capacity difference nobody made, logged on every boot
// of every node for the life of the stream. Dividing what the existing logs
// leave reaches above the fit two ways: a log holding less than its own fit
// leaves the difference to the missing ones, and a boot that stopped between
// two creates leaves the next one dividing a remainder whose floor rounding
// lands a byte or two over the empty-broker division — measured at 7158278827
// against a fit of 7158278826. Capping at the fit gives up nothing the budget
// promised and keeps every created log reproducible.
//
// Pure arithmetic, so every case is a table row rather than a broker.
func fitCeilings(asked map[string]domainCeiling, held map[string]int64, pool int64) map[string]domainCeiling {
	out := divide(asked, pool)
	missing := map[string]domainCeiling{}
	left := pool
	for name, ceiling := range asked {
		if holds, exists := held[name]; exists {
			left -= holds
			continue
		}
		missing[name] = ceiling
	}
	for name, created := range divide(missing, left) {
		fit := out[name]
		fit.Bytes = min(fit.Bytes, created.Bytes)
		out[name] = fit
	}
	return out
}

// shortOfFit is every log this sizing creates below its fit on an empty
// broker, with that fit: the logs the existing ones' holdings took bytes from.
func shortOfFit(asked map[string]domainCeiling, held map[string]int64,
	pool int64, sized map[string]domainCeiling) map[string]int64 {

	fit := divide(asked, pool)
	short := map[string]int64{}
	for name, ceiling := range sized {
		if _, exists := held[name]; !exists && ceiling.Bytes < fit[name].Bytes {
			short[name] = fit[name].Bytes
		}
	}
	return short
}

// divide scales the derived ceilings down to fit pool, leaving explicit ones as
// the operator wrote them.
//
// # The arithmetic, and the two ways the obvious version overshoots
//
// Explicit ceilings come off the pool FIRST, and the derived ones share what
// is left in proportion to what each asked for. Scaling every derived value by
// pool over a total that counts the explicit values hands the derived logs a
// share of bytes the explicit ones had already spent.
//
// And a log whose share falls below [MinDomainCeiling] is HELD at the floor
// and the rest divide what remains, rather than being raised to it after the
// division: raised afterwards, every floored log is bytes the others were also
// given. So the derived logs never share more than the explicit ones leave,
// and the total exceeds the pool only by the floors and by explicit ceilings
// that do not fit it, which the broker then grants or refuses by name. A pool
// already spent — below zero, where the logs that exist hold more than the
// share or an explicit ceiling is larger than it — is a pool of nothing, and
// every derived log is at its floor.
func divide(asked map[string]domainCeiling, pool int64) map[string]domainCeiling {
	out := maps.Clone(asked)
	remaining := pool
	var open []string
	var want int64
	for name, ceiling := range asked {
		if ceiling.Explicit {
			remaining -= ceiling.Bytes
			continue
		}
		open = append(open, name)
		want += ceiling.Bytes
	}
	if want <= remaining {
		return out
	}
	slices.Sort(open)
	for len(open) > 0 {
		want = 0
		for _, name := range open {
			want += asked[name].Bytes
		}
		share := max(remaining, 0)
		var floored []string
		for _, name := range open {
			if proportion(asked[name].Bytes, share, want) < MinDomainCeiling {
				floored = append(floored, name)
			}
		}
		if len(floored) == 0 {
			for _, name := range open {
				ceiling := out[name]
				ceiling.Bytes = proportion(asked[name].Bytes, share, want)
				out[name] = ceiling
			}
			break
		}
		for _, name := range floored {
			// SCALING NEVER RAISES: a log that asked for less than
			// the floor keeps what it asked for.
			ceiling := out[name]
			ceiling.Bytes = min(ceiling.Bytes, MinDomainCeiling)
			out[name] = ceiling
			remaining -= ceiling.Bytes
		}
		open = slices.DeleteFunc(open, func(name string) bool {
			return slices.Contains(floored, name)
		})
	}
	return out
}

// proportion is part × num / whole, for a part of whole, without the overflow
// the product reaches on a large volume: a ceiling near a tebibyte times a pool
// near one is past what an int64 holds.
func proportion(part, num, whole int64) int64 {
	hi, lo := bits.Mul64(uint64(part), uint64(num))
	q, _ := bits.Div64(hi, lo, uint64(whole))
	return int64(q)
}

// streamVolume is the directory whose volume the state logs' ceilings are
// derived from.
//
// THE STREAM'S OWN DIRECTORY when an embedded broker keeps one, because that is
// the volume a full log fills and the one the broker's own cap is taken from.
// It used to be the store's, which is the same volume in the shipped layout and
// a different one wherever an operator gave the stream a disk of its own. An
// in-memory or external broker has no local stream volume, and the store's is
// the one figure this node has.
func streamVolume(boot *config.Bootstrap) string {
	if dir := strings.TrimSpace(boot.Stream.StoreDir); dir != "" && boot.Stream.Type != config.StreamNATS {
		return dir
	}
	return filepath.Dir(boot.Store.Path)
}

// ceilingFor is the ceiling a domain's stream was sized at.
func (s *stateLog) ceilingFor(domain statelog.Domain) (domainCeiling, error) {
	ceiling, sized := s.ceilings[domain.Name()]
	if !sized || ceiling.Bytes <= 0 {
		return domainCeiling{}, fmt.Errorf("engine: %s's log was never sized, and a "+
			"stream created at a default nobody budgeted is the reservation that "+
			"refuses a boot", domain.Name())
	}
	return ceiling, nil
}

// storageRefused is the error a boot returns when the broker will not reserve
// a state log's ceiling.
//
// # What it says, and why each part is there
//
// The broker's own words are `insufficient storage resources available`, which
// names neither the number it needed nor the number it had nor anything an
// operator could change: a boot refused with that, on a disk that had room for
// two of the three logs, sent its operator looking at the disk. So this names
// all three. What the stream asked to reserve, what the broker had left and
// what sets that limit, and the Tier A field the ceiling came from.
//
// # And only the remedies this node can reach
//
// A log that already exists keeps the ceiling it was created with, and
// shrinking one is `crewlet retention set-capacity`, which runs on a node whose
// state logs are up. Every mode starts them, maintenance and seal included, so
// a node refused here cannot run it: offering it sent the operator to a verb
// that fails the same way. What is left is more room for the broker, or a
// smaller ceiling for the log it refused, which the floor bounds.
//
// The budget is read AGAIN, at the refusal, rather than carried from sizing:
// the logs created before this one have reserved since, and the number that
// matters is what the broker had when it said no.
func (s *stateLog) storageRefused(ctx context.Context, host domainHost,
	domain statelog.Domain, ceiling domainCeiling, cause error) error {

	budget, err := host.StreamBudget(ctx)
	had := roomLeft(budget, err, s.volume)
	from := fmt.Sprintf("%s is unset, so the ceiling was derived and scaled into "+
		"what the state logs that already exist leave of their share of the "+
		"broker, and it goes no lower than %d bytes", ceiling.Field, MinDomainCeiling)
	if ceiling.Explicit {
		from = fmt.Sprintf("%s sets the ceiling", ceiling.Field)
		if fits := budget.Available(); err == nil && fits >= MinDomainCeiling {
			from += fmt.Sprintf(", and at most %d bytes fits", fits)
		}
	}
	remedy := "Give the broker more room"
	if ceiling.Bytes > MinDomainCeiling {
		remedy += fmt.Sprintf(", or set %s to a smaller ceiling", ceiling.Field)
	}
	return fmt.Errorf("engine: the broker refused to reserve the %s log's "+
		"ceiling: %s needed %d bytes and %s. %s. %s; the state logs that already "+
		"exist keep the ceilings they were created with, and no Tier A setting "+
		"changes them: %w",
		domain.Name(), domain.Stream().Name, ceiling.Bytes, had, from, remedy, cause)
}

// roomLeft is what the broker had to reserve when it refused, in the terms an
// operator acts on: how much is left, how much of the limit is spoken for, what
// the limit is, and who sets it.
//
// ONE SENTENCE FOR BOTH REFUSALS, the create's ([stateLog.storageRefused]) and
// the raise's ([Engine.capacityRefusal]). They are the same four numbers about
// the same broker and were written twice, which is the shape this tree has paid
// for before: [internal/textcut], [internal/whsec] and [internal/jsprovision]
// each record a rule that drifted while two doc comments asserted they matched.
//
// PURE OVER THE VALUES IT IS GIVEN, and the read is the caller's — which buys
// the property that matters here: the numbers in the message are provably the
// numbers that decided, because there is no second source for them to come
// from. A caller that read the budget to decide and then asked again for the
// wording could refuse an operator against one limit and quote them another,
// on a topology where each read is a live round trip.
//
// The three cases are three different facts and never collapse: a limit that
// could not be read is not a limit of zero, and a broker that states none is
// not a broker with none — its servers still have caps this client cannot see.
func roomLeft(budget jetstream.StorageBudget, readErr error, volume string) string {
	switch {
	case readErr != nil:
		return fmt.Sprintf("what the broker had left could not be read (%v)", readErr)
	case budget.Limit < 0:
		return "the broker states no limit this node can read, so what refused it " +
			"is a server's own cap"
	}
	return fmt.Sprintf("the broker had %d bytes left to reserve (%d of its "+
		"%d-byte limit already reserved), and %s", budget.Available(),
		budget.Committed, budget.Limit, limitSource(budget.Source, volume))
}

// noApplicableLimit reports a budget whose zero means the account declares no
// limit that applies to this node at all, rather than a limit that is spent.
//
// TWO SOURCES AND NOT A ZERO TEST, because the two are the same number and the
// opposite instruction: a spent limit clears by somebody freeing room, and
// these clear only by a change of configuration.
func noApplicableLimit(source jetstream.BudgetSource) bool {
	switch source {
	case jetstream.BudgetAccountNoTier, jetstream.BudgetAccountTierNoLimit:
		return true
	}
	return false
}

// limitSource says who sets a broker's limit, in the terms an operator
// changes.
func limitSource(source jetstream.BudgetSource, volume string) string {
	switch source {
	case jetstream.BudgetServerStore:
		// BOTH SOURCES OF THE SAME NUMBER, because the broker reports
		// only the number and not which one set it: stream.store_max_bytes
		// where an operator declared one, and nats-server's own sizing of
		// the volume where nobody did. Naming only the second sends an
		// operator who set the first to a disk that has room.
		return fmt.Sprintf("that limit is stream.store_max_bytes where you set "+
			"one, and otherwise three quarters of the free space on the volume "+
			"holding stream.store_dir (%s), counting what the broker's streams "+
			"already hold there", volume)
	case jetstream.BudgetServerMemory:
		return "that limit is three quarters of this host's memory, because " +
			"stream.store_dir is unset and the embedded broker keeps its streams " +
			"in memory"
	case jetstream.BudgetAccount:
		return "that limit is the NATS account's own JetStream storage limit, " +
			"which whoever operates the broker sets"
	case jetstream.BudgetAccountTierNoLimit:
		// TWO REALITIES UNDER ONE REPORT, which is why this clause names
		// both refusals rather than the missing tier's alone. A class
		// the limit table has no entry for is never resolved and is
		// refused before a byte is compared; a class the account really
		// does declare with NO DISK — a memory-only tier, whose
		// DiskStorage reaches MaxStore verbatim — resolves normally and
		// is refused by the byte comparison instead. Nothing in the
		// account's own report tells the two apart, so an operator sent
		// looking for a missing declaration would be hunting the one
		// thing that account is not missing.
		return "that limit is zero because the NATS account is TIERED, reports the " +
			"replica class stream.replicas puts this node in and states no storage " +
			"for it — a class an account holds objects in is reported whether or not " +
			"a limit was ever set for it, so a class being present is not a class " +
			"being declared. Either the limit table has no entry for it, and every " +
			"create is refused before a byte is compared (`no JetStream default or " +
			"applicable tiered limit present`), or the tier is declared with no disk " +
			"and every create that reserves bytes is refused by the comparison " +
			"(`insufficient storage resources available`). Have the cluster's " +
			"operator declare storage on that tier, or set stream.replicas to a class " +
			"that has some"
	case jetstream.BudgetAccountNoTier:
		// NOT A CAPACITY SENTENCE, which is why it is not folded into the
		// one above. The account grants this node's replica class nothing
		// at all, so the broker refuses every create on it before it
		// compares a byte, and waiting for room is the one thing that
		// will never help.
		return "that limit is zero because the NATS account is TIERED and declares no " +
			"tier for the replica class stream.replicas puts this node in, so the " +
			"broker refuses every stream on it — `no JetStream default or applicable " +
			"tiered limit present` — whatever ceiling is asked for. Set " +
			"stream.replicas to a class the account declares, or have the cluster's " +
			"operator declare that tier"
	case jetstream.BudgetUnstated:
		// REACHED ONLY IF A CALLER STOPS BRANCHING ON THE LIMIT FIRST, an
		// unstated one being negative and read as "no limit this node can
		// see" before it gets here. Written anyway, because "unreachable"
		// is a claim about every caller rather than about this function,
		// and the cost of being wrong is the fallthrough below.
		return "the account states no limit this node can read, so what refused this " +
			"is a cap on a server it does not run; ask whoever operates that cluster"
	}
	return fmt.Sprintf("the broker's limit comes from %q, which this build does "+
		"not know", source)
}

// freeSpace is what an unprivileged process may actually use on the volume
// holding the file at path.
func freeSpace(path string) (int64, error) { return volumeFree(filepath.Dir(path)) }

// volumeFree is what an unprivileged process may actually use on the volume
// holding dir.
//
// Bavail rather than Bfree: the reserve only root can reach is not space this
// engine has, and counting it would derive a ceiling the disk cannot honour.
func volumeFree(dir string) (int64, error) {
	var fs unix.Statfs_t
	if err := unix.Statfs(dir, &fs); err != nil {
		return 0, fmt.Errorf("engine: measure the free space on %s: %w", dir, err)
	}
	//nolint:unconvert // Statfs_t.Bsize is int64 on linux and uint32 on
	// darwin, and both are release targets: the conversion is what makes
	// this one expression compile on the whole matrix.
	return int64(fs.Bavail) * int64(fs.Bsize), nil
}
