// Package screen keeps a job's browser screen on the node holding its seat:
// the per-job hub ([Hub]) that takes what the job's relay in its box posts
// about the box's browser, on internal/browser's wire, keeps the newest of
// each, and answers the relay's one held poll with whether anybody watches.
// internal/api/mcpbridge serves the routes the relay posts to and hands each
// post to the hub of the opening its token reaches; the hub decides what it
// takes. The path a box's screen takes to its seat's holder is recorded as
// ADR-0182.
//
// # One hub a bridge opening, memory only
//
// A hub lives as long as the bridge opening of a job whose launch asked for a
// browser: the opening's end closes it, whichever end it is, and a job opened
// again (a relaunch, a recovery, its seat moved to another node) gets a hub of
// its own. Nothing survives a hub. A new one starts empty under an id of its
// own, which its poll's answer names, and a relay that sees the id change
// sends again what it holds, so nothing a hub keeps is written anywhere or
// needs anybody else to agree on it.
//
// A hub built for a launch WAITS until the job's ensure answers ready at or
// above internal/browser's floor ([Hub.Serve]), and the bridge answers every
// verb of a waiting hub as one of a job with no browser: what serves the box
// exists before the box can call, and a browser the launch withholds, or one
// below the floor, never serves. A job opened again from its record serves at
// once, the record saying its browser was ready ([HubOptions.Serving]).
//
// # The box can be anything
//
// The model holds a shell in its box, so any process there can post as the
// relay. So a hub charges every post to its limits before it reads a byte of
// it, reads it into internal/browser's closed values, refuses it whole past
// any bound, never decodes a frame past its header, and decides nothing from
// what the box says: a frame or a state changes only what the hub would show,
// and a check-in only what it would report. What it keeps is bounded by the
// wire's own bounds, one of each, and a refusal it logs names the verb and
// why, never the box's words.
//
// # The newest only
//
// A hub keeps the newest frame, the newest state and the newest check-in, and
// nothing older: a person is shown the browser as it is, and a relay that
// meets a new hub restates its newest. A frame is kept only while somebody
// watches ([Hub.Watch]), since nothing else would read it.
//
// The box numbers its frames (seq) and its states (rev) under its
// supervisor's boot, 16 random hex digits, so two boots have no order: read
// as numbers or strings, every frame after a supervisor restart was either
// dropped as not newer or an earlier boot's replay taken as new. So a post of
// a boot the hub has not seen replaces the current boot, whose frame and
// state go with it, the supervisor having started again and numbered from
// one; a post of a boot it replaced is dropped, as is one of the current boot
// no newer than the newest it took. It remembers the last eight boots it
// replaced, more supervisor restarts than one job sees. A post dropped as
// stale is answered as a taken one: a reordered or retried post is no fault.
//
// # The limits
//
// A hub charges each post to [Limits], built once a node ([NewLimits]) and
// handed to every hub on it: internal/costlimit's limiters, the engine's one
// (ADR-0008), one a verb, each charge paid by the job's bucket, keyed by its
// launch so a job opened again keeps its own, and by the node's, which every
// job's hub on the node pays into. A refusal is internal/browser's TooFast,
// carrying the wait the refusing bucket gave, which the bridge answers as its
// Retry-After.
//
// A job's frames are held to twenty a second with a burst of four: twice the
// fastest a box sends (ten, while a person controls its browser), as a bucket
// rather than a least gap between frames, since frames bunch in transit and a
// gap would drop the newer of two. Its states and check-ins are held to
// internal/browser's ReportRate, ten a second, each with a burst of four, so a
// relay that meets a new hub restates both and retries each. A node's are two
// hundred a second with a burst of forty, each verb: ten times a job's frames,
// which the browser jobs one node's memory holds (a box's browser alone peaks
// near 600 MiB) never reach watched at two frames a second, or a person
// controlling twenty, while every box on the node together posts at most
// 100 MiB of frames a second into its hubs. How many browser jobs a node holds
// at once is production's to say; past this bound a relay waits out its 429.
//
// # The held poll and the watch
//
// A relay holds one poll at a time, naming the hub it last heard and the level
// of its last answer ([browser.PollQuery]). A poll naming another hub, or a
// level below the hub's, is answered at once; any other is held until the
// level moves. One poll is held a job: a newer one answers the held one, which
// is the answer a box that opened polls without limit gets, so it holds no
// more than one goroutine and relay stream on two nodes. The level rises with
// every change of whether anybody watches, so an answer lost on its way to
// the box is made good by the next poll, which names the level it last read.
//
// Whether anybody watches is a lease a viewer's node renews ([Hub.Watch]):
// each lives [WatchLapse] past its renewal, at most [MaxWatchNodes] of them,
// and the box's screen runs only while one lives. The hub lapses them on its
// own clock, one timer for the earliest.
package screen
