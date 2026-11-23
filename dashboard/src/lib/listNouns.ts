/**
 * What the items of each folded list are called, so its overflow says "+2 more
 * problems" rather than a bare "+2" (the kit's `overflowWords` and `ListOverflow`).
 */
import { overflowWords, type ListNoun, type TableTagsCellLabels } from "@crewlethq/ui";

const noun = (one: string, other = `${one}s`): ListNoun => ({ one, other });

export const AGENT_NOUN = noun("agent");
export const LABEL_NOUN = noun("label");
export const PARTICIPANT_NOUN = noun("participant");
export const PROBLEM_NOUN = noun("problem");
export const ROLE_NOUN = noun("role");
export const SOURCE_NOUN = noun("source");
export const SUBJECT_NOUN = noun("subject");
export const TOOL_NOUN = noun("tool");
export const USE_NOUN = noun("use");
export const WRITER_NOUN = noun("writer");

/** A table's tag cells, counted by what their tags are. */
export const ROLE_LABELS: Partial<TableTagsCellLabels> = {
  more: (n) => overflowWords(n, ROLE_NOUN),
};
export const LABEL_LABELS: Partial<TableTagsCellLabels> = {
  more: (n) => overflowWords(n, LABEL_NOUN),
};
