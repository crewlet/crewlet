package cdp

// InspectorDetached is Inspector.detached: the session was detached from its
// target, which ends it with a [*DetachedError] carrying the reason. First
// consumer: this package's session ends.
var InspectorDetached = Event[InspectorDetachedEvent]{name: "Inspector.detached"}

// InspectorDetachedEvent is Inspector.detached's parameters.
type InspectorDetachedEvent struct {
	// Reason is Chromium's: "target_closed", "replaced_with_devtools",
	// "Render process gone." and others.
	Reason string `json:"reason"`
}

// InspectorTargetCrashed is Inspector.targetCrashed: the session's target
// crashed, which ends every session attached to it with a [*CrashedError].
// First consumer: this package's session ends.
var InspectorTargetCrashed = Event[struct{}]{name: "Inspector.targetCrashed"}
