package config

import (
	"errors"
	"strings"
	"testing"
	"time"
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
func TestTheChatBackendDerivesFromTheVendorPresent(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		doc  string
		want ChatBackend
	}{
		"a company that configures nothing talks natively": {
			"name: Acme\n", ChatNative,
		},
		"an enabled mattermost block is a vendor surface": {
			"name: Acme\n" + mattermostBlock, ChatVendor,
		},
		// A BLOCK THAT IS NOT ENABLED STILL COUNTS, because the derivation
		// asks where this company's conversations live rather than whether
		// the transport is running today: switching a paused Mattermost
		// company to native chat behind its operator's back is how a team
		// comes back to two histories.
		"a mattermost block with nothing in it is a vendor surface": {
			"name: Acme\nintegrations:\n  mattermost: {}\n", ChatVendor,
		},
		"an org-level slack block is a vendor surface": {
			"name: Acme\nintegrations:\n  slack: {typing_status: addressed}\n", ChatVendor,
		},
		// SLACK IS THE ONE THAT HIDES. Every credential is per seat, and the
		// org-level block holds working-indicator settings a company may
		// never write — so a company with seven working Slack apps and no
		// `integrations.slack` is a vendor company, and asking the block
		// alone would derive native chat for it.
		"a seat's own slack app is a vendor surface with no org block at all": {
			"name: Acme\nroles:\n  - name: CEO\n    integrations:\n" +
				"      slack: {bot_token: \"${T}\", signing_secret: \"${S}\"}\n",
			ChatVendor,
		},
		"a seat's slack app inside a unit counts too": {
			"name: Acme\nunits:\n  - name: Core\n    channel: core\n    roles:\n" +
				"      - name: CTO\n        integrations:\n" +
				"          slack: {bot_token: \"${T}\", signing_secret: \"${S}\"}\n",
			ChatVendor,
		},
		"a named backend wins over the derivation": {
			"name: Acme\nchat: {backend: none}\n" + mattermostBlock, ChatNone,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := mustCompany(t, tc.doc)
			if got := cfg.ChatBackendFor(); got != tc.want {
				t.Errorf("chat = %q, want %q", got, tc.want)
			}
		})
	}
}

// EACH REFUSAL NAMES THE FIELD AN AUTHOR HAS TO EDIT. A conflict reported
// without a path is one an operator searches their file for by guessing.
func TestTheChatAxisRefusesWhatItCannotResolve(t *testing.T) {
	t.Parallel()

	// A BACKEND BESIDE ITS VENDOR IS THE MIRROR THE DOCTRINE FORBIDS, and
	// chat is where it cannot be repaired afterwards: a person replied in
	// one surface and the agents are reading the other.
	t.Run("native beside a vendor chat surface is refused", func(t *testing.T) {
		t.Parallel()
		err := rejects(t, "name: Acme\nchat: {backend: native}\n"+mattermostBlock,
			"chat.backend")
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("want ErrConflict, got %v", err)
		}
		for _, want := range []string{"integrations.mattermost", "in step", "Remove one"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal = %q, want it to say %q", err, want)
			}
		}
		// AND A SEAT'S OWN SLACK APP IS A VENDOR SURFACE HERE TOO, named
		// where it was written: a refusal pointing at `integrations.slack`
		// would send an author to a block their document does not have.
		err = rejects(t, "name: Acme\nchat: {backend: native}\nroles:\n"+
			"  - name: CEO\n    integrations:\n"+
			"      slack: {bot_token: \"${T}\", signing_secret: \"${S}\"}\n",
			"chat.backend")
		if !strings.Contains(err.Error(), "roles[0].integrations.slack") {
			t.Errorf("refusal = %q, want it to name the seat's own app", err)
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

	// A NATIVE POLICY BLOCK ON A COMPANY THAT IS NOT NATIVE describes
	// nothing, and the failure it produces is silence: a horizon no message
	// this company holds will ever be measured against.
	t.Run("a native block on a vendor company is refused", func(t *testing.T) {
		t.Parallel()
		err := rejects(t, "name: Acme\nchat:\n  native:\n    message_retention_days: 90\n"+
			mattermostBlock, "chat.native")
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
		// THE SET IS PRINTED, because "invalid value" leaves an author
		// guessing at exactly the moment they have already guessed once.
		for _, want := range strs(ChatBackends) {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal = %q, want it to offer %q", err, want)
			}
		}
	})

	// AND A NATIVE BLOCK ON A NATIVE COMPANY IS ORDINARY, including on the
	// company that named no backend at all — refusing the default
	// configuration would be the opposite of the intent.
	t.Run("a native block on a company that named nothing is accepted", func(t *testing.T) {
		t.Parallel()
		cfg := mustCompany(t, "name: Acme\nchat:\n  native:\n"+
			"    typing_status: addressed\n    thread_context_messages: 25\n")
		if got := cfg.ChatBackendFor(); got != ChatNative {
			t.Errorf("chat = %q, want native", got)
		}
		if got := cfg.Chat.Native.Status(); got != StatusAddressed {
			t.Errorf("typing status = %q, want addressed", got)
		}
		if got := cfg.Chat.Native.ThreadContext(); got != 25 {
			t.Errorf("thread context = %d, want 25", got)
		}
	})
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
		// A NATIVE BLOCK THAT SAYS NOTHING ABOUT THE HORIZON is still an
		// absent horizon: the field is three-valued on its own, not on
		// whether the block around it exists.
		"a native block that names other settings takes the default": {
			"name: Acme\nchat:\n  native: {typing_status: addressed}\n", year, true, false,
		},
		"zero is for ever": {
			"name: Acme\nchat:\n  native: {message_retention_days: 0}\n", 0, false, true,
		},
		"a number is that many days": {
			"name: Acme\nchat:\n  native: {message_retention_days: 30}\n",
			30 * 24 * time.Hour, true, true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := mustCompany(t, tc.doc)
			// THE FIELD ITSELF IS THREE-VALUED, and the accessor's answer
			// is derived from it: a test that only read the duration would
			// pass against a plain int that defaulted zero away.
			declared := cfg.Chat.Native != nil && cfg.Chat.Native.MessageRetentionDays != nil
			if declared != tc.declared {
				t.Errorf("declared = %v, want %v", declared, tc.declared)
			}
			got, bounded := cfg.Chat.Native.MessageRetention()
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
		"a fortnight":   "name: Acme\nchat:\n  native: {message_retention_days: 14}\n",
		"a century":     "name: Acme\nchat:\n  native: {message_retention_days: 36500}\n",
		"a negative":    "name: Acme\nchat:\n  native: {message_retention_days: -1}\n",
		"one day short": "name: Acme\nchat:\n  native: {message_retention_days: 29}\n",
	} {
		t.Run(name+" is refused", func(t *testing.T) {
			t.Parallel()
			err := rejects(t, doc, "chat.native.message_retention_days")
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

// THE THREAD WINDOW'S ZERO IS THE OTHER READING, deliberately: it takes the
// default, because there is no "no context" setting for it to collide with.
// A seat handed a reply with none of the thread it replies to cannot answer
// it — and would answer anyway — so this engine does not offer zero, and a
// company spending fewer tokens writes a smaller number.
func TestTheThreadWindowDefaultsAndIsBounded(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		doc  string
		want int
	}{
		"unwritten takes the default": {"name: Acme\n", DefaultThreadContextMessages},
		"zero takes the default": {
			"name: Acme\nchat:\n  native: {thread_context_messages: 0}\n",
			DefaultThreadContextMessages,
		},
		"a number is that many": {
			"name: Acme\nchat:\n  native: {thread_context_messages: 3}\n", 3,
		},
		"the ceiling is allowed": {
			"name: Acme\nchat:\n  native: {thread_context_messages: 50}\n",
			MaxThreadContextMessages,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := mustCompany(t, tc.doc).Chat.Native.ThreadContext(); got != tc.want {
				t.Errorf("thread context = %d, want %d", got, tc.want)
			}
		})
	}

	err := rejects(t, "name: Acme\nchat:\n  native: {thread_context_messages: 51}\n",
		"chat.native.thread_context_messages")
	if !errors.Is(err, ErrOutOfRange) {
		t.Fatalf("want ErrOutOfRange, got %v", err)
	}
}

// THE TYPING STATUS IS THE VENDOR BLOCKS' OWN ENUM, and this is what says so:
// a value one of them accepts is a value this one accepts, and a misspelling
// is refused here exactly as it is there rather than degrading silently to
// the default.
func TestNativeChatReadsTheSameWorkingStatusAsTheVendors(t *testing.T) {
	t.Parallel()
	for _, status := range WorkingStatuses {
		cfg := mustCompany(t, "name: Acme\nchat:\n  native: {typing_status: "+
			string(status)+"}\n")
		if got := cfg.Chat.Native.Status(); got != status {
			t.Errorf("typing status = %q, want %q", got, status)
		}
	}
	// AND AN UNSET ONE IS `always`, the default every surface shares: a
	// reader who sees nothing cannot tell an agent working from one that is
	// dead.
	if got := mustCompany(t, "name: Acme\n").Chat.Native.Status(); got != StatusAlways {
		t.Errorf("unset typing status = %q, want always", got)
	}
	err := rejects(t, "name: Acme\nchat:\n  native: {typing_status: alwyas}\n",
		"chat.native.typing_status")
	if !errors.Is(err, ErrUnknownValue) {
		t.Fatalf("want ErrUnknownValue, got %v", err)
	}
}

// A UNIT'S CHANNEL IS AN ADDRESS, so it has a grammar — and the three things
// an author reaches for instead are exactly what it refuses: a rendering
// (`#eng`), a vendor id (`C_ENG`), and prose.
//
// The field shipped with no validation at all and one consumer, a line in a
// prompt, so anything typed here was passed to a model as text. Native chat
// MINTS the room this names and hands the name back to a model to type into
// a tool call, which turns each of those into a channel nobody can address.
func TestAUnitChannelIsAnAddressWithAGrammar(t *testing.T) {
	t.Parallel()
	for name, channel := range map[string]string{
		"a plain name":               "engineering",
		"a hyphenated name":          "core-engineering",
		"digits":                     "team-42",
		"a name starting in a digit": "2fa-rollout",
		"the longest one":            strings.Repeat("e", 64),
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
		"an underscore":          "core_engineering",
		"upper case":             "Engineering",
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

// TWO UNITS IN ONE ROOM IS A WARNING BESIDE EACH OF THEM, because fixing it
// means renaming one of the two and an author who sees it on only one cannot
// tell which.
//
// A warning rather than a refusal: two teams that genuinely stand up together
// is a real company, and so is a unit split in the chart before its own room
// exists.
func TestAChannelTwoUnitsDeclareIsWarnedAboutBesideBoth(t *testing.T) {
	t.Parallel()
	c := mustCompany(t, "name: Acme\nunits:\n"+
		"  - name: Product\n    id: product\n    channel: delivery\n"+
		"    roles:\n      - name: PM\n"+
		"  - name: Engineering\n    id: engineering\n    channel: delivery\n"+
		"    roles:\n      - name: CTO\n")

	var located []string
	for _, w := range c.Warnings() {
		if w.Kind != WarningAdvisory {
			continue
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
	for _, w := range c.Warnings() {
		if w.Unit == "Product" && !strings.Contains(w.Message, "Engineering") {
			t.Errorf("the warning on Product does not name Engineering: %q", w.Message)
		}
	}

	// AND A CHILD THAT INHERITS ITS PARENT'S IS NOT A COLLISION. Inheritance
	// is the feature: reading the effective channel rather than the declared
	// one would warn about every team in a division that named a room once.
	inherits := mustCompany(t, "name: Acme\nunits:\n"+
		"  - name: Engineering\n    id: engineering\n    channel: delivery\n"+
		"    roles:\n      - name: CTO\n"+
		"    children:\n      - name: Platform\n        id: platform\n"+
		"        roles:\n          - name: SWE\n")
	if got := inherits.Warnings(); len(got) != 0 {
		t.Errorf("an inherited channel warned: %v", got)
	}
}

// A NATIVE-CHAT COMPANY WITH NO ROOMS AT ALL is the other half: the engine
// opens a channel for each one a unit declares and nothing else opens one, so
// a company that declares none has nowhere for a team to talk to itself.
func TestANativeChatCompanyWithNoChannelsIsWarnedAbout(t *testing.T) {
	t.Parallel()
	const chart = "name: Acme\nunits:\n  - name: Product\n    id: product\n" +
		"    roles:\n      - name: PM\n" +
		"  - name: Engineering\n    id: engineering\n    roles:\n      - name: CTO\n"

	c := mustCompany(t, chart)
	warnings := c.Warnings()
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

	// ONE DECLARED CHANNEL SILENCES IT: the company has a room, and which
	// units get their own after that is its own business.
	if got := mustCompany(t, strings.Replace(chart, "  - name: Product\n",
		"  - name: Product\n    channel: product\n", 1)).Warnings(); len(got) != 0 {
		t.Errorf("a company with one channel warned: %v", got)
	}

	// AND A VENDOR COMPANY IS NOT WARNED, because the rooms are already
	// there: somebody made them in Mattermost, and a unit that names none
	// costs a line in a prompt rather than a place to talk.
	if got := mustCompany(t, chart+mattermostBlock).Warnings(); len(got) != 0 {
		t.Errorf("a vendor-chat company warned about channels: %v", got)
	}

	// AND SO IS A COMPANY WITH NO UNITS. There is no line to write the
	// channel on, and a warning pointing at nothing is one nobody can act
	// on.
	if got := mustCompany(t, "name: Acme\nroles:\n  - name: CEO\n").Warnings(); len(got) != 0 {
		t.Errorf("a company with no units warned about channels: %v", got)
	}
}
