/**
 * An executor's question, as a reading heads it and as a line names it.
 *
 * ONE RULE ON EVERY SCREEN: an executor's question is its title, whole, and
 * what it says beside it is its context (`question_context`), its own. Nothing
 * is split out of the question. An older build's free-text question was, at
 * its first sentence, and a question with a full stop inside it ("Should I
 * drop the legacy flag (e.g. ...") was cut there into a title of half a
 * sentence with the rest read as its context.
 *
 * A QUESTION PAST THE TITLE'S CAP ([DECISION_QUESTION_MAX_BYTES], the
 * engine's `decision.MaxQuestion`, which the run's ask tool holds a question
 * to) is such a free-text question. A reading draws it clamped to three lines
 * with "Show all" to read every word; a line that names it in a row cuts it to
 * one, since the reading is a press away. A seat's notice has no reading behind
 * it but its Answer, so it reads as a reading does ([RunQuestionNotice]).
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
 * The question in a seat's notice, read before its Answer: drawn as the reading
 * draws it, clamped to three lines past the title's cap with "Show all" to read
 * every word, so a long address cannot use up the preview and the only way to
 * read on is not a write. Its references are drawn as what they are, and live
 * only where the dashboard built the link (`checked`): a model wrote these words.
 */
export function RunQuestionNotice({
  question,
  empty = "",
}: {
  question: string | undefined;
  empty?: string;
}) {
  const id = useId();
  const [whole, setWhole] = useState(false);
  const said = (question ?? "").trim();
  if (!said) return <>{empty}</>;
  const long = pastCap(said);
  return (
    <>
      <Text variant="inherit" id={id} clamp={long && !whole ? 3 : undefined}>
        {renderTokens(said, "question", "checked")}
      </Text>
      {long && (
        <Button
          size="small"
          variant="ghost"
          aria-expanded={whole}
          aria-controls={id}
          onClick={() => setWhole(!whole)}
        >
          {whole ? "Show less" : "Show all"}
        </Button>
      )}
    </>
  );
}

/**
 * The question as a line names it, in a row whose own link opens the reading:
 * whole, or cut to one line where it runs past the title's cap. `empty` is what
 * the line says where the run recorded no question.
 */
export function RunQuestionLine({
  question,
  empty = "",
  quoted = false,
}: {
  question: string | undefined;
  empty?: string;
  /** Between quote marks, drawn inside the cut so they travel with the words. */
  quoted?: boolean;
}) {
  const said = (question ?? "").trim();
  if (!said) return <>{empty}</>;
  const words = quoted ? `“${said}”` : said;
  return pastCap(said) ? (
    <Text variant="inherit" truncate>
      {words}
    </Text>
  ) : (
    <>{words}</>
  );
}
