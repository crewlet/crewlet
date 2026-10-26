/**
 * Writing a new page: a composer with an address, rather than a dialog over
 * whatever was open (`#/doclet/new?space=…&parent=…`).
 *
 * # Filed where it was asked for, and said
 *
 * A space's Create row, a page's "New sub-page" and the bar's "New page" each
 * open it filed where they were pressed, and the first row says where that is
 * and lets the writer change it: the space, then the page it goes under or
 * the space's top. A title is the page's ADDRESS inside its space, so a title
 * already taken there is said under the field as it is typed, beside a way
 * to open the page that has it; the engine's own refusal (`exists`) stays the
 * last word.
 *
 * # Search before you write
 *
 * "Similar pages" is the knowledge search run on the title as it settles,
 * the same search an agent runs, so the page that already answers this is
 * found before a second one is written.
 *
 * # Save as draft, or Publish
 *
 * Both are `write_page`, as the person signed in (ADR-0024), with `status`
 * set: a draft is listed in its space and marked Draft, and found by no
 * search until it is published. On success the reader is taken to the page
 * by the id the engine answered with, never by its title.
 *
 * # Nothing typed is lost to a stray click
 *
 * While anything is written, every move away asks first (`useLeaveGuard`),
 * as Discard does.
 */

import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import {
  Button,
  ConfirmModal,
  DocumentTitleField,
  EmptyState,
  FormField,
  RichEditor,
  SegmentedControl,
  Select,
  TagsInput,
  markdownWords,
} from "@crewlethq/ui";
import { BookOpenGlyph, FileTextGlyph, FolderGlyph } from "@crewlethq/icons/glyphs";
import { href, useLeaveGuard, useNavigator, useParam } from "~/app/router.tsx";
import { RefusalNote, WriteButton, pressable } from "~/components/WriteButton.tsx";
import {
  PAGE_BODY_MAX_BYTES,
  PAGE_LABELS_MAX,
  PAGE_LABEL_MAX_BYTES,
  PAGE_TITLE_MAX_BYTES,
} from "~/contract/pages.ts";
import { fmtBytes, fmtDateCompact, plural, textBudget, utf8Bytes } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { shownImage } from "~/lib/markdown.ts";
import { resolveSearchMode } from "~/lib/search.ts";
import { useAct } from "~/lib/useAct.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useQuiet } from "~/lib/useQuiet.ts";
import { useSearchQuery } from "~/lib/useSearchQuery.ts";
import type { PageSummary, PageStatus } from "~/protocol/index.ts";
import { useAssistantDraft } from "~/app/assistant/draft.ts";
import { composerDraft } from "./composerDraft.ts";
import { linkPageCommand } from "./page/LinkPicker.tsx";
import { DocletBar, PageGlyph, pagePath, useSpaces } from "./parts.tsx";
import { PAGES_WINDOW } from "./usePagedPages.ts";

/** The composer's own entry in the slash menu: a new page links to any page. */
const COMMANDS = [linkPageCommand()];

/** What a page can start from: a blank page, or the outline of a page the company writes often. */
export const TEMPLATES = [
  { value: "blank", label: "Blank", body: "" },
  {
    value: "design",
    label: "Design for a work item",
    body: "Spec: link the work item and the pages this design builds on.\n\n## Module layout\n- \n\n## Data model\n\n## Error handling\n",
  },
  {
    value: "runbook",
    label: "Runbook",
    body: "When to use this runbook, in one sentence.\n\n## Steps\n1. \n2. \n3. \n\n## If something fails\n",
  },
  {
    value: "decision",
    label: "Decision record",
    body: "## Context\n\n## Options\n1. \n2. \n\n## Decision\n\n## Who to tell\n",
  },
] as const;

type Template = (typeof TEMPLATES)[number]["value"];

/** How many pages that already cover the title the side lists. */
const SIMILAR_MAX = 3;

/** The pages of a space as places to file under, each with the path above it. */
function placesIn(pages: readonly PageSummary[]): { page: PageSummary; path: string }[] {
  const byID = new Map(pages.map((p) => [p.id, p]));
  const pathOf = (page: PageSummary): string => {
    const steps: string[] = [];
    const seen = new Set<string>();
    let at = page.parent_id ? byID.get(page.parent_id) : undefined;
    while (at && !seen.has(at.id)) {
      seen.add(at.id);
      steps.unshift(at.title);
      at = at.parent_id ? byID.get(at.parent_id) : undefined;
    }
    return steps.join(" / ");
  };
  return pages.map((page) => ({ page, path: pathOf(page) }));
}

export function Composer() {
  const nav = useNavigator();
  const now = useNow();
  const [spaceParam] = useParam("space", "");
  const [parentParam] = useParam("parent", "");
  const { spaces, read } = useSpaces();
  const write = useAct("write_page");

  const [space, setSpace] = useState(spaceParam);
  const [parent, setParent] = useState(parentParam);
  // A NEW ADDRESS IS A NEW PLACE: "New sub-page" pressed while the composer is
  // already open files the page where it was pressed.
  useEffect(() => {
    setSpace(spaceParam);
    setParent(parentParam);
  }, [spaceParam, parentParam]);
  const [template, setTemplate] = useState<Template>("blank");
  const [title, setTitle] = useState("");
  const [body, setBody] = useState("");
  const [labels, setLabels] = useState<string[]>([]);
  const [templateNote, setTemplateNote] = useState("");
  // WHERE A HELD MOVE WAS GOING, while Discard is asked about.
  const [leaving, setLeaving] = useState<(() => void) | null>(null);
  // LET GO, read by the guard as it is asked rather than on the next render:
  // the move a Discard or a Publish makes is asked of the guard in the same
  // press that lets it go.
  const released = useRef(false);

  const current = spaces.find((s) => s.key === space);
  const spaceName = current?.name || space;
  const name = title.trim();

  // THE PLACES TO FILE UNDER: the space's pages in one window, with the path
  // above each. A page named in the address that the window does not hold is
  // still offered, read on its own.
  const places = useQuery(
    "pages",
    { container: space, status: "published,draft", skills: false, limit: PAGES_WINDOW },
    { enabled: space !== "" },
  );
  const named = useQuery("page", { id: parent }, { enabled: parent !== "" });
  const filed = useMemo(() => placesIn(places.data?.pages ?? []), [places.data]);

  // IS THE TITLE TAKEN: asked once it rests, and only an exact match (in any
  // case) counts, because a title is the page's address in its space.
  const quietTitle = useQuiet(name);
  const sameTitle = useQuery(
    "pages",
    { container: space, title: quietTitle, limit: 10 },
    { enabled: space !== "" && quietTitle !== "" },
  );
  const taken = (sameTitle.data?.pages ?? []).find(
    (p) => p.title.toLowerCase() === name.toLowerCase(),
  );

  // SIMILAR PAGES: the knowledge search on the title, in the mode the engine
  // serves by default.
  const probe = useSearchQuery("knowledge", { q: "", mode: "semantic" });
  const similar = useSearchQuery(
    "knowledge",
    { q: quietTitle, mode: resolveSearchMode(null, probe.data) },
    { enabled: quietTitle.length >= 3 && probe.data !== null },
  );

  const titleBudget = textBudget(title, PAGE_TITLE_MAX_BYTES, "a page's title");
  const bodyBytes = utf8Bytes(body);
  const longLabel = labels.find((l) => utf8Bytes(l) > PAGE_LABEL_MAX_BYTES);
  // WHAT HOLDS SAVE AND PUBLISH, in the order a writer meets it: one answer
  // for both buttons and the shortcut.
  const blocked: string | undefined = !space
    ? "Choose the space to file it in."
    : !name
      ? "Give the page a title."
      : titleBudget.over
        ? titleBudget.line
        : taken
          ? `“${taken.title}” already exists in ${spaceName}.`
          : !body.trim()
            ? "Write the page first."
            : bodyBytes > PAGE_BODY_MAX_BYTES
              ? `The page is ${fmtBytes(bodyBytes)}, past its ${fmtBytes(PAGE_BODY_MAX_BYTES)} limit.`
              : labels.length > PAGE_LABELS_MAX
                ? `A page carries at most ${PAGE_LABELS_MAX} labels.`
                : longLabel
                  ? `The label “${longLabel}” is over ${PAGE_LABEL_MAX_BYTES} bytes. Shorten it.`
                  : undefined;

  const dirty = name !== "" || body.trim() !== "" || labels.length > 0;
  useLeaveGuard(
    dirty
      ? (_to, leave) => {
          if (released.current) return false;
          setLeaving(() => leave);
          return true;
        }
      : null,
  );

  // WHAT IS OPEN, for the assistant: the page as written so far, its fields
  // set through the composer's own setters and refused in its own words;
  // Save as draft and Publish stay the person's.
  const parentTitle =
    filed.find(({ page }) => page.id === parent)?.page.title ?? named.data?.page?.title ?? "";
  useAssistantDraft(
    composerDraft({
      value: { space, parent, title, body, labels },
      parentTitle,
      spaces,
      busy: write.busy,
      apply: (next) => {
        setSpace(next.space);
        setParent(next.parent);
        setTitle(next.title);
        setBody(next.body);
        setLabels([...next.labels]);
      },
    }),
  );

  const back = () => {
    if (parent) nav.to(pagePath(parent));
    else nav.to(space ? ["doclet", space] : ["doclet"]);
  };

  const press = async (status: PageStatus) => {
    const result = await write.run(
      {
        title: name,
        body,
        container: space,
        ...(parent ? { parent } : {}),
        ...(labels.length > 0 ? { labels } : {}),
        status,
      },
      {
        done:
          status === "draft"
            ? `Saved “${name}” as a draft in ${spaceName}.`
            : `Published “${name}” in ${spaceName}.`,
      },
    );
    if (!result || (result.kind !== "applied" && result.kind !== "pending")) return;
    const id = (result.receipt as { id?: unknown } | null)?.id;
    released.current = true;
    if (typeof id === "string" && id) nav.to(pagePath(id));
    else nav.to(["doclet", space]);
  };

  const startFrom = (next: Template) => {
    const was = TEMPLATES.find((t) => t.value === template);
    // A TEMPLATE NEVER REPLACES WRITING: only an empty page, or one still
    // holding the template it started from, takes another.
    if (body.trim() && body !== was?.body) {
      setTemplateNote("Clear the page first to start from a template.");
      return;
    }
    setTemplateNote("");
    setTemplate(next);
    setBody(TEMPLATES.find((t) => t.value === next)?.body ?? "");
  };

  if (read && spaces.length === 0) {
    return (
      <>
        <DocletBar />
        <EmptyState
          icon={<BookOpenGlyph size="xl" />}
          title="There is no space to file a page in yet"
          description="Give a team a space in Edit org."
        />
      </>
    );
  }

  const spaceOptions = spaces.map((s) => ({
    value: s.key,
    label: s.name || s.key,
    leading: <FolderGlyph size="sm" />,
    hint: s.key,
  }));
  const parentOptions = [
    {
      value: "",
      label: space ? `Top of ${spaceName}` : "Top of the space",
      leading: <FolderGlyph size="sm" />,
    },
    ...filed.map(({ page, path }) => ({
      value: page.id,
      label: page.title,
      text: `${path} ${page.title}`,
      leading: <PageGlyph folder={page.folder} />,
      ...(path ? { description: `in ${path}` } : {}),
    })),
    ...(parent && named.data && !filed.some(({ page }) => page.id === parent)
      ? [
          {
            value: parent,
            label: named.data.page.title,
            leading: <PageGlyph folder={named.data.folder} />,
          },
        ]
      : []),
  ];

  const save = (status: PageStatus, label: ReactNode, variant: "primary" | "secondary") => (
    <WriteButton
      write={write}
      size="small"
      variant={variant}
      showRefusal={false}
      blocked={blocked}
      onPress={() => void press(status)}
    >
      {label}
    </WriteButton>
  );

  return (
    <>
      <DocletBar>
        <Button
          size="small"
          variant="ghost"
          onClick={() => (dirty ? setLeaving(() => back) : back())}
          disabled={write.busy}
        >
          Discard
        </Button>
        {save("draft", "Save as draft", "secondary")}
        {save("published", "Publish", "primary")}
      </DocletBar>

      <div className="knowledge-composer">
        <div className="knowledge-composer-main">
          <div className="knowledge-composer-row" role="group" aria-label="File it in">
            <span className="knowledge-composer-label" aria-hidden="true">
              File it in
            </span>
            <Select
              size="sm"
              width="auto"
              ariaLabel="The space to file it in"
              placeholder="Choose a space"
              value={space || undefined}
              options={spaceOptions}
              onChange={(value) => {
                setSpace(String(value));
                setParent("");
              }}
            />
            <span aria-hidden="true">/</span>
            <Select
              size="sm"
              width="auto"
              searchable
              searchPlaceholder="Words from a title"
              ariaLabel="The page it goes under"
              disabled={!space}
              value={parent}
              options={parentOptions}
              onChange={(value) => setParent(String(value))}
            />
          </div>

          <div className="knowledge-composer-row">
            <span className="knowledge-composer-label" aria-hidden="true">
              Start from
            </span>
            <SegmentedControl
              semantics="radio"
              activate="manual"
              size="sm"
              label="Start from"
              value={template}
              onValueChange={startFrom}
              options={TEMPLATES.map((t) => ({ value: t.value, label: t.label }))}
            />
            {templateNote && (
              <span className="knowledge-note" role="status">
                {templateNote}
              </span>
            )}
          </div>

          {/* THE TITLE IS SET AS THE PAGE WILL WEAR IT: the page title's own
              type over the body, where a form's labelled box stood. Its link
              address note shows while the title is typed. */}
          <FormField
            htmlFor="knowledge-composer-title"
            helperOnFocus
            error={
              titleBudget.over ? (
                titleBudget.line
              ) : taken ? (
                <>
                  A page called &ldquo;{taken.title}&rdquo; already exists in {spaceName}.{" "}
                  <a className="prose-link" href={href(pagePath(taken.id))}>
                    Open it
                  </a>
                </>
              ) : undefined
            }
            helper={
              titleBudget.over || taken ? undefined : (titleBudget.line ?? "Also its link address.")
            }
          >
            {(field) => (
              <DocumentTitleField
                id={field.id}
                aria-label="Title"
                aria-describedby={field.describedBy}
                placeholder="Untitled"
                autoComplete="off"
                value={title}
                error={titleBudget.over || Boolean(taken)}
                onChange={(event) => setTitle(event.target.value)}
              />
            )}
          </FormField>

          <RichEditor
            value={body}
            onChange={setBody}
            resolveImage={shownImage}
            label="The page"
            placeholder="Write the page, or type a slash for blocks"
            commands={COMMANDS}
            onSubmit={() => {
              if (pressable(write, blocked)) void press("published");
            }}
          />
          <p
            className={
              bodyBytes > PAGE_BODY_MAX_BYTES
                ? "knowledge-composer-count is-over"
                : "knowledge-composer-count"
            }
          >
            <span>{plural(markdownWords(body), "word")}</span>
            <span>
              {fmtBytes(bodyBytes)} of {fmtBytes(PAGE_BODY_MAX_BYTES)}
            </span>
          </p>
          <RefusalNote write={write} />
        </div>

        <aside className="knowledge-composer-side" aria-label="About the new page">
          <section className="knowledge-composer-section" aria-labelledby="knowledge-similar">
            <h2 className="knowledge-side-title" id="knowledge-similar">
              Similar pages
            </h2>
            {quietTitle.length < 3 ? (
              <p className="knowledge-note">Similar pages appear as you type.</p>
            ) : similar.data && similar.data.hits.length === 0 ? (
              <p className="knowledge-note">Nothing similar yet.</p>
            ) : similar.data ? (
              <>
                <ul className="knowledge-links" role="list">
                  {similar.data.hits.slice(0, SIMILAR_MAX).map((hit) => (
                    <li key={hit.id}>
                      <a
                        className="knowledge-link"
                        href={hit.url || href(pagePath(hit.id))}
                        {...(hit.url ? { target: "_blank", rel: "noreferrer" } : {})}
                      >
                        <FileTextGlyph size="sm" />
                        <span className="knowledge-link-title">{hit.title}</span>
                        {hit.updated_at && (
                          <span className="knowledge-note">
                            {fmtDateCompact(hit.updated_at, now)}
                          </span>
                        )}
                      </a>
                    </li>
                  ))}
                </ul>
                <p className="knowledge-note">Extend one of these instead of starting over.</p>
              </>
            ) : (
              <p className="knowledge-note">Searching&hellip;</p>
            )}
          </section>
          <section className="knowledge-composer-section" aria-labelledby="knowledge-labels">
            <h2 className="knowledge-side-title" id="knowledge-labels">
              Labels
            </h2>
            <TagsInput
              value={labels}
              onChange={setLabels}
              allowCustom
              dedupe
              aria-labelledby="knowledge-labels"
              placeholder={labels.length ? "Add another" : "Add a label, then Enter"}
            />
          </section>
        </aside>
      </div>

      <ConfirmModal
        open={leaving !== null}
        destructive
        title="Discard this page?"
        message="Its title and what you wrote are not saved anywhere yet."
        cancelLabel="Keep editing"
        confirmLabel="Discard"
        onClose={() => setLeaving(null)}
        onConfirm={() => {
          const go = leaving;
          released.current = true;
          setLeaving(null);
          go?.();
        }}
      />
    </>
  );
}
