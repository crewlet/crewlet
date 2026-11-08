/**
 * The revision a repository's Files and Commits are read at: the default
 * branch where the address names none, or a branch, a tag or a commit.
 *
 * A BRANCH IS NAMED BY ITS NAME and a tag by its whole ref, as the address
 * carries them: the engine resolves a short name as a branch first
 * (`code.ResolveRef`), so a tag spelled short is read as the branch of that
 * name where both exist, and a tag's whole ref is the one spelling that cannot
 * be.
 */

import { useMemo } from "react";
import { Select } from "@crewlethq/ui";
import { isWholeOID, refName, shortOID } from "~/lib/code.ts";
import { useCodeQuery } from "~/lib/useCodeQuery.ts";
import type { CodeRefRow, CodeRepoDetail } from "~/protocol/index.ts";

/** Where a repository's tags are listed (`code.ReadQuery.Prefix`). */
const TAG_PREFIX = "refs/tags/";

/** No ref listed — one constant, so a listing not read yet is not a new array each render. */
const NONE: readonly CodeRefRow[] = [];

/** What a repository's revision pickers list: the first page of its branches and of its tags. */
export interface Revisions {
  branches: readonly CodeRefRow[];
  tags: readonly CodeRefRow[];
}

/**
 * Read a repository's branch and tag listings ONCE for a page, which hands
 * them to every picker it draws.
 *
 * EACH LISTING IS UP TO A THOUSAND ROWS (`code.MaxRefsPerPage`), each a ref, a
 * commit id and a log position — over a hundred kilobytes at that bound, which
 * a repository whose seats push a branch a run reaches — and a hook
 * keeps its own answer, so every picker that asked for itself read both
 * again: a comparison's two pickers asked the same two questions twice, and a
 * move between a repository's root and a path under it, which draws another
 * component, mounted a new picker that asked both again. The page that reads
 * them is the one that stays mounted while its pickers come and go: the
 * repository's page while its Files or Commits tab is open, and a comparison.
 */
export function useRevisions(name: string, enabled = true): Revisions {
  const branches = useCodeQuery("code_refs", { name }, { enabled });
  const tags = useCodeQuery("code_refs", { name, prefix: TAG_PREFIX }, { enabled });
  const branchRows = branches.data?.refs ?? NONE;
  const tagRows = tags.data?.refs ?? NONE;
  return useMemo(() => ({ branches: branchRows, tags: tagRows }), [branchRows, tagRows]);
}

/** A revision as a reader names it: a branch by its name, a tag as a tag, a commit by its short id. */
export function revisionWord(repo: CodeRepoDetail, rev: string): string {
  if (rev === "") return repo.default_branch;
  if (isWholeOID(rev)) return shortOID(rev);
  const ref = refName(rev);
  return ref.kind === "tag" ? `tag ${ref.name}` : ref.name;
}

/**
 * The revision picker: the default branch first, then the repository's other
 * branches and its tags, each the first page of the engine's listing by name
 * as the page read them ([useRevisions]) — and the revision the address names
 * where neither page holds it (a branch past the first page, or a commit a
 * Permalink names), so an address always reads as itself.
 *
 * `unset` is what no revision means where it is not the default branch — a
 * comparison's head, chosen or not — and the default branch is then listed
 * by its name like any other.
 */
export function RefPicker({
  repo,
  revisions,
  value,
  onChoose,
  label = "Revision",
  unset,
}: {
  repo: CodeRepoDetail;
  revisions: Revisions;
  value: string;
  onChoose: (rev: string) => void;
  label?: string;
  unset?: string;
}) {
  const options = [
    unset === undefined
      ? { value: "", label: `${repo.default_branch} (default)` }
      : { value: "", label: unset },
  ];
  for (const r of revisions.branches) {
    const name = refName(r.ref).name;
    if (unset !== undefined || name !== repo.default_branch) {
      options.push({
        value: name,
        label: name === repo.default_branch ? `${name} (default)` : name,
      });
    }
  }
  for (const r of revisions.tags)
    options.push({ value: r.ref, label: `tag ${refName(r.ref).name}` });
  if (!options.some((o) => o.value === value)) {
    options.push({ value, label: revisionWord(repo, value) });
  }
  return (
    // A TOOLBAR'S PICKER, at the row's small step: every caller draws it in one.
    <Select
      size="sm"
      width="auto"
      searchable
      value={value}
      onChange={(next) => onChoose(String(next))}
      options={options}
      ariaLabel={label}
      emptyMessage="No branch or tag matches"
      active={value !== ""}
    />
  );
}
