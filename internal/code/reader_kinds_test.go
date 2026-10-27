package code_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/events/types"
)

// EVERY PRINCIPAL KIND IS A READER THE AUDIT RECORDS, by one spelling.
//
// A read of a `visibility: unit` repository is recorded as the principal kind
// that read it (types.CodeFetched), and the usage domain that keeps the
// records refuses a kind it does not know — so a principal kind added here
// without its reader there would have every read by it refused at the writer
// and counted as a reader the day elided. The two sets are spelled apart only
// because the usage domain sits beneath this package in the import graph.
//
// Mutation: add a principal kind to code.PrincipalKinds alone, and this names
// it.
func TestEveryPrincipalKindIsAReaderTheAuditRecords(t *testing.T) {
	t.Parallel()
	var principals, readers []string
	for _, k := range code.PrincipalKinds() {
		principals = append(principals, string(k))
	}
	for _, k := range types.CodeReaderKinds() {
		readers = append(readers, string(k))
	}
	if !slices.Equal(principals, readers) {
		t.Errorf("the code host's principal kinds are %v and the reader kinds a read is recorded as are %v",
			principals, readers)
	}
}
