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
import { Button, cx, Document, EmptyState, SegmentedControl } from "@crewlethq/ui";
import { CheckGlyph, ClockGlyph } from "@crewlethq/icons/glyphs";
import { href, useParam } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { collapse, diffLines, diffStat, type DiffSection } from "~/lib/diff.ts";
import { fmtDateTime, plural, relTime } from "~/lib/format.ts";
import { renderMarkdown } from "~/lib/markdown.ts";
import { useQuery } from "~/lib/useQuery.ts";
import type { PageRevision } from "~/protocol/index.ts";
import { Face, changeNote, changeWords, drawnBy, type Who } from "../parts.tsx";

/**
 * A line diff, rendered as the document it is.
 *
 * MONOSPACE AND LINE-NUMBERED on both sides, because the two numbers are what
 * a reader uses to find the paragraph in the version beside it. A skipped run
 * is a row of its own saying how many lines it stands for: a gap silently
 * closed makes a document edited at both ends look like one rewritten in the
 * middle.
 */
export function DiffPane({ sections }: { sections: DiffSection[] }) {
  return (
    <div className="diff">
      {sections.map((section, s) => (
        <div key={s} className="diff-section">
          {section.skipped > 0 && (
            <div className="diff-skip">{plural(section.skipped, "unchanged line")}</div>
          )}
          {section.lines.map((line, i) => (
            // THE CLASS NAMES ARE LITERALS, never assembled from the value:
            // a stylesheet gate that cannot see a class cannot tell a rule
            // this file relies on from one nothing uses.
            <div
              key={i}
              className={cx(
                "diff-line",
                line.kind === "add" && "is-add",
                line.kind === "remove" && "is-remove",
              )}
            >
              <span className="diff-no">{line.before ?? ""}</span>
              <span className="diff-no">{line.after ?? ""}</span>
              <span className="diff-mark">
                {line.kind === "add" ? "+" : line.kind === "remove" ? "−" : " "}
              </span>
              <span className="diff-text">{line.text || " "}</span>
            </div>
          ))}
        </div>
      ))}
    </div>
  );
}

/**
 * What one body became from another: the stat line and the pane, or the
 * sentence that says nothing in the prose moved.
 */
export function BodyDiff({
  before,
  after,
  against,
}: {
  before: string;
  after: string;
  against: string;
}) {
  const diff = useMemo(() => collapse(diffLines(before, after)), [before, after]);
  const stat = useMemo(() => diffStat(diff.flatMap((section) => section.lines)), [diff]);
  if (stat.identical) {
    // IDENTICAL IS ITS OWN ANSWER: a save that changed only the title leaves
    // the body untouched, and a pane of unmarked lines reads as one that
    // failed to load.
    return (
      <EmptyState
        size="compact"
        icon={<CheckGlyph size="xl" />}
        title="The body did not change"
        description="Only the title, labels or position changed."
      />
    );
  }
  return (
    <>
      <p className="knowledge-note">
        <span className="diff-add-ink">+{stat.added}</span>{" "}
        <span className="diff-del-ink">−{stat.removed}</span> against {against}
      </p>
      <DiffPane sections={diff} />
    </>
  );
}

/**
 * One saved version, read in the page in place of the current body.
 *
 * The PREVIOUS version is the one below it in the history, not `version - 1`:
 * a page keeps a bounded number of revisions, so on a trimmed page
 * `version - 1` is a read that comes back not found.
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
  const prior = useQuery(
    "page_revision",
    { page: pageID, version: previous },
    { enabled: previous > 0 && lens === "diff" },
  );
  const rev = history.find((r) => r.version === version);
  const by = rev ? drawnBy(rev.author, rev.author_seat) : "";

  return (
    <section className="knowledge-version" aria-label={`Revision ${version}`}>
      <div className="knowledge-version-head">
        {by && <Face handle={by} who={who} />}
        <span className="knowledge-version-what">
          Revision {version}
          {rev && (
            <>
              {" · "}
              <b>{by ? who(by).name : "the engine"}</b>
              {" · "}
              <time dateTime={rev.created_at} title={fmtDateTime(rev.created_at)}>
                {relTime(rev.created_at, now)}
              </time>
              {rev.message && <> · &ldquo;{rev.message}&rdquo;</>}
            </>
          )}
        </span>
        <span className="spacer" />
        {/* THE FIRST VERSION HAS NOTHING TO COMPARE WITH, which is a fact about
            the page rather than a lens the reader failed to pick, so the
            control is absent rather than offering a diff that says
            "everything". */}
        {previous > 0 && (
          <SegmentedControl
            semantics="radio"
            activate="manual"
            size="sm"
            label="What to show of this revision"
            value={lens === "full" ? "full" : "diff"}
            onValueChange={setLens}
            options={[
              { value: "diff", label: "Changes", title: `What changed from rev ${previous}` },
              { value: "full", label: "Full text" },
            ]}
          />
        )}
        <Button size="small" variant="ghost" onClick={onClose}>
          Back to the page
        </Button>
      </div>
      <QueryState error={body.error} loading={body.loading}>
        {body.data ? (
          lens !== "full" && previous > 0 ? (
            <QueryState error={prior.error} loading={prior.loading}>
              {prior.data && (
                <BodyDiff
                  before={prior.data.body ?? ""}
                  after={body.data.body ?? ""}
                  against={`rev ${previous}`}
                />
              )}
            </QueryState>
          ) : body.data.body ? (
            <Document>{renderMarkdown(body.data.body)}</Document>
          ) : (
            <p className="knowledge-note">This revision had no body.</p>
          )
        ) : (
          // NOT FOUND IS NOT A FAILURE: a page keeps a bounded number of
          // revisions, and saying which of the two happened is the point.
          !body.loading && (
            <EmptyState
              size="compact"
              icon={<ClockGlyph size="xl" />}
              title="This node no longer holds that revision"
              description="Only recent revisions are kept."
            />
          )
        )}
      </QueryState>
    </section>
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
            const by = drawnBy(change.actor, change.actor_seat);
            const note = changeNote(change);
            return (
              <li key={change.id} className="knowledge-change">
                {by ? (
                  <Face handle={by} who={who} />
                ) : (
                  <span className="knowledge-revision-engine" />
                )}
                <span className="knowledge-change-what">
                  <b>{by ? who(by).name : "The engine"}</b> {changeWords(change.kind).phrase}
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
