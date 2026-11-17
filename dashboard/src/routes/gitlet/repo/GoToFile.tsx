/**
 * Go to file: a repository's files found by the characters of their path in
 * order, at the commit the listing resolved (`code_find`), each opened at the
 * revision the address names. `t` focuses it.
 */
import { useEffect, useRef, useState, type ReactNode } from "react";
import { Combobox, Kbd, type ComboboxOption } from "@crewlethq/ui";
import { SearchGlyph } from "@crewlethq/icons/glyphs";
import { capsOf, keyRow, useKeymap } from "~/app/keymap.ts";
import { useNavigator } from "~/app/router.tsx";
import { repoPath } from "~/lib/code.ts";
import { fmtCount, plural } from "~/lib/format.ts";
import { useCodeQuery } from "~/lib/useCodeQuery.ts";
import type { CodeFindMatch } from "~/protocol/index.ts";
import { EntryGlyph } from "../ui.tsx";
import { REFUSED_CODES, refusalSentence } from "./marks.ts";

/** How many files one answer offers: the best, best first. */
export const FIND_LIMIT = 20;
/** How long typing rests before the path is asked: a pause, not a keystroke. */
export const FIND_QUIET_MS = 150;
/** The engine's bound on a query, in UTF-8 bytes. */
const FIND_MAX_BYTES = 256;

const utf8 = new TextEncoder();

/** The text cut to the engine's bound at a code point: the field's length counts code units. */
function withinBound(text: string): string {
  if (utf8.encode(text).length <= FIND_MAX_BYTES) return text;
  let bytes = 0;
  let cut = "";
  for (const point of text) {
    bytes += utf8.encode(point).length;
    if (bytes > FIND_MAX_BYTES) break;
    cut += point;
  }
  return cut;
}

/** A path with its matched runs strong: `spans` count code points, never code units. */
function Matched({ path, spans }: { path: string; spans: readonly [number, number][] }) {
  const points = Array.from(path);
  const slash = path.lastIndexOf("/");
  const dirPoints = slash < 0 ? 0 : Array.from(path.slice(0, slash + 1)).length;
  const parts: ReactNode[] = [];
  let at = 0;
  const piece = (from: number, to: number, strong: boolean) => {
    // THE DIRECTORY PART SECONDARY, the base name primary: cut a run at the last slash.
    for (const [a, b] of [
      [from, Math.min(to, dirPoints)],
      [Math.max(from, dirPoints), to],
    ] as const) {
      if (a >= b) continue;
      const text = points.slice(a, b).join("");
      const cls = `${a < dirPoints ? "code-find__dir" : "code-find__base"}${strong ? " code-find__hit" : ""}`;
      parts.push(
        <span key={a} className={cls}>
          {text}
        </span>,
      );
    }
  };
  for (const [start, end] of spans) {
    if (start > at) piece(at, start, false);
    piece(Math.max(start, at), end, true);
    at = Math.max(at, end);
  }
  if (at < points.length) piece(at, points.length, false);
  return <span className="code-find__path">{parts}</span>;
}

export function GoToFile({ repo, rev, commit }: { repo: string; rev: string; commit: string }) {
  const nav = useNavigator();
  const field = useRef<HTMLInputElement>(null);
  const [typed, setTyped] = useState("");
  const [asked, setAsked] = useState("");
  const [open, setOpen] = useState(false);
  // ONE PAUSE: each key clears the one before it.
  useEffect(() => {
    const timer = window.setTimeout(() => setAsked(withinBound(typed)), FIND_QUIET_MS);
    return () => window.clearTimeout(timer);
  }, [typed]);
  const q = asked;
  const find = useCodeQuery(
    "code_find",
    { name: repo, ref: commit, q, limit: FIND_LIMIT },
    { enabled: commit !== "" && q.trim() !== "" },
  );
  useKeymap({ "code.find": () => field.current?.focus() });
  const answer = find.data?.find;
  const current = q === withinBound(typed) && !find.loading && commit !== "";
  const matches: readonly CodeFindMatch[] = current ? (answer?.matches ?? []) : [];
  const options: ComboboxOption[] = matches.map((m) => ({
    value: m.path,
    label: (
      <span className="code-find__row">
        <EntryGlyph entry={m} />
        <Matched path={m.path} spans={m.spans} />
      </span>
    ),
  }));
  const total = answer?.total ?? 0;
  if (current && answer && total > matches.length) {
    options.push({
      value: "",
      disabled: true,
      label: `The best ${fmtCount(matches.length)} of ${plural(total, "file")}: type more to narrow`,
    });
  }
  const empty = !current
    ? `Looking for “${typed}”…`
    : find.error
      ? REFUSED_CODES.has(find.error)
        ? `Finding a file is unavailable${refusalSentence(find) ? `: ${refusalSentence(find)}` : ""}`
        : "The files could not be read. Type again to retry."
      : `No file's path holds “${q}”, among ${plural(answer?.files ?? 0, "file")}`;
  const cap = capsOf(keyRow("code.find").presses[0]!);
  return (
    <Combobox
      ref={field}
      label="Files"
      aria-label="Go to file"
      placeholder="Go to file"
      inputSize="sm"
      containerClassName="code-find"
      leading={<SearchGlyph size="sm" />}
      trailing={typed === "" ? <Kbd subtle keys={cap} /> : undefined}
      maxLength={FIND_MAX_BYTES}
      mono
      value={typed}
      onValueChange={(next) => {
        setTyped(next);
        setOpen(next !== "");
      }}
      open={open && typed.trim() !== ""}
      onOpenChange={setOpen}
      options={options}
      emptyMessage={empty}
      onCommit={(option) => {
        if (option.value === "") return;
        setTyped("");
        setAsked("");
        setOpen(false);
        nav.to(repoPath(repo), { path: option.value, ...(rev ? { ref: rev } : {}) });
      }}
    />
  );
}
