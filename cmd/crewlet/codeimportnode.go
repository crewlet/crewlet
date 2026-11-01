package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// `crewlet code import` ON THE NODE — the default — a person's request that the
// code host create a project repository filled from one at another host, or
// kept in step with it as a pull mirror (-mirror).
//
// # Through the act transport, as the person the token is bound to
//
// The tool a person's assistant and the dashboard call, code_import_repo, over
// the same route (`POST /operator/act/{tool}`), so an import from a terminal
// is the same create with the same record. The credential is CREWLET_API_TOKEN
// and nothing else, and it must be a PERSON's own machine token: an import is
// a person's alone (U49 a), so a token bound to nobody is refused, naming why.
//
// # The node fetches, this machine does not
//
// The repository's home git node dials the source — over https alone, every
// address it resolves held to the public ones unless Tier A's
// git.import_allow_private says otherwise (M6-D18) — and lands it in chunks,
// so a history of any size goes in whatever code.limits.max_push_bytes says,
// with nothing to resume by hand: a restarted home fetches again from the refs
// already landed. The source's token is the node's too: -credential names the
// variable that holds it, in the node's secret store or environment, and is
// recorded as that reference — the token never crosses this command line, nor
// any record (M6-D17). `-local` is the other import, this machine's git
// pushing into a repository that exists (codeimport.go), for a source only
// this machine can reach.
//
// # A retry is the same create
//
// Every invocation mints an operation id before the request and sends it as
// the `Idempotency-Key` header; the repository's id is derived from that
// operation, so an answer whose outcome is unknown — or none at all — prints
// it, and `-op-id` sends it again, which the domain's ledger answers as the
// first create rather than refused as a name already taken.

// codeImportTool is the tool the command calls.
const codeImportTool = "code_import_repo"

// codeImportOnNode is one import the node carries out: call is the command line
// as [readImport] read it, not -local.
func codeImportOnNode(ctx context.Context, call importCall, stdout, stderr io.Writer) error {
	owner, err := ownerFlag(call.owner)
	if err != nil {
		fmt.Fprintln(stderr, codeImportUsage)
		return err
	}
	// THE OPERATION ID IS MINTED HERE, BEFORE THE REQUEST, as `code
	// purge-unreachable` mints one and for its reason: a create whose answer
	// never came may well be recorded, and the key it was sent under is the
	// only handle on it. One the person brought is held to the rule the node
	// holds it to before anything is sent, so the refusal names the flag.
	operation := strings.TrimSpace(call.opID)
	if operation == "" {
		operation = statelog.NewOpID(time.Now(), "import")
	} else if err = statelog.CheckCallerOpID(operation); err != nil {
		return fmt.Errorf("-op-id: %w", err)
	}
	args := map[string]any{"name": call.name, "url": call.source, "owner": owner}
	if call.credential != "" {
		args["credential"] = call.credential
	}
	if call.mirror {
		args["mirror"] = true
	}

	var answer struct {
		Outcome string `json:"outcome"`
		Receipt struct {
			Repo       string `json:"repo"`
			Position   string `json:"position"`
			Repository struct {
				Name          string `json:"name"`
				Status        string `json:"status"`
				DefaultBranch string `json:"default_branch"`
				Owner         struct {
					Kind string `json:"kind"`
					Ref  string `json:"ref"`
				} `json:"owner"`
			} `json:"repository"`
		} `json:"receipt"`
	}
	// THE ORDINARY CEILING: a create is one record on the name's subject,
	// decided from its rows alone — the "handful of writes"
	// [nodeRequestTimeout] is sized for. The fetch itself runs later, on the
	// repository's home, and nothing here waits for it.
	err = call.client.postKeyed(ctx, "/operator/act/"+url.PathEscape(codeImportTool), operation,
		map[string]any{"args": args}, &answer)
	retry := fmt.Sprintf("run the same command again with -op-id %s, so the retry is this create rather "+
		"than a second one", operation)
	var lost noAnswer
	if errors.As(err, &lost) {
		fmt.Fprintf(stderr, "The node did not answer, so whether %s was created is unknown: %s.\n", call.name,
			retry)
		return err
	}
	var refused *nodeRefusal
	if errors.As(err, &refused) && refused.Status == http.StatusServiceUnavailable &&
		(refused.Unsettled || refused.OpID != "") {
		if refused.Unvouched {
			return fmt.Errorf("%w\n\nThis node cannot tell whether it was recorded and will answer the same "+
				"way until it can: run it through another node with -url <that node> -op-id %s — never a "+
				"fresh id", err, operation)
		}
		return fmt.Errorf("%w\n\nTo find out, %s", err, retry)
	}
	if err != nil {
		return err
	}
	if answer.Outcome == string(statelog.OutcomeUnknown) {
		fmt.Fprintf(stdout, "import %s: unknown — the node could not confirm the create. To find out, %s\n",
			call.name, retry)
		return nil
	}
	r := answer.Receipt.Repository
	what := "importing"
	if call.mirror {
		what = "mirroring"
	}
	fmt.Fprintf(stdout, "%s %s into %s — %s, owned by %s:%s (at %s)\n", what, call.source, call.name,
		r.Status, r.Owner.Kind, r.Owner.Ref, answer.Receipt.Position)
	if call.mirror {
		fmt.Fprintln(stdout, "  a pull mirror: once imported, its home keeps it in step with the source, "+
			"and every push to it is refused")
	}
	fmt.Fprintf(stdout, "  its home git node fetches the source now; `crewlet code status -repo %s` shows it "+
		"importing until the fetch has landed\n", call.name)
	return nil
}

// ownerFlag is -owner as the tool takes an owner — `unit:<key>` or
// `seat:<handle>` — or why it is none.
func ownerFlag(v string) (map[string]any, error) {
	kind, ref, ok := strings.Cut(strings.TrimSpace(v), ":")
	ref = strings.TrimSpace(ref)
	switch {
	case v == "":
		return nil, errors.New("-owner: a project names its owner — unit:<key> or seat:<handle>")
	case !ok || ref == "" || kind != "unit" && kind != "seat":
		return nil, fmt.Errorf("-owner: %q is neither unit:<key> nor seat:<handle>", v)
	}
	return map[string]any{"kind": kind, "ref": ref}, nil
}
