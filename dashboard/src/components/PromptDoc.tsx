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
 * TWO VIEWS, AND EACH ONE DOES ITS WHOLE JOB. `Rendered` is for reading: the
 * outline, with every body set as the markdown it is. `Source` is the record:
 * the whole document, one block, byte for byte, one selection — what an
 * operator reproduces a turn from and what they diff when a model starts
 * behaving differently.
 *
 * WHY THE READING VIEW RENDERS. The bodies used to be source slices in code
 * blocks, on the rule that a prompt is a record. But THAT view was never the
 * record: building the outline consumes the heading lines, so what a fold
 * showed was the bytes with their structure taken out — the worst of both,
 * and the one thing the screen had no other route to was the reading. So a
 * seat's identity arrived as `You are **Engineer** at **Acme**`, a contract's
 * `- **done** — it meets the ask` as a column of literal hyphens and
 * asterisks, and every `` `submit_work` `` with its backticks: a reader
 * decoding markdown the model was handed already decoded.
 *
 * AND WHAT IS GENUINELY RECORD-SENSITIVE SURVIVES RENDERING. A fenced block
 * comes out as its own `pre` holding the exact text, so a tool schema, a JSON
 * example and a contract template are byte-identical in both views. What the
 * reading view spends is emphasis markers and list bullets — formatting, one
 * click from the bytes, on a control that says which one you are looking at.
 *
 * THE SWITCH IS OFFERED ON EVERY DOCUMENT, headings or none. It used to be
 * gated on having an outline, because without one the two views were the same
 * picture under two names. They are not any more: a prompt with no headings is
 * still markdown and still has a record behind it, so both views still answer
 * a question the other does not.
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
 */

import { useMemo, useState } from "react";
import { CodeBlock, Disclosure } from "@crewlethq/ui";
import { Segmented } from "~/ui/primitives.tsx";
import { nestSections, renderMarkdown, splitSections, type SectionNode } from "~/lib/markdown.ts";
import { fmtCount, plural } from "~/lib/format.ts";
import { RECORD_MAX_HEIGHT } from "~/components/common.tsx";

type View = "read" | "source";

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
 *
 * COUNTED ON THE SOURCE in both views, because it is a share of the prompt the
 * model was billed for rather than a measure of what this screen drew.
 */
function weigh(node: SectionNode): number {
  return node.body.length + node.children.reduce((sum, child) => sum + weigh(child), 0);
}

/**
 * One run of a prompt, as the markdown it is.
 *
 * `prose md` is the same pair a page body and a work item's description carry:
 * the renderer decides what a break is, and the blocks it emits get a
 * document's spacing. Unbounded, unlike the `Source` block — a ceiling belongs
 * on a wall of text a reader did not choose, and every block here is either a
 * section they opened by name or a document with no sections to open.
 */
function Rendered({ source }: { source: string }) {
  return <div className="prose md">{renderMarkdown(source)}</div>;
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
  // READING IS THE DEFAULT, because the question this fold is opened with is
  // what the phase was told, and the bytes are one click away. The other way
  // round, every reader pays the decoding on every open to serve the rarer
  // reproduce-and-diff.
  const [view, setView] = useState<View>("read");

  return (
    <div className="col gap-3">
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
            ? "rendered, and folded where the document has headings"
            : "the record as the model received it, byte for byte"}
        </span>
      </div>
      {docs.map((doc) => (
        <div className="col gap-1" key={doc.kind}>
          <div className="t-label">
            {doc.kind}
            <span className="muted">
              {" · "}
              {fmtCount(doc.text.length)} chars
              {view === "read" && outlined(doc)
                ? ` · ${plural(doc.sections.filter((s) => s.level > 0).length, "section")}`
                : ""}
            </span>
          </div>
          {view === "read" ? (
            // A document with no headings has no outline to draw and is still
            // markdown, so it is one rendered run rather than a fold nobody
            // could name.
            outlined(doc) ? (
              <Outline doc={doc} />
            ) : (
              <Rendered source={doc.text} />
            )
          ) : (
            /* THE TALLEST BLOCK ON THE PAGE by a wide margin, and `selectable`
               is how uilet makes it reachable: the tab stop, the accessible
               name and ⌘A tied into one flag. It is the record this view is
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
          <Rendered key={i} source={section.body} />
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
              {section.body !== "" && <Rendered source={section.body} />}
              {/* A heading with NOTHING under it — no text and no
                  sub-sections — is a fact about the prompt, so it is said
                  rather than drawn as an empty block: a block with no content
                  reads as one that failed to load. A heading that only
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
