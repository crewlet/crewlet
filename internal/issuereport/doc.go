// Package issuereport files anonymous reports of the engine's own defects,
// for a company that opted in (ADR-0101). The engine holds no GitHub
// credential: it posts a typed record to a relay, which files the issue.
//
// NOTHING CONFIDENTIAL CAN LEAVE, BY CONSTRUCTION AND NOT BY REDACTION. [Report]
// holds no plain string: each is a named type a validating constructor makes,
// an enum from the [Catalogue], a version matched by a strict pattern or a
// function of the engine's own public source. A signal's subject (a seat, a
// conversation) is hashed where it arrives, is counted in this node's memory
// and has no field on a report to reach. `TestAReportHoldsNoPlainString` holds
// the type, and `TestNoAdversarialValueReachesAReport` the path into it.
//
// ONLY A RELEASED OR CLEAN MODULE BUILD SENDS: [Origin] holds the binary to a
// release tag or a module pseudo-version, an unmodified tree and a recorded
// commit, and any other binary stays inert whatever the setting says. A
// development build's defect is often its developer's own unmerged change, and
// nothing about it belongs in a public issue.
//
// WHO HAS TO AGREE: the sent record is the whole company's, now, so it is one
// compare-and-set record per fingerprint in the coordination store
// ([coord.IssueReports]). Threshold counters are this node's alone: a seat that
// moves starts its count again, which delays a report and never doubles one.
package issuereport
