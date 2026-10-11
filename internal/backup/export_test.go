package backup

import "github.com/crewlet/crewlet/internal/objstore"

// SetReferenceTables has s read its copies against tables rather than
// references.All — for a case declaring a table no migration creates, which
// the one list may never hold.
func SetReferenceTables(s *Service, tables []objstore.ReferenceTable) { s.tables = tables }

// SetPageObserver has s call fn after each read of a copy's references, so a
// case can count how the copy is walked.
func SetPageObserver(s *Service, fn func()) { s.pageRead = fn }

// The bounds the backup's reads of the store are held to, for the cases that
// measure them.
const (
	FetchConcurrency       = backupFetchConcurrency
	StreamCheckConcurrency = streamCheckConcurrency
)
