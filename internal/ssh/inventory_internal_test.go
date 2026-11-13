package ssh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/redact/redacttest"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/ssh/sshdtest"
)

// inventoryNow is the instant every inventory read here is made at.
var inventoryNow = time.Date(2026, 11, 12, 7, 0, 0, 0, time.UTC)

// commandSecret stands in every command line and process name these tables
// carry: nothing a parse keeps may hold a piece of it.
const commandSecret = "--db-password=hunter2-opensesame"

// portsTable is what `ss -tln` prints, with the process column a newer ss
// adds: two open ports, each on both families, and two loopback sockets.
const portsTable = `State   Recv-Q  Send-Q   Local Address:Port    Peer Address:Port  Process
LISTEN  0       4096     127.0.0.53%lo:53           0.0.0.0:*      users:(("resolved ` + commandSecret + `",pid=411,fd=14))
LISTEN  0       128            0.0.0.0:22           0.0.0.0:*      users:(("sshd",pid=702,fd=3))
LISTEN  0       511                  *:443                *:*      users:(("nginx ` + commandSecret + `",pid=911,fd=6))
LISTEN  0       244          127.0.0.1:5432         0.0.0.0:*
LISTEN  0       128               [::]:22              [::]:*
LISTEN  0       128              [::1]:6379            [::]:*
`

// dockerTable lays rows out as Docker's table writer does: each column as
// wide as its longest cell, counted in runes, and three spaces apart.
func dockerTable(rows ...[]string) string {
	widths := make([]int, len(rows[0]))
	for _, row := range rows {
		for i, cell := range row {
			widths[i] = max(widths[i], len([]rune(cell)))
		}
	}
	var b strings.Builder
	for _, row := range rows {
		line := ""
		for i, cell := range row {
			line += cell + strings.Repeat(" ", widths[i]-len([]rune(cell))+3)
		}
		b.WriteString(strings.TrimRight(line, " ") + "\n")
	}
	return b.String()
}

// containersHeader is the header `docker ps` prints.
var containersHeader = []string{"CONTAINER ID", "IMAGE", "COMMAND", "CREATED", "STATUS", "PORTS", "NAMES"}

// containersTable is what `docker ps` prints: a command cell cut with an
// ellipsis as Docker cuts it, and a linked container's aliases.
var containersTable = dockerTable(containersHeader,
	[]string{"3f4e5d6c7b8a", "ghcr.io/acme/api:1.4.2", `"/bin/api ` + commandSecret + `…"`, "2 days ago", "Up 2 days",
		"0.0.0.0:8080->8080/tcp, :::8080->8080/tcp", "acme-api"},
	[]string{"9a8b7c6d5e4f", "postgres:16", `"docker-entrypoint.s…"`, "3 weeks ago", "Up 3 weeks",
		"127.0.0.1:5432->5432/tcp", "acme-api/db,orders-db"},
)

// THE PARSERS KEEP PORTS, NAMES AND IMAGES AND NOTHING ELSE. A socket's
// process and a container's command line, which is where a credential passed
// as an argument sits, are never copied out of a row; a loopback socket is not
// an open port; and a linked container is named by its own name.
//
// Mutation: cut the image cell to the CREATED column, or read a socket's row
// past its local address, and the command line is in the answer.
func TestTheParsersDropTheCommandColumn(t *testing.T) {
	t.Parallel()
	ports, err := ParseOpenPorts([]byte(portsTable))
	if err != nil || !slices.Equal(ports, []int{22, 443}) {
		t.Fatalf("the open ports are %v, %v; want 22 and 443", ports, err)
	}
	containers, skipped, err := ParseContainers([]byte(containersTable))
	want := []Container{{Name: "acme-api", Image: "ghcr.io/acme/api:1.4.2"}, {Name: "orders-db", Image: "postgres:16"}}
	if err != nil || skipped != 0 || !slices.Equal(containers, want) {
		t.Fatalf("the containers are %+v (%d skipped), %v; want %+v", containers, skipped, err, want)
	}
	kept := fmt.Sprintf("%v %+v", ports, containers)
	for _, piece := range []string{commandSecret, "hunter2", "/bin/api", "docker-entrypoint", "nginx", "pid="} {
		if strings.Contains(kept, piece) {
			t.Errorf("%q, a piece of a command line, is in what the parse kept: %s", piece, kept)
		}
	}
	// A ROW THAT DOES NOT SIT UNDER THE HEADER IS SKIPPED WHOLE: cutting it
	// by the header's columns would read its command as its image.
	crooked := strings.Replace(containersTable, "3f4e5d6c7b8a   ghcr.io", "3f4e5d6c7b8a ghcr.io", 1)
	if crooked == containersTable {
		t.Fatal("the crooked table is the straight one")
	}
	containers, skipped, err = ParseContainers([]byte(crooked))
	if err != nil || skipped != 1 || len(containers) != 1 || containers[0].Name != "orders-db" {
		t.Errorf("a crooked row parsed as %+v (%d skipped), %v", containers, skipped, err)
	}
	for name, out := range map[string]string{"an error": "permission denied\n", "nothing": "",
		"a table without names": "CONTAINER ID   IMAGE   COMMAND\n"} {
		if _, _, err := ParseContainers([]byte(out)); !errors.Is(err, ErrInventoryOutput) {
			t.Errorf("%s read as containers: %v", name, err)
		}
		if _, err := ParseOpenPorts([]byte(out)); !errors.Is(err, ErrInventoryOutput) {
			t.Errorf("%s read as ports: %v", name, err)
		}
	}
}

// scripted answers each inventory command with what a server would print, and
// remembers every call it was handed.
type scripted struct {
	mu      sync.Mutex
	calls   []InventoryCall
	answers map[string]Result
	errs    map[string]error
}

func (s *scripted) Inventory(_ context.Context, call InventoryCall) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, call)
	key := call.Host.ID + " " + call.Command
	if err := s.errs[key]; err != nil {
		return Result{}, err
	}
	if res, ok := s.answers[key]; ok {
		return res, nil
	}
	return s.answers[call.Command], nil
}

// inventoryCompany is a company with three servers: web-1 opts in with
// containers, db-1 opts in without, and bastion does not opt in.
func inventoryCompany(t *testing.T) (collect.Input, *setup.SSHCertificateAuthority) {
	t.Helper()
	held, err := setup.GenerateSSHCertificateAuthority()
	if err != nil {
		t.Fatal(err)
	}
	ca, err := setup.ParseSSHCertificateAuthority(held)
	if err != nil {
		t.Fatal(err)
	}
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostKey, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostKey)))}
	users := config.SSHHostUsers{Read: "crewlet-read"}
	company := &config.Company{}
	company.Integrations.SSH = &config.SSH{
		Enabled:      true,
		Provisioning: &config.SSHProvisioning{CertificateAuthority: "${SSH_CERTIFICATE_AUTHORITY}"},
		Hosts: []config.SSHHost{
			{ID: "web-1", Address: "web-1.acme.example", HostKeys: keys, Users: users, Containers: true, Inventory: true},
			{ID: "db-1", Address: "db-1.acme.example", HostKeys: keys, Users: users, Inventory: true},
			{ID: "bastion", Address: "bastion.acme.example", HostKeys: keys, Users: users, Containers: true},
		},
	}
	env := config.NewResolver(config.MapSource{"SSH_CERTIFICATE_AUTHORITY": held})
	return collect.Input{Config: company, Env: env, Now: inventoryNow}, ca
}

func inventoryView(read collect.Read) atlas.View {
	return atlas.Join(atlas.Fold(read.Claims, inventoryNow), atlas.Joined{}, inventoryNow)
}

// CONTAINERS BECOME WORKLOADS ON THEIR SERVER, AND OPEN PORTS FACTS OF IT. The
// read runs the two commands on a server with containers and the ports alone
// on one without, claims each container by its name and image as running on
// the server, says the server's open ports, and never claims the server
// itself, which the settings hold.
func TestTheInventoryBecomesWorkloadsAndOpenPorts(t *testing.T) {
	t.Parallel()
	in, _ := inventoryCompany(t)
	run := &scripted{answers: map[string]Result{
		config.SSHInventoryPorts:      {Stdout: []byte(portsTable)},
		config.SSHInventoryContainers: {Stdout: []byte(containersTable)},
	}}
	read, err := NewAtlasSource(run).Collect(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	var ran []string
	for _, c := range run.calls {
		ran = append(ran, c.Host.ID+": "+c.Command)
	}
	if want := []string{"web-1: " + config.SSHInventoryPorts, "web-1: " + config.SSHInventoryContainers,
		"db-1: " + config.SSHInventoryPorts}; !slices.Equal(ran, want) {
		t.Fatalf("the read ran %q, want %q", ran, want)
	}
	if read.Calls != 3 || len(read.Unread) != 0 || len(read.Withheld) != 0 {
		t.Errorf("the read cost %d calls, %+v unread, %+v withheld", read.Calls, read.Unread, read.Withheld)
	}
	v := inventoryView(read)
	web := atlas.MustRef(atlas.KindHost, "ssh/web-1")
	for _, c := range read.Claims {
		if c.Subject.Kind == atlas.KindHost && c.Facet.Kind == atlas.FacetExists {
			t.Errorf("the read claims the server %s exists, which the settings hold", c.Subject)
		}
		if c.Source != atlas.IntegrationSource(string(integration.KindSSH)) {
			t.Errorf("a claim speaks as %+v", c.Source)
		}
	}
	api, ok := v.Find(atlas.MustRef(atlas.KindWorkload, "ssh/web-1/acme-api").ID())
	if !ok || api.Name != "acme-api" {
		t.Fatalf("the container is not an entry: %+v", api)
	}
	out, err := json.Marshal(read)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"ghcr.io/acme/api:1.4.2"`, `"postgres:16"`, `"runs_on"`, web.Key, `"open_port"`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("the read lacks %s: %s", want, out)
		}
	}
	var ports []string
	for _, c := range read.Claims {
		if c.Subject == web && c.Facet.Fact == atlas.FactOpenPort {
			ports = append(ports, c.Value.Text)
		}
	}
	if !slices.Equal(ports, []string{"22", "443"}) {
		t.Errorf("web-1's open ports are %v, want 22 and 443", ports)
	}
	// COMMAND LINES NEVER REACH THE ATLAS: nothing of a command or a process
	// is anywhere in what the read reports.
	for _, piece := range []string{commandSecret, "hunter2", "/bin/api", "docker-entrypoint", "nginx", "resolved"} {
		if strings.Contains(string(out), piece) {
			t.Errorf("%q, a piece of a command line, reached the read: %s", piece, out)
		}
	}
}

// refusingDial fails every connection and remembers the address asked for.
type refusingDial struct {
	mu    sync.Mutex
	asked []string
}

func (d *refusingDial) dial(_ context.Context, _, address string) (net.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.asked = append(d.asked, address)
	return nil, errors.New("connection refused")
}

// A SERVER NOT OPTED IN IS NEVER CONTACTED, AND EVERY RUN HAS ITS RECORD. Over
// the real runner, the read dials only the servers that opt in, signs one
// certificate per command for the engine itself and records it before it is
// used, and records every command, here as one that could not reach its
// server. The runner refuses a command a server does not opt in to before
// anything is signed, dialled or recorded.
//
// Mutation: read every registered server, or run a command before its
// certificate is recorded, and a case is red.
func TestAServerNotOptedInIsNeverContacted(t *testing.T) {
	t.Parallel()
	in, ca := inventoryCompany(t)
	a, d := &audit{}, &refusingDial{}
	run := NewRunner(a, a)
	run.dial = d.dial
	read, err := NewAtlasSource(run).Collect(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range d.asked {
		if strings.Contains(address, "bastion") {
			t.Errorf("the read dialled %s, a server that does not opt in", address)
		}
	}
	if len(d.asked) != 3 || len(a.certs) != 3 || len(a.cmds) != 3 {
		t.Fatalf("three commands dialled %v, with %d certificates and %d commands recorded",
			d.asked, len(a.certs), len(a.cmds))
	}
	for i, cert := range a.certs {
		cmd := a.cmds[i]
		if cert.Purpose != PurposeInventory || cert.Seat != accessdomain.EngineHolder || len(cert.HostIDs) != 1 ||
			cert.Principals[0] != Principal(cert.HostIDs[0], LevelInventory) ||
			cert.KeyID != "crewlet/_engine/inventory/"+cmd.ID ||
			cert.ValidBefore.Sub(cert.ValidAfter) != Validity(PurposeInventory)+clockSkew {
			t.Errorf("certificate %d is recorded as %+v", i, cert)
		}
		if cmd.Handle != accessdomain.EngineHolder || cmd.Host == "bastion" || cmd.Level != accessdomain.LevelRead ||
			cmd.Ran || cmd.Failure != accessdomain.FailureUnreachable || cmd.CertificateSerial != cert.Serial ||
			!slices.Contains(config.SSHInventoryCommands(), cmd.Command) {
			t.Errorf("command %d is recorded as %+v", i, cmd)
		}
	}
	// A SERVER THAT COULD NOT BE REACHED IS A FINDING, never a fault: each of
	// its parts keeps what an earlier read recorded.
	if len(read.Claims) != 0 || len(read.Unread) != 3 || read.Unread[0].Part != "web-1 ports" ||
		read.Unread[1].Part != "web-1 containers" || read.Unread[2].Part != "db-1 ports" {
		t.Fatalf("an unreachable fleet read as %+v, unread %+v", read.Claims, read.Unread)
	}
	workload, _ := atlas.Exists(atlas.MustRef(atlas.KindWorkload, "ssh/web-1/acme-api"),
		atlas.IntegrationSource("ssh"), inventoryNow)
	other, _ := atlas.Exists(atlas.MustRef(atlas.KindWorkload, "ssh/web-10/acme-api"),
		atlas.IntegrationSource("ssh"), inventoryNow)
	if !read.Unread[1].Holds(workload) || read.Unread[1].Holds(other) || read.Unread[0].Holds(workload) {
		t.Error("a server's unread containers do not hold exactly that server's workloads")
	}

	bastion, _ := in.Config.Integrations.SSH.Host("bastion")
	before := len(d.asked)
	_, err = run.Inventory(t.Context(), InventoryCall{Use: Use{CA: ca}, Host: bastion, Command: config.SSHInventoryPorts})
	if !errors.Is(err, ErrRequest) || len(d.asked) != before || len(a.certs) != 3 || len(a.cmds) != 3 {
		t.Errorf("a server that does not opt in: %v, dialled %v", err, d.asked[before:])
	}
	// AND A SERVER WITHOUT CONTAINERS OPTS IN TO THE PORTS ALONE.
	db, _ := in.Config.Integrations.SSH.Host("db-1")
	_, err = run.Inventory(t.Context(), InventoryCall{Use: Use{CA: ca}, Host: db, Command: config.SSHInventoryContainers})
	if !errors.Is(err, ErrRequest) || len(d.asked) != before {
		t.Errorf("the containers of a server without them: %v", err)
	}
}

// WHAT STOPPED A PART IS A FINDING IN THE SOURCE'S OWN WORDS, and the engine's
// own fault fails the read whole: a gate's refusal, a failed command, output
// past the limit and output that is no table each leave the part unread, with
// nothing claimed from it, and a command whose record did not land reads
// nothing into the atlas at all.
func TestAnInventoryPartThatCouldNotBeReadIsAFinding(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		res  Result
		err  error
		want string
	}{
		"the gate refused it":   {res: Result{ExitCode: GateRefused}, want: "gate refused"},
		"the command failed":    {res: Result{ExitCode: 1, Stdout: []byte(containersTable)}, want: "status 1"},
		"it printed past limit": {res: Result{Truncated: true, Stdout: []byte(containersTable)}, want: "output limit"},
		"it printed no table":   {res: Result{Stdout: []byte("Cannot connect to the Docker daemon\n")}, want: "other than its table"},
		"the server refused the certificate": {
			err: failed(accessdomain.FailureAuthenticationRefused, errors.New("ssh: handshake failed")), want: "server setup again"},
	} {
		in, _ := inventoryCompany(t)
		run := &scripted{
			answers: map[string]Result{config.SSHInventoryPorts: {Stdout: []byte(portsTable)},
				"web-1 " + config.SSHInventoryContainers: c.res},
			errs: map[string]error{"web-1 " + config.SSHInventoryContainers: c.err},
		}
		read, err := NewAtlasSource(run).Collect(t.Context(), in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(read.Unread) != 1 || read.Unread[0].Part != "web-1 containers" ||
			!strings.Contains(read.Unread[0].Detail, c.want) {
			t.Errorf("%s: unread %+v, want the containers of web-1 and %q", name, read.Unread, c.want)
		}
		for _, claim := range read.Claims {
			if claim.Subject.Kind == atlas.KindWorkload {
				t.Errorf("%s: a workload was claimed from a part that was not read: %+v", name, claim)
			}
		}
	}
	in, _ := inventoryCompany(t)
	run := &scripted{errs: map[string]error{"web-1 " + config.SSHInventoryPorts: fmt.Errorf("%w: log down", ErrUnrecorded)}}
	if read, err := NewAtlasSource(run).Collect(t.Context(), in); !errors.Is(err, ErrUnrecorded) || len(read.Claims) != 0 {
		t.Errorf("a command whose record did not land read %+v, %v", read.Claims, err)
	}
	// A COMPANY WHOSE SERVERS ALL STAY OUT is read whole and empty, with the
	// authority never opened; one without SSH is not configured.
	out := *in.Config.Integrations.SSH
	out.Hosts = []config.SSHHost{{ID: "bastion", Users: config.SSHHostUsers{Read: "crewlet-read"}}}
	in.Config = &config.Company{}
	in.Config.Integrations.SSH = &out
	in.Env = config.NewResolver(config.MapSource{"SSH_CERTIFICATE_AUTHORITY": "not a key"})
	none := &scripted{}
	if read, err := NewAtlasSource(none).Collect(t.Context(), in); err != nil || len(none.calls) != 0 || len(read.Claims) != 0 {
		t.Errorf("a company with no server opted in read %+v, %v, after %d calls", read, err, len(none.calls))
	}
	in.Config.Integrations.SSH.Enabled = false
	if _, err := NewAtlasSource(none).Collect(t.Context(), in); !errors.Is(err, integration.ErrNotConfigured) {
		t.Errorf("SSH switched off: %v, want not configured", err)
	}
}

// NO CREDENTIAL SHAPE REACHES THE ATLAS FROM A SERVER. Every known shape is
// planted in every cell of both tables, the cells the parse keeps and the ones
// it drops: none of it is in the read, each kept cell that held one is
// withheld and counted, and the container beside it is still read.
//
// Mutation: record a container's name or image without the builder's grammar,
// or keep any other cell, and a shape reaches the read.
func TestNoCredentialShapeReachesTheAtlasFromAServer(t *testing.T) {
	t.Parallel()
	for _, shape := range redacttest.Shapes() {
		s := shape.Sample
		ports := "State   Recv-Q  Send-Q   Local Address:Port    Peer Address:Port  Process\n" +
			"LISTEN  0       128            0.0.0.0:22           0.0.0.0:*      users:((\"" + s + "\",pid=1,fd=3))\n" +
			"LISTEN  " + s + "  " + s + "  " + s + ":443  " + s + ":*  " + s + "\n" +
			"LISTEN  0       128            0.0.0.0:" + s + "    0.0.0.0:*\n" +
			s + "  0  128  0.0.0.0:8443  0.0.0.0:*\n"
		containers := dockerTable(containersHeader,
			[]string{s, s, s, s, s, s, s},
			[]string{"3f4e5d6c7b8a", "ghcr.io/acme/api:" + s, `"` + s + `"`, "2 days ago", "Up 2 days", s, "api-" + s},
			[]string{"9a8b7c6d5e4f", s, `"postgres"`, "2 days ago", "Up 2 days", "", "orders-db"},
			[]string{"1a2b3c4d5e6f", "postgres:16", `"postgres -c ` + s + `"`, s, s, s, "sessions-db"},
		)
		in, _ := inventoryCompany(t)
		run := &scripted{answers: map[string]Result{
			config.SSHInventoryPorts:      {Stdout: []byte(ports)},
			config.SSHInventoryContainers: {Stdout: []byte(containers)},
		}}
		read, err := NewAtlasSource(run).Collect(t.Context(), in)
		if err != nil {
			t.Fatalf("%s: %v", shape.Name, err)
		}
		out, err := json.Marshal(read)
		if err != nil {
			t.Fatal(err)
		}
		for _, piece := range []string{s[len(s)/2:], s[len(s)/4 : len(s)/4+12]} {
			if strings.Contains(string(out), piece) {
				t.Errorf("%s: a piece of the shape reached the read: %s", shape.Name, out)
			}
		}
		// A shape with a line break in it breaks every row that carries it,
		// so those rows are skipped rather than read.
		if strings.Contains(s, "\n") {
			continue
		}
		if len(read.Withheld) < 3 {
			t.Errorf("%s: %d values withheld, want the three rows that carried the shape in a kept cell: %+v",
				shape.Name, len(read.Withheld), read.Withheld)
		}
		if _, ok := inventoryView(read).Find(atlas.MustRef(atlas.KindWorkload, "ssh/web-1/sessions-db").ID()); !ok {
			t.Errorf("%s: the container beside the credential was not read: %s", shape.Name, out)
		}
	}
}

// installInventoryStandIns puts an ss and a docker on the server that print
// the tables above, since the test image carries neither.
func (r rig) installInventoryStandIns(t *testing.T) {
	t.Helper()
	for path, table := range map[string]string{"/usr/bin/ss": portsTable, "/usr/bin/docker": containersTable} {
		script := "#!/bin/sh\ncat <<'CREWLET_TABLE'\n" + table + "CREWLET_TABLE\n"
		if out, err := r.srv.Exec([]byte(script), "sh", "-c", "cat >"+path+" && chmod 0755 "+path); err != nil {
			t.Fatalf("install %s: %v\n%s", path, err, out)
		}
	}
}

// AN INVENTORY CERTIFICATE RUNS THE COMMAND IT NAMES AND NO OTHER, on a real
// server set up with the inventory on. The engine's own run prints the tables
// and is recorded; a client that holds the certificate and asks for another
// command, an allowlisted read included, gets the certificate's command
// instead; no certificate is signed for the inventory's principal with any
// other command; an agent's read certificate still runs its own reads; and
// Test connection proves the binding as its own step.
//
// Mutation: let the gate run what the client asked for under a certificate
// that names a command, or sign an inventory certificate for any command, and
// a case is red.
func TestAnInventoryCertificateRunsOnlyTheCommandItNames(t *testing.T) {
	t.Parallel()
	r := newRig(t, sshdtest.Images[1], "web-1", false, func(h *config.SSHHost) { h.Containers, h.Inventory = true, true })
	r.installInventoryStandIns(t)
	a := &audit{}
	run := r.runner(a)
	for command, want := range map[string]string{config.SSHInventoryPorts: portsTable, config.SSHInventoryContainers: containersTable} {
		res, err := run.Inventory(t.Context(), InventoryCall{Use: Use{CA: r.ca}, Host: r.host, Command: command})
		if err != nil || res.ExitCode != 0 || string(res.Stdout) != want {
			t.Fatalf("%q: exit %d, %v, printed %q\n%s", command, res.ExitCode, err, res.Stdout, res.Stderr)
		}
	}
	if len(a.certs) != 2 || len(a.cmds) != 2 || !a.cmds[0].Ran || a.cmds[0].Handle != accessdomain.EngineHolder ||
		a.cmds[0].CertificateSerial != a.certs[0].Serial || a.cmds[0].OutputSHA256 == "" {
		t.Fatalf("two inventory commands are recorded as %+v under %+v", a.cmds, a.certs)
	}
	if log := r.srv.Log(); !strings.Contains(log, "crewlet/_engine/inventory/"+a.cmds[0].ID) {
		t.Errorf("the server's log does not name the inventory's certificate:\n%s", log)
	}
	bound := r.sign(t, Request{Purpose: PurposeInventory, Nonce: "n-1", Hosts: []string{r.host.ID},
		Level: LevelInventory, ForceCommand: config.SSHInventoryPorts, Now: time.Now()})
	for _, asked := range []string{"uptime", "crewlet-gate version", "cat /etc/shadow", config.SSHInventoryContainers} {
		if res := r.run(t, "crewlet-read", bound.Signer, asked, nil); res.ExitCode != 0 || string(res.Stdout) != portsTable {
			t.Errorf("asked for %q under the ports certificate: exit %d, printed %q", asked, res.ExitCode, res.Stdout)
		}
	}
	if _, err := r.client.Run(t.Context(), r.host, "crewlet-ops", bound.Signer, Command{Line: "id"}); FailureOf(err) !=
		accessdomain.FailureAuthenticationRefused {
		t.Errorf("the operate user took the inventory's certificate: %v", err)
	}
	for _, force := range []string{"/usr/bin/uptime", "/usr/bin/ss -tlnp", "/usr/bin/sudo -n /usr/bin/docker inspect web",
		OperateCommand([]byte("id")), ""} {
		_, err := Sign(r.ca, Request{Purpose: PurposeInventory, Nonce: "n-2", Hosts: []string{r.host.ID},
			Level: LevelInventory, ForceCommand: force, Now: time.Now()})
		if !errors.Is(err, ErrRequest) {
			t.Errorf("an inventory certificate was signed for %q: %v", force, err)
		}
	}
	if res := r.run(t, "crewlet-read", r.read(t).Signer, "uptime", nil); res.ExitCode != 0 {
		t.Errorf("an agent's read on an inventory server: exit %d, %q", res.ExitCode, res.Stderr)
	}
	steps := r.client.TestConnection(t.Context(), TestInput{Host: r.host, CA: r.ca, Person: "ops-lead", Now: time.Now()},
		&fakeRecorder{})
	if i := slices.IndexFunc(steps, func(s TestStep) bool { return s.ID == StepInventory }); i < 0 || !steps[i].OK ||
		!steps[len(steps)-1].OK {
		t.Errorf("Test connection on an inventory server: %+v", steps)
	}
}

// A SERVER WHOSE SETUP RAN WITHOUT THE INVENTORY REFUSES ITS CERTIFICATE: the
// switch in the settings reaches a server only when its setup is run again,
// so until then the engine's run is refused at sign-in and recorded as such,
// and switching it off and running the setup again takes it away.
func TestAServerSetUpWithoutTheInventoryRefusesIt(t *testing.T) {
	t.Parallel()
	r := newRig(t, sshdtest.Images[1], "web-1", false, nil)
	r.installInventoryStandIns(t)
	opted := r.host
	opted.Inventory = true
	call := InventoryCall{Use: Use{CA: r.ca}, Host: opted, Command: config.SSHInventoryPorts}
	a := &audit{}
	if _, err := r.runner(a).Inventory(t.Context(), call); FailureOf(err) != accessdomain.FailureAuthenticationRefused ||
		len(a.cmds) != 1 || a.cmds[0].Ran || a.cmds[0].Failure != accessdomain.FailureAuthenticationRefused {
		t.Fatalf("a server not set up for the inventory: %v, recorded %+v", err, a.cmds)
	}
	r.host = opted
	r.setup(t)
	if res, err := r.runner(a).Inventory(t.Context(), call); err != nil || string(res.Stdout) != portsTable {
		t.Fatalf("after its setup ran again: %v, printed %q", err, res.Stdout)
	}
	r.host.Inventory = false
	r.setup(t)
	if _, err := r.runner(a).Inventory(t.Context(), call); FailureOf(err) != accessdomain.FailureAuthenticationRefused {
		t.Errorf("after the inventory was switched off and the setup ran again: %v", err)
	}
	if out, err := r.srv.Exec(nil, "sh", "-c", "ls /etc/ssh/crewlet/"); err != nil || strings.Contains(out, "inventory") {
		t.Errorf("the inventory's allowlist is still on the server: %s, %v", out, err)
	}
}
