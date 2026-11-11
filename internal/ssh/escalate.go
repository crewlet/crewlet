package ssh

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/delegated"
)

// The always-escalate list (ADR-0125): what no delegate allows, matched by the
// engine on an operate command and its stdin. A call any class or added
// pattern matches is put to people whatever its delegate said.
//
// THE PLAIN FORMS ONLY, and said so: a shell can spell any of these another
// way, so the list also escalates the spellings that hide what runs (decoding
// text into a shell, eval, escapes, a glob in a system path), and the delegate
// is told to escalate whatever it cannot read. Case is ignored: a match too
// many escalates a call, which costs a person's minute, and one too few is
// the failure this list exists against.

// EscalateClass is one kind of call no delegate allows.
type EscalateClass struct {
	// Name is the class's closed word, as the audit records it.
	Name string
	// Category is the delegated-decisions category it is a case of
	// (internal/delegated), "" for a spelling that hides what runs.
	Category string
	// Says is what the class is, as the reviewer and a person read it.
	Says string
	// patterns match the class; except, where set, clears a match whose text
	// it matches (a .env file under a temporary directory).
	patterns []string
	except   string
}

// The classes' names, as the audit and the verdict record them.
const (
	ClassSSHD          = "sshd_config"
	ClassSudoers       = "sudoers"
	ClassAccounts      = "users_and_groups"
	ClassAuthorizedKey = "authorized_keys"
	ClassGate          = "crewlet_gate"
	ClassAuthority     = "certificate_authority"
	ClassFirewall      = "firewall"
	ClassSecrets       = "secret_files"
	ClassDownloadShell = "download_into_shell"
	ClassHidden        = "hidden_text"
	ClassAuditing      = "auditing_and_logging"
	ClassDeletion      = "data_deletion"
	ClassCompanyDoc    = "crewlet_company"
	// ClassCompany is a pattern the company added
	// (integrations.ssh.always_escalate).
	ClassCompany = "company_pattern"
)

// interpreters are the programs a download or decoded text is piped into.
const interpreters = `(?:sudo\s+(?:-\S+\s+)*)?(?:env\s+)?(?:/\S*/)?(?:ba|da|z|k|c|tc|fi|a)?sh\b|` +
	`(?:sudo\s+(?:-\S+\s+)*)?(?:/\S*/)?(?:python[0-9.]*|perl|ruby|node|php|lua)\b`

// AlwaysEscalateClasses is the engine's own list, in the order a reader is
// told it, covering every category internal/delegated escalates for a
// command. Extended only by adding: a company's patterns join it, and no
// setting removes a class.
var AlwaysEscalateClasses = sync.OnceValue(func() []EscalateClass {
	return []EscalateClass{
		{Name: ClassSSHD, Category: delegated.CategoryAccess, Says: "changes or reads the SSH server's configuration",
			patterns: []string{`/etc/ssh\b`, `sshd_config`, `\bsshd\b`}},
		{Name: ClassSudoers, Category: delegated.CategoryAccess, Says: "changes who may use sudo",
			patterns: []string{`sudoers`, `\bvisudo\b`, `/etc/sudo`}},
		{Name: ClassAccounts, Category: delegated.CategoryAccess, Says: "adds, changes or removes a user or a group, or a password",
			patterns: []string{
				`\b(?:useradd|usermod|userdel|adduser|deluser|groupadd|groupmod|groupdel|addgroup|delgroup|` +
					`gpasswd|passwd|chpasswd|chage|newusers|vipw|vigr|chsh|chfn|pwck|grpck)\b`,
				`/etc/(?:passwd|group|shadow|gshadow|login\.defs)\b`,
			}},
		{Name: ClassAuthorizedKey, Category: delegated.CategoryAccess, Says: "changes keys a user signs in with",
			patterns: []string{`authorized_keys`, `authorizedkeys`, `\.ssh/`}},
		{Name: ClassGate, Category: delegated.CategoryAccess, Says: "changes Crewlet's gate or read allowlist on the server",
			patterns: []string{`crewlet-gate`, `read-allowlist`, `/etc/ssh/crewlet`, `crewlet-ops`}},
		{Name: ClassAuthority, Category: delegated.CategoryAccess, Says: "changes which certificate authority the server trusts, or signs keys",
			patterns: []string{`trustedusercakeys`, `user_ca`, `\bssh-keygen\b`, `\bca\.pub\b`}},
		{Name: ClassFirewall, Category: delegated.CategoryAccess, Says: "changes firewall rules",
			patterns: []string{
				`\b(?:iptables|ip6tables|iptables-restore|ip6tables-restore|ebtables|arptables|nft|ufw|` +
					`firewall-cmd|firewalld)\b`,
				`/etc/(?:nftables|iptables|ufw)\b`,
			}},
		{Name: ClassSecrets, Category: delegated.CategorySecrets, Says: "reads or writes a secret: a shadow file, a private key or an .env file",
			patterns: []string{
				`/etc/g?shadow\b`,
				`\bid_(?:rsa|dsa|ecdsa|ed25519)(?:_sk)?\b(?:[^.]|$)`,
				`ssh_host_[a-z0-9]+_key\b(?:[^.]|$)`,
				`-----begin [a-z0-9 ]*private key-----`,
				`\.(?:pem|key|p12|pfx|jks|keystore)\b`,
				`(?:^|[\s"'=<>:;|&(])(?:\S*/)?\.env(?:\.[a-z0-9_-]+)?\b`,
			},
			except: `^(?:[\s"'=<>:;|&(])?(?:/tmp|/var/tmp)/\S*\.env(?:\.[a-z0-9_-]+)?$`},
		{Name: ClassDeletion, Category: delegated.CategoryDeletion, Says: "deletes data: files, disks, databases or containers' volumes",
			patterns: []string{
				`\brm\s+(?:-\S*\s+)*-\S*[rR]`, `\brm\s+(?:-\S+\s+)*--recursive\b`,
				`\bfind\b[^\n|;&]*\s-delete\b`, `\bshred\b`, `\bdd\b[^\n|;&]*\bof=/dev/`,
				`\bmkfs(?:\.[a-z0-9]+)?\b`, `\bwipefs\b`, `\b(?:drop|truncate)\s+(?:database|schema|table)\b`,
				`\btruncate\s+table\b`, `\bdelete\s+from\b`, `\bdocker\s+(?:volume\s+(?:rm|prune)|system\s+prune)\b`,
				`\bkubectl\s+delete\b`, `\bzfs\s+destroy\b`, `\blvremove\b`, `\bredis-cli\b[^\n]*\bflush(?:all|db)\b`,
			}},
		{Name: ClassCompanyDoc, Category: delegated.CategoryCompany, Says: "changes Crewlet's own company document, secrets, people or budgets",
			patterns: []string{`\bcrewlet\s+(?:config|secrets|iam|seats|budgets)\b`}},
		{Name: ClassDownloadShell, Says: "pipes a download or decoded text into a shell or an interpreter",
			patterns: []string{
				`\b(?:curl|wget|fetch|aria2c)\b[^\n]*\|\s*(?:` + interpreters + `)`,
				`\b(?:base64\s+(?:-d|--decode)|xxd\s+-r|openssl\s+(?:enc|base64)\s[^\n]*-d)[^\n]*\|\s*(?:` +
					interpreters + `)`,
				`[<$]\(\s*(?:curl|wget|fetch)\b`,
				"`\\s*(?:curl|wget|fetch)\\b",
				`(?:^|[\s;&|])(?:source|\.)\s+<\(`,
			}},
		{Name: ClassHidden, Says: "hides what it runs: eval, escapes, decoding or a glob in a system path",
			patterns: []string{`\beval\b`, `\\x[0-9a-f]{2}`, `\$'`, `\bbase64\s+(?:-d|--decode)\b`,
				`\bxxd\s+-r\b`, `/(?:etc|root|boot|usr|var/log)/\S*[*?\[]`}},
		{Name: ClassAuditing, Category: delegated.CategoryAccess, Says: "turns off, clears or edits auditing or logs",
			patterns: []string{
				`\b(?:auditctl|auditd|augenrules|rsyslogd?|syslog-ng|systemd-journald|journald)\b`,
				`/etc/audit\b`, `/etc/(?:rsyslog|syslog-ng)`,
				`journalctl\s[^\n]*--(?:vacuum|rotate|flush)`,
				`\b(?:rm|truncate|shred|unlink|mv|chattr|ln)\b[^\n|;&]*/var/log\b`,
				`>{1,2}\s*/var/log\b`, `\bhistory\s+-c\b`, `histfile`, `\bsetenforce\b`,
			}},
	}
})

// compiled is every class's patterns, compiled once.
var compiled = sync.OnceValue(func() map[string][]*regexp.Regexp {
	out := map[string][]*regexp.Regexp{}
	for _, c := range AlwaysEscalateClasses() {
		for _, p := range c.patterns {
			out[c.Name] = append(out[c.Name], regexp.MustCompile(`(?i)`+p))
		}
		if c.except != "" {
			out[c.Name+"/except"] = []*regexp.Regexp{regexp.MustCompile(`(?i)` + c.except)}
		}
	}
	return out
})

// EscalateMatch is one class a call matched, or a company pattern.
type EscalateMatch struct {
	// Class is the class's name, [ClassCompany] for a company pattern.
	Class string `json:"class"`
	// Says is the class's sentence, or the pattern that matched.
	Says string `json:"says"`
}

// AlwaysEscalate is every class, then every company pattern, that command or
// stdin matches; none for a call the reviewer may allow. A company pattern
// that does not compile matches everything: validation refuses one, so a
// pattern that cannot be read here is one this node is too old for.
func AlwaysEscalate(command string, stdin []byte, company []string) []EscalateMatch {
	texts := []string{command, string(stdin)}
	var out []EscalateMatch
	for _, c := range AlwaysEscalateClasses() {
		if matchesClass(c, texts) {
			out = append(out, EscalateMatch{Class: c.Name, Says: c.Says})
		}
	}
	for _, pattern := range company {
		re, err := regexp.Compile(pattern)
		if err != nil || slices.ContainsFunc(texts, re.MatchString) {
			out = append(out, EscalateMatch{Class: ClassCompany, Says: "matches the company's pattern " +
				fmt.Sprintf("%q", pattern)})
		}
	}
	return out
}

// matchesClass reports whether any text holds a match of c that its exception
// does not clear.
func matchesClass(c EscalateClass, texts []string) bool {
	res := compiled()
	except := res[c.Name+"/except"]
	for _, re := range res[c.Name] {
		for _, text := range texts {
			for _, m := range re.FindAllString(text, -1) {
				// NO WAY BACK OUT of the temporary directory: /tmp/../etc/.env
				// is not a file under it.
				if len(except) == 0 || strings.Contains(m, "/..") ||
					!except[0].MatchString(strings.TrimRight(m, " \t\n")) {
					return true
				}
			}
		}
	}
	return false
}

// EscalateClassNames is every class's name, in the list's order.
func EscalateClassNames() []string {
	out := make([]string, 0, len(AlwaysEscalateClasses())+1)
	for _, c := range AlwaysEscalateClasses() {
		out = append(out, c.Name)
	}
	return append(out, ClassCompany)
}
