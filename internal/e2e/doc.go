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
// is stubbed except the vendor endpoint, because a stub anywhere else is a
// stub of the thing under test.
//
// The package has no exported surface. It exists as a package rather than as
// more files under internal/api because its subject is the composition, and a
// test that lives inside one of the components it composes tends, over time,
// to be written as a test of that component.
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
// delivery's, which the inbound edge handles in order — and never for a fixed
// time: an absence read after a sleep holds only for as long as the sleep
// happened to be long enough.
package e2e
