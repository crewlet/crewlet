package sandbox

// A BOX GOES BEFORE ITS ENGINE when the host runs out of memory. A box's
// processes descend from the engine and inherit its oom_score_adj, and the
// engine (broker, store, every turn on the node) is usually its host's largest
// process: a killed run is collected as the failed run it is, a killed engine
// takes every seat on the node down. So every box process is raised, before it
// runs anything, to [boxOOMScoreAdj] of the engine's value; off Linux there is
// no such knob ([boxOOMScore]).
//
// It is an ORDER, not a boundary: a fully permissioned box may lower its value
// again, down to what a privileged process last set. What a kill does to the
// rest of the engine's tree is the service manager's (systemd's OOMPolicy, the
// kubelet's memory.oom.group): docs/guides/deployment.md.

// oomScoreScale is the unit oom_score_adj is counted in: thousandths of the
// memory the kernel is choosing a process to kill for — the host's, or a
// memory cgroup's when its limit is what ran out. A process's score is the
// memory it holds in those same thousandths plus its adjustment.
const oomScoreScale = 1000

// oomScoreAdjMax is the kernel's ceiling on oom_score_adj (OOM_SCORE_ADJ_MAX):
// a process at it is the kernel's first choice whatever it holds, beside any
// process below it.
const oomScoreAdjMax = 1000

// boxOOMScoreAdj is the oom_score_adj a box's processes run at beside an
// engine at engine: the engine's plus the kernel's whole scale, held to the
// ceiling.
//
// THE WHOLE SCALE: no process holds more than all the memory, so the engine
// scores at most its adjustment plus [oomScoreScale] and a box at least its
// own; any smaller lead lets an engine holding enough more memory go first.
// RELATIVE TO THE ENGINE'S, not the ceiling, because the engine's value is its
// deployment's decision: a Guaranteed pod at -997 puts its boxes at 3, ahead
// of its engine but not of every other pod's work. Raising needs no privilege
// (going below what a privileged process set needs CAP_SYS_RESOURCE), so
// this asks for nothing the engine's user lacks.
func boxOOMScoreAdj(engine int) int { return min(engine+oomScoreScale, oomScoreAdjMax) }
