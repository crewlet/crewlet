package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
)

// freshFits reports whether a fresh node of stream boots on a stream volume
// with free bytes free, under an embedded broker whose limit is derived: the
// bucket reserved first, then every state log sized against what it leaves,
// all of it inside three quarters of the volume. The real sizing, not
// [BrokerFloor]'s, so the two are held to each other.
func freshFits(t *testing.T, stream config.Stream, free int64) bool {
	t.Helper()
	host := firstBootBroker(stream, free)
	sized, err := sizeCeilings(t.Context(), host, stream, free, "/var/lib/crewlet/stream")
	if err != nil {
		t.Fatalf("sizeCeilings: %v", err)
	}
	reserved := host.budget.Committed
	for _, ceiling := range sized {
		reserved += ceiling.Bytes
	}
	return reserved <= host.budget.Limit
}

// A FRESH NODE'S FLOOR IS WHAT THE SIZING ITSELF NEEDS, to the byte: a refused
// boot, `crewlet validate` and the deployment guide all quote it, so the default
// document's floor is held to the real sizing on both sides. A volume with
// FreeAtDefault free boots, and one a mebibyte smaller does not.
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

// THE GUIDES QUOTE THE FLOOR THIS BUILD COMPUTES: an operator sizes a host by
// those figures, so they are read from [BrokerFloorFor] and asserted here,
// never restated by hand.
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
		{"replication.md", "the refusal's floor", itoa(floor.Limit)},
		{"replication.md", "the refusal's logs", itoa(floor.Logs)},
		{"replication.md", "the refusal's bucket", itoa(floor.Bucket)},
	} {
		if !strings.Contains(read(c.guide), c.figure) {
			t.Errorf("the %s guide does not quote %s as %s", c.guide, c.what, c.figure)
		}
	}
}

// A BROKER LIMIT WRITTEN BELOW THE FLOOR IS SAID BEFORE THE BOOT REFUSES IT:
// the schema accepts a stream.store_max_bytes the default document's fresh node
// cannot boot in. Said only where it is exact (a data node on its own embedded
// broker); elsewhere the limit is a volume or an account this judgement cannot
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

	below := config.DefaultBootstrap()
	below.Stream.StoreMaxBytes = 6 * gib
	got := advised(below)
	if len(got) != 1 {
		t.Fatalf("a limit of 6 GiB under a %d-byte floor gave %d advisories, want one",
			floor.Limit, len(got))
	}
	for _, want := range []string{
		"may reserve 6442450944 bytes",
		"at least " + itoa(floor.Limit),
		"store.objects.nats_max_bytes",
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

// THE ASSISTANT'S LOG RESERVES ONLY ITS FLOOR ON A FRESH NODE, so a deployment
// sized for everything else boots with it. crewlet-operator states 12 GiB, three
// quarters of its smallest data volume (16Gi), and a fresh node fits it only
// with the assistant's unset 512 MiB scaled to its field's 64 MiB floor: counted
// in full it overshoots by just under the executor log's 85.3 MiB. The scaling
// also decides the floor itself: a broker stated at that floor boots a fresh
// node with the log scaled and refuses one counting it in full. The least a
// fresh node needs grows as the register does, and crewlet-operator's smallest
// volume moves with it.
func TestTheAssistantLogCountsOnlyItsFloorTowardAFreshNode(t *testing.T) {
	t.Parallel()
	boot := config.DefaultBootstrap()
	floor := BrokerFloorFor(&boot)
	var assistant domainCeiling
	for _, entry := range register() {
		if entry.Domain.Name() == "assistant" {
			assistant = entry.Ceiling(boot.Stream, 0)
		}
	}
	if assistant.Bytes != config.DefaultAssistantLogMaxBytes || assistant.scaleFloor() != config.AssistantLogMaxBytesFloor {
		t.Fatalf("the assistant's ceiling asks %d and scales to %d, want its default and its field's floor",
			assistant.Bytes, assistant.scaleFloor())
	}
	// crewlet-operator states three quarters of a company's data volume as
	// stream.store_max_bytes, and admits no volume smaller than 16Gi.
	stated := config.DefaultBootstrap()
	stated.Stream.StoreMaxBytes = (16 << 30) / embeddedShareDen * embeddedShareNum
	if got := BrokerFloorFor(&stated).Limit; got > stated.Stream.StoreMaxBytes {
		t.Errorf("a fresh node reserves at least %d bytes, more than the %d a 16Gi volume states",
			got, stated.Stream.StoreMaxBytes)
	}
	// An explicit ceiling is reserved as written, never scaled.
	explicit := config.DefaultBootstrap()
	explicit.Stream.AssistantLogMaxBytes = config.DefaultAssistantLogMaxBytes
	if grew := BrokerFloorFor(&explicit).Logs - floor.Logs; grew != config.DefaultAssistantLogMaxBytes-config.AssistantLogMaxBytesFloor {
		t.Errorf("an explicit 512 MiB moved the logs' floor by %d, want %d", grew,
			config.DefaultAssistantLogMaxBytes-config.AssistantLogMaxBytesFloor)
	}
	// Nor, beside the executor log, the 16Gi volume: counted in full the
	// assistant's log takes a fresh node past what that volume states, so
	// the scaling is what boots crewlet-operator's smallest volume too.
	inFull := explicit
	inFull.Stream.StoreMaxBytes = stated.Stream.StoreMaxBytes
	if got := BrokerFloorFor(&inFull).Limit; got <= inFull.Stream.StoreMaxBytes {
		t.Errorf("counted at its 512 MiB the assistant's log leaves a fresh node at %d "+
			"bytes, inside the %d a 16Gi volume states, so its scaling no longer decides "+
			"whether that volume boots", got, inFull.Stream.StoreMaxBytes)
	} else {
		t.Logf("counted at its 512 MiB the assistant's log takes a fresh node to %d bytes, "+
			"%d past the %d a 16Gi volume states", got, got-inFull.Stream.StoreMaxBytes,
			inFull.Stream.StoreMaxBytes)
	}
	// And reserved as written it does not fit a broker stated at the floor,
	// so the scaling is what lets that broker boot a fresh node rather than
	// a limit every fresh node clears.
	atFloor := explicit
	atFloor.Stream.StoreMaxBytes = floor.Limit
	if got := BrokerFloorFor(&atFloor).Limit; got <= atFloor.Stream.StoreMaxBytes {
		t.Errorf("counted at its 512 MiB the assistant's log leaves a fresh node at %d "+
			"bytes, inside the %d-byte floor it scales down to, so its scaling no longer "+
			"decides whether a broker at that floor boots", got, atFloor.Stream.StoreMaxBytes)
	}
}

// THE SIZE A TEST BROKER DECLARES BOOTS A FRESH NODE OF THE FIXTURES. Suites
// size brokers from brokertest.StoreMaxBytes, not the runner's disk, so a boot
// passes or fails on the code; once the floor grows past it (a log joins the
// register) every suite's node boot is refused. This case names the cause, both
// figures and the constant to raise, and goes through Tier A's own validation
// so the figure is one an operator could write.
func TestTheTestBrokerSizeHoldsAFreshNode(t *testing.T) {
	t.Parallel()
	boot := testBootstrap(t)
	brokertest.StoreIn(&boot.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
	if err := boot.Validate(); err != nil {
		t.Fatalf("Tier A refuses the fixtures' broker size of %d bytes: %v",
			brokertest.StoreMaxBytes, err)
	}
	floor := BrokerFloorFor(&boot)
	if floor.Limit > brokertest.StoreMaxBytes {
		t.Fatalf("brokertest.StoreMaxBytes is %d bytes and %s: raise it, or every "+
			"suite booting a node is refused at whichever state log no longer fits",
			brokertest.StoreMaxBytes, floor.Sentence())
	}
}

// THE EXECUTOR LOG FITS BESIDE THE OTHER LOGS AT THE OPERATOR'S SMALLEST
// VOLUMES, fresh or with the other logs already there: 12Gi with objects in S3
// and 16Gi on the nats bucket, three quarters stated as the broker's limit. A
// log the register gains is created at an existing node's next boot from what
// the others leave (448 MiB on either), so it scales to its 64 MiB floor
// ([config.ExecutorLogMaxBytesFloor]); the 85.3 MiB it adds to a fresh node's
// floor (the bucket's quarter included) crewlet-operator's own floors follow.
// The others are read off the register, so a later log is held without an edit.
//
// Mutation: drop ScalesTo from the executor's register entry, or raise its
// floor past 448 MiB, and the node whose other logs exist is refused it on
// both volumes.
func TestTheExecutorLogFitsBesideTheOtherLogsAtTheOperatorsSmallestVolumes(t *testing.T) {
	t.Parallel()
	executor := executordomain.Domain{}.Name()
	var others int64
	held := map[string]int64{}
	for _, entry := range register() {
		if entry.Domain.Name() == executor {
			continue
		}
		ceiling := entry.Ceiling(config.Stream{}, 0)
		floor := min(ceiling.Bytes, ceiling.scaleFloor())
		held[entry.Domain.Stream().Name] = floor
		others += floor
	}
	for name, tc := range map[string]struct {
		volume int64
		nats   bool
	}{
		"12Gi with the objects in S3":              {volume: 12 << 30},
		"16Gi with the objects on the nats bucket": {volume: 16 << 30, nats: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			limit := tc.volume / embeddedShareDen * embeddedShareNum
			boot := config.DefaultBootstrap()
			var bucket int64
			if tc.nats {
				bucket = derivedObjectsReservation(limit, 0)
			} else {
				boot.Store.Objects.Backend = config.ObjectBackendS3
			}
			if floor := BrokerFloorFor(&boot); floor.Limit > limit {
				t.Errorf("a fresh node needs a %d-byte limit and the volume states %d: %s",
					floor.Limit, limit, floor.Sentence())
			}
			host := sizingHost{
				budget: jetstream.StorageBudget{Limit: limit, Committed: bucket + others,
					Source: jetstream.BudgetServerStore},
				held: held,
			}
			sized, err := sizeCeilings(t.Context(), host, config.Stream{}, tc.volume,
				"/var/lib/crewlet/stream")
			if err != nil {
				t.Fatalf("sizeCeilings: %v", err)
			}
			created := sized[executor].Bytes
			if created != config.ExecutorLogMaxBytesFloor {
				t.Errorf("beside the other logs at their floors the executor log is created "+
					"at %d bytes, want its %d-byte floor", created, config.ExecutorLogMaxBytesFloor)
			}
			if total := bucket + others + created; total > limit {
				t.Errorf("the bucket, the other logs and the executor log reserve %d bytes "+
					"of a %d-byte limit: an existing node is refused the executor log", total, limit)
			}
		})
	}
}
