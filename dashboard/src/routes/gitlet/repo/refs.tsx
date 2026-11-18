/**
 * The revision a repository's Files and Commits are read at, and the picker that
 * chooses it: a branch by its name, a tag by its whole ref, as the address carries them.
 * The picker reads only while open: a page of branches by when they moved, or of tags newest first.
 */

import { useEffect, useId, useMemo, useRef, useState, type ReactNode, type RefObject } from "react";
import {
  Button,
  IconButton,
  Input,
  Popover,
  Skeleton,
  Tabs,
  Tag,
  useListbox,
  useOptionKeys,
} from "@crewlethq/ui";
import {
  ArrowRightGlyph,
  CheckGlyph,
  ChevronDownGlyph,
  GitBranchGlyph,
  GitCommitVerticalGlyph,
  SearchGlyph,
  ShieldCheckGlyph,
  TagGlyph,
  XGlyph,
} from "@crewlethq/icons/glyphs";
import { TOUCH_MEDIA } from "~/app/layout.ts";
import { useNow } from "~/lib/clock.ts";
import { isWholeOID, refName, shortOID } from "~/lib/code.ts";
import { fmtDateTime, relTime } from "~/lib/format.ts";
import { useCodeQuery } from "~/lib/useCodeQuery.ts";
import type { QueryResult } from "~/lib/useQuery.ts";
import type { CodeRepoDetail } from "~/protocol/index.ts";
import { repoHref } from "../paths.ts";
import { TAG_LATE, TAG_UNDATED, TAG_UNDATED_TITLE } from "./Branches.tsx";
import { protectionTitle, useBranchDetail } from "../branchDetail.ts";
import { FIND_QUIET_MS, withinBound } from "./GoToFile.tsx";
import { REFUSED_CODES, refusalSentence } from "./marks.ts";
import { branchRows, foundTagRows, tagRows, type PickerRow } from "./picker.ts";

/** A page of the picker: what its panel holds before View all. */
export const PICKER_LIMIT = 10;
const TAG_PREFIX = "refs/tags/";

/** A revision as a reader names it: a branch by its name, a tag as a tag, a commit by its short id. */
export function revisionWord(repo: CodeRepoDetail, rev: string): string {
  if (rev === "") return repo.default_branch;
  if (isWholeOID(rev)) return shortOID(rev);
  const ref = refName(rev);
  return ref.kind === "tag" ? `tag ${ref.name}` : ref.name;
}

/** The glyph that says what a revision is: a commit, a tag, or a branch. */
function revisionGlyph(rev: string): ReactNode {
  if (isWholeOID(rev)) return <GitCommitVerticalGlyph size="sm" />;
  if (refName(rev).kind === "tag") return <TagGlyph size="sm" />;
  return <GitBranchGlyph size="sm" />;
}

/**
 * The revision picker: a button naming the revision, opening a panel of the
 * repository's branches and tags. `unset` is what no revision means where it is
 * not the default branch (a comparison's head), and the default is then chosen by its name.
 */
export function RefPicker({
  repo,
  value,
  onChoose,
  label = "Branch or tag",
  title = "Switch branches or tags",
  unset,
}: {
  repo: CodeRepoDetail;
  value: string;
  onChoose: (rev: string) => void;
  label?: string;
  /** The panel's title, which names it. */
  title?: string;
  unset?: string;
}) {
  const field = useRef<HTMLInputElement>(null);
  const button = useRef<HTMLButtonElement>(null);
  const none = value === "" && unset !== undefined;
  const word = none ? unset : revisionWord(repo, value).replace(/^tag /, "");
  return (
    <Popover
      label={title}
      align="start"
      className="code-picker-panel"
      // A TAP OPENS ON THE PANEL: a focused field raises a keyboard over the list.
      initialFocus={() =>
        typeof window.matchMedia === "function" && window.matchMedia(TOUCH_MEDIA).matches
          ? (field.current?.closest<HTMLElement>(".crewlet-popover") ?? null)
          : field.current
      }
      trigger={(open, toggle) => (
        <Button
          ref={button}
          size="small"
          variant="secondary"
          className="code-picker__button"
          leadingIcon={none ? undefined : revisionGlyph(value)}
          trailingIcon={<ChevronDownGlyph size="sm" />}
          aria-haspopup="dialog"
          aria-expanded={open}
          aria-label={`${label}: ${word}`}
          onClick={toggle}
        >
          <span className={none ? "code-picker__word" : "code-picker__word mono"}>{word}</span>
        </Button>
      )}
    >
      {(close) => {
        const done = () => {
          close();
          button.current?.focus();
        };
        return (
          <PickerPanel
            repo={repo}
            value={value}
            unset={unset !== undefined}
            title={title}
            field={field}
            close={done}
            onChoose={(rev) => {
              done();
              onChoose(rev);
            }}
          />
        );
      }}
    </Popover>
  );
}

type Tab = "branches" | "tags";

/** What one tab's list holds: its rows, or the line said in their place, and an action after them. */
interface ListState {
  rows: PickerRow[];
  loading: boolean;
  /** A line in place of rows: why there are none, or what is being looked for. */
  status?: string;
  /** A note over the rows. */
  note?: string;
  /** An option after the rows: look on, or ask again. */
  action?: { label: string; run: () => void };
}

const NOTHING: ListState = { rows: [], loading: false };

function PickerPanel({
  repo,
  value,
  unset,
  title,
  field,
  close,
  onChoose,
}: {
  repo: CodeRepoDetail;
  value: string;
  unset: boolean;
  title: string;
  field: RefObject<HTMLInputElement | null>;
  close: () => void;
  onChoose: (rev: string) => void;
}) {
  const id = useId();
  const [tab, setTab] = useState<Tab>("branches");
  const [typed, setTyped] = useState("");
  const [asked, setAsked] = useState("");
  const want = withinBound(typed.trim());
  // ONE PAUSE: each key clears the one before it.
  useEffect(() => {
    const timer = window.setTimeout(() => setAsked(want), FIND_QUIET_MS);
    return () => window.clearTimeout(timer);
  }, [want]);
  const looking = asked !== want && want !== "" ? want : "";
  const branches = useBranchList(repo, value, unset, asked, looking, tab === "branches");
  const tags = useTagList(repo, value, asked, looking, tab === "tags");
  const list = tab === "branches" ? branches : tags;
  const count = list.rows.length + (list.action ? 1 : 0);
  const listbox = useListbox({
    id,
    open: true,
    count,
    // THE LIST IS THE PANEL'S BODY: Escape is the panel's, and closes it.
    popup: false,
    onClose: close,
    onCommit: (index) => {
      const row = list.rows[index];
      if (row) onChoose(row.rev);
      else if (index === list.rows.length) list.action?.run();
    },
  });
  const keys = useOptionKeys({ listbox, count, ends: false });
  const now = useNow();
  const what = tab === "branches" ? "branch" : "tag";
  return (
    <div className="code-picker">
      <div className="code-picker__head">
        <p className="code-picker__title">{title}</p>
        <IconButton label="Close" icon={<XGlyph />} size="sm" variant="ghost" onClick={close} />
      </div>
      <div className="code-picker__find">
        <Input
          ref={field}
          inputSize="sm"
          leading={<SearchGlyph size="sm" />}
          placeholder={`Find a ${what}`}
          aria-label={`Find a ${what}`}
          role="combobox"
          aria-expanded="true"
          aria-autocomplete="list"
          aria-controls={listbox.listId}
          aria-activedescendant={listbox.active >= 0 ? listbox.optionId(listbox.active) : undefined}
          value={typed}
          onChange={(e) => setTyped(e.target.value)}
          onKeyDown={(e) => {
            if (!keys.onKeyDown(e)) listbox.onKeyDown(e);
          }}
        />
      </div>
      <Tabs
        className="code-picker__tabs"
        size="sm"
        variant="underline"
        ariaLabel="Branches or tags"
        value={tab}
        onValueChange={(next) => {
          setTab(next === "tags" ? "tags" : "branches");
          listbox.setActive(0);
        }}
        items={[
          { value: "branches", label: "Branches", count: repo.branches },
          { value: "tags", label: "Tags", count: repo.tags },
        ]}
      />
      <div className="code-picker__body">
        {list.note && <p className="code-picker__note">{list.note}</p>}
        {list.loading && (
          <Skeleton
            variant="text"
            rows={4}
            label={tab === "branches" ? "Loading the branches" : "Loading the tags"}
          />
        )}
        {list.status && (
          <p className="code-picker__status" role="status">
            {list.status}
          </p>
        )}
        <ul
          className="code-picker__list"
          role="listbox"
          id={listbox.listId}
          ref={listbox.listRef}
          aria-label={tab === "branches" ? "Branches" : "Tags"}
        >
          {list.rows.map((row, i) => (
            <li
              key={row.ref}
              id={listbox.optionId(i)}
              role="option"
              aria-selected={i === listbox.active}
              aria-current={row.current ? "true" : undefined}
              className="crewlet-listbox__option code-picker__row"
              {...listbox.optionHandlers(i)}
            >
              <span className="code-picker__lead">
                {row.current && <CheckGlyph size="sm" className="code-picker__check" />}
              </span>
              <span className="code-picker__name mono" title={row.name}>
                {row.name}
              </span>
              <span className="code-picker__marks">
                {row.isDefault && (
                  <Tag appearance="outline" size="xs">
                    Default
                  </Tag>
                )}
                {row.latest && (
                  <Tag appearance="outline" size="xs">
                    Latest
                  </Tag>
                )}
                {row.protection && (
                  <span
                    className="code-picker__rule"
                    role="img"
                    aria-label={protectionTitle(row.protection)}
                    title={protectionTitle(row.protection)}
                  >
                    <ShieldCheckGlyph size="xs" />
                  </span>
                )}
              </span>
              <RowWhen row={row} now={now} />
            </li>
          ))}
          {list.action && (
            <li
              id={listbox.optionId(list.rows.length)}
              role="option"
              aria-selected={listbox.active === list.rows.length}
              className="crewlet-listbox__option code-picker__more"
              {...listbox.optionHandlers(list.rows.length)}
            >
              {list.action.label}
            </li>
          )}
        </ul>
      </div>
      <a
        className="code-picker__foot"
        href={repoHref(repo.name, { tab: "branches", view: tab === "branches" ? "all" : "tags" })}
        onClick={close}
      >
        {tab === "branches" ? "View all branches" : "View all tags"}
        <ArrowRightGlyph size="sm" />
      </a>
    </div>
  );
}

/** When a row's ref moved or was made, short, the whole date in its title. */
function RowWhen({ row, now }: { row: PickerRow; now: number }) {
  if (row.at) {
    return (
      <time className="code-picker__when" dateTime={row.at} title={fmtDateTime(row.at)}>
        {relTime(row.at, now)}
      </time>
    );
  }
  if (row.late) return <span className="code-picker__when">{TAG_LATE}</span>;
  if (row.undated) {
    return (
      <span className="code-picker__when" title={TAG_UNDATED_TITLE}>
        {TAG_UNDATED}
      </span>
    );
  }
  return <span className="code-picker__when" />;
}

/** A read's refusal in the engine's words, or a failure with Try again. */
function unread(
  read: Pick<QueryResult<unknown>, "error" | "detail" | "refusal" | "refetch">,
  refused: string,
  failed: string,
): ListState {
  if (REFUSED_CODES.has(read.error ?? "")) {
    const sentence = refusalSentence(read);
    return { ...NOTHING, status: sentence ? `${refused}: ${sentence}` : refused };
  }
  return { ...NOTHING, status: failed, action: { label: "Try again", run: () => read.refetch() } };
}

/** A find with no row on its page: look on where the engine stopped short, else say none holds it. */
function noneFound(what: string, q: string, next: string, look: (next: string) => void): ListState {
  return next
    ? {
        ...NOTHING,
        status: `None among the ${what === "branch" ? "branches" : "tags"} read so far`,
        action: { label: "Keep looking", run: () => look(next) },
      }
    : {
        ...NOTHING,
        status: `No ${what} name holds “${q}”`,
        note: "A find matches any part of a name, in any case.",
      };
}

/** A find's cursor, kept for the find it looks on from: a new find starts again. */
function useCursor(find: string): [string, (next: string) => void] {
  const [at, setAt] = useState({ find: "", cursor: "" });
  return [at.find === find ? at.cursor : "", (cursor) => setAt({ find, cursor })];
}

/** The Branches tab: by when each moved with the default pinned first, or a find's page. */
function useBranchList(
  repo: CodeRepoDetail,
  value: string,
  unset: boolean,
  q: string,
  looking: string,
  shown: boolean,
): ListState {
  const [cursor, setCursor] = useCursor(q);
  const params = useMemo(
    () => ({
      name: repo.name,
      order: "moved",
      detail: true,
      limit: PICKER_LIMIT,
      ...(q ? { contains: q } : {}),
      ...(cursor ? { cursor } : {}),
    }),
    [repo.name, q, cursor],
  );
  const page = useCodeQuery("code_refs", params, { enabled: shown });
  const rows = page.data?.refs_detail;
  const defaultRef = `refs/heads/${repo.default_branch}`;
  const onPage = rows?.find((r) => r.ref === defaultRef) ?? null;
  // THE DEFAULT PINNED FIRST is found by its name only where the page does not hold it.
  const findDefault = shown && q === "" && rows !== undefined && !onPage && Boolean(repo.head);
  const found = useBranchDetail(repo.name, repo.default_branch, findDefault);
  if (!shown) return NOTHING;
  if (looking) return { ...NOTHING, status: `Looking for “${looking}”…` };
  if (page.error) {
    return unread(
      page,
      "Listing branches by when they moved is unavailable",
      "The branches could not be read.",
    );
  }
  if (!rows || (findDefault && found.loading)) return { ...NOTHING, loading: true };
  const listed = branchRows(rows, onPage ?? (findDefault ? found.row : null), {
    defaultBranch: repo.default_branch,
    value,
    unset,
    finding: q !== "",
  });
  const next = page.data?.next ?? "";
  if (q !== "")
    return listed.length > 0
      ? { ...NOTHING, rows: listed }
      : noneFound("branch", q, next, setCursor);
  if (listed.length === 0) return { ...NOTHING, status: "No branch yet" };
  return {
    ...NOTHING,
    rows: listed,
    ...(next && repo.branches > PICKER_LIMIT
      ? { note: `The ${PICKER_LIMIT} most recently moved. View all for the rest.` }
      : {}),
  };
}

/** The Tags tab: newest first by their own date, or a find by name. */
function useTagList(
  repo: CodeRepoDetail,
  value: string,
  q: string,
  looking: string,
  shown: boolean,
): ListState {
  const [cursor, setCursor] = useCursor(q);
  const newest = useCodeQuery(
    "code_tags",
    useMemo(() => ({ name: repo.name, limit: PICKER_LIMIT }), [repo.name]),
    { enabled: shown && q === "" },
  );
  const findParams = useMemo(
    () => ({
      name: repo.name,
      prefix: TAG_PREFIX,
      contains: q,
      limit: PICKER_LIMIT,
      ...(cursor ? { cursor } : {}),
    }),
    [repo.name, q, cursor],
  );
  const found = useCodeQuery("code_refs", findParams, { enabled: shown && q !== "" });
  if (!shown) return NOTHING;
  if (looking) return { ...NOTHING, status: `Looking for “${looking}”…` };
  if (q === "") {
    if (newest.error) {
      return unread(newest, "Listing tags by date is unavailable", "The tags could not be read.");
    }
    const tags = newest.data?.tags;
    if (!tags) return { ...NOTHING, loading: true };
    const rows = tagRows(tags.tags ?? [], value);
    return rows.length === 0 ? { ...NOTHING, status: "No tag yet" } : { ...NOTHING, rows };
  }
  if (found.error) {
    return unread(found, "Finding a tag by name is unavailable", "The tags could not be read.");
  }
  if (!found.data) return { ...NOTHING, loading: true };
  const rows = foundTagRows(found.data.refs ?? [], value);
  if (rows.length === 0) return noneFound("tag", q, found.data.next ?? "", setCursor);
  return { ...NOTHING, rows, note: "Found by name; dates are on View all tags." };
}
