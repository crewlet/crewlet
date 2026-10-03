/**
 * Enrolling a second factor, and the recovery codes that stand in for it.
 *
 * ONE COMPONENT FOR BOTH DOORS: the enrolment a deployment REQUIRES before a
 * password session may do anything else (`#/enrol`), and the one a person
 * opens from their own menu to add or replace an authenticator. The steps are
 * the engine's and identical either way; only what surrounds them differs.
 *
 * # Two requests, and nothing is stored until the second
 *
 * The first `POST /auth/totp` answers a SEED and stores nothing. The second
 * sends the seed back with a code the authenticator derived from it — the
 * only evidence that the app on the other side works — and only then is the
 * factor stored. A person who closes this page between the two has enrolled
 * nothing, which is the right outcome rather than a factor that locks them
 * out.
 *
 * # A value the engine just minted is shown once, here, and never again
 *
 * The seed and the recovery codes are the one place this dashboard renders a
 * credential, and that is the design rule amended rather than broken (see the
 * design document's "No screen renders a credential"): the engine answers
 * each exactly once, in the response that minted it, and no route returns it
 * again. A reload mints a fresh seed and never re-reads the old one.
 *
 * NO QR CODE. Drawing one needs an encoder this product does not carry, and
 * the two things it would carry are here already: the `otpauth://` link,
 * which an authenticator on the same device opens, and the key itself, typed
 * into one that is not.
 */

import { useCallback, useEffect, useRef, useState } from "react";
import { Button, Callout, FormField, Input, Modal, Skeleton, Text } from "@crewlethq/ui";
import { KeyGlyph, ShieldUserGlyph } from "@crewlethq/icons/glyphs";
import { CopyButton, DownloadButton } from "~/ui/primitives.tsx";
import { refusalText } from "~/lib/refusal.ts";
import { goSignIn } from "~/lib/session.ts";
import {
  auth,
  RestError,
  type SecondFactorEnrolled,
  type SecondFactorSeed,
} from "~/protocol/index.ts";

/** A base32 key in groups of four, which is how an app asks for it to be typed. */
export function groupedKey(secret: string): string {
  return (secret.match(/.{1,4}/g) ?? []).join(" ");
}

export function SecondFactorSetup({
  onEnrolled,
}: {
  onEnrolled: (answer: SecondFactorEnrolled) => void;
}) {
  const [seed, setSeed] = useState<SecondFactorSeed | null>(null);
  const [unread, setUnread] = useState<string | null>(null);
  const [code, setCode] = useState("");
  const [busy, setBusy] = useState(false);
  const [refusal, setRefusal] = useState<string | null>(null);
  // WHETHER THE ENGINE SAID A FACTOR IS ALREADY HELD, which a session opened
  // on a password alone may not replace: the way on is to sign in with it.
  const [held, setHeld] = useState(false);

  const offer = useCallback(async () => {
    setUnread(null);
    try {
      setSeed(await auth.secondFactorSeed());
    } catch (err) {
      setUnread(refusalText(err));
    }
  }, []);

  // ONCE PER MOUNT, even under a development build's double effect: every
  // offer is a fresh seed, and the one on screen must be the one the code is
  // checked against.
  const asked = useRef(false);
  useEffect(() => {
    if (asked.current) return;
    asked.current = true;
    void offer();
  }, [offer]);

  async function confirm() {
    if (!seed || busy || code.trim() === "") return;
    setBusy(true);
    setRefusal(null);
    try {
      onEnrolled(await auth.enrolSecondFactor(seed.secret, code.trim()));
    } catch (err) {
      setHeld(err instanceof RestError && err.code === "second_factor_required");
      setCode("");
      setRefusal(refusalText(err));
    } finally {
      setBusy(false);
    }
  }

  if (unread) {
    return (
      <Callout
        variant="danger"
        role="alert"
        action={
          <Button size="small" variant="secondary" onClick={() => void offer()}>
            Try again
          </Button>
        }
      >
        {unread} Nothing has been set up yet.
      </Callout>
    );
  }
  if (!seed) return <Skeleton variant="text" rows={4} label="Preparing a key" />;

  return (
    <form
      className="signin-form"
      onSubmit={(e) => {
        e.preventDefault();
        void confirm();
      }}
    >
      <ol className="signin-steps">
        <li>
          <Text as="p" variant="body">
            Add an account to your authenticator app: on this device,{" "}
            <a className="prose-link" href={seed.uri}>
              open it in the app
            </a>
            ; on another, type this key into it.
          </Text>
          <div className="signin-row">
            <code className="inline">{groupedKey(seed.secret)}</code>
            <CopyButton text={seed.secret} label="Copy key" variant="ghost" />
          </div>
        </li>
        <li>
          <FormField
            label="The six-digit code the app now shows"
            helper="It changes every thirty seconds; any current one works."
          >
            {(field) => (
              <Input
                id={field.id}
                aria-describedby={field.describedBy}
                autoComplete="one-time-code"
                inputMode="numeric"
                spellCheck={false}
                width="sm"
                value={code}
                onChange={(e) => setCode(e.target.value)}
              />
            )}
          </FormField>
        </li>
      </ol>
      {refusal && (
        <Callout
          variant="danger"
          role="alert"
          action={
            held ? (
              <Button size="small" variant="secondary" onClick={goSignIn}>
                Sign in again
              </Button>
            ) : undefined
          }
        >
          {refusal}
        </Callout>
      )}
      <Button type="submit" variant="primary" disabled={busy || code.trim() === ""}>
        {busy ? "Checking" : "Confirm"}
      </Button>
    </form>
  );
}

/**
 * A set of recovery codes, shown the once the engine answers them.
 *
 * COPIED OR SAVED WHOLE, one per line, because a person keeps them in one
 * place — a password manager's note, a file, a printed page — and ten
 * separate copies is ten chances to miss one.
 */
export function RecoveryCodeList({ codes }: { codes: readonly string[] }) {
  const text = codes.join("\n");
  return (
    <div className="signin-codes">
      <ul className="signin-code-list" aria-label="Recovery codes">
        {codes.map((code) => (
          <li key={code}>
            <code className="inline">{code}</code>
          </li>
        ))}
      </ul>
      <div className="signin-row">
        <CopyButton text={text} label="Copy all" />
        <DownloadButton
          text={text}
          filename="crewlet-recovery-codes.txt"
          mime="text/plain;charset=utf-8"
          label="Download"
        />
      </div>
    </div>
  );
}

/**
 * The recovery codes a first enrolment issues, and the one step after it.
 *
 * ISSUED WITHOUT BEING ASKED, because a person who has just enrolled their
 * first authenticator holds no other way back in: a phone lost next week is
 * an account nobody but an administrator can reopen. A set that cannot be
 * issued is said, with the way to try again, and the way on without it —
 * the codes can be issued later from the person's own menu.
 */
export function FirstRecoveryCodes({ onDone }: { onDone: () => void }) {
  const [codes, setCodes] = useState<string[] | null>(null);
  const [refusal, setRefusal] = useState<string | null>(null);

  const issue = useCallback(async () => {
    setRefusal(null);
    try {
      setCodes((await auth.recoveryCodes()).codes);
    } catch (err) {
      setRefusal(refusalText(err));
    }
  }, []);

  const asked = useRef(false);
  useEffect(() => {
    if (asked.current) return;
    asked.current = true;
    void issue();
  }, [issue]);

  if (refusal) {
    return (
      <div className="signin-form">
        <Callout variant="warning" role="alert">
          {refusal} Your authenticator is set up; these codes are the way back in if you lose it.
        </Callout>
        <Button variant="primary" onClick={() => void issue()}>
          Try again
        </Button>
        <Button variant="ghost" onClick={onDone}>
          Continue without recovery codes
        </Button>
      </div>
    );
  }
  if (!codes) return <Skeleton variant="text" rows={4} label="Issuing recovery codes" />;
  return (
    <div className="signin-form">
      <Text as="p" variant="body">
        Your authenticator is set up. Keep these recovery codes somewhere safe: each one signs you
        in once in place of a code from the app. <strong>They are shown this once</strong> — the
        engine keeps only a fingerprint of each and cannot show them again.
      </Text>
      <RecoveryCodeList codes={codes} />
      <Button variant="primary" onClick={onDone}>
        I have saved them — continue
      </Button>
    </div>
  );
}

/**
 * Adding or replacing an authenticator, from a person's own menu.
 *
 * THE ENGINE ASKS FOR A FRESH PROOF FIRST — the sensitive window, because this
 * is the gesture that decides whether a stolen session becomes a permanent
 * hold on somebody's account — and the step-up ceremony answers it before the
 * seed arrives, so this dialog only ever shows a seed its reader proved they
 * may have.
 */
export function AuthenticatorDialog({ onClose }: { onClose: () => void }) {
  const [enrolled, setEnrolled] = useState(false);
  return (
    <Modal
      open
      title="Two-step verification"
      icon={<ShieldUserGlyph size="md" />}
      onClose={onClose}
      size="md"
      stackBody
      footer={
        <Button variant={enrolled ? "primary" : "ghost"} onClick={onClose}>
          {enrolled ? "Done" : "Cancel"}
        </Button>
      }
    >
      {enrolled ? (
        <Text as="p" variant="body">
          Your authenticator is set up, and signing in asks for its code from now on. If it replaced
          one you had, codes from the old app no longer work; your recovery codes are unchanged.
        </Text>
      ) : (
        <>
          <Text as="p" variant="body" tone="secondary">
            Signing in will ask for a code from this app as well as your password. If you already
            use one, this replaces it.
          </Text>
          <SecondFactorSetup onEnrolled={() => setEnrolled(true)} />
        </>
      )}
    </Modal>
  );
}

/**
 * Issuing a fresh set of recovery codes, from a person's own menu.
 *
 * ASKED FOR, never issued on opening: a new set RETIRES the old one, so a
 * person who opened this to look and closed it again must still hold the set
 * they came with.
 */
export function RecoveryCodesDialog({ onClose }: { onClose: () => void }) {
  const [codes, setCodes] = useState<string[] | null>(null);
  const [busy, setBusy] = useState(false);
  const [refusal, setRefusal] = useState<string | null>(null);

  async function issue() {
    if (busy) return;
    setBusy(true);
    setRefusal(null);
    try {
      setCodes((await auth.recoveryCodes()).codes);
    } catch (err) {
      // TWO DIFFERENT AFTERMATHS. A refusal stored nothing, so the set held
      // still works; an answer nobody can confirm — the engine's `503`, or no
      // answer at all — may have stored a set this page will never see, which
      // would have retired the one held. Saying "still works" there is the
      // claim a person would find out was false on the day they need it.
      const unknown = err instanceof RestError && (err.status === 503 || err.status === 0);
      setRefusal(
        `${refusalText(err)} ${
          unknown
            ? "It is not known whether a new set was stored, which would retire the one you hold — issue a new set to be sure."
            : "No codes were issued; the set you hold still works."
        }`,
      );
    } finally {
      setBusy(false);
    }
  }

  return (
    <Modal
      open
      title="Recovery codes"
      icon={<KeyGlyph size="md" />}
      onClose={onClose}
      dismissable={!busy}
      closeDisabledReason="Waiting for the engine to answer."
      size="md"
      stackBody
      footer={
        codes ? (
          <Button variant="primary" onClick={onClose}>
            I have saved them
          </Button>
        ) : (
          <>
            <Button variant="ghost" onClick={onClose} disabled={busy}>
              Cancel
            </Button>
            <Button variant="primary" onClick={() => void issue()} disabled={busy}>
              {busy ? "Issuing" : "Issue new codes"}
            </Button>
          </>
        )
      }
    >
      {codes ? (
        <>
          <Text as="p" variant="body">
            Your earlier codes no longer work. Keep these somewhere safe:{" "}
            <strong>they are shown this once</strong>.
          </Text>
          <RecoveryCodeList codes={codes} />
        </>
      ) : (
        <Text as="p" variant="body" tone="secondary">
          Each recovery code signs you in once in place of a code from your authenticator, for the
          day you do not have it. Issuing a new set retires the one you hold now.
        </Text>
      )}
      {refusal && (
        <Callout variant="danger" role="alert">
          {refusal}
        </Callout>
      )}
    </Modal>
  );
}
