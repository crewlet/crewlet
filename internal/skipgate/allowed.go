package main

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
	{
		Package: "internal/queue/jetstream",
		Test:    "TestConformance/NegativePaths/a_deferral_spends_no_dead_letter_budget",
		When:    Always,
		Why: "Defer is implemented as a Nak here, which costs one delivery count — why " +
			"MaxDeliver was re-derived from 10 to 25. Certified on the memory twin.",
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
}
