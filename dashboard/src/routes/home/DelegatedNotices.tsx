/**
 * What delegates decided for the reader, said on Home (ADR-0125): the day's
 * digest ("Delegates decided 12 requests yesterday") and the denials a
 * delegate judged harmful, each linking to Settings › Delegated decisions.
 *
 * ONLY FOR WHOEVER THE ENGINE ADMITS. The read is an administrator's and the
 * people a rule escalates to; the shell cannot tell the second from anybody,
 * so it asks for every signed-in reader and draws nothing on a refusal, an
 * unknown question (an engine with no access log) or a failure.
 *
 * PUT AWAY PER DAY, in this browser: the digest by the company day it covers,
 * the flagged denials by the newest one, so tomorrow's digest or a later flag
 * brings its notice back.
 */

import { useState } from "react";
import { ButtonLink, Callout, IconButton } from "@crewlethq/ui";
import { FlagGlyph, ListChecksGlyph, XGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import type { ViewerState } from "~/lib/viewer.ts";
import { newestFlag, readDismissed, writeDismissed } from "~/lib/delegation.ts";
import { fmtCount } from "~/lib/format.ts";

/** How often Home asks again: the digest moves once a day. */
const NOTICES_POLL_MS = 600_000;

/** The browser's own storage, read through the module's never-throwing helpers. */
const local = () => window.localStorage;

/** "12 requests", "1 request", "50+ requests": a page's count, and more where the log holds it. */
function requests(n: number, more: boolean): string {
  return `${fmtCount(n)}${more ? "+" : ""} ${n === 1 && !more ? "request" : "requests"}`;
}

export function DelegatedNotices({ viewer }: { viewer: Pick<ViewerState, "owner" | "loading"> }) {
  const read = useQuery("delegated_decisions", undefined, {
    enabled: !viewer.loading && viewer.owner !== "",
    pollMs: NOTICES_POLL_MS,
  });
  const [dismissed, setDismissed] = useState(() => readDismissed(local));
  const data = read.error ? null : read.data;
  if (!data) return null;
  // AN ANSWER WITHOUT ITS LISTS says nothing was decided, never a crash on Home.
  const decidedRows = data.decided ?? [];
  const flaggedRows = data.flagged ?? [];
  const flaggedAt = newestFlag(flaggedRows);
  const digest = decidedRows.length > 0 && dismissed.decided !== data.day;
  const flagged = flaggedRows.length > 0 && dismissed.flagged !== flaggedAt;
  if (!digest && !flagged) return null;
  const putAway = (next: { decided?: string; flagged?: string }) => {
    writeDismissed(local, next);
    setDismissed((was) => ({ ...was, ...next }));
  };
  return (
    <div className="col gap-2">
      {flagged && (
        <Callout
          variant="warning"
          icon={<FlagGlyph size="md" />}
          title={`A delegate flagged ${requests(flaggedRows.length, data.flagged_more)} as harmful`}
          action={
            <span className="row gap-1">
              <ButtonLink
                size="small"
                variant="secondary"
                href={href(["settings", "delegated-decisions"], { verdict: "deny" })}
              >
                Review
              </ButtonLink>
              <IconButton
                size="sm"
                variant="ghost"
                label="Dismiss"
                icon={<XGlyph size="sm" />}
                onClick={() => putAway({ flagged: flaggedAt })}
              />
            </span>
          }
        >
          Denied with high confidence over the last 7 days. Nothing ran.
        </Callout>
      )}
      {digest && (
        <Callout
          variant="info"
          icon={<ListChecksGlyph size="md" />}
          title={`Delegates decided ${requests(decidedRows.length, data.decided_more)} yesterday`}
          action={
            <span className="row gap-1">
              {/* THE DIGEST'S OWN DAY: the Settings view opens on today, so far. */}
              <ButtonLink
                size="small"
                variant="secondary"
                href={href(["settings", "delegated-decisions"], { day: data.day })}
              >
                See what
              </ButtonLink>
              <IconButton
                size="sm"
                variant="ghost"
                label="Dismiss for today"
                icon={<XGlyph size="sm" />}
                onClick={() => putAway({ decided: data.day })}
              />
            </span>
          }
        >
          Decided in your place under the company&rsquo;s delegated decisions rules.
        </Callout>
      )}
    </div>
  );
}
