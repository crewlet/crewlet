/**
 * A coding run's question, as a reading heads it and as a line names it.
 *
 * ONE RULE ON EVERY SCREEN: a run's question is its title, whole, and what it
 * says beside it is its context (`question_context`), the run's own. Nothing
 * is split out of the question. An older build's free-text question was, at
 * its first sentence, and a question with a full stop inside it ("Should I
 * drop the legacy flag (e.g. ...") was cut there into a title of half a
 * sentence with the rest read as its context.
 *
 * A QUESTION PAST THE TITLE'S CAP ([DECISION_QUESTION_MAX_BYTES], the
 * engine's `decision.MaxQuestion`, which the run's ask tool holds a question
 * to) is such a free-text question. A reading draws it clamped to three lines
 * with "Show all" to read every word; a line that names it cuts it to one,
 * since the reading is a press away.
 */

import { useId, useState } from "react";
import { Button, ReadingPaneTitle, Stack, Text } from "@crewlethq/ui";
import { DECISION_QUESTION_MAX_BYTES } from "~/contract/work.ts";
import { utf8Bytes } from "~/lib/format.ts";
import { renderTokens } from "~/lib/markdown.ts";

/** Whether a question runs past the title's cap. */
function pastCap(question: string): boolean {
  return utf8Bytes(question) > DECISION_QUESTION_MAX_BYTES;
}

/**
 * The question heading a run's reading: the Inbox's pane (`pane`, its
 * `ReadingPaneTitle`) or the answer dialog (`dialog`, a heading paragraph
 * under the dialog's own title). Its words are drawn with their marks and
 * links, part of the run's message where a `TokenScope` names one.
 */
export function RunQuestion({
  question,
  as = "pane",
}: {
  question: string;
  as?: "pane" | "dialog";
}) {
  const id = useId();
  const [whole, setWhole] = useState(false);
  const long = pastCap(question);
  const body = (
    <Text variant="inherit" id={id} clamp={long && !whole ? 3 : undefined}>
      {renderTokens(question, "question", "text", true)}
    </Text>
  );
  const title =
    as === "pane" ? (
      <ReadingPaneTitle>{body}</ReadingPaneTitle>
    ) : (
      <Text as="p" variant="heading">
        {body}
      </Text>
    );
  if (!long) return title;
  // THE CONTROL SITS UNDER THE TITLE, never inside it: inside, its words
  // would join the heading's name, and the pane is named by its heading.
  return (
    <Stack gap={1} align="start">
      {title}
      <Button
        size="small"
        variant="ghost"
        aria-expanded={whole}
        aria-controls={id}
        onClick={() => setWhole(!whole)}
      >
        {whole ? "Show less" : "Show all"}
      </Button>
    </Stack>
  );
}

/**
 * The question as a line names it, in a row, a banner or a sentence: whole,
 * or cut to one line where it runs past the title's cap. `empty` is what the
 * line says where the run recorded no question.
 */
export function RunQuestionLine({
  question,
  empty = "",
}: {
  question: string | undefined;
  empty?: string;
}) {
  const said = (question ?? "").trim();
  if (!said) return <>{empty}</>;
  return pastCap(said) ? (
    <Text variant="inherit" truncate>
      {said}
    </Text>
  ) : (
    <>{said}</>
  );
}
