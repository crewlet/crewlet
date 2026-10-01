package clientsource

// Exports is [exports], for the contract's own suite: the walk that holds the
// contract directory to "everything it exports is a row" has no caller
// outside a test, and exporting it would make it a reader a gate could reach
// for instead of the one the contract names.
var Exports = exports
