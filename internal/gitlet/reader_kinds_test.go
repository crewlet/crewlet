package gitlet_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
)

// EVERY PRINCIPAL KIND THAT ASKS IS A READER THE AUDIT RECORDS, by one
// spelling. A `visibility: unit` read is recorded as its principal kind
// (types.CodeFetched), and the usage domain refuses a kind it does not know,
// so a kind added here alone would have its reads refused at the writer. The
// sets are spelled apart only because the usage domain sits beneath this
// package. PrincipalSystem, carried only by the ref expiry's push, must not
// pass Validate as an asker: the audit cannot record it.
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
