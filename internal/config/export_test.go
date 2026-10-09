package config

// IsSecret is the predicate redaction masks by, for the gate that holds the
// dashboard's list of credential-bearing seat fields to it.
var IsSecret = isSecret
