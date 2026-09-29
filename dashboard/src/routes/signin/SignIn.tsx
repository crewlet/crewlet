/**
 * `#/login?next=` — signing in, as a person or with the deployment's token.
 *
 * # One refusal, in the engine's words
 *
 * A failed sign-in is ONE answer at one deadline whatever went wrong — a login
 * nobody holds, a wrong password, a suspended person — so that the form is not
 * a roster of who works here. This screen keeps that: every refusal is shown
 * as the engine's own sentence (`lib/refusal.ts`), and nothing here branches
 * on WHY, because the engine deliberately does not say. The two answers that
 * are not refusals are read by their code: `second_factor_required`, reached
 * only by somebody whose password proved itself, which asks for the code; and
 * `429 throttled`, whose `Retry-After` is the wait before the next attempt is
 * even looked at, shown as a number and held on the button.
 *
 * # The token is sent once and kept nowhere
 *
 * The deployment's own credential — a Tier A token from `crewlet.yaml` — is
 * the way in before anybody has been invited, and on the day sign-in itself
 * is broken. It is exchanged for a one-hour session (`POST /auth/token`) and
 * the field is emptied: the cookie that comes back is the credential, and a
 * page that also kept the token would hand the one value that outlives every
 * session to any script that ran in it.
 *
 * # Already signed in is said, not assumed
 *
 * A browser can arrive here holding a perfectly good session — the reader
 * asked to sign in as somebody else, or a request raced a step-up that
 * replaced the cookie. The screen says whose, and offers to carry on as them;
 * it does not send them on unasked, because "sign in as somebody else" is a
 * thing people mean.
 */

import { useEffect, useState } from "react";
import { Button, Callout, Disclosure, FormField, Input, Text } from "@crewlethq/ui";
import { useRoute } from "~/app/router.tsx";
import { refusalText } from "~/lib/refusal.ts";
import { useSignedIn, type SignedInAs } from "~/lib/session.ts";
import { auth, RestError, type SessionAnswer } from "~/protocol/index.ts";
import { SignInPage } from "./SignInPage.tsx";

/** What the posture read found: a backend, or nothing that could be read. */
type Backend = { kind: "read"; name: string } | { kind: "unread" };

export function SignIn() {
  const next = useRoute().query.get("next");
  const signedIn = useSignedIn();

  const [backend, setBackend] = useState<Backend | null>(null);
  const [current, setCurrent] = useState<SessionAnswer | null>(null);

  useEffect(() => {
    let live = true;
    auth.config().then(
      (config) => live && setBackend({ kind: "read", name: config.backend }),
      // UNREAD IS NOT "NONE". A node that could not answer the posture read
      // still takes a password, and the form is what a person can act on;
      // the engine refuses whatever it will not accept.
      () => live && setBackend({ kind: "unread" }),
    );
    auth.session().then(
      (session) => live && setCurrent(session),
      // Nobody signed in, which is why this screen is usually open.
      () => {},
    );
    return () => {
      live = false;
    };
  }, []);

  // A DEPLOYMENT WITH NO PEOPLE signs nobody in with a password, so the
  // form it would refuse is not offered — only the token.
  const passwords = backend === null || backend.kind === "unread" || backend.name === "local";

  return (
    <SignInPage
      title="Sign in to Crewlet"
      lede={
        passwords
          ? "Use the login or email address your invitation was for."
          : "This deployment signs nobody in with a password. Use one of its API tokens."
      }
    >
      {current && (
        <Callout
          variant="neutral"
          action={
            <Button size="small" variant="secondary" onClick={() => signedIn(current, next)}>
              Continue as {current.login}
            </Button>
          }
        >
          This browser is already signed in as <code className="inline">{current.login}</code>.
          Signing in below replaces that session in this browser.
        </Callout>
      )}
      {passwords && <PasswordForm onSignedIn={(answer) => signedIn(answer, next)} />}
      <TokenForm open={!passwords} onSignedIn={(answer) => signedIn(answer, next)} />
    </SignInPage>
  );
}

/** How long until a throttled attempt is looked at again, held on the button. */
function useWait(): [boolean, (seconds: number) => void] {
  const [until, setUntil] = useState(0);
  useEffect(() => {
    if (until === 0) return;
    const timer = setTimeout(() => setUntil(0), Math.max(0, until - Date.now()));
    return () => clearTimeout(timer);
  }, [until]);
  return [until !== 0, (seconds) => setUntil(Date.now() + seconds * 1000)];
}

function PasswordForm({ onSignedIn }: { onSignedIn: (answer: SignedInAs) => void }) {
  const [login, setLogin] = useState("");
  const [password, setPassword] = useState("");
  const [code, setCode] = useState("");
  // SET BY THE ENGINE, never guessed: it asks for the code only once the
  // password has proved itself, and only for a person who holds a factor.
  const [asked, setAsked] = useState(false);
  const [busy, setBusy] = useState(false);
  const [refusal, setRefusal] = useState<string | null>(null);
  const [waiting, wait] = useWait();

  // A CHANGED LOGIN OR PASSWORD IS A NEW ATTEMPT, and the code prompt was an
  // answer about the old one.
  function changeCredentials(apply: () => void) {
    apply();
    setAsked(false);
    setCode("");
  }

  const ready = login.trim() !== "" && password !== "" && (!asked || code.trim() !== "");

  async function submit() {
    if (busy || waiting || !ready) return;
    setBusy(true);
    setRefusal(null);
    try {
      const answer = await auth.login({
        login: login.trim(),
        password,
        ...(asked ? { code: code.trim() } : {}),
      });
      onSignedIn(answer);
    } catch (err) {
      if (err instanceof RestError && err.code === "second_factor_required") {
        setAsked(true);
        return;
      }
      if (err instanceof RestError && err.status === 429 && err.retryAfter !== null) {
        wait(err.retryAfter);
      }
      // A CODE IS SPENT OR WRONG after one try, so it is not left to be sent
      // again; the password stays, because retyping it is not what fixes a
      // code.
      setCode("");
      setRefusal(refusalText(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <form
      className="signin-form"
      onSubmit={(e) => {
        e.preventDefault();
        void submit();
      }}
    >
      <FormField label="Login or email address">
        {(field) => (
          <Input
            id={field.id}
            aria-describedby={field.describedBy}
            name="username"
            autoComplete="username"
            autoFocus
            width="full"
            spellCheck={false}
            value={login}
            onChange={(e) => changeCredentials(() => setLogin(e.target.value))}
          />
        )}
      </FormField>
      <FormField label="Password">
        {(field) => (
          <Input
            id={field.id}
            aria-describedby={field.describedBy}
            type="password"
            name="password"
            autoComplete="current-password"
            width="full"
            value={password}
            onChange={(e) => changeCredentials(() => setPassword(e.target.value))}
          />
        )}
      </FormField>
      {asked && (
        <FormField
          label="Code"
          helper="The six-digit code from your authenticator app, or one of your recovery codes."
        >
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              name="code"
              autoComplete="one-time-code"
              autoFocus
              width="full"
              spellCheck={false}
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
      <Button type="submit" variant="primary" disabled={busy || waiting || !ready}>
        {busy ? "Signing in" : "Sign in"}
      </Button>
    </form>
  );
}

function TokenForm({
  open,
  onSignedIn,
}: {
  /** Open from the start where the token is the only way in. */
  open: boolean;
  onSignedIn: (answer: SignedInAs) => void;
}) {
  const [typed, setTyped] = useState("");
  const [busy, setBusy] = useState(false);
  const [refusal, setRefusal] = useState<string | null>(null);

  async function submit() {
    const value = typed.trim();
    if (busy || value === "") return;
    setBusy(true);
    setRefusal(null);
    try {
      const answer = await auth.exchangeToken(value);
      setTyped("");
      onSignedIn(answer);
    } catch (err) {
      setRefusal(refusalText(err));
    } finally {
      setBusy(false);
    }
  }

  const form = (
    <form
      className="signin-form"
      onSubmit={(e) => {
        e.preventDefault();
        void submit();
      }}
    >
      <FormField
        label="API token"
        helper="One of this deployment's api.auth.tokens values. It is exchanged for a one-hour session and this browser does not keep it."
      >
        {(field) => (
          <Input
            id={field.id}
            aria-describedby={field.describedBy}
            type="password"
            autoComplete="off"
            width="full"
            spellCheck={false}
            value={typed}
            onChange={(e) => setTyped(e.target.value)}
          />
        )}
      </FormField>
      {refusal && (
        <Callout variant="danger" role="alert">
          {refusal}
        </Callout>
      )}
      <Button type="submit" variant="secondary" disabled={busy || typed.trim() === ""}>
        {busy ? "Signing in" : "Sign in with the token"}
      </Button>
    </form>
  );

  if (open) return form;
  return (
    <Disclosure title="Use an API token instead" size="compact" variant="aside">
      <Text as="p" variant="body" tone="secondary">
        For an operator before anybody has been invited, or when signing in with a password is not
        working.
      </Text>
      {form}
    </Disclosure>
  );
}
