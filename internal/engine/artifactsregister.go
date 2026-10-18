package engine

// artifactsRegistrations is the artifacts domain's entry in the register
// ([register]): its declaration, applier, write seams, barrier, generation
// record, node gate and Tier A ceiling (`stream.artifacts_log_max_bytes`, read
// through config.Stream.ArtifactsMaxBytes).
//
// IT HOLDS NO ENTRY until internal/artifactsdomain exists to declare one, and
// an empty slice is the whole of that state: [checkRegister] refuses an entry
// that omits a field, so a half-written entry here would be a boot failure,
// never a node that runs a domain it cannot apply.
func artifactsRegistrations() []registration {
	return nil
}
