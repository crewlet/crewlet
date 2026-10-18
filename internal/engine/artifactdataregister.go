package engine

// artifactDataRegistrations is the artifact datastore's entry in the register
// ([register]), after the artifacts domain: its declaration, applier, write
// seams, barrier, generation record, node gate and Tier A ceiling
// (`stream.artifact_data_log_max_bytes`, read through
// config.Stream.ArtifactDataMaxBytes).
//
// IT HOLDS NO ENTRY until internal/artifactdata exists to declare one, for
// [artifactsRegistrations]' reason.
func artifactDataRegistrations() []registration {
	return nil
}
