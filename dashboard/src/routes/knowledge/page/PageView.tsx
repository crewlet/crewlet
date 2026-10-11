/**
 * One page, as the approved Knowledge Home draws it: the outline of what it
 * says to its left, following the scroll, and the page itself in one column,
 * its properties under its title, its comments under its body and what it is
 * connected to under those.
 *
 * # Addressed by its id
 *
 * `#/knowledge/pages/{id}`, and by nothing else: a title changes on rename,
 * so every link anybody had copied broke the day somebody fixed a heading.
 * The trail above it (Knowledge, its space, the pages above it, the page) is
 * the page's PLACE, published to the page bar from the page's own answer.
 *
 * # The page bar
 *
 * The eye ([WatchControl]), History, Edit and More. More holds the star,
 * which is why this page tells the frame to draw none of its own
 * ([useOwnStar]): one control per action on screen. History swaps the outline
 * for the revisions and the comments for the page's changes; Edit writes
 * through `save_page` as the person the token is bound to, against the
 * revision they opened (`Editor.tsx`). For a reader who cannot write, every
 * control stays drawn and says why.
 *
 * # What a wiki cannot tell you, and this can
 *
 * Its readers are seats and every read is a recorded act, so the page says
 * who read it and how (the turn it was read in, the search it came up for); a
 * tool-skill page says who it was loaded by; and "Linked from" is every page
 * and task pointing at it. All of it is the engine's (`page_reads`,
 * `skill_loaded_by`, `linked_from`); nothing here is counted in the browser.
 */

import { useEffect, useMemo, useState } from "react";
import {
  Button,
  DescriptionList,
  Document,
  DocumentTitle,
  EmptyState,
  Menu,
  Outline,
  Skeleton,
  scrollToHeading,
  useActiveHeading,
  useToast,
} from "@crewlethq/ui";
import {
  ClockGlyph,
  CopyGlyph,
  EllipsisGlyph,
  FileTextGlyph,
  PencilGlyph,
  PlusGlyph,
  StarGlyph,
} from "@crewlethq/icons/glyphs";
import { pageSpaceKey, pageUpKey } from "~/app/crumbs.ts";
import { href, useNavigator, useParam } from "~/app/router.tsx";
import { useOwnStar, usePageLabels, usePageMenu } from "~/app/Shell.tsx";
import type { PageMenuEntry } from "~/app/header/PageHeader.tsx";
import { QueryState } from "~/components/common.tsx";
import { useNow } from "~/lib/clock.ts";
import { fmtDateTime, relTime } from "~/lib/format.ts";
import { outline, renderMarkdown } from "~/lib/markdown.ts";
import { copyPageLink } from "~/lib/pageLink.ts";
import { loadedBy } from "~/lib/pageReads.ts";
import { SCREEN_SCROLL_ID } from "~/lib/scroller.ts";
import { MaxStars, starredIn, useStarred, useToggleStar } from "~/lib/starred.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { menuHold, useWriteAccess } from "~/lib/useWriteAccess.ts";
import type { PageDetail } from "~/protocol/index.ts";
import {
  Face,
  KnowledgeBar,
  drawnBy,
  newPagePath,
  pagePath,
  usePeople,
  useSpaces,
  type Who,
} from "../parts.tsx";
import { withoutRepeatedTitle } from "./body.ts";
import { PageComments } from "./Comments.tsx";
import { Connections, Revisions } from "./Connections.tsx";
import { PageEditor } from "./Editor.tsx";
import { PageChanges, VersionView } from "./History.tsx";
import { WatchControl } from "./Watch.tsx";

/** How often a page reads itself again, and how often who read it. */
const PAGE_POLL_MS = 20_000;
const READS_POLL_MS = 60_000;

/**
 * Where a heading counts as the one being read: a fifth of the way down the
 * scroller, so the section filling the screen is the one marked.
 */
const READING_LINE = 0.2;

/** What kind of page this is, when it is not plain prose, as one line. */
function kindOf(detail: PageDetail, who: Who): string {
  const loads = detail.skill_loaded_by;
  const said = loads ? loadedBy(loads, (h) => who(h).name) : null;
  const skill = !detail.skill
    ? ""
    : said
      ? `Tool skill, ${said.verb.toLowerCase()} as a skill by ${said.names}${said.more ? ` and ${said.more} more` : ""} in 30 days`
      : loads
        ? "Tool skill, not loaded in 30 days"
        : "Tool skill";
  return [
    detail.page.status === "draft" ? "Draft, found by no search until it is published" : "",
    detail.page.status === "trashed" ? "In the trash" : "",
    detail.onboarding ? "Onboarding, read first by every new seat" : "",
    skill,
  ]
    .filter(Boolean)
    .join(" · ");
}

/**
 * The page's properties, minimal by rule: where it is filed, who wrote it,
 * who last saved what it says, and what kind of page it is. The revision and
 * the note kept with the save are on the save's own line, read by hovering it
 * or by a screen reader, because History in the page bar opens them in full.
 */
function PageProps({
  detail,
  spaceName,
  who,
  now,
}: {
  detail: PageDetail;
  spaceName: string;
  who: Who;
  now: number;
}) {
  const page = detail.page;
  const last = detail.history?.[0];
  const author = drawnBy(page.author, detail.author_seat);
  const saver = last ? drawnBy(last.author, last.author_seat) : "";
  const kind = kindOf(detail, who);
  const revision = last
    ? `Revision ${last.version}: ${last.message ? `“${last.message}”` : "no note was kept with this save"}`
    : "";
  return (
    <DescriptionList dense className="knowledge-props">
      <DescriptionList.Item term="Space">
        <span className="knowledge-props-place">
          <a className="knowledge-props-link" href={href(["knowledge", page.container])}>
            {spaceName}
          </a>
          {(detail.ancestors ?? []).map((a) => (
            <span key={a.id} className="knowledge-props-step">
              <span aria-hidden="true">/</span>
              <a className="knowledge-props-link" href={href(pagePath(a.id))}>
                {a.title}
              </a>
            </span>
          ))}
        </span>
      </DescriptionList.Item>
      <DescriptionList.Item term="Author">
        {author ? (
          <span className="knowledge-props-person">
            <Face handle={author} who={who} />
            {who(author).name}
          </span>
        ) : (
          "The engine"
        )}
      </DescriptionList.Item>
      <DescriptionList.Item term="Last saved">
        {last ? (
          <span title={revision}>
            <time dateTime={last.created_at} title={fmtDateTime(last.created_at)}>
              {relTime(last.created_at, now)}
            </time>{" "}
            by {saver ? who(saver).name : "the engine"}
            <span className="sr-only">. {revision}</span>
          </span>
        ) : (
          "Not saved over since it was written"
        )}
      </DescriptionList.Item>
      {kind && <DescriptionList.Item term="Kind">{kind}</DescriptionList.Item>}
    </DescriptionList>
  );
}

/**
 * The body as the reader sees it, or, for a FOLDER (a page with nothing
 * written in it), what it groups.
 */
function PageBody({ detail, body }: { detail: PageDetail; body: string }) {
  if (body.trim()) return <Document>{renderMarkdown(body, { anchors: true })}</Document>;
  const children = detail.children ?? [];
  if (!detail.folder) return <p className="knowledge-note">This page has no body.</p>;
  return (
    <div className="knowledge-folder">
      <p className="knowledge-note">
        A folder that groups pages.
        {children.length === 0 &&
          " Nothing is filed under it yet: use Create under it in its space."}
      </p>
      {children.length > 0 && (
        <ul className="knowledge-links" role="list">
          {children.map((child) => (
            <li key={child.id}>
              <a className="knowledge-link" href={href(pagePath(child.id))}>
                <FileTextGlyph size="sm" />
                <span className="knowledge-link-title">{child.title}</span>
              </a>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

/** "On this page", following the scroll. */
function OnThisPage({ body }: { body: string }) {
  const headings = useMemo(() => {
    const all = outline(body);
    if (all.length === 0) return [];
    // THE TWO TOP LEVELS THE DOCUMENT USES, its sections and their parts: a
    // third would make the outline the document.
    const top = Math.min(...all.map((h) => h.level));
    return all.filter((h) => h.level <= top + 1);
  }, [body]);
  const [root, setRoot] = useState<HTMLElement | null>(null);
  useEffect(() => setRoot(document.getElementById(SCREEN_SCROLL_ID)), []);
  const ids = useMemo(() => headings.map((h) => h.id), [headings]);
  const active = useActiveHeading(ids, {
    root,
    offset: Math.round((root?.clientHeight ?? 0) * READING_LINE),
  });
  if (headings.length < 2) return null;
  return (
    <Outline
      label="On this page"
      items={headings.map((h) => ({ id: h.id, label: h.text, level: h.level }))}
      activeId={active}
      onSelect={(id) => scrollToHeading(id)}
    />
  );
}

export function PageView({ id }: { id: string }) {
  const who = usePeople();
  const now = useNow();
  const nav = useNavigator();
  const toast = useToast();
  const { data, loading, error } = useQuery(
    "page",
    { id },
    { enabled: id !== "", pollMs: PAGE_POLL_MS },
  );
  const reads = useQuery("page_reads", { page: id }, { enabled: id !== "", pollMs: READS_POLL_MS });
  const { spaces } = useSpaces();
  const [edit, setEdit] = useParam("edit", "");
  const [versionParam, setVersion] = useParam("version", "");
  const version = Number(versionParam) || 0;
  const canEdit = useWriteAccess("save_page");
  const canWrite = useWriteAccess("write_page");
  const stars = useStarred();
  const toggleStar = useToggleStar();
  // THE STAR IS IN THIS PAGE'S MORE MENU, so the frame draws none beside it.
  useOwnStar();

  const page = data?.page;
  const history = useMemo(() => data?.history ?? [], [data]);
  const ancestors = useMemo(() => data?.ancestors ?? [], [data]);
  const space = spaces.find((c) => c.key === page?.container);
  const spaceName = space?.name || page?.container || "";

  // THE TRAIL: the space and every page above this one, for the page bar.
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
  const reading = version > 0 && !editing;
  const body = useMemo(
    () => (page ? withoutRepeatedTitle(page.body ?? "", page.title) : ""),
    [page],
  );

  const path = pagePath(id);
  const starred = starredIn(stars, path);
  const toggleHistory = () => {
    setEdit("");
    setVersion(version > 0 || history.length === 0 ? "" : String(history[0]!.version));
  };
  const menu: PageMenuEntry[] = page
    ? [
        {
          key: "star",
          label: starred ? "Remove the star" : "Star this page",
          icon: <StarGlyph filled={starred} size="sm" />,
          ...(starred ? {} : { description: "Pinned on Knowledge and kept in the sidebar" }),
          onSelect: () => {
            if (toggleStar({ path, label: page.title, workspace: "knowledge" }) === "full") {
              toast.show({ variant: "warning", title: `${MaxStars} is the limit` });
            }
          },
        },
        {
          key: "sub-page",
          label: "New sub-page",
          icon: <PlusGlyph size="sm" />,
          onSelect: () => {
            const to = newPagePath(page.container, page.id);
            nav.to(to.path, to.query);
          },
          ...menuHold(canWrite),
        },
        {
          key: "history",
          label: reading ? "Back to the page" : "History",
          icon: <ClockGlyph size="sm" />,
          onSelect: toggleHistory,
          ...(history.length === 0
            ? { disabled: true, description: "Nobody has saved over it yet." }
            : {}),
        },
        {
          key: "copy",
          label: "Copy the page's address",
          icon: <CopyGlyph size="sm" />,
          onSelect: () =>
            void copyPageLink().then((word) =>
              word === "Copied"
                ? toast.ok("Copied the page's address")
                : toast.show({
                    variant: "warning",
                    title: "The browser refused the clipboard",
                    message: "Copy the address from the address bar instead.",
                  }),
            ),
        },
      ]
    : [];
  usePageMenu(menu);

  if (error === "not_found") {
    return (
      <>
        <KnowledgeBar />
        <EmptyState
          icon={<FileTextGlyph size="xl" />}
          title="There is no such page"
          description="A link to a page carries its id, which a rename does not change. It may have been trashed or purged, or this node's copy of the knowledge base has not caught up with it yet."
        />
      </>
    );
  }

  return (
    <>
      <KnowledgeBar>
        {page && (
          <>
            <WatchControl page={page} />
            {/* ON A PHONE HISTORY AND MORE FOLD into the frame's menu, which
                holds the same entries (`usePageMenu`), and Edit stays. */}
            <span className="page-action-folds">
              <Button
                size="small"
                variant="ghost"
                leadingIcon={<ClockGlyph size="sm" />}
                disabledReason={history.length === 0 ? "Nobody has saved over it yet." : undefined}
                aria-pressed={reading}
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
                triggerVariant="ghost"
                align="end"
                items={menu}
              />
            </span>
          </>
        )}
      </KnowledgeBar>

      {loading && !data && <Skeleton variant="text" rows={10} label="Loading the page" />}
      <QueryState error={error} loading={loading}>
        {data && page && (
          <div className="knowledge-read">
            <div className="knowledge-read-side">
              {reading ? (
                <Revisions
                  history={history}
                  open={version}
                  onOpen={(v) => setVersion(String(v))}
                  who={who}
                  now={now}
                />
              ) : (
                !editing && <OnThisPage body={body} />
              )}
            </div>
            <article className="knowledge-read-page" aria-label={page.title}>
              <DocumentTitle>{page.title}</DocumentTitle>
              <PageProps detail={data} spaceName={spaceName} who={who} now={now} />
              {editing ? (
                <PageEditor key={page.id} detail={data} onDone={() => setEdit("")} />
              ) : reading ? (
                <VersionView
                  pageID={page.id}
                  version={version}
                  history={history}
                  who={who}
                  now={now}
                  onClose={() => setVersion("")}
                />
              ) : (
                <PageBody detail={data} body={body} />
              )}
              {edit === "1" && !canEdit.can && (
                <p className="knowledge-note">You cannot edit it here: {canEdit.reason}</p>
              )}
              {reading ? (
                <PageChanges pageID={page.id} who={who} now={now} />
              ) : (
                <>
                  <PageComments
                    pageID={page.id}
                    title={page.title}
                    comments={data.comments ?? []}
                    seats={data.comment_seats}
                    who={who}
                    now={now}
                  />
                  <Connections
                    links={data.linked_from}
                    linksStatus={data.linked_from_status}
                    reads={reads.data}
                    readsError={reads.error}
                    under={data.children ?? []}
                    underTotal={data.children_total ?? 0}
                    who={who}
                    now={now}
                  />
                </>
              )}
            </article>
          </div>
        )}
      </QueryState>
    </>
  );
}
