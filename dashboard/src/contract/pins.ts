/**
 * The artifact listing's page, as the sidebar's pinned list asks for it.
 *
 * A CONTRACT MODULE OF ITS OWN, because the sidebar reads it and the sidebar
 * is in the entry: `contract/artifacts.ts` is not, and a bundler places a
 * module whole, so reading this from there would carry every declaration of
 * it into every reader's first paint.
 */

/**
 * The most artifacts one listing answers (`artifactsdomain.MaxListLimit`): a
 * larger `max-keys` is read as this. Held by
 * `internal/api/artifactsapi.TestTheDashboardBoundsArtifactTextAtTheEnginesCaps`.
 */
export const ARTIFACT_LIST_MAX = 200;
