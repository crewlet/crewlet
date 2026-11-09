package cdp

import "reflect"

// Declared is one entry of the inventory, as the external tests read it
// against a browser's own description of the protocol.
type Declared struct {
	Name string
	// Event is an event; otherwise a command.
	Event bool
	// Params is the Go type its parameters are sent or read as, and Result
	// a command's result (nil for an event).
	Params, Result reflect.Type
}

// Inventory is every command and event this package declares.
func Inventory() []Declared {
	out := make([]Declared, 0, len(inventory))
	for _, d := range inventory {
		out = append(out, Declared{Name: d.Name(), Event: d.result() == nil, Params: d.params(), Result: d.result()})
	}
	return out
}

// PageCrash is Page.crash, which the crash case sends to crash a page's
// renderer. Declared here, in a test file, so it never reaches the inventory
// and no engine code can send it.
var PageCrash = Method[struct{}, struct{}]{name: "Page.crash"}

// The commands and events this package's own cases send and listen for, which
// never reach the inventory — the scripted peer's (peer_test.go) and
// Page.crash — registered here, so Call, Send, Listen and Of take them in
// this package's test binary and in no other build.
func init() {
	testOnly = []declared{testCall, testEvent, testOther, PageCrash}
}
