// Package fixedwait is the gate that refuses a NEW FIXED WAIT in a test: a
// claim whose verdict depends on wall-clock time having passed. Its suite
// walks every Go file of the module and fails on a wait in test code that
// nothing takes out of the class and the inventory in allowed.go does not
// hold.
//
// # The class
//
// A test decides a claim on a fixed wait when time.Sleep, time.After,
// time.AfterFunc, time.NewTimer, time.NewTicker or time.Tick stands where a
// signal should. It comes in three shapes, each failing in its own direction:
//
//   - A NEGATIVE WINDOW waits, then asserts that nothing happened. An event
//     that arrives after the window lets the test pass with the bug present,
//     and it never goes red on a slow runner, so nobody notices.
//   - A HEAD START waits so that one goroutine gets ahead of another. A
//     loaded runner loses the race: the test fails a healthy build, or passes
//     without exercising the path it names.
//   - A POSITIVE DEADLINE is a claim that something answers within a short
//     time. A loaded runner misses it and fails a healthy build.
//
// The survey behind crewlet/crewlet#734 found more than three hundred of
// them, and the series that issue tracks removes them a package at a time.
// This gate lands first, so the class stops growing while the series shrinks
// it.
//
// # The house patterns
//
// Each fix already merged is a pattern to copy, with the red-before evidence
// and the mutants that came with it:
//
//   - A SYNCTEST BUBBLE where the claim is about the clock (#634 in the state
//     log, #644 in the engine, #652 in the estate router): inside
//     synctest.Test a timer fires at exactly its time, and time moves only
//     when every goroutine of the case is blocked, so a deadline is exact
//     arithmetic rather than a bet on how late a loaded runner wakes.
//   - A WITNESS SIGNAL, A FENCE through the same channel, or THE PRODUCER'S
//     OWN COUNT OR STATE for "nothing happened" (#642, the sandbox and
//     procgroup): the absence is read off something that would have recorded
//     the event, after something that must come behind it.
//   - A WAIT ON THE PARKED STATE for a head start (#646, the engine): the
//     goroutine that must be ahead is waited until it is observably where
//     the case needs it, rather than for long enough that it probably is.
//
// # What the gate counts
//
// Test code is every `*_test.go` file, and every file of a package whose
// directory name ends in "test" — Go's own convention for a test-support
// package, which this repository follows (queuetest, coordtest, storetest,
// llmtest and the rest), computed from the directory rather than listed.
// The walk is internal/sourcetree's, and it skips every testdata directory,
// which nothing compiles.
//
// In test code the gate counts every reference to one of the six functions
// through the file's own name for package time — the default, an alias, or
// with a dot import the bare name — and a function value counts where it is
// named (`sleep := time.Sleep`). Three things are not counted:
//
//   - A reference INSIDE A BUBBLE: lexically inside the function literal
//     handed to synctest.Test or synctest.Run, through the file's own name
//     for testing/synctest. A BUBBLE RUNNER counts as synctest itself: a
//     top-level function of test code that hands one of its function
//     parameters whole to synctest.Test or synctest.Run and uses it nowhere
//     else (the inBubble helpers #644 and #652 wrote), so a literal handed to
//     it in that position runs in a bubble. A runner is found from its body,
//     never its name. A call is to it only from a file of its own directory
//     AND its own package clause — the external test package beside it may
//     declare a function of the same name that runs its parameter on the
//     machine's clock — and only where the enclosing top-level declaration
//     declares no local of that name (a parameter, a variable, a function
//     literal's parameter), which would be the callee instead. The file's
//     name for synctest is held the same way.
//
//   - A reference inside a BUBBLE-ONLY HELPER, a function whose doc carries
//
//     //fixedwait:bubble
//
//     That declaration is VERIFIED AT EVERY USE, in test code and
//     production code alike: every reference to the helper must sit inside a
//     bubble, inside another bubble-only helper, or be the function synctest
//     or a runner is handed. A helper is a function its callers choose where
//     to run, so a declaration nobody checked would be a marker that took
//     every caller's waits out of the class, the ones outside a bubble
//     included. Without types the uses are found by name — the conservative
//     direction: an identifier of the helper's name in its directory, a
//     selector of its name for a method, and an exported function reached
//     through an importing file's own name for its package — so a
//     bubble-only helper takes a name nothing else in its package uses. A
//     METHOD WITH AN EXPORTED NAME cannot be declared: it is reached where
//     nothing names its package — through an interface, an embedding, a
//     value another package hands back — so no use found by name holds
//     every caller, and its declaration is refused and its waits counted.
//     Unexport the method, or hand the bubble its waits as a literal. A
//     declaration on a function that holds no wait and calls no other
//     bubble-only helper is stale: it binds every caller for nothing.
//
//   - A reference under an EXPLICIT MARKER:
//
//     //fixedwait:liveness
//     //fixedwait:simulated <what it stands in for>
//     //fixedwait:bench
//
//     A marker covers ONE line, and its place decides which. Trailing code,
//     it covers that code's line; alone on its line, the line under it.
//     Never both: a marker written after a liveness timer must not also take
//     the wait on the next line out of the class, where whoever reads the
//     diff would not see it.
//
//     LIVENESS: the wait only bounds how long the test waits for something
//     that must happen — its firing can only fail the test (a select arm
//     into t.Fatal), or it paces a poll whose own condition decides. Such a
//     bound looks exactly like a POSITIVE DEADLINE, so ask what a broken
//     build does. If another assertion catches it, or it never meets the
//     condition at all (or meets it only far past any bound a test would
//     wait — a sweep an hour away), the bound is liveness. If it would meet
//     the condition LATER, inside such a span, the bound is the only check
//     that catches the bug: it is the claim, and belongs in a bubble or in a
//     signal the claim reads, and a sleep pacing that poll belongs with the
//     claim. So a drain that must end at once, bounded by a timer whose arm
//     fails "still waiting" while the bug would end it at its grace, is a
//     positive deadline; so is a poll for a re-armed heartbeat bounded at a
//     third of the heartbeat, or for a nudge bounded at half the fallback
//     poll the bug would wait for. SIMULATED: the wait plays the world's
//     latency inside a fake (a slow peer, a harness's kill latency), and the
//     reason is required, because this is where a deciding wait would hide.
//     BENCH: a benchmark's own work, in a function that takes a *testing.B,
//     and refused anywhere else, where it would pass a deciding wait for
//     free.
//
// # A wait that steps the clock
//
// A wait that only moves the wall clock past an instant the code under test
// stamped, so that a later stamp differs, is in the class — its verdict
// depends on wall-clock time having passed — though it cannot flake. No
// marker fits it, and simulated does not cover it: its fix is a clock the
// test controls (internal/clocktest, or a Now the code under test is
// handed), which removes the wait. The inventory counts these as clock
// steps.
//
// # Why markers, and never inference
//
// A duration, a shape or a name cannot tell a liveness bound from a claim.
// Ten seconds in `case <-time.After(10 * time.Second): t.Fatal(...)` is a
// liveness bound; the same select with the arms' roles swapped is a
// negative window, and so is a 50 ms sleep before an assertion that a
// channel is empty — or a simulated peer's latency, written identically.
// What a wait is FOR is the author's knowledge, so it is the author's
// statement, written at the wait and reviewed in the diff that writes it. A
// gate that guessed would pass the one wait it guessed wrong, which is the
// one it exists to stop.
//
// # The inventory
//
// allowed.go holds today's counted references, one entry per file and
// top-level function, counted by the survey's categories (the three shapes,
// clock steps, and exempt for one the survey read and found deciding
// nothing), each naming the piece of the series that removes it, 2 to
// [LastPiece]. Keyed by function rather than line, so an unrelated edit never
// churns it; counted, so a wait added beside an old one fails. The gate refuses a reference no entry covers, a
// function holding more references than its entry records, and an entry
// recording more than its function holds or naming a function or file that
// is gone — so the table only shrinks. As with internal/skipgate's
// allowlist, adding an entry is the decision, and no piece of the series
// adds one.
//
// # What the gate refuses about markers
//
// A marker of no kind it knows, a simulated one with no reason, a bench one
// outside a function that takes a *testing.B, and a marker with no counted
// reference on the one line it covers (a marker over a bubbled wait
// included, and every marker outside test code, where nothing is counted).
// A bubble declaration anywhere but the doc of a test-code function, or on
// a method with an exported name, is refused; so is one on a function that
// holds no wait and calls no other bubble-only helper, and so is a
// bubble-only helper referred to outside a bubble, at that reference.
//
// # What the gate does not see
//
// Known gaps. Two are the same risk through none of the six functions:
//
//   - A window built from context.WithTimeout or context.WithDeadline — a
//     negative window whose wait is a context's — left for a gate of its own
//     if it recurs.
//   - A bound on measured elapsed time (time.Since(start) < d) — a positive
//     deadline whose clock is read rather than waited on. It reddened main
//     once (#736), and piece 1b of crewlet/crewlet#734 extends this gate to
//     count it.
//
// The others are time functions the walk reads in a way it cannot prove:
//
//   - A fake in a package whose directory name does not end in "test"
//     (internal/objstore/s3obj/s3fake): its own files are not test code to
//     this walk, so a wait in them is not counted; a wait in its tests is.
//     Its three waits pace a simulated bucket today, and piece 9 of the
//     series renames the package s3faketest, which closes the gap.
//   - "Inside a bubble" is judged LEXICALLY: a function literal written
//     inside synctest's literal is taken as bubbled, though one handed to
//     something declared outside the bubble — the outer t's Cleanup, a
//     goroutine pool started before it — runs after the bubble, on the
//     machine's clock. Such a wait is not counted.
//   - A bubble runner of another package: a literal handed to one is counted
//     as outside a bubble, the safe direction.
//
// # The gate's own tests
//
// One fixture tree per rule under testdata/, each a module of its own, run
// through [Scan] and [Check] in both directions; and the gate itself, which
// walks this module.
package fixedwait
