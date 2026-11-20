/**
 * Words a person or an agent wrote, quoted by the line that says what came of
 * a question: a reply, the note beside a choice, the reason a question was
 * withdrawn ("You replied “Hold it a week.”").
 *
 * THEY ARE MARKDOWN, a comment on the task like any other, drawn by the
 * renderer the conversation draws that comment with (`renderQuoted`): one
 * paragraph quoted in place, in the primary ink, and anything more set under
 * the line. Drawn as text, a reply read `**Pass**` and its list ran into the
 * sentence as one line of dashes.
 */

import type { ReactNode } from "react";
import { Text } from "@crewlethq/ui";
import type { Quoted } from "~/lib/markdown.ts";

/** Words quoted in place in the sentence about them: “Hold it a week.” */
export function InPlace({ children }: { children: ReactNode }) {
  return (
    <Text variant="inherit" tone="primary">
      “{children}”
    </Text>
  );
}

/** Quoted words in a line: in place after its sentence, or under it, set as markdown. */
export function Said({ quoted }: { quoted: Quoted }) {
  if ("inline" in quoted) {
    return (
      <>
        {" "}
        <InPlace>{quoted.inline}</InPlace>
      </>
    );
  }
  return <div className="prose md said">{quoted.blocks}</div>;
}
