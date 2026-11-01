/**
 * The one step-up ceremony: confirm who you are, and the refused request goes
 * through.
 *
 * # One dialog, and no screen draws its own
 *
 * The engine asks for a fresh proof of identity before the gestures that
 * change what a company is — its configuration, its credentials, how somebody
 * signs in, who is in its directory — and refuses them `403
 * step_up_required`, naming its one window (`api.auth.session.step_up`).
 * Every screen's writes go through `protocol/rest.ts`, which asks here and
 * REPLAYS the refused request once the person has confirmed, so a form being
 * saved is saved rather than lost to a refusal. Mounted once, beside the
 * frame and outside it, because the enrolment screen asks for a step-up too
 * and it is drawn outside the frame.
 *
 * # What confirming does to the session
 *
 * `POST /auth/step-up` ENDS the session it was given and answers a fresh one,
 * whose cookie the browser now holds. The engine closes every socket the old
 * session opened `4401`, and the socket dials again once this tab's requests
 * — the step-up's own answer among them — have settled, with the new cookie
 * (`protocol/socket.ts`). Nothing here re-dials it.
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
import { STILL_WORKING } from "~/lib/waiting.ts";
import { useRest } from "~/lib/useRest.ts";
import { auth, currentSessionNeed, rest, RestError, setStepUpConfirmer } from "~/protocol/index.ts";

/**
 * Whether this person holds an authenticator app — the second factor the
 * engine asks a step-up's code for — read from their own credentials (`GET
 * /iam/credentials`, which is the Account page's read), or null while that is
 * not known.
 *
 * ASKED, because the dialog said "Code (optional)" to somebody who holds one,
 * and a password-only Confirm was then refused for the code it had said was
 * optional. Unknown — the read failed, or a session that may only enrol a
 * factor is refused it — stays optional, and the engine's refusal says what it
 * wants.
 *
 * `factor` is whether they hold ANY second factor, the app or recovery codes
 * alone: holding neither, the Code field is not drawn at all, since most
 * people hold no factor and a field they can never fill is one too many.
 */
function useSecondFactor(): { app: boolean; factor: boolean } | null {
  const read = useRest("/iam/credentials", async (signal) => {
    const answer = (await rest.get("/iam/credentials", signal)) as {
      credentials?: { method?: string; revoked?: boolean }[] | null;
    } | null;
    const live = (answer?.credentials ?? []).filter((c) => !c.revoked);
    const app = live.some((c) => c.method === "totp");
    return { app, factor: app || live.some((c) => c.method === "recovery") };
  });
  return read.data;
}

/**
 * The login this browser is signed in as, or "" until it is known: the
 * account a password manager files the confirmation under.
 *
 * READ HERE, not from the frame's viewer, because the host is mounted outside
 * the frame (the enrolment screen asks for a step-up too).
 */
function useSignedInLogin(): string {
  const read = useRest("/auth/session", async (signal) => (await auth.session(signal)).login);
  return read.data ?? "";
}

export function StepUpHost() {
  const [asking, setAsking] = useState(false);
  // THE ANSWER THE TRANSPORT IS WAITING ON. A ref rather than state, because
  // settling it is a side effect of closing the dialog, not something to
  // render — and it must be settled whichever way the dialog goes, the host
  // unmounting included, or the request behind it waits for ever.
  const settle = useRef<((confirmed: boolean) => void) | null>(null);

  useEffect(() => {
    const uninstall = setStepUpConfirmer(
      () =>
        new Promise<boolean>((resolve) => {
          settle.current = resolve;
          setAsking(true);
        }),
    );
    return () => {
      uninstall();
      settle.current?.(false);
      settle.current = null;
    };
  }, []);

  if (!asking) return null;

  const done = (confirmed: boolean) => {
    settle.current?.(confirmed);
    settle.current = null;
    setAsking(false);
  };

  return <StepUpDialog onDone={done} />;
}

function StepUpDialog({ onDone }: { onDone: (confirmed: boolean) => void }) {
  const [password, setPassword] = useState("");
  const [code, setCode] = useState("");
  const [busy, setBusy] = useState(false);
  const [refusal, setRefusal] = useState<string | null>(null);
  const held = useSecondFactor();
  const login = useSignedInLogin();
  const needsCode = held?.app === true;
  // UNKNOWN STAYS DRAWN, optional: the engine's refusal says what it wants.
  const showCode = held === null || held.factor;
  const ready = password !== "" && (!needsCode || code.trim() !== "");

  async function confirm() {
    if (busy || !ready) return;
    const withCode = code.trim() !== "";
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
      // A CONFIRMATION, NOT A SIGN-IN: the engine answers a wrong password
      // and a wrong code with the sign-in's one refusal ("Those sign-in
      // details were not accepted"), worded for a form this person is not
      // on. Said here as the confirmation it was — still one sentence for
      // either, since which of the two was wrong is not the engine's to say.
      setRefusal(
        err instanceof RestError && err.code === "sign_in_refused"
          ? withCode || needsCode
            ? "Wrong password or code. Try again."
            : "Wrong password. Try again."
          : refusalText(err),
      );
    } finally {
      setBusy(false);
    }
  }

  return (
    <Modal
      open
      title="Confirm it’s you"
      icon={<ShieldUserGlyph size="md" />}
      onClose={() => onDone(false)}
      dismissable={!busy}
      closeDisabledReason={STILL_WORKING}
      size="sm"
      stackBody
      onSubmit={() => void confirm()}
      footer={
        <>
          <Button variant="ghost" onClick={() => onDone(false)} disabled={busy}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" disabled={busy || !ready}>
            {busy ? "Confirming" : "Confirm"}
          </Button>
        </>
      }
    >
      {/* THE ACCOUNT THIS PASSWORD BELONGS TO, for a password manager. A
          password form with no username field reads to one as a new login,
          and it offers to save it. Bitwarden's offer is a bar over the top
          right of the page that renders blank here, so it sat invisible over
          the assistant's drawer and took every press there. Named, the
          confirmation matches the login the manager already holds. */}
      <input hidden readOnly type="text" name="username" autoComplete="username" value={login} />
      <Text as="p" variant="body">
        Enter your password{needsCode ? " and a code from your app" : ""} to continue.
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
      {showCode && (
        <FormField
          label="Code"
          optional={!needsCode}
          helper="The current code from your app, or a recovery code."
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
      )}
      {refusal && (
        <Callout variant="danger" role="alert">
          {refusal}
        </Callout>
      )}
    </Modal>
  );
}
