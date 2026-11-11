/**
 * Review the tools a server changed since their levels were decided: for
 * each, what moved (its title or description as a text diff, its schemas
 * whole with its arguments a line each above them where those moved, the
 * hints that moved keyed by the hint), and one choice, Accept or Keep blocked.
 *
 * THE ENGINE SAYS WHAT CHANGED AND WHAT IT HOLDS. Each row is the detail's
 * `changes` (the parts that moved, whether it now claims less risk, whether
 * it holds the tool at Needs approval), and a decision names the digest of
 * the definition reviewed, so a server that swapped it between the review
 * and the answer is refused and the review is read again.
 *
 * THE SUSPICIOUS DIRECTION IS SAID: a change whose hints now claim less risk
 * is flagged, because that is how a server would talk its way past an
 * approval.
 *
 * A PART THAT MOVED IS NEVER DRAWN AS UNCHANGED. The engine names the parts it
 * cannot show (`unshown`: a definition it keeps no whole copy of), and each is
 * said to have changed and not to be shown; every diff here also says so in
 * place of "Nothing changed", should its two texts read alike.
 *
 * WORDING ON READ-ONLY TOOLS IS ACCEPTED TOGETHER, ONCE READ: two or more
 * changes that only reword a read-only tool are offered as one action, which
 * first lists each tool with the words it changed (a rewording can still
 * change what a tool says it does), frozen as listed, and only on a second
 * press sends each listed tool's digest in one decision, so a definition
 * swapped since is still refused by name. Every other change is decided one
 * by one.
 *
 * A DECISION WRITTEN IN PART IS SAID AS SUCH: the engine writes several one
 * by one, and where the store fails one it names what was decided and what
 * was not, which the dialog says tool by tool before reading the changes
 * again.
 *
 * EVERY PART IS THE ONE DIFF THE DASHBOARD DRAWS (`components/DiffView.tsx`),
 * never the package's: its diffs paint a removed line in the danger state's
 * hue and an added one in the success state's, and here a removed hint is no
 * failure. The two texts are handed to it whole, so its own line diff
 * compares them; the hints are keyed, each moved hint its old word removed
 * and its new one added.
 */

import { useMemo, useState, type ReactNode } from "react";
import {
  Button,
  Callout,
  DescriptionList,
  Modal,
  RailList,
  Skeleton,
  Text,
  useToast,
} from "@crewlethq/ui";
import { DiffView, type DiffHunk } from "~/components/DiffView.tsx";
import type {
  McpChangesPartial,
  McpChangeView,
  McpMemberServerStatus,
  McpToolView,
} from "~/contract/mcp.ts";
import type { ToolAnnotations } from "~/contract/tools.ts";
import { plural } from "~/lib/format.ts";
import { changedRuns, type Run } from "~/lib/wordDiff.ts";
import { reviewChanges, type ChangesDecision } from "../api.ts";
import { useServerDetail } from "../data.ts";
import { RiskChips } from "../parts.tsx";
import { CHANGE_KIND, HINT, hintWord, refusedWords } from "../words.ts";

/** A schema's arguments, a line each, for the arguments diff: `name: type, required`. */
function argumentLines(schema: Record<string, unknown> | null | undefined): string {
  const properties =
    schema && typeof schema.properties === "object" && schema.properties !== null
      ? (schema.properties as Record<string, unknown>)
      : {};
  const required = Array.isArray(schema?.required) ? (schema.required as unknown[]) : [];
  return Object.entries(properties)
    .map(([name, p]) => {
      const type =
        typeof p === "object" && p !== null && typeof (p as { type?: unknown }).type === "string"
          ? (p as { type: string }).type
          : "any";
      return `${name}: ${type}${required.includes(name) ? ", required" : ""}`;
    })
    .join("\n");
}

/**
 * The hints that moved, as a KEYED diff: one hunk a hint, its old word
 * removed and its new one added under the hint's name, so the diff underlines
 * what moved. One hunk each, skipping nothing, because the moved hints are the
 * whole answer rather than runs cut out of a longer text.
 */
function hintHunks(before: ToolAnnotations, after: ToolAnnotations): DiffHunk[] {
  return (Object.keys(HINT) as (keyof typeof HINT)[])
    .filter((k) => before[k] !== after[k])
    .map((k) => ({
      skipped: 0,
      lines: [
        {
          kind: "remove" as const,
          text: hintWord(before[k]),
          before: null,
          after: null,
          path: HINT[k],
        },
        {
          kind: "add" as const,
          text: hintWord(after[k]),
          before: null,
          after: null,
          path: HINT[k],
        },
      ],
    }));
}

/** A definition's title, as its title part covers it: its own and the one its hints carry. */
function titleText(def: McpChangeView["before"] | McpChangeView["after"]): string {
  if (!def) return "";
  return [def.title, def.hints_title ? `In its hints: ${def.hints_title}` : ""]
    .filter(Boolean)
    .join("\n");
}

/** A definition's schemas whole, for a schema change its argument lines do not show. */
function schemaText(def: McpChangeView["before"] | McpChangeView["after"]): string {
  if (!def) return "";
  return JSON.stringify({ input: def.input_schema, output: def.output_schema }, null, 2);
}

/** The words a part is named by in a sentence. */
const PART_WORD: Readonly<Record<string, string>> = {
  title: "title",
  description: "description",
  schema: "arguments",
  annotations: "hints",
};

/**
 * A part that moved and cannot be drawn: said to have changed, never shown as
 * unchanged. `kept` is false where the engine keeps no whole copy of it.
 */
function NotShown({ kind, kept }: { kind: string; kept: boolean }) {
  const word = PART_WORD[kind] ?? kind;
  return (
    <Text as="p" variant="body" tone="secondary">
      {kept
        ? `The ${word} changed, but this review cannot show how.`
        : `The ${word} changed, but the engine keeps no whole copy of it, so this review cannot show how.`}{" "}
      Ask the server's vendor what changed before you accept it.
    </Text>
  );
}

/** One part of a change, under its name. */
function Part({ label, children }: { label: string; children: ReactNode }) {
  return (
    <section className="mcp-stack">
      <Text as="p" variant="label">
        {label}
      </Text>
      {children}
    </section>
  );
}

/** How one moved part changed: its diff, or why it cannot be drawn. */
function PartChange({
  change,
  kind,
  hints,
}: {
  change: McpChangeView;
  kind: string;
  hints: DiffHunk[];
}) {
  const { before, after, tool } = change;
  const label = CHANGE_KIND[kind] ?? kind;
  if (change.unshown.includes(kind)) {
    return (
      <Part label={label}>
        <NotShown kind={kind} kept={false} />
      </Part>
    );
  }
  const guard = <NotShown kind={kind} kept />;
  switch (kind) {
    case "title":
      return (
        <Part label={label}>
          <DiffView
            before={titleText(before)}
            after={titleText(after)}
            empty={guard}
            aria-label={`${tool} title, before and after`}
          />
        </Part>
      );
    case "description":
      return (
        <Part label={label}>
          <DiffView
            before={before?.description ?? ""}
            after={after?.description ?? ""}
            empty={guard}
            aria-label={`${tool} description, before and after`}
          />
        </Part>
      );
    case "schema": {
      // THE SCHEMAS WHOLE, ALWAYS: an argument's description, enum or default
      // and the output schema are text the model reads, and move beside an
      // added argument as readily as alone. The arguments a line each above
      // them, where those moved, as the summary.
      const was = argumentLines(before?.input_schema);
      const now = argumentLines(after?.input_schema);
      return (
        <>
          {was !== now ? (
            <Part label={label}>
              <DiffView
                before={was}
                after={now}
                empty={guard}
                aria-label={`${tool} arguments, before and after`}
              />
            </Part>
          ) : null}
          <Part label="Schema">
            <DiffView
              before={schemaText(before)}
              after={schemaText(after)}
              empty={guard}
              aria-label={`${tool} schema, before and after`}
            />
          </Part>
        </>
      );
    }
    case "annotations":
      return (
        <Part label={label}>
          <DiffView hunks={hints} empty={guard} aria-label={`${tool} hints, before and after`} />
        </Part>
      );
    default:
      return <Part label={label}>{guard}</Part>;
  }
}

/** What one change moved, part by part. */
function ChangeBody({ change }: { change: McpChangeView }) {
  const { before, after, kinds } = change;
  // BUILT ONCE PER CHANGE: a diff handed new hunks starts its window over, and
  // the dialog draws again on every answer it sends.
  const hints = useMemo(
    () => (before && after ? hintHunks(before.annotations, after.annotations) : []),
    [before, after],
  );
  if (kinds.includes("removed")) {
    return (
      <Text as="p" variant="body" tone="secondary">
        The server no longer lists it. Accepting that it went takes its row off the Tools tab;
        nothing else changes.
      </Text>
    );
  }
  if (kinds.includes("added") && after) {
    return (
      <div className="mcp-stack">
        <Text as="p" variant="body">
          {after.description ||
            after.title ||
            after.hints_title ||
            "The server says nothing about it."}
        </Text>
        {after.whole ? null : (
          <Text as="p" variant="caption" tone="secondary">
            Only the start of its description is kept.
          </Text>
        )}
        <RiskChips tool={after} />
        <Text as="p" variant="caption" tone="secondary">
          Arguments:{" "}
          {argumentLines(after.input_schema)
            .split("\n")
            .filter(Boolean)
            .map((line) => line.split(":")[0])
            .join(", ") || "none"}
          .{change.held ? " It asks first until you accept it." : ""}
        </Text>
      </div>
    );
  }
  return (
    <div className="mcp-stack">
      {kinds.map((kind) => (
        <PartChange key={kind} change={change} kind={kind} hints={hints} />
      ))}
    </div>
  );
}

/** The parts a wording change may move. */
const WORDING = new Set(["title", "description"]);

/**
 * Whether a change only rewords a read-only tool: its title or description
 * moved and nothing else, every part it moved can be shown, it claims no less
 * risk, and the tool is read-only both as its server declares it and as it
 * runs here, the company's overrides applied. The changes accepted together.
 */
function rewordsReadOnlyTool(change: McpChangeView, tools: readonly McpToolView[]): boolean {
  const { before, after, kinds } = change;
  if (!before || !after || change.loosened || change.unshown.length > 0) return false;
  if (kinds.length === 0 || !kinds.every((k) => WORDING.has(k))) return false;
  const row = tools.find((t) => t.raw === change.tool);
  return before.risk === "read_only" && after.risk === "read_only" && row?.risk === "read_only";
}

/** A text on one line: its runs of white space, line breaks included, as one space. */
function oneLine(text: string): string {
  return text.replace(/\s+/g, " ").trim();
}

/**
 * The words a wording change moved, as its diff underlines them (`was` and
 * `now`), one part after another. A rewrite shares no word, so the diff
 * underlines nothing and the whole of each is the change.
 */
function changedWords(change: McpChangeView): string {
  return change.kinds
    .filter((kind) => WORDING.has(kind))
    .map((kind) => {
      const was = kind === "title" ? titleText(change.before) : (change.before?.description ?? "");
      const now = kind === "title" ? titleText(change.after) : (change.after?.description ?? "");
      const runs = changedRuns(was, now);
      const rewrite = runs.before === null && runs.after === null;
      const cut = (text: string, run: Run | null) => (run ? text.slice(run.start, run.end) : "");
      const before = oneLine(rewrite ? was : cut(was, runs.before));
      const after = oneLine(rewrite ? now : cut(now, runs.after));
      const words =
        before && after
          ? `“${before}” → “${after}”`
          : after
            ? `adds “${after}”`
            : `drops “${before}”`;
      return kind === "title" ? `Title: ${words}` : words;
    })
    .join(" · ");
}

/** A wording change as the first press of its action froze it. */
interface Reworded {
  tool: string;
  /** The digest of the definition read, which the decision names. */
  digest: string | null;
  /** The words it changed, as they were listed. */
  words: string;
}

/** The decided and the undecided a decision written in part names, or null for any other refusal. */
function partialOf(body: Record<string, unknown>): McpChangesPartial | null {
  const list = (v: unknown) =>
    Array.isArray(v) ? v.filter((x): x is string => typeof x === "string") : null;
  const undecided = list(body.undecided);
  if (!undecided || undecided.length === 0) return null;
  return {
    accepted: list(body.accepted) ?? [],
    blocked: list(body.blocked) ?? [],
    moved: list(body.moved) ?? [],
    undecided,
  };
}

/** A partial write's title: nothing changed only where nothing was decided. */
function partialTitle(p: McpChangesPartial): string {
  const decided = p.accepted.length + p.blocked.length;
  return decided === 0
    ? "Nothing was changed"
    : `${plural(decided, "change")} decided, ${plural(p.undecided.length, "change")} not`;
}

/** What a partial write decided and what it did not, tool by tool. */
function partialWords(p: McpChangesPartial): string {
  return [
    p.accepted.length > 0 ? `Accepted: ${p.accepted.join(", ")}.` : "",
    p.blocked.length > 0 ? `Kept blocked: ${p.blocked.join(", ")}.` : "",
    p.moved.length > 0 ? `Decided by somebody else first: ${p.moved.join(", ")}.` : "",
    `Not decided: ${p.undecided.join(", ")}. The engine could not write them; review them again.`,
  ]
    .filter(Boolean)
    .join(" ");
}

export function DriftDialog({
  server,
  onClose,
  onDone,
}: {
  server: McpMemberServerStatus;
  onClose: () => void;
  onDone: () => void;
}) {
  const toast = useToast();
  const detail = useServerDetail(server.name);
  const changes = detail.data?.reading === "admin" ? detail.data.changes : [];
  const tools = detail.data?.tools ?? [];
  // THE ONES THAT CLAIM LESS RISK FIRST, then as the engine lists them.
  const ordered = [...changes].sort((a, b) => Number(b.loosened) - Number(a.loosened));
  const [chosen, setChosen] = useState("");
  const [busy, setBusy] = useState(false);
  // THE SECOND PRESS: the wording action first lists what it would accept,
  // FROZEN at the first press (each tool, the digest and the words shown), so
  // a reading that brings another definition meanwhile changes neither what
  // is listed nor what is sent; the engine refuses whatever moved since.
  const [confirming, setConfirming] = useState<readonly Reworded[] | null>(null);
  const current = ordered.find((c) => c.tool === chosen) ?? ordered[0] ?? null;
  const held = changes.filter((c) => c.held).length;

  // ONE TOOL A DECISION: a tool listed under two new definitions at once is
  // decided on its own, by the definition the person reads.
  const wording = changes.filter(
    (c) =>
      rewordsReadOnlyTool(c, tools) &&
      changes.filter((other) => other.tool === c.tool).length === 1,
  );

  const send = async (decision: ChangesDecision, said: string, done: string, decided: number) => {
    setBusy(true);
    try {
      const answer = await reviewChanges(server.name, decision, said);
      if (answer.kind === "ok") {
        if (answer.value.moved.length > 0) {
          toast.show({
            variant: "warning",
            title: "Somebody decided it first",
            message: `${answer.value.moved.join(", ")} was decided while this was open. Read it again.`,
          });
        } else {
          toast.show({ variant: "success", title: done });
        }
        setConfirming(null);
        onDone();
        void detail.reload({ quiet: true });
        if (changes.length <= decided) onClose();
        return;
      }
      const partial = partialOf(answer.body);
      if (partial) {
        // WRITTEN IN PART: say exactly what was decided and what was not,
        // and read the changes again, which no longer list the decided ones.
        setConfirming(null);
        onDone();
        void detail.reload({ quiet: true });
        toast.show({
          variant: "danger",
          title: partialTitle(partial),
          message: partialWords(partial),
        });
        return;
      }
      if (answer.code === "stale_version") {
        // THE SERVER CHANGED IT AGAIN, or somebody decided it: read it again.
        void detail.reload({ quiet: true });
      }
      toast.show({
        variant: "danger",
        title: "Nothing was changed",
        message: refusedWords(answer),
      });
    } finally {
      setBusy(false);
    }
  };

  const decide = (change: McpChangeView, how: "accept" | "block") =>
    send(
      { [how]: { [change.tool]: change.after?.digest ?? null } },
      how === "accept"
        ? `Accepted the change to ${change.tool} on ${server.name}`
        : `Kept ${change.tool} on ${server.name} blocked`,
      how === "accept" ? `${change.tool} is accepted` : `${change.tool} is blocked`,
      1,
    );

  // EACH BY THE DIGEST REVIEWED, as the first press froze it, in one
  // decision: the engine refuses it whole, naming the tools, if any of them
  // moved since.
  const acceptWording = (frozen: readonly Reworded[]) =>
    send(
      { accept: Object.fromEntries(frozen.map((c) => [c.tool, c.digest])) },
      `Accepted ${plural(frozen.length, "wording change")} on ${server.name}`,
      `${plural(frozen.length, "wording change")} accepted`,
      frozen.length,
    );

  if (detail.data === null) {
    return (
      <Modal open onClose={onClose} title={`Review ${server.title}'s tool changes`} size="lg">
        <Skeleton variant="text" rows={6} label="Reading the changes" />
      </Modal>
    );
  }
  if (!current) {
    return (
      <Modal
        open
        onClose={onClose}
        title="No changes to review"
        size="sm"
        footer={<Button onClick={onClose}>Close</Button>}
      >
        <Text as="p">Every tool on {server.title} matches what was decided.</Text>
      </Modal>
    );
  }
  if (confirming) {
    return (
      <Modal
        open
        onClose={onClose}
        title={`Accept ${plural(confirming.length, "wording change")} on ${server.title}?`}
        subtitle="Each tool only rewords itself and stays read-only. Read what each now says before you accept."
        size="lg"
        footer={
          <>
            <Button variant="secondary" disabled={busy} onClick={() => setConfirming(null)}>
              Cancel
            </Button>
            <Button variant="primary" loading={busy} onClick={() => void acceptWording(confirming)}>
              Accept {confirming.length} changes
            </Button>
          </>
        }
      >
        <DescriptionList
          aria-label="The wording each tool changed"
          items={confirming.map((c) => [
            <Text key="tool" as="span" variant="cell" mono>
              {c.tool}
            </Text>,
            <Text key="words" as="span" variant="body" truncate>
              {c.words}
            </Text>,
          ])}
        />
      </Modal>
    );
  }
  const removed = current.kinds.includes("removed");
  return (
    <Modal
      open
      onClose={onClose}
      title={`Review ${server.title}'s tool changes`}
      subtitle={`${plural(changes.length, "change")} to review.${
        held > 0
          ? ` Until you do, ${held === 1 ? "the tool it holds asks" : `the ${held} tools they hold ask`} for approval.`
          : ""
      }`}
      size="lg"
      footerStart={
        wording.length >= 2 ? (
          <Button
            variant="secondary"
            disabled={busy}
            onClick={() =>
              setConfirming(
                wording.map((c) => ({
                  tool: c.tool,
                  digest: c.after?.digest ?? null,
                  words: changedWords(c),
                })),
              )
            }
          >
            Accept {wording.length} wording changes
          </Button>
        ) : null
      }
      footer={
        <>
          {removed ? null : (
            <Button
              variant="secondary"
              disabled={busy}
              onClick={() => void decide(current, "block")}
            >
              Keep blocked
            </Button>
          )}
          <Button variant="primary" loading={busy} onClick={() => void decide(current, "accept")}>
            {removed
              ? "Accept that it went"
              : current.kinds.includes("added")
                ? "Accept the new tool"
                : "Accept change"}
          </Button>
        </>
      }
    >
      <RailList
        label="Changed tools"
        items={ordered.map((c) => ({
          key: c.tool,
          label: c.tool,
          description: c.kinds.map((k) => CHANGE_KIND[k] ?? k).join(", "),
        }))}
        value={current.tool}
        onChange={setChosen}
      >
        <div className="mcp-stack mcp-drift">
          <Text as="p" variant="cell" mono>
            {current.tool}
          </Text>
          {current.loosened ? (
            <Callout variant="warning" title="It now claims to be less risky">
              Its hints moved towards less risk. A server should rarely do that; accept only if the
              vendor says why.
            </Callout>
          ) : null}
          <ChangeBody change={current} />
        </div>
      </RailList>
    </Modal>
  );
}
