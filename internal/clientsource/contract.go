package clientsource

import (
	"fmt"
	"slices"
)

// Reader is how a declaration is read out of the dashboard's source.
type Reader string

const (
	// ReadLiteral reads a `const` initialised with an array or object
	// literal, through [Literal].
	ReadLiteral Reader = "literal"
	// ReadUnion reads a `type` alias that is a union of string literals,
	// through [Union].
	ReadUnion Reader = "union"
	// ReadInterface reads an `interface`'s members, through [Interface].
	ReadInterface Reader = "interface"
	// ReadScalar reads a `const` initialised with one number or string,
	// through [Scalar].
	ReadScalar Reader = "scalar"
)

// Valid reports whether r is a reader this package has.
func (r Reader) Valid() bool {
	switch r {
	case ReadLiteral, ReadUnion, ReadInterface, ReadScalar:
		return true
	}
	return false
}

// ContractDir is the directory, under [Tree], that holds every declaration in
// the contract and nothing else: `dashboard/src/contract/`.
//
// ONE HOME, because a declaration the engine owns that a screen keeps beside
// its own code is one a reader of that screen edits without knowing a Go gate
// holds it — and moves without knowing the move is invisible to the gate but
// not to the next person who greps for it. `contract_test.go` holds the
// directory both ways: every row is declared there, and everything the
// directory exports is a row.
const ContractDir = "contract"

// Entry is one row of the contract: a declaration the dashboard makes because
// the engine owns the value, how it is read, and the gate that holds it
// against the engine.
type Entry struct {
	// Name is the identifier the dashboard declares.
	Name string
	// Reader is the one reader that may read it.
	Reader Reader
	// Gate is the test that owns the comparison, as the package's directory
	// from the module root and the test's name:
	// `internal/events.TestCategoryChipsAreTheEngines`.
	Gate string
}

// contract is every declaration the engine's gates read out of the dashboard,
// grouped by the `dashboard/src/contract/` module that declares it.
//
// ONE OWNER PER DECLARATION. Two gates over one list is how the event
// categories came to be held twice, by two tests that agreed with each other
// and would have gone on agreeing while one of them was edited; a second
// question about a declaration belongs in the gate that already owns it.
//
// A declaration NO SCREEN IMPORTS is still a row when a gate reads it: the
// memory answer's row types are declared once and composed into the one
// interface a screen names, and each of them is a shape the engine sends.
var contract = []Entry{
	// categories.ts
	{"CATEGORIES", ReadLiteral, "internal/events.TestCategoryChipsAreTheEngines"},

	// turnbands.ts
	{"WENT_WRONG", ReadLiteral, "internal/events/types.TestTurnBandsNameOnlyTurnScopedEvents"},
	{"GIVEN", ReadLiteral, "internal/events/types.TestTurnBandsNameOnlyTurnScopedEvents"},
	{"DID", ReadLiteral, "internal/events/types.TestTurnBandsNameOnlyTurnScopedEvents"},
	{"LEFT_BEHIND", ReadLiteral, "internal/events/types.TestTurnBandsNameOnlyTurnScopedEvents"},
	{"TURN_STOP", ReadLiteral, "internal/events/types.TestTurnBandsNameOnlyTurnScopedEvents"},
	{"ABSORBED", ReadLiteral, "internal/events/types.TestTurnBandsNameOnlyTurnScopedEvents"},

	// reasons.ts
	{"PHRASES", ReadLiteral, "internal/api/queries.TestEveryWakeReasonReadsAsEnglishOnTheClient"},

	// spend.ts
	{"GROUPS", ReadLiteral, "internal/api/queries.TestEveryCostDimensionTheScreenOffersIsOneTheEngineAccepts"},
	{"BudgetState", ReadUnion, "internal/events/types.TestTheDashboardKnowsExactlyTheBudgetStatesTheEngineSends"},

	// work.ts
	{"WorkViewShape", ReadUnion, "internal/tracker.TestEveryViewShapeTheEngineMintsHasARenderer"},
	{"COLUMN_SORT_KEYS", ReadLiteral, "internal/tracker.TestEveryGridSortKeyIsOneTheGrammarTakes"},
	{"PROJECT_SORT_KEYS", ReadLiteral,
		"internal/tracker.TestTheProjectsDirectorySortsOnExactlyTheOrderingsTheEngineTakes"},
	{"CHANGES", ReadLiteral, "internal/tracker.TestEveryChangeKindTheEngineWritesHasAMarkAndAPhrase"},
	{"GROUP_AXES", ReadLiteral, "internal/tracker.TestEveryGroupingTheDashboardOffersIsOneTheGrammarTakes"},

	// attribution.ts
	{"CHANGE_FIELDS", ReadLiteral, "internal/tracker.TestAttributionFieldsAreTheEngines"},

	// config.ts
	{"ENTITY_KINDS", ReadLiteral, "internal/api/configapi.TestEntityKindsMatchTheClient"},
	{"BUDGET_WINDOWS", ReadLiteral, "internal/config.TestTheBudgetEditorOffersExactlyTheEnginesWindows"},

	// errors.ts
	{"QueryErrorCode", ReadUnion,
		"internal/api/stream.TestTheDashboardKnowsExactlyTheQueryErrorCodesTheEngineSends"},

	// coverage.ts
	{"Coverage", ReadInterface, "internal/eventfan.TestTheDashboardDeclaresExactlyTheCoverageTheEngineSends"},
	{"NodeCoverage", ReadInterface, "internal/eventfan.TestTheDashboardDeclaresExactlyTheCoverageTheEngineSends"},

	// integrations.ts
	{"IntegrationRow", ReadInterface, "internal/api/queries.TestTheIntegrationsRoomReadsWhatThisAnswerSends"},
	{"IntegrationsAnswer", ReadInterface, "internal/api/queries.TestTheIntegrationsRoomReadsWhatThisAnswerSends"},
	{"ReconcileStatus", ReadInterface, "internal/api/queries.TestTheIntegrationsRoomReadsWhatThisAnswerSends"},
	{"ReconcileFinding", ReadInterface, "internal/api/queries.TestTheIntegrationsRoomReadsWhatThisAnswerSends"},

	// memory.ts
	{"AgentMemory", ReadInterface, "internal/api/queries.TestTheMemoryScreenReadsWhatThisAnswerSends"},
	{"DiaryEntry", ReadInterface, "internal/api/queries.TestTheMemoryScreenReadsWhatThisAnswerSends"},
	{"Episode", ReadInterface, "internal/api/queries.TestTheMemoryScreenReadsWhatThisAnswerSends"},
	{"SynthesizedSkill", ReadInterface, "internal/api/queries.TestTheMemoryScreenReadsWhatThisAnswerSends"},
	{"CounterpartyProfile", ReadInterface, "internal/api/queries.TestTheMemoryScreenReadsWhatThisAnswerSends"},
	{"CounterpartySubject", ReadInterface, "internal/api/queries.TestTheMemoryScreenReadsWhatThisAnswerSends"},

	// health.ts
	{"EngineHealth", ReadInterface, "internal/api.TestTheDashboardDeclaresExactlyTheHealthTheEngineReports"},

	// wire.ts
	{"PushKind", ReadUnion, "internal/api/stream.TestTheDashboardKnowsExactlyThePushKindsTheEngineSends"},
	{"MAX_EVENTS", ReadScalar, "internal/api/livestate.TestTheDashboardKeepsTheFeedTheEngineKeeps"},
}

// Contract is every declaration the engine's gates read out of the
// dashboard's source, with its reader and its owning gate.
func Contract() []Entry { return slices.Clone(contract) }

// registered refuses a declaration the contract does not carry, or carries
// under another reader.
//
// THIS IS THE DIRECTION `contract_test.go` CANNOT WALK: a gate holding a
// declaration nobody registered would be a comparison no table knows about,
// and the table would stop describing what the engine checks. Refused at the
// read, the gate fails the first time it runs.
func registered(name string, reader Reader) error {
	i := slices.IndexFunc(contract, func(e Entry) bool { return e.Name == name })
	switch {
	case i < 0:
		return fmt.Errorf("clientsource: %s is not in the contract "+
			"(internal/clientsource/contract.go) — a declaration a gate holds "+
			"against the engine is registered there with its reader and its "+
			"owning gate before anything reads it", name)
	case contract[i].Reader != reader:
		return fmt.Errorf("clientsource: the contract reads %s as a %s, not a %s",
			name, contract[i].Reader, reader)
	}
	return nil
}
