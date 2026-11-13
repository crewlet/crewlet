package ssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/setup"
)

// Certificates, signed per use with a key pair made in memory for that use
// alone. A certificate's key ID names who it is for, its principals the hosts
// and level it reaches, and it carries no extension: no PTY, no forwarding of
// any kind, no user rc. Operate certificates carry the one command a person
// approved as their force-command, so the server runs that and nothing else.

// Purpose is what a certificate is for. The values are stored in the audit.
type Purpose string

// The purposes.
const (
	PurposeRead    Purpose = "read"
	PurposeBoxRead Purpose = "box_read"
	PurposeOperate Purpose = "operate"
	PurposeTest    Purpose = "test"
	// PurposeInventory is the engine's own read of a server that opted in,
	// for the atlas: one certificate per command, bound to that command.
	PurposeInventory Purpose = "inventory"
)

// Valid reports whether p is a purpose this build signs for.
func (p Purpose) Valid() bool {
	return slices.Contains([]Purpose{PurposeRead, PurposeBoxRead, PurposeOperate, PurposeTest, PurposeInventory}, p)
}

// Validity is how long a certificate of purpose is valid after its signing.
// Each is as short as its use allows: a read turn engine-side, a box's turn
// between its renewals, one approved command, one press of Test connection,
// one inventory command, which needs its certificate only to sign in.
func Validity(p Purpose) time.Duration {
	switch p {
	case PurposeBoxRead:
		return 30 * time.Minute
	case PurposeOperate:
		return 5 * time.Minute
	case PurposeTest, PurposeInventory:
		return 2 * time.Minute
	default:
		return 15 * time.Minute
	}
}

// clockSkew backdates every certificate so a server whose clock is behind the
// engine's accepts it at once.
const clockSkew = 60 * time.Second

// Principal is the principal a host's user accepts at level.
func Principal(hostID, level string) string { return "crewlet." + hostID + "." + level }

// LevelInventory is the principal word of the engine's own inventory: never a
// level a seat holds, so no agent's certificate carries it, and a server
// accepts it only once its setup was run with the inventory switched on.
const LevelInventory = "inventory"

// OperateCommand is the force-command an operate certificate binds: the gate,
// handed the approved command as standard base64, which the gate decodes and
// runs. Base64 keeps every byte of the command out of the server's own
// parsing of the option.
func OperateCommand(command []byte) string {
	return GatePath + " operate " + base64.StdEncoding.EncodeToString(command)
}

// GateVersionCommand is the force-command a test operate certificate binds.
const GateVersionCommand = GatePath + " version"

// Request is one certificate to sign.
type Request struct {
	Purpose Purpose

	// Seat and Turn name the agent and its turn, HeldCall the approved call
	// an operate certificate runs; Person and Nonce name a test's press, and
	// Nonce alone one inventory command, which is nobody's.
	Seat, Turn, HeldCall string
	Person, Nonce        string

	// Hosts are the hosts the certificate reaches, by id, at Level.
	Hosts []string
	Level string

	// ForceCommand binds the command the server runs; required for an
	// operate certificate and an inventory one, refused on a read one, whose
	// server forces its own.
	ForceCommand string

	// SourceAddresses, where set, are the only addresses it is used from.
	SourceAddresses []string

	// Now is the signing time.
	Now time.Time
}

// Issued is a signed certificate with the in-memory key it certifies, and the
// facts the audit records.
type Issued struct {
	Certificate *ssh.Certificate
	// Signer authenticates as the certificate. Its private key exists only
	// here and is never written anywhere.
	Signer ssh.Signer
	// Record is the audit's account of it.
	Record CertificateRecord
}

// CertificateRecord is what the audit keeps of one certificate: never a key.
// Seat is the agent it was signed for, the person who pressed a test, or
// accessdomain.EngineHolder for the engine's own inventory.
type CertificateRecord struct {
	Serial          uint64
	KeyID           string
	Seat, Turn      string
	HeldCall        string
	Purpose         Purpose
	HostIDs         []string
	Principals      []string
	ValidAfter      time.Time
	ValidBefore     time.Time
	PublicKeySHA256 string
	CASHA256        string
}

// ErrRequest is a request no certificate is signed for.
var ErrRequest = errors.New("ssh: no certificate is signed for this request")

// KeyID is the key ID a request's certificate carries, which the server logs.
func (r Request) KeyID() (string, error) {
	part := func(s string) bool { return s != "" && !strings.ContainsAny(s, "/\" \t\r\n") }
	switch r.Purpose {
	case PurposeRead, PurposeBoxRead:
		if part(r.Seat) && part(r.Turn) {
			return KeyIDPrefix(r.Seat) + "/" + r.Turn, nil
		}
	case PurposeOperate:
		if part(r.Seat) && part(r.Turn) && part(r.HeldCall) {
			return KeyIDPrefix(r.Seat) + "/" + r.Turn + "/" + r.HeldCall, nil
		}
	case PurposeTest:
		if part(r.Person) && part(r.Nonce) {
			return "crewlet/test/" + r.Person + "/" + r.Nonce, nil
		}
	case PurposeInventory:
		if part(r.Nonce) && r.Seat == "" && r.Person == "" {
			return KeyIDPrefix(accessdomain.EngineHolder) + "/" + string(PurposeInventory) + "/" + r.Nonce, nil
		}
	}
	return "", fmt.Errorf("%w: a %s certificate names its holder in parts without slashes or spaces", ErrRequest, r.Purpose)
}

// principals is the request's principals, held to its purpose: read reaches
// every host it names at read, operate exactly one host at operate, a test one
// host at either level or as the inventory, the inventory one host as itself.
func (r Request) principals() ([]string, error) {
	for _, h := range r.Hosts {
		if !config.SSHHostID(h) {
			return nil, fmt.Errorf("%w: %q is not a host id", ErrRequest, h)
		}
	}
	switch {
	case len(r.Hosts) == 0:
		return nil, fmt.Errorf("%w: it names no host", ErrRequest)
	case r.Purpose == PurposeRead || r.Purpose == PurposeBoxRead:
		if r.Level != config.SSHLevelRead {
			return nil, fmt.Errorf("%w: a read certificate is at read", ErrRequest)
		}
	case r.Purpose == PurposeOperate:
		if r.Level != config.SSHLevelOperate || len(r.Hosts) != 1 {
			return nil, fmt.Errorf("%w: an operate certificate reaches exactly one host at operate", ErrRequest)
		}
	case r.Purpose == PurposeTest:
		if len(r.Hosts) != 1 || (r.Level != LevelInventory && !slices.Contains(config.SSHLevels(), r.Level)) {
			return nil, fmt.Errorf("%w: a test certificate reaches one host at one level", ErrRequest)
		}
	case r.Purpose == PurposeInventory:
		if r.Level != LevelInventory || len(r.Hosts) != 1 {
			return nil, fmt.Errorf("%w: an inventory certificate reaches exactly one host as the inventory", ErrRequest)
		}
	}
	if r.Level == LevelInventory && r.Purpose != PurposeInventory && r.Purpose != PurposeTest {
		return nil, fmt.Errorf("%w: only the engine's inventory holds the inventory principal", ErrRequest)
	}
	hosts := slices.Clone(r.Hosts)
	slices.Sort(hosts)
	hosts = slices.Compact(hosts)
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, Principal(h, r.Level))
	}
	return out, nil
}

// Sign signs a certificate for req with ca, over a key pair made for it alone.
func Sign(ca *setup.SSHCertificateAuthority, req Request) (Issued, error) {
	if ca == nil || !req.Purpose.Valid() || req.Now.IsZero() {
		return Issued{}, fmt.Errorf("%w: no authority, purpose or time", ErrRequest)
	}
	keyID, err := req.KeyID()
	if err != nil {
		return Issued{}, err
	}
	principals, err := req.principals()
	if err != nil {
		return Issued{}, err
	}
	// AN OPERATE PRINCIPAL ALWAYS CARRIES THE GATE: the operate user has no
	// server-side forced command, so a certificate reaching it without one
	// would be a shell, whatever its purpose.
	// AN INVENTORY PRINCIPAL ALWAYS CARRIES ONE OF THE INVENTORY'S COMMANDS:
	// the closed set is held here, so no caller can have a certificate
	// signed for anything else, and the server's gate runs what it names.
	options := map[string]string{}
	switch {
	case strings.ContainsAny(req.ForceCommand, "\r\n\x00"):
		return Issued{}, fmt.Errorf("%w: a force-command is one line", ErrRequest)
	case req.Level == LevelInventory && !slices.Contains(config.SSHInventoryCommands(), req.ForceCommand):
		return Issued{}, fmt.Errorf("%w: an inventory certificate binds one of the inventory's own commands",
			ErrRequest)
	case req.Level == LevelInventory:
		options["force-command"] = req.ForceCommand
	case req.Level == config.SSHLevelOperate && req.Purpose == PurposeTest && req.ForceCommand != GateVersionCommand:
		return Issued{}, fmt.Errorf("%w: a test certificate at operate binds only the gate's version", ErrRequest)
	case req.Level == config.SSHLevelOperate && req.Purpose != PurposeTest && !operateBinding(req.ForceCommand):
		return Issued{}, fmt.Errorf("%w: an operate certificate binds the approved command through the gate", ErrRequest)
	case req.Level != config.SSHLevelOperate && req.ForceCommand != "":
		return Issued{}, fmt.Errorf("%w: a read certificate binds no command; the server forces its own", ErrRequest)
	case req.ForceCommand != "":
		options["force-command"] = req.ForceCommand
	}
	if len(req.SourceAddresses) > 0 {
		sources, sourceErr := canonicalSources(req.SourceAddresses)
		if sourceErr != nil {
			return Issued{}, sourceErr
		}
		options["source-address"] = sources
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Issued{}, err
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		return Issued{}, err
	}
	serial, err := randomSerial()
	if err != nil {
		return Issued{}, err
	}
	after := req.Now.Add(-clockSkew).UTC().Truncate(time.Second)
	before := req.Now.Add(Validity(req.Purpose)).UTC().Truncate(time.Second)
	cert := &ssh.Certificate{
		Key: signer.PublicKey(), Serial: serial, CertType: ssh.UserCert, KeyId: keyID,
		ValidPrincipals: principals,
		ValidAfter:      uint64(after.Unix()), ValidBefore: uint64(before.Unix()),
		Permissions: ssh.Permissions{CriticalOptions: options, Extensions: map[string]string{}},
	}
	if err = cert.SignCert(rand.Reader, ca.Signer); err != nil {
		return Issued{}, err
	}
	certSigner, err := ssh.NewCertSigner(cert, signer)
	if err != nil {
		return Issued{}, err
	}
	holder := req.Seat
	switch req.Purpose {
	case PurposeTest:
		// A TEST IS A PERSON'S PRESS, recorded under the seat they hold.
		holder = req.Person
	case PurposeInventory:
		// THE INVENTORY IS THE ENGINE'S OWN, recorded under no seat.
		holder = accessdomain.EngineHolder
	}
	return Issued{Certificate: cert, Signer: certSigner, Record: CertificateRecord{
		Serial: serial, KeyID: keyID, Seat: holder, Turn: req.Turn, HeldCall: req.HeldCall,
		Purpose: req.Purpose, HostIDs: hostIDs(req.Hosts), Principals: principals,
		ValidAfter: after, ValidBefore: before,
		PublicKeySHA256: ssh.FingerprintSHA256(signer.PublicKey()), CASHA256: ca.Fingerprint(),
	}}, nil
}

// operateBinding reports whether force is the gate handed one non-empty
// command as standard base64.
func operateBinding(force string) bool {
	payload, ok := strings.CutPrefix(force, GatePath+" operate ")
	if !ok {
		return false
	}
	command, err := base64.StdEncoding.DecodeString(payload)
	return err == nil && len(command) > 0 && base64.StdEncoding.EncodeToString(command) == payload
}

// canonicalSources is the address ranges as sshd reads them: each parsed and
// written in its canonical form, so stray spaces never reach the option.
func canonicalSources(ranges []string) (string, error) {
	out := make([]string, 0, len(ranges))
	for _, r := range ranges {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(r))
		if err != nil || prefix.Masked() != prefix {
			return "", fmt.Errorf("%w: %q is not an address range", ErrRequest, r)
		}
		out = append(out, prefix.String())
	}
	return strings.Join(out, ","), nil
}

func hostIDs(hosts []string) []string {
	out := slices.Clone(hosts)
	slices.Sort(out)
	return slices.Compact(out)
}

// randomSerial is a random non-zero serial.
func randomSerial() (uint64, error) {
	var b [8]byte
	for {
		if _, err := rand.Read(b[:]); err != nil {
			return 0, err
		}
		if n := binary.BigEndian.Uint64(b[:]); n != 0 {
			return n, nil
		}
	}
}

// ParsePublicLine reads an authority's public line as a server's
// TrustedUserCAKeys holds it: one plain Ed25519 key, never a certificate.
func ParsePublicLine(line string) (ssh.PublicKey, error) {
	if strings.ContainsAny(line, "\r\n") {
		return nil, errors.New("ssh: an authority's public line is one line")
	}
	pk, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(line))
	switch {
	case err != nil:
		return nil, errors.New("ssh: an authority's public line does not parse")
	case len(options) > 0 || len(rest) > 0 || pk.Type() != ssh.KeyAlgoED25519:
		return nil, errors.New("ssh: an authority's public line is one plain Ed25519 key")
	}
	return pk, nil
}
