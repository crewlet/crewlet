package config

import (
	"fmt"
	"maps"
	"net"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/gitref"
)

// WHICH CREDENTIAL A REMOTE MAY BE DIALED WITH (code.remote_credentials): the
// variables a repository's mirror or import may name for its credential, each
// with the hosts its value may be sent to. A credential a mirror or an import
// names is a ${VAR} reference, resolved where its repository's home Gitlet node
// dials — from the secret store and then that node's own environment — and
// sent to the host of the URL beside it. So a reference nothing bounds is a
// way to have the Gitlet node send ANY value it can resolve — the keyring's
// CREWLET_SECRET_KEY_*, a Tier A token, the object store's key, a model
// vendor's — to a host its writer chose, and a person who may create a
// repository (every member of the company, code_import_repo) is not thereby
// trusted with every secret of every Gitlet node. An administrator writes this
// table, and a remote's credential is one it declares for the remote's host:
// refused where a mirror or an import is declared (this document's
// validation), where a person asks for one (the code domain's lifecycle
// decide) and again at the dial, under the table the dialing node holds then
// ([CodeRemoteCredentials.Refusal], the one judgement all three read).
//
// KEYED BY THE VARIABLE'S NAME, which is what a record carries — the whole
// reference ${NAME} — so the declaration binds the record as it stands and a
// rotation of the value behind it is a secret store write, with nothing on any
// repository to change. The table holds no secret, only names and hosts: the
// value is the secret store's or the environment's, as it was.
//
// HOSTS, NEVER URLS: a credential is a forge's, and a forge's every repository
// is reached under its one host, while a URL prefix would read
// https://github.com/acme against https://github.com/acme-evil the same.
// A host is matched whole, its port included (443 where none is written), so
// a value declared for github.com is never sent to api.github.com, to
// github.com.evil.example or to another port of the same name.

const (
	// CodeMaxRemoteCredentials is the most variables code.remote_credentials
	// declares: SIXTY-FOUR, as many as a push mirror names refs
	// ([CodeMaxMirrorRefs]) — a typo guard, since each is one token at one
	// forge and a company with several forges and an organisation's token for
	// each holds a handful. Every decide and dial reads the entry its
	// reference names, by key.
	CodeMaxRemoteCredentials = 64

	// CodeMaxRemoteCredentialHosts is the most hosts one declared credential
	// may be sent to: SIXTEEN — a forge's one host, or a self-hosted forge's
	// few names and ports — a typo guard, read once a decide or a dial.
	CodeMaxRemoteCredentialHosts = 16

	// codeMaxRemoteCredentialNameBytes is the longest variable name
	// code.remote_credentials declares: what fits the ${NAME} a record
	// carries inside [CodeMaxCredentialRefBytes].
	codeMaxRemoteCredentialNameBytes = CodeMaxCredentialRefBytes - len("${}")
)

// CodeRemoteCredentials are the variables a repository's mirror or import may
// name for its credential, by the variable's name.
type CodeRemoteCredentials map[string]CodeRemoteCredential

// CodeRemoteCredential is one declared variable: the hosts its value may be
// sent to.
type CodeRemoteCredential struct {
	Hosts []string `yaml:"hosts" json:"hosts" js:"required" desc:"The hosts this variable's value may be sent to, each a host name or an IP address with an optional :port (github.com, gitlab.example.com:8443), matched whole against a mirror's or an import's URL, its port included (443 where none is written): at least one, at most 16, each once."`
}

// zero is a table that declares nothing.
func (c CodeRemoteCredentials) zero() bool { return len(c) == 0 }

// Refusal is why a remote at url may not be dialed with credential under this
// table, or "": a credential "" fetches anonymously and is never refused, and
// any other is the whole ${NAME} of a variable the table declares with the
// URL's host among its hosts. THE ONE JUDGEMENT, read where a declaration is
// validated, where a person's create or settings change is decided, and at
// every dial.
//
// THE SENTENCE NEVER QUOTES THE CREDENTIAL, as a remote's refusal never does
// (a text that is no reference is most likely a token): it names the host and
// the variables the table declares for it.
func (c CodeRemoteCredentials) Refusal(url, credential string) string {
	if credential == "" {
		return ""
	}
	r, err := gitref.ParseRemoteURL(url)
	if err != nil {
		return "the url is no remote: " + err.Error()
	}
	host := remoteHostKey(r.Host, r.Port)
	name, whole := envref.Whole(credential)
	if whole && credential == "${"+name+"}" {
		if declared, ok := c[name]; ok && slices.ContainsFunc(declared.Hosts, func(h string) bool {
			key, ok := declaredHostKey(h)
			return ok && key == host
		}) {
			return ""
		}
	}
	var forHost []string
	for _, n := range slices.Sorted(maps.Keys(c)) {
		if slices.ContainsFunc(c[n].Hosts, func(h string) bool {
			key, ok := declaredHostKey(h)
			return ok && key == host
		}) {
			forHost = append(forHost, n)
		}
	}
	said := "code.remote_credentials declares no variable for it"
	if len(forHost) > 0 {
		said = "the variables code.remote_credentials declares for it are " + strings.Join(forHost, ", ")
	}
	return fmt.Sprintf("the credential is no variable code.remote_credentials declares for %s, the host this "+
		"remote's value would be sent to (%s): an administrator declares which variable may be sent to which "+
		"host, so no other value Gitlet can resolve leaves for a host its writer chose",
		remoteHostWords(r.Host, r.Port), said)
}

// validate holds code.remote_credentials' RUNNABLE rules at path: each name
// one a ${NAME} reference spells, of at most
// [codeMaxRemoteCredentialNameBytes], and each with at least one host a
// remote's URL can name, each once — a name no reference spells, or a host no
// URL names, is a declaration that can never match, which the writer meant as
// one that does.
func (c CodeRemoteCredentials) validate(path Path) error {
	var p problems
	for _, name := range slices.Sorted(maps.Keys(c)) {
		where := entry(path, name)
		if !envref.ValidName(name) || len(name) > codeMaxRemoteCredentialNameBytes {
			p.add(where, ErrShape,
				"%q is not a variable's name a mirror's or an import's ${NAME} can spell: a letter or an "+
					"underscore, then letters, digits and underscores, at most %d bytes", name,
				codeMaxRemoteCredentialNameBytes)
		}
		hosts := c[name].Hosts
		if len(hosts) == 0 {
			p.add(at(where, "hosts"), ErrMissing,
				"name the hosts this variable's value may be sent to: a variable declared for none is sent nowhere")
		}
		first := make(map[string]int, len(hosts))
		for i, h := range hosts {
			hostPath := idx(at(where, "hosts"), i)
			key, ok := declaredHostKey(h)
			if !ok {
				p.add(hostPath, ErrShape,
					"%q is not a host a remote's https URL names: a host name or an IP address in its one "+
						"written form, with an optional :port and nothing else, no scheme, user or path", h)
				continue
			}
			if j, twice := first[key]; twice {
				p.add(hostPath, ErrConflict, "%s is hosts[%d] too", h, j)
				continue
			}
			first[key] = i
		}
	}
	return p.err()
}

// validateAdmission holds code.remote_credentials' ADMISSION rules at path:
// at most [CodeMaxRemoteCredentials] variables, and at most
// [CodeMaxRemoteCredentialHosts] hosts each — typo guards, POLICY a later
// build is free to change, since a decide and a dial read any number of
// either ([Code.validateAdmission] says why such a rule is held on a write
// and only warned about on an apply).
func (c CodeRemoteCredentials) validateAdmission(path Path) error {
	var p problems
	if n := len(c); n > CodeMaxRemoteCredentials {
		p.add(path, ErrOutOfRange, "%d variables, past the %d code.remote_credentials may declare, a typo "+
			"guard: each is one token at one forge", n, CodeMaxRemoteCredentials)
	}
	for _, name := range slices.Sorted(maps.Keys(c)) {
		if n := len(c[name].Hosts); n > CodeMaxRemoteCredentialHosts {
			p.add(at(entry(path, name), "hosts"), ErrOutOfRange,
				"%d hosts, past the %d one variable may be sent to, a typo guard: a forge is reached under one "+
					"host, or a few", n, CodeMaxRemoteCredentialHosts)
		}
	}
	return p.err()
}

// declaredHostKey is a declared host as a remote's URL names it — the host,
// lowercase, or the address in its one written form, and the port, 443 where
// it writes none — read by the rule a remote's URL is read by
// ([gitref.ParseRemoteURL]), so a host is matched as the dial will reach it;
// false for text that is not a host alone.
func declaredHostKey(h string) (string, bool) {
	if h == "" || strings.ContainsAny(h, "/@?#\\ ") {
		return "", false
	}
	r, err := gitref.ParseRemoteURL("https://" + h + "/")
	if err != nil {
		return "", false
	}
	return remoteHostKey(r.Host, r.Port), true
}

// remoteHostKey is a host and its port in one written form: host:port, an
// IPv6 address in brackets.
func remoteHostKey(host string, port int) string { return net.JoinHostPort(host, strconv.Itoa(port)) }

// remoteHostWords is a host as a person writes it in the table: its port only
// where it is not https' own 443.
func remoteHostWords(host string, port int) string {
	switch {
	case port != 443:
		return remoteHostKey(host, port)
	case strings.Contains(host, ":"):
		return "[" + host + "]"
	}
	return host
}
