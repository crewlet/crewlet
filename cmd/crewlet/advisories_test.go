package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// advisoryLine is one config_advisory record as a case reads it.
type advisoryLine struct {
	Level  string `json:"level"`
	Msg    string `json:"msg"`
	Config string `json:"config"`
	Path   string `json:"path"`
	Detail string `json:"detail"`
}

// loggedAdvisories is every line [logBootstrapAdvisories] writes for boot.
//
// It FAILS ON ANY OTHER LINE, so a case holds the whole of what the function
// wrote rather than searching it for what it hoped to find.
func loggedAdvisories(t *testing.T, boot *config.Bootstrap) []advisoryLine {
	t.Helper()
	var buf bytes.Buffer
	logBootstrapAdvisories(slog.New(slog.NewJSONHandler(&buf, nil)), boot)
	lines := advisoryLines(t, buf.Bytes())
	if got, want := len(lines), bytes.Count(buf.Bytes(), []byte("\n")); got != want {
		t.Fatalf("%d of %d lines are config_advisory:\n%s", got, want, buf.String())
	}
	return lines
}

// advisoryLines decodes the config_advisory records of a JSON log, in the
// order they were written, skipping every other record.
func advisoryLines(t *testing.T, log []byte) []advisoryLine {
	t.Helper()
	var out []advisoryLine
	scanner := bufio.NewScanner(bytes.NewReader(log))
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		var line advisoryLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatalf("a log line is not JSON: %q: %v", scanner.Text(), err)
		}
		if line.Msg == "config_advisory" {
			out = append(out, line)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("reading the log: %v", err)
	}
	return out
}

// advisoryPaths is the path of each line, in order.
func advisoryPaths(lines []advisoryLine) []string {
	paths := make([]string, len(lines))
	for i, line := range lines {
		paths[i] = line.Path
	}
	return paths
}

// EVERY TIER A ADVISORY IS ONE LINE AT WARN, IN THE ORDER THE DOCUMENT RAISES
// THEM, carrying what a person acts on: which tier, where the remedy is
// written, and the whole message.
//
// The document raises three, chosen to span the list: the backup owner it
// leaves unnamed, which [config.Bootstrap.Warnings] raises among its own
// checks, and the two API postures [config.API]'s warnings add at the end —
// an acknowledged insecure posture, and plain http on an address a browser
// reaches over the network. Named here rather than only read back from
// Warnings, so the case cannot pass on a document that raises nothing.
func TestEveryTierAAdvisoryIsLoggedOnceInOrder(t *testing.T) {
	t.Parallel()
	boot, err := parseTierA([]byte("api:\n  port: 8080\n"+
		"  external_url: http://crewlet.internal.example\n"+
		"  auth:\n    admin: serve\n    totp: optional\n    accept_insecure: true\n"+
		"    tokens:\n      - {id: founder, token: "+cliFixtureToken+", grants: [admin]}\n"),
		config.EnvOnly())
	if err != nil {
		t.Fatalf("expected a valid Tier A document, got: %v", err)
	}

	got := loggedAdvisories(t, boot)
	if want := []string{"retention.backup_owner", "api.auth.accept_insecure",
		"api.external_url"}; !slices.Equal(advisoryPaths(got), want) {
		t.Fatalf("logged advisories at %v, want %v", advisoryPaths(got), want)
	}
	// AND EACH IS THE WARNING ITSELF, field for field: the message whole,
	// because a detail cut short names the consequence without the remedy.
	warnings := boot.Warnings()
	for i, line := range got {
		want := advisoryLine{Level: slog.LevelWarn.String(), Msg: "config_advisory",
			Config: "bootstrap", Path: warnings[i].Path, Detail: warnings[i].Message}
		if line != want {
			t.Errorf("line %d = %+v, want %+v", i, line, want)
		}
	}
}

// AND A DEPLOYMENT THAT HAS SAID EVERYTHING LOGS NOTHING — the fully stated
// document of internal/config's own counterfactual, a keyring, a durable
// stream and a backup owner. A start that always warns is one nobody reads.
func TestAFullyStatedTierALogsNoAdvisory(t *testing.T) {
	t.Parallel()
	boot, err := parseTierA([]byte("retention:\n  backup_owner: platform-oncall\n"),
		config.EnvOnly())
	if err != nil {
		t.Fatalf("expected a valid Tier A document, got: %v", err)
	}
	if got := loggedAdvisories(t, boot); len(got) != 0 {
		t.Errorf("a fully stated deployment logged %+v", got)
	}
}

// THE ADVISORIES ARE JUDGED ON THE DOCUMENT THE RUN APPLIES, with every run
// flag folded into it. Judged on the file alone, a fleet node given `-roles`
// would be told its roles are undeclared, and a node started with `-debug`
// would be told its broker's debug output has nowhere to go — and to pass
// `-debug`.
//
// Each document raises exactly one advisory without its flag, so a fold that
// stopped reaching the document fails the flagged half here, and a document
// that stopped raising it fails the other half rather than passing both.
func TestTheRunFlagsAreFoldedInBeforeTheAdvisoriesAreJudged(t *testing.T) {
	t.Parallel()
	const fleet = "coordination:\n  type: embedded-kv\n" +
		"stream:\n  store_dir: /var/lib/crewlet/stream\n  replicas: 3\n" +
		"  cluster:\n    name: crewlet\n    port: 6222\n" +
		"    peers: [\"nats://node-b.internal:6222\", \"nats://node-c.internal:6222\"]\n" +
		"retention:\n  backup_owner: platform-oncall\n"
	const verboseBroker = "stream:\n  store_dir: /var/lib/crewlet/stream\n  debug: true\n" +
		"retention:\n  backup_owner: platform-oncall\n"

	for _, tc := range []struct {
		name, doc string
		// logging and node are the arguments of the two folds runEngine
		// makes, each parsed through the flags as the command declares
		// them ([runFlags], [parseOverrides]).
		logging, node []string
		want          []string
		// omits is text no line's detail may carry: a remedy that would
		// not work on the document the run applies.
		omits string
	}{
		{"a fleet node that left its roles out", fleet, nil, nil, []string{"node.roles"}, ""},
		{"-roles names them", fleet, nil, []string{"-roles", "data,seats,workers"}, nil, ""},
		{"a verbose broker nothing records", verboseBroker, nil, nil, []string{"stream.debug"}, ""},
		{"-debug records it", verboseBroker, []string{"-debug"}, nil, nil, ""},
		{"-log-level debug records it", verboseBroker, []string{"-log-level", "debug"}, nil, nil, ""},
		{"-log-level warn does not", verboseBroker, []string{"-log-level", "warn"}, nil,
			[]string{"stream.debug"}, ""},
		// THE FOLD IS EXACT, not "the flag was given so stay quiet": with
		// stderr handed to a file that keeps a level of its own, `-debug`
		// raises a console that is not a destination, and nothing records
		// what the broker says. And the line names the file's level as
		// the cause rather than telling a `-debug` run to pass `-debug`.
		{"-debug behind a quieter file that holds the stream",
			verboseBroker + "logging:\n  stderr: false\n" +
				"  file:\n    path: /var/log/crewlet/node.log\n    level: warn\n",
			[]string{"-debug"}, nil, []string{"stream.debug"}, "-debug"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			boot, err := parseTierA([]byte(tc.doc), config.EnvOnly())
			if err != nil {
				t.Fatalf("expected a valid Tier A document, got: %v", err)
			}
			fs, level, format, logFile, debug := runFlags(t, tc.logging...)
			if err = overrideLogging(boot, fs, level, format, debug, logFile); err != nil {
				t.Fatalf("overrideLogging: %v", err)
			}
			nodeFlags, roles, host, port := parseOverrides(t, tc.node)
			if err = overrideNode(boot, nodeFlags, roles, host, port); err != nil {
				t.Fatalf("overrideNode: %v", err)
			}
			lines := loggedAdvisories(t, boot)
			if got := advisoryPaths(lines); !slices.Equal(got, tc.want) {
				t.Errorf("logged advisories at %v, want %v", got, tc.want)
			}
			for _, line := range lines {
				if tc.omits != "" && strings.Contains(line.Detail, tc.omits) {
					t.Errorf("the %s advisory offers %s, which would not change "+
						"what this run records: %s", line.Path, tc.omits, line.Detail)
				}
			}
		})
	}
}

// A FLAG THAT BREAKS A TIER A RULE IS REFUSED HERE, NAMING THE FLAG — every
// rule, not only the two [overrideNode] re-runs for the node flags. `-api-port`
// on a document with no `api:` block turns on an API the document does not
// describe, and a node serving one must name `api.external_url`.
//
// And the refusal NORMALIZES what passes, as Tier A's validation always does,
// so the bind the dev principal is checked against afterwards is the one the
// node binds.
func TestTheFoldedDocumentIsHeldToEveryTierARule(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		args   []string
		refuse string
	}{
		{"an API flag on a document that describes none",
			[]string{"-api-port", "8080"}, "api.external_url"},
		{"no flag: the file passed as it loaded", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			boot, err := parseTierA([]byte("retention:\n  backup_owner: platform-oncall\n"),
				config.EnvOnly())
			if err != nil {
				t.Fatalf("expected a valid Tier A document, got: %v", err)
			}
			fs, roles, host, port := parseOverrides(t, tc.args)
			if err = overrideNode(boot, fs, roles, host, port); err != nil {
				t.Fatalf("overrideNode refused it, so this case no longer "+
					"reaches validateFolded: %v", err)
			}
			err = validateFolded(boot, fs)
			switch {
			case tc.refuse == "" && err != nil:
				t.Fatalf("validateFolded: %v", err)
			case tc.refuse != "" && err == nil:
				t.Fatalf("%v was accepted on a document that describes no API", tc.args)
			case tc.refuse != "" && (!strings.Contains(err.Error(), tc.refuse) ||
				!strings.HasPrefix(err.Error(), tc.args[0]+": ")):
				t.Errorf("the refusal = %v, want it to name %s and the rule %s",
					err, tc.args[0], tc.refuse)
			}
		})
	}

	t.Run("what passes is normalized", func(t *testing.T) {
		t.Parallel()
		boot, err := parseTierA([]byte("retention:\n  backup_owner: platform-oncall\n"),
			config.EnvOnly())
		if err != nil {
			t.Fatalf("expected a valid Tier A document, got: %v", err)
		}
		fs, roles, host, port := parseOverrides(t, []string{"-api-host", " 127.0.0.1 "})
		if err = overrideNode(boot, fs, roles, host, port); err != nil {
			t.Fatalf("overrideNode: %v", err)
		}
		if err = validateFolded(boot, fs); err != nil {
			t.Fatalf("validateFolded: %v", err)
		}
		if boot.API.Host != "127.0.0.1" {
			t.Errorf("api.host = %q after validation, want the trimmed bind", boot.API.Host)
		}
	})
}

// AND `crewlet run` ITSELF LOGS THEM: once its flags are folded in, and into
// the log file it attached — the durable record a node nobody watched is
// read from.
//
// The document leaves its backup owner unnamed and asks the embedded broker
// for debug output; the run passes `-debug`, so the one advisory it may log is
// the owner's. The run is made to stop at the step after the advisories, by a
// CREWLET_NODE_ID no node may carry, which is a refusal of the ENVIRONMENT
// and not of the file or the flags, so nothing earlier refuses it first and
// nothing is opened.
//
// Mutation: drop the call from runEngine and no line is logged; move it ahead
// of the logging fold and the stream.debug advisory joins the owner's; move
// it ahead of the log file and the file holds none.
//
// Not parallel: it reconfigures the process-wide logger and sets the
// process's environment. See [TestMain].
func TestRunLogsTheAdvisoriesOfTheDocumentItApplies(t *testing.T) {
	t.Cleanup(func() {
		silenceProcessLog()
	})
	t.Setenv(config.NodeIDEnvVar, "not a node id")
	dir := t.TempDir()
	boot := writeFile(t, dir, "crewlet.yaml", "store:\n  path: "+
		filepath.Join(dir, "crewlet.db")+"\n"+
		fixtureStream(dir)+"  debug: true\n"+fixtureKeyring)
	logFile := filepath.Join(dir, "node.log")

	err := runEngine([]string{"-config", boot,
		"-company", companyFile(t, dir, "company.yaml", nil),
		"-debug", "-log-format", "json", "-log-file", logFile}, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "node.id") {
		t.Fatalf("runEngine = %v, want the refusal of %s", err, config.NodeIDEnvVar)
	}

	body, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("reading the log file: %v", err)
	}
	got := advisoryLines(t, body)
	if want := []string{"retention.backup_owner"}; !slices.Equal(advisoryPaths(got), want) {
		t.Fatalf("the run logged advisories at %v, want %v:\n%s", advisoryPaths(got), want, body)
	}
	if line := got[0]; line.Level != slog.LevelWarn.String() || line.Config != "bootstrap" ||
		!strings.Contains(line.Detail, "backup owner") {
		t.Errorf("the advisory is %+v, want a WARN naming the bootstrap and the remedy", line)
	}
	// IN THE FILE AFTER IT OPENED AND BEFORE THE FAILURE THAT ENDED THE RUN.
	order := logOrder(string(body), "log_file_opened", "config_advisory", "run_failed")
	if !slices.IsSorted(order) || slices.Contains(order, -1) {
		t.Errorf("log_file_opened, config_advisory and run_failed logged at lines %v", order)
	}
}

// AND NOT BESIDE A REFUSAL A FLAG DREW. `-api-port` on a document with no
// `api:` block turns on an API the document does not describe, which Tier A's
// rules refuse — so the run stops naming the flag, and the log holds the
// failure and no advisory about a document this node was never going to run.
//
// CREWLET_NODE_ID is one no node may carry, as in the case above, so a run
// that skipped the check stops at the step after the advisories rather than
// opening anything — and fails here on the lines it logged and the refusal it
// returned instead.
//
// Not parallel: it reconfigures the process-wide logger and sets the
// process's environment. See [TestMain].
func TestRunLogsNoAdvisoryBesideARefusalItsFlagsDraw(t *testing.T) {
	t.Cleanup(func() {
		silenceProcessLog()
	})
	t.Setenv(config.NodeIDEnvVar, "not a node id")
	dir := t.TempDir()
	boot := writeFile(t, dir, "crewlet.yaml", "store:\n  path: "+
		filepath.Join(dir, "crewlet.db")+"\n"+
		fixtureStream(dir)+fixtureKeyring)
	logFile := filepath.Join(dir, "node.log")

	err := runEngine([]string{"-config", boot,
		"-company", companyFile(t, dir, "company.yaml", nil),
		"-api-port", "8080", "-log-format", "json", "-log-file", logFile}, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "api.external_url") ||
		!strings.Contains(err.Error(), "-api-port") {
		t.Fatalf("runEngine = %v, want the api.external_url refusal naming -api-port", err)
	}

	body, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("reading the log file: %v", err)
	}
	if got := advisoryLines(t, body); len(got) != 0 {
		t.Errorf("a refused run logged advisories at %v:\n%s", advisoryPaths(got), body)
	}
	if order := logOrder(string(body), "run_failed"); order[0] == -1 {
		t.Errorf("the refusal is not in the log file:\n%s", body)
	}
}
