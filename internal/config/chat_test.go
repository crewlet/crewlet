package config

import (
	"errors"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// mattermostBlock is a Mattermost integration written the way a real company
// writes it — enabled, with the two fields that are required then — so a case
// about the chat axis cannot pass by leaving the vendor block invalid.
const mattermostBlock = "integrations:\n  mattermost:\n    enabled: true\n" +
	"    url: https://chat.example.com\n    team: acme\n"

// THE THIRD AXIS DERIVES FROM THE VENDOR IT WOULD DUPLICATE, exactly as the
// tracker and the knowledge base do.
//
// The signal is the BLOCK'S PRESENCE and nothing inside it, because every
// setting a chat block carries has a default: a company that wrote
// `mattermost: {}` has said where its people are, and a company that wrote
// nothing at all has said its seats need somewhere to talk.
//
// AND THE FIELD'S DESCRIPTION NAMES EVERY SOURCE. It is what an editor shows
// beside `chat.backend`, and it once left out the org-level Slack block, so a
// company whose only Slack declaration was that block derived `vendor` with
// nothing on the field saying why. Each vendor row carries the words the
// description must contain for it.
func TestTheChatBackendDerivesFromTheVendorPresent(t *testing.T) {
	t.Parallel()
	backend, ok := reflect.TypeFor[Chat]().FieldByName("Backend")
	if !ok {
		t.Fatal("Chat has no Backend field")
	}
	desc := backend.Tag.Get("desc")
	for name, tc := range map[string]struct {
		doc  string
		want ChatBackend
		// names is what the field's description must say for this
		// source; empty on a row that derives nothing from a vendor.
		names string
	}{
		"a company that configures nothing talks natively": {
			"name: Acme\n", ChatChatlet, "",
		},
		"an enabled mattermost block is a vendor surface": {
			"name: Acme\n" + mattermostBlock, ChatVendor, "integrations.mattermost",
		},
		// A BLOCK THAT IS NOT ENABLED STILL COUNTS, because the derivation
		// asks where this company's conversations live rather than whether
		// the transport is running today: switching a paused Mattermost
		// company to Chatlet behind its operator's back is how a team
		// comes back to two histories.
		"a mattermost block with nothing in it is a vendor surface": {
			"name: Acme\nintegrations:\n  mattermost: {}\n", ChatVendor, "integrations.mattermost",
		},
		"an org-level slack block is a vendor surface": {
			"name: Acme\nintegrations:\n  slack: {typing_status: addressed}\n", ChatVendor,
			"integrations.slack",
		},
		// SLACK IS THE ONE THAT HIDES. Every credential is per seat, and the
		// org-level block holds working-indicator settings a company may
		// never write — so a company with seven working Slack apps and no
		// `integrations.slack` is a vendor company, and asking the block
		// alone would derive Chatlet for it.
		"a seat's own slack app is a vendor surface with no org block at all": {
			"name: Acme\nroles:\n  - name: CEO\n    integrations:\n" +
				"      slack: {bot_token: \"${T}\", signing_secret: \"${S}\"}\n",
			ChatVendor, "the seats' Slack apps",
		},
		"a seat's slack app inside a unit counts too": {
			"name: Acme\nunits:\n  - name: Core\n    channel: core\n    roles:\n" +
				"      - name: CTO\n        integrations:\n" +
				"          slack: {bot_token: \"${T}\", signing_secret: \"${S}\"}\n",
			ChatVendor, "the seats' Slack apps",
		},
		"a named backend wins over the derivation": {
			"name: Acme\nchat: {backend: none}\n" + mattermostBlock, ChatNone, "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := mustCompany(t, tc.doc)
			if got := cfg.ChatBackendFor(); got != tc.want {
				t.Errorf("chat = %q, want %q", got, tc.want)
			}
			if tc.names != "" && !strings.Contains(desc, tc.names) {
				t.Errorf("chat.backend's description does not name %q, which "+
					"this company derives `vendor` from: %q", tc.names, desc)
			}
		})
	}
}

// THE VENDOR CHAT SURFACES ARE ONE STATEMENT, and every reader of them agrees.
//
// Which backend a company derives, whether it declares a surface, the list a
// client is told and the path a refusal names all read [chatVendors]. They
// were a list of names beside a function that tested the same blocks by hand,
// and a vendor added to one would have had the viewer answer `vendor` naming
// nobody, or name a surface the backend decision never counted. So each
// vendor, declared ALONE, must be all four things at once.
func TestEveryVendorChatSurfaceIsReadFromOneStatement(t *testing.T) {
	t.Parallel()
	// HOW A COMPANY DECLARES EACH, written out by hand: a vendor added to the
	// table with no document here fails rather than passing over a company
	// that declares nothing.
	declaring := map[string]string{
		"mattermost": "name: Acme\n" + mattermostBlock,
		"slack":      "name: Acme\nintegrations:\n  slack: {typing_status: addressed}\n",
	}
	if len(chatVendors) != len(declaring) {
		t.Errorf("%d chat vendors and %d declaring documents: a vendor was "+
			"added to one and not the other", len(chatVendors), len(declaring))
	}
	for _, vendor := range chatVendors {
		t.Run(vendor.name, func(t *testing.T) {
			t.Parallel()
			doc, ok := declaring[vendor.name]
			if !ok {
				t.Fatalf("no document in this test declares the chat vendor %q",
					vendor.name)
			}
			cfg := mustCompany(t, doc)
			if got := cfg.ChatBackendFor(); got != ChatVendor {
				t.Errorf("a company declaring only %s derives chat %q, want %q",
					vendor.name, got, ChatVendor)
			}
			if !cfg.DeclaresIntegration(vendor.name) {
				t.Errorf("a company declaring only %s does not declare it", vendor.name)
			}
			if got := cfg.ChatVendors(); !reflect.DeepEqual(got, []string{vendor.name}) {
				t.Errorf("a company declaring only %s names its chat vendors %v",
					vendor.name, got)
			}
			// AND CHATLET BESIDE IT IS REFUSED BY THE PATH IT WAS
			// DECLARED AT, which is what an author has to delete.
			path := vendor.declaredAt(cfg)
			if path == "" {
				t.Fatalf("%s declares no path on a company that declares it", vendor.name)
			}
			cfg.Chat.Backend = ChatChatlet
			if err := cfg.validateChat(); err == nil || !strings.Contains(err.Error(), path) {
				t.Errorf("Chatlet beside %s: %v, want a refusal naming %s",
					vendor.name, err, path)
			}
		})
	}
	// A COMPANY DECLARING NONE names none, as an empty list rather than nil,
	// so a client is handed `[]` rather than `null`.
	if got := mustCompany(t, "name: Acme\n").ChatVendors(); got == nil || len(got) != 0 {
		t.Errorf("a company declaring no chat vendor names %#v", got)
	}
}

// EACH REFUSAL NAMES THE FIELD AN AUTHOR HAS TO EDIT. A conflict reported
// without a path is one an operator searches their file for by guessing.
func TestTheChatAxisRefusesWhatItCannotResolve(t *testing.T) {
	t.Parallel()

	// A BACKEND BESIDE ITS VENDOR IS THE MIRROR THE DOCTRINE FORBIDS, and
	// chat is where it cannot be repaired afterwards: a person replied in
	// one surface and the agents are reading the other.
	t.Run("Chatlet beside a vendor chat surface is refused", func(t *testing.T) {
		t.Parallel()
		err := rejects(t, "name: Acme\nchat: {backend: chatlet}\n"+mattermostBlock,
			"chat.backend")
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("want ErrConflict, got %v", err)
		}
		for _, want := range []string{"integrations.mattermost", "Remove one"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal = %q, want it to say %q", err, want)
			}
		}
		// AND A SEAT'S OWN SLACK APP IS A VENDOR SURFACE HERE TOO, named
		// where it was written: a refusal pointing at `integrations.slack`
		// would send an author to a block their document does not have.
		err = rejects(t, "name: Acme\nchat: {backend: chatlet}\nroles:\n"+
			"  - name: CEO\n    integrations:\n"+
			"      slack: {bot_token: \"${T}\", signing_secret: \"${S}\"}\n",
			"chat.backend")
		if !strings.Contains(err.Error(), "roles[0].integrations.slack") {
			t.Errorf("refusal = %q, want it to name the seat's own app", err)
		}
		// AND TWO VENDOR SURFACES ARE ONE SIDE, not two members of a pair:
		// "Remove one" is Chatlet or both blocks, and "both" is Chatlet and
		// the vendor chat, never three things.
		err = rejects(t, "name: Acme\nchat: {backend: chatlet}\n"+mattermostBlock+
			"  slack: {typing_status: addressed}\n", "chat.backend")
		const both = "Chatlet and the vendor surfaces at integrations.mattermost and " +
			"integrations.slack can't both run"
		if !strings.Contains(err.Error(), both) {
			t.Errorf("refusal = %q, want it to say %q", err, both)
		}
	})

	t.Run("a vendor backend with no vendor block is refused", func(t *testing.T) {
		t.Parallel()
		err := rejects(t, "name: Acme\nchat: {backend: vendor}\n", "chat.backend")
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("want ErrConflict, got %v", err)
		}
		if !strings.Contains(err.Error(), "integrations.mattermost") {
			t.Errorf("refusal = %q, want it to name a surface to add", err)
		}
	})

	// A CHATLET POLICY BLOCK ON A COMPANY THAT DOES NOT RUN CHATLET describes
	// nothing, and the failure it produces is silence: a horizon no message
	// this company holds will ever be measured against.
	t.Run("a Chatlet block on a vendor company is refused", func(t *testing.T) {
		t.Parallel()
		err := rejects(t, "name: Acme\nchat:\n  chatlet:\n    message_retention_days: 90\n"+
			mattermostBlock, "chat.chatlet")
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("want ErrConflict, got %v", err)
		}
		// AGAINST THE DERIVED BACKEND, so the message says which one this
		// company actually runs rather than "" for the field nobody wrote.
		if !strings.Contains(err.Error(), string(ChatVendor)) {
			t.Errorf("refusal = %q, want it to name the derived backend", err)
		}
	})

	t.Run("a backend outside the closed set is refused", func(t *testing.T) {
		t.Parallel()
		err := rejects(t, "name: Acme\nchat: {backend: mattermost}\n", "chat.backend")
		if !errors.Is(err, ErrShape) {
			t.Fatalf("want ErrShape, got %v", err)
		}
		// THE SET IS OFFERED, because "invalid value" leaves an author
		// guessing at exactly the moment they have already guessed once.
		if want := `must be one of "chatlet", "vendor" or "none"`; !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %q, want it to offer the backends as %s", err, want)
		}
	})

	// AND A CHATLET BLOCK ON A CHATLET COMPANY IS ORDINARY, including on the
	// company that named no backend at all — refusing the default
	// configuration would be the opposite of the intent.
	t.Run("a Chatlet block on a company that named nothing is accepted", func(t *testing.T) {
		t.Parallel()
		cfg := mustCompany(t, "name: Acme\nchat:\n  chatlet:\n"+
			"    typing_status: addressed\n")
		if got := cfg.ChatBackendFor(); got != ChatChatlet {
			t.Errorf("chat = %q, want chatlet", got)
		}
		if got := cfg.Chat.Chatlet.Status(); got != StatusAddressed {
			t.Errorf("typing status = %q, want addressed", got)
		}
	})
}

// THE CHAT AXIS SPEAKS IN SHORT SENTENCES, and so do Worklet's and Doclet's
// rules it was copied from. Each refusal and warning says
// what is wrong and what to set; the rationale a reader cannot act on, the
// em dash that carried it and the capitals that shouted it are what the
// rewrite of this package's other messages took out, and these are held to
// the same: no em dash, no semicolon gluing a second clause on, and no word
// in capitals outside a `code span`, a quoted value or a named acronym. Each
// rule is driven by its own failing document and read at its own path, so a
// message nobody produces cannot pass by being absent. Mutation: an em dash
// back in the retention range, or "ONE unit" back in the shared-room
// refusal, is red.
func TestTheChatAxisRefusesInShortSentences(t *testing.T) {
	t.Parallel()
	const (
		jira       = "integrations:\n  jira: {url: \"https://acme.example.com\", token: t, webhook_secret: s}\n"
		confluence = "integrations:\n  confluence: {url: \"https://wiki.example.com\", token: t, webhook_secret: s}\n"
		twoTeams   = "name: Acme\nunits:\n  - name: Alpha\n    channel: eng\n    roles:\n" +
			"      - name: Ann\n  - name: Beta\n    channel: eng\n    roles:\n      - name: Bob\n"
	)
	refusals := []struct{ name, doc, path string }{
		{"a backend outside the set", "name: Acme\nchat: {backend: mattermost}\n", "chat.backend"},
		{"Chatlet beside a vendor", "name: Acme\nchat: {backend: chatlet}\n" + mattermostBlock, "chat.backend"},
		{"a vendor with no surface", "name: Acme\nchat: {backend: vendor}\n", "chat.backend"},
		{"a Chatlet block on a vendor company",
			"name: Acme\nchat:\n  chatlet:\n    message_retention_days: 90\n" + mattermostBlock, "chat.chatlet"},
		{"a retention out of range",
			"name: Acme\nchat:\n  chatlet:\n    message_retention_days: 3\n", "chat.chatlet.message_retention_days"},
		{"two units on one channel", twoTeams, "units[0].channel"},
		{"a channel that is not a name",
			"name: Acme\nunits:\n  - name: Eng\n    channel: \"#Eng\"\n    roles:\n      - name: Ann\n",
			"units[0].channel"},
		// Worklet's and Doclet's, which the chat axis's rules were copied
		// from and read alike
		{"a worklet block on a jira company",
			"name: Acme\ntracker:\n  worklet:\n    inbox_retention_days: 90\n" + jira, "tracker.worklet"},
		{"an inbox retention out of range",
			"name: Acme\ntracker:\n  worklet:\n    inbox_retention_days: 3\n", "tracker.worklet.inbox_retention_days"},
		{"Worklet beside jira", "name: Acme\ntracker: {backend: worklet}\n" + jira, "tracker.backend"},
		{"Doclet beside confluence",
			"name: Acme\nknowledge: {backend: doclet}\n" + confluence, "knowledge.backend"},
		{"vectors with nothing to search",
			"name: Acme\nknowledge: {backend: none, vectors: true}\ntracker: {backend: none}\n",
			"knowledge.vectors"},
	}
	var said []struct{ name, message string }
	for _, tc := range refusals {
		_, err := ParseCompany([]byte(tc.doc))
		found := false
		for _, p := range Problems(err) {
			if p.Path == tc.path {
				found = true
				said = append(said, struct{ name, message string }{tc.name, p.Message})
			}
		}
		if !found {
			t.Errorf("%s: no problem at %s, so its words are not read: %v", tc.name, tc.path, err)
		}
	}
	if w := channelWarnings(mustCompany(t, "name: Acme\nunits:\n  - name: Eng\n"+
		"    roles:\n      - name: Ann\n")); len(w) == 1 {
		said = append(said, struct{ name, message string }{"no unit declares a channel", w[0].Message})
	} else {
		t.Errorf("a Chatlet company with no channel carries %d channel warnings, want 1", len(w))
	}
	_, err := ResolveNodeID(&Bootstrap{}, NewResolver(MapSource{NodeIDEnvVar: coord.OperatorBackupOwner}))
	if err == nil {
		t.Error("the operator's backup owner was taken as a node id")
	} else {
		said = append(said, struct{ name, message string }{"a reserved node id", err.Error()})
	}

	quoted := regexp.MustCompile("`[^`]*`|\"[^\"]*\"")
	capitals := regexp.MustCompile(`\b[A-Z]{3,}\b`)
	acronyms := map[string]bool{"API": true, "URL": true, "TTL": true, "JSON": true,
		"YAML": true, "HTTP": true, "HTTPS": true, "RFC": true, "TLS": true, "UTC": true,
		"CLI": true, "LLM": true, "MCP": true}
	for _, m := range said {
		if strings.Contains(m.message, "\u2014") || strings.Contains(m.message, ";") {
			t.Errorf("%s: %q glues a second clause on with a dash or a "+
				"semicolon: say it as its own short sentence", m.name, m.message)
		}
		for _, word := range capitals.FindAllString(quoted.ReplaceAllString(m.message, ""), -1) {
			if !acronyms[word] {
				t.Errorf("%s: %q shouts %q", m.name, m.message, word)
			}
		}
	}
}

// THREE SETTINGS, AND A PLAIN INT CARRIES TWO. Absent is the default horizon,
// 0 is no horizon at all, and a number is that number of days — so the one
// collapse this field must not make is absent onto 0 or 0 onto the default.
//
// The precedent is `providers.sandbox.default_pause_ttl_seconds`: read as a
// plain float64, every operator who wrote 0 ("never pause") was silently
// mapped onto the 1800-second default and billed for the snapshots the
// setting exists to refuse. Here the same collapse deletes a company's chat
// history a year after somebody asked for it to be kept for ever.
func TestTheMessageHorizonTellsAbsentFromZeroFromANumber(t *testing.T) {
	t.Parallel()
	const year = DefaultMessageRetentionDays * 24 * time.Hour

	for name, tc := range map[string]struct {
		doc      string
		want     time.Duration
		bounded  bool
		declared bool
	}{
		"no chat block at all takes the default": {
			"name: Acme\n", year, true, false,
		},
		// A CHATLET BLOCK THAT SAYS NOTHING ABOUT THE HORIZON is still an
		// absent horizon: the field is three-valued on its own, not on
		// whether the block around it exists.
		"a Chatlet block that names other settings takes the default": {
			"name: Acme\nchat:\n  chatlet: {typing_status: addressed}\n", year, true, false,
		},
		"zero is for ever": {
			"name: Acme\nchat:\n  chatlet: {message_retention_days: 0}\n", 0, false, true,
		},
		"a number is that many days": {
			"name: Acme\nchat:\n  chatlet: {message_retention_days: 30}\n",
			30 * 24 * time.Hour, true, true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := mustCompany(t, tc.doc)
			// THE FIELD ITSELF IS THREE-VALUED, and the accessor's answer
			// is derived from it: a test that only read the duration would
			// pass against a plain int that defaulted zero away.
			declared := cfg.Chat.Chatlet != nil && cfg.Chat.Chatlet.MessageRetentionDays != nil
			if declared != tc.declared {
				t.Errorf("declared = %v, want %v", declared, tc.declared)
			}
			got, bounded := cfg.Chat.Chatlet.MessageRetention()
			if bounded != tc.bounded {
				t.Fatalf("bounded = %v, want %v", bounded, tc.bounded)
			}
			if bounded && got != tc.want {
				t.Errorf("retention = %v, want %v", got, tc.want)
			}
		})
	}

	// AND THE RANGE IS REFUSED AT BOTH ENDS, with 0 carved out of it: below
	// a month a decision taken while somebody was away is gone before they
	// read it, and past ten years the horizon is a rounding error against
	// never pruning — which is what 0 says plainly.
	for name, doc := range map[string]string{
		"a fortnight":   "name: Acme\nchat:\n  chatlet: {message_retention_days: 14}\n",
		"a century":     "name: Acme\nchat:\n  chatlet: {message_retention_days: 36500}\n",
		"a negative":    "name: Acme\nchat:\n  chatlet: {message_retention_days: -1}\n",
		"one day short": "name: Acme\nchat:\n  chatlet: {message_retention_days: 29}\n",
	} {
		t.Run(name+" is refused", func(t *testing.T) {
			t.Parallel()
			err := rejects(t, doc, "chat.chatlet.message_retention_days")
			if !errors.Is(err, ErrOutOfRange) {
				t.Fatalf("want ErrOutOfRange, got %v", err)
			}
			// AND THE REFUSAL OFFERS THE OTHER SETTING, because an author
			// writing 36500 wants "for ever" and has no way to know the
			// field spells it 0.
			if !strings.Contains(err.Error(), "for ever") {
				t.Errorf("refusal = %q, want it to offer 0", err)
			}
		})
	}
}

// THE TYPING STATUS IS THE VENDOR BLOCKS' OWN ENUM, and this is what says so:
// a value one of them accepts is a value this one accepts, and a misspelling
// is refused here exactly as it is there rather than degrading silently to
// the default.
func TestChatletReadsTheSameWorkingStatusAsTheVendors(t *testing.T) {
	t.Parallel()
	for _, status := range WorkingStatuses {
		cfg := mustCompany(t, "name: Acme\nchat:\n  chatlet: {typing_status: "+
			string(status)+"}\n")
		if got := cfg.Chat.Chatlet.Status(); got != status {
			t.Errorf("typing status = %q, want %q", got, status)
		}
	}
	// AND AN UNSET ONE IS `always`, the default every surface shares: a
	// reader who sees nothing cannot tell an agent working from one that is
	// dead.
	if got := mustCompany(t, "name: Acme\n").Chat.Chatlet.Status(); got != StatusAlways {
		t.Errorf("unset typing status = %q, want always", got)
	}
	err := rejects(t, "name: Acme\nchat:\n  chatlet: {typing_status: alwyas}\n",
		"chat.chatlet.typing_status")
	if !errors.Is(err, ErrUnknownValue) {
		t.Fatalf("want ErrUnknownValue, got %v", err)
	}
}

// A UNIT'S CHANNEL IS AN ADDRESS, so it has a grammar — and the three things
// an author reaches for instead are exactly what it refuses: a rendering
// (`#eng`), a vendor id (`C_ENG`, refused for its capitals), and prose.
//
// The field shipped with no validation at all and one consumer, a line in a
// prompt, so anything typed here was passed to a model as text. Chatlet
// MINTS the room this names and hands the name back to a model to type into
// a tool call, which turns each of those into a channel nobody can address.
func TestAUnitChannelIsAnAddressWithAGrammar(t *testing.T) {
	t.Parallel()
	for name, channel := range map[string]string{
		"a plain name":               "engineering",
		"a hyphenated name":          "core-engineering",
		"digits":                     "team-42",
		"a name starting in a digit": "2fa-rollout",
		// THE UNDERSCORE IS EVERY VENDOR'S: a Slack or Mattermost company
		// whose real channel is `core_engineering` names it as written.
		"an underscore":   "core_engineering",
		"the longest one": strings.Repeat("e", 64),
	} {
		t.Run(name+" is accepted", func(t *testing.T) {
			t.Parallel()
			mustCompany(t, "name: Acme\nunits:\n  - name: Core\n    channel: "+
				channel+"\n    roles:\n      - name: CTO\n")
		})
	}

	for name, channel := range map[string]string{
		"a rendered mention":     "\"#engineering\"",
		"a slack id":             "C_ENG",
		"upper case":             "Engineering",
		"a leading underscore":   "_ops",
		"prose":                  "\"core engineering\"",
		"one character too long": strings.Repeat("e", 65),
		// A PADDED NAME IS REFUSED RATHER THAN TRIMMED, unlike the unit id
		// beside it: nothing trims a channel on its way into the
		// organization, so a trim here would mint a room whose name starts
		// with a space — and the published schema carries this same pattern
		// and cannot trim, so an accepted `" ops"` would be a config the
		// engine runs and an editor underlines.
		"a padded name": "\" ops\"",
	} {
		t.Run(name+" is refused", func(t *testing.T) {
			t.Parallel()
			err := rejects(t, "name: Acme\nunits:\n  - name: Core\n    channel: "+
				channel+"\n    roles:\n      - name: CTO\n", "units[0].channel")
			if !errors.Is(err, ErrShape) {
				t.Fatalf("want ErrShape, got %v", err)
			}
		})
	}

	// AND IT IS CHECKED AT EVERY DEPTH. Most of a real company's units are
	// children, and a rule that held for a root unit and not for the
	// identical unit one level down is not a rule.
	err := rejects(t, "name: Acme\nunits:\n  - name: Core\n    channel: core\n"+
		"    children:\n      - name: Platform\n        channel: \"#platform\"\n"+
		"        roles:\n          - name: SWE\n",
		"units[0].children[0].channel")
	if !errors.Is(err, ErrShape) {
		t.Fatalf("want ErrShape for a nested unit, got %v", err)
	}
}

// TWO UNITS IN ONE ROOM ON A VENDOR SURFACE IS A WARNING BESIDE EACH OF THEM,
// because fixing it means renaming one of the two and an author who sees it
// on only one cannot tell which.
//
// A warning rather than a refusal THERE: two teams that genuinely stand up
// together in one Mattermost channel is a real company, and so is a unit split
// in the chart before its own room exists. On Chatlet the engine builds
// the room and a room belongs to one unit, so the same shape is an admission
// rule instead (unitrooms_test.go).
func TestAChannelTwoUnitsDeclareIsWarnedAboutBesideBoth(t *testing.T) {
	t.Parallel()
	c := mustCompany(t, "name: Acme\n"+mattermostBlock+"units:\n"+
		"  - name: Product\n    id: product\n    channel: delivery\n"+
		"    roles:\n      - name: PM\n"+
		"  - name: Engineering\n    id: engineering\n    channel: delivery\n"+
		"    roles:\n      - name: CTO\n")
	if got := c.ChatBackendFor(); got != ChatVendor {
		t.Fatalf("the fixture's chat is %q — the advisory is the vendor surface's", got)
	}

	var located []string
	for _, w := range channelWarnings(c) {
		if w.Kind != WarningAdvisory {
			t.Errorf("warning at %s is a %s, want an advisory", w.Path, w.Kind)
		}
		located = append(located, w.Path+" "+w.Unit)
		if !strings.Contains(w.Message, "delivery") {
			t.Errorf("warning at %s does not name the channel: %q", w.Path, w.Message)
		}
	}
	want := []string{"units[0].channel Product", "units[1].channel Engineering"}
	if strings.Join(located, ", ") != strings.Join(want, ", ") {
		t.Fatalf("warnings = %v, want one beside each unit: %v", located, want)
	}
	// AND EACH NAMES THE OTHER, which is the half a path cannot carry: a
	// warning that says "this channel is shared" without saying with what
	// sends its reader looking through the whole chart.
	for _, w := range channelWarnings(c) {
		if w.Unit == "Product" && !strings.Contains(w.Message, "Engineering") {
			t.Errorf("the warning on Product does not name Engineering: %q", w.Message)
		}
	}

	// AND A CHILD THAT INHERITS ITS PARENT'S IS NOT A COLLISION. Inheritance
	// is the feature: reading the effective channel rather than the declared
	// one would warn about every team in a division that named a room once.
	inherits := mustCompany(t, "name: Acme\n"+mattermostBlock+"units:\n"+
		"  - name: Engineering\n    id: engineering\n    channel: delivery\n"+
		"    roles:\n      - name: CTO\n"+
		"    children:\n      - name: Platform\n        id: platform\n"+
		"        roles:\n          - name: SWE\n")
	if got := channelWarnings(inherits); len(got) != 0 {
		t.Errorf("an inherited channel warned: %v", got)
	}
}

// A CHATLET COMPANY WITH NO UNIT ROOMS is the other half: the engine opens
// a channel for each one a unit declares and keeps its members to the chart,
// so a company that declares none has no room any team owns until somebody
// opens one by hand and fills it by hand.
func TestAChatletCompanyWithNoChannelsIsWarnedAbout(t *testing.T) {
	t.Parallel()
	const chart = "name: Acme\nunits:\n  - name: Product\n    id: product\n" +
		"    roles:\n      - name: PM\n" +
		"  - name: Engineering\n    id: engineering\n    roles:\n      - name: CTO\n"

	c := mustCompany(t, chart)
	warnings := channelWarnings(c)
	if len(warnings) != 1 {
		t.Fatalf("warnings = %+v, want one about the missing channels", warnings)
	}
	// ONCE FOR THE COMPANY, at the first unit's `channel`: that is the line
	// an author adds one to, and the sentence carries the half the path
	// cannot — that it is about every unit here.
	if w := warnings[0]; w.Kind != WarningAdvisory || w.Path != "units[0].channel" ||
		w.Unit != "Product" || !strings.Contains(w.Message, "no unit in this company") {
		t.Fatalf("warning = %+v, want one advisory at units[0].channel", w)
	}
	// WHAT IT SAYS IS TRUE, in both directions. A unit's room is the one
	// kind whose members the org chart keeps, and that is the whole of what
	// a company without one is missing — a person can still open a room
	// from the dashboard. The message once said nothing else opens one and
	// no team had anywhere to talk, which an author who has opened a room
	// knows is false, and an advisory that overstates is one an author
	// learns to skip.
	message := warnings[0].Message
	for _, says := range []string{"members follow the org chart", "opens one by hand"} {
		if !strings.Contains(message, says) {
			t.Errorf("warning = %q, want it to say %q", message, says)
		}
	}
	for _, overstates := range []string{"nothing else opens", "anywhere to talk"} {
		if strings.Contains(message, overstates) {
			t.Errorf("warning = %q claims %q, and a person can open a room "+
				"from the dashboard", message, overstates)
		}
	}

	// ONE DECLARED CHANNEL SILENCES IT: the company has a room, and which
	// units get their own after that is its own business.
	if got := channelWarnings(mustCompany(t, strings.Replace(chart, "  - name: Product\n",
		"  - name: Product\n    channel: product\n", 1))); len(got) != 0 {
		t.Errorf("a company with one channel warned: %v", got)
	}

	// AND A VENDOR COMPANY IS NOT WARNED, because the rooms are already
	// there: somebody made them in Mattermost, and a unit that names none
	// costs a line in a prompt rather than a place to talk.
	if got := channelWarnings(mustCompany(t, chart+mattermostBlock)); len(got) != 0 {
		t.Errorf("a vendor-chat company warned about channels: %v", got)
	}

	// AND SO IS A COMPANY WITH NO UNITS. There is no line to write the
	// channel on, and a warning pointing at nothing is one nobody can act
	// on.
	if got := channelWarnings(mustCompany(t, "name: Acme\nroles:\n  - name: CEO\n")); len(got) != 0 {
		t.Errorf("a company with no units warned about channels: %v", got)
	}
}

// channelWarnings is every warning this company carries at a unit's
// `channel` — the only place either channel advisory is written.
//
// FILTERED BY PLACE RATHER THAN COUNTED WHOLE, because these fixtures are
// companies of agents alone and other advisories are about exactly that (a
// company nobody can join is told so at `roles`); a case that counted every
// warning would be asserting the set rather than the rule it names. Anything
// at a `channel` is kept whatever it says, so a third warning written there
// still fails the counts above.
func channelWarnings(c *Company) []Warning {
	var out []Warning
	for _, w := range c.Warnings() {
		if strings.HasSuffix(w.Path, ".channel") {
			out = append(out, w)
		}
	}
	return out
}
