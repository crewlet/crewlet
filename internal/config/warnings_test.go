package config_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/logging"
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
		// A BROKER ASKED TO BE VERBOSE INTO A SINK THAT TAKES NO DEBUG
		// produces nothing at all: `stream.debug` unlocks nats-server's
		// own Debugf population, and those are still DEBUG records.
		// Nothing refuses it, because the flags override the file.
		"a verbose broker with nowhere to say it": {
			func(b *config.Bootstrap) { b.Stream.Debug = true },
			"stream.debug", "no destination records it",
		},
		// AND THE CONSOLE'S LEVEL DOES NOT COUNT WHEN THE CONSOLE IS OFF.
		// `logging.stderr: false` hands the stream to the file, so a
		// `debug` console level beside a `warn` file installs no
		// destination that would record a broker line.
		"a verbose broker behind a console that was switched off": {
			func(b *config.Bootstrap) {
				b.Stream.Debug = true
				b.Logging.Level = logging.LevelDebug
				b.Logging.Stderr = new(bool)
				b.Logging.File.Path = "/var/log/crewlet/node.log"
				b.Logging.File.Level = logging.LevelWarn
			},
			"stream.debug", "no destination records it",
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
			if found.Kind != config.WarningAdvisory {
				t.Errorf("kind = %q, want %q", found.Kind, config.WarningAdvisory)
			}
			// AND IT NAMES NO REFERENCE. An advisory is about a setting,
			// so the three fields a dangling reference fills are empty,
			// which is what a consumer branching on Ref reads.
			if found.Ref != "" || found.From != "" || found.To != "" {
				t.Errorf("advisory = %+v, want ref, from and to empty", *found)
			}
			// AND IT CARRIES ITS PATH TAKEN APART. A rendered path is what
			// an operator reads; the segments are what a form marks the
			// field with, and a warning with only the first is one no
			// editor can jump to.
			if found.Segments == nil || found.Segments.String() != found.Path {
				t.Errorf("segments = %v, want the segments of %q", found.Segments, found.Path)
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

// AND IT GOES QUIET AS SOON AS ANY DESTINATION WOULD RECORD THE LINES. The
// console and the log file are separate levels on purpose — a `debug` file
// behind a `warn` console is a supported shape — so a warning that only read
// `logging.level` would call that combination silent when it is not.
func TestAVerboseBrokerIsQuietOnceSomethingRecordsIt(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*config.Bootstrap){
		"the console is at debug": func(b *config.Bootstrap) {
			b.Logging.Level = logging.LevelDebug
		},
		"only the log file is at debug": func(b *config.Bootstrap) {
			b.Logging.Level = logging.LevelWarn
			b.Logging.File.Path = "/var/log/crewlet/node.log"
			b.Logging.File.Level = logging.LevelDebug
		},
		// The file takes the stream over and FOLLOWS logging.level, which
		// is what makes `-debug` reach both destinations.
		"the file follows a debug logging.level with the console off": func(b *config.Bootstrap) {
			b.Logging.Level = logging.LevelDebug
			b.Logging.Stderr = new(bool)
			b.Logging.File.Path = "/var/log/crewlet/node.log"
		},
	} {
		t.Run(name, func(t *testing.T) {
			b := config.DefaultBootstrap()
			b.Stream.StoreDir = "/var/lib/crewlet/stream"
			b.Retention.BackupOwner = "platform-oncall"
			b.Stream.Debug = true
			mutate(&b)
			if err := b.Validate(); err != nil {
				t.Fatalf("the fixture does not validate: %v", err)
			}
			for _, w := range b.Warnings() {
				if w.Path == "stream.debug" {
					t.Errorf("warned anyway: %s", w.Message)
				}
			}
		})
	}
}

// A UNIT WITH NO ID IS REFUSED ON A WRITE AND WARNED ABOUT ON A STORED
// REVISION, which is what an ADMISSION rule is: a unit is referenced by its
// key, so a new document may not leave a team keyed on prose, while a company
// stored before the rule still boots on its name.
//
// It was an advisory once, and reporting it in both channels would have put
// one mistake in front of an operator twice.
func TestAUnitWithNoIDIsRefusedAndWarnedAbout(t *testing.T) {
	t.Parallel()
	c := &config.Company{
		Name: "Acme",
		Units: []config.Unit{{
			Name:  "Platform",
			Roles: []config.Role{{Name: "SWE", Handle: "swe"}},
		}},
	}
	// A SUBMITTED DOCUMENT IS REFUSED, and a running one is not: the key
	// falls back to the name, which is exactly how every company behaved
	// before the field.
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "units[0].id") {
		t.Errorf("Validate() = %v, want a refusal naming units[0].id", err)
	}
	// The fixture is a bare struct rather than the defaults, so the
	// runnable half has its own complaints; what matters is that the
	// missing id is not one of them.
	if err := c.ValidateRunnable(); err != nil && strings.Contains(err.Error(), "units[0].id") {
		t.Errorf("ValidateRunnable() = %v: a stored revision with no unit ids "+
			"still runs, keyed on its names", err)
	}

	warnings := c.Warnings()
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want one about the unit", warnings)
	}
	for _, want := range []string{"unit \"Platform\"", "has no id", "`manages:`", "id: platform"} {
		if !strings.Contains(warnings[0].Message, want) {
			t.Errorf("warning does not say %q: %q", want, warnings[0].Message)
		}
	}
	// AND IT IS PLACED AT THE FIELD THEY WOULD ADD, not at the unit in
	// prose: `units[0].id` is the line an editor opens and the node a
	// dashboard marks, and the unit is named for a reader with no document
	// in front of them.
	if w := warnings[0]; w.Kind != config.WarningAdmission || w.Path != "units[0].id" ||
		!reflect.DeepEqual(w.Segments, config.Path{"units", 0, "id"}) || w.Unit != "Platform" {
		t.Errorf("warning = %+v, want an admission warning at units[0].id naming the unit", w)
	}

	// AND AN ID SILENCES IT, in both channels.
	c.Units[0].ID = "platform"
	if got := c.Warnings(); len(got) != 0 {
		t.Errorf("a unit with an id still warned: %v", got)
	}
	if err := c.Validate(); err != nil && strings.Contains(err.Error(), "units[0].id") {
		t.Errorf("a unit with an id was refused at its id: %v", err)
	}
}

// AND A NESTED UNIT IS REPORTED WHERE IT WAS WRITTEN. The index is the half
// a rendered path cannot be rebuilt from: a document full of units with no
// id raises this once per unit, and a path that named them all the same
// place would point an editor at one line for every one of them.
func TestANestedUnitWithNoIDIsWarnedAboutWhereItWasWritten(t *testing.T) {
	t.Parallel()
	c := &config.Company{
		Name: "Acme",
		Units: []config.Unit{
			{Name: "Product", ID: "product", Roles: []config.Role{{Name: "PM", Handle: "pm"}}},
			{Name: "Engineering", ID: "engineering", Children: []config.Unit{
				{Name: "Platform", Roles: []config.Role{{Name: "SWE", Handle: "swe"}}},
			}},
		},
	}
	warnings := c.Warnings()
	if len(warnings) != 1 {
		t.Fatalf("warnings = %+v, want one about the child unit alone", warnings)
	}
	if w := warnings[0]; w.Kind != config.WarningAdmission ||
		w.Path != "units[1].children[0].id" ||
		!reflect.DeepEqual(w.Segments, config.Path{"units", 1, "children", 0, "id"}) ||
		w.Unit != "Platform" {
		t.Errorf("warning = %+v, want an admission warning at units[1].children[0].id", w)
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

// A NATIVE BACKEND ON AN IN-MEMORY STREAM IS REFUSED, and it takes both
// documents to see it.
//
// The engine's own tracker and its own knowledge base keep their write-ahead
// logs on the stream, and an embedded stream with no store directory keeps its
// streams in memory, so a restart recreates them empty and a node whose
// durable tables are ahead of a stream that restarted from nothing refuses to
// serve PERMANENTLY: every snapshot it could adopt is above the recreated
// stream too.
//
// EITHER BACKEND. The rule once asked only about the tracker, and the
// knowledge base is native by default, so a company on Jira with no Confluence
// ran its pages on an in-memory log and was accepted.
//
// Each tier validates alone and neither can see the other, which is why this
// is a rule of its own rather than a field's.
func TestANativeBackendNeedsAStreamThatSurvivesARestart(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		storeDir   string
		streamType config.StreamType
		tracker    config.TrackerBackend
		knowledge  config.KnowledgeBackend
		accept     bool
	}{
		"both native on an in-memory stream":      {"", config.StreamEmbedded, config.TrackerNative, config.KnowledgeNative, false},
		"a native tracker on an in-memory stream": {"", config.StreamEmbedded, config.TrackerNative, config.KnowledgeNone, false},
		"native pages on an in-memory stream":     {"", config.StreamEmbedded, config.TrackerNone, config.KnowledgeNative, false},
		"a blank store directory is none":         {"  ", config.StreamEmbedded, config.TrackerNone, config.KnowledgeNative, false},
		"native with a store directory":           {"/var/lib/crewlet/stream", config.StreamEmbedded, config.TrackerNative, config.KnowledgeNative, true},
		"native on an external cluster":           {"", config.StreamNATS, config.TrackerNative, config.KnowledgeNative, true},
		"vendors for both on the same stream":     {"", config.StreamEmbedded, config.TrackerNone, config.KnowledgeNone, true},
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
			c.Knowledge.Backend = tc.knowledge

			err := config.CheckTiers(&b, &c)
			if tc.accept {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("a native backend on an in-memory stream was accepted")
			}
			for _, want := range []string{
				"stream.store_dir", "refuses to serve",
				"tracker.backend: " + string(tc.tracker),
				"knowledge.backend: " + string(tc.knowledge),
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal = %q, want it to say %q", err, want)
				}
			}
		})
	}
}
