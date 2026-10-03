/**
 * The runtime audit: what a person did through a running node on the surfaces
 * that keep no record of their own — every operator tool call that is not a
 * proven read, and every `POST /backup`. The Audit log's fifth source.
 *
 * EXACTLY WHAT `internal/events/types` PUBLISHES — held there by
 * `TestTheAuditLogReadsEveryRuntimeAuditType`.
 */

/** The envelope source every runtime audit event carries, and what
 *  `events{source}` narrows to. */
export const OPERATOR_SOURCE = "operator";

/** Every event type the runtime audit writes. */
export const RUNTIME_AUDIT_TYPES = ["operator_acted", "backup_requested"] as const;
