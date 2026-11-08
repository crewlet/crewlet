/**
 * The repository a Gitlet screen is narrowed to, chosen from the ones the reader
 * may read, however many the company hosts.
 *
 * WHAT IS TYPED IS ASKED, never matched against one page in the browser. The
 * engine answers its listing a page at a time, so a picker built from one page
 * offered the first hundred names by name and no other, and a company with
 * more could not choose the rest from Changes, Queue, Search or Pushes. Typed
 * text is asked as the listing's `prefix` once it rests (`useQuiet`), which the
 * engine reads as one range of its index by name, so the names it begins come
 * back however many sort before them; Load more pages on through `usePaged`.
 * With nothing typed, the list is the listing from its first name. The loaded
 * rows are narrowed to what is typed while its own answer is on its way, which
 * only hides rows that answer will not hold. An opening no name could have,
 * such as a capital letter or a space, is said in the list and never asked:
 * the engine would refuse it (`isRepoPrefix`).
 *
 * THE FIELD SAYS WHAT IS CHOSEN until somebody types: the repository's name,
 * or nothing beside the screen's words for every repository. Leaving it
 * without taking a row puts the chosen name back, so the field never says one
 * repository while the screen reads another. The chosen name stays a row of
 * its own where the pages loaded do not hold it, so an address naming any
 * repository reads as itself.
 */
import { useMemo, useRef, useState } from "react";
import { Combobox, type ComboboxOption } from "@crewlethq/ui";
import { ChevronDownGlyph } from "@crewlethq/icons/glyphs";
import { isRepoPrefix } from "~/lib/code.ts";
import { codeRest, useCodeQuery } from "~/lib/useCodeQuery.ts";
import { usePaged } from "~/lib/usePaged.ts";
import { useQuiet } from "~/lib/useQuiet.ts";
import type { CodeReposAnswer } from "~/protocol/index.ts";

/**
 * The row that asks for the next page. Its value has a character no
 * repository name holds, so it is never taken for one.
 */
const MORE = "<more>";

export function RepoPicker({
  repo,
  onChoose,
  every,
  placeholder = "Choose a repository",
  size = "md",
}: {
  /** The chosen repository's name, or "" for none. */
  repo: string;
  onChoose: (name: string) => void;
  /**
   * The row that narrows to no repository, in the screen's words. Left out
   * where the screen reads one repository only, which then waits on a choice.
   */
  every?: string;
  /** What the empty field says where there is no `every` row. */
  placeholder?: string;
  /** The control step: small in a toolbar row, the default in a form. */
  size?: "sm" | "md";
}) {
  // NULL UNTIL SOMEBODY TYPES, and again once they leave the field: the field
  // shows the chosen name, and the list is the listing from its first name.
  const [typed, setTyped] = useState<string | null>(null);
  const [open, setOpen] = useState(false);
  // Load more is a row, so taking it closes the list as any row does; the
  // close that follows it is the one this keeps from happening.
  const paging = useRef(false);
  // LOWERCASE, as every name is: a capital typed is a name somebody means,
  // and the engine's grammar has none.
  const term = (typed ?? "").trim().toLowerCase();
  const askable = term === "" || isRepoPrefix(term);
  const rested = useQuiet(term);
  const asked = rested === "" || isRepoPrefix(rested) ? rested : null;
  const params = useMemo(() => (asked ? { prefix: asked } : {}), [asked]);
  const first = useCodeQuery("code_repos", params, { enabled: asked !== null });
  const paged = usePaged(
    first,
    params,
    (cursor) => codeRest.query("code_repos", { ...params, cursor }),
    (a: CodeReposAnswer) => a.repos ?? [],
    (r) => r.name,
    (a: CodeReposAnswer) => a.next ?? "",
  );
  const names = askable
    ? paged.items.map((r) => r.name).filter((name) => name.startsWith(term))
    : [];
  const current = asked === term;
  const options: ComboboxOption[] = [
    ...(every !== undefined && term === "" ? [{ value: "", label: every }] : []),
    ...(repo !== "" && term === "" && !names.includes(repo) ? [{ value: repo, label: repo }] : []),
    ...names.map((name) => ({ value: name, label: name })),
    ...(current && paged.more
      ? [
          {
            value: MORE,
            label: paged.paging
              ? "Reading more repositories…"
              : paged.pageError
                ? `Could not read more repositories (${paged.pageError}); try again`
                : "Load more repositories",
            disabled: paged.paging,
          },
        ]
      : []),
  ];
  const empty = !askable
    ? `No repository name begins with “${typed?.trim() ?? ""}”`
    : paged.loading || !current
      ? "Reading the repositories…"
      : paged.error
        ? `Could not read the repositories (${paged.error})`
        : term === ""
          ? "No repository you may read"
          : `No repository you may read begins with “${term}”`;

  return (
    <Combobox
      label="Repositories"
      aria-label="Repository"
      placeholder={every ?? placeholder}
      inputSize={size}
      width="sm"
      trailing={<ChevronDownGlyph size="sm" />}
      value={typed ?? repo}
      onValueChange={(next) => {
        setTyped(next);
        setOpen(true);
      }}
      onFocus={(event) => event.currentTarget.select()}
      onClick={() => setOpen(true)}
      open={open}
      onOpenChange={(next) => {
        if (!next && paging.current) {
          paging.current = false;
          return;
        }
        setOpen(next);
        if (!next) setTyped(null);
      }}
      options={options}
      emptyMessage={empty}
      onCommit={(option) => {
        if (option.value === MORE) {
          paging.current = true;
          paged.next();
          return;
        }
        setTyped(null);
        onChoose(option.value);
      }}
    />
  );
}
