package engine

import "testing"

// EVERY REGISTERED DOMAIN CAN BE RE-ANCHORED, and a test calls the switch
// rather than waiting for an operator to.
//
// A domain is DECLARED in [registeredDomains] and then needs an answer from
// switches the declaration cannot give. Most of them are asked at boot and
// refuse it naming the domain. The generation record ([generationEncoder]) is
// not: it is asked for only when an operator re-anchors a recreated log,
// mid-incident — which is how the usage domain was registered with no case
// there, and a recreated usage log was one no operator could move the fleet
// onto, with nothing reporting it until somebody tried.
//
// Calling it is the point. A scan of this package's SOURCE for each domain's
// name would go inert the moment somebody renamed the function or moved the
// switch, and would keep reporting a pass over a site that no longer exists.
func TestEveryRegisteredDomainHasAGenerationRecord(t *testing.T) {
	t.Parallel()
	for _, domain := range registeredDomains() {
		t.Run(domain.Name(), func(t *testing.T) {
			t.Parallel()

			// A GENERATION RECORD, and one EXACTLY where the domain
			// claims identity: first-writer-wins between two operators
			// re-anchoring at once is what that record is for, so an
			// identity log without one could be opened at one generation
			// by two of them, and a log that keeps one while claiming
			// nothing writes an audit row nobody reads.
			record, err := generationEncoder(domain)
			if err != nil {
				t.Fatalf("generationEncoder: %v — which nothing reports until an "+
					"operator tries it, mid-incident", err)
			}
			if _, keeps := record.GenerationSubject(1); keeps != domain.ClaimsIdentity() {
				t.Errorf("%s keeps a generation record = %v while claiming "+
					"identity = %v", domain.Name(), keeps, domain.ClaimsIdentity())
			}
		})
	}
}
