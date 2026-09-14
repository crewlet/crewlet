package configapi

// RevisionStore is the revision history the service writes through, exported
// to the test package so a case can wrap it: counting the writes a dry run
// must never make is the only honest proof that it makes none.
type RevisionStore = revisionStore

// WrapRevisions replaces the service's revision history with wrap applied to
// it. The wrapper forwards to the real store, so every other behaviour is the
// one production runs.
func (s *Service) WrapRevisions(wrap func(RevisionStore) RevisionStore) {
	s.configs = wrap(s.configs)
}
