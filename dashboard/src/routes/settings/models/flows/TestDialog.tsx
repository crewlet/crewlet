/**
 * Test a model: the engine runs the CLI for it as `crewlet llm doctor` does on
 * the host, with a real call, and the page shows each check in words, the
 * first failure's cause and what to do about it.
 *
 * THE CHECK'S NAME LEADS, THE ENGINE'S MEASUREMENT FOLLOWS (design QA Minor
 * 8). A line's detail is the doctor's own report ("ok: 812 in / 34 out", a
 * binary's path), written for a terminal: it stays, in the quiet ink under the
 * check's plain name, so the reading is "Answers a call, passed" and the
 * figures are there for whoever wants them.
 */

import { useEffect, useState } from "react";
import { Button, Callout, Modal, Spinner, StatusDot, Text, VisuallyHidden } from "@crewlethq/ui";
import type { ModelCheckName, ModelSignInRoute } from "~/contract/models.ts";
import type { CheckAnswer, CheckLine } from "~/protocol/models.ts";
import { checkModel, type ModelAnswer } from "../api.ts";
import type { ModelView } from "../data.ts";
import { CHECK } from "../words.ts";

/**
 * The sign-in a failure is named against: its route and, for a token, when it
 * expires. A model's page reads it off the model; the wizard's test off the
 * draft, which has no stored expiry yet.
 */
export interface SignInFacts {
  route: ModelSignInRoute;
  expiresAt: string | null;
}

/** A model's sign-in, as a failure is named against it. */
export function signInOf(model: ModelView): SignInFacts {
  return {
    route: model.entry.sign_in.route,
    expiresAt: model.admin?.sign_in.expires_at ?? null,
  };
}

/**
 * Why a call failed, by its kind, in the words of the credential the model
 * signs in with (design QA Minor 19): a key is a key and a token a token.
 */
export function failureWords(kind: string | undefined, signIn: SignInFacts): string {
  const route = signIn.route;
  switch (kind) {
    case "auth": {
      if (route === "token" && signIn.expiresAt && Date.parse(signIn.expiresAt) <= Date.now())
        return "The subscription token expired";
      if (route === "api-key") return "The API key was refused";
      if (route === "token") return "The subscription token was refused";
      if (route === "credentials") return "The CLI's login on the host was refused";
      if (route === "env") return "The cloud provider or gateway refused the sign-in";
      return "The sign-in was refused";
    }
    case "rate_limit":
      return "A usage limit stopped the call";
    case "timeout":
      return "The CLI did not answer in time";
    case "server":
      return "The vendor's service failed";
    case "fatal":
      return "The CLI stopped with an error";
    case "config":
      return "A setting needs changing";
    default:
      return "The test failed";
  }
}

const checkName = (check: string) => CHECK[check as ModelCheckName] ?? check;

/** A test's lines, one check each, the failing one with its cause and remedy. */
export function DoctorLines({
  lines,
  signIn,
}: {
  lines: readonly CheckLine[];
  signIn: SignInFacts;
}) {
  return (
    <ul className="models-doctor" aria-label="Test results">
      {lines.map((line) => (
        <li key={line.check} className="models-doctor__line">
          <StatusDot tone={line.ok ? "success" : "danger"}>
            {checkName(line.check)}
            <VisuallyHidden>{line.ok ? ", passed" : ", failed"}</VisuallyHidden>
          </StatusDot>
          <span className="models-doctor__detail">
            {!line.ok ? (
              <Text as="span" variant="body">
                {failureWords(line.kind, signIn)}.{line.remedy ? ` ${line.remedy}` : ""}
              </Text>
            ) : null}
            {line.detail ? (
              <Text as="span" variant="caption" tone="tertiary">
                {line.detail}
              </Text>
            ) : null}
          </span>
        </li>
      ))}
    </ul>
  );
}

type Reading =
  | { kind: "running" }
  | { kind: "answered"; answer: CheckAnswer }
  | { kind: "refused"; sentence: string };

const readingOf = (answer: ModelAnswer<CheckAnswer>): Reading =>
  answer.kind === "ok"
    ? { kind: "answered", answer: answer.value }
    : { kind: "refused", sentence: answer.sentence };

export function TestDialog({
  model,
  onClose,
  onDone,
}: {
  model: ModelView;
  onClose: () => void;
  onDone: () => void;
}) {
  const [reading, setReading] = useState<Reading>({ kind: "running" });
  const [run, setRun] = useState(0);
  const key = model.key;

  useEffect(() => {
    const abort = new AbortController();
    setReading({ kind: "running" });
    checkModel(key, { smoke: true }, abort.signal).then(
      (answer) => {
        setReading(readingOf(answer));
        // A TEST MOVES THE MODEL'S READING (its last answer, a refused
        // sign-in), so the page behind asks again at once.
        if (answer.kind === "ok") onDone();
      },
      () => {
        // Only this dialog's own abort rejects: it closed or ran again.
      },
    );
    return () => abort.abort();
    // `onDone` refreshes the page; a new run is asked for by `run` alone.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key, run]);

  return (
    <Modal
      open
      onClose={onClose}
      title={`Test ${key}`}
      subtitle="Runs the CLI once with this model's sign-in and settings, on the node that answers. It costs a few tokens."
      size="md"
      footer={
        <>
          <Button
            variant="secondary"
            onClick={() => setRun((n) => n + 1)}
            disabled={reading.kind === "running"}
          >
            Test again
          </Button>
          <Button variant="primary" onClick={onClose}>
            Done
          </Button>
        </>
      }
    >
      {reading.kind === "refused" ? (
        <Callout variant="danger" title="The test could not run" live="polite">
          {reading.sentence}
        </Callout>
      ) : reading.kind === "running" ? (
        <div className="models-waiting" role="status">
          <Spinner size="sm" />
          <Text variant="body">Running the CLI. This can take up to three minutes.</Text>
        </div>
      ) : (
        <div className="models-stack">
          <Callout
            variant={reading.answer.ok ? "success" : "danger"}
            live="polite"
            title={
              reading.answer.ok
                ? "Every check passed"
                : failureWords(reading.answer.failure, signInOf(model))
            }
          >
            {reading.answer.ok
              ? `The model signs in, answers, and runs with the tools it is allowed, on ${reading.answer.node}.`
              : `Calls to this model fail this way on ${reading.answer.node} until it is fixed. Seats move to their backups meanwhile.`}
          </Callout>
          <DoctorLines lines={reading.answer.lines} signIn={signInOf(model)} />
        </div>
      )}
    </Modal>
  );
}
