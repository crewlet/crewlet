/**
 * A finding answered (`code_resolve_finding`): an author's Resolved, Won't fix
 * or Disputed, which leave a blocker standing for whoever raised it to judge,
 * and Dismiss, its raiser's, a maintainer's or a person's, which clears it.
 *
 * Offered under every finding that still STANDS (open, or answered by an
 * author), one answer at a time with a note; a finding dismissed or
 * superseded stands no more and takes none. Who may give which answer is the
 * engine's to say, from the change's authors and the repository's chart, and
 * its refusal is drawn beside the control. A dismissal is confirmed in the
 * same form, which says that it clears the finding for good.
 */

import { useState } from "react";
import { Button, Input, Select } from "@crewlethq/ui";
import { WriteButton, pressable } from "~/components/WriteButton.tsx";
import { useAct } from "~/lib/useAct.ts";
import type { CodeChangeDetail, CodeFindingRow } from "~/protocol/index.ts";

/** The answers a finding takes, in the engine's order, and what each press says it did. */
export const FINDING_ANSWERS: readonly { value: string; label: string; done: string }[] = [
  { value: "resolved", label: "Resolved", done: "Marked resolved" },
  { value: "wont_fix", label: "Won't fix", done: "Marked won't fix" },
  { value: "disputed", label: "Disputed", done: "Disputed" },
  { value: "dismissed", label: "Dismiss", done: "Dismissed" },
];

/** The states a finding still stands in, which alone take an answer. */
const STANDING = ["open", "resolved", "wont_fix", "disputed"];

export function FindingAnswer({
  change,
  finding,
}: {
  change: CodeChangeDetail;
  finding: CodeFindingRow;
}) {
  const write = useAct("code_resolve_finding");
  const [open, setOpen] = useState(false);
  const [resolution, setResolution] = useState("resolved");
  const [note, setNote] = useState("");
  if (!STANDING.includes(finding.state)) return null;
  const answer = FINDING_ANSWERS.find((a) => a.value === resolution) ?? FINDING_ANSWERS[0]!;
  const blocked = resolution === finding.state ? "It is answered this way already." : undefined;
  const submit = async () => {
    if (!pressable(write, blocked)) return;
    const result = await write.run(
      {
        change: change.ref,
        findings: [finding.id],
        resolution,
        ...(note.trim() ? { note: note.trim() } : {}),
      },
      { done: `${answer.done} “${finding.title}”` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) {
      setOpen(false);
      setNote("");
    }
  };
  if (!open) {
    return (
      <WriteButton
        write={write}
        size="small"
        variant="secondary"
        aria-label={`Answer the finding “${finding.title}”`}
        onPress={() => setOpen(true)}
      >
        Answer finding
      </WriteButton>
    );
  }
  return (
    <form
      className="col gap-2"
      aria-label={`Answer the finding “${finding.title}”`}
      onSubmit={(event) => {
        event.preventDefault();
        void submit();
      }}
    >
      <div className="row gap-2 wrap">
        <Select
          size="sm"
          width="auto"
          ariaLabel="Answer"
          value={resolution}
          onChange={(value) => setResolution(String(value))}
          options={FINDING_ANSWERS.map((a) => ({ value: a.value, label: a.label }))}
        />
        <Input
          aria-label="Note"
          placeholder="A note for whoever raised it (optional)"
          value={note}
          onChange={(event) => setNote(event.target.value)}
        />
      </div>
      <p className="muted t-caption">
        {resolution === "dismissed"
          ? "A dismissal clears the finding for good: it holds the change back no more, and nothing answers it again. Its raiser, a maintainer of the repository or a person dismisses one."
          : "An author's answer. A blocker stays standing until whoever raised it dismisses it or leaves it out of a later review."}
      </p>
      <div className="row gap-2 wrap">
        <WriteButton
          write={write}
          size="small"
          variant={resolution === "dismissed" ? "danger" : "primary"}
          blocked={blocked}
          onPress={() => void submit()}
        >
          {resolution === "dismissed" ? "Dismiss finding" : "Send answer"}
        </WriteButton>
        <Button size="small" variant="ghost" onClick={() => setOpen(false)} disabled={write.busy}>
          Cancel
        </Button>
      </div>
    </form>
  );
}
