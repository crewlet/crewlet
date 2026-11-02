// Package tabular writes a table of rows as a file a person downloads: CSV and
// XLSX, the one implementation of each (ADR-0008).
//
// # Formula escaping is the rule that matters
//
// A cell that opens with `=`, `+`, `-`, `@`, a tab or a carriage return is read
// by a spreadsheet as a formula, so a value somebody else wrote (a task title,
// a page name) could run when the person opens the file. Every such cell is
// prefixed with an apostrophe, in both formats, except a plain number, so a
// negative one stays a number. The rule lived in
// internal/artifactdata's export, and moves here so the datastore's export and
// the Dashboard Assistant's table card downloads share it rather than copying
// it.
//
// XLSX is written with the standard library alone (a minimal SpreadsheetML
// package over archive/zip, inline strings and numbers, no styles), because a
// download of rows a person already saw needs nothing a dependency would add.
package tabular
