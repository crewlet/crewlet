/**
 * A decision's options as one-press buttons, for whichever write answers it,
 * and the same options once it is answered, the one chosen marked.
 */

import { ChoiceButtons, Stack, type ChoiceButtonOption } from "@crewlethq/ui";
import { RefusalNote, WriteButton, pressable } from "./WriteButton.tsx";
import { renderTokens } from "~/lib/markdown.ts";
import type { Act } from "~/lib/useAct.ts";
import type { ActionTool } from "~/protocol/act.ts";

/** A decision's option, as both lists draw it. */
interface Option {
  id: string;
  label: string;
  detail?: string;
}

/**
 * The options as `ChoiceButtons` takes them.
 *
 * AN OPTION IS A BUTTON: its colours and references are drawn with their
 * marks and never as links, which would steal the press. Each is PART OF THE
 * ASK'S MESSAGE where a `TokenScope` names one.
 */
function choiceOptions(options: readonly Option[]): ChoiceButtonOption[] {
  return options.map((o) => ({
    value: o.id,
    label: renderTokens(o.label, `option-${o.id}`, "text", true),
    detail:
      o.detail === undefined ? undefined : renderTokens(o.detail, `detail-${o.id}`, "text", true),
  }));
}

/**
 * A decision's options as buttons, each sending its choice with one press, the
 * recommended one first and filled, and "Something else" to answer in words.
 *
 * ONE COPY for both things a person is asked to choose on: a work item's ask
 * (`AnswerAsk`'s `buttons` layout, `comment_on_work_item{choice}`) and a
 * parked coding run (`answer_run{choice}`). The write is the caller's, so
 * the same buttons send either, and nothing here is pressable while it is in
 * flight.
 */
export function AnswerChoices<T extends ActionTool>({
  write,
  options,
  recommended,
  pending,
  onChoose,
  onSomethingElse,
}: {
  /** The write a choice is sent through. */
  write: Act<T>;
  options: readonly Option[];
  /** The option the asker recommends, if any. */
  recommended?: string | undefined;
  /** The option being sent, while the write is in flight. */
  pending: string | null;
  /** An option was pressed, and the write can take it. */
  onChoose: (choice: string) => void;
  /** "Something else" was pressed: the caller opens its field. */
  onSomethingElse: () => void;
}) {
  return (
    <Stack gap={2}>
      <ChoiceButtons
        label="Answers"
        options={choiceOptions(options)}
        recommended={recommended}
        pending={pending}
        disabledReason={write.access.can ? undefined : write.access.reason}
        onChoose={(choice) => {
          if (pressable(write)) onChoose(choice);
        }}
      />
      <WriteButton
        write={write}
        size="small"
        variant="ghost"
        pressing={false}
        showRefusal={false}
        blocked={write.busy ? "Your answer to this ask is being sent" : undefined}
        onPress={onSomethingElse}
      >
        Something else
      </WriteButton>
      <RefusalNote write={write} />
    </Stack>
  );
}

/**
 * A decision's options once it is answered, read as they were offered, with
 * the one chosen marked as chosen and none marked where it was answered in
 * words. Nothing here sends: every option says why it cannot be pressed.
 *
 * ONE COPY for every place an answered choice is read back: the Inbox's pane
 * for an answer just sent, and for one given before this visit.
 *
 * IN THE ORDER THEY WERE OFFERED, the recommended one first, but WITHOUT ITS
 * FILL: drawn chosen and recommended at once, the option carried the
 * accent's ink on the chosen fill, which does not clear contrast. The
 * answer's own line says when the choice was the recommended one.
 */
export function AnsweredChoices({
  options,
  recommended,
  chosen,
}: {
  options: readonly Option[];
  /** The option the asker recommended, if any. */
  recommended?: string | undefined;
  /** The option chosen, by id, or null for an answer in words. */
  chosen: string | null;
}) {
  const first = options.find((o) => o.id === recommended);
  const ordered = first ? [first, ...options.filter((o) => o !== first)] : options;
  return (
    <ChoiceButtons
      label="Answers"
      options={choiceOptions(ordered)}
      chosen={chosen}
      disabledReason="This question is answered."
      onChoose={() => undefined}
    />
  );
}
