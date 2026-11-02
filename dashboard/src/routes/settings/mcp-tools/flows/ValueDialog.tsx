/**
 * Replace a header or variable's value, or set one the server is sent and
 * the store does not hold (`PUT /mcp-servers/{name}/values/{key}`).
 *
 * WRITE-ONLY. The field takes a new value and nothing shows the old one: the
 * engine seals it on a row no reveal answers, decides the variable it is
 * sealed under, and points the key at it (or re-activates the revision that
 * already reads it), so the running server restarts with it.
 *
 * TESTED FIRST. The engine tests the server as it would run with the value,
 * where it runs, and a value the server refuses stores nothing: the refusal
 * is said on the field, with what the test found under it. Where no node
 * answered the test, the person may store it untested, as they may for a
 * per-agent server's own value, which is tested as each agent when that
 * agent's instance starts.
 *
 * A RECENT STEP-UP is asked by the engine (`step_up_required`), which the
 * dashboard's one ceremony answers and replays.
 */

import { useState } from "react";
import { Button, Callout, Disclosure, FormField, Input, Modal, useToast } from "@crewlethq/ui";
import type {
  McpMemberServerStatus,
  McpValueAnswer,
  McpValueRefusal,
  McpValueState,
} from "~/contract/mcp.ts";
import { storeValue } from "../api.ts";
import { TestResult } from "./TestDialog.tsx";

/** What a store came to, beyond a plain success. */
type Outcome =
  | { kind: "idle" }
  | { kind: "refused"; message: string; check: McpValueRefusal["check"] | null }
  /** No node answered the test: the value may be stored untested. */
  | { kind: "unanswered"; message: string };

/** What the toast says of a stored value, by whether its revision is active. */
function storedWords(answer: McpValueAnswer): { variant: "success" | "warning"; message: string } {
  if (answer.reload?.error) {
    return {
      variant: "warning",
      message: `It is sealed as ${answer.variable}, and the revision that reads it is not active yet: ${answer.reload.detail ?? answer.reload.error}`,
    };
  }
  return {
    variant: "success",
    message: answer.tested
      ? "Tested and sealed. The server restarts with it, and each agent's next turn uses it."
      : "Sealed untested. The server restarts with it, and each agent's next turn uses it.",
  };
}

export function ValueDialog({
  server,
  valueKey,
  value,
  onClose,
  onDone,
}: {
  server: McpMemberServerStatus;
  valueKey: string;
  /** The key as the server's answer lists it; null where it lists no such key yet. */
  value: McpValueState | null;
  onClose: () => void;
  onDone: () => void;
}) {
  const toast = useToast();
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const [outcome, setOutcome] = useState<Outcome>({ kind: "idle" });
  // A PER-AGENT SERVER'S OWN VALUE is tested as each agent, so it is stored untested.
  const untested = !server.shared;

  const save = async (test: boolean) => {
    setBusy(true);
    setOutcome({ kind: "idle" });
    const answer = await storeValue(server.name, valueKey, text, { test });
    if (answer.kind === "ok") {
      const words = storedWords(answer.value);
      toast.show({
        variant: words.variant,
        title: `${valueKey} ${value?.set ? "replaced" : "stored"}`,
        message: words.message,
      });
      onDone();
      onClose();
      return;
    }
    setBusy(false);
    if (answer.code === "value_test_failed") {
      const body = answer.body as Partial<McpValueRefusal>;
      setOutcome({ kind: "refused", message: answer.sentence, check: body.check ?? null });
    } else if (answer.status === 503) {
      setOutcome({ kind: "unanswered", message: answer.sentence });
    } else if (answer.body.stored === true) {
      // SEALED, AND THE CONFIGURATION NOT POINTED AT IT: sending it again finishes the change.
      setOutcome({
        kind: "refused",
        message: `${answer.sentence} The value is sealed; send it again to finish.`,
        check: null,
      });
    } else {
      setOutcome({ kind: "refused", message: answer.sentence, check: null });
    }
  };

  return (
    <Modal
      open
      onClose={onClose}
      title={`${value?.set ? "Replace" : "Set"} ${valueKey}`}
      subtitle={server.title}
      size="md"
      onSubmit={() => void save(!untested)}
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          {outcome.kind === "unanswered" ? (
            <Button
              variant="secondary"
              loading={busy}
              disabled={text.trim().length === 0}
              onClick={() => void save(false)}
            >
              Save untested
            </Button>
          ) : null}
          <Button
            variant="primary"
            type="submit"
            loading={busy && outcome.kind !== "unanswered"}
            disabled={text.trim().length === 0}
          >
            {untested ? "Save" : "Test and save"}
          </Button>
        </>
      }
    >
      <div className="mcp-stack">
        {/* The title names the header or variable, so the label need not, and
            a label in capitals would rewrite its name. One line says what
            happens to the value; the toast says when it applies. */}
        <FormField
          label="New value"
          helper={
            untested
              ? `Each agent's instance is started with it. The old value is kept until you save.`
              : `Tested against ${server.title} first. The old value stays if it is refused.`
          }
          error={outcome.kind === "refused" ? outcome.message : undefined}
        >
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              aria-invalid={field.invalid || undefined}
              error={field.invalid}
              type="password"
              autoComplete="new-password"
              spellCheck={false}
              placeholder={value?.set ? "Stored. Type a new value to replace it" : undefined}
              value={text}
              onChange={(e) => setText(e.target.value)}
            />
          )}
        </FormField>
        {outcome.kind === "refused" && outcome.check ? (
          <Disclosure title="What the test found" size="compact" headingLevel="none" flush>
            <TestResult answer={outcome.check} />
          </Disclosure>
        ) : null}
        {outcome.kind === "unanswered" ? (
          <Callout variant="warning" title="No node answered the test">
            {outcome.message} Nothing was stored. Save it untested to store it anyway.
          </Callout>
        ) : null}
      </div>
    </Modal>
  );
}
