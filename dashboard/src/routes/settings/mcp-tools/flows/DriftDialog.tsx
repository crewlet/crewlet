/**
 * Review the tools a server changed since their levels were decided: for
 * each, what moved (its title or description as a text diff, its arguments a
 * line each, the hints that moved keyed by the hint), and one choice, Accept
 * or Keep blocked.
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
 * EVERY PART IS THE ONE DIFF THE DASHBOARD DRAWS (`components/DiffView.tsx`),
 * never the package's: its diffs paint a removed line in the danger state's
 * hue and an added one in the success state's, and here a removed hint is no
 * failure. The two texts are handed to it whole, so its own line diff
 * compares them; the hints are keyed, each moved hint its old word removed
 * and its new one added.
 */

import { useMemo, useState } from "react";
import { Button, Callout, Modal, RailList, Skeleton, Text, useToast } from "@crewlethq/ui";
import { DiffView, type DiffHunk } from "~/components/DiffView.tsx";
import type { McpChangeView, McpMemberServerStatus } from "~/contract/mcp.ts";
import type { ToolAnnotations } from "~/contract/tools.ts";
import { plural } from "~/lib/format.ts";
import { reviewChanges } from "../api.ts";
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
          {after.description || after.title || "The server says nothing about it."}
        </Text>
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
      {kinds.includes("title") ? (
        <section className="mcp-stack">
          <Text as="p" variant="label">
            Title
          </Text>
          <DiffView
            before={before?.title ?? ""}
            after={after?.title ?? ""}
            aria-label={`${change.tool} title, before and after`}
          />
        </section>
      ) : null}
      {kinds.includes("description") ? (
        <section className="mcp-stack">
          <Text as="p" variant="label">
            Description
          </Text>
          <DiffView
            before={before?.description ?? ""}
            after={after?.description ?? ""}
            aria-label={`${change.tool} description, before and after`}
          />
        </section>
      ) : null}
      {kinds.includes("schema") ? (
        <section className="mcp-stack">
          <Text as="p" variant="label">
            Arguments
          </Text>
          <DiffView
            before={argumentLines(before?.input_schema)}
            after={argumentLines(after?.input_schema)}
            aria-label={`${change.tool} arguments, before and after`}
          />
        </section>
      ) : null}
      {kinds.includes("annotations") && before && after ? (
        <section className="mcp-stack">
          <Text as="p" variant="label">
            Hints
          </Text>
          <DiffView hunks={hints} aria-label={`${change.tool} hints, before and after`} />
        </section>
      ) : null}
    </div>
  );
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
  // THE ONES THAT CLAIM LESS RISK FIRST, then as the engine lists them.
  const ordered = [...changes].sort((a, b) => Number(b.loosened) - Number(a.loosened));
  const [chosen, setChosen] = useState("");
  const [busy, setBusy] = useState(false);
  const current = ordered.find((c) => c.tool === chosen) ?? ordered[0] ?? null;
  const held = changes.filter((c) => c.held).length;

  const decide = async (change: McpChangeView, how: "accept" | "block") => {
    setBusy(true);
    try {
      const said =
        how === "accept"
          ? `Accepted the change to ${change.tool} on ${server.name}`
          : `Kept ${change.tool} on ${server.name} blocked`;
      const answer = await reviewChanges(
        server.name,
        { [how]: { [change.tool]: change.after?.digest ?? null } },
        said,
      );
      if (answer.kind === "ok") {
        if (answer.value.moved.length > 0) {
          toast.show({
            variant: "warning",
            title: "Somebody decided it first",
            message: `${answer.value.moved.join(", ")} was decided while this was open. Read it again.`,
          });
        } else {
          toast.show({
            variant: "success",
            title: how === "accept" ? `${change.tool} is accepted` : `${change.tool} is blocked`,
          });
        }
        onDone();
        void detail.reload({ quiet: true });
        if (changes.length <= 1) onClose();
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
