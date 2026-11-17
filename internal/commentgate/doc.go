// Package commentgate holds the tree's comments to what a reader of the code
// needs beside it, and internal/README.md to a map.
//
// # Every reader of the code pays for its comments
//
// A comment is read with the code it sits beside, by everybody who reads that
// code, every time, and most of this tree's readers are agents that carry what
// they read for the rest of their run. In the week to 2026-11-17 they read some
// 71M tokens of this tree's Go, half of it in three packages, and in that Go
// comments were 56% of the bytes: blocks over ten lines were one block in ten
// but 42% of all comment bytes. The argument for a decision repeated beside
// each place it applies, the history of how the code got here, and a rule
// restated where it was not decided are what made them long.
//
// So a comment says what the reader of that code needs, within blockBound
// lines: the decision, and why the obvious alternative is wrong. The argument
// behind it is written once, in the package doc, which a reader opens on
// purpose (or in an ADR, when it binds more than one package), and the comment
// points there. A package doc is not bounded: it is where the argument goes.
// History stays in the commit, the pull request and the issue.
//
// # internal/README.md is a map
//
// internal/README.md says which package does what, so a reader knows which doc
// to read; a package's decisions live in its package doc, the authority. Its
// rows had become essays, 1.7 MB in 253 lines with one row of 179 KB, and for
// several packages the row held more of the package's decisions than the doc.
// A row is one line, rowBound characters at most.
//
// # An inventory that only shrinks
//
// Both bounds arrived after the code. excessInventory records, per package
// directory, the comment lines past the bound the package held then, and
// rowInventory each long row's length; the gates refuse a package or a row
// that grows, and a number not lowered when it shrinks, so the inventory only
// moves down, and two changes to one package's count meet as a conflict on its
// line. Issue #1474 removes the entries package by package, each by folding
// the package's row into its doc and condensing its comments, and its last
// piece removes the inventory and makes both bounds absolute.
//
// The package is test-only: this file is its documentation and the gates are
// its tests.
package commentgate
