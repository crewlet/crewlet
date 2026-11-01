package config

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/org"
)

// THE FIVE RULES ONLY A WHOLE FILE CAN CHECK.
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
				ToolServerName + ": {TOKEN: \"${T}\"}}}\n",
			"roles[0].mcp_env." + ToolServerName,
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

// A BOX ENTRY NAMING NO SERVER IS REFUSED, a vendor's name included.
//
// An executor box is given each listed server the company declares, and an
// entry naming none gives it nothing but a log line on every launch. A vendor's
// name is no exception: the engine reads a seat's `mcp_env.github` without a
// server, but a box is only ever given a declared one. A server turned off is
// declared, so listing it stays legal.
//
// Mutation: skip the sandbox field in the rule, and the first two cases pass
// validation.
func TestABoxEntryNamingNoServerIsRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, entry, path string }{
		{"a misspelt server", "trakcer", "roles[0].sandbox.mcp.servers[1]"},
		{"a vendor block's name", org.MCPEnvGitHub, "roles[0].sandbox.mcp.servers[1]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := rejects(t, `
name: Acme
mcp_servers:
  - {name: tracker, command: tracker-mcp}
roles:
  - name: Dev
    handle: dev
    sandbox: {mcp: {servers: [tracker, `+tc.entry+`]}}
`, tc.path)
			if !errors.Is(err, ErrUnknownValue) {
				t.Fatalf("want ErrUnknownValue, got %v", err)
			}
			if !strings.Contains(err.Error(), "mcp_servers has tracker") ||
				!strings.Contains(err.Error(), "gives the seat's executor nothing") {
				t.Errorf("the message must list the declared servers and say what the "+
					"entry does; got:\n%v", err)
			}
		})
	}
	mustCompany(t, `
name: Acme
mcp_servers:
  - {name: tracker, command: tracker-mcp, enabled: false}
units:
  - name: Eng
    id: eng
    roles:
      - name: Dev
        handle: dev
        sandbox: {mcp: {servers: [tracker]}}
`)
}

// AN optional_keys ENTRY THAT LEAVES NOTHING OUT IS REFUSED, and says which
// mistake it is.
//
// An entry naming nothing is never consulted, so the key it was meant for
// stays required and the server is not started wherever that key's value
// resolves to nothing — on a document that reads as though somebody said it
// could run without it. The likeliest slip is the VARIABLE's name where the
// key's belongs, which is told apart because its fix is the key it names.
func TestAnOptionalKeyThatLeavesNothingOutIsRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, doc, path string
		kind            error
		want            []string
	}{
		{
			"a variable named where its key belongs",
			`
name: Acme
mcp_servers:
  - name: search
    command: search-mcp
    env: {REGISTRY: "${NPM_CONFIG_REGISTRY}"}
    optional_keys: [NPM_CONFIG_REGISTRY]
`,
			"mcp_servers[0].optional_keys[0]", ErrUnknownValue,
			[]string{"is a variable", "not a key", "(REGISTRY)"},
		},
		{
			"a key nothing carries",
			`
name: Acme
mcp_servers:
  - name: search
    command: search-mcp
    env: {NPM_CONFIG_REGISTRY: "${NPM_CONFIG_REGISTRY}"}
    optional_keys: [NPM_CONFIG_REGISTRI]
`,
			"mcp_servers[0].optional_keys[0]", ErrUnknownValue,
			[]string{"is not a key of this server's env", "stays required"},
		},
		{
			"a header nothing carries, on an http server",
			`
name: Acme
mcp_servers:
  - name: docs
    transport: http
    url: "https://docs.example.com/mcp"
    headers: {X-Region: "${DOCS_REGION}"}
    optional_keys: [X-Regoin]
`,
			"mcp_servers[0].optional_keys[0]", ErrUnknownValue,
			[]string{"is not a key of this server's headers"},
		},
		{
			"a variable only the url references",
			`
name: Acme
mcp_servers:
  - name: docs
    transport: http
    url: "https://${DOCS_HOST}/mcp"
    headers: {X-Region: "${DOCS_REGION}"}
    optional_keys: [DOCS_HOST]
`,
			"mcp_servers[0].optional_keys[0]", ErrUnknownValue,
			[]string{"the url references", "the url is never optional"},
		},
		{
			// ONLY THIS SERVER'S BLOCKS: the seat's block for another
			// server lands in that server's environment, never this one's.
			"a key only another server's seat block writes",
			`
name: Acme
mcp_servers:
  - {name: tracker, shared: false, command: tracker-mcp, optional_keys: [REGION]}
  - {name: wiki, shared: false, command: wiki-mcp}
roles:
  - name: Dev
    handle: dev
    mcp_env:
      tracker: {TOKEN: "${DEV_TRACKER_TOKEN}"}
      wiki: {REGION: "${DEV_WIKI_REGION}"}
`,
			"mcp_servers[0].optional_keys[0]", ErrUnknownValue,
			[]string{"is not a key of this server's env"},
		},
		{
			"one header listed twice in two spellings",
			`
name: Acme
mcp_servers:
  - name: docs
    transport: http
    url: "https://docs.example.com/mcp"
    headers: {X-Region: "${DOCS_REGION}"}
    optional_keys: [X-Region, x-region]
`,
			"mcp_servers[0].optional_keys[1]", ErrConflict,
			[]string{"listed twice"},
		},
		{
			"a key whose values reference nothing",
			`
name: Acme
mcp_servers:
  - name: code
    command: glab
    env: {GLAB_CHECK_UPDATE: "false"}
    optional_keys: [GLAB_CHECK_UPDATE]
`,
			"mcp_servers[0].optional_keys[0]", ErrUnknownValue,
			[]string{"carries a ${VAR} reference", "take it out of optional_keys"},
		},
		{
			"a key listed twice",
			`
name: Acme
mcp_servers:
  - name: search
    command: search-mcp
    env: {NPM_CONFIG_REGISTRY: "${NPM_CONFIG_REGISTRY}"}
    optional_keys: [NPM_CONFIG_REGISTRY, NPM_CONFIG_REGISTRY]
`,
			"mcp_servers[0].optional_keys[1]", ErrConflict,
			[]string{"listed twice"},
		},
		{
			"an empty entry",
			`
name: Acme
mcp_servers:
  - name: search
    command: search-mcp
    env: {NPM_CONFIG_REGISTRY: "${NPM_CONFIG_REGISTRY}"}
    optional_keys: [" "]
`,
			"mcp_servers[0].optional_keys[0]", ErrMissing,
			[]string{"names the key"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := rejects(t, tc.doc, tc.path)
			if !errors.Is(err, tc.kind) {
				t.Fatalf("want %v, got %v", tc.kind, err)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the message must say %q; got:\n%v", want, err)
				}
			}
			// AN ADMISSION RULE: a stored revision carrying it still runs,
			// since the engine merely never consults the entry.
			cfg, perr := ParseCompanyDocument([]byte(tc.doc))
			if perr != nil {
				t.Fatalf("decode: %v", perr)
			}
			if rerr := cfg.ValidateRunnable(); rerr != nil {
				t.Errorf("a stored revision carrying it was refused to run: %v", rerr)
			}
		})
	}
}

// AN OPTIONAL KEY ONLY A SEAT OR A UNIT WRITES IS A KEY THE SERVER READS, so
// it is accepted.
//
// A seat's block for a server lands in the same environment as the server's
// own env, and a unit's block in every agent seat under it, so a per-seat
// template that carries no env of its own is the ordinary place for such a
// key. A rule that looked at the server alone would refuse it.
func TestAnOptionalKeyOnlyASeatOrAUnitWritesIsAccepted(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, doc string }{
		{"a seat inside a unit", `
name: Acme
mcp_servers:
  - {name: tracker, shared: false, command: tracker-mcp, optional_keys: [TRACKER_REGION]}
units:
  - name: Eng
    id: eng
    roles:
      - name: Dev
        handle: dev
        mcp_env: {tracker: {TOKEN: "${DEV_TOKEN}", TRACKER_REGION: "${DEV_REGION}"}}
`},
		{"a nested unit", `
name: Acme
mcp_servers:
  - {name: tracker, shared: false, command: tracker-mcp, optional_keys: [TRACKER_REGION]}
units:
  - name: Eng
    id: eng
    children:
      - name: Core
        id: core
        mcp_env: {tracker: {TRACKER_REGION: "${CORE_REGION}"}}
`},
		{"the server's own env", `
name: Acme
mcp_servers:
  - name: search
    command: search-mcp
    env: {NPM_CONFIG_REGISTRY: "${NPM_CONFIG_REGISTRY}"}
    optional_keys: [NPM_CONFIG_REGISTRY]
`},
		{"an http server's own header", `
name: Acme
mcp_servers:
  - name: docs
    transport: http
    url: "https://docs.example.com/mcp"
    headers: {X-Region: "${DOCS_REGION}"}
    optional_keys: [X-Region]
`},
		// HTTP NAMES A HEADER IN ANY CASE, and so does the list.
		{"a header named in another case than it is written", `
name: Acme
mcp_servers:
  - name: docs
    transport: http
    url: "https://docs.example.com/mcp"
    headers: {X-Region: "${DOCS_REGION}"}
    optional_keys: [x-region]
`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mustCompany(t, tc.doc)
		})
	}
}

// ONE HEADER SPELLED TWO WAYS IN ONE PLACE IS REFUSED, and only there.
//
// HTTP does not tell `authorization` from `Authorization`, so a server is sent
// one value for the two. The engine lays a seat's values over its template's
// by the header rather than by the spelling, so those two may differ; but two
// keys of one map, or a unit's block and a member's — which the org chart
// lays one under the other as written, keeping both — leave nothing to say
// which value is sent, and the request carried whichever came last out of a
// map. Seats are started apart, and an environment does tell the two
// spellings apart, so neither is refused.
func TestOneHeaderSpelledTwoWaysInOnePlaceIsRefused(t *testing.T) {
	t.Parallel()

	const server = `
name: Acme
mcp_servers:
  - name: code
    transport: http
    shared: false
    url: "https://code.example.com/mcp"
`
	for _, tc := range []struct{ name, doc, path, want string }{
		{"in the server's own headers", `
name: Acme
mcp_servers:
  - name: code
    transport: http
    url: "https://code.example.com/mcp"
    headers: {Authorization: "Bearer ${A}", authorization: "Bearer ${B}"}
`, "mcp_servers[0].headers.authorization", `"authorization" and "Authorization" name one header`},
		{"in one seat's block", server + `
roles:
  - name: Dev
    handle: dev
    mcp_env: {code: {X-Token: "${DEV_A}", x-token: "${DEV_B}"}}
`, "roles[0].mcp_env.code.x-token", `"x-token" and "X-Token" name one header`},
		{"between a unit's block and a member's", server + `
units:
  - name: Eng
    id: eng
    mcp_env: {code: {authorization: "Bearer ${ENG_TOKEN}"}}
    roles:
      - name: Dev
        handle: dev
        mcp_env: {code: {Authorization: "Bearer ${DEV_TOKEN}"}}
`, "units[0].roles[0].mcp_env.code.Authorization", `its unit's block writes as "authorization"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := rejects(t, tc.doc, tc.path)
			if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want a conflict saying %q, got:\n%v", tc.want, err)
			}
		})
	}

	for _, tc := range []struct{ name, doc string }{
		{"a seat's header over its template's", `
name: Acme
mcp_servers:
  - name: code
    transport: http
    shared: false
    url: "https://code.example.com/mcp"
    headers: {Authorization: "Bearer ${SHARED_TOKEN}"}
roles:
  - name: Dev
    handle: dev
    mcp_env: {code: {authorization: "Bearer ${DEV_TOKEN}"}}
`},
		{"two seats spelling one header apart", server + `
roles:
  - name: Dev
    handle: dev
    mcp_env: {code: {authorization: "Bearer ${DEV_TOKEN}"}}
  - name: Ops
    handle: ops
    mcp_env: {code: {Authorization: "Bearer ${OPS_TOKEN}"}}
`},
		{"a stdio server's two variables", `
name: Acme
mcp_servers:
  - {name: search, shared: false, command: search-mcp}
roles:
  - name: Dev
    handle: dev
    mcp_env: {search: {TOKEN: "${DEV_A}", token: "${DEV_B}"}}
`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mustCompany(t, tc.doc)
		})
	}
}

// AN EXECUTOR BOX'S optional_keys ARE HELD TO THE SAME RULE: a seat's sandbox
// block's over its env, and a setup step's over its env and its files' paths,
// wherever the step is declared.
//
// A file's body is a reference only when it is one whole ${VAR}: any other
// `${…}` in it is the file's own syntax, expanded inside the box, so a script
// full of `${HOME}` has nothing the engine could find missing.
func TestARunsOptionalKeyThatLeavesNothingOutIsRefused(t *testing.T) {
	t.Parallel()
	const head = `
name: Acme
providers:
  sandbox:
    fake: true
`
	for _, tc := range []struct{ name, doc, path, want string }{
		{"a seat block naming a variable", head + `
roles:
  - name: Dev
    handle: dev
    sandbox:
      env: {GITLAB_TOKEN: "${DEV_GITLAB_TOKEN}"}
      optional_keys: [DEV_GITLAB_TOKEN]
`, "roles[0].sandbox.optional_keys[0]", "(GITLAB_TOKEN)"},
		{"a provider-wide step naming no file of its own", head + `
    setup:
      - name: npm
        files: {.npmrc: "${NPMRC}"}
        optional_keys: [~/.npmrc]
`, "providers.sandbox.setup[0].optional_keys[0]", "is not a key of this step's env, nor a path of its files"},
		{"a seat's step naming a script", head + `
roles:
  - name: Dev
    handle: dev
    sandbox:
      setup:
        - name: tools
          files: {bin/setup.sh: "cp ${HOME}/.cache/x ."}
          optional_keys: [bin/setup.sh]
`, "roles[0].sandbox.setup[0].optional_keys[0]", "carries a ${VAR} reference"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := rejects(t, tc.doc, tc.path)
			if !errors.Is(err, ErrUnknownValue) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want an unknown value saying %q; got:\n%v", tc.want, err)
			}
		})
	}

	mustCompany(t, head+`
    setup:
      - name: npm
        files: {.npmrc: "${NPMRC}"}
        env: {NPM_TOKEN: "${NPM_TOKEN}"}
        optional_keys: [.npmrc, NPM_TOKEN]
roles:
  - name: Dev
    handle: dev
    sandbox:
      env: {DATABASE_URL: "postgres://${DB_HOST}/test"}
      optional_keys: [DATABASE_URL]
`)
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

// A SUB-ADDRESS IS THE SAME MAILBOX, AND TWO SEATS HOLDING IT COLLIDE.
//
// The registry matches a declared address in iam.NormalizeEmail's form, so
// `ada@` and `ada+ops@` are one key and the second seat is dropped from the
// index. A rule that folded case alone passed the document; the registry then
// answered every payload for either address with the first seat.
func TestTwoSeatsDeclaringOneMailboxThroughATagCollide(t *testing.T) {
	t.Parallel()

	err := rejects(t, `
name: Acme
roles:
  - {name: CEO, handle: ceo, email: ada@example.com}
  - {name: Dev, handle: dev, email: Ada+Ops@Example.com}
`, "roles[1].email")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}

	// A REFERENCE IS COMPARED AS WRITTEN: one variable is one address, and
	// two different variables are nothing validation can judge.
	if err := rejects(t, `
name: Acme
roles:
  - {name: CEO, handle: ceo, email: "${ADA_EMAIL}"}
  - {name: Dev, handle: dev, email: "${ADA_EMAIL}"}
`, "roles[0].email"); !errors.Is(err, ErrConflict) {
		t.Fatalf("two seats naming one variable: want ErrConflict, got %v", err)
	}
	mustCompany(t, `
name: Acme
roles:
  - {name: CEO, handle: ceo, email: "${ADA_EMAIL}"}
  - {name: Dev, handle: dev, email: "${ada_email}"}
`)
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

// A LOGIN-SHAPED HANDLE IS REFUSED IN THE FILE.
//
// A seat handle is one segment, a person's login joins segments with dots and
// a machine's with a colon ([iam.ValidSeatHandle]), so the three namespaces
// cannot collide by the shape of the value. A seat declared under a login's
// shape would be a name every lookup sends to the identity directory rather
// than to the company's seats.
func TestALoginShapedHandleIsRefusedInTheFile(t *testing.T) {
	t.Parallel()
	for _, handle := range []string{"jane.doe", "ci:release", "token:ops"} {
		t.Run(handle, func(t *testing.T) {
			t.Parallel()
			doc := "name: Acme\nroles:\n  - {name: Somebody, handle: \"" +
				handle + "\"}\n"
			rejects(t, doc, "roles[0].handle")
		})
	}
	// THE CONTROL: the same seat under a one-segment handle loads.
	if _, err := ParseCompany([]byte(
		"name: Acme\nroles:\n  - {name: Somebody, handle: jane-doe}\n")); err != nil {
		t.Errorf("a one-segment handle was refused: %v", err)
	}
}
