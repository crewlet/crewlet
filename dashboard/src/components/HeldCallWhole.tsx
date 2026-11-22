/**
 * A held call read whole, as the engine opens it for whoever may answer: its
 * question, then every argument, its standard input whole. A merge or a deploy
 * is drawn as its request, each value as text. Allow once names the digest of
 * this read, so it is never given from a view that showed less.
 */

import { CodeBlock, Text } from "@crewlethq/ui";
import type { McpHeldWhole } from "~/contract/mcp.ts";
import { releaseRequestOf } from "~/lib/delegation.ts";
import { ReleaseRequestFacts } from "./Delegation.tsx";

export function HeldCallWhole({ whole, act }: { whole: McpHeldWhole; act?: string | undefined }) {
  const request = releaseRequestOf(act, whole.arguments);
  return (
    <>
      <Text as="p" variant="body">
        {whole.question}
      </Text>
      {request ? (
        <ReleaseRequestFacts request={request} />
      ) : (
        <CodeBlock language="json" code={JSON.stringify(whole.arguments, null, 2)} />
      )}
    </>
  );
}
