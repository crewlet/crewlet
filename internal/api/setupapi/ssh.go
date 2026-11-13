package setupapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/plainjson"
	"github.com/crewlet/crewlet/internal/setup"
	sshint "github.com/crewlet/crewlet/internal/ssh"
	"github.com/crewlet/crewlet/internal/words"
)

// The SSH integration's own routes: its servers. A server is part of the
// company document, written here through the same merge, validation and
// activation PATCH /config performs, by the person who pressed Save; the scan
// only reads what a server presents, for that person to compare.

// SSHTests is the fleet's record of each server's latest Test connection.
type SSHTests interface {
	ListSSHTests(ctx context.Context) ([]coord.SSHTest, error)
	PutSSHTest(ctx context.Context, s coord.SSHTest) (coord.SSHTest, error)
}

// SSHServers is the SSH integration's extension of its answer.
type SSHServers struct {
	// CAPublicKey is the line servers trust; NextCAPublicKey a staged
	// rotation's. Never a private key.
	CAPublicKey     string `json:"ca_public_key,omitempty"`
	NextCAPublicKey string `json:"next_ca_public_key,omitempty"`
	// RetiredCAPublicKeys are the lines a rotation retired, newest last:
	// each server drops them when its setup runs again.
	RetiredCAPublicKeys []string `json:"retired_ca_public_keys"`
	// SetupVersion is the server setup this engine renders.
	SetupVersion int `json:"setup_version"`
	// InventoryCommands is the closed set the engine itself runs on a server
	// that opts in to the atlas's inventory, as the server's gate takes them.
	InventoryCommands []SSHInventoryCommand `json:"inventory_commands"`
	// Hosts are the registered servers, in document order.
	Hosts []SSHServer `json:"hosts"`
}

// SSHInventoryCommand is one command of the inventory. Containers marks the
// one run only on a server whose read user may list containers.
type SSHInventoryCommand struct {
	Command    string `json:"command"`
	Containers bool   `json:"containers"`
}

// sshInventoryCommands is the inventory's closed set as the answer carries it.
func sshInventoryCommands() []SSHInventoryCommand {
	out := []SSHInventoryCommand{}
	for _, command := range config.SSHInventoryCommands() {
		out = append(out, SSHInventoryCommand{Command: command, Containers: command == config.SSHInventoryContainers})
	}
	return out
}

// SSHServer is one registered server, with its keys as fingerprints.
type SSHServer struct {
	ID           string              `json:"id"`
	Address      string              `json:"address"`
	Port         int                 `json:"port"`
	Fingerprints []string            `json:"fingerprints"`
	Users        config.SSHHostUsers `json:"users"`
	Description  string              `json:"description,omitempty"`
	Containers   bool                `json:"containers,omitempty"`
	// Inventory is whether the engine itself reads the server for the atlas.
	Inventory bool `json:"inventory,omitempty"`
	// GrantedTo is every agent naming it, on or off.
	GrantedTo []string `json:"granted_to"`
	// LastTest is its latest Test connection with the current authority as
	// the server is now, absent where none was pressed since it changed.
	LastTest *coord.SSHTest `json:"last_test,omitempty"`
}

// heldAuthority is the authority a reference names, parsed, or nil.
func (s *Service) heldAuthority(ref string) *setup.SSHCertificateAuthority {
	if strings.TrimSpace(ref) == "" {
		return nil
	}
	ca, err := setup.ParseSSHCertificateAuthority(setup.Deref(ref, s.resolve))
	if err != nil {
		return nil
	}
	return ca
}

// sshServers is the extension of the SSH answer for company.
func (s *Service) sshServers(ctx context.Context, company *config.Company) (*SSHServers, error) {
	block := company.Integrations.SSH
	if block == nil {
		return nil, nil
	}
	out := &SSHServers{SetupVersion: sshint.SetupVersion, Hosts: []SSHServer{},
		InventoryCommands:   sshInventoryCommands(),
		RetiredCAPublicKeys: s.retiredAuthorities()}
	if out.RetiredCAPublicKeys == nil {
		out.RetiredCAPublicKeys = []string{}
	}
	var current string
	if block.Provisioning != nil {
		if ca := s.heldAuthority(block.Provisioning.CertificateAuthority); ca != nil {
			out.CAPublicKey, current = ca.PublicLine(), ca.Fingerprint()
		}
		if ca := s.heldAuthority(block.Provisioning.NextCertificateAuthority); ca != nil {
			out.NextCAPublicKey = ca.PublicLine()
		}
	}
	tests, err := s.sshTests.ListSSHTests(ctx)
	if err != nil {
		return nil, err
	}
	for _, h := range block.Hosts {
		server := SSHServer{ID: h.ID, Address: h.Address, Port: h.PortOrDefault(), Users: h.Users,
			Description: h.Description, Containers: h.Containers, Inventory: h.Inventory, Fingerprints: []string{},
			GrantedTo: company.SSHHostGrantees(h.ID)}
		if server.GrantedTo == nil {
			server.GrantedTo = []string{}
		}
		for _, line := range h.HostKeys {
			if pk, err := config.ParseSSHHostKey(line); err == nil {
				server.Fingerprints = append(server.Fingerprints, ssh.FingerprintSHA256(pk))
			}
		}
		server.LastTest = latestTest(tests, h, current)
		out.Hosts = append(out.Hosts, server)
	}
	return out, nil
}

// sshConfigured answers the request itself where SSH is not connected and on.
func sshConfigured(w http.ResponseWriter, company *config.Company) bool {
	if b := company.Integrations.SSH; b == nil || !b.Enabled {
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeNotConnected, map[string]string{
			"detail": "SSH is not connected, or is switched off",
			"hint":   "connect it with POST /setup/integrations/ssh/inputs first",
		})
		return false
	}
	return true
}

// scanRequest is what the scan reads.
type scanRequest struct {
	Address string `json:"address"`
	Port    int    `json:"port"`
}

// sshScan serves POST /setup/integrations/ssh/hosts/scan: the host keys the
// address presents, read without signing in, for a person to compare with
// the server's console before pinning them.
func (s *Service) sshScan(w http.ResponseWriter, r *http.Request) {
	fc, ok := s.companyFor(w, r, "no company configuration is active")
	if !ok || !sshConfigured(w, fc.company) {
		return
	}
	var req scanRequest
	if !readSSHBody(w, r, &req) {
		return
	}
	req.Address = strings.TrimSpace(req.Address)
	if sentence := config.SSHAddress(req.Address); req.Address == "" || sentence != "" {
		if sentence == "" {
			sentence = "give the server's host name or IP address"
		}
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalid, map[string]string{
			"field": "address", "detail": sentence})
		return
	}
	keys, err := sshint.Scan(r.Context(), req.Address, req.Port)
	if err != nil {
		httpjson.FailWith(w, http.StatusBadGateway, httpjson.CodeServerUnreachable, map[string]string{
			"detail": "the server's host keys could not be read: " + err.Error(),
			"hint":   "check the address and port, and that the engine's address may reach port 22 there",
		})
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"host_keys": keys})
}

// hostRequest is a server's fields as the Add server dialog sends them.
type hostRequest struct {
	Address       string              `json:"address"`
	Port          int                 `json:"port"`
	HostKeys      []string            `json:"host_keys"`
	Users         config.SSHHostUsers `json:"users"`
	Description   string              `json:"description"`
	Containers    bool                `json:"containers"`
	Inventory     bool                `json:"inventory"`
	ReadAllowlist []string            `json:"read_allowlist"`
	IfMatch       string              `json:"if_match"`
}

// sshPutHost serves PUT /setup/integrations/ssh/hosts/{id}: the server added,
// or replaced whole, as a revision the caller authors.
func (s *Service) sshPutHost(w http.ResponseWriter, r *http.Request) {
	fc, ok := s.companyFor(w, r, "no company configuration is active")
	if !ok || !sshConfigured(w, fc.company) {
		return
	}
	id := r.PathValue("id")
	var req hostRequest
	if !readSSHBody(w, r, &req) {
		return
	}
	host := config.SSHHost{ID: id, Address: strings.TrimSpace(req.Address), Port: req.Port, HostKeys: req.HostKeys,
		Users: req.Users, Description: req.Description, Containers: req.Containers, Inventory: req.Inventory,
		ReadAllowlist: req.ReadAllowlist}
	hosts := slices.Clone(fc.company.Integrations.SSH.Hosts)
	verb := "add"
	if i := slices.IndexFunc(hosts, func(h config.SSHHost) bool { return h.ID == id }); i >= 0 {
		hosts[i], verb = host, "update"
	} else {
		hosts = append(hosts, host)
	}
	s.writeSSHHosts(w, r, hosts, verb+" SSH server "+id, req.IfMatch, fc.revision)
}

// inventoryRequest is one server's inventory switch.
type inventoryRequest struct {
	Inventory bool   `json:"inventory"`
	IfMatch   string `json:"if_match"`
}

// sshPutInventory serves PUT /setup/integrations/ssh/hosts/{id}/inventory:
// whether the engine itself reads the server for the atlas, as a revision the
// caller authors. It changes that one field, so a caller that holds none of
// the server's key lines can still switch it.
func (s *Service) sshPutInventory(w http.ResponseWriter, r *http.Request) {
	fc, ok := s.companyFor(w, r, "no company configuration is active")
	if !ok || !sshConfigured(w, fc.company) {
		return
	}
	id := r.PathValue("id")
	var req inventoryRequest
	if !readSSHBody(w, r, &req) {
		return
	}
	hosts := slices.Clone(fc.company.Integrations.SSH.Hosts)
	i := slices.IndexFunc(hosts, func(h config.SSHHost) bool { return h.ID == id })
	if i < 0 {
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNotFound, map[string]string{
			"detail": "no SSH server " + id + " is registered"})
		return
	}
	hosts[i].Inventory = req.Inventory
	verb := "stop"
	if req.Inventory {
		verb = "start"
	}
	s.writeSSHHosts(w, r, hosts, verb+" the atlas inventory of SSH server "+id, req.IfMatch, fc.revision)
}

// sshDeleteHost serves DELETE /setup/integrations/ssh/hosts/{id}, refused while
// an agent names the server.
func (s *Service) sshDeleteHost(w http.ResponseWriter, r *http.Request) {
	fc, ok := s.companyFor(w, r, "no company configuration is active")
	if !ok || !sshConfigured(w, fc.company) {
		return
	}
	id := r.PathValue("id")
	hosts := fc.company.Integrations.SSH.Hosts
	i := slices.IndexFunc(hosts, func(h config.SSHHost) bool { return h.ID == id })
	if i < 0 {
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNotFound, map[string]string{
			"detail": "no SSH server " + id + " is registered"})
		return
	}
	if seats := fc.company.SSHHostGrantees(id); len(seats) > 0 {
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeConflict, map[string]string{
			"detail": "host " + id + " is granted to " + words.And(seats) + "; remove it from those agents first"})
		return
	}
	s.writeSSHHosts(w, r, slices.Delete(slices.Clone(hosts), i, i+1), "remove SSH server "+id, "", fc.revision)
}

// writeSSHHosts writes the server list as one revision built on base, or on
// the revision the caller stated.
func (s *Service) writeSSHHosts(w http.ResponseWriter, r *http.Request, hosts []config.SSHHost,
	summary, stated, base string) {
	if hosts == nil {
		hosts = []config.SSHHost{}
	}
	patch, err := json.Marshal(map[string]any{"integrations": map[string]any{"ssh": map[string]any{"hosts": hosts}}})
	if err != nil {
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
		return
	}
	expect := base
	if stated != "" {
		expect = stated
	}
	applied, err := s.config.Apply(r.Context(), configapi.ApplyRequest{
		Patch: patch, Summary: summary, By: attributionOf(r), Expect: expect, Stated: stated != "",
	})
	if err != nil {
		s.refuse(w, r, err, setup.Result{})
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{
		"key": integration.KindSSH, "revision_id": applied.RevisionID, "epoch": applied.Epoch})
}

// sshSetup serves GET /setup/integrations/ssh/hosts/{id}/setup: the server
// setup rendered for the server, trusting the current authority and a staged
// one.
func (s *Service) sshSetup(w http.ResponseWriter, r *http.Request) {
	fc, ok := s.companyFor(w, r, "no company configuration is active")
	if !ok || !sshConfigured(w, fc.company) {
		return
	}
	block := fc.company.Integrations.SSH
	host, found := block.Host(r.PathValue("id"))
	if !found {
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNotFound, map[string]string{
			"detail": "no SSH server " + r.PathValue("id") + " is registered"})
		return
	}
	var lines []string
	if prov := block.Provisioning; prov != nil {
		for _, ref := range []string{prov.CertificateAuthority, prov.NextCertificateAuthority} {
			if ca := s.heldAuthority(ref); ca != nil {
				lines = append(lines, ca.PublicLine())
			}
		}
	}
	script, err := sshint.RenderSetup(host, lines)
	if err != nil {
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeNotConnected, map[string]string{
			"detail": strings.TrimPrefix(err.Error(), "ssh: "),
			"hint":   "save the SSH connection so its certificate authority is minted, and check the server's fields",
		})
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"script": script, "version": sshint.SetupVersion})
}

// maxSSHBody bounds a server's fields: eight key lines of a 16384-bit RSA key
// and a hundred allowlist rules fit in well under it.
const maxSSHBody = 256 << 10

// readSSHBody reads a strict JSON body into into, answering a refusal itself.
func readSSHBody(w http.ResponseWriter, r *http.Request, into any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSSHBody))
	if err == nil {
		err = decode(body, into)
	}
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		httpjson.FailWith(w, http.StatusRequestEntityTooLarge, httpjson.CodeInvalid, map[string]string{
			"detail": "the request is larger than a server's fields ever are"})
		return false
	case err != nil:
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalid, map[string]string{
			"detail": plainjson.Unreadable("the request", err)})
		return false
	}
	return true
}
