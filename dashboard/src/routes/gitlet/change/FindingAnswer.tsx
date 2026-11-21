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

import { useEffect, useRef, useState } from "react";
import { Button, Input, Select } from "@crewlethq/ui";
import { CircleCheckGlyph } from "@crewlethq/icons/glyphs";
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

/** Whether a finding in `state` still stands, and so takes an answer. */
export function findingStands(state: string): boolean {
  return STANDING.includes(state);
}

/** Answer, or once pressed its form, which is the card's whole foot until Send or Cancel. */
export function FindingAnswer({
  change,
  finding,
  open,
  onOpenChange,
  focusBack = false,
}: {
  change: CodeChangeDetail;
  finding: CodeFindingRow;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** The form just closed: Answer takes the focus back. */
  focusBack?: boolean;
}) {
  const write = useAct("code_resolve_finding");
  const [resolution, setResolution] = useState("resolved");
  const [note, setNote] = useState("");
  const press = useRef<HTMLSpanElement>(null);
  useEffect(() => {
    if (focusBack && !open) press.current?.querySelector("button")?.focus();
  }, [focusBack, open]);
  if (!findingStands(finding.state)) return null;
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
      onOpenChange(false);
      setNote("");
    }
  };
  if (!open) {
    return (
      <span ref={press} className="code-finding__press">
        <WriteButton
          write={write}
          size="small"
          variant="ghost"
          leadingIcon={<CircleCheckGlyph size="sm" />}
          aria-label={`Answer the finding “${finding.title}”`}
          onPress={() => onOpenChange(true)}
        >
          Answer
        </WriteButton>
      </span>
    );
  }
  return (
    <form
      className="code-finding__answer"
      aria-label={`Answer the finding “${finding.title}”`}
      onSubmit={(event) => {
        event.preventDefault();
        void submit();
      }}
    >
      <div className="code-finding__answer-fields">
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
          width="full"
          placeholder="A note for whoever raised it (optional)"
          autoFocus
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
        <Button
          size="small"
          variant="ghost"
          onClick={() => onOpenChange(false)}
          disabled={write.busy}
        >
          Cancel
        </Button>
      </div>
    </form>
  );
}
