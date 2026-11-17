/**
 * What a change is, beside its conversation: the rail every code host draws
 * there. Whom it asked for a review and where each stands, who owns and wrote
 * it, its work item, its branches, its head, and whether it merges into its
 * target — each section a heading and its rows.
 *
 * READ FROM THE CHANGE'S OWN PARTS: the detail the page holds, its revisions,
 * the reviews the conversation read, and the one part this rail asks of its
 * own, its reviewers (`code_change`, `part=reviewers`), asked again when the
 * change or its conversation moves (`reads.ts`). A reviewer's standing is the
 * verdict of their latest review among the pages the conversation loaded, or
 * that they are asked and have not answered.
 */

import { Button, EmptyValue, Tag } from "@crewlethq/ui";
import { PageFailed } from "~/components/common.tsx";
import { refName, verdictWord } from "~/lib/code.ts";
import { plural } from "~/lib/format.ts";
import type { CodeChangeDetail, CodeReviewRow, CodeRevisionRow } from "~/protocol/index.ts";
import { changesHref, commitHref } from "../paths.ts";
import { BranchChip, Face, ShaChip, useWhoName } from "../ui.tsx";
import { usePagedReviewers } from "./reads.ts";
import "~/styles/gitlet.css";

/** A section of the rail: its heading, then its rows. */
function RailSection({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <section className="code-rail__section" aria-label={title}>
      <h2 className="code-rail__title">{title}</h2>
      {children}
    </section>
  );
}

/** Where one reviewer stands, in words: their latest verdict, or that they are asked. */
function standingOf(latest: CodeReviewRow | undefined, withdrawn: boolean): string {
  if (latest) return `${verdictWord(latest.verdict)} on revision ${latest.revision}`;
  return withdrawn ? "no longer asked" : "asked, not answered yet";
}

/** Whom the change asked for a review, each with where they stand. */
function Reviewers({
  change,
  reviews,
}: {
  change: CodeChangeDetail;
  reviews: readonly CodeReviewRow[];
}) {
  const whoName = useWhoName();
  const asked = usePagedReviewers(change);
  // EACH REVIEWER'S LATEST VERDICT, from every review page loaded: the pages
  // are oldest first, so the last one met is the newest.
  const latest = new Map<string, CodeReviewRow>();
  for (const r of reviews) latest.set(r.reviewer, r);
  // A REVIEWER NOBODY ASKED is listed too: their review is a fact about the
  // change whether or not they were asked.
  const unasked = [...latest.keys()].filter((who) => !asked.items.some((a) => a.reviewer === who));
  if (asked.loading && asked.items.length === 0 && unasked.length === 0) {
    return <p className="code-rail__note">Reading who is asked…</p>;
  }
  if (asked.items.length === 0 && unasked.length === 0) {
    return (
      <p className="code-rail__note">
        {asked.error ? <PageFailed error={asked.error} /> : "Nobody is asked to review it yet."}
      </p>
    );
  }
  return (
    <>
      <ul className="code-rail__people">
        {asked.items.map((a) => (
          <li key={`${a.kind}:${a.reviewer}`}>
            <Face who={a.reviewer} />
            <span className="code-rail__person">
              <span className="code-rail__name">{whoName(a.reviewer)}</span>
              <span className="code-rail__note">
                {standingOf(latest.get(a.reviewer), a.state === "withdrawn")}
                {a.reason === "path_owner" ? " · owns paths it touches" : ""}
              </span>
            </span>
          </li>
        ))}
        {unasked.map((who) => (
          <li key={`unasked:${who}`}>
            <Face who={who} />
            <span className="code-rail__person">
              <span className="code-rail__name">{whoName(who)}</span>
              <span className="code-rail__note">{standingOf(latest.get(who), false)}</span>
            </span>
          </li>
        ))}
      </ul>
      {asked.more && (
        <Button
          size="small"
          variant="ghost"
          onClick={asked.next}
          disabled={asked.paging}
          loading={asked.paging}
        >
          Load more reviewers
        </Button>
      )}
      {asked.pageError && <PageFailed error={asked.pageError} />}
    </>
  );
}

/** Whether the latest revision merges into its target, as its home recorded it. */
function Mergeability({ change }: { change: CodeChangeDetail }) {
  const m = change.mergeability;
  const target = refName(change.target_ref).name;
  if (!m) return <p className="code-rail__note">Not measured against {target} yet.</p>;
  if (m.state === "clean") {
    // COLOUR IS STATE: a revision that merges cleanly is done with this
    // question, and one that conflicts is stopped on it.
    return (
      <p className="code-rail__row">
        <Tag size="sm" variant="success">
          Merges cleanly
        </Tag>
        <span className="code-rail__note">
          into {target}, revision {m.revision}
        </span>
      </p>
    );
  }
  if (m.state === "conflict") {
    const count = (m.conflicts?.length ?? 0) + (m.conflicts_omitted ?? 0);
    return (
      <div className="code-rail__stack">
        <p className="code-rail__row">
          <Tag size="sm" variant="danger">
            Conflicts
          </Tag>
          <span className="code-rail__note">
            with {target} in {plural(count, "path")}
          </span>
        </p>
        {(m.conflicts ?? []).length > 0 && (
          <ul className="code-rail__paths">
            {(m.conflicts ?? []).map((path) => (
              <li key={path} className="mono">
                {path}
              </li>
            ))}
          </ul>
        )}
      </div>
    );
  }
  return (
    <p className="code-rail__note">
      Its home could not decide whether revision {m.revision} merges into {target} in a read’s time.
    </p>
  );
}

export function ChangeRail({
  change,
  revisions,
  reviews,
}: {
  change: CodeChangeDetail;
  revisions: readonly CodeRevisionRow[];
  reviews: readonly CodeReviewRow[];
}) {
  const whoName = useWhoName();
  const work = change.work_item;
  const head = revisions.find((r) => r.n === change.revision)?.head ?? change.head;
  return (
    <div className="code-rail">
      <RailSection title="Reviewers">
        <Reviewers change={change} reviews={reviews} />
      </RailSection>
      <RailSection title="Owner">
        <p className="code-rail__row">
          <Face who={change.owner} />
          <a className="code-rail__name" href={changesHref({ owner: change.owner })}>
            {whoName(change.owner)}
          </a>
        </p>
      </RailSection>
      {change.authors.length > 0 && (
        <RailSection title="Authors">
          <ul className="code-rail__people">
            {change.authors.map((a) => (
              <li key={a.author}>
                <Face who={a.author} />
                <span className="code-rail__person">
                  <span className="code-rail__name">{whoName(a.author)}</span>
                  <span className="code-rail__note">from revision {a.first_revision}</span>
                </span>
              </li>
            ))}
          </ul>
        </RailSection>
      )}
      <RailSection title="Work item">
        {work ? (
          <a
            className="code-rail__name mono"
            href={changesHref({ work: `${work.backend}:${work.id}`, state: "all" })}
            title="Every change of the work item"
          >
            {work.key || work.id}
          </a>
        ) : (
          <EmptyValue label="No work item" />
        )}
      </RailSection>
      <RailSection title="Branches">
        <p className="code-rail__branches">
          <BranchChip name={refName(change.source_ref).name} />{" "}
          <span className="code-rail__note">into</span>{" "}
          <BranchChip name={refName(change.target_ref).name} />
        </p>
      </RailSection>
      <RailSection title="Head">
        <p className="code-rail__row">
          <ShaChip oid={head} />
          <a className="t-link" href={commitHref(change.repo, head)}>
            Revision {change.revision}
          </a>
        </p>
      </RailSection>
      {(change.state === "open" || change.state === "draft") && (
        <RailSection title="Merges">
          <Mergeability change={change} />
        </RailSection>
      )}
    </div>
  );
}
