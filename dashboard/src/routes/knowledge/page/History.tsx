/**
 * A page's past: one saved version, what it changed, and everything else that
 * happened to the page.
 *
 * # A version is read in the page's own column
 *
 * The rail lists the revisions and the History button opens the newest; either
 * sets `version=`, and the article then draws THAT version — what it changed
 * against the one saved before it, or the whole of it — in place of the
 * current body, with the way back beside it. A list of version numbers is not
 * a history: the detail carries summaries, and reading one back is the
 * `page_revision` question, which this asks.
 *
 * # An old version is an ordinary absence
 *
 * A page keeps a bounded number of revisions, so asking for one this node no
 * longer holds is not a failure, and the view says which of the two happened.
 */

import { useMemo } from "react";
import { Button, cx, EmptyState, Tag } from "@crewlethq/ui";
import {
  ChevronLeftGlyph,
  ChartNoAxesGanttGlyph,
  CheckGlyph,
  ClockGlyph,
} from "@crewlethq/icons/glyphs";
import { href, useParam } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { collapse, diffLines, diffStat, type DiffSection } from "~/lib/diff.ts";
import { fmtDateTime, plural, relTime } from "~/lib/format.ts";
import { plainText, renderMarkdown } from "~/lib/markdown.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { Segmented } from "~/ui/primitives.tsx";
import type { PageRevision } from "~/protocol/index.ts";

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
    // IDENTICAL IS ITS OWN ANSWER. A save that changed only the title leaves
    // the body untouched, and a pane of unmarked lines reads as one that
    // failed to load.
    return (
      <EmptyState
        size="compact"
        icon={<CheckGlyph size="xl" />}
        title="The body did not change"
        description="A page's title, its labels and its place in the tree are saved beside its body — the prose is the same as before."
      />
    );
  }
  return (
    <>
      <p className="t-caption">
        <span className="diff-add-ink">+{stat.added}</span>{" "}
        <span className="diff-del-ink">−{stat.removed}</span> against {against}
      </p>
      <DiffPane sections={diff} />
    </>
  );
}

/**
 * One saved version, read in the article in place of the current body.
 *
 * The PREVIOUS version is the one below it in the history, not `version - 1`:
 * a page keeps a bounded number of revisions, so off a trimmed page
 * `version - 1` is a read that comes back not found.
 */
export function VersionView({
  pageID,
  version,
  history,
  seatName,
  now,
  onClose,
}: {
  pageID: string;
  version: number;
  /** Newest first, as the `page` answer orders it. */
  history: PageRevision[];
  seatName: (handle: string) => string;
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

  return (
    <section className="kpage-version" aria-label={`Revision ${version}`}>
      <div className="kpage-version-head">
        <Button
          size="small"
          variant="ghost"
          leadingIcon={<ChevronLeftGlyph size="sm" />}
          onClick={onClose}
        >
          The page as it is now
        </Button>
        <span className="spacer" />
        {/* THE FIRST VERSION HAS NOTHING TO COMPARE WITH, which is a fact
            about the page rather than a lens the reader failed to pick — so
            the control is absent rather than offering a diff that can only
            say "everything". */}
        {previous > 0 && (
          <Segmented
            ariaLabel="What to show"
            value={lens}
            onChange={setLens}
            options={[
              { value: "diff", label: `Changes from rev ${previous}` },
              { value: "full", label: "The whole revision" },
            ]}
          />
        )}
      </div>
      <h2 className="kpage-version-title">
        Revision {version}
        {rev && (
          <span className="kpage-version-by">
            {" "}
            · {rev.author ? seatName(rev.author) : "the engine"} ·{" "}
            <time dateTime={rev.created_at} title={fmtDateTime(rev.created_at)}>
              {relTime(rev.created_at, now)}
            </time>
          </span>
        )}
      </h2>
      {rev?.message && <p className="kpage-version-message">{rev.message}</p>}
      <QueryState error={body.error} loading={body.loading}>
        {body.data ? (
          lens === "diff" && previous > 0 ? (
            <QueryState error={prior.error} loading={prior.loading}>
              {prior.data && (
                <BodyDiff
                  before={prior.data.body ?? ""}
                  after={body.data.body ?? ""}
                  against={`rev ${previous}`}
                />
              )}
            </QueryState>
          ) : (
            <div className="prose md kpage-prose">
              {body.data.body ? (
                renderMarkdown(body.data.body)
              ) : (
                <span className="muted">This revision had no body.</span>
              )}
            </div>
          )
        ) : (
          // NOT FOUND IS NOT A FAILURE: a page keeps a bounded number of
          // revisions, and saying which of the two happened is the point.
          !body.loading && (
            <EmptyState
              size="compact"
              icon={<ClockGlyph size="xl" />}
              title="This node no longer holds that revision"
              description="A page keeps a bounded number of revisions. The entry in its history is the record that it existed."
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
 * `pages_history` has one row per change — a save, a comment, a rename, a
 * move, a label — who made it, whether it announced anything, and the TURN
 * that made it, which is what a wiki cannot have: "why did this page change"
 * is one click rather than a search of the event log.
 */
export function PageChanges({
  pageID,
  seatName,
  now,
}: {
  pageID: string;
  seatName: (handle: string) => string;
  now: number;
}) {
  const feed = useQuery("page_activity", { page: pageID }, { pollMs: 60_000 });
  const changes = feed.data?.changes ?? [];
  return (
    <section className="kpage-section" aria-labelledby={`${pageID}-activity`}>
      <h2 className="kpage-section-title" id={`${pageID}-activity`}>
        <ChartNoAxesGanttGlyph size="sm" />
        Activity
        <span className="kpage-count">{changes.length}</span>
      </h2>
      <QueryState
        error={feed.error}
        loading={feed.loading}
        empty={
          changes.length
            ? undefined
            : {
                title: "Nothing has happened to this page",
                hint: "Every change writes an entry — a save, a comment, a rename, a move, a label. A page with none was created and left alone.",
              }
        }
      >
        <ol className="kpage-changes">
          {changes.map((change) => (
            <li key={change.id} className="kpage-change">
              <span className="row gap-2">
                <Tag appearance="outline" size="xs">
                  {change.kind}
                </Tag>
                <span>{change.actor ? seatName(change.actor) : "the engine"}</span>
                {change.quiet && (
                  <span className="t-caption" title="this change announced nothing">
                    quiet
                  </span>
                )}
                <span className="spacer" />
                {change.turn_id && (
                  <a className="t-link t-caption" href={href(["live", "turns", change.turn_id])}>
                    turn →
                  </a>
                )}
                <time
                  className="muted t-caption"
                  dateTime={change.at}
                  title={fmtDateTime(change.at)}
                >
                  {relTime(change.at, now)}
                </time>
              </span>
              {change.excerpt && <p className="t-caption">{plainText(change.excerpt)}</p>}
            </li>
          ))}
        </ol>
      </QueryState>
    </section>
  );
}
