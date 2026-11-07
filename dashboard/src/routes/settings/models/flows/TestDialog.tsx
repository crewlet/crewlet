/**
 * Test a model: the engine runs the harness for it as `crewlet llm doctor` does on
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
import {
  Button,
  Callout,
  Disclosure,
  Modal,
  Spinner,
  StatusDot,
  Text,
  VisuallyHidden,
} from "@crewlethq/ui";
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
 * signs in with (design QA Minor 19): a key is a key and a token a token. An
 * auth failure the harness reported of a pasted key or token it never used
 * (`meaning` "unread") says so, since the vendor did not refuse it.
 */
export function failureWords(
  kind: string | undefined,
  signIn: SignInFacts,
  meaning?: CheckLine["meaning"],
): string {
  const route = signIn.route;
  switch (kind) {
    case "auth": {
      // ONLY OF A CREDENTIAL THE ENGINE HANDS THE HARNESS: a host login that
      // says it is not logged in is not logged in.
      if (meaning === "unread" && route === "api-key") return "The harness did not use the API key";
      if (meaning === "unread" && route === "token")
        return "The harness did not use the subscription token";
      if (route === "token" && signIn.expiresAt && Date.parse(signIn.expiresAt) <= Date.now())
        return "The subscription token expired";
      if (route === "api-key") return "The API key was refused";
      if (route === "token") return "The subscription token was refused";
      if (route === "credentials") return "The harness's login on the host was refused";
      if (route === "env") return "The cloud provider or gateway refused the sign-in";
      return "The sign-in was refused";
    }
    case "rate_limit":
      return "A usage limit stopped the call";
    case "timeout":
      return "The harness did not answer in time";
    // THE ENGINE'S KINDS (`llm.ErrorKind`): `server` is a harness that ran and
    // gave no usable answer, `fatal` one the engine could not start.
    case "server":
      return "The harness gave no usable answer";
    case "fatal":
      return "The engine could not start the harness";
    case "config":
      return "A setting needs changing";
    default:
      return "The test failed";
  }
}

const checkName = (check: string) => CHECK[check as ModelCheckName] ?? check;

/** A sentence as the page writes one: its first letter capitalised. */
const sentenceCase = (s: string) => s.replace(/^\s*\S/, (c) => c.toUpperCase());

/**
 * What the engine said about a check, each sentence once: its problems (the
 * remedy, one per line) and its detail, which for a failed call is often the
 * same verdict again. Compared with their whitespace folded.
 */
export function engineWords(line: CheckLine): string[] {
  const out: string[] = [];
  const seen = new Set<string>();
  for (const said of [...(line.remedy ?? "").split("\n"), line.detail]) {
    const words = said.trim();
    const folded = words.replace(/\s+/g, " ").toLowerCase();
    if (!words || seen.has(folded)) continue;
    seen.add(folded);
    out.push(sentenceCase(words));
  }
  return out;
}

/** Words with their `backticked` spans drawn as code, as the engine writes a command. */
function withCode(words: string) {
  return words.split("`").map((part, i) => (i % 2 === 1 ? <code key={i}>{part}</code> : part));
}

/**
 * A test's lines, one check each, every one led by one sentence in one size:
 * a failure in the page's words, or a passed check's measurement. A failure's
 * own words from the engine sit under Details, each said once.
 */
export function DoctorLines({
  lines,
  signIn,
}: {
  lines: readonly CheckLine[];
  signIn: SignInFacts;
}) {
  return (
    <ul className="models-doctor" aria-label="Test results">
      {lines.map((line) => {
        const said = line.ok ? [] : engineWords(line);
        return (
          <li key={line.check} className="models-doctor__line">
            <StatusDot tone={line.ok ? "success" : "danger"}>
              {checkName(line.check)}
              <VisuallyHidden>{line.ok ? ", passed" : ", failed"}</VisuallyHidden>
            </StatusDot>
            <span className="models-doctor__detail">
              <Text as="span" variant="body" tone={line.ok ? "secondary" : "primary"}>
                {line.ok
                  ? withCode(line.detail)
                  : `${failureWords(line.kind, signIn, line.meaning)}.`}
              </Text>
              {said.length > 0 ? (
                <Disclosure title="Details" size="compact" flush headingLevel="none">
                  <ul className="models-doctor__said">
                    {said.map((words) => (
                      <li key={words}>{withCode(words)}</li>
                    ))}
                  </ul>
                </Disclosure>
              ) : null}
            </span>
          </li>
        );
      })}
    </ul>
  );
}

/** A failed test's headline: the first failing check's own words, so it names what failed. */
export function headline(answer: CheckAnswer, signIn: SignInFacts): string {
  const first = answer.lines.find((l) => !l.ok);
  return first
    ? failureWords(first.kind, signIn, first.meaning)
    : failureWords(answer.failure, signIn);
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
      subtitle="Runs the harness once with this model's sign-in and settings, on the node that answers. It costs a few tokens."
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
          <Text variant="body">Running the harness. This can take up to three minutes.</Text>
        </div>
      ) : (
        <div className="models-stack">
          <Callout
            variant={reading.answer.ok ? "success" : "danger"}
            live="polite"
            title={
              reading.answer.ok ? "Every check passed" : headline(reading.answer, signInOf(model))
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
