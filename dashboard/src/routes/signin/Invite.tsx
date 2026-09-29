/**
 * `#/invite/<id>.<secret>` — joining a company from the link somebody sent.
 *
 * # The link is two halves, and only one of them may ever be in a URL
 *
 * The engine mints `<external_url>/dashboard#/invite/<id>.<secret>`, with the
 * whole credential in the FRAGMENT, which no browser sends to any server — so
 * neither half reaches a proxy's access log on the way to this page. This
 * screen keeps that promise on the way back out: the id goes in the route's
 * path, and the secret travels BESIDE it, in a header for the view and in the
 * body for the redemption (`protocol/auth.ts`). A secret in a query string
 * would put the credential in exactly the logs the fragment kept it out of.
 *
 * # Rendering it spends nothing
 *
 * The view is a GET the engine answers without spending the link, because a
 * mail client prefetching, a scanner or a preview card follows the same URL;
 * only the form's POST — a person, having chosen a password — redeems it.
 *
 * # One answer for every link that no longer works
 *
 * Redeemed, withdrawn, expired, a secret that is not the link's, an id nobody
 * issued: the engine answers all of them `410` in the same bytes, because
 * telling them apart would say which links exist to somebody guessing at
 * them, and "already used" to somebody whose link merely aged out. So this
 * screen has one sentence for all of them too — the engine's — and the remedy
 * that is right for every one: ask whoever sent it for a new one.
 *
 * # A refusal the person can fix keeps their form
 *
 * A login somebody already holds is `409`, a password under the floor or a
 * login outside the grammar `400` naming the rule; both are shown in the
 * engine's words above a form that keeps everything typed, because a person
 * told to choose another login should not have to type their password again.
 * The redemption can be retried until it lands — the engine derives the
 * person from the invitation, so a second attempt names the same person.
 */

import { useCallback, useEffect, useState } from "react";
import { Button, Callout, EmptyState, FormField, Input, Skeleton, Text } from "@crewlethq/ui";
import { ExploreGlyph, KeyGlyph } from "@crewlethq/icons/glyphs";
import { useNavigator } from "~/app/router.tsx";
import { refusalText } from "~/lib/refusal.ts";
import { LANDING, useSignedIn } from "~/lib/session.ts";
import { auth, RestError, type InvitationView } from "~/protocol/index.ts";
import { SignInPage } from "./SignInPage.tsx";

/**
 * The two halves of a link's last segment, or null for one that is not a
 * whole link. The id is a uuid and the secret unpadded URL-safe base64, so
 * neither half can hold the dot that joins them.
 */
export function parseInviteLink(segment: string): { id: string; secret: string } | null {
  const at = segment.indexOf(".");
  if (at <= 0 || at === segment.length - 1) return null;
  return { id: segment.slice(0, at), secret: segment.slice(at + 1) };
}

/** How many characters a password is, as the engine counts them: runes. */
function characters(text: string): number {
  return [...text].length;
}

type Read =
  | { state: "loading" }
  | { state: "ready"; view: InvitationView }
  | { state: "spent"; sentence: string }
  | { state: "failed"; sentence: string };

export function Invite({ link }: { link: string }) {
  const parsed = parseInviteLink(link);
  if (!parsed) {
    return (
      <SignInPage title="This invitation link is incomplete">
        <EmptyState
          icon={<ExploreGlyph size={32} />}
          title="Part of the link is missing."
          description="An invitation link ends in two parts joined by a dot. Copy the whole link from the message it came in, or ask whoever sent it for a new one."
          headingLevel="none"
        />
      </SignInPage>
    );
  }
  return <Invitation id={parsed.id} secret={parsed.secret} />;
}

function Invitation({ id, secret }: { id: string; secret: string }) {
  const [read, setRead] = useState<Read>({ state: "loading" });

  const load = useCallback(() => {
    setRead({ state: "loading" });
    let live = true;
    auth.viewInvite(id, secret).then(
      (view) => live && setRead({ state: "ready", view }),
      (err: unknown) => {
        if (!live) return;
        if (err instanceof RestError && err.status === 410) {
          setRead({ state: "spent", sentence: err.sentence || refusalText(err) });
        } else {
          setRead({ state: "failed", sentence: refusalText(err) });
        }
      },
    );
    return () => {
      live = false;
    };
  }, [id, secret]);

  useEffect(load, [load]);

  switch (read.state) {
    case "loading":
      return (
        <SignInPage title="Opening your invitation">
          <Skeleton variant="text" rows={4} label="Reading the invitation" />
        </SignInPage>
      );
    case "spent":
      return <Spent sentence={read.sentence} />;
    case "failed":
      return (
        // NOTHING WAS DECIDED ABOUT THE LINK: the engine could not be asked
        // or could not read its records, and saying the link is bad would
        // send somebody to ask for a new one they do not need.
        <SignInPage title="Your invitation could not be read">
          <Callout
            variant="warning"
            role="alert"
            action={
              <Button size="small" variant="secondary" onClick={load}>
                Try again
              </Button>
            }
          >
            {read.sentence} Nothing about the invitation has been decided — it has not been used.
          </Callout>
        </SignInPage>
      );
    case "ready":
      return (
        <Redeem
          id={id}
          secret={secret}
          view={read.view}
          onSpent={(sentence) => setRead({ state: "spent", sentence })}
        />
      );
  }
}

function Spent({ sentence }: { sentence: string }) {
  const nav = useNavigator();
  return (
    <SignInPage title="This invitation can no longer be used">
      <EmptyState
        icon={<KeyGlyph size={32} />}
        title={sentence}
        description="A link works once, and for a limited time. If you already joined with it, sign in instead."
        headingLevel="none"
        action={
          <Button variant="secondary" onClick={() => nav.to(["login"])}>
            Sign in
          </Button>
        }
      />
    </SignInPage>
  );
}

function Redeem({
  id,
  secret,
  view,
  onSpent,
}: {
  id: string;
  secret: string;
  view: InvitationView;
  onSpent: (sentence: string) => void;
}) {
  const signedIn = useSignedIn();
  const [login, setLogin] = useState(view.login ?? "");
  const [name, setName] = useState("");
  const [password, setPassword] = useState("");
  const [tried, setTried] = useState(false);
  const [busy, setBusy] = useState(false);
  const [refusal, setRefusal] = useState<string | null>(null);

  const floor = view.min_password_length;
  const short = characters(password) < floor;
  const loginMissing = login.trim() === "";

  async function submit() {
    setTried(true);
    if (busy || short || loginMissing) return;
    setBusy(true);
    setRefusal(null);
    try {
      const answer = await auth.redeemInvite(id, {
        secret,
        login: login.trim(),
        name: name.trim(),
        password,
      });
      signedIn(answer, LANDING);
    } catch (err) {
      if (err instanceof RestError && err.status === 410) {
        onSpent(err.sentence || refusalText(err));
        return;
      }
      setRefusal(refusalText(err));
    } finally {
      setBusy(false);
    }
  }

  const seat = view.seat;
  return (
    <SignInPage
      title="Join your company on Crewlet"
      lede={
        <>
          {view.invited_by ? (
            <>
              <strong>{view.invited_by}</strong> invited{" "}
            </>
          ) : (
            "This is an invitation for "
          )}
          <strong>{view.email}</strong>. Choose how you sign in, and you are in.
        </>
      }
    >
      {seat &&
        (seat.handle ? (
          <Callout variant="neutral">
            <Text as="span" variant="body">
              You will hold the seat <strong>{seat.name || seat.handle}</strong>{" "}
              <code className="inline">{seat.handle}</code> in the org chart: work addressed to it
              reaches you, and your changes are recorded under it.
            </Text>
          </Callout>
        ) : (
          // THE ENGINE REFUSES THIS REDEMPTION, and says so only once it is
          // posted: the seat it binds is no longer in this node's chart.
          // Saying it before the person types a password is the difference
          // between a warning and a wasted form.
          <Callout variant="warning">
            The seat this invitation was for is no longer in the org chart, so redeeming it will be
            refused. Ask whoever sent it for a new one.
          </Callout>
        ))}
      <form
        className="signin-form"
        onSubmit={(e) => {
          e.preventDefault();
          void submit();
        }}
      >
        {/* FOR A PASSWORD MANAGER, which files a new password under the
            field named `username` beside it — and the address is one of
            the two things the sign-in form takes. */}
        <input
          hidden
          readOnly
          type="email"
          name="username"
          autoComplete="username"
          value={view.email}
        />
        <FormField
          label="Login"
          helper="What your changes are recorded under while you hold no seat, and one way to sign in. Lowercase words joined by dots, such as jane.doe."
          error={tried && loginMissing ? "Choose a login." : undefined}
        >
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              aria-invalid={field.invalid || undefined}
              autoComplete="off"
              spellCheck={false}
              width="full"
              value={login}
              onChange={(e) => setLogin(e.target.value)}
            />
          )}
        </FormField>
        <FormField label="Your name" optional helper="How your colleagues see you.">
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              autoComplete="name"
              width="full"
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
          )}
        </FormField>
        <FormField
          label="Password"
          helper={`At least ${floor} characters. There are no other rules; a long phrase is the strongest password there is.`}
          error={
            tried && short
              ? `This is ${characters(password)} characters, and the minimum is ${floor}.`
              : undefined
          }
        >
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              aria-invalid={field.invalid || undefined}
              type="password"
              autoComplete="new-password"
              width="full"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
            />
          )}
        </FormField>
        {refusal && (
          <Callout variant="danger" role="alert">
            {refusal}
          </Callout>
        )}
        <Button type="submit" variant="primary" disabled={busy}>
          {busy ? "Joining" : "Join"}
        </Button>
      </form>
    </SignInPage>
  );
}
