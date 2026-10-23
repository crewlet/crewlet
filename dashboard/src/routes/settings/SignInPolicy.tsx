/**
 * Settings › Sign-in policy: how a person signs in, as this node enforces it.
 *
 * # What the engine enforces, on the node that answered
 *
 * The answer (`sign_in_policy`, an administrator's) is the posture the sign-in
 * code itself reads: whether a second factor is required, the shortest
 * password, how long a session lives from sign-in and unused, and how long a
 * proof of identity authorises a sensitive gesture, each with its default
 * applied where the node's file writes none. It is TIER A, written in each
 * node's own `crewlet.yaml` (`api.auth`), so it is THIS node's and nodes of
 * one fleet may differ: the page names the node rather than presenting the
 * reading as the company's. Read only: it changes with an edit to that file
 * and a restart.
 */

import { Card, DescriptionList, InlineCode } from "@crewlethq/ui";
import { ShieldUserGlyph } from "@crewlethq/icons/glyphs";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { QueryState } from "~/components/common.tsx";
import { fmtSetting, plural } from "~/lib/format.ts";
import { useQuery } from "~/lib/useQuery.ts";

export function SignInPolicy() {
  const { data, loading, error, refusal, detail } = useQuery("sign_in_policy", undefined, {
    pollMs: 300_000,
  });
  return (
    <>
      <PageNote>
        {data ? (
          <>
            Set in this node&rsquo;s crewlet.yaml (<InlineCode>{data.node}</InlineCode>, under
            api.auth), and changed with a restart.
          </>
        ) : (
          "Set in each node's crewlet.yaml (under api.auth), and changed with a restart."
        )}
      </PageNote>
      <QueryState error={error} refusal={refusal} detail={detail} loading={loading && !data}>
        {data && (
          <div className="col gap-4">
            <Card>
              <Card.Header icon={<ShieldUserGlyph size="sm" />}>
                <Card.Title>Signing in</Card.Title>
              </Card.Header>
              <DescriptionList>
                <DescriptionList.Item term="Two-step verification">
                  {data.policy.second_factor === "required"
                    ? "Required for everybody who signs in with a password"
                    : "Optional, each person chooses"}
                </DescriptionList.Item>
                <DescriptionList.Item term="Shortest password">
                  {plural(data.policy.min_password_length, "character")}
                </DescriptionList.Item>
              </DescriptionList>
            </Card>
            <Card>
              <Card.Header icon={<ShieldUserGlyph size="sm" />}>
                <Card.Title>Sessions</Card.Title>
              </Card.Header>
              <DescriptionList>
                <DescriptionList.Item term="Longest a session lives">
                  {fmtSetting(data.policy.session_absolute_seconds)}
                </DescriptionList.Item>
                <DescriptionList.Item term="Signed out when unused for">
                  {fmtSetting(data.policy.session_idle_seconds)}
                </DescriptionList.Item>
                <DescriptionList.Item term="A sensitive change asks again after">
                  {fmtSetting(data.policy.step_up_seconds)}
                </DescriptionList.Item>
              </DescriptionList>
            </Card>
          </div>
        )}
      </QueryState>
    </>
  );
}
