package subagent

// ErrCallDeadline is the cause the call's own wall-clock cap ends the call
// with. A case about what that cap does ends the call with this cause itself,
// at the moment it has a signal for, rather than racing a short cap against
// everything a worker does before the moment the claim is about: the cause
// is what every status is read from ([stopReason]), so the call is classified
// exactly as it is when the cap's own timer fires.
var ErrCallDeadline = errCallDeadline
