package mcpbridge

// WHICH SESSION A JOB'S BOX STARTED, AND THE GUARD HELD TO IT.
//
// A run that continues a session has its harness take that session up, and
// whether its box can — the archive carried into it unpacks, OpenCode takes the
// import, the box still holds its own copy — is the box's to find out, as the
// run starts: where it cannot, the run's script starts a session of its own in
// that one's place, on the brief written for one (internal/sandbox/executor).
// The run's skill guard is the session's it is in — the skills a continued
// session holds the bodies of, or none — so the bridge has to know which session
// the box started before it admits a single call of the run.
//
// So a launch that continues a session hands its box the seat's tools under TWO
// tokens of the job: the one [Bridge.Open] answers, for the session the launch
// continues, and its other ([Bridge.FreshEndpoint]), for a session of its own.
// The run's script installs the one of the session it started and removes the
// other from the box before its harness starts, so the token a request comes
// with IS the box's word on which session it started:
//
//   - THE MOST DIRECT CHANNEL from the box to the guard: every request of the
//     run carries it, so it reaches the bridge with the run's first request and
//     ahead of its first call, with no message of its own that could arrive
//     late, be lost, or be answered out of order with a call; and it is read
//     where every request's token already is, in the routing every route under
//     the prefix takes.
//   - ONE THE RUN'S MODEL CANNOT FORGE: the fleet signs the token, the session is
//     in what it signs ([freshMark]), and by the time the model can read a file
//     the box holds no token but the one of the session it is in. The script's
//     own record of its restore — the outcome files a collection reads — is a
//     file the model can rewrite, and a report posted to a route of the bridge is
//     a request the model can make with the token it holds; either could say a
//     session was continued that was not.
//
// THE FIRST REQUEST DECIDES, for each opening of the job ([binding.begin]): the
// handshake, a listing, a call, a poll for notes or a skill's report alike, it
// tells the session which session the box started ([Session.Started]) BEFORE it
// is served, and every request that arrives meanwhile waits for that — so no
// call is admitted against the guard of a session the run is not in. A later
// request on the job's other token is refused: the box started one session, and
// that token is gone from it.
//
// A BOX THAT NEVER SAYS — a harness that never reaches its bridge — makes no
// call, so no guard is asked; and its run's record says nothing of the session
// it started, which every later rebuild of the guard reads as a session of its
// own (internal/sandbox, LaunchRecord.SessionStart): the safe direction, since a
// guard short of a skill asks the run to load it again, where one holding a
// skill its session never read lets through a call the skill exists to hold
// back.

// otherSession is why a request on a job's other token is refused once the
// opening has been told which session its box started.
const otherSession = "the job's box already started the other session, and holds this token no " +
	"longer: a box starts one session"

// begin holds the opening to the session the job's box started, as the token
// sub was read from says ([freshMark]): told to the session
// ([Session.Started]) the first time a request of the job reaches the opening,
// before that request is served, every route's alike. A request that arrives
// while it is being told waits for it. It answers false for a request on the
// job's other token once the opening has been told, which is refused
// ([otherSession]).
//
// UNDER THE RUN'S OWN CONTEXT, as every call of the run is
// ([toolsession.Dispatcher.Call]), never the request's: what the session is told
// holds for the run, whether or not the request that told it stays to be
// answered.
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
