/** The revision picker's rows, from one page of branches or tags: pure, so the order is tested alone. */
import { refName } from "~/lib/code.ts";
import type { CodeRefListing, CodeRefProtection, CodeTagRow } from "~/protocol/index.ts";

/** One row: what it names, what choosing it writes to the address, and its marks. */
export interface PickerRow {
  /** The whole ref, the row's key. */
  ref: string;
  name: string;
  /** What choosing it writes as `ref=`. */
  rev: string;
  /** When it moved (a branch) or was made (a tag); absent where unknown. */
  at?: string;
  /** A tag dated past the year 9999, which `at` cannot carry. */
  late?: boolean;
  /** A tag with no date of its own. */
  undated?: boolean;
  protection?: CodeRefProtection;
  isDefault: boolean;
  latest: boolean;
  /** The revision the address names. */
  current: boolean;
}

/** What choosing a ref writes: a tag by its whole ref, the default branch as none unless `unset` is given, else a name. */
export function revSpelling(ref: string, defaultBranch: string, unset: boolean): string {
  const r = refName(ref);
  if (r.kind === "tag") return ref;
  return r.name === defaultBranch && !unset ? "" : r.name;
}

/**
 * Branch rows: without a find, the default branch pinned first and dropped from the page;
 * with one, the page in the engine's order.
 */
export function branchRows(
  page: readonly CodeRefListing[],
  pinned: CodeRefListing | null,
  opts: { defaultBranch: string; value: string; unset: boolean; finding: boolean },
): PickerRow[] {
  const defaultRef = `refs/heads/${opts.defaultBranch}`;
  const rows =
    opts.finding || !pinned ? page : [pinned, ...page.filter((r) => r.ref !== defaultRef)];
  return rows.map((r) => {
    const rev = revSpelling(r.ref, opts.defaultBranch, opts.unset);
    return {
      ref: r.ref,
      name: refName(r.ref).name,
      rev,
      at: r.moved_at,
      protection: r.protection,
      isDefault: r.ref === defaultRef,
      latest: false,
      // THE DEFAULT BRANCH NAMED BY ITS NAME is still the default's row.
      current: rev === opts.value || (r.ref === defaultRef && opts.value === opts.defaultBranch),
    };
  });
}

/** Tag rows, newest first: Latest on the first of an unfound page only. */
export function tagRows(tags: readonly CodeTagRow[], value: string): PickerRow[] {
  return tags.map((t, i) => ({
    ref: t.ref,
    name: refName(t.ref).name,
    rev: t.ref,
    at: t.date,
    late: t.date === undefined && t.date_unix !== undefined,
    undated: t.date === undefined && t.date_unix === undefined,
    isDefault: false,
    latest: i === 0,
    current: t.ref === value,
  }));
}

/** Tags a find names by their name alone: the by-name listing carries no date. */
export function foundTagRows(refs: readonly { ref: string }[], value: string): PickerRow[] {
  return refs.map((t) => ({
    ref: t.ref,
    name: refName(t.ref).name,
    rev: t.ref,
    isDefault: false,
    latest: false,
    current: t.ref === value,
  }));
}
