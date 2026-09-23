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
)

// Valid reports whether r is a reader this package has.
func (r Reader) Valid() bool {
	switch r {
	case ReadLiteral, ReadUnion, ReadInterface:
		return true
	}
	return false
}

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

// contract is every declaration the engine's gates read out of the dashboard.
//
// ONE OWNER PER DECLARATION. Two gates over one list is how the event
// categories came to be held twice, by two tests that agreed with each other
// and would have gone on agreeing while one of them was edited; a second
// question about a declaration belongs in the gate that already owns it.
var contract = []Entry{
	{"CATEGORIES", ReadLiteral, "internal/events.TestCategoryChipsAreTheEngines"},

	{"WENT_WRONG", ReadLiteral, "internal/events/types.TestTurnBandsNameOnlyTurnScopedEvents"},
	{"GIVEN", ReadLiteral, "internal/events/types.TestTurnBandsNameOnlyTurnScopedEvents"},
	{"DID", ReadLiteral, "internal/events/types.TestTurnBandsNameOnlyTurnScopedEvents"},
	{"LEFT_BEHIND", ReadLiteral, "internal/events/types.TestTurnBandsNameOnlyTurnScopedEvents"},
	{"TURN_STOP", ReadLiteral, "internal/events/types.TestTurnBandsNameOnlyTurnScopedEvents"},
	{"ABSORBED", ReadLiteral, "internal/events/types.TestTurnBandsNameOnlyTurnScopedEvents"},

	{"WorkViewShape", ReadUnion, "internal/tracker.TestEveryViewShapeTheEngineMintsHasARenderer"},
	{"COLUMN_SORT_KEYS", ReadLiteral, "internal/tracker.TestEveryGridSortKeyIsOneTheGrammarTakes"},
	{"PROJECT_SORT_KEYS", ReadLiteral,
		"internal/tracker.TestTheProjectsDirectorySortsOnExactlyTheOrderingsTheEngineTakes"},
	{"CHANGES", ReadLiteral, "internal/tracker.TestEveryChangeKindTheEngineWritesHasAMarkAndAPhrase"},
	{"GROUP_AXES", ReadLiteral, "internal/tracker.TestEveryGroupingTheDashboardOffersIsOneTheGrammarTakes"},
	{"CHANGE_FIELDS", ReadLiteral, "internal/tracker.TestAttributionFieldsAreTheEngines"},

	{"ENTITY_KINDS", ReadLiteral, "internal/api/configapi.TestEntityKindsMatchTheClient"},

	{"QueryErrorCode", ReadUnion,
		"internal/api/stream.TestTheDashboardKnowsExactlyTheQueryErrorCodesTheEngineSends"},

	{"IntegrationRow", ReadInterface, "internal/api/queries.TestTheIntegrationsRoomReadsWhatThisAnswerSends"},
	{"IntegrationsAnswer", ReadInterface, "internal/api/queries.TestTheIntegrationsRoomReadsWhatThisAnswerSends"},
	{"PHRASES", ReadLiteral, "internal/api/queries.TestEveryWakeReasonReadsAsEnglishOnTheClient"},
	{"GROUPS", ReadLiteral, "internal/api/queries.TestEveryCostDimensionTheScreenOffersIsOneTheEngineAccepts"},
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
