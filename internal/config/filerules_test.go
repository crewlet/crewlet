package config

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/org"
)

// THE THREE RULES ONLY A WHOLE FILE CAN CHECK.
//
// Each one is a fault with no run-time symptom: nothing crashes, nothing
// logs, and the company runs — wrongly, quietly, until somebody notices that
// a tool has no credentials, that a person stopped getting mail, or that a
// lead resolves to nobody. That is what makes the document the only place
// they can be caught, and what makes a test that cannot fail worse here than
// anywhere else.

// AN mcp_env KEY NAMING NO SERVER IS REFUSED, WHEREVER IT WAS WRITTEN.
func TestAnMCPEnvKeyNamingNoServerIsRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, doc, path string }{
		{
			"on a root seat",
			`
name: Acme
mcp_servers:
  - {name: tracker, command: tracker-mcp}
roles:
  - {name: CEO, handle: ceo, mcp_env: {trakcer: {TOKEN: "${T}"}}}
`,
			"roles[0].mcp_env.trakcer",
		},
		{
			"on a seat inside a unit",
			`
name: Acme
mcp_servers:
  - {name: tracker, command: tracker-mcp}
units:
  - name: Eng
    id: eng
    roles:
      - {name: Dev, handle: dev, mcp_env: {trakcer: {TOKEN: "${T}"}}}
`,
			"units[0].roles[0].mcp_env.trakcer",
		},
		{
			// THE WALK REACHES A NESTED UNIT'S OWN BLOCK, which is the half
			// a hand-rolled loop drops: a unit's mcp_env is what its whole
			// team inherits, so a typo there costs every seat under it.
			"on a nested unit",
			`
name: Acme
mcp_servers:
  - {name: tracker, command: tracker-mcp}
units:
  - name: Eng
    id: eng
    children:
      - name: Core
        id: core
        mcp_env: {trakcer: {TOKEN: "${T}"}}
`,
			"units[0].children[0].mcp_env.trakcer",
		},
		{
			// THE BRIDGE NAME IS NOT A SERVER, and it cannot become one —
			// declaring a server under it is refused where servers are
			// declared. So a block keyed on it reaches nothing.
			"the seat's own tool bridge",
			"name: Acme\nroles:\n  - {name: CEO, handle: ceo, mcp_env: {" +
				BridgeServerName + ": {TOKEN: \"${T}\"}}}\n",
			"roles[0].mcp_env." + BridgeServerName,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := rejects(t, tc.doc, tc.path)
			if !errors.Is(err, ErrUnknownValue) {
				t.Fatalf("want ErrUnknownValue, got %v", err)
			}
			// THE MESSAGE SAYS WHY NOTHING HAPPENS, because the symptom an
			// operator is chasing is a tool that will not authenticate —
			// and "unknown key" sends them to look at the tool.
			if !strings.Contains(err.Error(), "per SERVER") {
				t.Errorf("the message must say why the block is never read; got:\n%v", err)
			}
		})
	}
}

// A VENDOR BLOCK IS READ WITHOUT ANY SERVER AT ALL, so refusing it would
// refuse a company that works.
//
// # Why this is the case that matters most
//
// The rule above is a strictness, and a strictness that is one name too wide
// refuses documents the engine runs perfectly — which is a worse failure than
// the silence it was written to end, because it happens at boot, to a
// configuration somebody already deployed. Six blocks are read by the ENGINE
// directly rather than through `mcp_servers`: a Datadog company declares no
// Datadog tool server at all, and a company using the GitHub App for
// reconciliation alone declares no GitHub one.
func TestTheVendorBlocksTheEngineReadsNeedNoServer(t *testing.T) {
	t.Parallel()

	for _, block := range org.EngineReadMCPEnv() {
		t.Run(block, func(t *testing.T) {
			t.Parallel()
			// NO `mcp_servers:` AT ALL, which is the shape the rule has to
			// admit rather than merely tolerate.
			doc := "name: Acme\nroles:\n  - {name: CEO, handle: ceo, mcp_env: {" +
				block + ": {TOKEN: \"${T}\"}}}\n"
			if _, err := ParseCompany([]byte(doc)); err != nil {
				t.Fatalf("a seat holding its %s credential with no tool server "+
					"was refused, and the engine reads that block by name: %v",
					block, err)
			}
		})
	}
}

// THE VENDOR TABLE IS THE ONE EVERY READER USES.
//
// Each vendor package reads its own block through a constant, and this rule
// admits a block through [org.EngineReadMCPEnv]. If those come apart, the
// validator refuses a document the reader would have found a credential in —
// silently, from the operator's side, because the message says the name is
// not a server and the name is exactly right.
//
// THE ASSERTION IS THE TABLE'S CONTENT rather than the constants' equality,
// because they are now the same identifiers: what can still drift is a
// vendor adding a block and not adding it here.
func TestEveryBlockAVendorReadsIsInTheTable(t *testing.T) {
	t.Parallel()

	table := org.EngineReadMCPEnv()
	if !slices.IsSorted(table) {
		t.Errorf("the table is not sorted: %v — the message it feeds lists it "+
			"verbatim, and an unsorted list reorders between runs", table)
	}
	// EVERY BLOCK A READER IN THIS TREE LOOKS UP. Derived by reading the
	// callers rather than by copying the table, which is the only direction
	// that catches an omission: grep `MCPEnv[` across internal/ and every
	// key is one of these.
	for _, block := range []string{
		org.MCPEnvAtlassian, org.MCPEnvConfluence, org.MCPEnvDatadog,
		org.MCPEnvGitHub, org.MCPEnvGitLab, org.MCPEnvJira,
	} {
		if !slices.Contains(table, block) {
			t.Errorf("%q is read by a vendor package and is not in the table, "+
				"so a seat holding that credential is refused by a rule that "+
				"says the name is not a server", block)
		}
	}
	// AND NOTHING ELSE IS IN IT. A block in the table that nothing reads is
	// a hole in the rule: it admits a key that is as inert as any typo.
	if len(table) != 6 {
		t.Errorf("the table holds %d blocks, want the six the engine reads: %v",
			len(table), table)
	}
}

// TWO SEATS SHARING AN EMAIL ARE REFUSED, AND BOTH ARE NAMED.
func TestTwoSeatsMayNotDeclareOneEmail(t *testing.T) {
	t.Parallel()

	doc := `
name: Acme
roles:
  - {name: CEO, handle: ceo, email: Ada@example.com}
units:
  - name: Eng
    id: eng
    roles:
      - {name: Dev, handle: dev, email: ada@example.com}
`
	err := rejects(t, doc, "roles[0].email")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}

	// REPORTED AT EVERY SEAT THAT HOLDS IT, because an operator fixing this
	// has to choose which seat keeps the address and a message naming one
	// of the two tells them nothing about the other.
	if !strings.Contains(err.Error(), "units[0].roles[0].email") {
		t.Errorf("the second seat is not reported; got:\n%v", err)
	}
	for _, name := range []string{"CEO", "Dev"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the message does not name the seat %q; got:\n%v", name, err)
		}
	}

	// FOLDED EXACTLY AS THE REGISTRY FOLDS IT. `Ada@` and `ada@` are one
	// address to the index that drops the second seat, so a rule comparing
	// them as different values would pass a document the registry then
	// collapses.
	if !strings.Contains(err.Error(), "ada@example.com") {
		t.Errorf("the message must name the folded address; got:\n%v", err)
	}
}

// AND TWO SEATS WITH NO EMAIL DO NOT COLLIDE.
//
// The rule every duplicate check in this tree follows: an identity that is
// missing is not a shared one. Without it a company where nobody set an
// address — the ordinary case — would be refused entirely.
func TestSeatsWithNoEmailDoNotCollide(t *testing.T) {
	t.Parallel()
	mustCompany(t, `
name: Acme
roles:
  - {name: CEO, handle: ceo}
  - {name: CTO, handle: cto, email: "  "}
  - {name: Dev, handle: dev}
`)
}

// A REFERENCE SHAPED LIKE A DISPLAY NAME IS REFUSED, AT EVERY FIELD THAT
// TAKES ONE.
func TestAReferenceThatIsNeitherAHandleNorAKeyIsRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, doc, path, want string }{
		{
			"a unit's lead",
			"name: Acme\nunits:\n  - {name: Eng, id: eng, lead: QA Lead}\n",
			"units[0].lead", "names the seat's HANDLE",
		},
		{
			"a nested unit's lead",
			"name: Acme\nunits:\n  - name: Eng\n    id: eng\n    children:\n      - {name: Core, id: core, lead: QA Lead}\n",
			"units[0].children[0].lead", "names the seat's HANDLE",
		},
		{
			"a root seat's unit",
			"name: Acme\nroles:\n  - {name: Dev, handle: dev, unit: Engineering Platform}\n",
			"roles[0].unit", "joins its unit by KEY",
		},
		{
			"a manages entry",
			"name: Acme\nroles:\n  - {name: CEO, handle: ceo, manages: [QA Lead]}\n",
			"roles[0].manages[0]", "neither a seat handle nor a unit key",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := rejects(t, tc.doc, tc.path)
			if !errors.Is(err, ErrShape) {
				t.Fatalf("want ErrShape, got %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the message must say what the field takes; got:\n%v", err)
			}
		})
	}
}

// A WELL-FORMED REFERENCE THAT RESOLVES TO NOTHING IS STILL ACCEPTED.
//
// THE POINT OF THE RULE ABOVE, stated from the other side. A chart is
// assembled in pieces and every intermediate state is one every node applies,
// so a `lead:` naming a seat nobody has hired yet must not refuse the
// document — it is a dangling reference, reported by
// [Company.ReferenceWarnings], exactly as it was before.
//
// A `manages:` entry takes EITHER a handle or a unit key, which is what makes
// `manages: [engineering]` expand to a division, so both spellings are
// exercised here.
func TestAWellFormedReferenceThatResolvesToNothingIsAccepted(t *testing.T) {
	t.Parallel()

	cfg := mustCompany(t, `
name: Acme
roles:
  - {name: CEO, handle: ceo, manages: [not-yet-hired, some_unit], unit: no_such_unit}
units:
  - {name: Eng, id: eng, lead: not-yet-hired}
`)
	if len(cfg.DanglingRefs()) == 0 {
		t.Fatal("none of the four references was reported as dangling, so this " +
			"case would pass for a build that refused them instead")
	}
}
