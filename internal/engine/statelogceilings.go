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
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE STATE LOGS' STREAM CEILINGS: what each log's stream is created with, and
// what a boot that cannot reserve one says.
//
// A ceiling is a reservation the broker grants in full, so the logs share one
// budget and are sized together over [registeredDomains], never a list here a
// new log could sit outside. A registered domain with no Tier A ceiling fails
// the boot naming itself. Sizing decides only what a MISSING stream is created
// with: a booting node is not a stream's configuration writer, so an existing
// log keeps its ceiling (resizing is the capacity operation's) and counts at it,
// and a log created beside it gets what it leaves ([fitCeilings]).

// domainCeiling is one domain's stream ceiling and where Tier A puts it.
type domainCeiling struct {
	// Bytes is what the stream is created with.
	Bytes int64

	// Field is the Tier A key that governs it, which is what a refusal
	// names.
	Field string

	// Explicit reports that the operator set Field. An explicit ceiling is
	// never scaled: the operator sized it for a broker they can see (perhaps
	// an emergency grant), and a refusal naming the field beats silently
	// lowering it.
	Explicit bool

	// Floor is the smallest value Field accepts (the FIELD's, which Tier A
	// validates, not the budget's), declared by the register entry beside
	// Field, so a refusal offers a smaller ceiling down to it: several
	// fields go well below [MinDomainCeiling], where scaling stops unless
	// the entry declares ScalesTo.
	Floor int64

	// FollowsVolume reports that an UNSET Field derives from the stream
	// volume's free space rather than a fixed default. MEASURED by
	// [followsVolume], not declared, so it cannot disagree with the
	// arithmetic.
	FollowsVolume bool

	// ScalesTo is how far sharing the broker may lower an UNSET ceiling below
	// [MinDomainCeiling]; zero means that gibibyte. A log that grows with
	// headcount rather than a corpus declares its field's floor here, so its
	// default gives way where the broker lacks room instead of being a
	// reservation every fresh node must find in full.
	ScalesTo int64
}

// scaleFloor is where scaling stops for this ceiling: [domainCeiling.ScalesTo]
// where the entry declares one, [MinDomainCeiling] otherwise.
func (c domainCeiling) scaleFloor() int64 {
	if c.ScalesTo > 0 {
		return c.ScalesTo
	}
	return MinDomainCeiling
}

// StreamBudgetShare is how much of what the broker can grant the state logs
// their ceilings may reserve between them, existing streams' ceilings included.
//
// AT MOST TWO THIRDS: the same broker holds the mailboxes, the coordination
// buckets and the join snapshot, which reserve nothing, so what the logs leave
// must cover at least the `nats` object bucket's quarter ([objectsBucketShare]),
// taken of the limit first. AT LEAST 0.593: a re-embedding puts the whole corpus
// in the vector changelog's window (8.46 GB at the reference company's fifth
// year), which must fit beside the other logs' asks on a 64 GiB volume
// (TestTheVectorChangelogIsCreatedToHoldAModelChange, which fails when a newly
// registered log moves the bound). Five eighths is the smallest eighth past it
// (docs/guides/replication.md, "What the code log's reservation costs").
const StreamBudgetShare = 0.625

// MinDomainCeiling is the floor a scaled-down ceiling never goes below, unless
// its register entry declares a lower one ([domainCeiling.ScalesTo]).
//
// A gibibyte: Tier A's own floor on the CORPUS-SIZED logs' fields (the
// tracker's, vectors', knowledge base's, code, artifacts, artifact datastore's
// and chat's), below which such a log refuses appends within a week of a
// company starting. It is where SCALING stops and nobody else's floor: other
// fields go down to 64 MiB, scaling never raises a smaller ask, and the
// capacity verb holds a target to the log's own field floor ([capacityBounds]).
const MinDomainCeiling int64 = 1 << 30

// tierACeiling is where Tier A puts domain's ceiling with free bytes of headroom.
//
// A REGISTER FIELD, not a domain method, for [stateLog.applierFor]'s reason: a
// new domain without one fails here at boot, naming itself. An unset ceiling is
// DERIVED (a fixed number is years of history on one disk, a refused boot on
// another): the mutation log a quarter of free space, clamped; of its ceiling,
// set or derived, the knowledge base a quarter (same window, a quarter the
// rate) and the code log two thirds (config.DerivedCodeLogNumerator; a push
// names its pack, never carries it); the vector changelog the volume share
// whatever the mutation log is set to, as a model change puts the whole corpus
// in its window (config.Stream.VectorsMaxBytes); chat a fixed budget the volume
// caps ([chatMaxBytes], [config.Stream.ChatLogMaxBytes]).
func tierACeiling(stream config.Stream, domain statelog.Domain, free int64) (domainCeiling, error) {
	entry, found := registrationFor(domain.Name())
	if !found || entry.Ceiling == nil {
		return domainCeiling{}, fmt.Errorf("engine: domain %q is registered and Tier A "+
			"declares no ceiling for its stream, so it would reserve its own default "+
			"outside the budget every other state log is sized into", domain.Name())
	}
	ceiling := entry.Ceiling(stream, free)
	ceiling.FollowsVolume = followsVolume(entry, stream)
	return ceiling, nil
}

// chatMaxBytes is the chat log's ask, and whether it was derived.
//
// Not a config.Stream method like its siblings: an unset value's ask is
// DECLARED BY THE DOMAIN ([chatlet.ChatLogMaxBytes], beside its census), which
// internal/config cannot import, and a copy in config would be two spellings of
// one number with nothing holding them together. An unset value is an EXCURSION
// budget (a fully blocked trim), so the disk caps it and the node boots; one an
// operator wrote is theirs. The cap is a QUARTER of the mutation log's share of
// free space ([config.DerivedChatLogDivisor] has the figures): the fit scales
// derived asks in proportion and this one is paid from the vector changelog's
// room for a model change. Never below [config.ChatLogMaxBytesFloor], so a node
// never derives a ceiling its own validation refuses.
func chatMaxBytes(stream config.Stream, free int64) (int64, bool) {
	if stream.ChatLogMaxBytes > 0 {
		return stream.ChatLogMaxBytes, false
	}
	capped := min(int64(chatlet.ChatLogMaxBytes),
		config.DerivedLogMaxBytes(free)/config.DerivedChatLogDivisor)
	return max(capped, config.ChatLogMaxBytesFloor), true
}

// followsVolume reports whether entry's ceiling, left unset, is derived from
// the stream volume's free space: ASKED of the ceiling itself (an empty volume
// and an enormous one size it differently) rather than flagged by hand, a second
// statement of the arithmetic nobody updates. An explicit ceiling follows nothing.
func followsVolume(entry registration, stream config.Stream) bool {
	empty, vast := entry.Ceiling(stream, 0), entry.Ceiling(stream, 1<<50)
	return !empty.Explicit && empty.Bytes != vast.Bytes
}

// ceilingsFor sizes every registered domain's byte budget from Tier A and the
// broker's own budget.
func ceilingsFor(ctx context.Context, host domainHost, boot *config.Bootstrap) (map[string]domainCeiling, error) {
	if boot == nil {
		return nil, errors.New("engine: a state log's ceilings are sized from Tier A, and this node was given none")
	}
	volume := streamVolume(boot)
	free, err := volumeFree(volume)
	if err != nil {
		// LOGGED AND CARRIED with a zero, which every derivation clamps
		// up to its floor: a node that cannot measure its disk still has
		// to boot, and a floor fits any volume this engine runs on.
		log.WarnContext(ctx, "statelog_free_space_unmeasured",
			"path", volume, "error", err.Error(),
			"detail", unmeasuredDetail(diskDerivedFields(boot.Stream)))
	}
	return sizeCeilings(ctx, host, boot.Stream, free, volume)
}

// diskDerivedFields is the Tier A field of every state log whose ceiling is
// unset AND derived from the stream volume's free space, the ones an unmeasured
// volume sizes at their smallest. Asked of the register ([followsVolume])
// rather than listed, since a list drifts from it.
func diskDerivedFields(stream config.Stream) []string {
	var fields []string
	for _, entry := range register() {
		if followsVolume(entry, stream) {
			fields = append(fields, entry.Ceiling(stream, 0).Field)
		}
	}
	return fields
}

// unmeasuredDetail is what a node that could not measure its stream volume
// says it did about the ceilings that volume decides.
func unmeasuredDetail(fields []string) string {
	if len(fields) == 0 {
		return "every state log's ceiling is set in Tier A or fixed, so none " +
			"depends on the free space this node could not measure"
	}
	return "the ceilings derived from the volume's free space are sized as " +
		"though it had none, which is each one's smallest; set " +
		strings.Join(fields, ", ") + " to choose them"
}

// sizeCeilings is [ceilingsFor] on a volume already measured.
//
// The pool is [StreamBudgetShare] of what the broker can grant the logs
// ([logsGrantable]). Its figure ([jetstream.Queue.StreamBudget]) is what it has
// LEFT, which the logs' own ceilings have spent, so they are added back and a
// restart divides the pool the creating boot did. An account stating no limit
// still refuses what its servers cannot back, so the cap a default server takes
// of this volume ([unstatedLimit]) stands in. Free space is never spent by a
// reservation, so the logs' ceilings are NOT added to it (that counts them
// twice), and the other reservations, the object bucket's quarter
// ([objectsReservation]) among them, come off it, or the last log is refused.
func sizeCeilings(ctx context.Context, host domainHost, stream config.Stream,
	free int64, volume string) (map[string]domainCeiling, error) {

	domains := registeredDomains()
	asked := make(map[string]domainCeiling, len(domains))
	for _, domain := range domains {
		ceiling, err := tierACeiling(stream, domain, free)
		if err != nil {
			return nil, err
		}
		asked[domain.Name()] = ceiling
	}
	held := map[string]int64{}
	var holding int64
	for _, read := range readHeld(ctx, host, domains) {
		switch {
		case read.err != nil:
			// CARRIED AS ABSENT, not a failed boot: an unanswered read
			// was already retried for the whole lookup ceiling
			// ([jetstream.Queue.DomainStreamCeiling]), and the provision
			// that follows asks again. An existing stream keeps its
			// ceiling either way, but every log this boot creates is
			// sized without what it holds (counted at its ask, its
			// reservation not added back), and a created ceiling is kept
			// for good: the boot succeeds, so only this line says so.
			log.WarnContext(ctx, "statelog_ceiling_unread",
				"domain", read.domain.Name(), "stream", read.domain.Stream().Name,
				"error", read.err.Error(),
				"detail", "the broker did not say whether this log's stream "+
					"exists or what ceiling it holds, so it is counted as "+
					"absent: if it exists, every log this boot creates is "+
					"sized without knowing what it holds, and keeps the "+
					"ceiling that gives it — larger or smaller than a boot "+
					"that read it would make — until `crewlet retention "+
					"set-capacity` changes it once this node is up")
		case read.found:
			held[read.domain.Name()] = read.holds
			holding += read.holds
		}
	}

	budget, err := host.StreamBudget(ctx)
	if err != nil {
		log.WarnContext(ctx, "statelog_broker_budget_unread", "error", err.Error(),
			"detail", "the state logs' ceilings are sized from this node's free "+
				"space instead, and the broker still refuses one it cannot reserve")
	}
	grantable := logsGrantable(budget, err, free, holding)
	// A ZERO LIMIT FROM AN ACCOUNT DECLARING NONE FOR THIS NODE IS SAID
	// OUT LOUD: the one zero that is a misconfiguration. The broker refuses
	// every create on it, so a node whose objects all exist works until it
	// must make something new. Every other zero is a full broker, which the
	// refusal explains when it arrives.
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
	// A LOG CREATED BELOW ITS FIT IS SAID OUT LOUD: the boot succeeds, and
	// the next sign would be a log refusing appends early. The broker's
	// capacity line reports the logs whose holdings did it; this names the
	// one they took from, and the gesture that moves both.
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

// logsGrantable is what the broker can grant the state logs if they held
// nothing: the figure [StreamBudgetShare] is taken of.
//
// One rule for every broker: the limit, less every reservation not a state
// log's. A stated limit gives what is left plus the logs' own ceilings
// (holding). Otherwise the cap such a server takes of free space
// ([unstatedLimit]) stands in, less what the broker reports reserved beyond the
// logs' own; free space already includes the logs' ceilings, since a
// reservation never spends it. An unread budget reports no reservation.
func logsGrantable(budget jetstream.StorageBudget, readErr error, free, holding int64) int64 {
	if readErr != nil {
		return unstatedLimit(free)
	}
	if available := budget.Available(); available >= 0 {
		return available + holding
	}
	return max(unstatedLimit(free)-max(budget.Committed-holding, 0), 0)
}

// heldCeiling is one log's answer to [domainHost.DomainStreamCeiling]: the
// ceiling its stream already holds, whether it exists, or why the broker did
// not say.
type heldCeiling struct {
	domain statelog.Domain
	holds  int64
	found  bool
	err    error
}

// readHeld asks the broker what every registered log's stream already holds,
// all reads at once, the answers in the register's order.
//
// AT ONCE because an unanswered read is retried for up to
// [jsprovision.LookupBudget] ([jetstream.Queue.DomainStreamCeiling]): in
// sequence the boot pays the sum of every read's ceiling, and one ceiling SHARED
// by a sequence starves the last read. Together the pass costs its slowest read,
// and each read gets the whole ceiling. Nothing orders them; the register's
// order holds because answers return only after the last read. Each logs a
// breadcrumb while waiting, for [jsprovision.WhenSlow]'s reason: a silent read
// is otherwise quiet until `statelog_ceiling_unread`.
func readHeld(ctx context.Context, host domainHost, domains []statelog.Domain) []heldCeiling {
	reads := make([]heldCeiling, len(domains))
	var wg sync.WaitGroup
	for i, domain := range domains {
		reads[i].domain = domain
		stream := domain.Stream().Name
		wg.Go(func() {
			stop := jsprovision.WhenSlow(ctx, func(after time.Duration) {
				log.WarnContext(ctx, "statelog_ceiling_read_slow",
					"domain", domain.Name(), "stream", stream, "waited", after,
					"detail", "still asking the broker what ceiling this log's "+
						"stream already holds, before any log is created; on a "+
						"fleet that is usually a stream another node has in "+
						"flight, and `statelog_ceilings` or "+
						"`statelog_ceiling_unread` says how it ended")
			})
			defer stop()
			reads[i].holds, reads[i].found, reads[i].err = host.DomainStreamCeiling(ctx, stream)
		})
	}
	wg.Wait()
	return reads
}

// fitCeilings sizes every log inside pool: what a MISSING log is created with,
// and the value an EXISTING one is reported against, never applied. held is
// the ceiling each existing log's stream holds, by domain.
//
// An existing log counts at what it HOLDS, not its ask, so the missing logs
// divide what the existing ones leave; holdings above the pool leave them their
// floors ([shortOfFit] reports it, [stateLog.storageRefused] names a refusal).
// An existing log's own value is its fit on an EMPTY broker, the figure a
// restart reproduces, and a missing log is created at NO MORE than that fit:
// above it (a log holding under its fit, or floor rounding after a boot stopped
// between creates) is a capacity difference nobody made, reported every boot.
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
// Explicit ceilings come off the pool FIRST and the derived ones share the rest
// in proportion to their asks; scaling over a total counting the explicit ones
// hands out bytes those already spent. A log whose share falls below its
// [domainCeiling.scaleFloor] is HELD there and the rest divide what remains;
// raising it afterwards gives its floor bytes the others were also given. So the
// total exceeds the pool only by the floors and explicit ceilings that do not
// fit, which the broker grants or refuses by name. A pool below zero is a pool
// of nothing: every derived log is at its floor.
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
			if proportion(asked[name].Bytes, share, want) < asked[name].scaleFloor() {
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
			ceiling.Bytes = min(ceiling.Bytes, ceiling.scaleFloor())
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
// derived from: the stream's own directory when an embedded broker keeps one,
// the volume a full log fills and the broker's cap is taken from. An in-memory
// or external broker has no local stream volume, and the store's is the one
// figure this node has.
func streamVolume(boot *config.Bootstrap) string {
	if dir := strings.TrimSpace(boot.Stream.StoreDir); dir != "" && boot.Stream.Type != config.StreamNATS {
		return dir
	}
	return filepath.Dir(boot.Store.Path)
}

// ceilingFor is the ceiling a domain's log was sized at: its whole Tier A
// budget, fitted into the broker's pool.
func (s *stateLog) ceilingFor(domain statelog.Domain) (domainCeiling, error) {
	ceiling, sized := s.ceilings[domain.Name()]
	if !sized || ceiling.Bytes <= 0 {
		return domainCeiling{}, fmt.Errorf("engine: %s's log was never sized, and a "+
			"stream created at a default nobody budgeted is the reservation that "+
			"refuses a boot", domain.Name())
	}
	return ceiling, nil
}

// specOf is the domain's stream as this node creates it: the domain's own
// ([statelog.Domain.Stream]) at the ceiling Tier A sized
// ([stateLog.ceilingFor]) rather than the domain's declared default — so the
// spec a log is built from and the ceiling its stream is created with are one
// number.
func (s *stateLog) specOf(domain statelog.Domain) (statelog.StreamSpec, domainCeiling, error) {
	ceiling, err := s.ceilingFor(domain)
	if err != nil {
		return statelog.StreamSpec{}, domainCeiling{}, err
	}
	spec := domain.Stream()
	spec.MaxBytes = ceiling.Bytes
	return spec, ceiling, nil
}

// storageRefused is the error a boot returns when the broker will not reserve
// a state log's ceiling.
//
// The broker's `insufficient storage resources available` names nothing an
// operator can change, so this names what the stream asked, what the broker had
// and what sets that limit, and the Tier A field the ceiling came from, as SET,
// DERIVED, SCALED ([domainCeiling.ScalesTo]) or FIXED, since each bounds a
// smaller ceiling differently, down to the FIELD's floor. Remedies are only
// those this node can reach: `crewlet retention set-capacity` needs the state
// logs up, which every mode starts, so it would fail too; "more room" quotes
// [BrokerFloor]'s least so one addition suffices. The budget is read AGAIN here: logs created since sizing have
// reserved, and what matters is what the broker had when it said no.
func (s *stateLog) storageRefused(ctx context.Context, host domainHost,
	spec statelog.StreamSpec, domain statelog.Domain, ceiling domainCeiling, cause error) error {

	budget, err := host.StreamBudget(ctx)
	had := roomLeft(budget, err, s.volume)
	var from string
	switch {
	case ceiling.Explicit:
		from = fmt.Sprintf("%s sets the ceiling", ceiling.Field)
		if fits := budget.Available(); err == nil && fits >= ceiling.Floor {
			from += fmt.Sprintf(", and at most %d bytes fits", fits)
		}
	case ceiling.FollowsVolume:
		from = fmt.Sprintf("%s is unset, so the ceiling was derived from the "+
			"stream volume's free space and scaled into the state logs' share of "+
			"the broker, and scaling takes it no lower than %d bytes",
			ceiling.Field, ceiling.scaleFloor())
	case ceiling.ScalesTo > 0:
		from = fmt.Sprintf("%s is unset, so the ceiling is the log's own default "+
			"scaled into the state logs' share of the broker, and scaling takes it "+
			"no lower than %d bytes", ceiling.Field, ceiling.scaleFloor())
	default:
		from = fmt.Sprintf("%s is unset, so the ceiling is the log's own fixed "+
			"default rather than a share of the stream volume's free space",
			ceiling.Field)
	}
	remedy := "Give the broker more room"
	if ceiling.Bytes > ceiling.Floor {
		remedy += fmt.Sprintf(", or set %s to a smaller ceiling, no lower than "+
			"its %d-byte floor", ceiling.Field, ceiling.Floor)
	}
	if s.floor.Limit > 0 && (err != nil || !noApplicableLimit(budget.Source)) {
		remedy += "; " + s.floor.Sentence()
	}
	return fmt.Errorf("engine: the broker refused to reserve the %s log's "+
		"ceiling: %s needed %d bytes and %s. %s. %s; the state logs that already "+
		"exist keep the ceilings they were created with, and no Tier A setting "+
		"changes them: %w",
		domain.Name(), spec.Name, ceiling.Bytes, had, from, remedy, cause)
}

// roomLeft is what the broker had to reserve when it refused, in an operator's
// terms: what is left, what of the limit is reserved, the limit, and who sets it.
//
// One sentence for both refusals, [stateLog.storageRefused] and
// [Engine.capacityRefusal], so the two cannot drift. PURE over its inputs: the
// numbers quoted are the numbers that decided, never a second live read. The
// three cases never collapse: an unread limit is not a zero, and a broker that
// states none still has server caps this client cannot see.
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
// limit that applies to this node at all, rather than a limit that is spent:
// decided by source, not a zero test, since a spent limit clears by freeing
// room and these only by a configuration change.
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
		// BOTH SOURCES, since the broker reports the number but not which
		// set it; naming only the volume sends an operator who set
		// stream.store_max_bytes to a disk that has room.
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
		// TWO REALITIES UNDER ONE REPORT, so this names both refusals: a
		// class missing from the limit table is refused before a byte is
		// compared, and one declared with NO DISK (a memory-only tier) by
		// the byte comparison. The account's report cannot tell them apart.
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
		// NOT A CAPACITY SENTENCE, so not folded into the one above: the
		// account grants this replica class nothing, every create is
		// refused before a byte is compared, and waiting for room never helps.
		return "that limit is zero because the NATS account is TIERED and declares no " +
			"tier for the replica class stream.replicas puts this node in, so the " +
			"broker refuses every stream on it — `no JetStream default or applicable " +
			"tiered limit present` — whatever ceiling is asked for. Set " +
			"stream.replicas to a class the account declares, or have the cluster's " +
			"operator declare that tier"
	case jetstream.BudgetUnstated:
		// REACHED ONLY IF A CALLER STOPS BRANCHING ON A NEGATIVE LIMIT
		// FIRST; written anyway, since "unreachable" is a claim about every
		// caller and the fallthrough below is the cost of being wrong.
		return "the account states no limit this node can read, so what refused this " +
			"is a cap on a server it does not run; ask whoever operates that cluster"
	}
	return fmt.Sprintf("the broker's limit comes from %q, which this build does "+
		"not know", source)
}

// volumeFree is what an unprivileged process may actually use on the volume
// holding dir.
//
// Bavail rather than Bfree: the reserve only root can reach is not space this
// engine has, and counting it would derive a ceiling the disk cannot honour.
func volumeFree(dir string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, fmt.Errorf("engine: measure the free space on %s: %w", dir, err)
	}
	//nolint:unconvert // Statfs_t.Bsize is int64 on linux and uint32 on
	// darwin, and both are release targets: the conversion is what makes
	// this one expression compile on the whole matrix.
	return int64(st.Bavail) * int64(st.Bsize), nil
}
