package sandbox

import "slices"

// What the layer asks a runner instead of its harness's name: each fact an
// optional capability of the [Runner] the manager holds for the harness
// ([Manager.RunnerFor]), never a method of [Runner] itself, so a runner that
// states none is read as the zero answer. What only the engine and a relaunch
// read ([SessionTraits], the window) the engine asks the executor for.

// ThreadTraits is what a runner's main-thread read can settle
// ([mainThreadReader]). The zero value reads no thread.
type ThreadTraits struct {
	// Reads says the harness's main thread is read at all: a collection
	// counts it, and a stop's or loss's read asks it receipts.
	Reads bool
	// CountsCutOff says a copy whose job the harness did not end itself is
	// still counted: false where the harness's record of the session (an
	// export the script takes as it ends) may lack what the copy holds.
	CountsCutOff bool
	// Receipts says the runner confirms a take off the job's own record:
	// its hooks' serves its thread shows, or the notes its session record
	// holds; a read of a copy holding a serve asks it its receipts
	// ([TakeReceipts]), whether or not it reads a whole thread.
	Receipts bool
	// PlacesWithResumeRead says a capture is placed at a save point only
	// where its release's resume was read ([TakenSession.ResumeRead]).
	PlacesWithResumeRead bool
}

// ThreadTraitsOf is runner's [ThreadTraits], zero for a runner that states
// none or a nil one.
func ThreadTraitsOf(runner Runner) ThreadTraits {
	if reader, ok := runner.(mainThreadReader); ok {
		return reader.ThreadTraits()
	}
	return ThreadTraits{}
}

// FirstRequest is how a harness treats a continued session's first request,
// as a relaunch judges it: whether it may compact the session before it
// reads the brief, and who can say.
type FirstRequest string

const (
	// FirstRequestNone is a harness that continues no session, so no first
	// request of a continued one is judged; read of one that does, it holds the
	// session whatever its brief, which no harness this build drives states.
	FirstRequestNone FirstRequest = ""
	// FirstRequestWindow means the harness compacts by the run's window
	// ([ExecutorWindow.SendsWhole]), judged before the launch.
	FirstRequestWindow FirstRequest = "window"
	// FirstRequestBox means the harness compacts at a point only the box the
	// launch gets can say ([BoxPoint.SendsWhole]).
	FirstRequestBox FirstRequest = "box"
)

// FirstRequests is every [FirstRequest] this build knows.
var FirstRequests = []FirstRequest{FirstRequestNone, FirstRequestWindow, FirstRequestBox}

// Valid reports whether this build knows f.
func (f FirstRequest) Valid() bool { return slices.Contains(FirstRequests, f) }

// CallKeying is the key a reading of a harness's session names a bridged call
// by ([HoldingsAsk]).
type CallKeying string

const (
	// CallKeyingNone means the harness's calls carry no key a reading asks by.
	CallKeyingNone CallKeying = ""
	// CallKeyingByCallID keys by the call's own id ([BridgeCall.CallID]), valid
	// where [ValidCallID] says so.
	CallKeyingByCallID CallKeying = "call-id"
	// CallKeyingByLogPlace keys by the call's place in its round's log, beside an
	// admission place, asked with its arguments' key; never for a call a
	// person's approval held.
	CallKeyingByLogPlace CallKeying = "log-place"
)

// CallKeyings is every [CallKeying] this build knows.
var CallKeyings = []CallKeying{CallKeyingNone, CallKeyingByCallID, CallKeyingByLogPlace}

// Valid reports whether this build knows k.
func (k CallKeying) Valid() bool { return slices.Contains(CallKeyings, k) }

// Charge is the fewest bytes of a session its harness counts as one token:
// [Charge.Tokens] of n bytes is the most n bytes can count for. Zero counts
// nothing.
type Charge int64

// Tokens is n bytes at c, rounded up; zero where n or c is not above zero.
func (c Charge) Tokens(n int64) int64 {
	if n <= 0 || c <= 0 {
		return 0
	}
	return (n + int64(c) - 1) / int64(c)
}

// SessionTraits is what a relaunch reads of the harness that wrote a session
// it continues, set by the engine from that harness's runner
// (internal/sandbox/executor's SessionTraits).
type SessionTraits struct {
	FirstRequest FirstRequest
	CallKeying   CallKeying
	// Charge is what a brief counts for in the harness's own estimate, for
	// the relaunch's account of a first request it judged.
	Charge Charge
}

// windowPinner is a [Runner] whose harness pins its run to a context window,
// as the collected record states it ([types.ExecutorCollected]). Optional: a
// runner that is none records no window.
type windowPinner interface{ PinnedWindow(model string) int }

// pinnedWindow is the window runner pins a run on model to, 0 for a runner
// that pins none.
func pinnedWindow(runner Runner, model string) int {
	if pinner, ok := runner.(windowPinner); ok {
		return pinner.PinnedWindow(model)
	}
	return 0
}

// notesOffExplainer is a [noteReader] that says what in a box can switch its
// harness's reading of notes off, for the warning a run that never asked for
// its notes gets ([Coordinator.warnNotesUnasked]). Optional.
type notesOffExplainer interface{ NotesOffWhy() string }

// notesOffWhy is what runner says can switch its harness's reading of notes
// off, "" where it says nothing.
func notesOffWhy(runner Runner) string {
	if explainer, ok := runner.(notesOffExplainer); ok {
		return explainer.NotesOffWhy()
	}
	return ""
}

// freshStarter is a [Runner] whose harness continues no earlier session, so
// every launch on it starts one of its own whatever session the turn or the
// box keeps ([Coordinator.continueSession]). Optional: a runner that is none
// continues what a launch hands it.
type freshStarter interface{ StartsFresh() bool }

// launchRouter is a [Runner] whose harness was measured to send a resumed
// session's calls on the route its launch hands it ([EntryContinues]).
// Optional: a runner that is none continues a session only under the entry it
// ran under.
type launchRouter interface{ ResumesOnLaunchRoute() bool }

// resumesOnLaunchRoute reports whether runner's harness was measured so.
func resumesOnLaunchRoute(runner Runner) bool {
	router, ok := runner.(launchRouter)
	return ok && router.ResumesOnLaunchRoute()
}

// startsFresh reports whether runner continues no earlier session.
func startsFresh(runner Runner) bool {
	starter, ok := runner.(freshStarter)
	return ok && starter.StartsFresh()
}

// telemetryExporter is a [Runner] whose harness exports its run's OTLP to
// the engine's receiver by the environment the run is started with
// ([withTelemetry]). Optional: a runner that is none is handed none of
// Crewlet's telemetry, so its model's shell holds none of it either.
type telemetryExporter interface{ ExportsTelemetry() bool }

// exportsTelemetry reports whether runner's harness exports its run's OTLP to
// the engine's receiver.
func exportsTelemetry(runner Runner) bool {
	exporter, ok := runner.(telemetryExporter)
	return ok && exporter.ExportsTelemetry()
}
