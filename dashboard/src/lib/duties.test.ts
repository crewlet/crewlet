import { expect, test } from "vitest";
import { DUTY_WORDS, dutyName } from "./duties.ts";

// Every duty the engine claims through `workerDuty`, by its lease key (the
// constants in internal/engine, internal/schedule, internal/atlas* and
// internal/trashreap). A duty with no words is drawn on Nodes by its key.
const ENGINE_DUTIES = [
  "scheduler",
  "retention",
  "code-retention",
  "chat-prune",
  "object-collector",
  "iam_sweep",
  "integration-reconcile",
  "launch-node-watch",
  "learning",
  "embeddings",
  "maintenance",
  "atlas-collect",
  "atlas-wake",
  "delegated-decisions",
  "trash-reaper",
];

test("every duty the engine claims has words", () => {
  expect(ENGINE_DUTIES.filter((duty) => !(duty in DUTY_WORDS))).toEqual([]);
});

test("a duty a newer engine adds is drawn by its key", () => {
  expect(dutyName("a-new-duty")).toBe("a-new-duty");
});
