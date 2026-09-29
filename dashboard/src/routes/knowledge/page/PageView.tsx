/**
 * One page, as the approved Knowledge artboard draws it: the document in a
 * reading column, and a rail beside it saying where to go on it, who read it,
 * what links to it and how it got here.
 *
 * # Addressed by its id
 *
 * `#/knowledge/pages/{id}`, and by nothing else: a title changes on rename,
 * so every link anybody had copied broke the day somebody fixed a heading.
 * The trail above it (Knowledge › space › the pages above it › the page) is
 * the page's PLACE, published to the page bar from the page's own answer.
 *
 * # What a wiki cannot tell you, and this can
 *
 * Its readers are seats, and every read is a recorded act — so the page says
 * who read it, how (the turn it was read in, the search it came up for) and
 * how many read it on the company's today; a tool-skill page says who it was
 * loaded by; and "linked from" is every page and task pointing at it. All of
 * it is the engine's (`page_reads`, `page.skill_loaded_by`,
 * `page.linked_from`); nothing here is counted in the browser.
 *
 * # Edited as you
 *
 * Edit (`edit=1`) writes through `save_page` as the person the token is bound
 * to, against the revision they opened — see `Editor.tsx` for what a save
 * that raced somebody else's does. A comment and a new sub-page write the same
 * way. For a reader who cannot write, every control stays drawn and says why.
 */

import { useMemo, useState } from "react";
import { Button, EmptyState, Menu, Skeleton, Tag } from "@crewlethq/ui";
import {
  ClockGlyph,
  EllipsisGlyph,
  FileTextGlyph,
  PencilGlyph,
  PlusGlyph,
  WandSparklesGlyph,
} from "@crewlethq/icons/glyphs";
import { useParam } from "~/app/router.tsx";
import { pageSpaceKey, pageUpKey } from "~/app/crumbs.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { usePageLabels, usePageMenu } from "~/app/Shell.tsx";
import { useFillScreen } from "~/app/fill.tsx";
import { PHONE_BREAKPOINT } from "~/app/layout.ts";
import { QueryState } from "~/components/common.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg, seatLookup } from "~/lib/seats.ts";
import { useNow } from "~/lib/clock.ts";
import { useMediaQuery } from "~/lib/media.ts";
import { fmtDateTime, relTime } from "~/lib/format.ts";
import { outline, renderMarkdown } from "~/lib/markdown.ts";
import { loadedBy, readToday } from "~/lib/pageReads.ts";
import { menuHold, useWriteAccess } from "~/lib/useWriteAccess.ts";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import type { PageDetail } from "~/protocol/index.ts";
import { NewPageDialog } from "../NewPage.tsx";
import { PageComments } from "./Comments.tsx";
import { PageEditor } from "./Editor.tsx";
import { PageChanges, VersionView } from "./History.tsx";
import {
  Children,
  LinkedFrom,
  OnThisPage,
  ReadBy,
  ReadToday,
  Revisions,
  Watchers,
} from "./Rail.tsx";

type Who = (handle: string) => { name: string; kind?: "agent" | "human" };

/** How often a page re-reads itself, and how often who read it. */
const PAGE_POLL_MS = 20_000;
const READS_POLL_MS = 60_000;

/**
 * The body without a first heading that only repeats the page's title.
 *
 * A seat writing a page often opens it with `# {title}`, and drawn under the
 * page's own title that is the same words twice, the second smaller. Only an
 * EXACT repeat on the first line goes; any other opening heading is content.
 */
export function withoutRepeatedTitle(body: string, title: string): string {
  const match = /^\s*#{1,6}\s+(.+?)\s*#*\s*(?:\n|$)/.exec(body);
  if (!match || match[1]!.trim().toLowerCase() !== title.trim().toLowerCase()) return body;
  return body.slice(match[0].length).replace(/^\s*\n/, "");
}

/** The state a page wears beside its meta line when it is not plain prose. */
function stateTags(detail: PageDetail) {
  const page = detail.page;
  return (
    <>
      {page.status !== "published" && (
        <Tag size="xs" variant={page.status === "draft" ? "warning" : "neutral"} dot>
          {page.status}
        </Tag>
      )}
      {detail.onboarding && (
        <Tag size="xs" variant="warning" title="Where a new seat's reading starts">
          onboarding
        </Tag>
      )}
    </>
  );
}

/** "Loaded as a skill by SWE, CTO +2" — on a tool-skill page only. */
function SkillPill({ detail, who }: { detail: PageDetail; who: Who }) {
  if (!detail.skill) return null;
  const loads = detail.skill_loaded_by;
  const said = loads ? loadedBy(loads, (h) => who(h).name) : null;
  return (
    <Tag
      size="xs"
      variant="neutral"
      leadingIcon={<WandSparklesGlyph size="sm" />}
      title="A tool skill: machinery the engine offers a phase, not prose written to be read"
    >
      {said
        ? `${said.verb} as a skill by ${said.names}${said.more ? ` +${said.more}` : ""}`
        : loads
          ? "Tool skill · not loaded in 30 days"
          : "Tool skill"}
    </Tag>
  );
}

/** The title and the line under it: who saved it last, when, and its revision. */
function DocHead({ detail, who, now }: { detail: PageDetail; who: Who; now: number }) {
  const page = detail.page;
  // THE LAST SAVE IS THE NEWEST REVISION, not the page's author: a page's
  // `author` is who STARTED it and no save moves it, so "updated by" beside it
  // would credit the creator with every later edit.
  const last = detail.history?.[0];
  const by = last ? last.author : page.author;
  const at = last ? last.created_at : page.updated_at;
  const seat = by ? who(by) : null;
  return (
    <header className="kpage-head">
      <h1 className="kpage-title">{page.title}</h1>
      <div className="kpage-meta">
        {seat && <SeatAvatar name={seat.name} kind={seat.kind ?? "agent"} size="xs" decorative />}
        <span>
          Updated by <b>{seat ? seat.name : "the engine"}</b>{" "}
          <time dateTime={at} title={fmtDateTime(at)}>
            {relTime(at, now)}
          </time>
        </span>
        <span aria-hidden="true">·</span>
        <span>revision {page.version}</span>
        {(detail.skill || detail.onboarding || page.status !== "published") && (
          <span aria-hidden="true">·</span>
        )}
        <SkillPill detail={detail} who={who} />
        {stateTags(detail)}
      </div>
    </header>
  );
}

export function PageView({ id }: { id: string }) {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const who: Who = useMemo(() => {
    const look = seatLookup(index);
    return (handle: string) => {
      const seat = look(handle);
      return { name: seat.name, kind: seat.kind === "human" ? "human" : "agent" };
    };
  }, [index]);
  const now = useNow();
  const phone = useMediaQuery(`(width < ${PHONE_BREAKPOINT}px)`);
  // THE DOCUMENT AND ITS RAIL EACH SCROLL ON THEIR OWN, the way the task page's
  // do — above a phone, where they are one column and the page scrolls.
  useFillScreen(!phone);

  const { data, loading, error } = useQuery(
    "page",
    { id },
    { enabled: id !== "", pollMs: PAGE_POLL_MS },
  );
  const reads = useQuery("page_reads", { page: id }, { enabled: id !== "", pollMs: READS_POLL_MS });
  const containers = useQuery("containers", undefined, { pollMs: 60_000 });
  const [edit, setEdit] = useParam("edit", "");
  const [versionParam, setVersion] = useParam("version", "");
  const version = Number(versionParam) || 0;
  const [newChild, setNewChild] = useState(false);
  const canEdit = useWriteAccess("save_page");
  const canWrite = useWriteAccess("write_page");

  const page = data?.page;
  const history = useMemo(() => data?.history ?? [], [data]);
  const ancestors = useMemo(() => data?.ancestors ?? [], [data]);
  const space = containers.data?.containers.find((c) => c.key === page?.container);

  // THE TRAIL: the space and every page above this one, published for the
  // page bar (`crumbs.ts`).
  usePageLabels(
    page
      ? {
          [id]: page.title,
          [pageSpaceKey(id)]: page.container,
          ...(space?.name ? { [page.container]: space.name } : {}),
          ...Object.fromEntries(
            ancestors.flatMap((a, i) => [
              [pageUpKey(id, i), a.id],
              [a.id, a.title],
            ]),
          ),
        }
      : {},
  );

  const editing = edit === "1" && Boolean(page) && canEdit.can;
  const body = useMemo(
    () => (page ? withoutRepeatedTitle(page.body ?? "", page.title) : ""),
    [page],
  );
  const headings = useMemo(() => outline(body), [body]);
  const readers = reads.data?.readers;
  const faces = useMemo(
    () => (readers ? readToday(readers, now, org?.timezone) : []),
    [readers, now, org?.timezone],
  );

  const toggleHistory = () => {
    setEdit("");
    setVersion(version > 0 || history.length === 0 ? "" : String(history[0]!.version));
  };
  const historyEntry = {
    key: "history",
    label: version > 0 ? "Back to the page" : "History",
    icon: <ClockGlyph size="sm" />,
    onSelect: toggleHistory,
    ...(history.length === 0
      ? { disabled: true, description: "Nobody has saved over it yet." }
      : {}),
  };
  const newSubPage = {
    key: "sub-page",
    label: "New sub-page",
    icon: <PlusGlyph size="sm" />,
    onSelect: () => setNewChild(true),
    ...menuHold(canWrite),
  };
  usePageMenu(page ? [historyEntry, newSubPage] : []);

  if (error === "not_found") {
    return (
      <EmptyState
        icon={<FileTextGlyph size="xl" />}
        title="There is no such page"
        description="A link to a page carries its id, which a rename does not change. It may have been trashed or purged — or this node's copy of the knowledge base has not caught up with it yet."
      />
    );
  }

  return (
    <>
      <PageActions>
        {/* ON A PHONE THE COUNT AND HISTORY FOLD: the rail below the page
            lists who read it and every revision, and the bar's one line
            keeps Edit in view (`page-action-folds`). */}
        {reads.data && (
          <span className="page-action-folds">
            <ReadToday count={reads.data.distinct_seats_today} faces={faces} who={who} />
          </span>
        )}
        {page && (
          <>
            <span className="page-action-folds">
              <Button
                size="small"
                variant="ghost"
                leadingIcon={<ClockGlyph size="sm" />}
                disabledReason={history.length === 0 ? "Nobody has saved over it yet." : undefined}
                aria-pressed={version > 0}
                onClick={toggleHistory}
              >
                History
              </Button>
            </span>
            <Button
              size="small"
              variant="secondary"
              leadingIcon={<PencilGlyph size="sm" />}
              disabledReason={
                canEdit.can ? (editing ? "You are editing it." : undefined) : canEdit.reason
              }
              title={canEdit.can ? undefined : canEdit.reason}
              onClick={() => {
                setVersion("");
                setEdit("1");
              }}
            >
              Edit
            </Button>
            <span className="page-action-folds">
              <Menu
                label={`More for “${page.title}”`}
                icon={<EllipsisGlyph size="sm" />}
                align="end"
                items={[newSubPage]}
              />
            </span>
          </>
        )}
      </PageActions>

      {loading && !data && <Skeleton variant="text" rows={10} label="Loading the page" />}
      <QueryState error={error} loading={loading}>
        {data && page && (
          <div className="kpage">
            <div className="kpage-grid">
              <div className="kpage-main">
                <article className="kpage-doc" aria-label={page.title}>
                  <DocHead detail={data} who={who} now={now} />
                  {editing ? (
                    <PageEditor key={page.id} detail={data} onDone={() => setEdit("")} />
                  ) : version > 0 ? (
                    <VersionView
                      pageID={page.id}
                      version={version}
                      history={history}
                      seatName={(h) => who(h).name}
                      now={now}
                      onClose={() => setVersion("")}
                    />
                  ) : body.trim() ? (
                    <div className="prose md kpage-prose">
                      {renderMarkdown(body, { anchors: true })}
                    </div>
                  ) : (
                    <p className="muted">This page has no body.</p>
                  )}
                  {edit === "1" && !canEdit.can && (
                    <p className="t-caption kpage-readonly">
                      You cannot edit it here: {canEdit.reason}
                    </p>
                  )}
                </article>
                <PageComments
                  pageID={page.id}
                  title={page.title}
                  comments={data.comments ?? []}
                  who={who}
                  now={now}
                />
                <PageChanges pageID={page.id} seatName={(h) => who(h).name} now={now} />
              </div>
              <aside className="kpage-rail" aria-label={`About “${page.title}”`}>
                {!editing && version === 0 && <OnThisPage headings={headings} />}
                <ReadBy reads={reads.data} error={reads.error} who={who} now={now} />
                <LinkedFrom links={data.linked_from} status={data.linked_from_status} />
                <Revisions
                  history={history}
                  open={version}
                  onOpen={(v) => {
                    setEdit("");
                    setVersion(v > 0 ? String(v) : "");
                  }}
                  who={who}
                  now={now}
                />
                <Children children={data.children ?? []} total={data.children_total ?? 0} />
                <Watchers watchers={page.watchers ?? []} who={who} />
              </aside>
            </div>
          </div>
        )}
      </QueryState>
      {newChild && page && (
        <NewPageDialog
          container={page.container}
          parent={{ id: page.id, title: page.title }}
          onClose={() => setNewChild(false)}
        />
      )}
    </>
  );
}
