// Package inbox is a person's inbox as every native domain keeps one: the
// MARKS a person lays over that domain's notices, the one GESTURE that moves
// them, how a page of notices reads against them, and the MOVEMENT a domain's
// applier announces when a committed batch moved somebody's inbox.
//
// # One set of rules, kept by each domain on its own log
//
// The tracker wrote these rules first, and it is not the only domain whose
// changes are news to a person: the code host's are too. A second copy of the
// rules would drift exactly where an inbox cannot afford it — a read mark one
// copy prunes and the other keeps, a snooze one honours past its instant — and
// nobody would see the drift until a person's inbox resurfaced what they had
// read. So the rules live here, and each domain keeps the MARKS on its own log,
// beside the notices they mark: a person's tracker marks on the tracker's
// person record. One record for every domain would hold positions on several
// logs in one number space, which nothing can compare — a position is a
// coordinate on ONE stream ([statelog.Position]) — and a gesture crossing two
// logs would be two decides that cannot be one.
//
// # What a domain brings, and what it keeps
//
// A domain states the log its notices are filed on and the reasons it routes
// under ([CheckGesture]), looks up where each notice a gesture names sits
// inside its own decide, and stores the [Marks] its record holds. It keeps its
// notification rows, their columns and the scan over them: a page of notices
// is that domain's own read, filtered IN THE SCAN by the sets a [Classifier]
// holds and classified by the same Classifier, so the filter and the marks a
// page carries are one rule stated once.
//
// It is a LEAF above the state log's position and the standard library: every
// domain that keeps an inbox imports it, and nothing here reaches back into
// one.
package inbox
