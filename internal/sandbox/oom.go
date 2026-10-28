package sandbox

// A BOX GOES BEFORE ITS ENGINE when the host runs out of memory.
//
// Every process a local box runs descends from the engine, and a child takes
// its parent's oom_score_adj: the kernel's bias, in thousandths of the memory
// it is choosing for, when it picks a process to kill because that memory ran
// out. Left at the engine's own, a coding CLI and the builds it runs competed
// with the engine on size alone, and the engine — the embedded broker, the
// store and every turn on the node — is usually the largest process on its
// host, so the kernel took the node to spare a run. A box is the most
// expendable work an engine host holds: a run the kernel kills is collected as
// the failed run it is, its partial output and exit status with it, while the
// engine killed takes every seat on the node down at once.
//
// So every process a box runs stands ahead of the engine: raised, before it
// runs anything, to [boxOOMScoreAdj] of the engine's own value. On a host with
// no such knob for an unprivileged process (macOS, whose memory-pressure
// priorities are the system's) a box and its engine compete as any two
// processes do.
//
// It is an ORDER for the kernel and not a boundary: an unprivileged process may
// lower its own value again, down to what a privileged one last set, and a
// fully permissioned box can do that. What a kill then does to the rest of
// the engine's process tree is the service manager's (systemd's OOMPolicy, the
// kubelet's memory.oom.group), which docs/guides/deployment.md walks through.

// oomScoreScale is the unit oom_score_adj is counted in: thousandths of the
// memory the kernel is choosing a process to kill for — the host's, or a
// memory cgroup's when its limit is what ran out. A process's score is the
// memory it holds in those same thousandths plus its adjustment.
const oomScoreScale = 1000

// oomScoreAdjMax is the kernel's ceiling on oom_score_adj (OOM_SCORE_ADJ_MAX):
// a process at it is the kernel's first choice whatever it holds, beside any
// process below it.
const oomScoreAdjMax = 1000

// boxOOMScoreAdj is the oom_score_adj every process of a box runs at beside
// an engine running at engine: the engine's value plus the kernel's whole
// scale, held to the kernel's ceiling.
//
// THE WHOLE SCALE, because it is the one lead that holds whatever the two
// hold. No process holds more than all of the memory, so the engine's score
// is at most its adjustment plus [oomScoreScale], and a box's is at least its
// own adjustment: a box that much ahead outranks the engine even when the
// engine holds nearly everything and the box nearly nothing. Any smaller lead
// is a size past which the engine goes first: at +500, an engine holding half
// of the memory more than the box.
//
// RELATIVE TO THE ENGINE'S, never the ceiling outright, because the engine's
// own value is a decision its deployment made, and the box is placed by that
// decision rather than over it: a Kubernetes Guaranteed pod runs at -997 so
// its node's kernel takes it last, and its boxes at 3 go before its engine
// without going before every other pod's work on the node. An engine at -1000
// (never killed) puts its boxes at 0. An engine above 0 — a Burstable or
// BestEffort pod — leaves less than the whole scale below the ceiling, and its
// boxes stand at the ceiling: the furthest ahead the kernel lets them.
//
// Raising its own value is the one direction the kernel lets an unprivileged
// process move it — going below what a privileged process set needs
// CAP_SYS_RESOURCE — so this asks for nothing the engine's user lacks.
func boxOOMScoreAdj(engine int) int { return min(engine+oomScoreScale, oomScoreAdjMax) }
