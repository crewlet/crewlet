/**
 * What every Doclet screen draws the same way: who a handle is, a page's
 * glyph and the words beside its title, the row of spaces at the top, and
 * the search in the page bar.
 *
 * # Who a handle is
 *
 * Every author, last saver, commenter, revision author and change actor is ONE
 * NAME, the one the record carries (`iam.ActorFor`): a person writes AS their
 * seat, so the name is the seat's handle, and a credential the directory binds
 * to no seat writes under its own login. A handle the chart holds is drawn as
 * that seat, with the face the chart gives it ([usePeople]), and anything else
 * is a login, drawn as its own words. There is no second name beside the
 * author to prefer over it, so a page's author, its last save and its activity
 * can never name one person three ways; a change the engine itself made names
 * nobody and is drawn as the engine ([changedBy]). The page reader rebuilt every badge
 * from a name and a kind, which dropped each agent's own character and drew a
 * seat the chart no longer holds as an agent.
 *
 * # Colour is a state, never a kind
 *
 * A draft is a state and wears the warning tone; a tool skill and the
 * onboarding page are KINDS and are drawn neutral, as words ([PageTags]). The
 * reader drew both kinds in state tones, so the onboarding page read as a page
 * with something wrong with it.
 */

import { useEffect, useId, useRef, useState, type ReactNode } from "react";
import { Button, Input, KeyChip, Menu, Tabs, Tag } from "@crewlethq/ui";
import {
  ChevronDownGlyph,
  FileTextGlyph,
  FolderGlyph,
  PlusGlyph,
  SearchGlyph,
} from "@crewlethq/icons/glyphs";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { href, useNavigator, useRoute } from "~/app/router.tsx";
import { useSearchTarget } from "~/app/searchTarget.ts";
import { plural } from "~/lib/format.ts";
import { authorWords } from "~/lib/attribution.ts";
import { plainText } from "~/lib/markdownText.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useSearchQuery } from "~/lib/useSearchQuery.ts";
import { useSeatBadgeOf, type SeatBadge } from "~/lib/seats.ts";
import {
  SEARCH_MODES,
  asSearchMode,
  defaultSearchMode,
  modeLabel,
  modeUnavailable,
} from "~/lib/search.ts";
import { useWriteAccess } from "~/lib/useWriteAccess.ts";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import type { QueryErrorCode } from "~/contract/errors.ts";
import { PAGE_CHANGES } from "~/contract/pages.ts";
import type {
  LogRefusal,
  PageChange,
  PageContainer,
  PageStatus,
  QueryRefusal,
  SearchMode,
} from "~/protocol/index.ts";
import { repeatsTitle } from "./page/body.ts";

/** How often the spaces are read again: a space is created by a config apply. */
export const SPACES_POLL_MS = 60_000;

/** The address of one page: its id, which a rename does not move. */
export const pagePath = (id: string): string[] => ["doclet", "pages", id];

/** The composer's address, filed in a space and under a page where one is named. */
export function newPagePath(
  space?: string,
  parent?: string,
): { path: string[]; query: Record<string, string> } {
  const query: Record<string, string> = {};
  if (space) query.space = space;
  if (parent) query.parent = parent;
  return { path: ["doclet", "new"], query };
}

/** Who a handle is, with the face the chart gives them. */
export type Who = (handle: string) => SeatBadge;

/**
 * Every seat by handle or name, with its face. A handle the chart does not
 * hold keeps its own words and no kind, which is a third answer rather than
 * "an agent" ([SeatBadge]).
 */
export function usePeople(): Who {
  return useSeatBadgeOf();
}

const CHANGE_WORDS = new Map<string, { verb: string; phrase: string }>(
  PAGE_CHANGES.map((c) => [c.kind, { verb: c.verb, phrase: c.phrase }]),
);

/**
 * What a change to a page reads as: `verb` before the page's title, `phrase`
 * where the page goes without saying. A kind this build has no words for (a
 * newer engine's) reads as itself rather than as nothing.
 */
export function changeWords(kind: string): { verb: string; phrase: string } {
  return CHANGE_WORDS.get(kind) ?? { verb: kind, phrase: kind };
}

/**
 * The note a change carries under who made it, as the prose it renders to, or
 * "" when it has none worth drawing: what a save's writer said it changed, a
 * comment's words, a new page's first line.
 *
 * NEVER THE TITLE AGAIN. A new page's excerpt is the body's first line, and a
 * seat opens a page with `# {title}` more often than not, so a row named the
 * page and then named it again as its note. Such a note is dropped rather than
 * replaced: the line under it is in the page, and a feed carries no body to
 * read it from.
 */
export function changeNote(change: Pick<PageChange, "excerpt" | "title">): string {
  const note = plainText(change.excerpt ?? "");
  return repeatsTitle(note, change.title ?? "") ? "" : note;
}

/**
 * Who made a change, as the one name a row draws: the name the record carries
 * (`iam.ActorFor`), a seat's handle or a credential's login, and "" for the
 * engine's own change, which a row draws as the engine rather than as the name
 * it writes under.
 */
export function changedBy(change: Pick<PageChange, "actor" | "actor_kind">): string {
  return change.actor_kind === "system" ? "" : (change.actor ?? "");
}

/**
 * Who made a change, as its line names them: the writer's name, "with the
 * assistant" where the write came through the Dashboard Assistant
 * (`authorWords`, off the server-set `operator_id`), or "The engine". The
 * grant id is never printed.
 */
export function changeAuthor(
  change: Pick<PageChange, "actor" | "actor_kind" | "operator_id">,
  who: Who,
): string {
  const by = changedBy(change);
  return by ? authorWords(who(by).name, change.operator_id) : "The engine";
}

/** A seat's face, sized for a line of text, and named by the words beside it. */
export function Face({
  handle,
  who,
  size = "xs",
}: {
  handle: string;
  who: Who;
  size?: "xs" | "sm";
}) {
  const seat = who(handle);
  return (
    <SeatAvatar
      name={seat.name}
      // A KIND THE CHART DOES NOT KNOW is drawn with the kit's default
      // outline, the agent's; the name beside it is the handle as written.
      kind={seat.kind ?? "agent"}
      avatar={seat.avatar}
      size={size}
      decorative
    />
  );
}

/** A page's glyph: a folder for a page with nothing written in it, else a document. */
export function PageGlyph({ folder }: { folder?: boolean | undefined }) {
  return folder ? <FolderGlyph size="sm" /> : <FileTextGlyph size="sm" />;
}

/**
 * The words a page wears beside its title: Draft and Trashed, states, Draft in
 * its tone; and Tool skill and Onboarding, kinds, as neutral outlined words.
 */
export function PageTags({
  status,
  skill,
  onboarding,
}: {
  status?: PageStatus | undefined;
  skill?: boolean | undefined;
  onboarding?: boolean | undefined;
}) {
  return (
    <>
      {status === "draft" && (
        <Tag size="xs" variant="warning" title="Unfinished: listed here and found by no search">
          Draft
        </Tag>
      )}
      {status === "trashed" && (
        <Tag size="xs" variant="neutral" title="In the trash, hidden from search">
          Trashed
        </Tag>
      )}
      {skill && (
        <Tag size="xs" appearance="outline" title="Instructions for agents">
          Tool skill
        </Tag>
      )}
      {onboarding && (
        <Tag size="xs" appearance="outline" title="Where a new seat's reading starts">
          Onboarding
        </Tag>
      )}
    </>
  );
}

/** The company's spaces, polled with the rest of Doclet. */
export function useSpaces(): {
  spaces: PageContainer[];
  /** The spaces have answered at least once. */
  read: boolean;
  loading: boolean;
  error: QueryErrorCode | null;
  /** Why the read was refused, beside `error`: what `QueryState` names. */
  refusal: QueryRefusal | LogRefusal | null;
} {
  const containers = useQuery("containers", undefined, { pollMs: SPACES_POLL_MS });
  return {
    spaces: containers.data?.containers ?? [],
    read: containers.data !== null,
    loading: containers.loading,
    error: containers.error,
    refusal: containers.refusal,
  };
}

/**
 * Every space in one row, labelled Spaces and drawn with the folder and the key
 * a space carries everywhere else: the landing's way in, and kept on a space's
 * own page with that space marked.
 *
 * LINKS IN A NAVIGATION LANDMARK, never tabs: each one goes to a place, and a
 * screen reader told it was a tab would look for a panel that never arrives.
 * The one the reader is on carries `aria-current="page"`.
 */
export function SpacesRow({ current }: { current?: string | undefined }) {
  const { spaces } = useSpaces();
  if (spaces.length === 0) return null;
  return (
    <div className="knowledge-spaces">
      {/* The landmark is named by its own label; the drawn word is the same
          name, so it is not read twice. */}
      <span className="knowledge-spaces-label" aria-hidden="true">
        Spaces
      </span>
      <Tabs
        variant="chip"
        size="sm"
        ariaLabel="Spaces"
        value={current ?? ""}
        items={spaces.map((space) => ({
          value: space.key,
          label: space.name || space.key,
          icon: <FolderGlyph size="sm" />,
          purpose: space.purpose ?? "",
          pages: space.pages,
        }))}
        renderItem={({ item, className, isActive, children }) => (
          <a
            className={className}
            href={href(["doclet", item.value])}
            aria-current={isActive ? "page" : undefined}
            title={[
              `${String(item.label)}: ${plural(Number(item.pages), "page")}`,
              String(item.purpose ?? ""),
            ]
              .filter(Boolean)
              .join(". ")}
          >
            {children}
            <KeyChip aria-hidden="true">{item.value}</KeyChip>
          </a>
        )}
      />
    </div>
  );
}

/**
 * The search, in the page bar of every Doclet screen: a field, and the
 * ranking mode in a menu BESIDE it. A control inside a text box is one a
 * pointer reaches by missing the box, so the menu sits next to the field
 * rather than in it.
 *
 * It navigates to the results with the phrase and the mode in the address, so
 * a search is a place a reader can come back to.
 */
export function DocletSearchBar() {
  const route = useRoute();
  const nav = useNavigator();
  const box = useRef<HTMLInputElement>(null);
  useSearchTarget(box);
  const onResults = route.path.length === 1 && route.path[0] === "doclet";
  const urlQ = onResults ? (route.query.get("q") ?? "") : "";
  const urlMode = onResults ? route.query.get("mode") : null;
  const [draft, setDraft] = useState(urlQ);
  // WHAT THE READER CHOSE, or null for the default, which is the probe's to
  // decide, so it is resolved at render rather than copied into state first.
  const [picked, setPicked] = useState<SearchMode | null>(urlMode ? asSearchMode(urlMode) : null);
  // THE ADDRESS WINS WHEN IT MOVES: Back to an earlier search, or a pasted
  // link, puts that search's phrase and mode in the box.
  useEffect(() => {
    if (onResults) setDraft(urlQ);
  }, [onResults, urlQ]);
  useEffect(() => {
    if (onResults) setPicked(urlMode ? asSearchMode(urlMode) : null);
  }, [onResults, urlMode]);

  const probe = useSearchQuery(
    "knowledge",
    { q: "", mode: "semantic" },
    { pollMs: SPACES_POLL_MS },
  );
  // THE CHOSEN MODE IS NEVER ONE THE MENU SAYS IT CANNOT RUN unless the reader
  // asked for it by name: with no embeddings provider the default is Keyword.
  const mode = picked ?? defaultSearchMode(probe.data);
  const reasonID = useId();

  const go = (phrase: string, how: SearchMode) => {
    const query: Record<string, string> = {};
    if (phrase) query.q = phrase;
    if (how !== "hybrid") query.mode = how;
    nav.to(["doclet"], query);
  };

  return (
    // THE BAR'S FIND FIELD (`page-find`, as a gallery's search is): on a
    // phone the field gives way, so New page and More stay in view where the
    // line scrolled them past the edge.
    <div className="knowledge-search page-find">
      <form
        role="search"
        aria-label="Search the knowledge base"
        onSubmit={(event) => {
          event.preventDefault();
          go(draft.trim(), mode);
        }}
      >
        <Input
          type="search"
          inputSize="sm"
          width="full"
          ref={box}
          aria-label="Search pages"
          aria-describedby={reasonID}
          placeholder="Search pages"
          leading={<SearchGlyph size="sm" />}
          value={draft}
          onChange={(event) => setDraft(event.target.value)}
          clearLabel="Clear the search"
          onClear={() => {
            setDraft("");
            if (onResults && urlQ) go("", mode);
          }}
        />
      </form>
      <Menu
        label={`Rank by: ${modeLabel(mode)}`}
        trigger={modeLabel(mode)}
        trailingIcon={<ChevronDownGlyph size="sm" />}
        triggerVariant="ghost"
        align="end"
        items={SEARCH_MODES.map((m) => {
          const why = modeUnavailable(probe.data, m.value);
          return {
            key: m.value,
            label: m.label,
            // WRITTEN, NOT ONLY HOVERED: why a mode cannot run is its second
            // line, which a keyboard and a phone reach as readily as a pointer.
            description: why ?? m.hint,
            disabled: why !== null,
            checked: m.value === mode,
            onSelect: () => {
              setPicked(m.value);
              // ON THE RESULTS A MODE CHANGES WHAT IS SHOWN, so it runs again
              // there; anywhere else it is the mode the next search runs in.
              if (onResults && urlQ) go(urlQ, m.value);
            },
          };
        })}
      />
      <span id={reasonID} hidden>
        {`Ranked by ${modeLabel(mode)}.`}
      </span>
    </div>
  );
}

/** The page bar every Doclet screen draws: the search, then the screen's own actions. */
export function DocletBar({ children }: { children?: ReactNode }) {
  return (
    <PageActions>
      <DocletSearchBar />
      {children}
    </PageActions>
  );
}

/**
 * "New page": the composer, filed in the space the screen is about where it
 * is about one. Drawn for every reader, and disabled with the reason for one
 * who cannot write.
 */
export function NewPageButton({ space }: { space?: string | undefined }) {
  const nav = useNavigator();
  const can = useWriteAccess("write_page");
  const to = newPagePath(space);
  return (
    <Button
      size="small"
      variant="secondary"
      leadingIcon={<PlusGlyph size="sm" />}
      disabledReason={can.can ? undefined : can.reason}
      title={can.can ? "Files the page in a space you choose" : can.reason}
      onClick={() => nav.to(to.path, to.query)}
    >
      New page
    </Button>
  );
}
