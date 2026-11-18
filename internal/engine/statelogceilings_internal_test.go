package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/atlasdomain"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

const gib = int64(1) << 30

// THE GATE RESERVE AT EVERY LOG'S FLOOR ABSORBS A FLEET'S APPENDS IN FLIGHT,
// AND LEAVES THE LOG MOST OF ITSELF.
//
// The overshoot is a property of the LOG: every peer of the admitting node may
// hold one of its largest appends in flight unseen, so the reserve must hold
// that plus a gate room at every ceiling the log can reach, down to the floor
// Tier A holds its field to. The walk is over the REGISTER, each log at its
// field's floor as Tier A's validation finds it, so a new log is held to the
// rule unnamed. The other side keeps this from a tautology: the reserve is
// kept FROM ordinary writes, which must keep at least half the log at its
// floor, so a log whose largest record is oversized fails.
func TestTheGateReserveAtEveryFloorAbsorbsItsFleet(t *testing.T) {
	t.Parallel()
	keys := streamKeys(t)
	reserved := 0
	for _, domain := range registeredDomains() {
		if !statelog.ReservationOf(domain).Kept {
			continue
		}
		reserved++
		t.Run(domain.Name(), func(t *testing.T) {
			t.Parallel()
			ceiling, err := tierACeiling(config.Stream{}, domain, 0)
			if err != nil {
				t.Fatalf("tierACeiling: %v", err)
			}
			floor := tierAFloor(t, keys, ceiling.Field, ceiling.Bytes)
			if floor != ceiling.Floor {
				t.Errorf("the register holds %s to a floor of %d and Tier A's "+
					"validation to %d — the capacity verb and a refused boot "+
					"would offer an operator a ceiling Tier A does not take",
					ceiling.Field, ceiling.Floor, floor)
			}
			spec := domain.Stream()
			reservation := statelog.ReservationOf(domain)
			fleet := uint64(statelog.GateReserveFleet-1)*uint64(spec.MaxAppendBytes()) +
				statelog.GateRoom
			if got := reservation.Bytes(uint64(floor)); got < fleet {
				t.Errorf("at %s's floor of %d bytes the gate reserve is %d: %d "+
					"peers' appends in flight at %d bytes each, plus %d for the "+
					"gate records, need %d — an eviction on a log full to its "+
					"ordinary ceiling can find the reserve spent", ceiling.Field,
					floor, got, statelog.GateReserveFleet-1, spec.MaxAppendBytes(),
					statelog.GateRoom, fleet)
			}
			if ordinary := reservation.Ordinary(uint64(floor)); ordinary < uint64(floor)/2 {
				t.Errorf("at %s's floor of %d bytes ordinary writes keep %d — "+
					"under half the log, because its declared largest record "+
					"(%d bytes) sizes a reserve the floor cannot afford",
					ceiling.Field, floor, ordinary, spec.MaxRecordBytes)
			}
		})
	}
	if reserved == 0 {
		t.Fatal("no registered domain keeps a gate reserve, so this walked nothing")
	}
}

// THE RETENTION GUIDE STATES EVERY LOG'S GATE RESERVE AS THE LOG KEEPS IT.
//
// docs/guides/retention.md's table is what an operator sizes a small log by.
// Each row is held to [statelog.ReservationOf] at the floor Tier A's
// validation holds the log's field to (largest record, floor, reserve, what
// ordinary writes keep), and every log that keeps a reserve has exactly one
// row and nothing else has one, so a log registered later fails here until
// the guide names it.
func TestTheRetentionGuideStatesEveryLogsGateReserve(t *testing.T) {
	t.Parallel()
	guide, err := os.ReadFile(filepath.Join(sourcetree.Root(t), "docs", "guides", "retention.md"))
	if err != nil {
		t.Fatalf("read the retention guide: %v", err)
	}
	rows := gateReserveRows(t, string(guide))
	labels := map[string]string{
		worklet.Domain{}.Name():         "tracker",
		doclet.Domain{}.Name():          "knowledge base",
		gitlet.Domain{}.Name():          "Gitlet",
		artifactsdomain.Domain{}.Name(): "artifacts",
		artifactdata.Domain{}.Name():    "artifact datastore",
		iamdomain.Domain{}.Name():       "identity estate",
		assistantdomain.Domain{}.Name(): "Ask Crewlet",
		chatlet.Domain{}.Name():         "chat",
		executordomain.Domain{}.Name():  "executor",
		accessdomain.Domain{}.Name():    "access",
		atlasdomain.Domain{}.Name():     "atlas",
	}
	keys := streamKeys(t)
	held := map[string]bool{}
	for _, domain := range registeredDomains() {
		reservation := statelog.ReservationOf(domain)
		label, named := labels[domain.Name()]
		row, listed := rows[label]
		if !reservation.Kept {
			if listed {
				t.Errorf("the guide lists %q, and %s keeps no gate reserve", label, domain.Name())
			}
			continue
		}
		if !named {
			t.Errorf("%s keeps a gate reserve and this test knows no row of the guide's "+
				"table for it — add its label here and its row to the guide", domain.Name())
			continue
		}
		if !listed {
			t.Errorf("%s keeps a gate reserve and the guide's table has no %q row",
				domain.Name(), label)
			continue
		}
		held[label] = true
		ceiling, err := tierACeiling(config.Stream{}, domain, 0)
		if err != nil {
			t.Fatalf("tierACeiling: %v", err)
		}
		floor := tierAFloor(t, keys, ceiling.Field, ceiling.Bytes)
		reserve := int64(reservation.Bytes(uint64(floor)))
		shape := "the fleet term"
		if reserve == floor/statelog.GateReserveDivisor {
			shape = "a sixteenth"
		}
		want := []string{
			sizeText(domain.Stream().MaxRecordBytes),
			sizeText(floor),
			fmt.Sprintf("%s (%s)", sizeText(reserve), shape),
			sizeText(floor - reserve),
		}
		if !slices.Equal(row, want) {
			t.Errorf("the guide's %q row says %q; %s keeps %q at its %s floor",
				label, row, domain.Name(), want, ceiling.Field)
		}
	}
	for label := range rows {
		if !held[label] {
			t.Errorf("the guide's table lists %q, which names no log that keeps a gate reserve", label)
		}
	}
}

// gateReserveRows reads the retention guide's table of the gate reserve at
// each log's floor: every log a row names, against the row's other four cells.
func gateReserveRows(t *testing.T, guide string) map[string][]string {
	t.Helper()
	const header = "| log | largest record | floor | reserve at the floor | ordinary writes at the floor |"
	_, table, found := strings.Cut(guide, header+"\n")
	if !found {
		t.Fatalf("the retention guide has no table headed %q", header)
	}
	rows := map[string][]string{}
	for _, line := range strings.Split(table, "\n") {
		if !strings.HasPrefix(line, "|") {
			break
		}
		if strings.HasPrefix(line, "|---") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		if len(cells) != 5 {
			t.Fatalf("a gate reserve row has %d cells, want 5: %q", len(cells), line)
		}
		for _, label := range strings.Split(cells[0], ", ") {
			if _, twice := rows[label]; twice {
				t.Errorf("the guide's table lists %q twice", label)
			}
			rows[label] = cells[1:]
		}
	}
	if len(rows) == 0 {
		t.Fatal("the retention guide's gate reserve table has no rows")
	}
	return rows
}

// sizeText spells a byte count the way the retention guide's tables do: whole
// gibibytes, kibibytes under a mebibyte, and otherwise the nearest mebibyte
// with what it is short of or past.
func sizeText(b int64) string {
	const kib, mib = int64(1) << 10, int64(1) << 20
	switch {
	case b >= gib && b%gib == 0:
		return fmt.Sprintf("%d GiB", b/gib)
	case b < mib:
		return fmt.Sprintf("%d KiB", b/kib)
	}
	m := (b + mib/2) / mib
	switch d := b - m*mib; {
	case d < 0:
		return fmt.Sprintf("%d MiB less %d KiB", m, -d/kib)
	case d > 0:
		return fmt.Sprintf("%d MiB plus %d KiB", m, d/kib)
	}
	return fmt.Sprintf("%d MiB", m)
}

// A LOG'S DECLARED CEILING IS TIER A'S DEFAULT WHERE THE DEFAULT IS FIXED.
//
// The framework's own suites provision a domain's stream at its declared
// ceiling, so they certify the log a node runs only if the two agree. A
// ceiling that follows the volume has no one number to agree with, and is
// left to its own derivation.
func TestADomainsDeclaredCeilingIsItsFixedTierADefault(t *testing.T) {
	t.Parallel()
	fixed := 0
	for _, domain := range registeredDomains() {
		ceiling, err := tierACeiling(config.Stream{}, domain, 200*gib)
		if err != nil {
			t.Fatalf("tierACeiling: %v", err)
		}
		if ceiling.FollowsVolume {
			continue
		}
		fixed++
		if declared := domain.Stream().MaxBytes; declared != ceiling.Bytes {
			t.Errorf("%s declares a %d-byte ceiling and an unset %s creates its "+
				"stream at %d — its suites certify a log no node runs",
				domain.Name(), declared, ceiling.Field, ceiling.Bytes)
		}
	}
	if fixed == 0 {
		t.Fatal("no registered domain has a fixed Tier A default, so this walked nothing")
	}
}

// EVERY REGISTERED DOMAIN IS SIZED FROM TIER A, and the field a refusal names
// is the field that actually sets the value.
//
// Asserted over the register rather than a list, so a domain added without a
// Tier A field fails here rather than on somebody's small disk. Each field is
// SET through its own YAML key and read back, because a refusal naming the
// wrong key sends an operator to edit a number that changes nothing.
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
			if derived.Explicit {
				t.Errorf("unset = %+v, want a DERIVED ceiling", derived)
			}
			// A NODE NEVER DERIVES A CEILING ITS OWN VALIDATION WOULD
			// REFUSE TO BE TOLD. Each field's floor is Tier A's to state
			// and they differ (the identity estate's and the usage log's
			// sit far below the corpus logs' gibibyte), so the derived
			// value round-trips THROUGH Tier A's validation rather than a
			// constant here, and a new domain inherits the rule.
			boot := testBootstrap(t)
			boot.Stream.StoreDir = t.TempDir()
			reflect.ValueOf(&boot.Stream).Elem().
				FieldByIndex(keys[strings.TrimPrefix(derived.Field, "stream.")]).
				SetInt(derived.Bytes)
			if err := boot.Validate(); err != nil {
				t.Errorf("%s derives %d, which Tier A then refuses: %v.\n"+
					"A node that derives a ceiling its own validation will not "+
					"accept cannot be configured to the value it chose for "+
					"itself.", derived.Field, derived.Bytes, err)
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

// THE ARITHMETIC FITS THE POOL, and every way it has overshot is a row here.
//
// Each row is held to three INVARIANTS as well as its numbers ("inside the
// pool" is not one: logs already holding more than the pool hold it anyway):
//
//   - What the logs reserve (existing holdings plus the missing logs' creates)
//     exceeds the larger of the pool and the existing holdings only by missing
//     logs created at their floor or at a ceiling somebody set.
//   - A derived log created above its floor proves there was room to spare,
//     so it is never found beside a total past the pool.
//   - A missing log is never created above its fit on an empty broker, the
//     value every later boot reports its stream against as a difference.
func TestTheCeilingsFitThePool(t *testing.T) {
	t.Parallel()
	derived := func(bytes int64) domainCeiling { return domainCeiling{Bytes: bytes} }
	explicit := func(bytes int64) domainCeiling { return domainCeiling{Bytes: bytes, Explicit: true} }
	// A log that declares where its scaling stops, as the assistant's does.
	scales := func(bytes, to int64) domainCeiling { return domainCeiling{Bytes: bytes, ScalesTo: to} }
	for name, tc := range map[string]struct {
		asked map[string]domainCeiling
		held  map[string]int64
		pool  int64
		want  map[string]int64
		// short is what [shortOfFit] reports: every log created below
		// its fit on an empty broker, with that fit.
		short map[string]int64
	}{
		"a log that declares its own scaling floor gives way with the rest": {
			asked: map[string]domainCeiling{"a": derived(7 * 512 << 20), "b": scales(512<<20, 64<<20)},
			pool:  2 * gib,
			want:  map[string]int64{"a": 7 * 256 << 20, "b": 256 << 20},
		},
		"and is held at that floor, not at a gibibyte, when its share is smaller": {
			asked: map[string]domainCeiling{"a": derived(7 * 512 << 20), "b": scales(512<<20, 64<<20)},
			pool:  gib,
			want:  map[string]int64{"a": gib, "b": 64 << 20},
		},
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
		// 4), and the missing ones divide the 6 its 10 leave, not the 12
		// its fit would.
		"a log that exists counts at what it holds, not at what it asks": {
			asked: map[string]domainCeiling{
				"a": derived(8 * gib), "b": derived(16 * gib), "c": derived(8 * gib),
			},
			held:  map[string]int64{"a": 10 * gib},
			pool:  16 * gib,
			want:  map[string]int64{"a": 4 * gib, "b": 4 * gib, "c": 2 * gib},
			short: map[string]int64{"b": 8 * gib, "c": 4 * gib},
		},
		// A measured boot: a tracker log created at an explicit 16 GiB
		// and later unset, with 398676992 bytes left on the broker. The
		// missing logs get their floors, and the vector changelog is
		// reported short of its fit.
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
		// A log holding less than its fit leaves room the missing log is
		// NOT given: the 6 a's 2 leave would create b above its fit of 4,
		// a capacity difference on every later boot.
		"a log holding less than its fit leaves a missing one at its fit": {
			asked: map[string]domainCeiling{"a": derived(8 * gib), "b": derived(8 * gib)},
			held:  map[string]int64{"a": 2 * gib},
			pool:  8 * gib,
			want:  map[string]int64{"a": 4 * gib, "b": 4 * gib},
		},
		// A first boot that stopped after creating the tracker's stream at
		// its fit: the vector changelog's share of the 8947848534 the
		// tracker leaves floors to one byte over its empty-broker fit of
		// 7158278826, so it must be created at the fit.
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

// sizingHost is a broker that answers only the two questions sizing asks: what
// it can grant, and what each log's stream holds. The rest of [domainHost] is
// the NIL interface it embeds, so a sizing that provisions or opens a log or
// a consumer panics rather than succeeding against a broker nobody modelled.
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

// THE VECTOR CHANGELOG A FIRST BOOT CREATES HOLDS A MODEL CHANGE WITHOUT
// ALARMING, from the smallest volume the shared budget gives it that room on.
//
// A model change republishes every source, so the peak is the whole corpus
// (vectorYearFivePeak) whatever the mutation log is set to, and it must leave
// [statelog.HeadroomAlarmFraction] of the ceiling free. The volumes are the
// smallest measured to clear the alarm with every log registered at
// [StreamBudgetShare]: 76 GiB under a broker that states its limit, 100 GiB on
// the default document's boot, whose nats bucket reserves a quarter first
// ([derivedObjectsReservation]). Both need the chat log's quarter-share cap
// ([config.DerivedChatLogDivisor]): set the divisor to 3, or drop the cap,
// and the derived case fails at both.
func TestTheVectorChangelogIsCreatedToHoldAModelChange(t *testing.T) {
	t.Parallel()
	for _, boot := range []struct {
		name   string
		free   int64
		bucket bool
	}{
		{"a broker that states its limit and holds no bucket", 76 * gib, false},
		{"the default document, its bucket reserved first", 100 * gib, true},
	} {
		limit := boot.free / 4 * 3
		var bucket int64
		if boot.bucket {
			bucket = derivedObjectsReservation(limit, 0)
		}
		host := sizingHost{budget: jetstream.StorageBudget{Limit: limit,
			Committed: bucket, Source: jetstream.BudgetServerStore}}
		for name, stream := range map[string]config.Stream{
			"every ceiling derived":                  {},
			"a 4 GiB mutation log an operator wrote": {TrackerLogMaxBytes: 4 * gib},
		} {
			name := boot.name + ", " + name
			sized, err := sizeCeilings(t.Context(), host, stream, boot.free,
				"/var/lib/crewlet/stream")
			if err != nil {
				t.Fatalf("%s: sizeCeilings: %v", name, err)
			}
			got := sized[search.Domain{}.Name()].Bytes
			if got < vectorYearFivePeak {
				t.Errorf("%s: the vector changelog is created at %d bytes on %d free, "+
					"under the reference company's fifth-year model change (%d)",
					name, got, boot.free, vectorYearFivePeak)
			}
			if left := 1 - float64(vectorYearFivePeak)/float64(got); left < statelog.HeadroomAlarmFraction {
				t.Errorf("%s: the vector changelog is created at %d bytes on %d free, so "+
					"the fifth-year model change leaves %.1f %% of it free and raises "+
					"log_headroom, which fires under %.0f %%", name, got, boot.free,
					100*left, 100*statelog.HeadroomAlarmFraction)
			}
		}
	}
}

// THE LOGS' SHARE LEAVES THE ESTATE AT LEAST WHAT THE BUCKET RESERVES, the
// other bound on [StreamBudgetShare]. The mailboxes, coordination buckets and
// a joining node's snapshot reserve nothing and grow in what the logs and the
// `nats` object bucket leave, so that remainder may not be smaller than the
// bucket: sized as the boot sizes them ([logsGrantable]), on every broker
// from 8 GiB to a petabyte.
func TestTheLogsShareLeavesTheEstateTheBucketsRoom(t *testing.T) {
	t.Parallel()
	for limit := 8 * gib; limit <= 1<<50; limit *= 2 {
		bucket := derivedObjectsReservation(limit, 0)
		budget := jetstream.StorageBudget{Limit: limit, Committed: bucket,
			Source: jetstream.BudgetServerStore}
		logs := int64(float64(logsGrantable(budget, nil, limit, 0)) * StreamBudgetShare)
		if left := limit - bucket - logs; left < bucket {
			t.Errorf("on a %d-byte broker the bucket reserves %d and the logs' share %d, "+
				"leaving the mailboxes and coordination %d, less than the bucket: a "+
				"share of %v of what the bucket leaves is past the two thirds that "+
				"keeps them its size", limit, bucket, logs, left, StreamBudgetShare)
		}
	}
}

// A LOG CREATED BESIDE ONES THAT EXIST FITS THE SHARE, on every reading of the
// broker's budget: the missing logs are sized from what the existing ones
// hold, not their ask, and where the pool is taken from free space the
// existing ceilings are not added to it, since free space never paid for a
// reservation.
func TestALogCreatedBesideOnesThatExistFitsTheShare(t *testing.T) {
	t.Parallel()
	// The tracker asks a quarter of the free space (11 GiB at 44, 15 at 60)
	// and its stream holds 16, more than its ask (where the two are equal
	// neither mistake shows), so counting it at its ask, or adding what it
	// holds to the free-space figure, hands the others more than the share
	// leaves. What it leaves must still hold every other log at its
	// floor (7.625 GiB), or no sizing could keep to the share: the case
	// measures the counting, not the floors. Hence 60 GiB for the rows that
	// read no limit: its stand-in ([unstatedLimit]) grants 45 and leaves
	// 12.125, where 44 GiB's would leave 4.6.
	const trackerHolds = 16 * gib
	for name, tc := range map[string]struct {
		budget jetstream.StorageBudget
		unread error
		free   int64
		// grantable is what the broker could grant the logs if they held
		// nothing, worked out here rather than read back from the code.
		grantable int64
	}{
		// 48 GiB, of which 4 GiB is reserved by other streams and 16 GiB
		// by the tracker's.
		"a broker that states its limit": {
			budget: jetstream.StorageBudget{Limit: 48 * gib, Committed: 20 * gib,
				Source: jetstream.BudgetServerStore},
			free:      44 * gib,
			grantable: 44 * gib,
		},
		"a broker that states no limit": {
			budget:    jetstream.StorageBudget{Limit: -1, Source: jetstream.BudgetUnstated},
			free:      60 * gib,
			grantable: 45 * gib,
		},
		"a budget that could not be read": {
			unread:    errors.New("no responders"),
			free:      60 * gib,
			grantable: 45 * gib,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			host := sizingHost{budget: tc.budget, unread: tc.unread, held: map[string]int64{
				worklet.Domain{}.Stream().Name: trackerHolds,
			}}
			sized, err := sizeCeilings(t.Context(), host, config.Stream{}, tc.free, "/var/lib/crewlet/stream")
			if err != nil {
				t.Fatalf("sizeCeilings: %v", err)
			}
			reserved := trackerHolds
			for _, domain := range registeredDomains() {
				if domain.Name() != (worklet.Domain{}).Name() {
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
// Free space is the figure a node falls back to where the broker states no
// limit or cannot be read, and no reservation spends it, so unlike under a
// stated limit ([TestARestartSizesTheLogsAsTheFirstBootDid]) the logs'
// reservations are not added back: that grows the pool on every restart.
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
		// THE BUCKET'S RESERVATION COMES OFF BOTH BOOTS ALIKE, and the
		// logs' own come off neither: the broker reports both as granted,
		// and only the first is another stream's.
		"a broker that states no limit beside another stream's reservation": {
			budget: jetstream.StorageBudget{Limit: -1, Committed: gib,
				Source: jetstream.BudgetUnstated},
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
				host.budget.Committed += first[domain.Name()].Bytes
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

// AN OPERATOR'S SERVER THAT STATES NO LIMIT HOLDS THE BUCKET AND EVERY LOG.
//
// Such a server caps itself at three quarters of its volume's free space,
// which this node cannot read, so it stands that share of its own free space
// in for the limit ([unstatedLimit]). Against that cap the bucket, every log
// and the coordination remainder fit on every volume that holds the floors
// with the remainder beside them.
//
// THE SMALLEST ROW IS THAT VOLUME, computed from the register so it moves with
// the floors. It is not BrokerFloor's figure, which counts no remainder: the
// least that boots is not the least that runs (the deployment guide).
// Mutation: stand the whole free space in for the limit and it fails.
func TestAnUnstatedServerHoldsTheBucketAndEveryLog(t *testing.T) {
	t.Parallel()
	boot := config.DefaultBootstrap()
	floors := BrokerFloorFor(&boot).Logs
	least := BrokerFloor{Limit: leastLimitHolding(floors+objectsCoordinationFloor, 0)}.FreeAtDefault()
	for _, free := range []int64{least, 20 * gib, 24 * gib, 86104199168, 1 << 40} {
		t.Run(strconv.FormatInt(free, 10), func(t *testing.T) {
			t.Parallel()
			bucket := derivedObjectsReservation(unstatedLimit(free), 0)
			host := sizingHost{budget: jetstream.StorageBudget{Limit: -1, Committed: bucket,
				Source: jetstream.BudgetUnstated}}
			sized, err := sizeCeilings(t.Context(), host, config.Stream{}, free, "/var/lib/crewlet/stream")
			if err != nil {
				t.Fatalf("sizeCeilings: %v", err)
			}
			reserved := bucket
			for _, domain := range registeredDomains() {
				ceiling := sized[domain.Name()].Bytes
				if ceiling <= 0 {
					t.Fatalf("%s's log was sized at %d bytes", domain.Name(), ceiling)
				}
				reserved += ceiling
			}
			for _, name := range []string{(artifactsdomain.Domain{}).Name(), (artifactdata.Domain{}).Name()} {
				if _, found := sized[name]; !found {
					t.Errorf("the %s log was not sized beside the bucket", name)
				}
			}
			serverCap := free / 4 * 3
			if reserved+objectsCoordinationFloor > serverCap {
				t.Errorf("the bucket (%d) and the state logs reserve %d bytes between "+
					"them, which leaves no %d-byte coordination remainder in a %d-byte "+
					"server cap: %+v", bucket, reserved, objectsCoordinationFloor,
					serverCap, sized)
			}
		})
	}
}

// A FIRST BOOT THAT STOPPED BETWEEN TWO CREATES LEAVES LOGS A RESTART REPORTS
// AS THEY WERE MADE.
//
// A kill, timeout or refused create between two creates leaves some streams
// existing, and the restart sizes the missing ones from what the existing ones
// leave of the share; on this budget the vector changelog's part floors to
// one byte past its fit. So three sizings run as the broker sees them: the
// first boot, a restart after only the tracker's create, and a restart with
// every stream, each of which must be reported at exactly what it holds.
func TestAnInterruptedFirstBootLeavesLogsARestartReportsAsMade(t *testing.T) {
	t.Parallel()
	// 64 GiB free under a 30 GiB limit: the tracker and the vector
	// changelog each ask for 16 GiB.
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
	trackerName := worklet.Domain{}.Name()
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
// ITS CAP IS DECLARED ([brokertest.StoreMaxBytes]), not three quarters of the
// runner's free disk: nats-server never checks a declared cap against the
// disk, so the arithmetic is the same on every machine. AND THE REST OF IT RESERVED, rather than the cap set to the
// headroom, or a sizing that divided the cap instead of what is left would
// pass every case. A scenario needing more than the cap leaves fails naming
// both figures.
func brokerWithHeadroom(t *testing.T, headroom int64) *jetstream.Queue {
	t.Helper()
	q, err := jetstream.Open(t.Context(), jetstream.Config{
		StoreDir: brokertest.TempDir(t), StoreMaxBytes: brokertest.StoreMaxBytes,
	})
	if err != nil {
		t.Fatalf("open the broker: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
	budget, err := q.StreamBudget(t.Context())
	if err != nil {
		t.Fatalf("StreamBudget: %v", err)
	}
	if budget.Available() < headroom+gib {
		t.Fatalf("the test broker's %d-byte cap leaves %d bytes to reserve, and "+
			"the scenario needs %d of headroom plus a gibibyte to spend around "+
			"it: shrink the scenario, or raise brokertest.StoreMaxBytes",
			brokertest.StoreMaxBytes, budget.Available(), headroom)
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
	s := &stateLog{ceilings: ceilings, volume: "/var/lib/crewlet/stream",
		floor: BrokerFloorFor(&config.Bootstrap{Stream: stream})}
	_, err = s.provisionAll(t.Context(), q)
	return err
}

// THE STATE LOGS FIT THE BROKER THEY BOOT ON: every log is sized inside one
// budget, the fixed defaults beside the ones scaled to their floors.
//
// THE HOST IS THE SMALLEST THE LOGS' FLOORS FIT, the figure the deployment
// guide states: 11.67 GiB, whose broker grants three quarters of it. A log
// added later moves this figure, and the guide's, with it.
func TestTheStateLogsFitTheBrokerTheyBootOn(t *testing.T) {
	t.Parallel()
	// 11.67 GiB, in hundredths so the arithmetic stays in integers: the
	// first hundredth of a gibibyte whose three quarters hold the logs.
	free := 1167 * gib / 100
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

// A RESTART SIZES THE LOGS AS THE BOOT THAT CREATED THEM DID. Once they exist
// their reservations count against the broker, so a budget read without
// adding them back shrinks every restart and reports differences nobody made.
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
// AND WHAT TO CHANGE, none of which the broker's refusal names.
//
// WHAT TO CHANGE is only what this node can reach: `crewlet retention
// set-capacity` needs a node whose state logs are up, so it is never offered.
// WHERE THE CEILING CAME FROM is one of three facts: derived, set, or the
// log's own fixed default (the identity log's, below the gibibyte scaling
// stops at), which is offered the smaller ceiling its field accepts. The
// identity log is provisioned last, so on a tight broker it is the likeliest
// refusal.
func TestARefusedReservationNamesWhatItNeededAndHad(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		headroom int64
		stream   config.Stream
		log      string
		needed   int64
		says     []string
		never    []string
	}{
		// Three derived ceilings at their floors do not fit: the pages
		// log's gibibyte is refused with half a gibibyte left. A ceiling
		// at its floor goes no lower, so room is the only remedy.
		"a derived ceiling at its floor": {
			headroom: 5 * gib / 2,
			log:      "CREWLET_PAGES_LOG",
			needed:   gib,
			says: []string{
				"stream.pages_log_max_bytes is unset",
				"derived from the stream volume's free space",
				"no lower than 1073741824 bytes",
				"Give the broker more room;",
				// HOW MUCH ROOM: the least a fresh node reserves, and
				// the setting that moves the bucket's part of it.
				"a fresh node of this configuration reserves at least " +
					itoa(BrokerFloorFor(&config.Bootstrap{}).Limit) + " bytes",
				"store.objects.nats_max_bytes",
			},
			never: []string{"to a smaller ceiling", "fixed default"},
		},
		// An explicit ceiling is never scaled, so the operator is told
		// what would have fitted, and that a smaller one is theirs to set.
		"an explicit ceiling": {
			headroom: 7 * gib / 2,
			stream:   config.Stream{PagesLogMaxBytes: 2 * gib},
			log:      "CREWLET_PAGES_LOG",
			needed:   2 * gib,
			says: []string{
				"stream.pages_log_max_bytes sets the ceiling",
				"at most 1610612736 bytes fits",
				"Give the broker more room, or set stream.pages_log_max_bytes " +
					"to a smaller ceiling, no lower than its 1073741824-byte floor;",
			},
		},
		// THE IDENTITY LOG'S FIXED DEFAULT, refused with the derived logs
		// at their floors reserved. Its field takes 64 MiB, so a smaller
		// ceiling is offered, and nothing about a derivation is said.
		"the identity log's fixed default": {
			headroom: 3*gib + gib/4,
			log:      "CREWLET_IAM_LOG",
			needed:   config.DefaultIamLogMaxBytes,
			says: []string{
				"stream.iam_log_max_bytes is unset",
				"the log's own fixed default",
				"Give the broker more room, or set stream.iam_log_max_bytes " +
					"to a smaller ceiling, no lower than its 67108864-byte floor;",
			},
			never: []string{"derived", "scaled", "1073741824"},
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
				tc.log + " needed " + itoa(tc.needed) + " bytes",
				"the broker had " + itoa(budget.Available()) + " bytes left",
				"stream.store_dir (/var/lib/crewlet/stream)",
				"the state logs that already exist keep the ceilings they were created with",
			}, tc.says...) {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q:\n%v", want, err)
				}
			}
			if strings.Contains(err.Error(), "set-capacity") {
				t.Errorf("the refusal offers set-capacity, which this node cannot "+
					"do:\n%v", err)
			}
			for _, untrue := range tc.never {
				if strings.Contains(err.Error(), untrue) {
					t.Errorf("the refusal says %q, which is not true of this "+
						"ceiling:\n%v", untrue, err)
				}
			}
		})
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// EVERY BUDGET SOURCE SAYS WHO SETS THE LIMIT, IN THE TERMS THAT CHANGE IT.
//
// The sentence is what an operator acts on after a refused boot. An unknown
// source falls through to a sentence naming the value itself, honest and
// useless, so the walk is over the CLOSED SET [jetstream.BudgetSources] and a
// source added to the queue package fails here.
func TestEveryBudgetSourceNamesTheLeverThatChangesIt(t *testing.T) {
	t.Parallel()
	// The lever each source's sentence has to reach, and the volume the
	// server-store one names.
	const volume = "/var/lib/crewlet/stream"
	levers := map[jetstream.BudgetSource]string{
		jetstream.BudgetServerStore:        "stream.store_max_bytes",
		jetstream.BudgetServerMemory:       "stream.store_dir",
		jetstream.BudgetAccount:            "whoever operates the broker",
		jetstream.BudgetAccountNoTier:      "stream.replicas",
		jetstream.BudgetAccountTierNoLimit: "declare storage on that tier",
		// UNSTATED HAS NO SETTING TO NAME (the account declares no limit
		// and a client cannot read the server's), so it names who to ask.
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
	// AND THE NO-TIER SENTENCE IS NOT A CAPACITY ONE: its zero limit is also
	// what an exhausted account reports, but one sends an operator to wait
	// for room and the other to edit a field.
	noTier := limitSource(jetstream.BudgetAccountNoTier, volume)
	if !strings.Contains(noTier, "no JetStream default or applicable tiered limit present") {
		t.Errorf("the no-tier sentence does not quote what the broker will "+
			"actually say, so nobody can match the two: %s", noTier)
	}
	// AND IT DOES NOT QUOTE THE BYTE COMPARISON'S REFUSAL: a missing tier
	// is refused before anything is compared.
	if strings.Contains(noTier, "insufficient storage resources available") {
		t.Errorf("the no-tier sentence offers a refusal a missing tier cannot "+
			"reach: %s", noTier)
	}
	// THE LIMITLESS TIER QUOTES BOTH: its report covers a class with no
	// entry in the limit table (refused before any comparison) and a class
	// declared with no disk (refused by it), which the account cannot tell
	// apart.
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
// A refused create and a refused raise share it. Neither unknown case is ever
// spelled as a number: a limit that could not be read reported as zero, or an
// unstated one as none, tells an operator there is no room when nobody said so.
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

	// UNREAD IS NOT ZERO: the read failed, so there is no number to give.
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

// A NODE THAT CANNOT MEASURE ITS VOLUME NAMES THE FIELDS THAT VOLUME DECIDES,
// and only those: the warning is the operator's one chance to set the ceilings
// a missing measurement shrinks, so it names exactly the unset ones that
// follow free space, never the identity or usage log's flat asks or a field
// the operator already set.
func TestAnUnmeasuredVolumeNamesTheCeilingsItDecides(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		stream config.Stream
		want   []string
	}{
		{"nothing set", config.Stream{}, []string{"stream.tracker_log_max_bytes",
			"stream.tracker_vectors_max_bytes", "stream.pages_log_max_bytes",
			"stream.artifacts_log_max_bytes", "stream.artifact_data_log_max_bytes",
			"stream.chat_log_max_bytes", "stream.code_log_max_bytes"}},
		// The knowledge base's, artifacts, datastore and code logs follow
		// the mutation log's ceiling ([config.Stream.PagesMaxBytes],
		// [config.Stream.ArtifactsMaxBytes], [config.Stream.CodeMaxBytes],
		// [config.Stream.ArtifactDataMaxBytes]), so once it is set the
		// volume no longer decides them. The CHAT LOG's cap is a share of
		// the volume whatever the mutation log is ([chatMaxBytes]).
		{"the mutation log set", config.Stream{TrackerLogMaxBytes: 8 << 30},
			[]string{"stream.tracker_vectors_max_bytes", "stream.chat_log_max_bytes"}},
		{"every volume-derived log set", config.Stream{TrackerLogMaxBytes: 8 << 30,
			TrackerVectorsMaxBytes: 8 << 30, PagesLogMaxBytes: 2 << 30,
			ChatLogMaxBytes: 2 << 30}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := diskDerivedFields(c.stream)
			if !slices.Equal(got, c.want) {
				t.Errorf("a volume this node cannot measure names %v, want %v", got, c.want)
			}
			detail := unmeasuredDetail(got)
			for _, field := range got {
				if !strings.Contains(detail, field) {
					t.Errorf("the warning does not name %s: %s", field, detail)
				}
			}
			for _, flat := range []string{"stream.iam_log_max_bytes",
				"stream.usage_log_max_bytes"} {
				if strings.Contains(detail, flat) {
					t.Errorf("the warning names %s, whose ceiling no volume "+
						"decides: %s", flat, detail)
				}
			}
		})
	}
}

// recordingHost is a broker that provisions nothing and remembers every stream
// it was asked to create, so a refusal that must come before the broker is
// asked can be seen to.
type recordingHost struct {
	domainHost
	ensured []string
}

func (h *recordingHost) EnsureDomainStream(_ context.Context, spec jetstream.DomainStream) error {
	h.ensured = append(h.ensured, spec.Name)
	return nil
}

func (*recordingHost) DomainLog(context.Context, string) (*jetstream.DomainLog, error) {
	return nil, nil
}

// A LOG NOBODY SIZED IS NOT CREATED AT THE DOMAIN'S DEFAULT: that would be a
// reservation no Tier A budget counted, refusing a boot on a broker sized for
// the logs it was told about. A domain the sizing left out, or sized at zero,
// refuses by name before its stream is asked for.
func TestALogNobodySizedIsRefusedBeforeItsStreamIsCreated(t *testing.T) {
	t.Parallel()
	unsized := doclet.Domain{}
	for name, ceiling := range map[string]*domainCeiling{
		"left out of the sizing": nil,
		"sized at zero bytes":    {Bytes: 0, Field: "stream.pages_log_max_bytes"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ceilings := map[string]domainCeiling{}
			for _, d := range registeredDomains() {
				ceilings[d.Name()] = domainCeiling{Bytes: gib, Field: "stream." + d.Name()}
			}
			delete(ceilings, unsized.Name())
			if ceiling != nil {
				ceilings[unsized.Name()] = *ceiling
			}
			s := &stateLog{ceilings: ceilings}

			_, _, err := s.specOf(unsized)
			if err == nil || !strings.Contains(err.Error(), "was never sized") ||
				!strings.Contains(err.Error(), unsized.Name()) {
				t.Fatalf("specOf(%s) = %v, want the refusal naming the unsized domain",
					unsized.Name(), err)
			}

			host := &recordingHost{}
			_, err = s.provisionAll(t.Context(), host)
			if err == nil || !strings.Contains(err.Error(), "was never sized") ||
				!strings.Contains(err.Error(), unsized.Name()) {
				t.Fatalf("provisionAll = %v, want the refusal naming %s", err, unsized.Name())
			}
			if slices.Contains(host.ensured, unsized.Stream().Name) {
				t.Errorf("the unsized %s log's stream %s was created anyway (asked for %v)",
					unsized.Name(), unsized.Stream().Name, host.ensured)
			}
		})
	}
}

// AN UNSET CHAT CEILING IS THE DOMAIN'S BUDGET, CAPPED BY THE VOLUME AND
// NEVER BELOW ITS FIELD'S FLOOR.
//
// The chat log asks for its domain's excursion budget
// ([chatlet.ChatLogMaxBytes]), on a small volume no more than a quarter of the
// mutation log's derived share, which keeps the vector changelog's room for a
// model change clear of the headroom alarm ([config.DerivedChatLogDivisor]),
// and never less than `stream.chat_log_max_bytes` accepts. A value an operator
// wrote is theirs. Mutation: drop the cap, the divisor or the budget, or let
// the derivation follow the mutation log's field, and a row names the volume
// it went wrong on.
func TestAnUnsetChatCeilingIsTheDomainsBudgetCappedByTheVolume(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		stream  config.Stream
		free    int64
		want    int64
		derived bool
	}{
		{"an empty volume", config.Stream{}, 0, config.ChatLogMaxBytesFloor, true},
		{"a 64 GiB volume", config.Stream{}, 64 * gib, 4 * gib, true},
		{"the volume the whole budget is asked from", config.Stream{}, 128 * gib,
			int64(chatlet.ChatLogMaxBytes), true},
		{"a terabyte", config.Stream{}, 1 << 40, int64(chatlet.ChatLogMaxBytes), true},
		{"beside a mutation log an operator sized", config.Stream{TrackerLogMaxBytes: 512 * gib},
			64 * gib, 4 * gib, true},
		{"a ceiling an operator wrote", config.Stream{ChatLogMaxBytes: 2 * gib}, 1 << 40,
			2 * gib, false},
	} {
		got, derived := chatMaxBytes(c.stream, c.free)
		if got != c.want || derived != c.derived {
			t.Errorf("%s: the chat log asks for %d (derived %v), want %d (derived %v)",
				c.name, got, derived, c.want, c.derived)
		}
	}
}
