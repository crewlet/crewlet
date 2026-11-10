// Package fixedwait is the gate that refuses a NEW FIXED WAIT in a test: a
// claim whose verdict depends on wall-clock time having passed. Its suite
// walks every Go file of the module and fails on a wait, or a reading of
// elapsed time, in test code that nothing takes out of the class and the
// inventory in allowed.go does not hold.
//
// # The class
//
// A test decides a claim on a fixed wait when time.Sleep, time.After,
// time.AfterFunc, time.NewTimer, time.NewTicker or time.Tick stands where a
// signal should, or when time.Since, time.Until, or a time.Time's Sub,
// Before, After or Compare reads the wall clock a claim is held to. It comes
// in four shapes, each failing in its own direction:
//
//   - A NEGATIVE WINDOW waits, then asserts that nothing happened. An event
//     that arrives after the window lets the test pass with the bug present,
//     and it never goes red on a slow runner, so nobody notices.
//   - A HEAD START waits so that one goroutine gets ahead of another. A
//     loaded runner loses the race: the test fails a healthy build, or passes
//     without exercising the path it names.
//   - A POSITIVE DEADLINE is a claim that something answers within a short
//     time. A loaded runner misses it and fails a healthy build.
//   - An ELAPSED BOUND is a positive deadline whose clock is READ rather than
//     waited on: time.Since(start) < d, time.Until(deadline) >= d,
//     t1.Sub(t0) held to a bound, or time.Now().Before(deadline) deciding
//     that a step came in time. It fails a healthy build when a loaded
//     runner loses time between the two readings — #736's served replies,
//     which wanted half a margin left on the wall clock, reddened main. AN
//     ORDERING of two instants the code under test stamped from the wall
//     clock is one too, though no bound is written —
//     a.CreatedAt.Before(b.CreatedAt) — since it holds only once the clock
//     has moved between the two stamps, and a coarse clock stamps two events
//     equal.
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
//   - For an elapsed bound, #736's: THE INSTANT A WAIT WAS ARMED WITH, held
//     exactly, where a fake sees the wait; THE BUBBLE'S ANSWER INSTANT where
//     none does; otherwise the signal the bound stood in for.
//   - For an ordering of two stamps, THE ORDER THE CODE ALREADY RECORDS — a
//     log position, a sequence, a seq column — asserted in place of the
//     stamps, which needs no clock at all.
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
// In test code the gate counts every reference to one of the six waits, and
// to time.Since and time.Until, through the file's own name for package
// time — the default, an alias, or with a dot import the bare name — and a
// function value counts where it is named (`sleep := time.Sleep`).
//
// It counts every reference to a Sub, Before, After or Compare WHOSE
// RECEIVER IS A time.Time, called or not, and tells each from every other
// method of its name — a big.Int's Sub, a set's, a version's Compare, a
// page's After — by the receiver's TYPE, never by a name or a shape: they
// are called on any expression, and nothing in a name says what a value is.
// Before, After and Compare decide on the span Sub reads —
// time.Now().Before(deadline) is time.Until(deadline) > 0, and
// t.Compare(d) < 0 and !t.After(d) are the claim t.Before(d) is — so they
// are readings of elapsed time as Sub is. Equal is not counted: equality
// decides nothing about elapsed time (a re-stamp asserted with it is a gap,
// below). The types come from
// internal/sourcetree's [sourcetree.TypeCheck], the module's one loader of
// its own types (the standard library's go/types over `go list -export`
// data), which type-checks each package whose test code may hold one as its
// test binary compiles it — its in-package tests with it, its external test
// package against its test variant — and only those: a package with a
// test-code file holding a selector of one of the four names, since only
// such a file can hold one. time.After through the file's own name for
// package time is not such a selector: it is package time's function, the
// wait, and never a method, since Go lets no package declare a name its files
// import a package by. So an alias of time.Time is one, a struct embedding
// one promotes its methods, and a method expression (time.Time.Before) is
// counted; a DEFINED type over time.Time (type instant time.Time) has none
// of its methods, as Go gives a defined type only the methods its own
// declaration lists, so a Sub, Before, After or Compare it declares is its
// own and is not counted until it is converted back. A file the platform the
// gate runs on does not build is in no package go list answers, and a
// selector of those names in it cannot be typed: it is refused rather than
// passed.
//
// Three things are not counted:
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
//     //fixedwait:measure <why it decides nothing>
//     //fixedwait:stated <which instants or clock>
//
//     The first three are a fixed wait's, measure is any elapsed reading's,
//     and stated is a comparison of two instants' alone, a time.Time's Sub,
//     Before, After or Compare: each covers only its own shape, and one over
//     only references it does not cover is refused, since a wait is no
//     measurement, a measurement no wait, and a reading of now never stated.
//     A marker covers ONE line, and its place decides which: trailing code,
//     it covers that code's line; alone on its line, the line under it. Never
//     both: a marker written after a liveness timer must not also take the
//     wait on the next line out of the class, where whoever reads the diff
//     would not see it.
//
//     A LINE HOLDING BOTH SHAPES — a wait sized by an elapsed reading,
//     time.After(time.Until(deadline)) — takes one marker of each, one alone
//     on the line above and one trailing it, each covering its own shape. Two
//     markers alone on consecutive lines do not do it: the first covers the
//     second's line, which holds no reference, and is stale.
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
//     free. MEASURE: the reading of elapsed time decides nothing — it is
//     logged or reported, or it is a poll's own deadline, whose firing only
//     ends a wait for a condition that decides — and the reason is required,
//     as for simulated, because a bound held to a loaded runner's clock is
//     written identically. STATED: a comparison of two instants — a
//     time.Time's Sub, Before, After or Compare — reads no wall clock between
//     them: the test stated both (constants, calendar arithmetic on them), a
//     clock the test controls stamped both (a stepped or frozen fake the code
//     under test is handed), or the code under test DERIVED one from the
//     other by its own arithmetic, computed once (an expiry it computes as
//     the instant it stored an entry plus the entry's TTL). So it may decide
//     a claim, and decides it exactly, with no runner's load between its
//     readings. The reason names those instants, that clock or that
//     derivation, and is required, because instants the code stamps from the
//     wall clock are written identically: a fake's "now" that adds the
//     time.Since of its own start is the wall clock again, and TWO
//     INDEPENDENT READINGS of the wall clock never take it, whoever made
//     them. An ordering of two such stamps is an elapsed bound, and its usual
//     fix needs no clock: assert the order the code records (a log position,
//     a sequence, a seq column) rather than the stamps. Never time.Since or
//     time.Until, which read now at the call.
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
// A duration, a shape or a name cannot tell a liveness bound from a claim,
// nor a logged elapsed time from a deadline, nor a fake's instant from the
// wall clock's. Ten seconds in
// `case <-time.After(10 * time.Second): t.Fatal(...)` is a liveness bound;
// the same select with the arms' roles swapped is a negative window, and so
// is a 50 ms sleep before an assertion that a channel is empty — or a
// simulated peer's latency, written identically.
// What a wait is FOR is the author's knowledge, so it is the author's
// statement, written at the wait and reviewed in the diff that writes it. A
// gate that guessed would pass the one wait it guessed wrong, which is the
// one it exists to stop.
//
// # The inventory
//
// allowed.go holds today's counted references, one entry per file and
// top-level function, counted by the survey's categories — the four shapes,
// the fourth as elapsed bounds; clock steps; and, of each shape, exempt for
// one the survey read and found deciding nothing on the wall clock — each
// naming the piece of the series that removes it, 2 to [LastPiece]. Keyed by
// function rather than line, so an unrelated edit never churns it; counted
// BY SHAPE, the fixed waits apart from the readings of elapsed time, so a
// wait added beside an old one fails, and so does a reading added where a
// removed wait was. The gate refuses a reference no entry covers, a function
// holding more of a shape than its entry records, and an entry recording
// more of a shape than its function holds or naming a function or file that
// is gone — so the table only shrinks. As with internal/skipgate's
// allowlist, adding an entry is the decision, and no piece of the series
// adds one.
//
// # What the gate refuses about markers
//
// A marker of no kind it knows; a simulated, a measure or a stated one with
// no reason; a bench one outside a function that takes a *testing.B; a
// marker over only references it does not cover — a measure or a stated one
// over a fixed wait, a fixed wait's over an elapsed reading, a stated one
// over time.Since or time.Until; and a marker with no counted reference at
// all on the one line it covers (a marker over a bubbled wait included, and
// every marker outside test code, where nothing is counted). A bubble
// declaration anywhere but the doc of a test-code function, or on a method
// with an exported name, is refused; so is one on a function that holds no
// wait or elapsed reading and calls no other bubble-only helper, and so is a
// bubble-only helper referred to outside a bubble, at that reference.
//
// # What the gate does not see
//
// Known gaps. Two are the same risk through none of the functions it counts:
//
//   - A window built from context.WithTimeout or context.WithDeadline — a
//     negative window whose wait is a context's — left for a gate of its own
//     if it recurs.
//   - A time.Time's Equal, which is not counted, since equality decides
//     nothing about elapsed time. But a re-stamp asserted with `!Equal` is a
//     clock-step claim the gate does not count:
//     `if b.Equal(a) { t.Fatal(...) }`, holding a second stamp off the
//     first, passes only once the wall clock has moved between them, as a
//     clock step's verdict does. And so is an Equal used as a proxy for
//     kept, unchanged or not re-stamped, read off the same entity's
//     wall-clock stamp before and after:
//     `if !after.Equal(before) { t.Fatal(...) }` passes a rebuild or a
//     re-stamp that lands inside one tick of the clock that stamped both.
//     Either is decided only by a stated instant, a clock the test
//     controls, or the order or count the code records.
//
// A bound on measured elapsed time was another, until it reddened main
// (#736): it is the fourth shape now, counted at time.Since, time.Until and a
// time.Time's Sub, Before, After and Compare. What of it the walk still does
// not see is one of those four reached where no type says time.Time —
// through an interface or a type parameter whose method set lists it — and
// an instant turned into a number first (UnixNano, Unix, UnixMilli) and
// compared or subtracted as one, which reads the same clock through none of
// them.
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
