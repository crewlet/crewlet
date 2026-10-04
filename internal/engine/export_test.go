package engine

import (
	"github.com/crewlet/crewlet/internal/sandbox"
)

// SeatBoxSetupForTest is the provisioning a seat's box is given at launch, as
// this node would hand it over: the provider-wide steps passed in, then the
// seat's own from the running company, every file's body read through this
// node's resolver.
//
// EXPORTED FOR A TEST ONLY: in production it is one step of a launch that
// needs a manager, a pending-run store and a budget, none of which is what a
// case about how a file reaches the box is about.
func SeatBoxSetupForTest(e *Engine, defaults []sandbox.SetupStep, handle string) []sandbox.SetupStep {
	return e.boxSetup(defaults, handle, seatSandbox(e.Company(), handle))
}

// ForgetPersonBlinderForTest drops this node's resolved blinder, so the next
// use resolves the company's key again.
//
// EXPORTED FOR A TEST ONLY: in production the cache is right for the life of
// the process, because the key never changes while a company runs. What a
// test needs is the state a restarted node is in after somebody deleted the
// key — and an engine cannot be restarted over the same store in one process.
func ForgetPersonBlinderForTest(e *Engine) {
	e.personBlinds.mu.Lock()
	defer e.personBlinds.mu.Unlock()
	e.personBlinds.held = nil
}
