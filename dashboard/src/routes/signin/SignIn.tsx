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
 * # The password form is always here; the token is the way in before anybody
 *
 * Password sign-in is always served — there is no deployment setting that
 * switches it off — so the form is never withheld. The deployment's own
 * credential — a Tier A token from `crewlet.yaml` — waits behind "Use an API
 * token instead": it is the way in before anybody has been invited, and on
 * the day sign-in itself is broken. On a deployment whose `/health` says
 * `identity: unclaimed` — nobody has been invited yet — it is the ONLY way in,
 * so the token form is open from the start and the page says how to begin:
 * sign in with the token, then invite yourself from Settings › People &
 * access. `unknown` (a node that cannot read its identity estate) and a
 * `/health` that did not answer are not "unclaimed": they keep the ordinary
 * page, because telling somebody to invite a founder into a company that has
 * one is the wrong instruction to guess.
 *
 * # The token is sent once and kept nowhere
 *
 * It is exchanged for a one-hour session (`POST /auth/token`) and the field is
 * emptied: the cookie that comes back is the credential, and a page that also
 * kept the token would hand the one value that outlives every session to any
 * script that ran in it.
 *
 * # Arriving from a reset link is said, and its login is typed
 *
 * `?login=` fills the login and `?after=reset` says the password was just set
 * — what the reset screen's own "Sign in" sends — so somebody who was just told
 * which login to sign in with is not handed a blank form under a sentence about
 * an invitation. Neither is a credential: a link carrying either fills a field
 * and changes a sentence, nothing more.
 *
 * # Signed out from somewhere else is said
 *
 * A tab that was read by somebody (`lib/reader.ts`) and finds no session here
 * lost it without signing out in this tab — a sign-out made here empties the
 * tab's storage, its reader with it. Something else ended it: a sign-out from
 * another browser or tab, a password change, an administrator, or its own
 * deadline. The page says so rather than dropping the person on a plain form
 * as though they had never been signed in. The engine answers every refused
 * credential with one `401`, so the sentence names what can end a session
 * rather than guessing which one did; the person's own Account page lists
 * each session with what ended it.
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
import { currentReader } from "~/lib/reader.ts";
import { refusalText } from "~/lib/refusal.ts";
import { useSignedIn, type SignedInAs } from "~/lib/session.ts";
import { auth, RestError, type SessionAnswer } from "~/protocol/index.ts";
import { SignInPage } from "./SignInPage.tsx";

export function SignIn() {
  const query = useRoute().query;
  const next = query.get("next");
  const typed = query.get("login") ?? "";
  const reset = query.get("after") === "reset";
  const signedIn = useSignedIn();

  const [unclaimed, setUnclaimed] = useState(false);
  // UNDEFINED UNTIL `/auth/session` HAS ANSWERED, and null once it answered
  // nobody: a tab holding a good session must not flash "you were signed out"
  // for the round trip it takes to say so.
  const [current, setCurrent] = useState<SessionAnswer | null | undefined>(undefined);
  // AS THE PAGE OPENED: a sign-in records the next reader before it moves on.
  const [read] = useState(() => currentReader() !== null);

  useEffect(() => {
    let live = true;
    auth.firstPerson().then(
      (identity) => live && setUnclaimed(identity === "unclaimed"),
      // AN UNREAD ANSWER IS NOT "UNCLAIMED" — see the package doc.
      () => {},
    );
    auth.session().then(
      (session) => live && setCurrent(session),
      // Nobody signed in, which is why this screen is usually open.
      () => live && setCurrent(null),
    );
    return () => {
      live = false;
    };
  }, []);

  return (
    <SignInPage
      title="Sign in to Crewlet"
      lede={
        unclaimed
          ? "Nobody has been invited to this deployment yet."
          : reset
            ? "Your password is set. Sign in with it — a second factor you hold is still asked for."
            : "Use the login or email address your invitation was for."
      }
    >
      {read && current === null && (
        <Callout variant="warning" title="You were signed out">
          Your session in this browser ended without a sign-out here: it was signed out from another
          browser or tab, ended by a password change or by an administrator, or it timed out. Sign
          in again to carry on.
        </Callout>
      )}
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
      {unclaimed && (
        <Callout variant="neutral" title="Getting started">
          Sign in with one of this deployment&rsquo;s API tokens — the founder token its{" "}
          <code className="inline">crewlet.yaml</code> declares under{" "}
          <code className="inline">api.auth.tokens</code> — then open Settings › People &amp; access
          and invite yourself. The invitation&rsquo;s link is where you choose your login and
          password.
        </Callout>
      )}
      <PasswordForm
        focus={!unclaimed}
        typed={typed}
        onSignedIn={(answer) => signedIn(answer, next)}
      />
      <TokenForm open={unclaimed} onSignedIn={(answer) => signedIn(answer, next)} />
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

function PasswordForm({
  focus,
  typed,
  onSignedIn,
}: {
  /** Whether the form takes the focus: not where the token is the way in. */
  focus: boolean;
  /** A login already known — `?login=` — or "". The password takes the focus then. */
  typed: string;
  onSignedIn: (answer: SignedInAs) => void;
}) {
  const [login, setLogin] = useState(typed);
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
      if (err instanceof RestError && err.status === 429 && err.retryAfterSeconds !== null) {
        wait(err.retryAfterSeconds);
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
            autoFocus={focus && typed === ""}
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
            autoFocus={focus && typed !== ""}
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
  /** Open from the start where the token is the only way in: nobody invited yet. */
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
            autoFocus={open}
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
