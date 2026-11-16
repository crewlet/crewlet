/**
 * A repository's branches and its tags.
 *
 * BRANCHES are the engine's listing of its live refs under `refs/heads/`
 * (`code_refs`), in name order a page at a time — the rows, read at the tab's
 * floor, never git's — each opening its files. TAGS are the same listing
 * under `refs/tags/`, below them, each opening its files at the tag.
 *
 * The tab makes a branch (New branch), and each row deletes its own or opens
 * a protection rule on it (`BranchWrites.tsx`, `Protect.tsx`), every one held
 * with its reason. A branch a rule names already leads to that rule instead.
 */

import { useMemo } from "react";
import { Skeleton, Tag, VisuallyHidden } from "@crewlethq/ui";
import { BookmarkGlyph } from "@crewlethq/icons/glyphs";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { PageToolbar } from "~/app/frame/PageToolbar.tsx";
import { PageFailed, QueryState } from "~/components/common.tsx";
import { refName } from "~/lib/code.ts";
import { plural } from "~/lib/format.ts";
import { codeRest, useCodeQuery } from "~/lib/useCodeQuery.ts";
import { usePaged } from "~/lib/usePaged.ts";
import type { CodeRefRow, CodeRefsAnswer, CodeRepoDetail } from "~/protocol/index.ts";
import { compareHref, repoHref } from "../paths.ts";
import { BranchChip, ShaChip } from "../ui.tsx";
import { DeleteBranchButton, NewBranchButton } from "./BranchWrites.tsx";
import { ProtectButton } from "./Protect.tsx";
import "~/styles/gitlet.css";

/** Where a repository's tags are listed (`gitlet.ReadQuery.Prefix`). */
const TAG_PREFIX = "refs/tags/";

/** The repository's branches, the default first in name order as the engine lists them, each opening its files. */
export function Branches({ repo }: { repo: CodeRepoDetail }) {
  const params = useMemo(() => ({ name: repo.name }), [repo.name]);
  const first = useCodeQuery("code_refs", params);
  const paged = usePaged(
    first,
    params,
    (cursor) => codeRest.query("code_refs", { ...params, cursor }),
    (a: CodeRefsAnswer) => a.refs ?? [],
    (r: CodeRefRow) => r.ref,
    (a: CodeRefsAnswer) => a.next ?? "",
  );
  const rows = paged.items;
  // THE REFS A RULE NAMES EXACTLY, a branch's own rule; a glob's is drawn in
  // Settings beside the refs it matches.
  const protectedRefs = useMemo(
    () => new Set((repo.protections ?? []).map((p) => p.pattern)),
    [repo.protections],
  );
  return (
    <>
      <PageToolbar>
        <NewBranchButton repo={repo} />
      </PageToolbar>
      {paged.loading && rows.length === 0 && (
        <Skeleton variant="text" rows={6} label={`Loading the branches of ${repo.name}`} />
      )}
      <QueryState
        error={paged.error}
        refusal={paged.refusal}
        detail={first.detail}
        loading={paged.loading}
        empty={
          rows.length > 0 || paged.more
            ? undefined
            : {
                title: "No branch yet",
                hint: "A branch is listed once a push that creates it applies.",
              }
        }
      >
        <DataGrid<CodeRefRow>
          rows={rows}
          rowKey={(r) => r.ref}
          // THE ENGINE'S ORDER, by name.
          serverSorted
          rowHref={(r) => repoHref(repo.name, { ref: branchRev(repo, r) })}
          onLoadMore={paged.more ? paged.next : undefined}
          loadedNote={
            paged.more ? `${plural(rows.length, "branch", "branches")} loaded` : undefined
          }
          aria-label={`Branches of ${repo.name}`}
          columns={[
            {
              key: "name",
              header: "Branch",
              floor: "12rem",
              phoneLead: true,
              cell: (r) => (
                <span className="code-ref">
                  <BranchChip name={refName(r.ref).name} />
                  {refName(r.ref).name === repo.default_branch && (
                    <Tag appearance="outline">default</Tag>
                  )}
                  {protectedRefs.has(r.ref) && <Tag appearance="outline">protected</Tag>}
                </span>
              ),
            },
            {
              // A VALUE, not a row action, so a phone names it on a line of its own.
              key: "head",
              header: "Head",
              shrink: true,
              cell: (r) => (r.oid ? <ShaChip oid={r.oid} /> : null),
            },
            {
              key: "history",
              header: "History",
              shrink: true,
              drop: 1,
              cell: (r) => (
                <a
                  className="t-link"
                  href={repoHref(repo.name, { tab: "commits", ref: branchRev(repo, r) })}
                >
                  Commits →
                </a>
              ),
            },
            {
              key: "compare",
              header: "Compare",
              shrink: true,
              drop: 2,
              // WITH THE DEFAULT BRANCH, which a branch is proposed into
              // unless a change names another; nothing for the default
              // branch itself, which compares with nothing here.
              cell: (r) =>
                branchRev(repo, r) ? (
                  <a className="t-link" href={compareHref(repo.name, { head: branchRev(repo, r) })}>
                    Compare →
                  </a>
                ) : null,
            },
            {
              key: "actions",
              header: <VisuallyHidden>Actions</VisuallyHidden>,
              action: true,
              shrink: true,
              // A RULE ON THIS BRANCH ALONE, and its delete at the head this
              // row shows. A branch a rule names already is edited where its
              // rule is drawn whole, gate and all (Settings): a new-rule form
              // opened here would start from the engine's defaults rather
              // than from what the rule holds.
              cell: (r) => (
                <span className="row gap-1 nowrap">
                  {protectedRefs.has(r.ref) ? (
                    <a className="t-link" href={repoHref(repo.name, { tab: "settings" })}>
                      Protection →
                    </a>
                  ) : (
                    <ProtectButton repo={repo} pattern={r.ref} label="Protect" />
                  )}
                  <DeleteBranchButton repo={repo} row={r} />
                </span>
              ),
            },
          ]}
        />
      </QueryState>
      {paged.pageError && <PageFailed error={paged.pageError} />}
      <Tags repo={repo} />
    </>
  );
}

/**
 * The repository's tags, under its branches: the engine's listing under
 * `refs/tags/`, by name a page at a time, each opening its files at the tag —
 * named by its whole ref, which no branch of the same name can shadow.
 */
function Tags({ repo }: { repo: CodeRepoDetail }) {
  const params = useMemo(() => ({ name: repo.name, prefix: TAG_PREFIX }), [repo.name]);
  const first = useCodeQuery("code_refs", params);
  const paged = usePaged(
    first,
    params,
    (cursor) => codeRest.query("code_refs", { ...params, cursor }),
    (a: CodeRefsAnswer) => a.refs ?? [],
    (r: CodeRefRow) => r.ref,
    (a: CodeRefsAnswer) => a.next ?? "",
  );
  const rows = paged.items;
  return (
    <section className="code-section" aria-labelledby="code-tags-title">
      <h2 id="code-tags-title" className="code-section__title">
        Tags
      </h2>
      {paged.loading && rows.length === 0 && (
        <Skeleton variant="text" rows={3} label={`Loading the tags of ${repo.name}`} />
      )}
      <QueryState
        error={paged.error}
        refusal={paged.refusal}
        detail={first.detail}
        loading={paged.loading}
        empty={
          rows.length > 0 || paged.more
            ? undefined
            : { title: "No tag yet", hint: "A tag is listed once a push that creates it applies." }
        }
      >
        <DataGrid<CodeRefRow>
          name="tags"
          rows={rows}
          rowKey={(r) => r.ref}
          serverSorted
          rowHref={(r) => repoHref(repo.name, { ref: r.ref })}
          onLoadMore={paged.more ? paged.next : undefined}
          loadedNote={paged.more ? `${plural(rows.length, "tag")} loaded` : undefined}
          aria-label={`Tags of ${repo.name}`}
          columns={[
            {
              key: "name",
              header: "Tag",
              floor: "12rem",
              phoneLead: true,
              cell: (r) => (
                <span className="code-ref">
                  <BookmarkGlyph size="sm" className="code-glyph" />
                  <span className="code-ref__name">{refName(r.ref).name}</span>
                </span>
              ),
            },
            {
              key: "commit",
              header: "Commit",
              shrink: true,
              action: true,
              cell: (r) => (r.oid ? <ShaChip oid={r.oid} /> : null),
            },
          ]}
        />
      </QueryState>
      {paged.pageError && <PageFailed error={paged.pageError} />}
    </section>
  );
}

/** A branch as an address names it: none for the default branch, its name for any other. */
function branchRev(repo: CodeRepoDetail, r: CodeRefRow): string {
  const name = refName(r.ref).name;
  return name === repo.default_branch ? "" : name;
}
