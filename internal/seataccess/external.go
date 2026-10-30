package seataccess

import "github.com/crewlet/crewlet/internal/integration"

// ExternalFinding is what the access pass reports for an integration the
// company connects whose agents' credentials are handed in from outside
// Crewlet (D4b): the engine holds no authority to create an agent's account
// there, so it adopts, changes, turns off and removes none of them, and each
// agent's tool reads the credential handed in for it.
//
// AN ADVISORY, not a fault: the integration works exactly as it was set up,
// and the sentence says what Crewlet does not do there, so nobody has to
// infer it from an Agents tab that never changes anything at the vendor.
func ExternalFinding(kind integration.Kind) integration.Finding {
	name := KindName(kind)
	return integration.Finding{
		Kind: integration.FindingCoveragePartial, Subject: string(kind),
		Title: "Accounts managed outside Crewlet",
		Detail: "Each agent's tool uses the " + name + " credential handed in for it. " +
			"Crewlet creates, changes and turns off none of these accounts",
		Remedy: "To have Crewlet manage them, connect " + name + " with administrator " +
			"access that creates agents' accounts",
	}
}
