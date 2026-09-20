/**
 * A phase's prompt, as the document it is rather than as one wall of text.
 *
 * WHAT WAS WRONG. Both halves of a phase's prompt were one `CodeBlock` each,
 * and a seat's system prompt runs to tens of kilobytes: identity, mission,
 * policies, the roster, the turn contract, the sandbox contract, whatever the
 * turn prefetched, the workers, the skills and the tool catalogue — a dozen
 * named sections in a scroller with no way to reach one. An operator asking
 * the question these screens exist for ("what was the reviewer actually told
 * about self-iterating?") had to scroll a 30 kB block looking for a heading.
 *
 * WHAT IT IS INSTEAD. Every prompt this engine builds is markdown, and its
 * sections are exactly the `##` headings the builders in
 * `internal/agent/prompts` emit. So the outline is DERIVED from the document
 * rather than declared here: one fold per heading, titled with the heading,
 * sized, closed. Nothing in this file names a section, which is what keeps it
 * correct when a prompt grows one — a hardcoded list would render a stale
 * outline and there would be no symptom but a section nobody could find.
 *
 * THE BODIES STAY VERBATIM. A prompt is a RECORD: it is what an operator
 * reproduces a turn from and what they diff when a model starts behaving
 * differently, so a section's body is the source slice, in a code block, not
 * this app's reading of it. Rendering it as prose would lose the one property
 * the screen is for.
 *
 * THE OUTLINE IS THE DOCUMENT'S OWN SHAPE, which mostly means flat and
 * sometimes does not. Every section a prompt builder writes is a peer, so an
 * executor's thirteen sections are thirteen rows; but a ledger's entries
 * (`### Iteration 1`, `### 2026-08-20`) are written INSIDE their block, and
 * hoisting those to the top would put a turn of somebody's conversation
 * between "Earlier in this conversation" and the ask. So the folds nest on
 * heading level, which is correct only because the levels are — the engine's
 * own suite holds every builder to one, after an `h1` over a run of `h2`s
 * would have nested the whole prompt inside the agent's identity.
 *
 * AND THE WHOLE DOCUMENT STAYS ONE SELECTION. An outline that is the only
 * route to the record turns "copy the prompt" into a dozen opens and a dozen
 * select-alls. The view switch is what pays that back: `whole` is byte for
 * byte the block this screen drew before, and it is offered only where there
 * are headings to have split — on a document with none, the two views would be
 * the same picture under two names.
 */

import { useMemo, useState } from "react";
import { CodeBlock, Disclosure } from "@crewlethq/ui";
import { Segmented } from "~/ui/primitives.tsx";
import { nestSections, splitSections, type SectionNode } from "~/lib/markdown.ts";
import { fmtCount, plural } from "~/lib/format.ts";
import { RECORD_MAX_HEIGHT } from "~/components/common.tsx";

type View = "sections" | "whole";

/**
 * One half of the prompt, with what it is called and what to call its blocks.
 *
 * `Half` rather than `Document`, which is a DOM global: a file that shadows
 * one reads fine until somebody reaches for the real type in it.
 */
interface Half {
  /** "System" or "User" — the label above the block. */
  kind: string;
  /** The source, verbatim. */
  text: string;
  /** What a screen reader calls this document when a block takes focus. */
  label: string;
  /** Its sections, nested on their heading levels. */
  sections: SectionNode[];
}

/** Whether a document has an outline at all, rather than one untitled run. */
function outlined(doc: Half): boolean {
  return doc.sections.some((s) => s.level > 0);
}

/**
 * How many characters of the prompt a section accounts for, its own
 * sub-sections included.
 *
 * THE SUBTREE, not the section's own body: a reader looking for where a heavy
 * prompt's weight went is asking about the BLOCK, and a conversation ledger
 * whose own body is its six rules and whose children are eight turns would
 * otherwise report the rules.
 */
function weigh(node: SectionNode): number {
  return node.body.length + node.children.reduce((sum, child) => sum + weigh(child), 0);
}

/**
 * The system prompt and the user message of one phase.
 *
 * Both are rendered under ONE view switch rather than one each: they are two
 * halves of a single record, and a reader who wants the bytes wants them for
 * the prompt, not for the system half of it.
 */
export function PromptRecord({
  phase,
  system,
  user,
}: {
  phase: string;
  system: string;
  user: string;
}) {
  const docs = useMemo<Half[]>(
    () =>
      [
        { kind: "System", text: system, label: `The ${phase} phase's system prompt` },
        { kind: "User", text: user, label: `The ${phase} phase's user message` },
      ]
        .filter((d) => d.text !== "")
        .map((d) => ({ ...d, sections: nestSections(splitSections(d.text)) })),
    [phase, system, user],
  );
  const [view, setView] = useState<View>("sections");
  const splittable = docs.some(outlined);

  return (
    <div className="col gap-3">
      {splittable && (
        <div className="row gap-2 wrap">
          <Segmented<View>
            size="sm"
            ariaLabel="How to show the prompt"
            value={view}
            onChange={setView}
            options={[
              { value: "sections", label: "Sections", title: "One fold per markdown heading" },
              { value: "whole", label: "Whole", title: "The document as it was sent" },
            ]}
          />
          <span className="t-caption muted">
            {view === "sections"
              ? "folded on the document's own headings"
              : "the record as the model received it"}
          </span>
        </div>
      )}
      {docs.map((doc) => (
        <div className="col gap-1" key={doc.kind}>
          <div className="t-label">
            {doc.kind}
            <span className="muted">
              {" · "}
              {fmtCount(doc.text.length)} chars
              {view === "sections" && outlined(doc)
                ? ` · ${plural(doc.sections.filter((s) => s.level > 0).length, "section")}`
                : ""}
            </span>
          </div>
          {view === "sections" && outlined(doc) ? (
            <Outline doc={doc} />
          ) : (
            /* THE TALLEST BLOCK ON THE PAGE by a wide margin, and `selectable`
               is how uilet makes it reachable: the tab stop, the accessible
               name and ⌘A tied into one flag. It is the record this screen is
               about, which is exactly what that flag is for. */
            <CodeBlock
              plain
              maxHeight={RECORD_MAX_HEIGHT}
              selectable
              label={doc.label}
              code={doc.text}
            />
          )}
        </div>
      ))}
    </div>
  );
}

/** One document as its sections. */
function Outline({ doc, nodes }: { doc: Half; nodes?: SectionNode[] }) {
  return (
    <div className="col gap-1">
      {(nodes ?? doc.sections).map((section, i) =>
        section.level === 0 ? (
          // THE LEAD RUN HAS NO HEADING, so it has no title to click and is
          // not a fold. It is the reviewer's one-line identity and the
          // sub-agent's parent prompt — short, and the first thing the model
          // read, so hiding it behind a control called nothing would be worse
          // than the wall of text this replaces.
          <CodeBlock
            key={i}
            plain
            maxHeight={RECORD_MAX_HEIGHT}
            focusWhenScrollable
            label={`${doc.label} — before the first heading`}
            code={section.body}
          />
        ) : (
          <Disclosure
            key={i}
            title={section.title}
            count={`${fmtCount(weigh(section))} chars`}
            meta={
              section.children.length > 0 ? plural(section.children.length, "section") : undefined
            }
            // A PROMPT'S HEADINGS ARE NOT THIS PAGE'S HEADINGS. uilet wraps a
            // disclosure's trigger in a real heading by default, which is
            // right for the card's own folds and wrong here: a turn with six
            // phases would put eighty headings from six quoted documents into
            // one screen's outline, and none of them is a section OF the
            // screen. Same rule a tool row follows in `PhaseCard`.
            headingLevel="none"
            // Closed, and mounting nothing until it is opened. The outline is
            // the point — twelve open sections is the wall of text again —
            // and a closed fold must not put its body into the card's text.
            lazy
          >
            <div className="col gap-1">
              {/* A SECTION IS A RECORD TOO, so it takes the same flag the
                  whole document does: while it has focus ⌘A means this
                  section rather than the page. */}
              {section.body !== "" && (
                <CodeBlock
                  plain
                  maxHeight={RECORD_MAX_HEIGHT}
                  selectable
                  label={`${doc.label} — ${section.title}`}
                  code={section.body}
                />
              )}
              {/* A heading with NOTHING under it — no text and no
                  sub-sections — is a fact about the prompt, so it is said
                  rather than drawn as an empty block: a `CodeBlock` with no
                  code reads as one that failed to load. A heading that only
                  introduces its sub-sections is not that, and gets no note. */}
              {section.body === "" && section.children.length === 0 && (
                <span className="t-caption muted">Nothing under this heading.</span>
              )}
              {section.children.length > 0 && <Outline doc={doc} nodes={section.children} />}
            </div>
          </Disclosure>
        ),
      )}
    </div>
  );
}
