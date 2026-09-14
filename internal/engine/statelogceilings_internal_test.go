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
)

const gib = int64(1) << 30

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

// THE ARITHMETIC FITS THE POOL, and the two ways the obvious version overshot
// it are rows here.
//
// The version this replaced scaled every derived ceiling by pool over a total
// that COUNTED the explicit ones (so the derived logs were handed bytes an
// explicit ceiling had already spent) and raised a floored log to the floor
// AFTER dividing (so the floor was bytes the others were also given).
func TestTheCeilingsFitThePool(t *testing.T) {
	t.Parallel()
	derived := func(bytes int64) domainCeiling { return domainCeiling{Bytes: bytes} }
	explicit := func(bytes int64) domainCeiling { return domainCeiling{Bytes: bytes, Explicit: true} }
	for name, tc := range map[string]struct {
		asked map[string]domainCeiling
		pool  int64
		want  map[string]int64
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
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := fitCeilings(tc.asked, tc.pool)
			for name, want := range tc.want {
				if got[name].Bytes != want {
					t.Errorf("%s = %d, want %d (all: %+v)", name, got[name].Bytes, want, got)
				}
				if got[name].Explicit != tc.asked[name].Explicit {
					t.Errorf("%s's explicit flag changed", name)
				}
			}
		})
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
	ceilings, err := sizeCeilings(t.Context(), q, stream, free)
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
	first, err := sizeCeilings(t.Context(), q, config.Stream{}, free)
	if err != nil {
		t.Fatalf("sizeCeilings: %v", err)
	}
	s := &stateLog{ceilings: first}
	if _, err := s.provisionAll(t.Context(), q); err != nil {
		t.Fatalf("provision: %v", err)
	}
	again, err := sizeCeilings(t.Context(), q, config.Stream{}, free)
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
func TestARefusedReservationNamesWhatItNeededAndHad(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		headroom int64
		stream   config.Stream
		needed   int64
		says     []string
	}{
		// Every derived ceiling is at its floor and three floors do not
		// fit: the tracker and vector logs take two gibibytes, and the
		// pages log's one is refused with half a gibibyte left.
		"a derived ceiling at its floor": {
			headroom: 5 * gib / 2,
			needed:   gib,
			says: []string{
				"stream.pages_log_max_bytes is unset",
				"no lower than 1073741824 bytes",
			},
		},
		// An explicit ceiling is never scaled, so the operator is told
		// what would have fitted.
		"an explicit ceiling": {
			headroom: 7 * gib / 2,
			stream:   config.Stream{PagesLogMaxBytes: 2 * gib},
			needed:   2 * gib,
			says: []string{
				"stream.pages_log_max_bytes sets the ceiling",
				"at most 1610612736 bytes fits",
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
				"crewlet retention set-capacity",
			}, tc.says...) {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q:\n%v", want, err)
				}
			}
		})
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
