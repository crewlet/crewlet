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

import { Select } from "@crewlethq/ui";
import { isWholeOID, refName, shortOID } from "~/lib/code.ts";
import { useCodeQuery } from "~/lib/useCodeQuery.ts";
import type { CodeRepoDetail } from "~/protocol/index.ts";

/** Where a repository's tags are listed (`code.ReadQuery.Prefix`). */
const TAG_PREFIX = "refs/tags/";

/** A revision as a reader names it: a branch by its name, a tag as a tag, a commit by its short id. */
export function revisionWord(repo: CodeRepoDetail, rev: string): string {
  if (rev === "") return repo.default_branch;
  if (isWholeOID(rev)) return shortOID(rev);
  const ref = refName(rev);
  return ref.kind === "tag" ? `tag ${ref.name}` : ref.name;
}

/**
 * The revision picker: the default branch first, then the repository's other
 * branches and its tags, each the first page of the engine's listing by name —
 * and the revision the address names where neither page holds it (a branch
 * past the first page, or a commit a Permalink names), so an address always
 * reads as itself.
 *
 * `unset` is what no revision means where it is not the default branch — a
 * comparison's head, chosen or not — and the default branch is then listed
 * by its name like any other.
 */
export function RefPicker({
  repo,
  value,
  onChoose,
  label = "Revision",
  unset,
}: {
  repo: CodeRepoDetail;
  value: string;
  onChoose: (rev: string) => void;
  label?: string;
  unset?: string;
}) {
  const branches = useCodeQuery("code_refs", { name: repo.name });
  const tags = useCodeQuery("code_refs", { name: repo.name, prefix: TAG_PREFIX });
  const options = [
    unset === undefined
      ? { value: "", label: `${repo.default_branch} (default)` }
      : { value: "", label: unset },
  ];
  for (const r of branches.data?.refs ?? []) {
    const name = refName(r.ref).name;
    if (unset !== undefined || name !== repo.default_branch) {
      options.push({
        value: name,
        label: name === repo.default_branch ? `${name} (default)` : name,
      });
    }
  }
  for (const r of tags.data?.refs ?? [])
    options.push({ value: r.ref, label: `tag ${refName(r.ref).name}` });
  if (!options.some((o) => o.value === value)) {
    options.push({ value, label: revisionWord(repo, value) });
  }
  return (
    <Select
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
