/**
 * Sign a Codex model in with a device code: the engine runs Codex's own
 * device sign-in for the model and answers only the address and the one-time
 * code. The person approves on OpenAI's page; Codex keeps the login. No
 * password, token or callback passes through Crewlet.
 *
 * ONE SIGN-IN PER MODEL ACROSS THE FLEET. A start while one runs is refused
 * with the running one (`sign_in_in_progress`), and so is a model whose
 * reading already carries one: the dialog follows that session rather than
 * leaving it orphaned beside a second.
 *
 * APPROVAL REFRESHES THE PAGE AT ONCE (design QA M4): the model's reading is
 * the engine's, so the page behind asks again the moment the session says
 * approved, and stops saying the sign-in was refused.
 */

import { useEffect, useLayoutEffect, useRef, useState } from "react";
import {
  Button,
  ButtonLink,
  Callout,
  Copyable,
  Modal,
  RelativeTime,
  Spinner,
  StatusDot,
  Text,
  useToast,
} from "@crewlethq/ui";
import type { QueryErrorCode } from "~/contract/errors.ts";
import { DEVICE_SIGN_IN_ENDED, type ModelDeviceStatus } from "~/contract/models.ts";
import { useLifetimeSignal } from "~/lib/useLifetimeSignal.ts";
import { useQuery } from "~/lib/useQuery.ts";
import type { ModelDevice } from "~/protocol/models.ts";
import { cancelDeviceSignIn, startDeviceSignIn } from "../api.ts";
import type { ModelView } from "../data.ts";
import { DEVICE } from "../words.ts";

/**
 * How often the page asks whether the code was approved: a person types an
 * eight-character code, and 3 s keeps the wait after it short at one cheap
 * coordination read (design §5.1).
 */
export const DEVICE_POLL_MS = 3_000;

/** Whether a session has ended, by the engine's own set; every other status is still moving. */
export const deviceEnded = (status: ModelDeviceStatus) =>
  (DEVICE_SIGN_IN_ENDED as readonly ModelDeviceStatus[]).includes(status);

/**
 * The host a sign-in address points at, for the button that opens it, or
 * null where it is not an https address. The address is the engine's, read
 * off the CLI's output, and the page opens it in a new tab: only a secure
 * one is offered, so a CLI's output can never hand a person a plain-http or
 * script address to follow.
 */
function hostOf(url: string): string | null {
  try {
    const parsed = new URL(url);
    return parsed.protocol === "https:" ? parsed.host : null;
  } catch {
    return null;
  }
}

/**
 * The poll's answers that end following a session: it is gone, or no longer
 * this reader's to see. Every other failure is a moment's, and the poll asks
 * again.
 */
const LOST: readonly QueryErrorCode[] = ["not_found", "unauthorized"];

/** What went wrong, by what the person was doing: each has its own title and its own way on. */
type Problem = { kind: "start" | "cancel"; sentence: string };

export function DeviceDialog({
  model,
  onClose,
  onDone,
}: {
  model: ModelView;
  onClose: () => void;
  onDone: () => void;
}) {
  const toast = useToast();
  const running = model.admin?.device_sign_in ?? null;
  const [session, setSession] = useState<ModelDevice | null>(
    running && !deviceEnded(running.status) ? running : null,
  );
  const [problem, setProblem] = useState<Problem | null>(null);
  const [attempt, setAttempt] = useState(0);
  const [cancelling, setCancelling] = useState(false);
  const approved = useRef(false);
  const openRef = useRef<HTMLAnchorElement>(null);
  const lifetime = useLifetimeSignal();
  const key = model.key;

  // START, unless the reading already follows a session for this model.
  useEffect(() => {
    if (session) return;
    const abort = new AbortController();
    setProblem(null);
    startDeviceSignIn(key, abort.signal).then(
      (answer) => {
        // AN ANSWER THAT LANDS AFTER THE DIALOG LEFT is not this dialog's to show.
        if (abort.signal.aborted) return;
        if (answer.kind === "ok") setSession(answer.value);
        else if (answer.code === "sign_in_in_progress" && isDevice(answer.body.running))
          setSession(answer.body.running);
        else setProblem({ kind: "start", sentence: answer.sentence });
      },
      () => {
        // Only this dialog's own abort rejects.
      },
    );
    return () => abort.abort();
    // A new start is asked for by `attempt` alone.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key, attempt]);

  const live = session !== null && !deviceEnded(session.status);
  const polled = useQuery(
    "llm_device_sign_in",
    { id: session?.id ?? "" },
    { enabled: live, pollMs: DEVICE_POLL_MS },
  );
  useEffect(() => {
    if (polled.data && polled.data.id === session?.id) setSession(polled.data);
  }, [polled.data, session?.id]);
  // A SESSION THE ENGINE NO LONGER ANSWERS FOR (gone, or no longer this
  // reader's to see) is said, with the way on, rather than drawn as waiting
  // for an approval that can never be read back. Anything else (a node not
  // ready yet, a slow answer, a dropped socket) passes: the status stays and
  // the next poll asks again.
  const lost =
    live && polled.error && LOST.includes(polled.error)
      ? polled.detail || "The engine no longer answers for it."
      : null;

  useEffect(() => {
    if (session?.status !== "approved" || approved.current) return;
    approved.current = true;
    onDone();
    toast.ok(`${key} is signed in.`);
  }, [session?.status, key, onDone, toast]);

  // THE ADDRESS TAKES THE FOCUS when it arrives (design QA Minor 11): it is
  // the next thing to do, and Cancel, which the dialog opened on while the
  // sign-in started, is not. IN THE COMMIT THAT DRAWS IT, as a layout effect:
  // a passive one ran after the commit, so for a moment the address was on
  // the page without the focus, and a key pressed then went to Cancel. It
  // depends on the address alone, never on whether a poll has answered.
  const focused = useRef(false);
  const hasLink = Boolean(session?.url && !deviceEnded(session.status));
  useLayoutEffect(() => {
    if (!hasLink || focused.current) return;
    focused.current = true;
    openRef.current?.focus();
  }, [hasLink]);

  async function cancel() {
    if (!session || !live) {
      onClose();
      return;
    }
    // A CANCEL THAT HAS STARTED FINISHES, so the session is ended rather
    // than left in doubt; an answer that lands after the dialog closed acts
    // for nothing on screen.
    const screen = lifetime();
    setCancelling(true);
    const answer = await cancelDeviceSignIn(session.id);
    if (screen.aborted) return;
    setCancelling(false);
    if (answer.kind === "refused") {
      setProblem({ kind: "cancel", sentence: answer.sentence });
      return;
    }
    onDone();
    onClose();
  }

  function again() {
    approved.current = false;
    focused.current = false;
    setProblem(null);
    setSession(null);
    setAttempt((n) => n + 1);
  }

  const ended = session !== null && deviceEnded(session.status);
  const host = session?.url ? hostOf(session.url) : null;

  return (
    <Modal
      open
      // CLOSING IS NOT CANCELLING: a session left running still signs the
      // model in when approved, and the model's page offers it again.
      onClose={onClose}
      title={`Sign ${key} in to ChatGPT`}
      subtitle="Codex's own device sign-in. You approve it on OpenAI's site."
      size="md"
      initialFocus={() => openRef.current}
      footer={
        session?.status === "approved" ? (
          <Button variant="primary" onClick={onClose}>
            Done
          </Button>
        ) : (
          <>
            {ended || problem?.kind === "start" || lost ? (
              <Button variant="primary" onClick={again}>
                Start again
              </Button>
            ) : null}
            <Button variant="secondary" loading={cancelling} onClick={() => void cancel()}>
              {live ? "Cancel sign-in" : "Close"}
            </Button>
          </>
        )
      }
    >
      {problem?.kind === "start" ? (
        <Callout variant="danger" title="The sign-in could not start" live="polite">
          {problem.sentence}
        </Callout>
      ) : !session ? (
        <div className="models-waiting" role="status">
          <Spinner size="sm" />
          <Text variant="body">Starting Codex's device sign-in. This takes up to 20 seconds.</Text>
        </div>
      ) : session.status === "approved" ? (
        <Callout variant="success" title="Signed in" live="polite">
          {key} signs in with ChatGPT. Codex keeps the login on {session.node} and refreshes it
          itself, and every node restores it.
        </Callout>
      ) : ended ? (
        <Callout
          variant={DEVICE[session.status].tone === "danger" ? "danger" : "warning"}
          title={DEVICE[session.status].label}
          live="polite"
        >
          {session.detail ||
            (session.status === "expired"
              ? "Nobody approved the code before it expired. Start again for a new one."
              : "Nothing was signed in. Start again for a new code.")}
        </Callout>
      ) : (
        <div className="models-device">
          {problem?.kind === "cancel" ? (
            <Callout variant="danger" title="The sign-in could not be cancelled" live="polite">
              {problem.sentence} It is still running, and still signs the model in if approved.
            </Callout>
          ) : null}
          {lost ? (
            <Callout variant="danger" title="This sign-in can no longer be followed" live="polite">
              {lost} Start again for a new code.
            </Callout>
          ) : null}
          <ol className="models-steps">
            <li>
              <Text as="p" variant="body">
                Open OpenAI's sign-in page and sign in with the ChatGPT account this model should
                use.
              </Text>
              {session.url && host ? (
                <ButtonLink
                  ref={openRef}
                  href={session.url}
                  external
                  size="small"
                  variant="secondary"
                >
                  Open {host}
                </ButtonLink>
              ) : session.url ? (
                <Text as="p" variant="caption" tone="secondary">
                  The sign-in address is not a secure (https) address, so it is not offered.
                </Text>
              ) : (
                <Text as="p" variant="caption" tone="tertiary">
                  Waiting for the address.
                </Text>
              )}
            </li>
            <li>
              <Text as="p" variant="body">
                Enter this code when it asks:
              </Text>
              {session.code ? (
                <Copyable
                  value={session.code}
                  variant="block"
                  monospace
                  ariaLabel="Copy the code"
                  className="models-copy-block"
                />
              ) : (
                <Text as="p" variant="caption" tone="tertiary">
                  Waiting for the code.
                </Text>
              )}
              <Text as="p" variant="caption" tone="tertiary">
                It expires <RelativeTime value={session.expires_at} />.
              </Text>
            </li>
          </ol>
          {lost ? null : (
            <div className="models-device__status" role="status">
              <StatusDot tone={DEVICE[session.status].tone} pulse>
                {session.status === "pending"
                  ? "Waiting for you to approve the code"
                  : DEVICE[session.status].label}
              </StatusDot>
            </div>
          )}
          <Text as="p" variant="caption" tone="secondary" className="models-device__note">
            Device sign-in has to be turned on in your ChatGPT security settings first. Signing in
            with a ChatGPT plan uses its included usage; an OpenAI API key is billed per token
            instead.
          </Text>
        </div>
      )}
    </Modal>
  );
}

function isDevice(value: unknown): value is ModelDevice {
  return (
    typeof value === "object" &&
    value !== null &&
    typeof (value as ModelDevice).id === "string" &&
    typeof (value as ModelDevice).status === "string"
  );
}
