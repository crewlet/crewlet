package gitlet_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
)

// EVERY PRINCIPAL KIND THAT ASKS IS A READER THE AUDIT RECORDS, by one
// spelling.
//
// A read of a `visibility: unit` repository is recorded as the principal kind
// that read it (types.CodeFetched), and the usage domain that keeps the
// records refuses a kind it does not know — so a principal kind added here
// without its reader there would have every read by it refused at the writer
// and counted as a reader the day elided. The two sets are spelled apart only
// because the usage domain sits beneath this package in the import graph.
// Every kind asks but the engine's own account, which only the ref expiry's
// push carries — held here too: one that Validate admitted as an asker would
// be a reader the audit cannot record.
//
// Mutations: add a principal kind to gitlet.PrincipalKinds alone, and this names
// it; admit gitlet.PrincipalSystem in Principal.Validate, and this names it.
func TestEveryPrincipalKindIsAReaderTheAuditRecords(t *testing.T) {
	t.Parallel()
	var principals, readers []string
	for _, k := range gitlet.PrincipalKinds() {
		if k == gitlet.PrincipalSystem {
			if err := (gitlet.Principal{Kind: k}).Validate(); err == nil {
				t.Errorf("the engine's own account is a principal that may ask, and no reader kind records it")
			}
			continue
		}
		principals = append(principals, string(k))
	}
	for _, k := range types.CodeReaderKinds() {
		readers = append(readers, string(k))
	}
	if !slices.Equal(principals, readers) {
		t.Errorf("Gitlet's principal kinds are %v and the reader kinds a read is recorded as are %v",
			principals, readers)
	}
}
