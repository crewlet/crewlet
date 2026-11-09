package cdp

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
)

// declared is one command or event this package declares: its protocol name
// and the Go types its parameters and result are read and sent as (an event
// has no result).
type declared interface {
	Name() string
	params() reflect.Type
	result() reflect.Type
}

// inventory is every command and event this package declares, and so every
// one the engine can send or listen for: [Call] and [Send] send, and
// [Session.Listen] and [Event.Of] read, only an entry of it, with the very
// types it is declared with here (checkDeclared), and [Method] and [Event]
// keep their names unexported, so nothing outside this package can declare
// another. TestEveryDeclaredMethodAndEventIsInTheInventory holds the list to
// the declarations in both directions, and the floor's measurement reads
// every name and field of it, so a command added here is one the floor is
// measured against.
var inventory = []declared{
	// Browser
	BrowserGetVersion,
	BrowserSetDownloadBehavior,
	BrowserDownloadProgress,
	BrowserDownloadWillBegin,
	// Target
	TargetActivateTarget,
	TargetAttachToTarget,
	TargetCloseTarget,
	TargetCreateBrowserContext,
	TargetCreateTarget,
	TargetDisposeBrowserContext,
	TargetGetBrowserContexts,
	TargetGetTargets,
	targetSetAutoAttach,
	TargetSetDiscoverTargets,
	TargetAttachedToTarget,
	TargetDetachedFromTarget,
	TargetTargetCrashed,
	TargetTargetCreated,
	TargetTargetDestroyed,
	TargetTargetInfoChanged,
	// Inspector
	InspectorDetached,
	InspectorTargetCrashed,
	// Runtime
	RuntimeAddBinding,
	RuntimeCallFunctionOn,
	RuntimeEnable,
	RuntimeEvaluate,
	RuntimeReleaseObject,
	runtimeRunIfWaitingForDebugger,
	RuntimeBindingCalled,
	RuntimeExecutionContextCreated,
	RuntimeExecutionContextDestroyed,
	RuntimeExecutionContextsCleared,
	// Page
	PageAddScriptToEvaluateOnNewDocument,
	PageCaptureScreenshot,
	PageCreateIsolatedWorld,
	PageEnable,
	PageGetFrameTree,
	PageGetLayoutMetrics,
	PageGetNavigationHistory,
	PageHandleJavaScriptDialog,
	PageNavigate,
	PageNavigateToHistoryEntry,
	PageScreencastFrameAck,
	PageSetInterceptFileChooserDialog,
	PageSetLifecycleEventsEnabled,
	PageStartScreencast,
	PageStopScreencast,
	PageFileChooserOpened,
	PageFrameAttached,
	PageFrameDetached,
	PageFrameNavigated,
	PageJavascriptDialogClosed,
	PageJavascriptDialogOpening,
	PageLifecycleEvent,
	PageNavigatedWithinDocument,
	PageScreencastFrame,
	// DOM
	DOMDescribeNode,
	DOMFocus,
	DOMGetBoxModel,
	DOMGetContentQuads,
	DOMGetFrameOwner,
	DOMGetNodeForLocation,
	DOMResolveNode,
	DOMScrollIntoViewIfNeeded,
	// DOMSnapshot
	DOMSnapshotCaptureSnapshot,
	// Accessibility
	AccessibilityEnable,
	AccessibilityGetFullAXTree,
	// Input
	InputDispatchKeyEvent,
	InputDispatchMouseEvent,
	InputInsertText,
	// Fetch
	FetchContinueRequest,
	FetchContinueResponse,
	FetchContinueWithAuth,
	FetchEnable,
	FetchFailRequest,
	FetchFulfillRequest,
	FetchTakeResponseBodyAsStream,
	FetchAuthRequired,
	FetchRequestPaused,
	// Network
	NetworkDeleteCookies,
	NetworkEnable,
	NetworkSetBypassServiceWorker,
	NetworkResponseReceivedExtraInfo,
	// Storage
	StorageGetCookies,
	StorageGetStorageKey,
	StorageSetCookies,
	// DOMStorage
	DOMStorageClear,
	DOMStorageEnable,
	DOMStorageGetDOMStorageItems,
	DOMStorageSetDOMStorageItem,
	DOMStorageDomStorageItemAdded,
	DOMStorageDomStorageItemRemoved,
	DOMStorageDomStorageItemUpdated,
	DOMStorageDomStorageItemsCleared,
	// IO
	IOClose,
	IORead,
	// Emulation
	EmulationSetDeviceMetricsOverride,
	EmulationSetFocusEmulationEnabled,
}

// testOnly is the commands and events this package's own tests send and
// listen for, which the inventory never holds: the scripted peer's.
// export_test.go registers them, so Call, Send, Listen and Of take them in
// this package's test binary and in no other build. No file but a test file
// writes it: the package's other files name it twice, here and in
// declarations' one read of it, and
// TestEveryDeclaredMethodAndEventIsInTheInventory fails a third.
var testOnly []declared

// declarations is what Call, Send, Listen and Of take, by name: the inventory,
// and in this package's test binary testOnly beside it. Built at its first
// use, which comes after every init, export_test.go's among them.
var declarations = sync.OnceValue(func() map[string]declared {
	m := map[string]declared{}
	for _, d := range slices.Concat(inventory, testOnly) {
		m[d.Name()] = d
	}
	return m
})

// errUndeclared is Call's, Send's, Listen's and Of's refusal of a command or
// event that is not an entry of the inventory with the types it is declared
// with there.
var errUndeclared = errors.New("only a command or event the inventory declares, with the types it declares there, is sent or read")

// errNilEvent is Listen's refusal of a nil Event, which has no name to give.
var errNilEvent = fmt.Errorf("cdp: a nil Event names no event: %w", errUndeclared)

// checkDeclared is nil when d is an entry of the inventory with exactly the
// parameter and result types it is declared with there, and otherwise why d
// is refused. Matching the types as well as the name is what holds a
// descriptor this package builds by any spelling but its entry's — a type
// alias of Method, a name set on a zero one, a struct of its shape converted,
// a declared name under other type arguments — to what the floor's
// measurement read: the walk of TestEveryDeclaredMethodAndEventIsInTheInventory
// reads one spelling, and this check refuses every other.
func checkDeclared(d declared) error {
	name := d.Name()
	switch {
	case name != "":
		if e, ok := declarations()[name]; ok && e.params() == d.params() && e.result() == d.result() {
			return nil
		}
		return fmt.Errorf("cdp: %s as %s: %w", name, reflect.TypeOf(d), errUndeclared)
	case d.result() == nil:
		return fmt.Errorf("cdp: an Event declared outside internal/browser/cdp names no event: %w", errUndeclared)
	}
	return fmt.Errorf("cdp: a Method declared outside internal/browser/cdp names no command: %w", errUndeclared)
}
