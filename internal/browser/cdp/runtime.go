package cdp

import "encoding/json"

// ExecutionContextID is Runtime.ExecutionContextId: one JavaScript context of
// a target — a frame's main world, an isolated world, a worker's global.
type ExecutionContextID int64

// RemoteObjectID is Runtime.RemoteObjectId: a handle to a value held in a
// target, released with Runtime.releaseObject.
type RemoteObjectID string

// runtimeRunIfWaitingForDebugger is Runtime.runIfWaitingForDebugger: resume a
// target Chromium holds at its start. This package's AutoAttach alone sends
// it, and it is unexported so that nothing else can: sent to a page, it also
// releases every popup that page holds, before the popup's own setup
// (AutoAttach says how that was measured).
var runtimeRunIfWaitingForDebugger = Method[struct{}, struct{}]{name: "Runtime.runIfWaitingForDebugger"}

// RuntimeEnable is Runtime.enable: report the target's execution contexts.
// It also has Chromium send the page's console calls and its uncaught
// exceptions (Runtime.consoleAPICalled, Runtime.exceptionThrown), which this
// package declares no event for and no listener can ask for: each is dropped
// as it arrives, past ReadLimit as below it. First consumer: isolated worlds
// and the landing check (internal/browser).
var RuntimeEnable = Method[struct{}, struct{}]{name: "Runtime.enable"}

// RuntimeEvaluate is Runtime.evaluate: run an expression in one context —
// the engine's own scripts in its own isolated world, never a model's. First
// consumer: the landing check.
var RuntimeEvaluate = Method[RuntimeEvaluateParams, RuntimeEvaluateResult]{name: "Runtime.evaluate"}

// RuntimeEvaluateParams is Runtime.evaluate's parameters.
type RuntimeEvaluateParams struct {
	Expression string `json:"expression"`
	// ContextID or UniqueContextID names the context; neither is the
	// target's default one.
	ContextID       ExecutionContextID `json:"contextId,omitempty"`
	UniqueContextID string             `json:"uniqueContextId,omitempty"`
	ReturnByValue   bool               `json:"returnByValue,omitempty"`
	AwaitPromise    bool               `json:"awaitPromise,omitempty"`
	// Silent keeps an exception off the page's own console and pause
	// state.
	Silent bool `json:"silent,omitempty"`
}

// RuntimeEvaluateResult is Runtime.evaluate's result: the value, or the
// exception the expression threw.
type RuntimeEvaluateResult struct {
	Result           RemoteObject      `json:"result"`
	ExceptionDetails *ExceptionDetails `json:"exceptionDetails"`
}

// RuntimeCallFunctionOn is Runtime.callFunctionOn: call a function with an
// object (a resolved node) as its receiver. First consumer: credential fills
// and storage (internal/browser).
var RuntimeCallFunctionOn = Method[RuntimeCallFunctionOnParams, RuntimeCallFunctionOnResult]{name: "Runtime.callFunctionOn"}

// RuntimeCallFunctionOnParams is Runtime.callFunctionOn's parameters.
type RuntimeCallFunctionOnParams struct {
	FunctionDeclaration string `json:"functionDeclaration"`
	// ObjectID is the receiver; without it, ExecutionContextID names the
	// context the function runs in.
	ObjectID           RemoteObjectID     `json:"objectId,omitempty"`
	ExecutionContextID ExecutionContextID `json:"executionContextId,omitempty"`
	Arguments          []CallArgument     `json:"arguments,omitempty"`
	ReturnByValue      bool               `json:"returnByValue,omitempty"`
	AwaitPromise       bool               `json:"awaitPromise,omitempty"`
	Silent             bool               `json:"silent,omitempty"`
}

// RuntimeCallFunctionOnResult is Runtime.callFunctionOn's result.
type RuntimeCallFunctionOnResult struct {
	Result           RemoteObject      `json:"result"`
	ExceptionDetails *ExceptionDetails `json:"exceptionDetails"`
}

// CallArgument is Runtime.CallArgument: a value, or a handle to one.
type CallArgument struct {
	// Value is any JSON value, sent as it is written.
	Value    json.RawMessage `json:"value,omitempty"`
	ObjectID RemoteObjectID  `json:"objectId,omitempty"`
}

// RuntimeReleaseObject is Runtime.releaseObject. First consumer: credential
// fills.
var RuntimeReleaseObject = Method[RuntimeReleaseObjectParams, struct{}]{name: "Runtime.releaseObject"}

// RuntimeReleaseObjectParams is Runtime.releaseObject's parameters.
type RuntimeReleaseObjectParams struct {
	ObjectID RemoteObjectID `json:"objectId"`
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
// storage capture (internal/browser).
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
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	ClassName string `json:"className"`
	// Value is the value itself, when it was returned by value or is a
	// primitive.
	Value json.RawMessage `json:"value"`
	// UnserializableValue spells a number JSON cannot: "NaN", "-0",
	// "Infinity", a bigint.
	UnserializableValue string         `json:"unserializableValue"`
	Description         string         `json:"description"`
	ObjectID            RemoteObjectID `json:"objectId"`
}

// ExceptionDetails is Runtime.ExceptionDetails: what a script threw.
type ExceptionDetails struct {
	ExceptionID  int64         `json:"exceptionId"`
	Text         string        `json:"text"`
	LineNumber   int64         `json:"lineNumber"`
	ColumnNumber int64         `json:"columnNumber"`
	URL          string        `json:"url"`
	Exception    *RemoteObject `json:"exception"`
}

// RuntimeBindingCalled is Runtime.bindingCalled: a page called a binding.
// First consumer: storage capture.
var RuntimeBindingCalled = Event[RuntimeBindingCalledEvent]{name: "Runtime.bindingCalled"}

// RuntimeBindingCalledEvent is Runtime.bindingCalled's parameters.
type RuntimeBindingCalledEvent struct {
	Name               string             `json:"name"`
	Payload            string             `json:"payload"`
	ExecutionContextID ExecutionContextID `json:"executionContextId"`
}

// RuntimeExecutionContextCreated is Runtime.executionContextCreated. First
// consumer: isolated worlds.
var RuntimeExecutionContextCreated = Event[RuntimeExecutionContextCreatedEvent]{name: "Runtime.executionContextCreated"}

// RuntimeExecutionContextCreatedEvent is Runtime.executionContextCreated's
// parameters.
type RuntimeExecutionContextCreatedEvent struct {
	Context ExecutionContextDescription `json:"context"`
}

// ExecutionContextDescription is Runtime.ExecutionContextDescription.
type ExecutionContextDescription struct {
	ID ExecutionContextID `json:"id"`
	// UniqueID names the context across every process, where ID does not.
	UniqueID string `json:"uniqueId"`
	Origin   string `json:"origin"`
	// Name is an isolated world's name, the one it was created with.
	Name string `json:"name"`
	// AuxData is Chromium's own description of the context (its frameId,
	// isDefault and type), whose shape the protocol declares nowhere, so
	// it is left raw for its reader.
	AuxData json.RawMessage `json:"auxData"`
}

// RuntimeExecutionContextDestroyed is Runtime.executionContextDestroyed,
// which names the context by its unique id: its executionContextId is
// deprecated. First consumer: isolated worlds.
var RuntimeExecutionContextDestroyed = Event[RuntimeExecutionContextDestroyedEvent]{name: "Runtime.executionContextDestroyed"}

// RuntimeExecutionContextDestroyedEvent is Runtime.executionContextDestroyed's
// parameters.
type RuntimeExecutionContextDestroyedEvent struct {
	ExecutionContextUniqueID string `json:"executionContextUniqueId"`
}

// RuntimeExecutionContextsCleared is Runtime.executionContextsCleared: every
// context of the target is gone, as a navigation leaves them. First
// consumer: isolated worlds.
var RuntimeExecutionContextsCleared = Event[struct{}]{name: "Runtime.executionContextsCleared"}
