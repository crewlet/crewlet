/**
 * A page's past: one saved version, what it changed, and everything else that
 * happened to the page.
 *
 * # A version is read in the page's own column
 *
 * History opens the newest revision and the revision list beside the page
 * opens any other; either sets `version=`, and the page then draws THAT
 * version, what it changed against the one saved before it or the whole of
 * it, in place of the current body, with the way back beside it. Reading one
 * back is the `page_revision` question, which this asks.
 *
 * # An old version is an ordinary absence
 *
 * A page keeps a bounded number of revisions, so asking for one this node no
 * longer holds is not a failure, and the view says which of the two happened.
 */

import { useMemo } from "react";
import { Button, Document, EmptyState, VersionCompare } from "@crewlethq/ui";
import { CheckGlyph, ClockGlyph } from "@crewlethq/icons/glyphs";
import { href, useParam } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { DiffView } from "~/components/DiffView.tsx";
import { fmtDateTime, relTime } from "~/lib/format.ts";
import { renderMarkdown } from "~/lib/markdown.ts";
import { useQuery } from "~/lib/useQuery.ts";
import type { PageRevision } from "~/protocol/index.ts";
import { Face, changeAuthor, changeNote, changeWords, changedBy, type Who } from "../parts.tsx";

/**
 * One saved version, read in the page in place of the current body.
 *
 * The PREVIOUS version is the one below it in the history, not `version - 1`:
 * a page keeps a bounded number of revisions, so on a trimmed page
 * `version - 1` is a read that comes back not found.
 *
 * The two `page_revision` reads and their states live here; the head and the
 * lens switch are uilet's [VersionCompare], and the changes lens is the
 * dashboard's one [DiffView], handed over as `changes` — never the two texts,
 * which would draw the package's own line diff in the state hues a diff does
 * not carry. THE FIRST VERSION HAS NOTHING TO COMPARE WITH, so it is handed
 * only its full text and no switch is drawn, rather than a diff that says
 * "everything".
 */
export function VersionView({
  pageID,
  version,
  history,
  who,
  now,
  onClose,
}: {
  pageID: string;
  version: number;
  /** Newest first, as the `page` answer orders it. */
  history: PageRevision[];
  who: Who;
  now: number;
  onClose: () => void;
}) {
  const body = useQuery("page_revision", { page: pageID, version }, { enabled: version > 0 });
  const previous = useMemo(() => {
    const i = history.findIndex((rev) => rev.version === version);
    return i >= 0 ? (history[i + 1]?.version ?? 0) : 0;
  }, [history, version]);
  const [lens, setLens] = useParam("lens", "diff", "filter");
  // READ WHATEVER THE LENS, so the switch stays on screen from either side
  // instead of appearing only once the reader has asked for the changes.
  const prior = useQuery(
    "page_revision",
    { page: pageID, version: previous },
    { enabled: previous > 0 },
  );
  const rev = history.find((r) => r.version === version);
  const by = rev?.author ?? "";
  const text = body.data?.body;

  const full = body.data ? (
    text ? (
      <Document>{renderMarkdown(text)}</Document>
    ) : (
      <p className="knowledge-note">This revision had no body.</p>
    )
  ) : (
    <QueryState error={body.error} refusal={body.refusal} loading={body.loading}>
      {/* NOT FOUND IS NOT A FAILURE: a page keeps a bounded number of
          revisions, and saying which of the two happened is the point. */}
      {!body.loading && (
        <EmptyState
          size="compact"
          icon={<ClockGlyph size="xl" />}
          title="This node no longer holds that revision"
          description="Only recent revisions are kept."
        />
      )}
    </QueryState>
  );
  const changes =
    body.data && previous > 0 ? (
      prior.data ? (
        <DiffView
          before={prior.data.body ?? ""}
          after={text ?? ""}
          against={`rev ${previous}`}
          aria-label={`Changes from rev ${previous} to rev ${version}`}
          empty={
            <EmptyState
              size="compact"
              icon={<CheckGlyph size="xl" />}
              title="The body did not change"
              description="Only the title, labels or position changed."
            />
          }
        />
      ) : (
        <QueryState error={prior.error} refusal={prior.refusal} loading={prior.loading} />
      )
    ) : undefined;

  return (
    <VersionCompare
      title={
        <>
          {by && <Face handle={by} who={who} />} Revision {version}
        </>
      }
      description={
        rev && (
          <>
            {by ? who(by).name : "The engine"}
            {" · "}
            <time dateTime={rev.created_at} title={fmtDateTime(rev.created_at)}>
              {relTime(rev.created_at, now)}
            </time>
            {rev.message && <> · &ldquo;{rev.message}&rdquo;</>}
          </>
        )
      }
      fromLabel={`rev ${previous}`}
      toLabel={`rev ${version}`}
      lens={lens === "full" ? "versions" : "changes"}
      onLensChange={(next) => setLens(next === "versions" ? "full" : "diff")}
      labels={{ lens: "What to show of this revision", versions: "Full text" }}
      changes={changes}
      to={full}
      actions={
        <Button size="small" variant="ghost" onClick={onClose}>
          Back to the page
        </Button>
      }
    />
  );
}

/**
 * Everything that happened to this page, which is not the same as its saves.
 *
 * `pages_history` has one row per change (a save, a comment, a rename, a
 * move, a label), who made it, whether it announced anything, and the TURN
 * that made it, which a wiki cannot have: "why did this page change" is one
 * press rather than a search of the event log.
 */
export function PageChanges({ pageID, who, now }: { pageID: string; who: Who; now: number }) {
  const feed = useQuery("page_activity", { page: pageID }, { pollMs: 60_000 });
  const changes = feed.data?.changes ?? [];
  const headingID = `${pageID}-changes`;
  return (
    <section className="knowledge-changes" aria-labelledby={headingID}>
      <h2 className="knowledge-section-title" id={headingID}>
        <ClockGlyph size="sm" />
        Changes
        <span className="knowledge-count">{changes.length}</span>
      </h2>
      <QueryState
        error={feed.error}
        refusal={feed.refusal}
        loading={feed.loading}
        empty={
          changes.length
            ? undefined
            : {
                title: "Nothing has happened to this page",
                hint: "Saves, comments, renames and moves appear here.",
              }
        }
      >
        <ol className="knowledge-change-list">
          {changes.map((change) => {
            const by = changedBy(change);
            const note = changeNote(change);
            return (
              <li key={change.id} className="knowledge-change">
                {by ? (
                  <Face handle={by} who={who} />
                ) : (
                  <span className="knowledge-revision-engine" />
                )}
                <span className="knowledge-change-what">
                  <b>{changeAuthor(change, who)}</b> {changeWords(change.kind).phrase}
                  {change.quiet && (
                    <span className="knowledge-note" title="This change announced nothing">
                      {" "}
                      (quiet)
                    </span>
                  )}
                  {note && <span className="knowledge-change-note">{note}</span>}
                </span>
                {change.turn_id && (
                  <a
                    className="t-link knowledge-note"
                    href={href(["live", "turns", change.turn_id])}
                  >
                    The turn
                  </a>
                )}
                <time
                  className="knowledge-note"
                  dateTime={change.at}
                  title={fmtDateTime(change.at)}
                >
                  {relTime(change.at, now)}
                </time>
              </li>
            );
          })}
        </ol>
      </QueryState>
    </section>
  );
}
