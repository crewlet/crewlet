package cdp

import "encoding/json"

// ExecutionContextID is Runtime.ExecutionContextId: one JavaScript context of
// a target — a frame's main world, an isolated world, a worker's global.
type ExecutionContextID int64

// runtimeRunIfWaitingForDebugger is Runtime.runIfWaitingForDebugger: resume a
// target Chromium holds at its start. This package's AutoAttach alone sends
// it, and it is unexported so that nothing else can: sent to a page, it also
// releases every popup that page holds, before the popup's own setup (the
// package doc says how that was measured).
var runtimeRunIfWaitingForDebugger = Method[struct{}, struct{}]{name: "Runtime.runIfWaitingForDebugger"}

// RuntimeEnable is Runtime.enable: report the target's execution contexts.
// It also has Chromium send the page's console calls and its uncaught
// exceptions ([RuntimeConsoleAPICalled], [RuntimeExceptionThrown]), each
// dropped as it arrives unless a listener asked for it. First consumer: the
// render's console report and frames (internal/artifactrender).
var RuntimeEnable = Method[struct{}, struct{}]{name: "Runtime.enable"}

// RuntimeEvaluate is Runtime.evaluate: run an expression in one context, the
// render's own scripts in a world of its own. First consumer: the render's
// checks.
var RuntimeEvaluate = Method[RuntimeEvaluateParams, RuntimeEvaluateResult]{name: "Runtime.evaluate"}

// RuntimeEvaluateParams is Runtime.evaluate's parameters.
type RuntimeEvaluateParams struct {
	Expression string `json:"expression"`
	// ContextID names the context; zero is the target's default one.
	ContextID     ExecutionContextID `json:"contextId,omitempty"`
	ReturnByValue bool               `json:"returnByValue,omitempty"`
	AwaitPromise  bool               `json:"awaitPromise,omitempty"`
}

// RuntimeEvaluateResult is Runtime.evaluate's result: the value, or the
// exception the expression threw.
type RuntimeEvaluateResult struct {
	Result           RemoteObject      `json:"result"`
	ExceptionDetails *ExceptionDetails `json:"exceptionDetails"`
}

// RuntimeAddBinding is Runtime.addBinding: a function on the global object of
// every context of the target — or only of the isolated world
// ExecutionContextName names — whose calls arrive as Runtime.bindingCalled.
// Enable Runtime on the session first: Chromium carries a session's bindings
// into the process a navigation moves the target to only for a session whose
// Runtime is enabled, so without it a held popup or out-of-process frame
// loses its binding as its document commits (measured on both pinned
// Chromiums). Runtime.enable also brings the page's console calls and
// exceptions (RuntimeEnable). A page calls a binding with whatever it likes,
// so a call past ReadLimit ends the session whose listener asked for
// Runtime.bindingCalled, with ErrMessageTooBig, and no other. First consumer:
// the render's host page.
var RuntimeAddBinding = Method[RuntimeAddBindingParams, struct{}]{name: "Runtime.addBinding"}

// RuntimeAddBindingParams is Runtime.addBinding's parameters.
type RuntimeAddBindingParams struct {
	Name                 string `json:"name"`
	ExecutionContextName string `json:"executionContextName,omitempty"`
}

// RemoteObject is Runtime.RemoteObject: a value as Chromium hands it back.
type RemoteObject struct {
	// Type is "object", "function", "undefined", "string", "number",
	// "boolean", "symbol" or "bigint".
	Type string `json:"type"`
	// Value is the value itself, when it was returned by value or is a
	// primitive.
	Value json.RawMessage `json:"value"`
	// UnserializableValue spells a number JSON cannot: "NaN", "-0",
	// "Infinity", a bigint.
	UnserializableValue string `json:"unserializableValue"`
	Description         string `json:"description"`
}

// ExceptionDetails is Runtime.ExceptionDetails: what a script threw.
type ExceptionDetails struct {
	Text       string        `json:"text"`
	LineNumber int64         `json:"lineNumber"`
	URL        string        `json:"url"`
	Exception  *RemoteObject `json:"exception"`
	StackTrace *StackTrace   `json:"stackTrace"`
	// ExecutionContextID is the context the script ran in.
	ExecutionContextID ExecutionContextID `json:"executionContextId"`
}

// StackTrace is Runtime.StackTrace: where a console call or an exception came
// from, innermost frame first.
type StackTrace struct {
	CallFrames []CallFrame `json:"callFrames"`
}

// CallFrame is Runtime.CallFrame: one frame of a stack, its line and column
// counted from zero.
type CallFrame struct {
	URL        string `json:"url"`
	LineNumber int64  `json:"lineNumber"`
}

// RuntimeConsoleAPICalled is Runtime.consoleAPICalled: a page called the
// console. Arguments past ReadLimit end only the listening session. First
// consumer: the artifact render's console report (internal/artifactrender).
var RuntimeConsoleAPICalled = Event[RuntimeConsoleAPICalledEvent]{name: "Runtime.consoleAPICalled"}

// RuntimeConsoleAPICalledEvent is Runtime.consoleAPICalled's parameters.
type RuntimeConsoleAPICalledEvent struct {
	// Type is "log", "warning", "error", "assert", "info", "debug" or
	// another console method Chromium names.
	Type               string             `json:"type"`
	Args               []RemoteObject     `json:"args"`
	ExecutionContextID ExecutionContextID `json:"executionContextId"`
	StackTrace         *StackTrace        `json:"stackTrace"`
}

// RuntimeExceptionThrown is Runtime.exceptionThrown: a script threw and
// nothing caught it. First consumer: the artifact render's console report.
var RuntimeExceptionThrown = Event[RuntimeExceptionThrownEvent]{name: "Runtime.exceptionThrown"}

// RuntimeExceptionThrownEvent is Runtime.exceptionThrown's parameters.
type RuntimeExceptionThrownEvent struct {
	ExceptionDetails ExceptionDetails `json:"exceptionDetails"`
}

// RuntimeBindingCalled is Runtime.bindingCalled: a page called a binding.
// First consumer: the render's host page.
var RuntimeBindingCalled = Event[RuntimeBindingCalledEvent]{name: "Runtime.bindingCalled"}

// RuntimeBindingCalledEvent is Runtime.bindingCalled's parameters.
type RuntimeBindingCalledEvent struct {
	Name               string             `json:"name"`
	Payload            string             `json:"payload"`
	ExecutionContextID ExecutionContextID `json:"executionContextId"`
}

// RuntimeExecutionContextCreated is Runtime.executionContextCreated. First
// consumer: the render's frames.
var RuntimeExecutionContextCreated = Event[RuntimeExecutionContextCreatedEvent]{name: "Runtime.executionContextCreated"}

// RuntimeExecutionContextCreatedEvent is Runtime.executionContextCreated's
// parameters.
type RuntimeExecutionContextCreatedEvent struct {
	Context ExecutionContextDescription `json:"context"`
}

// ExecutionContextDescription is Runtime.ExecutionContextDescription.
type ExecutionContextDescription struct {
	ID ExecutionContextID `json:"id"`
	// AuxData is Chromium's own description of the context (its frameId,
	// isDefault and type), whose shape the protocol declares nowhere, so
	// it is left raw for its reader.
	AuxData json.RawMessage `json:"auxData"`
}
