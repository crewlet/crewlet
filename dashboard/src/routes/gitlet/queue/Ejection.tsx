/** What a merge queue took out lately: a danger callout each, with why in words and its run. */

import { useMemo, useState, type ReactNode } from "react";
import { Button, Callout } from "@crewlethq/ui";
import { href } from "~/app/router.tsx";
import { AgoTime } from "~/components/AgoTime.tsx";
import { latestAttempts } from "~/lib/checks.ts";
import { changeRef, shortOID } from "~/lib/code.ts";
import { useLatestChecks } from "~/lib/useLatestChecks.ts";
import type { CodeEjectedRow, CodeEjectionTrain } from "~/protocol/index.ts";
import { changeHref } from "../paths.ts";
import { Face, useWhoName } from "../ui.tsx";
import { ejectionWords, runKey } from "./words.ts";

/** How many a box draws until asked, so a broken queue's ejections cost three runs' reads. */
export const EJECTIONS_SHOWN = 3;

/** A box's ejections, newest first, three until "Show N more" is pressed. */
export function Ejections({
  repo,
  rows,
  action,
}: {
  repo: string;
  rows: readonly CodeEjectedRow[];
  /** The callout's write for one ejection: Re-queue. */
  action?: (e: CodeEjectedRow) => ReactNode;
}) {
  const [all, setAll] = useState(false);
  const newest = useMemo(
    () => [...rows].sort((a, b) => Date.parse(b.ejected_at) - Date.parse(a.ejected_at)),
    [rows],
  );
  const shown = all ? newest : newest.slice(0, EJECTIONS_SHOWN);
  const rest = newest.length - shown.length;
  return (
    <>
      {shown.map((e) => (
        <Ejection key={e.entry_id} repo={repo} e={e} action={action?.(e)} />
      ))}
      {rest > 0 && (
        <span>
          <Button variant="ghost" size="small" onClick={() => setAll(true)}>
            Show {rest} more taken out in the last day
          </Button>
        </span>
      )}
    </>
  );
}

function Ejection({ repo, e, action }: { repo: string; e: CodeEjectedRow; action?: ReactNode }) {
  const whoName = useWhoName();
  const ref = changeRef(repo, e.number);
  const { why, onto } = ejectionWords(e);
  const by = e.enqueued_by.handle;
  return (
    <div role="group" aria-label={`Taken out of ${repo}'s queue`}>
      <Callout
        variant="danger"
        action={
          <span className="code-queue__ejection-acts">
            {e.tested_in && e.reason === "check_failed" && (
              <RunLinks repo={repo} e={e} train={e.tested_in} />
            )}
            {action}
            <a className="t-link" href={changeHref(repo, e.number)}>
              Open {ref}
            </a>
          </span>
        }
      >
        <p className="code-queue__why">
          <strong className="mono">{ref}</strong> was taken out of the queue{" "}
          <AgoTime value={e.ejected_at} />: {why}
          {onto && (
            <>
              {" "}
              <span className="mono">{shortOID(onto)}</span>
            </>
          )}
        </p>
        <p className="code-queue__who t-caption">
          {e.title && <span>{e.title} · </span>}
          <Face who={by} /> {whoName(by)}{" "}
          <span>
            enqueued it <AgoTime value={e.enqueued_at} />
          </span>
        </p>
      </Callout>
    </div>
  );
}

/** View the run of each check that failed on the train, read once and never polled. */
function RunLinks({
  repo,
  e,
  train,
}: {
  repo: string;
  e: CodeEjectedRow;
  train: CodeEjectionTrain;
}) {
  const { rows } = useLatestChecks({ repo, oid: train.commit_oid, target: e.target_ref });
  if (!rows) return null;
  const latest = latestAttempts(rows);
  const failed = (e.checks ?? []).slice(0, EJECTIONS_SHOWN).flatMap((name) => {
    const row = latest.find((r) => r.name === name);
    return row ? [row] : [];
  });
  if (failed.length === 0) {
    return <span className="t-caption">Its run is no longer kept.</span>;
  }
  return (
    <>
      {failed.map((row) => (
        <a
          key={row.name}
          className="t-link"
          href={href(["gitlet", "checks"], { run: runKey({ ...row, repo }) })}
        >
          {failed.length === 1 && (e.checks?.length ?? 0) === 1
            ? "View the run"
            : `View ${row.name}'s run`}
        </a>
      ))}
    </>
  );
}
