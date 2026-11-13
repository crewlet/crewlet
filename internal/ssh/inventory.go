package ssh

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/config"
)

// The engine's own inventory (ADR-0134): on a server that opts in, the engine
// runs the closed set config.SSHInventoryCommands for the atlas, each on a
// certificate signed for that one command and recorded as an agent's read is.
// What a command printed is parsed here into typed values and dropped: the
// parsers read the columns they keep and no other, so a container's command
// line and a socket's process never leave this file.

// InventoryCall is one inventory command on one server.
type InventoryCall struct {
	Use
	Host config.SSHHost
	// Command is one of Host.InventoryCommands, word for word.
	Command string
}

// Inventory runs call on a certificate signed for it alone, recorded first.
// A server that does not opt in to the command is never contacted and no
// certificate is signed for it.
func (r *Runner) Inventory(ctx context.Context, call InventoryCall) (Result, error) {
	if !slices.Contains(call.Host.InventoryCommands(), call.Command) {
		return Result{}, fmt.Errorf("%w: %s does not opt in to this inventory command", ErrRequest, call.Host.ID)
	}
	id := uuid.Must(uuid.NewV7()).String()
	cmd := accessdomain.NewCommand{
		ID: id, Handle: accessdomain.EngineHolder, Host: call.Host.ID, User: call.Host.Users.Read,
		Level: accessdomain.LevelRead, Command: call.Command,
	}
	minted := r.now().UTC()
	if call.CA == nil {
		return Result{}, r.unrun(ctx, minted, cmd, fmt.Errorf("%w: no authority", ErrRequest))
	}
	// THE COMMAND'S OWN ID NAMES ITS CERTIFICATE, so a server's log line
	// and the access log's two records are one thread.
	issued, err := Sign(call.CA, Request{Purpose: PurposeInventory, Nonce: id,
		Hosts: []string{call.Host.ID}, Level: LevelInventory, ForceCommand: call.Command,
		SourceAddresses: call.SourceAddresses, Now: r.now()})
	if err == nil {
		err = r.certs.RecordCertificate(ctx, issued.Record)
	}
	if err != nil {
		return Result{}, r.unrun(ctx, minted, cmd, err)
	}
	cmd.CertificateSerial = issued.Record.Serial
	return r.run(ctx, minted, cmd, call.Use, call.Host, call.Host.Users.Read, issued, Command{Line: call.Command})
}

// ErrInventoryOutput is output that is not the table its command prints.
var ErrInventoryOutput = errors.New("ssh: the output is not the table this command prints")

// ParseOpenPorts reads `ss -tln` output into the TCP ports the server listens
// on beyond itself, sorted, each once. A socket bound to a loopback address
// is not an open port and is left out. Only the state and the local address
// of a row are read: a process column, where a server's ss prints one, is
// never looked at.
func ParseOpenPorts(out []byte) ([]int, error) {
	lines := strings.Split(string(out), "\n")
	if len(lines) == 0 || !strings.HasPrefix(strings.TrimSpace(lines[0]), "State") {
		return nil, ErrInventoryOutput
	}
	var ports []int
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[0] != "LISTEN" {
			continue
		}
		local := fields[3]
		at := strings.LastIndexByte(local, ':')
		if at < 0 {
			continue
		}
		port, err := strconv.Atoi(local[at+1:])
		if err != nil || port < 1 || port > 65535 || loopback(local[:at]) {
			continue
		}
		ports = append(ports, port)
	}
	slices.Sort(ports)
	return slices.Compact(ports), nil
}

// loopback reports a bind address that only the server itself reaches. An
// address that does not parse is read as loopback, so a row the parser does
// not understand never becomes an open port.
func loopback(bind string) bool {
	bind = strings.TrimSuffix(strings.TrimPrefix(bind, "["), "]")
	if zone := strings.IndexByte(bind, '%'); zone >= 0 {
		bind = bind[:zone]
	}
	if bind == "*" {
		return false
	}
	addr, err := netip.ParseAddr(bind)
	return err != nil || addr.Unmap().IsLoopback()
}

// Container is one running container as the inventory keeps it. There is no
// field for its command, its ports or its state: none is read.
type Container struct {
	Name  string
	Image string
}

// containerName is a name Docker gives a container.
var containerName = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
})

// ParseContainers reads `docker ps` table output into each container's name
// and image, sorted by name. The table is cut by its header's columns, in
// runes as Docker pads it, and ONLY the IMAGE and NAMES cells are read: the
// COMMAND cell, which holds a container's command line and whatever
// credential somebody passed on it, is never copied out of the row. A row
// whose cells do not sit under the header is skipped whole and counted.
func ParseContainers(out []byte) (containers []Container, skipped int, err error) {
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	header := []rune(lines[0])
	image, command, names := column(header, "IMAGE"), column(header, "COMMAND"), column(header, "NAMES")
	if image <= 0 || command <= image || names <= command {
		return nil, 0, ErrInventoryOutput
	}
	for _, line := range lines[1:] {
		row := []rune(line)
		if len(strings.TrimSpace(line)) == 0 {
			continue
		}
		// A CELL STARTS AFTER A SPACE: a row where one does not is not
		// aligned with the header, and cutting it would read one cell's
		// text as another's.
		if len(row) <= names || row[image-1] != ' ' || row[command-1] != ' ' || row[names-1] != ' ' {
			skipped++
			continue
		}
		ref := strings.TrimSpace(string(row[image:command]))
		// Docker joins a linked container's aliases with commas; its own
		// name is the one without a slash.
		name := ""
		for _, n := range strings.Split(strings.TrimSpace(string(row[names:])), ",") {
			if !strings.Contains(n, "/") {
				name = n
				break
			}
		}
		if !containerName().MatchString(name) || ref == "" || strings.ContainsAny(ref, " \t\"'") {
			skipped++
			continue
		}
		containers = append(containers, Container{Name: name, Image: ref})
	}
	slices.SortFunc(containers, func(a, b Container) int { return strings.Compare(a.Name, b.Name) })
	return containers, skipped, nil
}

// column is where title starts in header, in runes, as a whole column title;
// -1 where the header has none.
func column(header []rune, title string) int {
	text := string(header)
	at := strings.Index(text, title)
	if at < 0 {
		return -1
	}
	return len([]rune(text[:at]))
}
