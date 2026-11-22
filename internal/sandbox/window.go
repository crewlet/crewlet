package sandbox

// ExecutorWindow is the context window an executor run is pinned to, the
// least window its harness holds a run of that model to, and the headroom the
// harness keeps below it, as the harness's runner states them for a harness
// that compacts by its window ([FirstRequestWindow]; internal/sandbox/executor's
// Window). The zero value is no window.
//
// THE HARNESS COMPACTS UNDER THE SMALLER OF THE PIN AND ITS OWN WINDOW FOR THE
// MODEL, and keeps its own headroom below it: a reply reserve, then a
// compaction buffer ([ExecutorWindow.SendsWhole]), so the engine adds none.
type ExecutorWindow struct {
	// Tokens is the pin: the model's own window at its largest, the most the
	// harness may hold the run to, which the launch floor judges a continued
	// session under ([ExecutorWindow.Carries]).
	Tokens int
	// Standard is the least window the harness holds a run of the model to
	// where nothing served to the account or opted into raises it: what a
	// continuation's telling is judged under ([ExecutorWindow.SendsWhole]).
	// It can be below Tokens. Zero, with Known true, is a window this build
	// cannot say.
	Standard int
	// Known is false where this build does not know the model, and Tokens
	// and Standard are the runner's fallback.
	Known bool
	// Reserve is the most of a window the harness keeps back for a reply: a
	// request past the rest cannot be sent whole.
	Reserve int
	// Buffer is the room the harness keeps between its compaction point and
	// the window less Reserve ([ExecutorWindow.Point]).
	Buffer int
	// Attachments is the most, in tokens, a resumed run adds to its session
	// beside its brief before its first request.
	Attachments int
	// Charge is what a brief counts for in the harness's own estimate.
	Charge Charge
	// OlderText says the harness counts the model's text at its older rate
	// ([NextRequest.OlderTokens]).
	OlderText bool
}

// Carries reports whether one request under w can carry a continued session whose
// last request carried context tokens, with a brief of about brief tokens: a
// request the harness can send, which leaves the budget the session whole. NOT
// that the harness sends it without compacting the session first: its
// compaction point is lower ([ExecutorWindow.SendsWhole]), and a continuation
// past it is told as one that may have been cut off, never refused for it.
func (w ExecutorWindow) Carries(context int64, brief int) bool {
	return context+int64(brief) <= int64(w.Tokens-w.Reserve)
}

// SendsWhole reports whether the harness, holding a run to w, sends a resumed
// session's first request without compacting first: whether count's bound,
// the brief and the resume's own attachments stay below [ExecutorWindow.Point]
// of w's standard window, or of the window the run that left the copy
// reported where that is less ([ResumedCount.Window]). False where the count
// has no bound ([ResumedCount.Bound]) or the window is unknown (an unknown
// model, or a known one whose Standard is zero).
//
// STRICTLY BELOW: the harness compacts where its count is at least the point,
// and the bound is at least its count. Monotone in the brief: a longer brief
// never sends whole where a shorter one does not.
func (w ExecutorWindow) SendsWhole(count *ResumedCount, brief string) bool {
	bound, ok := count.Bound()
	if !ok || !w.Known || w.Standard <= 0 {
		return false
	}
	window := int64(w.Standard)
	if count.Window > 0 && count.Window < window {
		window = count.Window
	}
	return bound+w.Charge.Tokens(int64(len(brief)))+int64(w.Attachments) < w.Point(window)
}

// Point is the count at which the harness compacts a conversation under a
// window of window tokens: the window less its reply reserve and its
// compaction buffer ([ExecutorWindow.SendsWhole]).
func (w ExecutorWindow) Point(window int64) int64 {
	return window - int64(w.Reserve) - int64(w.Buffer)
}

// Least is the window a launch that may run under w or o is judged under: the
// lower standard window, Known only where both are, and the larger headroom
// and charge, so the judge holds a continuation to more and never less. An
// unknown window lowers nothing but Known: its standard window is a fallback.
func (w ExecutorWindow) Least(o ExecutorWindow) ExecutorWindow {
	switch {
	case !o.Known:
		w.Known = false
	case o.Standard < w.Standard:
		w.Standard = o.Standard
	}
	w.Reserve, w.Buffer = max(w.Reserve, o.Reserve), max(w.Buffer, o.Buffer)
	w.Attachments = max(w.Attachments, o.Attachments)
	if o.Charge > 0 && (w.Charge <= 0 || o.Charge < w.Charge) {
		w.Charge = o.Charge
	}
	return w
}

// pinnedWindow is the window the runner c's manager holds for harness pins a
// run on model to, 0 where the node has no runtime, no runner for it, or one
// that pins none ([windowPinner]).
func (c *Coordinator) pinnedWindow(harness, model string) int {
	manager := c.mgr()
	if manager == nil {
		return 0
	}
	runner, err := manager.RunnerFor(harness)
	if err != nil {
		return 0
	}
	return pinnedWindow(runner, model)
}
