package config_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/org"
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
			b := config.RunnableBootstrap()
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
	b := config.RunnableBootstrap()
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
			b := config.RunnableBootstrap()
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
		// A REACHABLE PERSON'S SEAT, so the one warning counted is the
		// unit's: a company with no human seat carries an advisory of its
		// own ([config.Company.AdvisoryWarnings]).
		Roles: []config.Role{founderSeat()},
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

// founderSeat is a human seat somebody can be reached at: what a fixture
// counting warnings declares, so the count is its subject's alone — a company
// with no human seat is advised that nobody can join it, and a human seat with
// no contact identity that nobody can be mentioned at.
func founderSeat() config.Role {
	return config.Role{Name: "Founder", Handle: "founder", Kind: org.KindHuman,
		Contact: &org.HumanContact{SlackUserID: "U0FOUNDER"}}
}

// AND A NESTED UNIT IS REPORTED WHERE IT WAS WRITTEN. The index is the half
// a rendered path cannot be rebuilt from: a document full of units with no
// id raises this once per unit, and a path that named them all the same
// place would point an editor at one line for every one of them.
func TestANestedUnitWithNoIDIsWarnedAboutWhereItWasWritten(t *testing.T) {
	t.Parallel()
	c := &config.Company{
		Name:  "Acme",
		Roles: []config.Role{founderSeat()},
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

// A STATE LOG ON AN IN-MEMORY STREAM IS REFUSED BY TIER A ALONE, company or
// none.
//
// A domain's write-ahead log lives on the stream, and an embedded stream with
// no store directory keeps its streams in memory — so a restart recreates them
// empty, and a node whose durable tables are ahead of a stream that restarted
// from nothing refuses to serve PERMANENTLY: every snapshot it could adopt is
// above the recreated stream too.
//
// # Why this is not a cross-tier rule any more
//
// It was one — `CheckTiers`, which asked the company too — while a node's
// logs waited for its first company: only a company started one. Every node
// runs the core runtime from boot now, every domain's log and the identity
// estate on it, so the pair's check let through exactly the node it should
// have refused: one started with no company, whose first person's invitation
// would be gone at its first restart. So there is no company in these cases at
// all.
//
// Mutation: let `Durable` pass an embedded stream with a blank directory and
// the first rows are accepted; drop the call from the stream's validator and
// Validate accepts them while Durable refuses.
func TestEveryNodeNeedsAStreamThatSurvivesARestart(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		stream config.Stream
		accept bool
	}{
		"an embedded stream in memory":      {config.Stream{Type: config.StreamEmbedded, Replicas: 1}, false},
		"an unset type is embedded":         {config.Stream{Replicas: 1}, false},
		"a blank store directory is none":   {config.Stream{Type: config.StreamEmbedded, Replicas: 1, StoreDir: "  "}, false},
		"an embedded stream that persists":  {config.Stream{Type: config.StreamEmbedded, Replicas: 1, StoreDir: "/var/lib/crewlet/stream"}, true},
		"an external cluster keeps its own": {config.Stream{Type: config.StreamNATS, URL: "nats://broker.example.com:4222", Replicas: 1}, true},
	} {
		t.Run(name, func(t *testing.T) {
			// THE ENGINE'S DOOR, which asks the same function for a
			// Bootstrap that did not come through Validate.
			stream := tc.stream
			durable := stream.Durable()
			// AND TIER A'S OWN, over the whole document.
			b := config.RunnableBootstrap()
			b.Stream = tc.stream
			if tc.stream.Type == config.StreamNATS {
				// A fleet's broker needs the fleet's coordination.
				b.Coordination = config.Coordination{Type: config.CoordinationEmbeddedKV}
			}
			validated := b.Validate()
			if tc.accept {
				if durable != nil || validated != nil {
					t.Fatalf("refused: Durable %v, Validate %v", durable, validated)
				}
				return
			}
			for door, err := range map[string]error{"Durable": durable, "Validate": validated} {
				if err == nil {
					t.Fatalf("%s accepted a state log on an in-memory stream, so "+
						"this node would lose every record it holds on its "+
						"first restart and then refuse to serve", door)
				}
				for _, want := range []string{
					"stream.store_dir", "refuses to serve", "identity estate",
				} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("%s's refusal = %q, want it to say %q", door, err, want)
					}
				}
			}
		})
	}
}

// A COMPANY WITH NO HUMAN SEAT IS ONE NOBODY CAN JOIN, and it is said before
// anybody tries.
//
// Every person holds a human seat for as long as they are here (ADR-0026):
// an invitation and an administrator's create each name the seat the person
// will hold, and both are refused without one. So a company declaring none
// admits nobody — which an operator otherwise learns from the refusal of their
// first invitation, after the revision is live. `crewlet validate` and the
// builder's check read [config.Company.Warnings], so that is where it is said.
//
// ADVISED, NEVER REFUSED: a company its agents run alone, administered through
// the deployment's Tier A tokens, is legitimate and validates. And a human seat
// ANYWHERE silences it — inside a unit as much as at the top — because the
// remedy is a seat, not a seat in a particular place.
//
// Mutations: drop the advisory from AdvisoryWarnings and the first two cases
// go quiet; walk only the top-level roles and the unit's seat is missed.
func TestACompanyNobodyCanJoinIsAdvisedNotRefused(t *testing.T) {
	t.Parallel()
	const agents = "name: Acme\nproviders:\n  llm:\n    main:\n" +
		"      type: anthropic\n      model: claude-sonnet-5\n" +
		"      api_keys: [\"${K}\"]\n"
	for name, tc := range map[string]struct {
		doc     string
		advised bool
	}{
		"a company declaring no seat at all": {
			doc: agents, advised: true,
		},
		"a company of agents alone": {
			doc: agents + "roles:\n  - name: CEO\n    handle: ceo\n    llm: main\n" +
				"units:\n  - name: Platform\n    id: platform\n    roles:\n" +
				"      - name: SWE\n        handle: swe\n        llm: main\n",
			advised: true,
		},
		"a human seat at the top": {
			doc: agents + "roles:\n  - name: CEO\n    handle: ceo\n    llm: main\n" +
				"  - name: Founder\n    handle: founder\n    kind: human\n" +
				"    contact:\n      slack_user_id: U0FOUNDER\n",
		},
		"a human seat inside a unit": {
			doc: agents + "roles:\n  - name: CEO\n    handle: ceo\n    llm: main\n" +
				"units:\n  - name: Platform\n    id: platform\n    roles:\n" +
				"      - name: Lead\n        handle: platform-lead\n        kind: human\n" +
				"        contact:\n          slack_user_id: U0LEAD\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// NOT A REFUSAL: the document parses and validates.
			c, err := config.ParseCompany([]byte(tc.doc))
			if err != nil {
				t.Fatalf("a company with no human seat was refused — it is "+
					"legitimate for a company its agents run alone: %v", err)
			}
			var advised []config.Warning
			for _, w := range c.Warnings() {
				if w.Path == "roles" {
					advised = append(advised, w)
				}
			}
			if !tc.advised {
				if len(advised) != 0 {
					t.Errorf("a company declaring a human seat was advised "+
						"nobody can join it: %+v", advised)
				}
				return
			}
			if len(advised) != 1 {
				t.Fatalf("warnings at `roles` = %+v, want the one advisory "+
					"that nobody can be invited or created", advised)
			}
			w := advised[0]
			if w.Kind != config.WarningAdvisory || w.Seat != "" || w.Unit != "" ||
				!reflect.DeepEqual(w.Segments, config.Path{"roles"}) {
				t.Errorf("warning = %+v, want an advisory at `roles` about no "+
					"seat or unit in particular", w)
			}
			// WHAT HAPPENS, WHY, WHEN IT IS FINE, AND WHAT TO DO — the
			// last as the command that works, which names the seat.
			for _, says := range []string{
				"no `kind: human` seat", "nobody can be invited",
				"every person holds a human seat", "Tier A tokens",
				"Agents › Org chart",
				"`crewlet iam invite <address> -seat <handle>`",
			} {
				if !strings.Contains(w.Message, says) {
					t.Errorf("the advisory does not say %q: %q", says, w.Message)
				}
			}
		})
	}
}
