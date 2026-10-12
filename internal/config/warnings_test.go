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
				b.Stream.StoreDir = "/var/lib/crewlet/stream"
				b.Stream.Cluster = config.StreamCluster{
					Name: "crewlet", Port: 6222,
					Peers: []string{"nats://node-b.internal:6222", "nats://node-c.internal:6222"},
				}
			},
			"stream.sync", "30s behind the disk",
		},
		// A PEER ENTRY THAT IS NOT ANOTHER MEMBER is left out of every
		// count of the cluster, and the operator is told which one and why
		// — the count they wrote is not the count the rules used.
		"this node's own route in its peer list is not counted": {
			func(b *config.Bootstrap) {
				b.Coordination = config.Coordination{Type: config.CoordinationEmbeddedKV}
				b.Stream.Replicas = 3
				b.Stream.StoreDir = "/var/lib/crewlet/stream"
				b.Stream.Cluster = config.StreamCluster{
					Name: "crewlet", Port: 6222, Host: "10.0.0.11",
					Peers: []string{"nats://10.0.0.11:6222", "nats://node-b.internal:6222",
						"nats://node-c.internal:6222"},
				}
			},
			"stream.cluster.peers[0]", "this node's own route (it matches cluster.host and cluster.port)",
		},
		"a member listed twice is counted once": {
			func(b *config.Bootstrap) {
				b.Coordination = config.Coordination{Type: config.CoordinationEmbeddedKV}
				b.Stream.Replicas = 3
				b.Stream.StoreDir = "/var/lib/crewlet/stream"
				b.Stream.Cluster = config.StreamCluster{
					Name: "crewlet", Port: 6222,
					Peers: []string{"nats://node-b.internal:6222", "nats://node-c.internal:6222",
						"nats://node-c.internal:6222"},
				}
			},
			"stream.cluster.peers[2]", "repeats peers[1]",
		},
		"an operator floor never trims until it is told": {
			func(b *config.Bootstrap) {
				b.Stream.TrackerRetention.BackupFloor = config.BackupFloorOperator
			},
			"stream.tracker_retention.backup_floor", "a backup of that log has been acknowledged",
		},
		"nobody owns the backup": {
			func(b *config.Bootstrap) {},
			"retention.backup_owner", "backup owner",
		},
		// THE INGRESS ROLE WITH NO LISTENER: the node integrations, a
		// browser and an orchestrator all look for binds nothing.
		"an ingress node with no port": {
			func(b *config.Bootstrap) {
				b.Node.Roles = []string{"data", "ingress", "seats", "workers"}
			},
			"api.port", "no orchestrator can probe it",
		},
		"an in-memory stream loses everything": {
			func(b *config.Bootstrap) { b.Stream.StoreDir = "" },
			"stream.store_dir", "keeps them in memory",
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

// AND A PEER LIST OF THE OTHER MEMBERS SAYS NOTHING ABOUT ITS PEERS — with
// this node's own listener and advertised address both stated, so the quiet
// is the comparison finding no match rather than having nothing to compare.
func TestAPeerListOfOtherMembersWarnsAboutNothing(t *testing.T) {
	t.Parallel()
	b := config.DefaultBootstrap()
	b.Stream.StoreDir = "/var/lib/crewlet/stream"
	b.Retention.BackupOwner = "platform-oncall"
	b.Coordination = config.Coordination{Type: config.CoordinationEmbeddedKV}
	b.Stream.Replicas = 3
	// DECLARED, as a fleet node's roles are: a member of a fleet that left
	// them to the every-role default is warned about that, which is not
	// what this case is about.
	b.Node.Roles = []string{"data", "ingress", "seats", "workers"}
	// AND A PORT, as an ingress node's is: one with none is warned about
	// that, which is not what this case is about either.
	b.API.Port = 8080
	b.Stream.Cluster = config.StreamCluster{
		Name: "crewlet", Port: 6222, Host: "10.0.0.11", Advertise: "node-a.internal",
		Peers: []string{"nats://node-b.internal:6222", "nats://node-c.internal:6222"},
	}
	if err := b.Validate(); err != nil {
		t.Fatalf("the fixture does not validate: %v", err)
	}
	if got := b.Warnings(); len(got) != 0 {
		t.Errorf("a peer list of the other members warned: %v", got)
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

// A UNIT WITH NO ID IS KEYED ON ITS NAME, and the warning names BOTH halves —
// because giving it an id fixes one of them and not the other.
func TestAUnitWithNoIDIsWarnedAboutInBothHalves(t *testing.T) {
	t.Parallel()
	// THE CHANNEL IS HERE SO THIS CASE STAYS ABOUT THE ID. A native-chat
	// company whose units declare no channel raises an advisory of its own,
	// and a fixture that collected it would assert two warnings while
	// naming one rule.
	c := &config.Company{
		Name: "Acme",
		Units: []config.Unit{{
			Name:    "Platform",
			Channel: "platform",
			Roles:   []config.Role{{Name: "SWE", Handle: "swe"}},
		}},
	}
	warnings := c.Warnings()
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want one about the unit", warnings)
	}
	for _, want := range []string{"unit \"Platform\"", "no `id`", "renaming it moves", "re-onboarding"} {
		if !strings.Contains(warnings[0].Message, want) {
			t.Errorf("warning does not say %q: %q", want, warnings[0].Message)
		}
	}
	// AND IT IS PLACED AT THE FIELD THEY WOULD ADD, not at the unit in
	// prose: `units[0].id` is the line an editor opens and the node a
	// dashboard marks, and the unit is named for a reader with no document
	// in front of them.
	if w := warnings[0]; w.Kind != config.WarningAdvisory || w.Path != "units[0].id" ||
		!reflect.DeepEqual(w.Segments, config.Path{"units", 0, "id"}) || w.Unit != "Platform" {
		t.Errorf("warning = %+v, want an advisory at units[0].id naming the unit", w)
	}

	// AND AN ID SILENCES IT.
	c.Units[0].ID = "platform"
	if got := c.Warnings(); len(got) != 0 {
		t.Errorf("a unit with an id still warned: %v", got)
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
		// The channels keep this case about the id, as above — and the
		// child deliberately declares none, which is inheritance working
		// rather than an omission: a unit that takes its parent's channel
		// raises nothing.
		Units: []config.Unit{
			{Name: "Product", ID: "product", Channel: "product",
				Roles: []config.Role{{Name: "PM", Handle: "pm"}}},
			{Name: "Engineering", ID: "engineering", Channel: "engineering", Children: []config.Unit{
				{Name: "Platform", Roles: []config.Role{{Name: "SWE", Handle: "swe"}}},
			}},
		},
	}
	warnings := c.Warnings()
	if len(warnings) != 1 {
		t.Fatalf("warnings = %+v, want one about the child unit alone", warnings)
	}
	if w := warnings[0]; w.Kind != config.WarningAdvisory ||
		w.Path != "units[1].children[0].id" ||
		!reflect.DeepEqual(w.Segments, config.Path{"units", 1, "children", 0, "id"}) ||
		w.Unit != "Platform" {
		t.Errorf("warning = %+v, want an advisory at units[1].children[0].id", w)
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

// AN OPERATOR FLOOR NAMES EVERY CEILING THAT WILL REFUSE, not the mutation
// log's alone.
//
// Until somebody acknowledges a backup of it nothing trims any of the three
// ordered logs, and none of them carries an age bound, so each grows until its
// own ceiling refuses an append. Which one fills first depends on the volume
// and the company rather than on the configuration this advisory reads, so a
// message naming one field sends the operator to the wrong ceiling whenever
// another log won — and when that is the chat log the refusal is a message a
// person could not send.
//
// AND THE ACKNOWLEDGEMENT IS PER LOG — every log. It names one stream, and the
// backup term blocks every log the counted point does not cover, so a message
// saying the first acknowledgement unblocks the trim told an operator who
// acknowledged the mutation log that the other two would trim as well; and the
// two compacted logs, which never refuse a write, raise their `backup_age`
// alarm without one, so a message naming only the ordered three left two
// alarms firing for ever.
func TestAnOperatorFloorNamesEveryCeilingThatWillRefuse(t *testing.T) {
	t.Parallel()
	b := config.DefaultBootstrap()
	b.Stream.TrackerRetention.BackupFloor = config.BackupFloorOperator
	if err := b.Validate(); err != nil {
		t.Fatalf("the fixture does not validate: %v", err)
	}
	var message string
	for _, w := range b.Warnings() {
		if w.Path == "stream.tracker_retention.backup_floor" {
			message = w.Message
		}
	}
	if message == "" {
		t.Fatalf("no warning about the operator floor: %v", b.Warnings())
	}
	for _, ceiling := range []string{
		"`stream.tracker_log_max_bytes`",
		"`stream.pages_log_max_bytes`",
		"`stream.chat_log_max_bytes`",
		// AND THE TWO COMPACTED LOGS, which never refuse a write but
		// wait on an acknowledgement all the same: their backup_age
		// alarm fires without one.
		"`stream.tracker_vectors_max_bytes`",
		"`stream.usage_log_max_bytes`",
		"`backup_age`",
	} {
		if !strings.Contains(message, ceiling) {
			t.Errorf("warning = %q, want it to name %s", message, ceiling)
		}
	}
	if strings.Contains(message, "each ordered log is trimmed") {
		t.Errorf("warning = %q says only the ordered logs wait on an "+
			"acknowledgement; every log does", message)
	}
	for _, perLog := range []string{"once per log", "-stream <log>"} {
		if !strings.Contains(message, perLog) {
			t.Errorf("warning = %q, want it to say the acknowledgement is per "+
				"log (%q)", message, perLog)
		}
	}
	if strings.Contains(message, "first acknowledgement") {
		t.Errorf("warning = %q reads as though one acknowledgement unblocks "+
			"every log", message)
	}
}

// A NATIVE BACKEND ON AN IN-MEMORY STREAM IS REFUSED, and it takes both
// documents to see it.
//
// The engine's own tracker, knowledge base and chat keep their write-ahead
// logs on the stream, and an embedded stream with no store directory keeps its
// streams in memory, so a restart recreates them empty and a node whose
// durable tables are ahead of a stream that restarted from nothing refuses to
// serve PERMANENTLY: every snapshot it could adopt is above the recreated
// stream too.
//
// ANY BACKEND. The rule once asked only about the tracker, and the knowledge
// base is native by default, so a company on Jira with no Confluence ran its
// pages on an in-memory log and was accepted. Chat is native by default too,
// and a company that declares no chat block at all is the commonest one there
// is, so it has a row of its own beside the one that names native outright.
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
		// chat is EVERY ROW'S, so each row stays about the backend it
		// names; empty is the derivation, and only the row that is about
		// the derivation leaves it so.
		chat   config.ChatBackend
		accept bool
		leaf   bool
	}{
		// A LEAF KEEPS NO STREAM: every log it reaches is a member's, and
		// the member holds the store directory.
		"native on a leaf with no store directory": {"", config.StreamEmbedded, config.TrackerNative, config.KnowledgeNative, config.ChatNative, true, true},
		"native chat on a leaf":                    {"", config.StreamEmbedded, config.TrackerNone, config.KnowledgeNone, config.ChatNative, true, true},
		"all three native on an in-memory stream":  {"", config.StreamEmbedded, config.TrackerNative, config.KnowledgeNative, config.ChatNative, false, false},
		"a native tracker on an in-memory stream":  {"", config.StreamEmbedded, config.TrackerNative, config.KnowledgeNone, config.ChatNone, false, false},
		"native pages on an in-memory stream":      {"", config.StreamEmbedded, config.TrackerNone, config.KnowledgeNative, config.ChatNone, false, false},
		"native chat alone on an in-memory stream": {"", config.StreamEmbedded, config.TrackerNone, config.KnowledgeNone, config.ChatNative, false, false},
		// THE ROW THE RULE MISSED: nothing native named anywhere, and no
		// vendor chat declared, so chat derives native and its rooms are
		// on the stream.
		"chat derived native on an in-memory stream": {"", config.StreamEmbedded, config.TrackerNone, config.KnowledgeNone, "", false, false},
		"a blank store directory is none":            {"  ", config.StreamEmbedded, config.TrackerNone, config.KnowledgeNative, config.ChatNone, false, false},
		"native with a store directory":              {"/var/lib/crewlet/stream", config.StreamEmbedded, config.TrackerNative, config.KnowledgeNative, config.ChatNative, true, false},
		"native on an external cluster":              {"", config.StreamNATS, config.TrackerNative, config.KnowledgeNative, config.ChatNative, true, false},
		"nothing native on the same stream":          {"", config.StreamEmbedded, config.TrackerNone, config.KnowledgeNone, config.ChatNone, true, false},
	} {
		t.Run(name, func(t *testing.T) {
			b := config.DefaultBootstrap()
			b.Stream.Type = tc.streamType
			b.Stream.StoreDir = tc.storeDir
			if tc.streamType == config.StreamNATS {
				b.Stream.URL = "nats://broker.example.com:4222"
			}
			if tc.leaf {
				b.Stream.Leaf.URLs = []string{"nats-leaf://data-a.example.com:7422"}
			}
			c := config.DefaultCompany()
			c.Name = "Acme"
			c.Tracker.Backend = tc.tracker
			c.Knowledge.Backend = tc.knowledge
			c.Chat.Backend = tc.chat

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
				// The RESOLVED value, which is what the row about the
				// derivation needs the refusal to tell its author.
				"chat.backend: " + string(c.ChatBackendFor()),
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal = %q, want it to say %q", err, want)
				}
			}
		})
	}
}

// THE PORT IS WARNED ABOUT ONLY WHERE INGRESS WAS ASKED FOR. A node without the
// role that binds nothing is a satellite with nothing inbound, which an
// operator chooses; an omitted role list is the single-process default before
// anybody chose a port; and a declared ingress node with a port is the shape
// the warning asks for. Each is held quiet here, so a warning that fired on
// every port of 0 fails this rather than training its reader to skip it.
func TestOnlyADeclaredIngressNodeIsWarnedForBindingNothing(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*config.Bootstrap){
		"a data node without ingress": func(b *config.Bootstrap) {
			b.Node.Roles = []string{"data", "seats", "workers"}
		},
		"roles left to the default": func(b *config.Bootstrap) {},
		"a declared ingress node with a port": func(b *config.Bootstrap) {
			b.Node.Roles = []string{"data", "ingress", "seats", "workers"}
			b.API.Port = 8080
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := config.DefaultBootstrap()
			b.Stream.StoreDir = "/var/lib/crewlet/stream"
			b.Retention.BackupOwner = "platform-oncall"
			mutate(&b)
			if err := b.Validate(); err != nil {
				t.Fatalf("the fixture does not validate: %v", err)
			}
			for _, w := range b.Warnings() {
				if w.Path == "api.port" {
					t.Errorf("warned about api.port: %s", w.Message)
				}
			}
		})
	}
}
