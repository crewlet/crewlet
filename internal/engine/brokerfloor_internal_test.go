package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
)

// freshFits reports whether a fresh node of stream boots on a stream volume
// with free bytes free, under an embedded broker whose limit is derived: the
// bucket reserved first, then every state log sized against what it leaves,
// all of it inside three quarters of the volume. The real sizing, not
// [BrokerFloor]'s, so the two are held to each other.
func freshFits(t *testing.T, stream config.Stream, free int64) bool {
	t.Helper()
	limit := free / embeddedShareDen * embeddedShareNum
	bucket := derivedObjectsReservation(limit, stream.DeclaredLogCeilings())
	host := sizingHost{budget: jetstream.StorageBudget{Limit: limit, Committed: bucket,
		Source: jetstream.BudgetServerStore}}
	sized, err := sizeCeilings(t.Context(), host, stream, free, "/var/lib/crewlet/stream")
	if err != nil {
		t.Fatalf("sizeCeilings: %v", err)
	}
	reserved := bucket
	for _, ceiling := range sized {
		reserved += ceiling.Bytes
	}
	return reserved <= limit
}

// A FRESH NODE'S FLOOR IS WHAT THE SIZING ITSELF NEEDS, to the byte.
//
// The floor is quoted by a refused boot, by `crewlet validate` and by the
// deployment guide, and every one of them is wrong the day it disagrees with
// what a boot actually reserves. So the floor of the default document is held
// to the real sizing on both sides of it: a volume with FreeAtDefault free
// boots, and one a mebibyte smaller does not.
func TestAFreshNodesFloorIsWhatItsSizingNeeds(t *testing.T) {
	t.Parallel()
	boot := config.DefaultBootstrap()
	floor := BrokerFloorFor(&boot)
	if floor.Bucket == 0 || floor.BucketSet {
		t.Fatalf("the default document reserves a derived nats bucket, and the "+
			"floor read %+v", floor)
	}
	if got := floor.Limit - floor.Bucket; got < floor.Logs {
		t.Errorf("a limit of %d less its %d-byte bucket leaves %d, short of the "+
			"logs' %d", floor.Limit, floor.Bucket, got, floor.Logs)
	}
	if !freshFits(t, boot.Stream, floor.FreeAtDefault()) {
		t.Errorf("a fresh node does not boot with %d bytes free, the floor's "+
			"own figure: %+v", floor.FreeAtDefault(), floor)
	}
	if freshFits(t, boot.Stream, floor.FreeAtDefault()-(1<<20)) {
		t.Errorf("a fresh node boots with a mebibyte less than %d free, so the "+
			"floor overstates what it needs: %+v", floor.FreeAtDefault(), floor)
	}
}

// STREAM.STORE_MAX_BYTES ACCEPTS NO LIMIT THE DEFAULT DOCUMENT CANNOT BOOT IN.
//
// [config.StoreMaxBytesFloor] is the least a Tier A document may write there,
// and internal/config cannot sum the register, so it is a constant — and a
// constant beside a computation is a second statement of it, which is how the
// floor read six gibibytes while every fresh node of the default document
// needed more than eight. So it is held here, in both directions: the smallest
// whole number of gibibytes whose limit holds the default document's logs at
// their floors, the nats bucket that limit derives and
// [objectsCoordinationFloor] beside them, which is what the engine itself
// leaves the streams that reserve nothing. A log that joins the register, a
// bucket derived another way or a floor that moves fails here, naming the
// figure the constant — and its field's js tag, which `make schema` publishes
// as the schema's minimum — must now say.
//
// And held to the real sizing as well as to the arithmetic: a broker that
// states the floor as its limit sizes every log into what its bucket leaves
// and still keeps the coordination floor beside them, and one a gibibyte
// smaller does not.
func TestStoreMaxBytesFloorIsWhatTheDefaultDocumentBootsIn(t *testing.T) {
	t.Parallel()
	boot := config.DefaultBootstrap()
	if declared := boot.Stream.DeclaredLogCeilings(); declared != 0 {
		t.Fatalf("the default document declares %d bytes of log ceilings, so it "+
			"is not the document every ceiling unset describes", declared)
	}
	floor := BrokerFloorFor(&boot)
	needs := leastLimitHolding(floor.Logs+objectsCoordinationFloor, 0)
	want := (needs + gib - 1) / gib * gib
	if config.StoreMaxBytesFloor != want {
		t.Errorf("config.StoreMaxBytesFloor is %d bytes, and the default "+
			"document's logs reserve %d at their floors: beside the bucket a "+
			"limit derives and the %d-byte coordination floor they need a limit "+
			"of %d, so the floor is %d (%d GiB). Move the constant and the js "+
			"tag's min on stream.store_max_bytes, then run `make schema`",
			config.StoreMaxBytesFloor, floor.Logs, objectsCoordinationFloor,
			needs, want, want/gib)
	}

	// What a broker stating limit as its limit has left once its bucket and
	// every log the boot sizes into what that bucket leaves are reserved —
	// on a volume far larger than the limit, so every derived ask is at its
	// clamp and the division scales each down to its floor.
	leftBeside := func(limit int64) int64 {
		bucket := derivedObjectsReservation(limit, 0)
		host := sizingHost{budget: jetstream.StorageBudget{Limit: limit,
			Committed: bucket, Source: jetstream.BudgetServerStore}}
		sized, err := sizeCeilings(t.Context(), host, boot.Stream, 1<<40,
			"/var/lib/crewlet/stream")
		if err != nil {
			t.Fatalf("sizeCeilings: %v", err)
		}
		reserved := bucket
		for _, ceiling := range sized {
			reserved += ceiling.Bytes
		}
		return limit - reserved
	}
	if left := leftBeside(config.StoreMaxBytesFloor); left < objectsCoordinationFloor {
		t.Errorf("a broker limited to the floor, %d bytes, keeps %d beside what a "+
			"fresh node reserves, short of the %d-byte coordination floor",
			config.StoreMaxBytesFloor, left, objectsCoordinationFloor)
	}
	if left := leftBeside(config.StoreMaxBytesFloor - gib); left >= objectsCoordinationFloor {
		t.Errorf("a broker a gibibyte under the floor still keeps %d bytes beside "+
			"what a fresh node reserves, so the floor overstates what the default "+
			"document needs", left)
	}
}

// EVERY SETTING THAT MOVES THE FLOOR MOVES IT AS THE BOOT WOULD.
//
// An explicit log ceiling is reserved as written, never scaled; a bucket the
// operator sized is reserved as written beside the logs; and objects kept in
// S3 reserve no bucket at all.
func TestTheFloorFollowsTheSettingsThatMoveIt(t *testing.T) {
	t.Parallel()
	base := config.DefaultBootstrap()
	floor := BrokerFloorFor(&base)

	explicit := config.DefaultBootstrap()
	explicit.Stream.PagesLogMaxBytes = 3 * gib
	if got := BrokerFloorFor(&explicit).Logs; got != floor.Logs+2*gib {
		t.Errorf("a pages log set at 3 GiB moved the logs' floor from %d to %d, "+
			"want %d", floor.Logs, got, floor.Logs+2*gib)
	}

	sized := config.DefaultBootstrap()
	sized.Store.Objects.NatsMaxBytes = config.NatsObjectsMaxBytesFloor
	got := BrokerFloorFor(&sized)
	if !got.BucketSet || got.Bucket != config.NatsObjectsMaxBytesFloor ||
		got.Limit != got.Logs+config.NatsObjectsMaxBytesFloor {
		t.Errorf("a bucket set at its floor read as %+v", got)
	}
	if !strings.Contains(got.Sentence(), "store.objects.nats_max_bytes sets") {
		t.Errorf("the sentence does not say the bucket is set: %s", got.Sentence())
	}

	s3 := config.DefaultBootstrap()
	s3.Store.Objects.Backend = config.ObjectBackendS3
	if got := BrokerFloorFor(&s3); got.Bucket != 0 || got.Limit != got.Logs {
		t.Errorf("objects in S3 still reserve a bucket: %+v", got)
	}
}

// THE GUIDES QUOTE THE FLOOR THIS BUILD COMPUTES.
//
// The figures there are what an operator sizes a host by, so they are read
// from [BrokerFloorFor] and asserted here rather than restated by hand: the
// two artifact logs raised the floor by two gibibytes, and a guide written
// before them would have sized a host that refuses to boot.
func TestTheGuidesQuoteTheFloor(t *testing.T) {
	t.Parallel()
	read := func(name string) string {
		raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "guides", name))
		if err != nil {
			t.Fatalf("read the %s guide: %v", name, err)
		}
		return string(raw)
	}
	gibs := func(n int64) string { return fmt.Sprintf("%.2f GiB", float64(n)/float64(gib)) }

	boot := config.DefaultBootstrap()
	floor := BrokerFloorFor(&boot)
	small := config.DefaultBootstrap()
	small.Store.Objects.NatsMaxBytes = config.NatsObjectsMaxBytesFloor
	smallest := BrokerFloorFor(&small)
	for _, c := range []struct{ guide, what, figure string }{
		{"deployment.md", "the logs at their floors", gibs(floor.Logs)},
		{"deployment.md", "the derived bucket at the minimum", gibs(floor.Bucket)},
		{"deployment.md", "the broker limit", gibs(floor.Limit)},
		{"deployment.md", "the free space at the default", gibs(floor.FreeAtDefault())},
		{"deployment.md", "the limit with the bucket at its floor", gibs(smallest.Limit)},
		{"deployment.md", "the free space with the bucket at its floor", gibs(smallest.FreeAtDefault())},
		{"deployment.md", "the least stream.store_max_bytes accepts",
			fmt.Sprintf("accepts values from %d GiB", config.StoreMaxBytesFloor/gib)},
		{"../getting-started/configuration.md", "stream.store_max_bytes' bounds",
			fmt.Sprintf("%d GiB..%d TiB", config.StoreMaxBytesFloor/gib,
				config.StoreMaxBytesCeiling>>40)},
		{"replication.md", "the refusal's floor", itoa(floor.Limit)},
		{"replication.md", "the refusal's logs", itoa(floor.Logs)},
		{"replication.md", "the refusal's bucket", itoa(floor.Bucket)},
	} {
		if !strings.Contains(read(c.guide), c.figure) {
			t.Errorf("the %s guide does not quote %s as %s", c.guide, c.what, c.figure)
		}
	}
}

// A BROKER LIMIT WRITTEN BELOW THE FLOOR IS SAID BEFORE THE BOOT REFUSES IT.
//
// stream.store_max_bytes accepts no limit the default document cannot boot in
// ([TestStoreMaxBytesFloorIsWhatTheDefaultDocumentBootsIn]), but a document
// that reserves more than the default does — the bucket sized by hand, a log
// written larger — needs more than that floor, and only the engine can sum
// what it reserves. So a limit validation admits can still be one the boot
// refuses, and that is said as an advisory. Said only where it is exact: a
// data node on its own embedded broker, whose limit is the number written.
// Anywhere else the limit is a volume or an account this judgement cannot
// see, and it says nothing rather than guess.
func TestAStoreLimitBelowTheFloorIsAnAdvisory(t *testing.T) {
	t.Parallel()
	base := config.DefaultBootstrap()
	floor := BrokerFloorFor(&base)
	advised := func(b config.Bootstrap) []config.Warning {
		var out []config.Warning
		for _, w := range BootstrapWarnings(&b) {
			if w.Path == "stream.store_max_bytes" {
				out = append(out, w)
			}
		}
		return out
	}

	// The least limit the field accepts, half of it given to the files:
	// validation sums the bucket with the declared ceilings, of which there
	// are none, and admits it, while the logs at their floors do not fit in
	// the other half.
	below := testBootstrap(t)
	below.Stream.StoreDir = t.TempDir()
	below.Stream.StoreMaxBytes = config.StoreMaxBytesFloor
	below.Store.Objects.NatsMaxBytes = config.StoreMaxBytesFloor / 2
	if err := below.Validate(); err != nil {
		t.Fatalf("validation refuses the document, so it is not a value the "+
			"schema takes: %v", err)
	}
	if need := BrokerFloorFor(&below).Limit; need <= below.Stream.StoreMaxBytes {
		t.Fatalf("a %d-byte limit holds this document's %d-byte floor, so the "+
			"case asserts nothing", below.Stream.StoreMaxBytes, need)
	}
	got := advised(below)
	if len(got) != 1 {
		t.Fatalf("a %d-byte limit under a %d-byte floor gave %d advisories, want one",
			below.Stream.StoreMaxBytes, BrokerFloorFor(&below).Limit, len(got))
	}
	for _, want := range []string{
		"may reserve " + itoa(config.StoreMaxBytesFloor) + " bytes",
		"at least " + itoa(BrokerFloorFor(&below).Limit),
		"store.objects.nats_max_bytes sets",
	} {
		if !strings.Contains(got[0].Message, want) {
			t.Errorf("the advisory does not say %q: %s", want, got[0].Message)
		}
	}

	for name, b := range map[string]func() config.Bootstrap{
		"a limit at the floor": func() config.Bootstrap {
			b := config.DefaultBootstrap()
			b.Stream.StoreMaxBytes = floor.Limit
			return b
		},
		"a derived limit": config.DefaultBootstrap,
		"an external broker": func() config.Bootstrap {
			b := config.DefaultBootstrap()
			b.Stream.Type, b.Stream.StoreMaxBytes = config.StreamNATS, 6*gib
			return b
		},
		"a node holding no estate": func() config.Bootstrap {
			b := config.DefaultBootstrap()
			b.Node.Roles, b.Stream.StoreMaxBytes = []string{"seats"}, 6*gib
			return b
		},
	} {
		if got := advised(b()); len(got) != 0 {
			t.Errorf("%s gave an advisory it cannot be sure of: %+v", name, got)
		}
	}
}
