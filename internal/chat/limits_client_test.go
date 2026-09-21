package chat_test

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/clientsource"
)

// TestTheDashboardsChatLimitsAreTheEngines holds the five bounds the chat
// screens enforce — in the composer, the name field, the room settings and the
// participant picker — against the ones this package actually refuses on.
//
// THE DASHBOARD CANNOT IMPORT A GO CONSTANT, so where a screen must refuse
// early it carries its own copy, and every copy of an engine-owned value in
// this tree has drifted at least once (see [clientsource]). This pair drifts
// SILENTLY IN BOTH DIRECTIONS, and neither direction looks like a bug from the
// side that sees it:
//
//   - the client's copy too LOW greys out `send` on a message the engine would
//     have taken. Nothing is logged and nothing is refused, so the person
//     concludes the limit is whatever the screen says it is.
//   - the client's copy too HIGH accepts what the write path then rejects, so
//     the refusal arrives after the typing rather than during it — which for a
//     32 KiB body is the one case where losing the draft costs something.
//
// The engine side is where the check belongs, because the engine owns the
// value: the screen's copy is a convenience and this package's constant is the
// rule.
//
// KEYED ON THE DECLARATION rather than on a path, so moving a screen is
// invisible and the two failures reported are the two that matter — nothing
// declares it (a gate certifying nothing) and two files declare it (two copies
// free to drift from each other as well as from here).
//
// MAX_BROWSE_ROOMS is deliberately NOT here. It is how many rooms the browse
// dialog offers at once, which is a decision about a list somebody scans and
// has no twin in this package — a constant with no engine owner is one this
// gate would be inventing an authority for.
func TestTheDashboardsChatLimitsAreTheEngines(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		client string
		engine int
	}{
		{"MAX_CHANNEL_NAME", chat.MaxChannelName},
		{"MAX_BODY_BYTES", chat.MaxBody},
		{"MAX_TOPIC_BYTES", chat.MaxTopic},
		{"MAX_PURPOSE_BYTES", chat.MaxPurpose},
		{"MAX_DIRECT_PARTICIPANTS", chat.MaxDMParticipants},
	} {
		t.Run(c.client, func(t *testing.T) {
			t.Parallel()
			// THE RHS IS CAPTURED WHOLE rather than as a bare
			// literal: a screen is free to write a byte bound as
			// `32 * 1024`, and a pattern matching only digits
			// would report "nothing declares it" — the failure
			// that reads as a missing constant rather than as a
			// gate too narrow to see the one that is there.
			body, err := clientsource.Declaration(clientsource.Tree,
				`export const `+c.client+` = ([^;]+);`)
			if err != nil {
				t.Fatal(err)
			}
			got, err := evalByteBound(body)
			if err != nil {
				t.Fatalf("%s is declared as %q, which this gate cannot "+
					"evaluate: %v. Write it as a literal or a product of "+
					"literals, or the gate stops holding it against %d",
					c.client, body, err, c.engine)
			}
			if got != c.engine {
				t.Errorf("the dashboard enforces %s = %d and this package "+
					"refuses at %d. Too low silently greys out a gesture the "+
					"engine would take; too high moves the refusal from the "+
					"form to the write path. The engine owns the value, so "+
					"the screen is what changes", c.client, got, c.engine)
			}
		})
	}
}

// evalByteBound reads the small arithmetic a byte bound is written with.
//
// A LITERAL, OR A PRODUCT OF LITERALS, and nothing else. `32 * 1024` is how a
// kibibyte reads in the client, and asserting against the digits `32768`
// instead would fail the gate on a change that altered only the spelling.
// Anything past a product is REFUSED rather than guessed at, because a gate
// that silently mis-evaluates an expression reports an agreement it never
// checked.
func evalByteBound(src string) (int, error) {
	product := 1
	for _, term := range strings.Split(src, "*") {
		n, err := strconv.Atoi(strings.TrimSpace(term))
		if err != nil {
			return 0, fmt.Errorf("term %q is not an integer: %w", term, err)
		}
		product *= n
	}
	return product, nil
}

// TestTheDashboardsChannelNameGrammarIsTheEngines holds the room-name rule the
// create dialog refuses on against [chat.ValidName].
//
// THE PATTERN IS THE FRAGILE HALF OF THIS MIRROR. A number that drifts is
// wrong by an amount somebody notices; a character class that drifts is wrong
// for one input in a thousand, and the direction decides who finds out:
//
//   - the client STRICTER than the engine makes a legal name unreachable from
//     this dashboard, with nothing anywhere to say so. `name.ts` names that as
//     the failure it must never have, and nothing was holding it to that.
//   - the client LOOSER costs a round trip, which is the cheap direction and
//     still not free: the point of the copy is that somebody learns while the
//     caret is in the field.
//
// COMPARED BY BEHAVIOUR RATHER THAN BY SPELLING. Two engines' regex dialects
// can write one rule two ways, so asserting the source text matched would fail
// on a change that altered nothing — and asserting it did NOT match would pass
// a rule that reads the same and decides differently. What both sides owe is
// the same ANSWER, so the probes are the edges of the rule: the first
// character's narrower class, the length ceiling, the case folding the
// normaliser is supposed to have already done, and the characters a subject
// token cannot carry.
func TestTheDashboardsChannelNameGrammarIsTheEngines(t *testing.T) {
	t.Parallel()

	// NOT `export`ed in the client, so the pattern must not require it:
	// a gate that silently matched nothing is the failure [clientsource]
	// was written after.
	src, err := clientsource.Declaration(clientsource.Tree, `const NAME = /(.+)/;`)
	if err != nil {
		t.Fatal(err)
	}
	client, err := regexp.Compile(src)
	if err != nil {
		t.Fatalf("the dashboard's channel-name pattern %q does not compile "+
			"here, so this gate cannot hold it against the engine: %v", src, err)
	}

	for _, probe := range []string{
		"", "a", "0", "launch", "product-launch", "a-", "-launch", "1launch",
		"Launch", "product launch", "product_launch", "product.launch",
		"prodüct", "product#launch", "#launch", "launch/1", "launch*",
		strings.Repeat("a", 64), strings.Repeat("a", 65),
		strings.Repeat("a", 63) + "-", "a" + strings.Repeat("-", 63),
	} {
		engine, screen := chat.ValidName(probe), client.MatchString(probe)
		if engine == screen {
			continue
		}
		verdict := "the engine refuses it and the create dialog offers it, so " +
			"the refusal arrives from the server instead of from the field"
		if engine {
			verdict = "the engine ACCEPTS it and the create dialog refuses it, " +
				"which makes a legal room name unreachable from this dashboard " +
				"with nothing anywhere to say so"
		}
		t.Errorf("channel name %q (%d bytes): %s. The engine owns the grammar "+
			"(chat.ValidName); the screen's copy is what changes",
			probe, len(probe), verdict)
	}
}
