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
package e2e
