/**
 * One page, as the approved Doclet Home draws it: the outline of what it
 * says to its left, following the scroll, and the page itself in one column,
 * its properties under its title, its comments under its body and what it is
 * connected to under those.
 *
 * # Addressed by its id
 *
 * `#/doclet/pages/{id}`, and by nothing else: a title changes on rename,
 * so every link anybody had copied broke the day somebody fixed a heading.
 * The trail above it (Doclet, its space, the pages above it, the page) is
 * the page's PLACE, published to the page bar from the page's own answer.
 *
 * # The page bar
 *
 * The eye ([WatchControl]), History, Edit and More. More holds the star,
 * which is why this page tells the frame to draw none of its own
 * ([useOwnStar]): one control per action on screen. History swaps the outline
 * for the revisions and the comments for the page's changes; Edit turns the
 * page itself into the editor, the bar into the editing state with Cancel and
 * Save, and writes through `save_page` as the person signed in, against the
 * revision they opened (`Editor.tsx`). For a reader who cannot write, every
 * control stays drawn and says why.
 *
 * # A draft says so, with the way out of it
 *
 * A page saved as a draft is listed in its space marked as one and found by
 * no search. The page says that above its body with Publish beside it, and
 * More holds the same press (and, on a published page, the way back to a
 * draft): `save_page` with a `status` and nothing else, against the version on
 * screen. Without it a draft stayed one for good, since the editor saved a
 * body and nothing more.
 *
 * # Tidying it
 *
 * More holds Rename, Labels, Move and Move to trash (`Tidy.tsx`), each the
 * engine's own route or `save_page`; a page in the trash says so above its
 * body with Restore, and Delete permanently for an admin.
 *
 * # What a wiki cannot tell you, and this can
 *
 * Its readers are seats and every read is a recorded act, so the page says
 * who read it and how (the turn it was read in, the search it came up for); a
 * tool-skill page says who it was loaded by; and "Linked from" is every page
 * and task pointing at it. All of it is the engine's (`page_reads`,
 * `skill_loaded_by`, `linked_from`); nothing here is counted in the browser.
 */

import { useMemo } from "react";
import {
  Button,
  Callout,
  DescriptionList,
  Document,
  DocumentTitle,
  Menu,
  Skeleton,
  preloadRichEditor,
  useToast,
} from "@crewlethq/ui";
import {
  ClockGlyph,
  CopyGlyph,
  EllipsisGlyph,
  EyeOffGlyph,
  FileTextGlyph,
  PencilGlyph,
  PlusGlyph,
  SendGlyph,
  StarGlyph,
} from "@crewlethq/icons/glyphs";
import { pageSpaceKey, pageUpKey } from "~/app/crumbs.ts";
import { href, useNavigator, useParam } from "~/app/router.tsx";
import { useOwnStar, usePageLabels, usePageMenu } from "~/app/Shell.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import type { PageMenuEntry } from "~/app/header/PageHeader.tsx";
import { QueryState } from "~/components/common.tsx";
import { RefusalNote, WriteButton } from "~/components/WriteButton.tsx";
import { useAct } from "~/lib/useAct.ts";
import { useNow } from "~/lib/clock.ts";
import { fmtDateTime, relTime } from "~/lib/format.ts";
import { renderMarkdown } from "~/lib/markdown.ts";
import { OnThisPage } from "~/components/OnThisPage.tsx";
import { copyPageLink } from "~/lib/pageLink.ts";
import { loadedBy } from "~/lib/pageReads.ts";
import { MaxStars, starredIn, useStarred, useToggleStar } from "~/lib/starred.ts";
import { useReader } from "~/lib/reader.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { menuHold, useWriteAccess } from "~/lib/useWriteAccess.ts";
import type { PageDetail } from "~/protocol/index.ts";
import {
  Face,
  DocletBar,
  newPagePath,
  pagePath,
  usePeople,
  useSpaces,
  type Who,
  NotInDoclet,
  PathSteps,
} from "../parts.tsx";
import { withoutRepeatedTitle } from "./body.ts";
import { PageCommentThread } from "./CommentThread.tsx";
import { Connections, Revisions } from "./Connections.tsx";
import { PageEditor } from "./Editor.tsx";
import { PageChanges, VersionView } from "./History.tsx";
import { IN_TRASH, TrashedNotice, usePageTidy } from "./Tidy.tsx";
import { WatchControl } from "./Watch.tsx";

/** How often a page reads itself again, and how often who read it. */
const PAGE_POLL_MS = 20_000;
const READS_POLL_MS = 60_000;

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
    detail.page.status === "draft" ? "Draft" : "",
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
  const author = page.author ?? "";
  const saver = last?.author ?? "";
  const kind = kindOf(detail, who);
  const revision = last
    ? `Revision ${last.version}: ${last.message ? `“${last.message}”` : "no note was kept with this save"}`
    : "";
  return (
    <DescriptionList dense className="knowledge-props">
      <DescriptionList.Item term="Space">
        <PathSteps>
          <a className="knowledge-props-link" href={href(["doclet", page.container])}>
            {spaceName}
          </a>
          {(detail.ancestors ?? []).map((a) => (
            <a key={a.id} className="knowledge-props-link" href={href(pagePath(a.id))}>
              {a.title}
            </a>
          ))}
        </PathSteps>
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
  // THE BODY IS THE PAGE under its title's h2 (the trail holds the h1), so its
  // first heading is an h3 whatever `#` it was written with.
  if (body.trim())
    return <Document>{renderMarkdown(body, { anchors: true, headingsFrom: 3 })}</Document>;
  const children = detail.children ?? [];
  if (!detail.folder) return <p className="knowledge-note">This page has no body.</p>;
  return (
    <div className="knowledge-folder">
      <p className="knowledge-note">
        {children.length === 0 ? "Nothing filed here yet." : "A folder that groups pages."}
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

export function PageView({ id }: { id: string }) {
  const who = usePeople();
  const now = useNow();
  const nav = useNavigator();
  const toast = useToast();
  const { data, loading, error, refusal } = useQuery(
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
  // STARS ARE KEPT PER READER (`lib/starred.ts`), so a tab that has not
  // learned who reads it has no list to keep one in, and the star is not
  // offered: a star kept in a list nobody owns would be offered to whoever
  // signs in next. The page header's star follows the same rule (`StarPage`).
  const reader = useReader();
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

  // A PAGE IN THE TRASH READS AND TAKES NO CHANGE: every control that would
  // change it is held with the one reason (`IN_TRASH`), before the reader's
  // own, and the engine refuses the write all the same.
  const hold = page?.status === "trashed" ? IN_TRASH : undefined;
  const editHold = hold ?? (canEdit.can ? undefined : canEdit.reason);
  const editing = edit === "1" && Boolean(page) && editHold === undefined;
  const reading = version > 0 && !editing;
  const body = useMemo(
    () => (page ? withoutRepeatedTitle(page.body ?? "", page.title) : ""),
    [page],
  );

  // A PAGE'S STATUS IS MOVED BY ONE PRESS, against the version on screen:
  // publish a draft, or take a published page back to one. The engine decides
  // who may (the page's creator, its space's lead, an admin), and its refusal
  // is said under the notice or in a toast, never swallowed.
  const setStatus = useAct("save_page");
  const draft = page?.status === "draft";
  const moveStatus = async (to: "published" | "draft") => {
    if (!page || setStatus.busy) return;
    const result = await setStatus.run(
      { page: page.id, base_version: page.version, status: to },
      {
        done: to === "published" ? `Published “${page.title}”` : `“${page.title}” is a draft again`,
      },
    );
    if (result?.kind === "refused" && to === "draft") {
      toast.show({
        variant: "warning",
        title: "The page is still published",
        message: [result.sentence, result.hint].filter(Boolean).join(" "),
      });
    }
  };

  const tidy = usePageTidy(page);

  const path = pagePath(id);
  const starred = starredIn(stars, path);
  const toggleHistory = () => {
    setEdit("");
    setVersion(version > 0 || history.length === 0 ? "" : String(history[0]!.version));
  };
  const menu: PageMenuEntry[] = page
    ? [
        ...(reader === null
          ? []
          : [
              {
                key: "star",
                label: starred ? "Remove the star" : "Star this page",
                icon: <StarGlyph filled={starred} size="sm" />,
                ...(starred ? {} : { description: "Pinned on Doclet and kept in the sidebar" }),
                onSelect: () => {
                  if (toggleStar({ path, label: page.title, workspace: "doclet" }) === "full") {
                    toast.show({ variant: "warning", title: `${MaxStars} is the limit` });
                  }
                },
              },
            ]),
        {
          key: "sub-page",
          label: "New sub-page",
          icon: <PlusGlyph size="sm" />,
          onSelect: () => {
            const to = newPagePath(page.container, page.id);
            nav.to(to.path, to.query);
          },
          ...(hold ? { disabled: true, description: hold } : menuHold(canWrite)),
        },
        ...(page.status === "draft"
          ? [
              {
                key: "publish",
                label: "Publish",
                icon: <SendGlyph size="sm" />,
                description: "Every reader and every search finds it",
                onSelect: () => void moveStatus("published"),
                ...menuHold(canEdit),
              },
            ]
          : page.status === "published"
            ? [
                {
                  key: "unpublish",
                  label: "Move back to draft",
                  icon: <EyeOffGlyph size="sm" />,
                  description: "Search leaves it out until it is published again",
                  onSelect: () => void moveStatus("draft"),
                  ...menuHold(canEdit),
                },
              ]
            : []),
        ...tidy.entries,
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
        <DocletBar />
        <NotInDoclet
          icon={<FileTextGlyph size="xl" />}
          title="There is no such page"
          description="It may have been trashed, or the link names a page that never existed."
        />
      </>
    );
  }

  return (
    <>
      {/* WHILE EDITING, THE BAR IS THE EDITOR'S: its state, Cancel and Save
          (`Editor.tsx`), drawn into the same slot. */}
      {!editing && (
        // THE PAGE'S OWN ACTIONS ALONE, no search: see `DocletBar`.
        <PageActions>
          {page && (
            <>
              <WatchControl page={page} hold={hold} />
              {/* ON A PHONE HISTORY AND MORE FOLD into the frame's menu, which
                holds the same entries (`usePageMenu`), and Edit stays. */}
              <span className="page-action-folds">
                <Button
                  size="small"
                  variant="ghost"
                  leadingIcon={<ClockGlyph size="sm" />}
                  disabledReason={
                    history.length === 0 ? "Nobody has saved over it yet." : undefined
                  }
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
                disabledReason={editHold ?? (editing ? "You are editing it." : undefined)}
                title={editHold}
                onPointerEnter={preloadRichEditor}
                onFocus={preloadRichEditor}
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
        </PageActions>
      )}

      {loading && !data && <Skeleton variant="text" rows={10} label="Loading the page" />}
      <QueryState error={error} refusal={refusal} loading={loading}>
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
              {/* AN h2, as every object's title is in the frame: the trail's
                  last crumb is the page's h1 and already names it, and a
                  second h1 gave a screen reader two titles. */}
              <DocumentTitle as="h2">{page.title}</DocumentTitle>
              <PageProps detail={data} spaceName={spaceName} who={who} now={now} />
              {page.status === "trashed" && !editing && <TrashedNotice page={page} />}
              {draft && !editing && !reading && (
                <Callout
                  variant="neutral"
                  title="This page is a draft"
                  action={
                    <WriteButton
                      write={setStatus}
                      size="small"
                      variant="primary"
                      showRefusal={false}
                      leadingIcon={<SendGlyph size="sm" />}
                      onPress={() => void moveStatus("published")}
                    >
                      Publish
                    </WriteButton>
                  }
                >
                  Search leaves it out until it is published.
                  <RefusalNote write={setStatus} />
                </Callout>
              )}
              {editing ? (
                <PageEditor
                  key={page.id}
                  detail={data}
                  reading={<PageBody detail={data} body={body} />}
                  onDone={() => setEdit("")}
                />
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
              {edit === "1" && editHold !== undefined && (
                <p className="knowledge-note">You cannot edit it here: {editHold}</p>
              )}
              {editing ? null : reading ? (
                <PageChanges pageID={page.id} who={who} now={now} />
              ) : (
                <>
                  <PageCommentThread
                    pageID={page.id}
                    title={page.title}
                    comments={data.comments ?? []}
                    now={now}
                    hold={hold}
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
      {tidy.dialog}
    </>
  );
}
