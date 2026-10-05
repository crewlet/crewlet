/**
 * A phase's prompt, as a document a reader can find their way around.
 *
 * WHAT WAS WRONG, TWICE. Both halves of a phase's prompt were one `CodeBlock`
 * each, and a seat's system prompt runs to tens of kilobytes: identity,
 * mission, policies, the roster, the turn contract, the sandbox contract,
 * whatever the turn prefetched, the workers, the skills and the tool
 * catalogue. An operator asking the question these screens exist for ("what
 * was the reviewer actually told about self-iterating?") scrolled a 30 kB block
 * looking for a heading. The first answer was a stack of closed folds, one per
 * `##` — and that was hard to follow too: a column of look-alike rows titled
 * with raw inline markdown (backticks and all), sized in UTF-16 "chars" that
 * left the heading lines out and so never added up, bodies whose single
 * newlines collapsed into run-on paragraphs, and an outline cut wherever a `##`
 * appeared, including inside the content a prompt QUOTES.
 *
 * WHAT IT IS NOW: a map, an outline and one section.
 *
 *  - THE MAP is one slim bar for the whole request — the system prompt's
 *    sections, a gap, the user message's — each as wide as its share of the
 *    bytes, so where the weight went is a glance rather than a sum. It is a
 *    PICTURE of the outline, so it is `aria-hidden`; the outline is the
 *    keyboard's way to the same sections.
 *  - THE OUTLINE lists both halves under their names and sizes, one row per
 *    top-level section with its title rendered, its size and a weight bar.
 *    One tab stop; the arrows, Home and End move through it and the reader
 *    follows. Beside the reader where the card is wide enough (a container
 *    query on the prompt view itself, never the window — the sidebar and a
 *    peek take width the window does not report); a picker above it where
 *    not.
 *  - THE READER shows the ONE selected section: its title (a styled line —
 *    a quoted prompt's headings are not this page's), its size and share,
 *    Previous and Next naming where they go, then the body, with the headings
 *    inside the section drawn in place under it.
 *  - FIND annotates rather than filters: every row says how many matches it
 *    holds, a row that has some lifts and a row with none recedes. Typing
 *    takes the reader to a section that matches when the one shown has none,
 *    until the reader chooses a section themselves; Enter and Shift+Enter
 *    jump between the sections that match and bring the first match into
 *    view; and the matches in the reader are marked where the browser can
 *    mark text without touching the DOM (the CSS Custom Highlight API).
 *    Counted on the SOURCE — what the model was told — not on the rendering,
 *    and marked only where the source is: the title when it is the section's
 *    own heading line, never the reader's own words.
 *
 * WHERE THE OUTLINE COMES FROM. The builder's own section map when the record
 * carries one that tiles its prompt, and the prompt's headings otherwise
 * (`lib/promptmap.ts` holds the rule and why). Nothing in this file names a
 * section, which is what keeps it right when a prompt grows one — a list kept
 * here would render a stale outline with no symptom but a section nobody
 * could find.
 *
 * TWO VIEWS, AND EACH ONE DOES ITS WHOLE JOB. `Rendered` is for reading.
 * `Source` is the record: each half whole, one block, byte for byte, one
 * selection — what an operator reproduces a turn from and what they diff when
 * a model starts behaving differently. It is offered on every document,
 * headings or none, because one decodes the markdown and one is the bytes.
 *
 * WHAT IS RECORD-SENSITIVE SURVIVES RENDERING. A fenced block comes out as its
 * own `pre` holding the exact text, so a tool schema, a JSON example and a
 * contract template are byte-identical in both views; what the reading view
 * spends is emphasis markers and list bullets. Its line breaks are the
 * prompt's own — every newline a builder wrote is a break, because a prompt is
 * written line by line for a model and an identity block of one fact per line
 * read as soft wraps became one run-on sentence.
 *
 * NO INNER SCROLLER AND NO STICKY BAND. The card that holds this hides its
 * overflow, and a box that scrolls inside the page is a second thing under the
 * wheel (see "The document does not scroll" in the design doc); one section at
 * a time is what keeps the page short instead.
 */

import {
  useEffect,
  useId,
  useMemo,
  useRef,
  useState,
  type KeyboardEvent,
  type RefObject,
} from "react";
import { Button, CodeBlock, Input, Select, cx } from "@crewlethq/ui";
import { ChevronLeftGlyph, ChevronRightGlyph, SearchGlyph } from "@crewlethq/icons/glyphs";
import { Segmented } from "~/ui/primitives.tsx";
import { MODEL_WORDS, plainText, renderInline, renderMarkdown } from "~/lib/markdown.ts";
import type { SectionNode } from "~/lib/markdown.ts";
import {
  findCounts,
  findPattern,
  neighbours,
  outlineHalf,
  sectionsKey,
  sectionsOf,
  share,
  stepToMatch,
  type Half,
  type OutlineHalf,
  type OutlineSection,
} from "~/lib/promptmap.ts";
import { fmtBytes, plural } from "~/lib/format.ts";
import type { PromptSection } from "~/protocol/index.ts";

type View = "read" | "source";

/**
 * What each half is called: a group's label, a picker's group, a share's "of
 * the …", and the name of a half that has no headings at all.
 */
const HALF: Record<Half, { name: string; of: string; whole: string }> = {
  system: { name: "System prompt", of: "the system prompt", whole: "The whole system prompt" },
  user: { name: "User message", of: "the user message", whole: "The whole user message" },
};

/**
 * The words for a section with no title: a lead run the headings gave, which
 * has neither a heading nor a builder to name it. Said, rather than left
 * blank, because a row with no words is a row nobody can choose by ear.
 *
 * WHICH WORDS DEPEND ON THE HALF. Before a heading, it is the run "before the
 * first heading". In a half with no heading at all — an older engine's
 * record, an onboarding message, a worker's task prose — it is the whole half,
 * and "before the first heading" claimed a heading the prompt does not have.
 * A map always names its spans, so neither label appears on a mapped prompt.
 */
function untitled(s: OutlineSection, halves: readonly OutlineHalf[]): string {
  const half = halves.find((h) => h.half === s.half);
  return half && half.sections.length > 1 ? "Before the first heading" : HALF[s.half].whole;
}

/** A section's title as the plain words it renders to — for a label, a tooltip, a button. */
function plainTitle(s: OutlineSection, halves: readonly OutlineHalf[]): string {
  return s.title ? plainText(s.title) : untitled(s, halves);
}

/**
 * A section's title as the inline markdown it is, or the untitled lead's words.
 *
 * `links: "text"` where the title is drawn INSIDE a control — an outline row,
 * a picker's option — whose children are presentational to assistive tech and
 * whose click a link would take for a navigation: a quoted heading can carry
 * one. The reader's own title keeps its links; nothing there is a control.
 */
function Title({
  section,
  halves,
  links = "anchor",
}: {
  section: OutlineSection;
  halves: readonly OutlineHalf[];
  links?: "anchor" | "text";
}) {
  return section.title ? (
    <>{renderInline(section.title, `t-${section.id}`, { links })}</>
  ) : (
    <span className="muted">{untitled(section, halves)}</span>
  );
}

/**
 * The system prompt and the user message of one phase.
 *
 * Both are read under ONE view switch rather than one each: they are two
 * halves of a single request, and a reader who wants the bytes wants them for
 * the prompt, not for the system half of it.
 */
export function PromptRecord({
  phase,
  system,
  user,
  systemSections,
  userSections,
}: {
  phase: string;
  system: string;
  user: string;
  /** The builders' section maps, when the record carries them — see `lib/promptmap.ts`. */
  systemSections?: PromptSection[] | null;
  userSections?: PromptSection[] | null;
}) {
  // KEYED ON THE MAPS' VALUES, not on the arrays: a record is rebuilt from the
  // wire on every push — a streaming phase's five frames a second, and every
  // seat's push on the turn page — so the maps are new arrays each time while
  // what they say is not, and keyed on identity this memo never hit: both
  // halves were re-sliced and re-split, and the find re-counted, on every
  // frame of an open Prompt fold.
  const systemKey = sectionsKey(systemSections);
  const userKey = sectionsKey(userSections);
  const halves = useMemo<OutlineHalf[]>(
    () => {
      const out: OutlineHalf[] = [];
      if (system !== "") out.push(outlineHalf("system", system, systemSections));
      if (user !== "") out.push(outlineHalf("user", user, userSections));
      return out;
    },
    // The keys ARE the maps' values; the arrays are what they were read from.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [system, user, systemKey, userKey],
  );
  // READING IS THE DEFAULT, because the question this fold is opened with is
  // what the phase was told, and the bytes are one click away. The other way
  // round, every reader pays the decoding on every open to serve the rarer
  // reproduce-and-diff.
  const [view, setView] = useState<View>("read");

  return (
    <div className="prompt-doc col gap-3">
      <div className="row gap-2 wrap">
        <Segmented<View>
          size="sm"
          ariaLabel="How to show the prompt"
          value={view}
          onChange={setView}
          options={[
            { value: "read", label: "Rendered", title: "The prompt as the markdown it is" },
            { value: "source", label: "Source", title: "The record, exactly as it was sent" },
          ]}
        />
        <span className="t-caption muted">
          {view === "read"
            ? "one section at a time, from its outline"
            : "the record as the model received it, byte for byte"}
        </span>
      </div>
      {view === "read" ? (
        <Reading phase={phase} halves={halves} />
      ) : (
        halves.map((h) => (
          <div className="col gap-1" key={h.half}>
            <div className="t-label">
              {HALF[h.half].name}
              <span className="muted">
                {" · "}
                {fmtBytes(h.bytes)}
              </span>
            </div>
            {/* THE TALLEST BLOCK ON THE PAGE by a wide margin, and `selectable`
                is how uilet makes it reachable: the tab stop, the accessible
                name and ⌘A tied into one flag. It is the record this view is
                about, which is exactly what that flag is for. */}
            <CodeBlock
              plain
              selectable
              label={`The ${phase} phase's ${HALF[h.half].name.toLowerCase()}`}
              code={h.text}
            />
          </div>
        ))
      )}
    </div>
  );
}

/** The reading view: the map, find, and the outline beside the one section it selects. */
function Reading({ phase, halves }: { phase: string; halves: OutlineHalf[] }) {
  const all = useMemo(() => sectionsOf(halves), [halves]);
  // The first section of the request is where reading starts: the lead run
  // when there is one, which is the first thing the model read.
  const [picked, setPicked] = useState("");
  const current = all.find((s) => s.id === picked) ?? all[0];
  const [query, setQuery] = useState("");
  const counts = useMemo(() => findCounts(all, query), [all, query]);
  const finding = findPattern(query) !== null;
  const matched = [...counts.values()].reduce((n, c) => n + c, 0);
  // Whether the reader chose a section by hand during THIS query — from the
  // first character typed until the box is cleared. See `onQuery`.
  const [held, setHeld] = useState(false);
  // Bumped by every Enter: the reader asked to be taken to a match, so the
  // marks bring the first one into view (`useFindMarks`).
  const [reveal, setReveal] = useState(0);
  const reader = useRef<HTMLElement | null>(null);
  useFindMarks(reader, query, current?.id ?? "", reveal);
  const ids = useId();

  if (!current) return null;
  const half = halves.find((h) => h.half === current.half)!;
  const { prev, next } = neighbours(all, current.id);
  const titleId = `${ids}-title`;

  /** A section the READER chose — the outline, the picker, the map, Previous and Next. */
  const choose = (id: string) => {
    setPicked(id);
    if (finding) setHeld(true);
  };

  /**
   * THE SELECTION FOLLOWS THE FIND until the reader takes it. Typing a query
   * that the section on screen does not contain moves the reader to the next
   * section that does — from where they are, wrapping, which is the first one
   * when they have not moved, and what a browser's own find does — so the
   * first thing a find shows is a match rather than a count and a section
   * with none in it. Only while the reader has not chosen a section by hand
   * during this query: once they have, they are reading something on purpose,
   * and the next keystroke must not take it away. Enter is the find's own
   * step, so it does not count as a choice; clearing the query ends it.
   */
  const onQuery = (value: string) => {
    setQuery(value);
    if (findPattern(value) === null) {
      setHeld(false);
      return;
    }
    if (held) return;
    const ahead = findCounts(all, value);
    if ((ahead.get(current.id) ?? 0) > 0) return;
    const to = stepToMatch(all, ahead, current.id, 1);
    if (to) setPicked(to.id);
  };

  /**
   * Where Previous and Next say they go: the section, and its half when it is
   * the other one — unless the section IS that whole half, whose name says so.
   */
  const target = (s: OutlineSection) => {
    const name = plainTitle(s, halves);
    return s.half === current.half || name === HALF[s.half].whole
      ? name
      : `${name} (${HALF[s.half].of})`;
  };

  const onFindKey = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key === "Enter") {
      e.preventDefault();
      const to = stepToMatch(all, counts, current.id, e.shiftKey ? -1 : 1);
      if (to) {
        setPicked(to.id);
        setReveal((n) => n + 1);
      }
    } else if (e.key === "Escape" && query !== "") {
      // Only while there is something to clear: an Escape on an empty box is
      // the surface's own, and goes on to it.
      e.preventDefault();
      e.stopPropagation();
      onQuery("");
    }
  };

  return (
    <div className="prompt-reading">
      <PromptMap
        halves={halves}
        current={current.id}
        counts={counts}
        finding={finding}
        onPick={choose}
      />
      <div className="prompt-find">
        <Input
          type="search"
          inputSize="sm"
          width="md"
          aria-label="Find in this prompt"
          placeholder="Find in this prompt"
          leading={<SearchGlyph size="xs" />}
          value={query}
          onChange={(e) => onQuery(e.target.value)}
          onKeyDown={onFindKey}
        />
        {/* POLITE: a count that changes per keystroke must not interrupt the
            typing it describes. Empty while there is no query, so a reader
            opening the prompt hears nothing about a find they did not ask
            for. */}
        <span className="t-caption muted" role="status">
          {finding
            ? matched === 0
              ? "No matches"
              : `${plural(matched, "match", "matches")} in ${plural(counts.size, "section")}`
            : ""}
        </span>
      </div>
      <div className="prompt-layout">
        <Outline
          halves={halves}
          current={current.id}
          counts={counts}
          finding={finding}
          onPick={choose}
          label={`Sections of the ${phase} phase's prompt`}
        />
        {/* THE NARROW FORM OF THE OUTLINE: one control naming the section the
            reader is on, which lists the rest. The design system's listbox, so
            it takes the theme and its keyboard; grouped by half like the
            outline it stands in for. */}
        <div className="prompt-picker">
          <Select
            ariaLabel="Section"
            value={current.id}
            onChange={(v) => choose(String(v))}
            options={all.map((s) => ({
              value: s.id,
              label: <Title section={s} halves={halves} links="text" />,
              text: plainTitle(s, halves),
              description: `${fmtBytes(s.bytes)}${
                finding && counts.get(s.id)
                  ? ` · ${plural(counts.get(s.id)!, "match", "matches")}`
                  : ""
              }`,
              group: `${HALF[s.half].name} · ${fmtBytes(halves.find((h) => h.half === s.half)!.bytes)}`,
            }))}
          />
        </div>
        {/* WHAT A FIND MARKS IS WHAT IT COUNTED: the section's source, so its
            title only when the title is the section's own heading line (a
            builder's "Task" or the untitled lead's words are not in the
            source, and a mark there would be one the count never made), and
            never this view's own words — the size, the share, where Previous
            and Next go. `data-find-skip` is how a subtree says so. */}
        <section className="prompt-reader" aria-labelledby={titleId} ref={reader}>
          {/* A STYLED LINE, NEVER AN h-ELEMENT. A prompt's headings are not this
              page's headings: a turn with six phases would put eighty of them
              from six quoted documents into one screen's outline. The region
              takes its name from it instead. */}
          <div className="prompt-reader-head">
            <div
              id={titleId}
              className="prompt-reader-title"
              data-find-skip={current.headed ? undefined : ""}
            >
              <Title section={current} halves={halves} />
            </div>
            <div className="t-caption muted t-num" data-find-skip="">
              {fmtBytes(current.bytes)} · {share(current.bytes, half.bytes)} of{" "}
              {HALF[current.half].of}
              {finding && counts.get(current.id)
                ? ` · ${plural(counts.get(current.id)!, "match", "matches")}`
                : ""}
            </div>
          </div>
          {/* THEY SAY WHERE THEY GO, and they stay: at either end the control
              says why it cannot move rather than vanishing — a button that
              disappears under the press takes the reader's focus with it. One
              column each, so on a phone a long destination wraps inside its
              own half instead of pushing Next onto a line under Previous. */}
          <div className="prompt-reader-nav" data-find-skip="">
            <Button
              variant="ghost"
              size="small"
              className="prompt-step"
              leadingIcon={<ChevronLeftGlyph size="xs" />}
              disabledReason={prev ? undefined : "This is the first section"}
              onClick={() => prev && choose(prev.id)}
            >
              {prev ? `Previous: ${target(prev)}` : "Previous"}
            </Button>
            <Button
              variant="ghost"
              size="small"
              className="prompt-step is-next"
              trailingIcon={<ChevronRightGlyph size="xs" />}
              disabledReason={next ? undefined : "This is the last section"}
              onClick={() => next && choose(next.id)}
            >
              {next ? `Next: ${target(next)}` : "Next"}
            </Button>
          </div>
          <SectionBody key={current.id} body={current.body} nested={current.children} />
        </section>
      </div>
    </div>
  );
}

/**
 * The whole request as one bar, each section as wide as its bytes.
 *
 * NEUTRAL FILLS, in two alternating steps so neighbours stay apart; the accent
 * marks only the section being read, which is the one thing the accent means
 * in this product. Hidden from assistive tech because it adds nothing the
 * outline does not say in words, and its segments take no focus: the outline
 * is the keyboard's path to every one of them.
 */
function PromptMap({
  halves,
  current,
  counts,
  finding,
  onPick,
}: {
  halves: OutlineHalf[];
  current: string;
  counts: ReadonlyMap<string, number>;
  finding: boolean;
  onPick: (id: string) => void;
}) {
  return (
    <div className="prompt-map" aria-hidden="true">
      {halves.map((h) => (
        <div key={h.half} className="prompt-map-half" style={{ flexGrow: h.bytes }}>
          {h.sections.map((s) => (
            <div
              key={s.id}
              className={cx(
                "prompt-map-seg",
                s.id === current && "is-selected",
                finding && !counts.get(s.id) && "is-dim",
              )}
              style={{ flexGrow: s.bytes }}
              title={`${plainTitle(s, halves)} · ${fmtBytes(s.bytes)} · ${share(s.bytes, h.bytes)} of ${HALF[h.half].of}`}
              onClick={() => onPick(s.id)}
            />
          ))}
        </div>
      ))}
    </div>
  );
}

/**
 * Every top-level section, grouped by half: ONE tab stop, and the arrows move.
 *
 * A LISTBOX WITH GROUPS, and selection follows focus — the pattern for a list
 * whose every row is a view of one region beside it. The selected row is the
 * tab stop (a roving `tabIndex`), so Tab enters at the section being read and
 * leaves to the reader; ArrowUp/ArrowDown step across both halves, Home and End
 * go to the ends, and focus moves with the selection so it is never left on a
 * row that is no longer the one shown.
 */
function Outline({
  halves,
  current,
  counts,
  finding,
  onPick,
  label,
}: {
  halves: OutlineHalf[];
  current: string;
  counts: ReadonlyMap<string, number>;
  finding: boolean;
  onPick: (id: string) => void;
  label: string;
}) {
  const rows = useRef(new Map<string, HTMLDivElement>());
  const all = sectionsOf(halves);

  const go = (to: OutlineSection | undefined) => {
    if (!to) return;
    onPick(to.id);
    rows.current.get(to.id)?.focus();
  };
  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    const i = all.findIndex((s) => s.id === current);
    const to =
      e.key === "ArrowDown"
        ? all[Math.min(all.length - 1, i + 1)]
        : e.key === "ArrowUp"
          ? all[Math.max(0, i - 1)]
          : e.key === "Home"
            ? all[0]
            : e.key === "End"
              ? all[all.length - 1]
              : undefined;
    if (!to) return;
    e.preventDefault();
    go(to);
  };

  return (
    <div role="listbox" aria-label={label} className="prompt-toc" onKeyDown={onKeyDown}>
      {halves.map((h) => (
        <div
          key={h.half}
          role="group"
          aria-label={`${HALF[h.half].name}, ${fmtBytes(h.bytes)}`}
          className="prompt-toc-group"
        >
          <div className="prompt-toc-head t-label" aria-hidden="true">
            {HALF[h.half].name}
            <span className="muted"> · {fmtBytes(h.bytes)}</span>
          </div>
          {h.sections.map((s) => {
            const selected = s.id === current;
            const hits = counts.get(s.id) ?? 0;
            return (
              <div
                key={s.id}
                ref={(el) => {
                  if (el) rows.current.set(s.id, el);
                  else rows.current.delete(s.id);
                }}
                role="option"
                aria-selected={selected}
                tabIndex={selected ? 0 : -1}
                className={cx(
                  "prompt-toc-row",
                  selected && "is-selected",
                  finding && (hits === 0 ? "is-dim" : "is-hit"),
                )}
                onClick={() => go(s)}
              >
                <span className="prompt-toc-title">
                  <Title section={s} halves={halves} links="text" />
                </span>
                <span className="prompt-toc-meta t-num">
                  {finding && hits > 0 && (
                    <span className="prompt-toc-hits">
                      {hits}
                      <span className="sr-only"> {hits === 1 ? "match" : "matches"},</span>
                    </span>
                  )}
                  {fmtBytes(s.bytes)}
                </span>
                <span className="prompt-toc-weight" aria-hidden="true">
                  <span style={{ width: `${Math.max(1, (s.bytes / h.bytes) * 100)}%` }} />
                </span>
              </div>
            );
          })}
        </div>
      ))}
    </div>
  );
}

/**
 * One section's text and the headings inside it, drawn in place.
 *
 * NESTING FOLLOWS HEADING LEVEL — a ledger writes one `###` per prior turn
 * inside its block, and a quoted thread brings its own `##`s — and each is a
 * styled sub-title rather than a heading, for the reason the reader's own
 * title is. Rendered with the prompt's own line breaks (`MODEL_WORDS`: text
 * written line by line for a model reads by a model's habits).
 */
function SectionBody({ body, nested }: { body: string; nested: SectionNode[] }) {
  const rendered = useMemo(() => (body ? renderMarkdown(body, MODEL_WORDS) : null), [body]);
  if (body === "" && nested.length === 0) {
    // A heading with NOTHING under it is a fact about the prompt, so it is
    // said rather than drawn as an empty block, which reads as one that
    // failed to load.
    return (
      <span className="t-caption muted" data-find-skip="">
        Nothing under this heading.
      </span>
    );
  }
  return (
    <div className="prompt-body">
      {rendered && <div className="prose md">{rendered}</div>}
      {nested.map((child, i) => (
        <div key={i} className="prompt-sub">
          <div className="prompt-sub-title">{renderInline(child.title, `s${i}`)}</div>
          <SectionBody body={child.body} nested={child.children} />
        </div>
      ))}
    </div>
  );
}

/**
 * Marks the find's matches in the reader, where the browser can, and brings
 * the first one into view when the reader asked to be taken to it.
 *
 * THE CSS CUSTOM HIGHLIGHT API, because it marks ranges of text without
 * touching the DOM: wrapping each match in an element would re-render the
 * markdown on every keystroke and split text nodes the renderer owns. Where
 * it is absent nothing is marked and the counts still say where the matches
 * are.
 *
 * ONE NAMED HIGHLIGHT FOR THE PAGE, which `::highlight(prompt-find)` in
 * screens.css paints — so every open prompt contributes its ranges to one
 * shared set rather than overwriting the others'.
 *
 * WHAT IS MARKED IS WHAT WAS COUNTED: every text node under `root` except a
 * subtree carrying `data-find-skip`, which is how the reader leaves out its
 * own words (a size, a share, where Previous goes) and a title that is not
 * the section's own heading line.
 *
 * `reveal` CHANGING is a request — Enter, or Shift+Enter — and the first
 * match is scrolled to the NEAREST edge of whatever scrolls it: the page
 * scroller on most screens, a full-height column on others, and nothing at
 * all when it is already in view. Not on every keystroke: scrolling the page
 * under a box somebody is typing into moves the box away from them.
 */
function useFindMarks(
  root: RefObject<HTMLElement | null>,
  query: string,
  /** What is shown: the marks are re-taken whenever it changes. */
  on: string,
  reveal: number,
) {
  const revealed = useRef(reveal);
  useEffect(() => {
    const asked = revealed.current !== reveal;
    revealed.current = reveal;
    const pattern = findPattern(query);
    const box = root.current;
    if (!pattern || !box) return;
    const ranges = matchRanges(box, pattern);
    if (asked) ranges[0]?.startContainer.parentElement?.scrollIntoView({ block: "nearest" });
    if (!highlights()) return;
    const me = Symbol("prompt");
    marks.set(me, ranges);
    paint();
    return () => {
      marks.delete(me);
      paint();
    };
  }, [root, query, on, reveal]);
}

/** One range per match of `pattern` in the text under `root`, skipping `data-find-skip` subtrees. */
function matchRanges(root: HTMLElement, pattern: RegExp): Range[] {
  const ranges: Range[] = [];
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_ELEMENT | NodeFilter.SHOW_TEXT, {
    acceptNode: (node) =>
      node.nodeType !== Node.ELEMENT_NODE
        ? NodeFilter.FILTER_ACCEPT
        : (node as Element).hasAttribute("data-find-skip")
          ? NodeFilter.FILTER_REJECT
          : NodeFilter.FILTER_SKIP,
  });
  for (let node = walker.nextNode(); node; node = walker.nextNode()) {
    const text = node.nodeValue ?? "";
    for (const m of text.matchAll(pattern)) {
      const range = document.createRange();
      range.setStart(node, m.index);
      range.setEnd(node, m.index + m[0].length);
      ranges.push(range);
    }
  }
  return ranges;
}

/** The highlight registry, or null where the browser has none. */
function highlights(): {
  registry: { set(name: string, h: unknown): void; delete(name: string): void };
  Highlight: new (...ranges: Range[]) => unknown;
} | null {
  const registry = (globalThis.CSS as unknown as { highlights?: unknown } | undefined)?.highlights;
  const Highlight = (globalThis as unknown as { Highlight?: unknown }).Highlight;
  if (!registry || typeof Highlight !== "function") return null;
  return {
    registry: registry as { set(name: string, h: unknown): void; delete(name: string): void },
    Highlight: Highlight as new (...ranges: Range[]) => unknown,
  };
}

/** Every open prompt's marks, by the instance that took them. */
const marks = new Map<symbol, Range[]>();

/** Repaint the page's one highlight from every instance's marks. */
function paint() {
  const api = highlights();
  if (!api) return;
  const ranges = [...marks.values()].flat();
  if (ranges.length === 0) api.registry.delete(FIND_HIGHLIGHT);
  else api.registry.set(FIND_HIGHLIGHT, new api.Highlight(...ranges));
}

/** The highlight's name, which `::highlight()` in screens.css paints. */
const FIND_HIGHLIGHT = "prompt-find";
