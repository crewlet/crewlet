// Package artifactdataapi is the HTTP surface for an artifact's datastore:
// writes, undo, the schema, the CSV export, and the read questions (documents,
// one document, a query, the history and the schema) that the dashboard and the
// socket ask.
//
// A WRITE IS HTTP, never a socket frame, because a lost answer must be retried
// as the same operation under its Idempotency-Key. The route admits any
// participant; the collection rules decide each op in the applier, from the
// authority this surface stamps. Every read path applies the collection's read
// rule, so a row hidden from a reader is hidden from its history and its export
// too.
package artifactdataapi
