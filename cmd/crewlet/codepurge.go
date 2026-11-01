package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/statelog"
)

// `crewlet code purge-unreachable` — a person's purge of a repository's
// unreachable objects, from a shell.
//
// # Through the act transport, as the person the token is bound to
//
// The tool a person's assistant and a repository's Settings tab on the
// dashboard call, code_purge_unreachable, through the operator catalogue's act
// route (`POST /operator/act/{tool}`), so a purge from a terminal is the same
// request with the same record. The credential is
// CREWLET_API_TOKEN and nothing else, and it must be a PERSON's own machine
// token: a purge is a person's alone, so a token bound to nobody is refused,
// naming why. The step-up a purge asks of a dashboard session is not asked of
// it: a machine token is stepped up by construction (internal/api/auth).
//
// # A retry is the same request
//
// Every invocation mints an operation id before the request and sends it as
// the `Idempotency-Key` header; the request's id is that operation, so an
// answer whose outcome is unknown — or none at all — prints it, and `-op-id`
// sends it again, which the repository's committer answers as the first
// request rather than recording a second.

// codePurgeTool is the tool the command calls.
const codePurgeTool = "code_purge_unreachable"

// codePurge is one purge request.
func codePurge(environ config.Source, args []string, stdout, stderr io.Writer) error {
	name, rest := splitSubject(args)
	var opID *string
	client, err := nodeClientFor(environ, rest, "code purge-unreachable", stderr, func(fs *flag.FlagSet) {
		opID = fs.String("op-id", "",
			"retry an `unknown` outcome with the id it printed, so the retry is the same request; "+
				"empty starts a new one")
	})
	if err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	if name == "" || strings.HasPrefix(name, "-") {
		fmt.Fprint(stderr, codeUsage)
		return errors.New("name the repository to purge, as code status lists it")
	}
	// THE OPERATION ID IS MINTED HERE, BEFORE THE REQUEST, as `seats pause`
	// mints one and for its reason: a request whose answer never came may
	// well be recorded, and the key it was sent under is the only handle on
	// it. One the person brought is held to the rule the node holds it to
	// before anything is sent, so the refusal names the flag.
	operation := strings.TrimSpace(*opID)
	if operation == "" {
		operation = statelog.NewOpID(time.Now(), "purge")
	} else if err = statelog.CheckCallerOpID(operation); err != nil {
		return fmt.Errorf("-op-id: %w", err)
	}

	var answer struct {
		Outcome string `json:"outcome"`
		Receipt struct {
			Repo       string `json:"repo"`
			Request    string `json:"request"`
			Position   string `json:"position"`
			Maintainer string `json:"maintainer"`
			Unplaced   string `json:"unplaced"`
			Holds      struct {
				GitNodes      []string `json:"git_nodes"`
				BackupsBefore string   `json:"backups_before"`
				StoreUntil    string   `json:"store_until"`
			} `json:"holds"`
		} `json:"receipt"`
	}
	// THE ORDINARY CEILING: a request is one record on the repository's
	// subject, decided from its row alone — the "handful of writes"
	// [nodeRequestTimeout] is sized for. The purge itself runs later, on the
	// repository's maintainer, and nothing here waits for it.
	err = client.postKeyed(context.Background(), "/operator/act/"+url.PathEscape(codePurgeTool), operation,
		map[string]any{"args": map[string]any{"repo": name}}, &answer)
	retry := fmt.Sprintf("run the same command again with -op-id %s, so the retry is this request rather "+
		"than a second one", operation)
	var lost noAnswer
	if errors.As(err, &lost) {
		fmt.Fprintf(stderr, "The node did not answer, so whether the purge of %s was recorded is unknown: %s.\n",
			name, retry)
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
	r := answer.Receipt
	if answer.Outcome == string(statelog.OutcomeUnknown) {
		fmt.Fprintf(stdout, "purge %s: unknown — the node could not confirm the request. To find out, %s\n",
			name, retry)
		return nil
	}
	if r.Request == "" {
		fmt.Fprintf(stdout, "%s holds no pack: nothing was left to purge, and the request is served\n",
			name)
		return nil
	}
	fmt.Fprintf(stdout, "purge of %s requested (%s, at %s)\n", name, r.Request, r.Position)
	switch {
	case r.Maintainer != "":
		fmt.Fprintf(stdout, "  served by %s, the repository's maintainer\n", r.Maintainer)
	case r.Unplaced != "":
		fmt.Fprintf(stdout, "  its maintainer could not be named: %s\n", r.Unplaced)
	}
	fmt.Fprintln(stdout, "  until it is served and after, the purged objects are still held by:")
	if len(r.Holds.GitNodes) > 0 {
		fmt.Fprintf(stdout, "    the caches of %s, until each syncs past the purge\n",
			strings.Join(r.Holds.GitNodes, ", "))
	}
	fmt.Fprintf(stdout, "    %s\n    %s\n", r.Holds.BackupsBefore, r.Holds.StoreUntil)
	fmt.Fprintf(stdout, "  `crewlet code status -repo %s` shows the request until it is served\n", name)
	return nil
}
