/** Re-run: one run of one check again, named by its commit and the branch it was checked for. */

import { RotateCwGlyph } from "@crewlethq/icons/glyphs";
import { WriteButton } from "~/components/WriteButton.tsx";
import { useAct } from "~/lib/useAct.ts";
import type { CodeCheckRow } from "~/protocol/index.ts";

/** A rerun's receipt; one check's refusal comes back as a refused act, drawn under the button. */
interface RerunReceipt {
  reruns?: { name: string; attempt: number }[];
}

/** Why the press is held, or undefined where it may be pressed. */
export function rerunHeld(row: CodeCheckRow, latest: number | null): string | undefined {
  if (!row.repo) return "Its repository is not named.";
  if (row.state === "queued" || row.state === "running") {
    return "It has not ended: an ended run is run again.";
  }
  if (latest !== null && latest > row.attempt) {
    return `Attempt ${latest} is its latest: run that one again.`;
  }
  return undefined;
}

export function RerunCheck({
  row,
  latest,
  onReran,
}: {
  row: CodeCheckRow;
  /** The latest attempt known of the same check, where one is later than this one. */
  latest: number | null;
  /** The attempt the receipt queued. */
  onReran: (attempt: number) => void;
}) {
  const rerun = useAct("code_rerun_checks");
  const press = async () => {
    const result = await rerun.run(
      {
        repo: row.repo,
        sha: row.oid,
        checked_for: row.target,
        checks: [{ name: row.name, attempt: row.attempt }],
      },
      { done: `Ran ${row.name} again` },
    );
    if (result?.kind !== "applied") return;
    const one = (result.receipt as RerunReceipt | null)?.reruns?.find((r) => r.name === row.name);
    if (one) onReran(one.attempt);
  };
  return (
    <WriteButton
      write={rerun}
      size="small"
      variant="secondary"
      leadingIcon={<RotateCwGlyph size="sm" />}
      blocked={rerunHeld(row, latest)}
      title={`Run ${row.name} again as attempt ${row.attempt + 1}`}
      onPress={() => void press()}
    >
      Re-run
    </WriteButton>
  );
}
