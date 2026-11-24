/**
 * What a removal did with the stored values it was asked to delete, in the
 * words its toast says, for every remove dialog (a model's, an MCP server's).
 */

import type { SecretDeletion } from "~/contract/config.ts";
import { listed } from "./format.ts";

/** Where a person deletes a value a removal left behind. */
export const SECRETS_PLACE = "Settings › Secrets";

/** Whether any value may still be stored, which the toast warns about. */
export const someFailed = (secrets: readonly SecretDeletion[]) =>
  secrets.some((s) => s.outcome === "failed");

/** What became of each value, in a sentence or two; undefined where none was asked. */
export function removedSecretsWords(secrets: readonly SecretDeletion[]): string | undefined {
  const deleted = secrets.filter((s) => s.outcome === "deleted" || s.outcome === "absent");
  const kept = secrets.filter((s) => s.outcome === "kept");
  const failed = secrets.filter((s) => s.outcome === "failed");
  const one = (list: readonly SecretDeletion[], it: string, them: string) =>
    list.length === 1 ? it : them;
  const lines = [
    ...(deleted.length ? [`Deleted ${listed(deleted.map((s) => s.name))}.`] : []),
    ...(kept.length
      ? [
          `Kept ${listed(kept.map((s) => s.name))}: something else now uses ${one(kept, "it", "them")}.`,
        ]
      : []),
    ...(failed.length
      ? [
          `${listed(failed.map((s) => s.name))} may not have been deleted: delete ${one(failed, "it", "them")} in ${SECRETS_PLACE}.`,
        ]
      : []),
  ];
  return lines.length ? lines.join(" ") : undefined;
}
