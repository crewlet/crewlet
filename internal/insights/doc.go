// Package insights turns the advisor's findings about how a company works into
// typed weekly records an opted-in engine may share (ADR-0119).
//
// A [Record] has no plain string: a catalogue class, the ISO week, the release
// tag or `main`, and bucketed facts, each made only by [Records] from findings
// the advisor's deterministic detectors raised, never from a model.
//
// Every finding of a class folds into one record, so two companies with the
// same pattern send the same bytes and no field joins one record to another.
package insights
