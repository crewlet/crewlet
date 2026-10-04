package memobj_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/objstore/objstoretest"
)

// THE TWIN IS CERTIFIED BY THE SAME CASES AS THE REAL BACKENDS, so a test
// that passes against it is a test of the contract.
func TestContract(t *testing.T) {
	t.Parallel()
	objstoretest.Run(t, func(*testing.T) objstore.Backend { return memobj.New() }, objstoretest.Options{})
}
