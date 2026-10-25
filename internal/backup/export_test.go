package backup

import "github.com/crewlet/crewlet/internal/objstore"

// SetReferenceTables has s read its copies against tables rather than
// references.All — for a case declaring a table no migration creates, which
// the one list may never hold.
func SetReferenceTables(s *Service, tables []objstore.ReferenceTable) { s.tables = tables }

// SetPageObserver has s call fn after each read of a copy's references, so a
// case can count how the copy is walked.
func SetPageObserver(s *Service, fn func()) { s.pageRead = fn }

// SetDeferredPageObserver has s call fn after each read of the records a
// copy's appliers deferred, with how many records the read answered.
func SetDeferredPageObserver(s *Service, fn func(rows int)) { s.deferredRead = fn }

// DeferredPage is how many deferred records one read of a copy takes.
const DeferredPage = deferredPage

// The bounds the backup's reads of the store are held to, for the cases that
// measure them.
const (
	FetchConcurrency       = backupFetchConcurrency
	StreamCheckConcurrency = streamCheckConcurrency
)

// FetchAttempts is how many times a backup asks the store about one object
// before a failure that is not definite refuses it.
const FetchAttempts = fetchAttempts
