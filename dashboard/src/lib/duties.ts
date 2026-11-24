/**
 * A fleet duty, by what it does: the engine names each by its lease key
 * (`iam_sweep`, `atlas-collect`), the name two builds share in a rolling
 * upgrade, which is a coordination key and not a word a reader knows.
 *
 * WORDS ONLY. A duty this build has no words for is drawn by its key, so a
 * duty a newer engine adds is still listed; the key stays on the row's title
 * for the operator who matches it against a log line.
 */
export const DUTY_WORDS: Readonly<Record<string, string>> = {
  scheduler: "Schedules",
  retention: "Log trimming",
  "code-retention": "Code history trimming",
  "chat-prune": "Chat history trimming",
  "object-collector": "File storage cleanup",
  iam_sweep: "Sign-in cleanup",
  "integration-reconcile": "Integration sync",
  "launch-node-watch": "Runs of nodes gone for good",
  learning: "Agent learning",
  embeddings: "Search embeddings",
  maintenance: "Maintenance",
  "embedded broker": "Event broker",
  "atlas-collect": "Atlas collection",
  "atlas-wake": "Atlas wake-ups",
  "delegated-decisions": "Delegated decision deadlines",
  "trash-reaper": "Trash emptying",
};

/** A duty's name in words, or its key where this build has none. */
export function dutyName(duty: string): string {
  return DUTY_WORDS[duty] ?? duty;
}
