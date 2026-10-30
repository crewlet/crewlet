package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// `crewlet fleet broker credential` — the operator key a member's broker
// admits on its loopback client listener, for the nats tool.
//
// # Why a gesture at all
//
// Every connection to an embedded broker presents a key derived from
// secrets.keys (ADR-0064), and the docs prescribe broker gestures no engine
// verb performs: above all the resize every stream and bucket needs before a
// raised `stream.replicas` lets a node start, which only `nats stream update`
// does. So a clustered member's client listener — open on loopback because
// nats-server routes only once one is — admits one more kind of connection:
// an operator presenting the nkey derived from a key of the ring under
// [jetstream.ClientKeyDomain]. This writes that key's seed where the nats
// tool reads one (`--nkey FILE`) and prints the invocation.
//
// The key grants nothing its holder lacks: whoever reads this node's Tier A
// holds the keyring, which already decrypts the company document and the
// secret store and mints every run token. And it reaches the global account,
// never the system one, so a membership change still goes through `crewlet
// fleet broker remove` and the engine.
//
// # Derived here, addressed by the node
//
// The seed is derived from THIS host's Tier A, never fetched: no API carries a
// credential. The address is the running node's own — its client listener's
// port is the OS's, chosen at each start — read from GET /fleet/broker, which
// takes `admin` as `list` does. A listener on loopback is reachable from its
// own host alone, so an answer from any other node (a -url naming a balancer,
// or a peer) is refused rather than printed. A member that serves no API
// (api.port 0) is asked nothing: its broker_keys line names the listener.
//
// # Never to the terminal
//
// The seed goes to a file created 0600, never stdout — break-glass, as
// `crewlet secrets get` is: no scrollback, no screen share — and an existing
// path is refused, so a credential file is never overwritten.

// fleetBrokerCredential is `crewlet fleet broker credential`.
func fleetBrokerCredential(environ config.Source, args []string, stdout, stderr io.Writer) error {
	bootstrapPath, args := splitSubject(args)
	flags := flag.NewFlagSet("fleet broker credential", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultBootstrapPath,
		"node config file: the keyring the credential derives from, and where this node serves its API")
	addr := flags.String("url", "",
		"this host's node's base URL (default: from the config's api block). "+
			"The token comes from "+apiTokenEnv+", never a flag")
	out := flags.String("o", "",
		"the file the operator key's seed is written to: created 0600, and never one that exists")
	if err := flags.Parse(args); err != nil {
		return err
	}
	bootstrapPath, given := onePositional(flags, bootstrapPath)
	if given > 1 {
		fmt.Fprintln(stderr, fleetBrokerUsage)
		return errors.New("name at most one config document")
	}
	if bootstrapPath == "" {
		bootstrapPath = *configPath
	}
	if strings.TrimSpace(*out) == "" {
		fmt.Fprintln(stderr, fleetBrokerUsage)
		return errors.New("-o FILE is required: the operator key is written to a " +
			"file the nats tool reads, never to the terminal")
	}

	boot, err := config.LoadBootstrap(bootstrapPath, config.EnvOnly())
	if err != nil {
		return err
	}
	switch boot.BrokerKind() {
	case placement.BrokerClient:
		return errors.New("this node dials an external NATS cluster, and no key of " +
			"secrets.keys admits anybody to it: reach it with the credentials its " +
			"own operator issues")
	case placement.BrokerLeaf:
		return errors.New(noClientListener)
	}
	seed, _, keyID, err := jetstream.OperatorCredential(boot.Secrets.TokenMaterial())
	if err != nil {
		return fmt.Errorf("derive the operator key from secrets.keys: %w", err)
	}

	// THE ADDRESS, asked of this host's own node — before anything is written,
	// so a refusal leaves nothing behind.
	listener, asked := "<client listener>", boot.API.Port != 0 || strings.TrimSpace(*addr) != ""
	if asked {
		if listener, err = ownClientListener(environ, boot, *addr); err != nil {
			return err
		}
	}
	if err := writeCredential(*out, seed); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Wrote the operator key derived from secrets.keys id %q to %s (0600).\n"+
		"It reaches this member's broker on its loopback client listener, from this host only:\n\n"+
		"  nats --server nats://%s --nkey %s <command>\n\n"+
		"for example `stream ls`, or `stream update <name> --replicas 3`. It reaches every stream "+
		"and bucket, never the system account (change the broker's membership with `crewlet "+
		"fleet broker remove`), and it is admitted until key %q leaves secrets.keys: write it "+
		"again once a rotation drops it.\n", keyID, *out, listener, *out, keyID)
	if !asked {
		fmt.Fprintln(stdout, "\nThis node serves no API (api.port is 0), so nothing was asked "+
			"for its client listener: its broker_keys line, logged at every start of its "+
			"broker, names it.")
	}
	return nil
}

// noClientListener is the refusal for a node whose broker opens no client
// listener.
const noClientListener = "this node's broker has no client listener — a solo member and a " +
	"leaf listen on none, and a member with only a leaf listener runs no cluster; run this " +
	"on a clustered member's host"

// ownClientListener is this host's node's client listener, as the running node
// answers it — refused where the node that answered is not this host's.
func ownClientListener(environ config.Source, boot *config.Bootstrap, override string) (string, error) {
	self, err := config.ResolveNodeID(boot, nil)
	if err != nil {
		return "", err
	}
	base, err := nodeBaseURL(boot, override, "this node's broker")
	if err != nil {
		return "", err
	}
	client := &nodeClient{base: base, token: nodeTokenOrEmpty(environ),
		http: httpx.Client(nodeRequestTimeout)}
	var view brokerView
	if err := client.get(context.Background(), "/fleet/broker", &view); err != nil {
		return "", err
	}
	switch {
	case view.Node != self:
		// A LISTENER ON LOOPBACK IS ITS OWN HOST'S: another node's address
		// would point the tool at a port on this host that is not its broker.
		return "", fmt.Errorf("the node that answered is %q and this host's node is %q "+
			"(node.id, else CREWLET_NODE_ID in this shell): a client listener is on its "+
			"own node's loopback — run this against this host's own API (drop -url), in "+
			"the environment the node runs with", view.Node, self)
	case view.External:
		return "", errors.New("this fleet's broker is an external NATS cluster: reach it " +
			"with the credentials its own operator issues")
	case view.Client == "":
		return "", errors.New(noClientListener)
	}
	return view.Client, nil
}

// writeCredential creates path, 0600, holding seed — refused where anything is
// already there, so a credential file is never overwritten.
func writeCredential(path string, seed []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%s exists, and a credential file is never overwritten: "+
			"remove it, or name another with -o", path)
	}
	if err != nil {
		return fmt.Errorf("write the operator key: %w", err)
	}
	// A FILE THAT WAS NOT WRITTEN WHOLE IS REMOVED: a truncated seed is no
	// credential, and leaving it would make the retry refuse its own path.
	_, err = f.Write(append(append([]byte(nil), seed...), '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("write the operator key to %s: %w", path, err)
	}
	return nil
}
