package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
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
// stream.store_max_bytes accepts six gibibytes, and a fresh node of the default
// document needs more than ten, so a value the schema takes is one the boot
// refuses. Said only where it is exact: a data node on its own embedded broker,
// whose limit is the number written. Anywhere else the limit is a volume or an
// account this judgement cannot see, and it says nothing rather than guess.
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
// sized for everything else boots with it. Its unset 512 MiB counted in full
// raised the least a fresh node reserves past the broker limit
// crewlet-operator gives the smallest data volume it admits, the one its
// getting-started company runs on, and every such node refused to boot on
// CREWLET_ASSISTANT_LOG. Scaled, it counts its field's 64 MiB floor, and that
// limit holds the whole fresh node again.
//
// THAT LIMIT IS 12 GiB, three quarters of a 16Gi volume. It was 9 GiB, of
// 12Gi, when the assistant's scaling was written; then the chat log joined the
// register at the gibibyte every corpus-sized log is scaled no lower than
// ([config.ChatLogMaxBytesFloor]), and a fresh node reserved 10826896725 bytes,
// three quarters of a 14Gi volume. Chat does not give way on a short broker as
// the assistant does: it is not a feature a company may never turn on — it is
// the company's chat unless a vendor's is declared — and the room a 9 GiB
// limit leaves it, 192 MiB beside the eight other logs at their floors and the
// bucket, is less than the one min_age window of messages a healthily trimming
// chat log keeps, about 225 MB at its declared census. Then the code host's log
// joined at the same gibibyte ([config.CodeLogMaxBytesFloor]): it is the
// write-ahead log of every repository the company hosts, which gives way on a
// short broker no more than chat does, and a fresh node reserves 12258552490
// bytes, past 15Gi's 11.25 GiB. So the least a fresh node needs moved with the
// register, as it did when the artifact logs joined, and crewlet-operator's
// smallest volume moved with it.
//
// THE SCALING NO LONGER DECIDES WHETHER THAT VOLUME BOOTS: counted at its
// 512 MiB, a fresh node needs 12884901887 bytes, which 16Gi's limit holds by
// one byte. What it decides is the floor itself, the least a broker boots a
// fresh node in, which it lowers by the 448 MiB it gives way and the bucket's
// quarter beside them: a broker stated at that floor boots a fresh node with
// the assistant's log scaled and refuses one counting it in full.
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

// THE SIZE A TEST BROKER DECLARES BOOTS A FRESH NODE OF THE FIXTURES.
//
// The suites size their brokers from brokertest.StoreMaxBytes rather than from
// the runner's free disk, so that a boot passes or fails on the code and not on
// the machine. That holds only while the figure is above the floor of the
// fixtures' own Tier A, and the floor grows whenever a log joins the register:
// past it, every node boot in every suite is refused on every machine. This
// case fails beside them, as the one that names the cause, both figures and
// the constant to raise — and through Tier A's own validation, so the figure
// is one an operator could write.
func TestTheTestBrokerSizeHoldsAFreshNode(t *testing.T) {
	t.Parallel()
	boot := testBootstrap(t)
	brokertest.StoreIn(&boot.Stream, filepath.Join(t.TempDir(), "stream"))
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
