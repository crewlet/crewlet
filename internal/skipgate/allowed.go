package main

import "strings"

// Skip is one test that reported itself skipped.
type Skip struct {
	// Package is the import path with the module prefix removed, so an
	// entry below reads as the directory a reader would go to.
	Package string
	Test    string
}

// When says whether a skip is expected on every run or only on some machines.
//
// The distinction is what stops this table being either useless or wrong. A
// STRUCTURAL skip — a backend that cannot do the thing, a driver capability
// that has not landed — fires on every run by construction, so one that stops
// firing means the world changed and the entry is stale. An ENVIRONMENT skip
// fires only where a tool, daemon or binary is missing, so whether it fires is
// a fact about the machine and demanding it would fail the build on the
// machine that has more, not less.
type When string

const (
	// Always: this skip is expected on EVERY run, and an entry that does not
	// fire is reported as stale.
	Always When = "always"

	// Environment: this skip fires only where the machine lacks something,
	// and is checked in the unlisted direction only.
	Environment When = "environment"
)

// Allowance is one declared skip, with why.
type Allowance struct {
	Package string
	Test    string
	When    When
	// Why is for the person reading a CI log, and it has a job: it must say
	// what is NOT being checked, and where that coverage lives instead if it
	// lives anywhere. "It skips because the backend cannot" is not a reason;
	// "the memory twin certifies the same case" is.
	Why string
}

// allowed is every skip this repository declares.
//
// ADDING AN ENTRY IS THE DECISION, so it belongs in a diff somebody reviews
// rather than in a counter somebody raises. Before adding one, check that the
// case is covered somewhere — the two defects this gate was written after were
// both cases that ran NOWHERE, and each looked like an ordinary capability skip
// from inside the one run that saw it.
var allowed = []Allowance{
	// -----------------------------------------------------------------
	// Structural: the shipped JetStream backend deliberately behaves
	// otherwise, and the memory twin runs the same case. queuetest's own
	// capability flags document what each cost when the suite demanded it.
	// -----------------------------------------------------------------
	{
		Package: "internal/queue/jetstream",
		Test:    "TestConformance/EventQueue/nak_returns_the_event_to_the_front_of_the_mailbox",
		When:    Always,
		Why: "JetStream returns a redelivered message BEHIND never-delivered ones. " +
			"queue.OrderForDispatch exists because of it. Certified on the memory twin.",
	},
	{
		Package: "internal/queue/jetstream",
		Test:    "TestConformance/EventQueue/start_stop_lifecycle",
		When:    Always,
		Why: "Open establishes the connection and the streams, so Start is a no-op and " +
			"a publish before it is not refused. The contract deliberately requires no " +
			"Start barrier. Certified on the memory twin, which declares RequiresStart.",
	},
	{
		Package: "internal/queue/jetstream",
		Test:    "TestConformance/EventQueue/stop_clears_pause",
		When:    Always,
		Why: "JetStream treats Stop as terminal — a restart needs a fresh queue — so " +
			"there is no restart for a pause to survive. Certified on the memory twin, " +
			"which declares Restartable.",
	},
	{
		Package: "internal/queue/jetstream",
		Test:    "TestConformance/Batch/zero_linger_dispatches_inline_single_event_batches",
		When:    Always,
		Why: "Pull consumers fetch on their own schedule, so batch boundaries are not " +
			"deterministic and there is no inline dispatch to observe. Certified on the " +
			"memory twin.",
	},

	// THE SAME FOUR, THROUGH A LEAF. The suite runs a second time against
	// a client of a JetStream-less leaf, which is the same JetStream client
	// reaching the members' domain across one link — so each case skips
	// for the reason its member entry above gives, and the memory twin
	// certifies it once for both.
	{
		Package: "internal/queue/jetstream",
		Test:    "TestConformanceThroughALeaf/EventQueue/nak_returns_the_event_to_the_front_of_the_mailbox",
		When:    Always,
		Why: "The member's reason, across a leaf link: JetStream returns a redelivered " +
			"message behind never-delivered ones. Certified on the memory twin.",
	},
	{
		Package: "internal/queue/jetstream",
		Test:    "TestConformanceThroughALeaf/EventQueue/start_stop_lifecycle",
		When:    Always,
		Why: "The member's reason, across a leaf link: Open establishes the connection " +
			"and the streams, so there is no Start barrier. Certified on the memory twin.",
	},
	{
		Package: "internal/queue/jetstream",
		Test:    "TestConformanceThroughALeaf/EventQueue/stop_clears_pause",
		When:    Always,
		Why: "The member's reason, across a leaf link: Stop is terminal, so there is " +
			"no restart for a pause to survive. Certified on the memory twin.",
	},
	{
		Package: "internal/queue/jetstream",
		Test:    "TestConformanceThroughALeaf/Batch/zero_linger_dispatches_inline_single_event_batches",
		When:    Always,
		Why: "The member's reason, across a leaf link: pull consumers fetch on their " +
			"own schedule, so batch boundaries are not deterministic. Certified on the " +
			"memory twin.",
	},

	// -----------------------------------------------------------------
	// Structural: a driver capability that has not reached Go yet. This is
	// the tree's model skip and CLAUDE.md names it — the case turns into a
	// passing test the day the feature lands, and gate() ALSO fails when a
	// capability appears that the matrix does not record, so the measurement
	// cannot drift in either direction.
	// -----------------------------------------------------------------
	{
		Package: "internal/store",
		Test:    "TestCapabilityMatrix/VectorIndex",
		When:    Always,
		Why: "Turso has no reachable ANN vector index from Go; `USING vector`/`USING diskann` " +
			"answer `unknown module name` even behind experimental=index_method. Recall is a " +
			"scan behind the per-agent index, which internal/search is written against.",
	},
	{
		Package: "internal/store",
		Test:    "TestCapabilityMatrix/FullTextSearch",
		When:    Always,
		Why: "Turso has no fts5 and no `fts` index method, which is the whole reason " +
			"internal/textindex exists — the engine ships its own BM25 inverted list.",
	},
	{
		Package: "internal/store",
		Test:    "TestCapabilityMatrix/WithoutRowid",
		When:    Always,
		Why:     "WITHOUT ROWID is not available on this driver. No schema here depends on it.",
	},

	// -----------------------------------------------------------------
	// Re-entry guards, not gaps. Each names a test that only means anything
	// as a subprocess; its PARENT runs on every run and now requires the
	// child to have actually run, not merely to have exited 0.
	// -----------------------------------------------------------------
	{
		Package: "internal/store",
		Test:    "TestTursoLibraryPreparedByAChildProcess",
		When:    Always,
		Why: "The child half of TestConcurrentStartsDoNotCorruptTheLibraryCache, which " +
			"runs ten of these per run and fails when one does not report itself.",
	},
	{
		Package: "internal/store",
		Test:    "TestOpenWithABrokenLibraryCacheInAChildProcess",
		When:    Always,
		Why: "The child half of TestOpenReportsABrokenLibraryCacheInsteadOfPanicking, " +
			"which runs it on every run and requires its sentinel.",
	},
	{
		Package: "internal/store",
		Test:    "TestPendingWithABrokenLibraryCacheInAChildProcess",
		When:    Always,
		Why: "The child half of TestPendingReportsABrokenLibraryCacheInsteadOfPanicking, " +
			"which runs it on every run and requires its sentinel.",
	},

	// -----------------------------------------------------------------
	// Environment: a vendor binary nobody installs on a pull request.
	//
	// These are the only skips here that are a real coverage gap rather than
	// a property of the code, and they are the argument for a scheduled
	// workflow that installs the CLIs at a pin and runs them — not for
	// downloading ~450 MB of vendor binaries on every PR, past a
	// supply-chain surface no Dependabot ecosystem watches.
	// -----------------------------------------------------------------
	{
		Package: "internal/providers/llm/cliagent",
		Test:    "TestTheGrokProfileArgvParsesAgainstTheRealCLI",
		When:    Environment,
		Why: "Needs xAI's own `grok` on PATH. Nothing else can catch a vendor flag rename: " +
			"the fake CLI accepts any argv and a unit test over the slice re-states the " +
			"profile back to itself. `crewlet llm doctor` reports the same drift to an operator.",
	},
	{
		Package: "internal/providers/llm/cliagent",
		Test:    "TestTheKimiProfileArgvParsesAgainstTheRealCLI",
		When:    Environment,
		Why: "Needs MoonshotAI's own `kimi` on PATH. Same reasoning as grok and muse: " +
			"only the real binary can refuse a profile's argv, and the fake accepts any.",
	},
	{
		Package: "internal/providers/llm/cliagent",
		Test:    "TestThePiProfileArgvParsesAgainstTheRealCLI",
		When:    Environment,
		Why: "Needs `pi` on PATH. Same reasoning as the other real-CLI argv cases; the " +
			"profile's isolation flags are what it proves and the fake cannot refuse them.",
	},
	{
		Package: "internal/providers/llm/cliagent",
		Test:    "TestTheHermesProfileArgvParsesAgainstTheRealCLI",
		When:    Environment,
		Why:     "Needs `hermes` on PATH. Same reasoning as the other real-CLI argv cases.",
	},
	{
		Package: "internal/providers/llm/cliagent",
		Test:    "TestTheMuseProfileArgvParsesAgainstTheRealCLI",
		When:    Environment,
		Why: "Needs `muse` on PATH. Same reasoning as the grok case, and the flags it " +
			"proves — --disable-shell and --disable-write — are absent from the vendor's " +
			"published list and were read off the binary.",
	},

	// -----------------------------------------------------------------
	// Environment: a container runtime. It does NOT skip in CI — the run's
	// own log carries zero SKIP lines and `--- PASS:
	// TestTheContainerModeRunsTheSameProtocol` — so this entry covers a
	// workstation without docker or podman. If it ever starts skipping in
	// CI, this gate is what says so.
	// -----------------------------------------------------------------
	{
		Package: "internal/e2e",
		Test:    "TestTheContainerModeRunsTheSameProtocol",
		When:    Environment,
		Why: "Needs a usable docker or podman. It is the only place containerBox's " +
			"in-box paths are exercised against a real container; the direct mode covers " +
			"the run protocol itself.",
	},

	// -----------------------------------------------------------------
	// Environment: a Bourne shell. Listed for the same reason as the
	// container entry — it does not fire on any machine the gates run on,
	// and an entry is what makes the day it starts firing visible rather
	// than a quiet loss of the only case that proves the quoting.
	// -----------------------------------------------------------------
	{
		Package: "internal/envfile",
		Test:    "TestEveryWrittenAssignmentSurvivesARealShell",
		When:    Environment,
		Why: "Needs `sh` on PATH. It is the only case that proves FormatAssignment's " +
			"quoting against a real parser rather than against this package's own reading " +
			"of it; the table cases check the bytes written, which is what a wrong quoting " +
			"rule would agree with.",
	},

	// -----------------------------------------------------------------
	// Environment: enough free disk for the scenario's own headroom. The
	// state logs' ceilings are sized against a REAL embedded broker, whose
	// cap is three quarters of the volume it stores on — a number a test
	// cannot choose. What it can choose is how much of that cap is already
	// reserved, so each case reserves all but the headroom it needs and
	// skips, naming the figures, on a machine whose disk cannot hold even
	// that. It does not fire on any machine the gates run on; an entry is
	// what makes the day it starts firing visible rather than a quiet loss
	// of the only cases that hold the arithmetic against a broker.
	// -----------------------------------------------------------------
	{
		Package: "internal/engine",
		Test:    "TestTheStateLogsFitTheBrokerTheyBootOn",
		When:    Environment,
		Why: "Needs a volume whose broker can reserve 5.7 GiB plus a gibibyte to spend " +
			"around it. It is the only case that proves the three logs fit one budget " +
			"against a real broker; fitCeilings' table covers the arithmetic with no " +
			"broker in it, and would agree with a ceiling reserved outside the budget.",
	},
	{
		Package: "internal/engine",
		Test:    "TestARestartSizesTheLogsAsTheFirstBootDid",
		When:    Environment,
		Why: "Needs a broker that can reserve 12 GiB plus a gibibyte. It is the only " +
			"case that proves a restart divides the pool the first boot divided on a " +
			"broker that states its limit, which is a property of what the running " +
			"streams hold and has no unit form; the free-space fallback's has one.",
	},
	{
		Package: "internal/engine",
		Test:    "TestARefusedReservationNamesWhatItNeededAndHad/a_derived_ceiling_at_its_floor",
		When:    Environment,
		Why: "Needs a broker that can reserve 2.5 GiB plus a gibibyte. It is the only " +
			"case that reads the refusal a real broker gives a derived ceiling already " +
			"at its floor, including which remedies it must not offer.",
	},
	{
		Package: "internal/engine",
		Test:    "TestARefusedReservationNamesWhatItNeededAndHad/an_explicit_ceiling",
		When:    Environment,
		Why: "Needs a broker that can reserve 3.5 GiB plus a gibibyte. Same refusal " +
			"against an operator's own ceiling, which is never scaled and is told what " +
			"would have fitted instead.",
	},
}

// Measurement is one test whose OUTPUT reaches the log even when it PASSES.
//
// # Why this table has to exist
//
// [read] renders the stream back as plain `go test` output, which means a
// passing test's own output is dropped — that is what `go test` without -v
// does, and echoing everything would make a CI log tens of times longer and
// get the gate taken back out.
//
// But a handful of tests exist to PRINT A NUMBER that somebody is meant to
// watch: a drain rate, a throughput, a measured ratio. Dropping their output on
// a pass means the number reaches a log only on the run where the test FAILED —
// which is the one run where it is least useful and most likely to be blamed on
// the change under test. internal/store's drain measurement spent its whole
// life in exactly that state: it logged the applier's rows/s on every green
// run, and not one of those lines survived to a CI log, so when the wall-clock
// assertion it carried finally failed there was no history to compare against
// and n = 1.
//
// An entry here is therefore a declaration that this test's output is DATA, not
// noise, and that somebody reads it.
type Measurement struct {
	Package string
	Test    string

	// Why says what number the test prints and who reads it — the same bar
	// [Allowance.Why] sets. "It logs some timings" is not a reason; "the
	// drain rate docs/guides/replication.md publishes a floor against" is.
	Why string
}

// measured is every test whose output survives a pass.
//
// TWO-SIDED, exactly like the Always half of [allowed]: an entry whose package
// ran but whose test never reported is STALE and fails the build. A renamed or
// deleted measurement would otherwise stop reaching the log silently, which is
// the failure this table was added to fix — so the table cannot be allowed to
// rot into describing a test that no longer exists.
var measured = []Measurement{
	{
		Package: "internal/store",
		Test:    "TestTheMultiRowApplyIssuesOneStatementPerChunk",
		Why: "the applier's drain rate in rows/s across the three statement " +
			"shapes, and the statement counts behind it. Every read-latency " +
			"bound in the design is derived from this number and " +
			"docs/guides/replication.md publishes a floor against it, so it " +
			"belongs in the log of the runs that PASS — the ones that " +
			"establish what normal is.",
	},
}

// measurement finds the entry covering one test, matching a parent's subtests
// too so a table-driven measurement declares itself once.
func measurement(pkg, test string) *Measurement {
	for i := range measured {
		if measured[i].Package != pkg {
			continue
		}
		if measured[i].Test == test || strings.HasPrefix(test, measured[i].Test+"/") {
			return &measured[i]
		}
	}
	return nil
}

// JudgeMeasured reports declared measurements whose package ran but which never
// reported a result — a renamed or deleted test, printing nothing, for ever.
func JudgeMeasured(seen map[string]bool, ran map[string]bool) []Skip {
	var stale []Skip
	for _, m := range measured {
		if !ran[m.Package] || seen[m.Package+"\x00"+m.Test] {
			continue
		}
		stale = append(stale, Skip{Package: m.Package, Test: m.Test})
	}
	return stale
}
