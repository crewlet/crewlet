package cdp

// The commands and events this package's own cases send and listen for, which
// never reach the inventory — the scripted peer's (peer_test.go) — registered
// here, so Call, Send, Listen and Of take them in this package's test binary
// and in no other build.
func init() {
	testOnly = []declared{testCall, testEvent, testOther}
}
