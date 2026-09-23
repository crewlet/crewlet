package engine

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

const gib = int64(1) << 30

// THE GATE RESERVE AT THE SMALLEST CEILING ABSORBS A FLEET'S APPENDS IN FLIGHT.
//
// The reserve is a fraction of the ceiling and the overshoot it absorbs is not:
// every peer of the admitting node may hold up to [statelog.MaxAppendBytes] in
// flight that its reading cannot see. So the fraction is only sound down to
// some ceiling, and every floor a log's ceiling can reach — the one the
// division of the broker holds a log at, and Tier A's floor for each
// identity-claiming log — must be at or above it, with room left for the gate
// records themselves. A divisor raised, a fleet size raised or a floor lowered
// without the others moving fails here, rather than as an eviction refused on
// a full log.
func TestTheGateReserveAtEveryFloorAbsorbsItsFleet(t *testing.T) {
	// A thousand gate records, each under a kibibyte: an eviction and a
	// readmission per node per identity log, many times over.
	const gateRoom = 1 << 20
	peers := uint64(statelog.GateReserveFleet-1) * statelog.MaxAppendBytes
	for name, floor := range map[string]int64{
		"the division's MinDomainCeiling":             MinDomainCeiling,
		"Tier A's stream.tracker_log_max_bytes floor": config.TrackerLogMaxBytesFloor,
		"Tier A's stream.pages_log_max_bytes floor":   config.PagesLogMaxBytesFloor,
	} {
		reserve := statelog.GateReserve(uint64(floor))
		if reserve < peers+gateRoom {
			t.Errorf("at %s (%d bytes) the gate reserve is %d bytes: %d peers' "+
				"appends in flight at %d bytes each, plus %d for the gate records, "+
				"need %d — an eviction on a log full to its ordinary ceiling can "+
				"find the reserve spent", name, floor, reserve,
				statelog.GateReserveFleet-1, int64(statelog.MaxAppendBytes),
				gateRoom, peers+gateRoom)
		}
	}
}

// EVERY REGISTERED DOMAIN IS SIZED FROM TIER A, and the field a refusal names
// is the field that actually sets the value.
//
// The pages log was registered with no Tier A ceiling at all and reserved a
// fixed default outside the budget the other two logs were scaled into. So the
// assertion is over the register rather than over a list: a domain added to
// it without a Tier A field fails here rather than on somebody's small disk.
// And each field is SET, through its own YAML key, and read back as the value,
// because a refusal naming the wrong key sends an operator to edit a number
// that changes nothing.
func TestEveryRegisteredDomainIsSizedFromTierA(t *testing.T) {
	t.Parallel()
	keys := streamKeys(t)
	for _, domain := range registeredDomains() {
		t.Run(domain.Name(), func(t *testing.T) {
			t.Parallel()
			derived, err := tierACeiling(config.Stream{}, domain, 200*gib)
			if err != nil {
				t.Fatalf("tierACeiling: %v", err)
			}
			if derived.Bytes < MinDomainCeiling || derived.Explicit {
				t.Errorf("unset = %+v, want a derived ceiling of at least %d",
					derived, MinDomainCeiling)
			}
			key, found := strings.CutPrefix(derived.Field, "stream.")
			field, known := keys[key]
			if !found || !known {
				t.Fatalf("%s's ceiling is governed by %q, which is not a Tier A "+
					"stream key", domain.Name(), derived.Field)
			}

			var stream config.Stream
			reflect.ValueOf(&stream).Elem().FieldByIndex(field).SetInt(3 * gib)
			set, err := tierACeiling(stream, domain, 200*gib)
			if err != nil {
				t.Fatalf("tierACeiling: %v", err)
			}
			if set.Bytes != 3*gib || !set.Explicit || set.Field != derived.Field {
				t.Errorf("with %s set to %d = %+v, want that value, explicit, "+
					"under the same field", derived.Field, 3*gib, set)
			}
		})
	}
}

// streamKeys maps each Tier A stream key to its field.
func streamKeys(t *testing.T) map[string][]int {
	t.Helper()
	out := map[string][]int{}
	typ := reflect.TypeFor[config.Stream]()
	for i := range typ.NumField() {
		key, _, _ := strings.Cut(typ.Field(i).Tag.Get("yaml"), ",")
		if key != "" {
			out[key] = typ.Field(i).Index
		}
	}
	return out
}

// THE ARITHMETIC FITS THE POOL, and every way an earlier version overshot it
// is a row here.
//
// The first version scaled every derived ceiling by pool over a total that
// COUNTED the explicit ones (so the derived logs were handed bytes an explicit
// ceiling had already spent) and raised a floored log to the floor AFTER
// dividing (so the floor was bytes the others were also given). The next
// divided the pool by what every log ASKED, the ones that already exist
// included, so a log being created was sized as though an existing one held
// its ask — and one holding more than that was bytes the missing logs were
// also given. Measured at twice the share: the "a log holding twice the
// share" row is that boot's arithmetic.
//
// So every row is held to the INVARIANTS as well as to its numbers, and there
// are three, because "inside the pool" is not one of them: logs that already
// hold more than the pool hold it whatever this arithmetic says.
//
//   - What the logs reserve between them — what the existing ones hold plus
//     what the missing ones are created with — exceeds the larger of the pool
//     and what the existing ones hold only by the missing logs created at
//     their floor or at a ceiling somebody set.
//   - A derived log created above its floor is proof there was room to spare,
//     so it is never found beside a total past the pool.
//   - A missing log is never created above its fit on an empty broker, which
//     is the value every later boot reports its stream against: above it, the
//     log is a capacity difference nobody made on every boot that follows.
func TestTheCeilingsFitThePool(t *testing.T) {
	t.Parallel()
	derived := func(bytes int64) domainCeiling { return domainCeiling{Bytes: bytes} }
	explicit := func(bytes int64) domainCeiling { return domainCeiling{Bytes: bytes, Explicit: true} }
	for name, tc := range map[string]struct {
		asked map[string]domainCeiling
		held  map[string]int64
		pool  int64
		want  map[string]int64
		// short is what [shortOfFit] reports: every log created below
		// its fit on an empty broker, with that fit.
		short map[string]int64
	}{
		"everything fits as asked": {
			asked: map[string]domainCeiling{"a": derived(4 * gib), "b": derived(2 * gib)},
			pool:  8 * gib,
			want:  map[string]int64{"a": 4 * gib, "b": 2 * gib},
		},
		"derived ceilings scale together to the pool": {
			asked: map[string]domainCeiling{"a": derived(8 * gib), "b": derived(4 * gib)},
			pool:  6 * gib,
			want:  map[string]int64{"a": 4 * gib, "b": 2 * gib},
		},
		"an explicit ceiling comes off the pool first": {
			asked: map[string]domainCeiling{"a": explicit(2 * gib), "b": derived(4 * gib)},
			pool:  4 * gib,
			want:  map[string]int64{"a": 2 * gib, "b": 2 * gib},
		},
		"a floored log is held and the rest share what is left": {
			asked: map[string]domainCeiling{
				"a": derived(16 * gib), "b": derived(16 * gib), "c": derived(4 * gib),
			},
			pool: 5 * gib,
			want: map[string]int64{"a": 2 * gib, "b": 2 * gib, "c": gib},
		},
		"the floors hold when the pool cannot": {
			asked: map[string]domainCeiling{
				"a": derived(4 * gib), "b": derived(4 * gib), "c": derived(gib),
			},
			pool: gib,
			want: map[string]int64{"a": gib, "b": gib, "c": gib},
		},
		"an explicit ceiling past the pool is not scaled": {
			asked: map[string]domainCeiling{"a": explicit(32 * gib), "b": derived(4 * gib)},
			pool:  8 * gib,
			want:  map[string]int64{"a": 32 * gib, "b": gib},
		},
		"a log that asked under the floor is not raised to it": {
			asked: map[string]domainCeiling{"a": derived(64 * gib), "b": derived(gib / 2)},
			pool:  gib,
			want:  map[string]int64{"a": gib, "b": gib / 2},
		},
		"a tebibyte-scale volume does not overflow": {
			asked: map[string]domainCeiling{"a": explicit(1024 * gib), "b": derived(64 * gib), "c": derived(16 * gib)},
			pool:  1064 * gib,
			want:  map[string]int64{"a": 1024 * gib, "b": 32 * gib, "c": 8 * gib},
		},
		// An existing log is reported at its fit on an empty broker (a's
		// 4), and the missing ones divide the 6 its 10 leave rather than
		// the 12 its fit would: sized from its ask they took 12 and the
		// logs reserved 22 of a 16 pool.
		"a log that exists counts at what it holds, not at what it asks": {
			asked: map[string]domainCeiling{
				"a": derived(8 * gib), "b": derived(16 * gib), "c": derived(8 * gib),
			},
			held:  map[string]int64{"a": 10 * gib},
			pool:  16 * gib,
			want:  map[string]int64{"a": 4 * gib, "b": 4 * gib, "c": 2 * gib},
			short: map[string]int64{"b": 8 * gib, "c": 4 * gib},
		},
		// The measured boot: a tracker log created at an explicit 16 GiB
		// that was later unset, the broker with 398676992 bytes left, and
		// so a pool of 8789273088, here on a 16 GiB volume. The vector
		// changelog was sized at 3857765632 in bytes the tracker already
		// held; it is the floor, and a broker that cannot grant even that
		// refuses it by name.
		"a log holding twice the share leaves the missing ones their floors": {
			asked: map[string]domainCeiling{
				"tracker": derived(4 * gib), "vectors": derived(4 * gib), "pages": derived(gib),
			},
			held: map[string]int64{"tracker": 16 * gib},
			pool: (398676992 + 16*gib) / 2,
			want: map[string]int64{
				"tracker": 3857765632, "vectors": gib, "pages": gib,
			},
			short: map[string]int64{"vectors": 3857765632},
		},
		// An explicit ceiling is what the operator wrote for a stream this
		// boot creates, and nothing about a stream that exists: 10 GiB is
		// what a's reservation is, whatever its field says now.
		"an explicit log that exists counts at what it holds, not at its field": {
			asked: map[string]domainCeiling{"a": explicit(2 * gib), "b": derived(16 * gib)},
			held:  map[string]int64{"a": 10 * gib},
			pool:  12 * gib,
			want:  map[string]int64{"a": 2 * gib, "b": 2 * gib},
			short: map[string]int64{"b": 10 * gib},
		},
		// And the other direction: a log holding less than its fit (a
		// capacity operation gave some back) leaves room the missing log
		// is NOT given. The 6 a's 2 leave would create b above its fit of
		// 4, which is what every later boot reports b's stream against —
		// a capacity difference on every boot for the life of the stream.
		"a log holding less than its fit leaves a missing one at its fit": {
			asked: map[string]domainCeiling{"a": derived(8 * gib), "b": derived(8 * gib)},
			held:  map[string]int64{"a": 2 * gib},
			pool:  8 * gib,
			want:  map[string]int64{"a": 4 * gib, "b": 4 * gib},
		},
		// A first boot that stopped after creating the tracker's stream at
		// its fit, measured: the logs the restart creates divide the
		// 8947848534 the tracker leaves, and the vector changelog's share
		// of that floors to 7158278827 — one byte over the 7158278826 the
		// empty-broker division gives it. Created there, it was reported
		// as a capacity difference on every boot after.
		"a boot that stopped between two creates sizes the rest at their fit": {
			asked: map[string]domainCeiling{
				"tracker": derived(16 * gib), "vectors": derived(16 * gib), "pages": derived(4 * gib),
			},
			held: map[string]int64{"tracker": 7158278826},
			pool: 16106127360,
			want: map[string]int64{
				"tracker": 7158278826, "vectors": 7158278826, "pages": 1789569706,
			},
		},
		// A restart: nothing is missing, so what the logs hold changes no
		// value, and each is the number the broker's capacity line holds
		// its stream against.
		"when every log exists each is reported at its fit": {
			asked: map[string]domainCeiling{
				"a": derived(8 * gib), "b": derived(16 * gib), "c": derived(8 * gib),
			},
			held: map[string]int64{"a": 16 * gib, "b": gib, "c": gib},
			pool: 16 * gib,
			want: map[string]int64{"a": 4 * gib, "b": 8 * gib, "c": 4 * gib},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := fitCeilings(tc.asked, tc.held, tc.pool)
			for name, want := range tc.want {
				if got[name].Bytes != want {
					t.Errorf("%s = %d, want %d (all: %+v)", name, got[name].Bytes, want, got)
				}
				if got[name].Explicit != tc.asked[name].Explicit {
					t.Errorf("%s's explicit flag changed", name)
				}
			}
			fit := divide(tc.asked, tc.pool)
			var reserved, holding, allowed int64
			var roomToSpare []string
			for name, ceiling := range got {
				if holds, exists := tc.held[name]; exists {
					reserved += holds
					holding += holds
					continue
				}
				reserved += ceiling.Bytes
				if ceiling.Bytes > fit[name].Bytes {
					t.Errorf("%s is created at %d, above its fit of %d on an "+
						"empty broker — every later boot reports its stream as "+
						"a capacity difference", name, ceiling.Bytes, fit[name].Bytes)
				}
				if ceiling.Explicit || ceiling.Bytes <= MinDomainCeiling {
					allowed += ceiling.Bytes
					continue
				}
				roomToSpare = append(roomToSpare, name)
			}
			if bound := max(tc.pool, holding) + allowed; reserved > bound {
				t.Errorf("the logs reserve %d bytes between them, past the %d "+
					"that the larger of the pool (%d) and what the existing logs "+
					"hold (%d) allows with the floors and set ceilings of the "+
					"missing ones: %+v", reserved, bound, tc.pool, holding, got)
			}
			if reserved > tc.pool && len(roomToSpare) > 0 {
				t.Errorf("the logs reserve %d bytes of a %d-byte pool between "+
					"them, and %v were created above the floor: %+v",
					reserved, tc.pool, roomToSpare, got)
			}
			short := shortOfFit(tc.asked, tc.held, tc.pool, got)
			if len(short) != len(tc.short) {
				t.Errorf("short of their fit = %v, want %v", short, tc.short)
			}
			for name, fit := range tc.short {
				if short[name] != fit {
					t.Errorf("short of their fit = %v, want %v", short, tc.short)
				}
			}
		})
	}
}

// sizingHost is a broker that answers the two questions sizing asks — what it
// can grant, and what each log's stream holds — and nothing else.
//
// The rest of [domainHost] is the NIL interface it embeds, so a sizing that
// began provisioning, or opened a log or a consumer, panics rather than
// succeeding against a broker nobody modelled.
type sizingHost struct {
	domainHost
	budget jetstream.StorageBudget
	unread error
	// held is each existing stream's ceiling, by stream name.
	held map[string]int64
}

func (h sizingHost) StreamBudget(context.Context) (jetstream.StorageBudget, error) {
	return h.budget, h.unread
}

func (h sizingHost) DomainStreamCeiling(_ context.Context, stream string) (int64, bool, error) {
	holds, exists := h.held[stream]
	return holds, exists, nil
}

// A LOG CREATED BESIDE ONES THAT EXIST FITS THE SHARE, on every reading of the
// broker's budget.
//
// Two ways past it, one per reading. Where the broker states its limit, the
// missing logs were sized as though the existing one held its ask. Where the
// pool falls back to the volume's free space, the existing ceilings were ADDED
// to it as well, although free space never paid for a reservation — so even
// sized from what the others hold, the missing logs were given half of what
// they hold again.
func TestALogCreatedBesideOnesThatExistFitsTheShare(t *testing.T) {
	t.Parallel()
	// 64 GiB free: the tracker and the vector changelog each ask for
	// 16 GiB and the knowledge base's log for 4. The tracker's stream
	// already holds 16.
	const free = 64 * gib
	const trackerHolds = 16 * gib
	for name, tc := range map[string]struct {
		budget jetstream.StorageBudget
		unread error
		// grantable is what the broker could grant the logs if they held
		// nothing, worked out here rather than read back from the code.
		grantable int64
	}{
		// 44 GiB, of which 4 GiB is reserved by other streams and 16 GiB
		// by the tracker's.
		"a broker that states its limit": {
			budget: jetstream.StorageBudget{Limit: 44 * gib, Committed: 20 * gib,
				Source: jetstream.BudgetServerStore},
			grantable: 40 * gib,
		},
		"a broker that states no limit": {
			budget:    jetstream.StorageBudget{Limit: -1, Source: jetstream.BudgetUnstated},
			grantable: free,
		},
		"a budget that could not be read": {
			unread:    errors.New("no responders"),
			grantable: free,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			host := sizingHost{budget: tc.budget, unread: tc.unread, held: map[string]int64{
				tracker.Domain{}.Stream().Name: trackerHolds,
			}}
			sized, err := sizeCeilings(t.Context(), host, config.Stream{}, free, "/var/lib/crewlet/stream")
			if err != nil {
				t.Fatalf("sizeCeilings: %v", err)
			}
			reserved := trackerHolds
			for _, domain := range registeredDomains() {
				if domain.Name() != (tracker.Domain{}).Name() {
					reserved += sized[domain.Name()].Bytes
				}
			}
			if share := int64(float64(tc.grantable) * StreamBudgetShare); reserved > share {
				t.Errorf("the state logs reserve %d bytes between them, of a "+
					"%d-byte share: %+v", reserved, share, sized)
			}
		})
	}
}

// A RESTART SIZING FROM FREE SPACE SIZES THE LOGS AS THE FIRST BOOT DID.
//
// [TestARestartSizesTheLogsAsTheFirstBootDid] holds this against a broker that
// states its limit, where the logs' reservations spend the figure and have to
// be added back. Free space is the figure a node falls back to where the
// broker states none or cannot be read, and no reservation spends it, so
// adding them back grew the pool on every restart by half of what the logs
// hold: at 16 GiB free the first boot scaled 9 GiB of asks into an 8 GiB share,
// and a restart divided 12 and reported every stream as a difference nobody
// had made.
func TestARestartSizingFromFreeSpaceSizesTheLogsAsTheFirstBootDid(t *testing.T) {
	t.Parallel()
	const free = 16 * gib
	for name, tc := range map[string]struct {
		budget jetstream.StorageBudget
		unread error
	}{
		"a broker that states no limit": {
			budget: jetstream.StorageBudget{Limit: -1, Source: jetstream.BudgetUnstated},
		},
		"a budget that could not be read": {unread: errors.New("no responders")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			host := sizingHost{budget: tc.budget, unread: tc.unread}
			first, err := sizeCeilings(t.Context(), host, config.Stream{}, free, "/var/lib/crewlet/stream")
			if err != nil {
				t.Fatalf("sizeCeilings: %v", err)
			}
			host.held = map[string]int64{}
			for _, domain := range registeredDomains() {
				host.held[domain.Stream().Name] = first[domain.Name()].Bytes
			}
			again, err := sizeCeilings(t.Context(), host, config.Stream{}, free, "/var/lib/crewlet/stream")
			if err != nil {
				t.Fatalf("sizeCeilings: %v", err)
			}
			if !reflect.DeepEqual(first, again) {
				t.Errorf("the first boot sized %+v and a restart %+v", first, again)
			}
		})
	}
}

// A FIRST BOOT THAT STOPPED BETWEEN TWO CREATES LEAVES LOGS A RESTART REPORTS
// AS THEY WERE MADE.
//
// A boot provisions the logs one after another, and a kill, a timeout or a
// refused create between two of them leaves some streams existing and the rest
// missing. The restart sizes the missing ones from what the existing ones
// leave of the share — and on this budget the vector changelog's part of that
// remainder floors to one byte past the fit every later boot holds its stream
// against, so from then on every boot of every node logged a capacity
// difference nobody had made. So the three sizings are run as the broker sees
// them: the first boot, a restart where only the tracker's stream was created
// (its reservation spent from the broker's figure), and a restart with all
// three, where every stream must be reported at exactly what it holds.
func TestAnInterruptedFirstBootLeavesLogsARestartReportsAsMade(t *testing.T) {
	t.Parallel()
	// 64 GiB free under a 30 GiB limit: the tracker and the vector
	// changelog each ask for 16 GiB and the knowledge base's log for 4,
	// into a 15 GiB share.
	const free = 64 * gib
	host := sizingHost{budget: jetstream.StorageBudget{
		Limit: 30 * gib, Source: jetstream.BudgetServerStore,
	}}
	boot := func() map[string]domainCeiling {
		t.Helper()
		sized, err := sizeCeilings(t.Context(), host, config.Stream{}, free, "/var/lib/crewlet/stream")
		if err != nil {
			t.Fatalf("sizeCeilings: %v", err)
		}
		return sized
	}
	create := func(name string, bytes int64) {
		for _, domain := range registeredDomains() {
			if domain.Name() == name {
				host.held[domain.Stream().Name] = bytes
				host.budget.Committed += bytes
				return
			}
		}
		t.Fatalf("no registered domain is named %q", name)
	}

	first := boot()
	host.held = map[string]int64{}
	trackerName := tracker.Domain{}.Name()
	create(trackerName, first[trackerName].Bytes)

	restart := boot()
	for _, domain := range registeredDomains() {
		if domain.Name() != trackerName {
			create(domain.Name(), restart[domain.Name()].Bytes)
		}
	}

	again := boot()
	for _, domain := range registeredDomains() {
		holds := host.held[domain.Stream().Name]
		if reported := again[domain.Name()].Bytes; reported != holds {
			t.Errorf("%s's stream holds %d and every boot now reports it against "+
				"%d — a capacity difference nobody made", domain.Name(), holds, reported)
		}
	}
}

// brokerWithHeadroom is an embedded file-store broker with exactly headroom
// bytes left to reserve.
//
// The broker's own cap is three quarters of this machine's free disk, which a
// test cannot choose; what it can choose is how much of that is already
// reserved, and a reservation is all the cap counts. A machine whose disk
// cannot hold the scenario at all is skipped, loudly, rather than failed for
// a reason that is not the code's.
func brokerWithHeadroom(t *testing.T, headroom int64) *jetstream.Queue {
	t.Helper()
	q, err := jetstream.Open(t.Context(), jetstream.Config{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open the broker: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
	budget, err := q.StreamBudget(t.Context())
	if err != nil {
		t.Fatalf("StreamBudget: %v", err)
	}
	if budget.Available() < headroom+gib {
		t.Skipf("this machine's broker can reserve %d bytes, and the scenario "+
			"needs %d of headroom plus a gibibyte to spend around it",
			budget.Available(), headroom)
	}
	if err := q.EnsureDomainStream(t.Context(), jetstream.DomainStream{
		Name:       "CREWLET_TEST_ELSEWHERE",
		Subjects:   []string{"crewlet.test.elsewhere.>"},
		MaxBytes:   budget.Available() - headroom,
		Duplicates: time.Minute,
	}); err != nil {
		t.Fatalf("reserve all but the headroom: %v", err)
	}
	return q
}

// sizeAndProvision sizes every registered domain on a volume with free bytes
// and provisions each against q, as a boot does.
func sizeAndProvision(t *testing.T, q *jetstream.Queue, stream config.Stream, free int64) error {
	t.Helper()
	ceilings, err := sizeCeilings(t.Context(), q, stream, free, "/var/lib/crewlet/stream")
	if err != nil {
		t.Fatalf("sizeCeilings: %v", err)
	}
	s := &stateLog{ceilings: ceilings, volume: "/var/lib/crewlet/stream"}
	_, err = s.provisionAll(t.Context(), q)
	return err
}

// THE STATE LOGS BOOT ON THE HOST THAT REFUSED THEM.
//
// Measured: a node on a volume with 7.6 GiB free refused to boot with the
// broker's `insufficient storage resources available`, and booted with 16 GiB.
// The broker's cap there is three quarters of the volume, 5.7 GiB. The tracker
// and vector logs were scaled into half of the free space, 1.9 GiB each, and
// the pages log then reserved its fixed 4 GiB on top: 7.8 GiB against 5.7.
// Every log is sized inside one budget now, and all three fit.
func TestTheStateLogsFitTheBrokerTheyBootOn(t *testing.T) {
	t.Parallel()
	// 7.6 GiB, in tenths so the arithmetic stays in integers.
	free := 76 * gib / 10
	brokerCap := free / 4 * 3
	q := brokerWithHeadroom(t, brokerCap)
	if err := sizeAndProvision(t, q, config.Stream{}, free); err != nil {
		t.Fatalf("the state logs did not fit a broker with %d bytes to give them: %v", brokerCap, err)
	}
	var reserved int64
	for _, domain := range registeredDomains() {
		held, found, err := q.DomainStreamCeiling(t.Context(), domain.Stream().Name)
		if err != nil || !found {
			t.Fatalf("%s's stream = (found %v, %v)", domain.Name(), found, err)
		}
		reserved += held
	}
	if reserved > brokerCap {
		t.Errorf("the state logs reserved %d bytes of a %d-byte cap", reserved, brokerCap)
	}
}

// A RESTART SIZES THE LOGS AS THE BOOT THAT CREATED THEM DID.
//
// The logs' own reservations count against the broker once they exist, so a
// budget read without adding them back shrinks on every restart: the derived
// ceilings a second boot computed were floors, reported against the running
// streams as differences nobody had made.
func TestARestartSizesTheLogsAsTheFirstBootDid(t *testing.T) {
	t.Parallel()
	const free = 64 * gib
	q := brokerWithHeadroom(t, 12*gib)
	first, err := sizeCeilings(t.Context(), q, config.Stream{}, free, "/var/lib/crewlet/stream")
	if err != nil {
		t.Fatalf("sizeCeilings: %v", err)
	}
	s := &stateLog{ceilings: first}
	if _, err := s.provisionAll(t.Context(), q); err != nil {
		t.Fatalf("provision: %v", err)
	}
	again, err := sizeCeilings(t.Context(), q, config.Stream{}, free, "/var/lib/crewlet/stream")
	if err != nil {
		t.Fatalf("sizeCeilings: %v", err)
	}
	if !reflect.DeepEqual(first, again) {
		t.Errorf("the first boot sized %+v and a restart %+v", first, again)
	}
}

// A BOOT THAT STILL CANNOT RESERVE ITS LOGS SAYS WHAT IT NEEDED, WHAT IT HAD
// AND WHAT TO CHANGE.
//
// The broker's refusal names none of the three, and the operator who got it
// went looking at a disk that had room for two of the three logs.
//
// And WHAT TO CHANGE is only what this node can reach. Shrinking a log that
// already exists is `crewlet retention set-capacity`, which needs a node whose
// state logs are up, and every mode starts them: the refusal once offered it,
// sending the operator to a verb that fails the same way.
func TestARefusedReservationNamesWhatItNeededAndHad(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		headroom int64
		stream   config.Stream
		needed   int64
		says     []string
		never    []string
	}{
		// Every derived ceiling is at its floor and three floors do not
		// fit: the tracker and vector logs take two gibibytes, and the
		// pages log's one is refused with half a gibibyte left. A ceiling
		// at the floor goes no lower, so room is the only remedy.
		"a derived ceiling at its floor": {
			headroom: 5 * gib / 2,
			needed:   gib,
			says: []string{
				"stream.pages_log_max_bytes is unset",
				"no lower than 1073741824 bytes",
				"Give the broker more room;",
			},
			never: []string{"to a smaller ceiling"},
		},
		// An explicit ceiling is never scaled, so the operator is told
		// what would have fitted, and that a smaller one is theirs to set.
		"an explicit ceiling": {
			headroom: 7 * gib / 2,
			stream:   config.Stream{PagesLogMaxBytes: 2 * gib},
			needed:   2 * gib,
			says: []string{
				"stream.pages_log_max_bytes sets the ceiling",
				"at most 1610612736 bytes fits",
				"Give the broker more room, or set stream.pages_log_max_bytes " +
					"to a smaller ceiling;",
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			q := brokerWithHeadroom(t, tc.headroom)
			err := sizeAndProvision(t, q, tc.stream, 8*gib)
			if !errors.Is(err, jetstream.ErrInsufficientStorage) {
				t.Fatalf("provision = %v, want the broker's refusal", err)
			}
			budget, budgetErr := q.StreamBudget(t.Context())
			if budgetErr != nil {
				t.Fatalf("StreamBudget: %v", budgetErr)
			}
			for _, want := range append([]string{
				"CREWLET_PAGES_LOG needed " + itoa(tc.needed) + " bytes",
				"the broker had " + itoa(budget.Available()) + " bytes left",
				"stream.store_dir (/var/lib/crewlet/stream)",
				"the state logs that already exist keep the ceilings they were created with",
			}, tc.says...) {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q:\n%v", want, err)
				}
			}
			for _, unreachable := range append([]string{"set-capacity"}, tc.never...) {
				if strings.Contains(err.Error(), unreachable) {
					t.Errorf("the refusal offers %q, which this node cannot do:\n%v",
						unreachable, err)
				}
			}
		})
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// EVERY BUDGET SOURCE SAYS WHO SETS THE LIMIT, IN THE TERMS THAT CHANGE IT.
//
// The sentence is what an operator acts on after a refused boot, and the source
// is the only thing that decides it. A source this build does not know falls
// through to a sentence naming the value itself, which is honest and useless —
// so the assertion is over the CLOSED SET rather than over the cases somebody
// remembered, and a source added to the queue package fails here rather than
// on a node whose boot just failed.
func TestEveryBudgetSourceNamesTheLeverThatChangesIt(t *testing.T) {
	t.Parallel()
	// The lever each source's sentence has to reach, and the volume for
	// the one whose answer depends on it.
	const volume = "/var/lib/crewlet/stream"
	levers := map[jetstream.BudgetSource]string{
		jetstream.BudgetServerStore:        "stream.store_max_bytes",
		jetstream.BudgetServerMemory:       "stream.store_dir",
		jetstream.BudgetAccount:            "whoever operates the broker",
		jetstream.BudgetAccountNoTier:      "stream.replicas",
		jetstream.BudgetAccountTierNoLimit: "declare storage on that tier",
		// UNSTATED HAS NO SETTING TO NAME — the account declares no
		// limit and a client cannot read the server's — so what it owes
		// a reader is who to ask.
		jetstream.BudgetUnstated: "whoever operates that cluster",
	}
	for _, source := range jetstream.BudgetSources {
		t.Run(string(source), func(t *testing.T) {
			t.Parallel()
			said := limitSource(source, volume)
			if strings.Contains(said, "which this build does not know") {
				t.Fatalf("%q reaches an operator as an unknown source, so a "+
					"refused boot names no lever at all: %s", source, said)
			}
			lever, named := levers[source]
			if !named {
				t.Fatalf("%q has no lever written down here, so this test "+
					"passes it without checking anything: add the one its "+
					"sentence names", source)
			}
			if !strings.Contains(said, lever) {
				t.Errorf("%q does not name %q, so an operator is sent to the "+
					"wrong setting: %s", source, lever, said)
			}
		})
	}
	// AND THE NO-TIER SENTENCE IS NOT A CAPACITY ONE. Its limit is zero,
	// which is also what an exhausted account reports, and the two send an
	// operator to opposite places: one waits for room, the other edits a
	// field. A reader told to free space on an account that grants this
	// node's class nothing waits for something that will never happen.
	noTier := limitSource(jetstream.BudgetAccountNoTier, volume)
	if !strings.Contains(noTier, "no JetStream default or applicable tiered limit present") {
		t.Errorf("the no-tier sentence does not quote what the broker will "+
			"actually say, so nobody can match the two: %s", noTier)
	}
	// AND IT DOES NOT OFFER THE BYTE COMPARISON'S WORDS, which a missing
	// tier never reaches: the class is not resolved at all, so nothing is
	// ever compared. Naming both would send an operator to look at how
	// full an account is when the account has no entry for them.
	if strings.Contains(noTier, "insufficient storage resources available") {
		t.Errorf("the no-tier sentence offers a refusal a missing tier cannot "+
			"reach: %s", noTier)
	}
	// THE LIMITLESS TIER NAMES BOTH, because its report covers two
	// realities the account cannot be asked to tell apart: a class with no
	// entry in the limit table, and a class declared with no disk. The
	// first is refused before a byte is compared and the second by the
	// comparison, and an operator sent to only one of them is hunting
	// either a declaration that is there or a fullness that is not.
	tierNoLimit := limitSource(jetstream.BudgetAccountTierNoLimit, volume)
	for _, want := range []string{
		"no JetStream default or applicable tiered limit present",
		"insufficient storage resources available",
	} {
		if !strings.Contains(tierNoLimit, want) {
			t.Errorf("the limitless-tier sentence does not quote %q, so one of "+
				"the two refusals it covers reaches a reader unexplained: %s",
				want, tierNoLimit)
		}
	}
	if strings.Contains(limitSource(jetstream.BudgetAccount, volume), "stream.replicas") {
		t.Error("an ordinary account limit sends an operator to stream.replicas, " +
			"which changes nothing about how full their account is")
	}
}

// THE ROOM CLAUSE IS THE SAME FOUR NUMBERS WHEREVER IT APPEARS, and its three
// cases are three different facts.
//
// It is the one sentence a refused create and a refused raise share, and they
// were written separately once — which is the shape internal/textcut,
// internal/whsec and internal/jsprovision each record drifting while two doc
// comments asserted the two matched. What a test can hold is that neither of
// the two unreadable cases is ever spelled as a number: a limit that could not
// be read reported as zero, or a broker that states none reported as a broker
// with none, is an operator told there is no room when nobody said so.
func TestTheRoomClauseNeverSpellsAnUnknownAsANumber(t *testing.T) {
	t.Parallel()
	const volume = "/var/lib/crewlet/stream"
	stated := jetstream.StorageBudget{
		Limit: 8 * gib, Committed: 6 * gib, Source: jetstream.BudgetServerStore,
	}
	said := roomLeft(stated, nil, volume)
	for _, want := range []string{
		itoa(2 * gib), // left to reserve
		itoa(6 * gib), // already reserved
		itoa(8 * gib), // the limit
		"stream.store_max_bytes",
	} {
		if !strings.Contains(said, want) {
			t.Errorf("a stated limit does not say %q: %s", want, said)
		}
	}

	// UNREAD IS NOT ZERO. The read failed, so there is no number — and a
	// clause carrying one would be an invention on the one line an
	// operator reads after a refused boot.
	unread := roomLeft(jetstream.StorageBudget{}, errors.New("no responders"), volume)
	if !strings.Contains(unread, "could not be read") {
		t.Errorf("an unreadable budget does not say so: %s", unread)
	}
	if strings.Contains(unread, "0 bytes left") || strings.Contains(unread, "-byte limit") {
		t.Errorf("an unreadable budget is spelled as a number: %s", unread)
	}

	// AND UNSTATED IS NOT UNLIMITED. The account states no limit this
	// client can read; every server behind it still has a cap of its own.
	unstated := roomLeft(jetstream.StorageBudget{Limit: -1,
		Source: jetstream.BudgetUnstated}, nil, volume)
	if !strings.Contains(unstated, "states no limit") {
		t.Errorf("an unstated limit does not say so: %s", unstated)
	}
	if strings.Contains(unstated, "-1") {
		t.Errorf("an unstated limit reaches an operator as the number -1: %s", unstated)
	}
}
