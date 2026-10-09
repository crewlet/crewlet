package backup

import "github.com/crewlet/crewlet/internal/objstore"

// SetReferenceTables has s read its copies against tables rather than
// references.All — for a case declaring a table no migration creates, which
// the one list may never hold.
func SetReferenceTables(s *Service, tables []objstore.ReferenceTable) { s.tables = tables }
