// Package e2e holds the gates that only a whole running node can answer.
//
// Everything else in this tree tests one seam: a store against its contract, a
// phase against a scripted model, a socket against a fake queue. Those catch
// what is wrong INSIDE a component. What they cannot catch is a
// misunderstanding BETWEEN two of them that both sides implement consistently
// with their own tests — an engine publishing an event shape the projection
// does not key on, a projection pushing a slice the client does not read.
//
// So the tests here run the real thing: a real engine on a real embedded
// stream, the real API in front of it, a real turn driven by a scripted model,
// and the dashboard's OWN modules consuming the frames that come out. Nothing
// is stubbed except the model itself: the harness every model call goes through is
// a real backend launching a real process — this test binary, re-executed,
// driving its own loop as Claude Code does, calling the seat's tools over the
// real relay and session and asking the script for each model response — and
// every seat's executor is a real run in a real `direct` box, launched by the
// real Claude Code runner, whose `claude` is this binary again, calling the
// seat's tools over the node's real bridge and asking the same script
// (codingcli_test.go). A stub anywhere else is a stub of the thing under test.
//
// The package has no exported surface. It exists as a package rather than as
// more files under internal/api because its subject is the composition, and a
// test that lives inside one of the components it composes tends, over time,
// to be written as a test of that component.
//
// # The stand-in executor harness and the client half
//
// A case that forms a cluster runs in `make test-solo`, not `make test`.
//
// The executor's harness stands in for Claude Code and files its session as
// Claude Code does: a rooted chain whose every record names the release it
// answered, so a relaunch's reading of a kept session reads what the stand-in
// wrote. Its cut-off mode files only some of the calls it makes, so the telling
// by that reading is held end to end (exacttelling_test.go).
//
// replay.mjs is the one piece of code in internal/ that is not Go: the CLIENT
// half of golden_test.go's wire-protocol gate. It pushes the captured frames,
// the /operator/act answers (each with the questions that read the log its
// write made) and what the captured tab was pushed in the company's own chat
// through the committed static/protocol/protocol.js (built beside the embedded
// tree, never in it) under plain `node`, and fails on anything the client
// cannot read. It sits beside golden_test.go because the two are one
// test that changes together: not under dashboard/, since it is no test of the
// dashboard (its Vitest suites are).
//
// # How a case here is written
//
// A case that stands up ONE node, or a stateless node beside one data node,
// calls t.Parallel: each has its own directories, its own scripted model and
// its own broker, so nothing is shared but the machine. What used to be
// shared is the process environment, and a node is HANDED its environment
// instead (nodeSpec.env, nodeEnvironment) — t.Setenv cannot run beside
// t.Parallel, and a case that read the runner's environment held only on
// runners that agreed with it.
//
// A case that stands up a FLEET calls noParallel, and runs before every
// parallel case starts, alone; the fleet's claims share one fleet
// (TestAFleetOfThree) rather than each paying a boot and a teardown.
//
// A case asserting that something did NOT happen waits for the record that
// says the thing was decided — a delivery's notification_skipped, or a later
// delivery's, which the inbound edge handles in order; for a Mattermost post,
// each node's answer to a ping sent behind it, which a node's socket reads in
// order — and never for a fixed time: an absence read after a sleep holds only
// for as long as the sleep happened to be long enough. Each of those orders is
// held by a case of its own, since a settle that stopped holding would let
// every absence behind it pass without failing.
package e2e
