/**
 * The one step-up ceremony: confirm who you are, and the refused request goes
 * through.
 *
 * # One dialog, and no screen draws its own
 *
 * The engine asks for a fresh proof of identity before the gestures that
 * change what a company is — its configuration, its credentials, its chart,
 * how somebody signs in — and refuses them `403 step_up_required` naming the
 * window. Every screen's writes go through `protocol/rest.ts`, which asks
 * here and REPLAYS the refused request once the person has confirmed, so a
 * form being saved is saved rather than lost to a refusal. Mounted once,
 * beside the frame and outside it, because the enrolment screen asks for the
 * sensitive window too and it is drawn outside the frame.
 *
 * # What confirming does to the session
 *
 * `POST /auth/step-up` ENDS the session it was given and answers a fresh one,
 * whose cookie the browser now holds. The socket was opened on the old
 * session, so it is re-dialled — the engine would close it within a minute
 * anyway, and a minute of pushes on an ended session is a minute nobody
 * should have to reason about.
 *
 * # When confirming cannot work
 *
 * A session the engine no longer accepts is not confirmed here: its `401` is
 * noted by the transport and the application goes to the sign-in, so this
 * dialog closes and the refused request is reported as refused. The rest —
 * a wrong password, a code, a wait — are the engine's own words in the
 * dialog, which stays open for another try.
 */

import { useEffect, useRef, useState } from "react";
import { Button, Callout, FormField, Input, Modal, Text } from "@crewlethq/ui";
import { ShieldUserGlyph } from "@crewlethq/icons/glyphs";
import { refusalText } from "~/lib/refusal.ts";
import { useClient } from "~/lib/store-hooks.ts";
import {
  auth,
  currentSessionNeed,
  setStepUpConfirmer,
  type StepUpWindow,
} from "~/protocol/index.ts";

/** Why this window is being asked for, in a person's terms. */
function reasonFor(window: StepUpWindow): string {
  return window === "step_up_sensitive"
    ? "This is one of the few changes that need a very recent confirmation: it changes how somebody signs in, or shows a credential."
    : "This change needs you to have confirmed who you are recently.";
}

export function StepUpHost() {
  const { socket } = useClient();
  const [asking, setAsking] = useState<StepUpWindow | null>(null);
  // THE ANSWER THE TRANSPORT IS WAITING ON. A ref rather than state, because
  // settling it is a side effect of closing the dialog, not something to
  // render — and it must be settled whichever way the dialog goes, the host
  // unmounting included, or the request behind it waits for ever.
  const settle = useRef<((confirmed: boolean) => void) | null>(null);

  useEffect(() => {
    const uninstall = setStepUpConfirmer(
      (window) =>
        new Promise<boolean>((resolve) => {
          settle.current = resolve;
          setAsking(window);
        }),
    );
    return () => {
      uninstall();
      settle.current?.(false);
      settle.current = null;
    };
  }, []);

  if (asking === null) return null;

  const done = (confirmed: boolean) => {
    settle.current?.(confirmed);
    settle.current = null;
    setAsking(null);
    if (confirmed) socket.reconnect();
  };

  return <StepUpDialog window={asking} onDone={done} />;
}

function StepUpDialog({
  window,
  onDone,
}: {
  window: StepUpWindow;
  onDone: (confirmed: boolean) => void;
}) {
  const [password, setPassword] = useState("");
  const [code, setCode] = useState("");
  const [busy, setBusy] = useState(false);
  const [refusal, setRefusal] = useState<string | null>(null);

  async function confirm() {
    if (busy || password === "") return;
    setBusy(true);
    setRefusal(null);
    try {
      await auth.stepUp({ password, ...(code.trim() ? { code: code.trim() } : {}) });
      onDone(true);
    } catch (err) {
      // THE SESSION ITSELF WAS REFUSED, which the transport has noted and
      // the application is already acting on: there is nothing left here to
      // confirm.
      if (currentSessionNeed() !== null) {
        onDone(false);
        return;
      }
      setCode("");
      setRefusal(refusalText(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Modal
      open
      title="Confirm it is you"
      icon={<ShieldUserGlyph size="md" />}
      onClose={() => onDone(false)}
      dismissable={!busy}
      closeDisabledReason="Waiting for the engine to answer."
      size="sm"
      stackBody
      onSubmit={() => void confirm()}
      footer={
        <>
          <Button variant="ghost" onClick={() => onDone(false)} disabled={busy}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" disabled={busy || password === ""}>
            {busy ? "Confirming" : "Confirm"}
          </Button>
        </>
      }
    >
      <Text as="p" variant="body">
        {reasonFor(window)} Enter your password to carry on: what you were doing is kept, and sent
        again once you have.
      </Text>
      <FormField label="Password">
        {(field) => (
          <Input
            id={field.id}
            aria-describedby={field.describedBy}
            type="password"
            autoComplete="current-password"
            autoFocus
            width="full"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
          />
        )}
      </FormField>
      <FormField
        label="Code"
        optional
        helper="If you sign in with an authenticator app: its current code, or one of your recovery codes."
      >
        {(field) => (
          <Input
            id={field.id}
            aria-describedby={field.describedBy}
            autoComplete="one-time-code"
            spellCheck={false}
            width="full"
            value={code}
            onChange={(e) => setCode(e.target.value)}
          />
        )}
      </FormField>
      {refusal && (
        <Callout variant="danger" role="alert">
          {refusal}
        </Callout>
      )}
    </Modal>
  );
}
