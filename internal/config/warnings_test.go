package config_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// A WARNING IS A VALID CONFIGURATION WITH A CONSEQUENCE, and the consequence
// is the part that matters: a warning that only says something is unusual is
// one people learn to ignore.
func TestTierAWarnsAboutWhatItCannotRefuse(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		mutate func(*config.Bootstrap)
		path   string
		says   string
	}{
		"a declined fsync names its window": {
			func(b *config.Bootstrap) {
				b.Coordination = config.Coordination{Type: config.CoordinationEmbeddedKV}
				b.Stream.Replicas = 3
				b.Stream.Sync = "30s"
				b.Stream.Cluster = config.StreamCluster{
					Name: "crewlet", Port: 6222,
					Peers: []string{"nats://node-b.internal:6222", "nats://node-c.internal:6222"},
				}
			},
			"stream.sync", "30s behind the disk",
		},
		"an operator floor never trims until it is told": {
			func(b *config.Bootstrap) {
				b.Stream.TrackerRetention.BackupFloor = config.BackupFloorOperator
			},
			"stream.tracker_retention.backup_floor", "never trimmed",
		},
		"nobody owns the backup": {
			func(b *config.Bootstrap) {},
			"retention.backup_owner", "backup owner",
		},
		"an in-memory stream loses everything": {
			func(b *config.Bootstrap) { b.Stream.StoreDir = "" },
			"stream.store_dir", "keeps everything in memory",
		},
	} {
		t.Run(name, func(t *testing.T) {
			b := config.DefaultBootstrap()
			tc.mutate(&b)
			if err := b.Validate(); err != nil {
				t.Fatalf("the fixture does not validate, so this is a refusal "+
					"rather than a warning: %v", err)
			}
			var found *config.Warning
			for _, w := range b.Warnings() {
				if w.Path == tc.path {
					found = &w
					break
				}
			}
			if found == nil {
				t.Fatalf("no warning about %s: %v", tc.path, b.Warnings())
			}
			if !strings.Contains(found.Message, tc.says) {
				t.Errorf("warning = %q, want it to say %q", found.Message, tc.says)
			}
		})
	}
}

// AND A DEPLOYMENT THAT HAS SAID EVERYTHING IS QUIET. A channel that always
// has something in it is one nobody reads.
func TestAFullyStatedDeploymentWarnsAboutNothing(t *testing.T) {
	t.Parallel()
	b := config.DefaultBootstrap()
	b.Stream.StoreDir = "/var/lib/crewlet/stream"
	b.Retention.BackupOwner = "platform-oncall"
	if err := b.Validate(); err != nil {
		t.Fatalf("the fixture does not validate: %v", err)
	}
	if got := b.Warnings(); len(got) != 0 {
		t.Errorf("a fully stated deployment warned: %v", got)
	}
}

// A UNIT WITH NO ID IS KEYED ON ITS NAME, and the warning names BOTH halves —
// because giving it an id fixes one of them and not the other.
func TestAUnitWithNoIDIsWarnedAboutInBothHalves(t *testing.T) {
	t.Parallel()
	c := &config.Company{
		Name: "Acme",
		Units: []config.Unit{{
			Name:  "Platform",
			Roles: []config.Role{{Name: "SWE", Handle: "swe"}},
		}},
	}
	warnings := c.Warnings()
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want one about the unit", warnings)
	}
	for _, want := range []string{"no `id`", "renaming it moves", "re-onboarding"} {
		if !strings.Contains(warnings[0].Message, want) {
			t.Errorf("warning does not say %q: %q", want, warnings[0].Message)
		}
	}

	// AND AN ID SILENCES IT.
	c.Units[0].ID = "platform"
	if got := c.Warnings(); len(got) != 0 {
		t.Errorf("a unit with an id still warned: %v", got)
	}
}

// VECTORS NEED A CORPUS, AND A NATIVE TRACKER IS ONE.
//
// The rule this narrows refused vectors whenever `knowledge.backend` was
// `none` — which was right when the knowledge base was the only thing there
// was to embed, and wrong once the engine held the work items too. A company
// that keeps its documents somewhere the engine cannot search and its work
// here is entitled to semantic recall over the work.
func TestVectorsNeedAKnowledgeBaseOrANativeTracker(t *testing.T) {
	t.Parallel()
	on := true
	for name, tc := range map[string]struct {
		knowledge config.KnowledgeBackend
		tracker   config.TrackerBackend
		accept    bool
	}{
		"a knowledge base and a native tracker": {config.KnowledgeNative, config.TrackerNative, true},
		"no knowledge base, native tracker":     {config.KnowledgeNone, config.TrackerNative, true},
		"a knowledge base, no tracker":          {config.KnowledgeNative, config.TrackerNone, true},
		"neither":                               {config.KnowledgeNone, config.TrackerNone, false},
	} {
		t.Run(name, func(t *testing.T) {
			c := config.DefaultCompany()
			c.Name = "Acme"
			c.Knowledge.Backend = tc.knowledge
			c.Tracker.Backend = tc.tracker
			c.Knowledge.Vectors = &on
			c.Providers.Embeddings = &config.EmbeddingProvider{
				Type: config.EmbeddingOpenAI, Model: "text-embedding-3-large",
			}
			err := c.Validate()
			refused := err != nil && strings.Contains(err.Error(), "knowledge.vectors")
			if tc.accept && refused {
				t.Fatalf("refused: %v", err)
			}
			if !tc.accept {
				if !refused {
					t.Fatal("vectors with no corpus at all were accepted")
				}
				if !strings.Contains(err.Error(), "native tracker") {
					t.Errorf("refusal = %q, want it to name the other corpus", err)
				}
			}
		})
	}
}

// A NATIVE TRACKER ON AN IN-MEMORY STREAM IS REFUSED, and it takes both
// documents to see it.
//
// The engine's own tracker keeps its write-ahead log on the stream, and an
// embedded stream with no store directory keeps its streams in memory — so a
// restart recreates them empty, and a node whose durable tables are ahead of a
// stream that restarted from nothing refuses to serve the tracker
// PERMANENTLY: every snapshot it could adopt is above the recreated stream
// too.
//
// Each tier validates alone and neither can see the other, which is why this
// is a rule of its own rather than a field's.
func TestANativeTrackerNeedsAStreamThatSurvivesARestart(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		storeDir   string
		streamType config.StreamType
		tracker    config.TrackerBackend
		accept     bool
	}{
		"native on an in-memory stream": {"", config.StreamEmbedded, config.TrackerNative, false},
		"native with a store directory": {"/var/lib/crewlet/stream", config.StreamEmbedded, config.TrackerNative, true},
		"native on an external cluster": {"", config.StreamNATS, config.TrackerNative, true},
		"no tracker on the same stream": {"", config.StreamEmbedded, config.TrackerNone, true},
	} {
		t.Run(name, func(t *testing.T) {
			b := config.DefaultBootstrap()
			b.Stream.Type = tc.streamType
			b.Stream.StoreDir = tc.storeDir
			if tc.streamType == config.StreamNATS {
				b.Stream.URL = "nats://broker.example.com:4222"
			}
			c := config.DefaultCompany()
			c.Name = "Acme"
			c.Tracker.Backend = tc.tracker

			err := config.CheckTiers(&b, &c)
			if tc.accept {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("a native tracker on an in-memory stream was accepted")
			}
			for _, want := range []string{"stream.store_dir", "refuses to serve"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal = %q, want it to say %q", err, want)
				}
			}
		})
	}
}
