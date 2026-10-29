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

import { useEffect, useRef, useState } from "react";
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
import type { ModelDeviceStatus } from "~/contract/models.ts";
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

/** The states a session ends in; every other one is still moving. */
const ENDED: readonly ModelDeviceStatus[] = ["approved", "failed", "expired", "cancelled"];
export const deviceEnded = (status: ModelDeviceStatus) => ENDED.includes(status);

/** The host a sign-in address points at, for the button that opens it. */
function hostOf(url: string): string | null {
  try {
    return new URL(url).host;
  } catch {
    return null;
  }
}

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
  const [error, setError] = useState<string | null>(null);
  const [attempt, setAttempt] = useState(0);
  const [cancelling, setCancelling] = useState(false);
  const approved = useRef(false);
  const openRef = useRef<HTMLAnchorElement>(null);
  const key = model.key;

  // START, unless the reading already follows a session for this model.
  useEffect(() => {
    if (session) return;
    const abort = new AbortController();
    setError(null);
    startDeviceSignIn(key, abort.signal).then(
      (answer) => {
        if (answer.kind === "ok") setSession(answer.value);
        else if (answer.code === "sign_in_in_progress" && isDevice(answer.body.running))
          setSession(answer.body.running);
        else setError(answer.sentence);
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

  useEffect(() => {
    if (session?.status !== "approved" || approved.current) return;
    approved.current = true;
    onDone();
    toast.ok(`${key} is signed in.`);
  }, [session?.status, key, onDone, toast]);

  // THE ADDRESS TAKES THE FOCUS when it arrives (design QA Minor 11): it is
  // the next thing to do, and Cancel, which the dialog opened on while the
  // sign-in started, is not.
  const focused = useRef(false);
  const hasLink = Boolean(session?.url && !deviceEnded(session.status));
  useEffect(() => {
    if (!hasLink || focused.current) return;
    focused.current = true;
    openRef.current?.focus();
  }, [hasLink]);

  async function cancel() {
    if (!session || !live) {
      onClose();
      return;
    }
    setCancelling(true);
    const answer = await cancelDeviceSignIn(session.id).catch(() => null);
    setCancelling(false);
    if (answer && answer.kind === "refused") {
      setError(answer.sentence);
      return;
    }
    onDone();
    onClose();
  }

  function again() {
    approved.current = false;
    focused.current = false;
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
            {ended ? (
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
      {error ? (
        <Callout variant="danger" title="The sign-in could not start" live="polite">
          {error}
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
          <div className="models-device__status" role="status">
            <StatusDot tone={DEVICE[session.status].tone} pulse>
              {session.status === "pending"
                ? "Waiting for you to approve the code"
                : DEVICE[session.status].label}
            </StatusDot>
          </div>
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
