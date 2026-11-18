package mcpbridge

// WHICH SESSION A JOB'S BOX STARTED. Only the box learns whether it can continue
// its session, else its script starts one of its own (internal/sandbox/executor),
// and the run's skill guard is that session's. So a continuing launch hands the
// box two tokens ([Bridge.Open]'s and [Bridge.FreshEndpoint]'s) and the script
// removes the unused one before its harness starts: the token then is the box's
// word, arriving with the run's first request, and unforgeable by the model (the
// fleet signs the session into it, [freshMark]), where an outcome file or a report
// route could lie. A box that never reaches its bridge makes no call, and a rebuilt
// guard reads it as a fresh session (LaunchRecord.SessionStart): the safe side,
// since a guard short of a skill asks for it again, where one holding a skill the
// session never read lets through a call that skill exists to hold back.

// otherSession is why a request on a job's other token is refused once the
// opening has been told which session its box started.
const otherSession = "the job's box already started the other session, and holds this token no " +
	"longer: a box starts one session"

// begin tells the session which session the job's box started, as sub's token
// says ([freshMark], [Session.Started]), on the opening's first request of any
// route and before it is served; requests meanwhile wait under startMu, so no
// call meets the wrong guard. It answers false for the job's other token
// afterwards ([otherSession]). The hook runs under the run's context, never the
// request's, as every call does ([toolsession.Dispatcher.Call]): what it is told
// holds for the run even if that request goes unanswered.
func (o *binding) begin(sub subject) bool {
	o.startMu.Lock()
	defer o.startMu.Unlock()
	if o.started {
		return o.startedFresh == sub.fresh
	}
	o.started, o.startedFresh = true, sub.fresh
	if hook := o.session.Started; hook != nil {
		hook(o.ctx, sub.fresh)
	}
	return true
}
