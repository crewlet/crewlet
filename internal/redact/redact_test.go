package redact_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/redact"
)

// The values here are SHAPES, not credentials: each is a syntactically valid
// example of a vendor's format built from filler characters, which is what the
// denylist matches on.
func TestEveryKnownShapeIsReplaced(t *testing.T) {
	cases := map[string]string{
		"sk-proj-" + strings.Repeat("A", 32):                              "api-key",
		"sk-ant-api03-" + strings.Repeat("x", 40):                         "api-key",
		"sk-" + strings.Repeat("z", 40):                                   "api-key",
		"xoxb-" + strings.Repeat("1", 12) + "-" + strings.Repeat("2", 12): "slack-token",
		"xoxp-" + strings.Repeat("3", 24):                                 "slack-token",
		"AKIA" + strings.Repeat("Q", 16):                                  "aws-key",
		"ghp_" + strings.Repeat("b", 36):                                  "github-token",
		"gho_" + strings.Repeat("c", 36):                                  "github-token",
		"github_pat_" + strings.Repeat("d", 60):                           "github-token",
		"glpat-" + strings.Repeat("e", 20):                                "gitlab-token",
		"glrt-" + strings.Repeat("f", 20):                                 "gitlab-token",
		runToken("4bf92f3577b34da6a3ce929d0e0e4736"):                      "run-token",
		"password: hunter2":                                               "password",
		"PASSWORD=hunter2":                                                "password",
		"pwd = hunter2":                                                   "password",
	}
	for secret, want := range cases {
		text := "before " + secret + " after"
		got := redact.Secrets(text)
		if strings.Contains(got, secret) {
			t.Fatalf("%s survived redaction: %q", want, got)
		}
		if !strings.Contains(got, redact.Marker+want+"]") {
			t.Fatalf("redacting a %s gave %q, want a %s marker", want, got, want)
		}
		if !strings.HasPrefix(got, "before ") || !strings.HasSuffix(got, " after") {
			t.Fatalf("surrounding text was disturbed: %q", got)
		}
	}
}

// A project key must not be labelled a plain api-key: the bare sk- rule would
// swallow it, so order is load-bearing.
func TestTheMoreSpecificShapeWinsOverThePrefixThatContainsIt(t *testing.T) {
	got := redact.Secrets("sk-proj-" + strings.Repeat("A", 32))
	if got != redact.Marker+"api-key]" {
		t.Fatalf("got %q, want exactly one marker with nothing left over", got)
	}
}

// runToken is the shape of the engine's own per-run token (internal/runtoken)
// over subject, built from filler as every shape here is: the version, an
// eight-character tag, the subject, a Unix expiry and a 43-character
// signature, a dot between each.
func runToken(subject string) string {
	return "crewlet_rt1.Tq0d_8-x." + subject + ".1792927401." + strings.Repeat("S", 40) + "a_-"
}

// A RUN TOKEN IS TAKEN WHOLE AND ONCE: the engine's own per-run token is one
// marker however its subject reads, and redacting it again changes nothing.
//
// ITS TAG AND SIGNATURE ARE BASE64URL AND ITS SUBJECT ANYTHING BUT THE DOT
// THAT ENDS IT, so a vendor's prefix and body can be spelled inside one — an
// artifact view ticket's subject is base64url of its claims — and one can be
// written straight after a vendor's key. So it is taken FIRST: taken after the
// vendors' rules, a piece of its subject would be replaced and the rest of the
// token, its signature among it, left in the text; and a token run into the
// key before it would have its own `crewlet_rt1` read as the end of that key,
// and the rest left. Its subject may be EMPTY, since a runtoken signs and
// answers an empty one. And what it takes is a token and no more: no line
// break and no space, since a token rides a URL path, and no marker's
// bracket, so a value another rule redacted where a subject would stand is
// not read, on a second pass, as a subject.
//
// Mutation: take the run token after the vendors' rules, and the rows with a
// vendor's shape inside it or before it fail; admit a space or a line break
// in its subject, and the rows split across one fail; admit a bracket, and a
// second pass over the value redacted where a subject would stand changes it;
// require a subject, and the empty one is published.
func TestARunTokenIsTakenWholeAndOnce(t *testing.T) {
	t.Parallel()
	m := redact.Marker + "run-token]"
	sig := strings.Repeat("S", 40) + "a_-"
	for name, c := range map[string]struct{ text, want string }{
		"bare":                    {runToken("4bf92f3577b34da6a3ce929d0e0e4736"), m},
		"a vendor's key inside":   {runToken("eyJhcnRp" + "sk-" + strings.Repeat("Q", 24) + "In0"), m},
		"a GitHub token inside":   {runToken("ghp_" + strings.Repeat("b", 36)), m},
		"after a vendor's key":    {"sk-" + strings.Repeat("A", 24) + runToken("x"), redact.Marker + "api-key]" + m},
		"an empty subject":        {runToken(""), m},
		"in a URL, then a line":   {"url: https://engine.example.com/otlp/" + runToken("t") + "/v1/traces\nnext", "url: https://engine.example.com/otlp/" + m + "/v1/traces\nnext"},
		"a line break in it":      {"crewlet_rt1.Tq0d_8-x.sub\nject.1792927401." + sig, "crewlet_rt1.Tq0d_8-x.sub\nject.1792927401." + sig},
		"a space in it":           {"crewlet_rt1.Tq0d_8-x.sub ject.1792927401." + sig, "crewlet_rt1.Tq0d_8-x.sub ject.1792927401." + sig},
		"a signature too short":   {"crewlet_rt1.Tq0d_8-x.s.1792927401." + sig[1:], "crewlet_rt1.Tq0d_8-x.s.1792927401." + sig[1:]},
		"prose naming its prefix": {"every per-run token opens with crewlet_rt1.", "every per-run token opens with crewlet_rt1."},
		// No token — its subject would hold a dot, which no token's does —
		// and the password's value up to the key after it is redacted
		// where a subject would stand.
		"a value redacted where a subject would stand": {
			"crewlet_rt1.Tq0d_8-x.pwd=a.bsk-" + strings.Repeat("Q", 24) + ".1792927401." + sig,
			"crewlet_rt1.Tq0d_8-x.pwd=" + redact.Marker + "password]" + redact.Marker + "api-key].1792927401." + sig,
		},
	} {
		got := redact.Secrets(c.text)
		if got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, c.want)
		}
		if again := redact.Secrets(got); again != got {
			t.Errorf("%s: a second pass changed %q to %q", name, got, again)
		}
		for _, sizes := range [][]int{{1}, {5, 400}} {
			if shown, _ := settle(c.text, sizes); shown != c.want {
				t.Errorf("%s in pieces of %v: shown as it settled\n%q\nwant\n%q", name, sizes, shown, c.want)
			}
		}
	}
}

func TestAPrivateKeyBlockGoesWholeRatherThanLineByLine(t *testing.T) {
	block := "-----BEGIN OPENSSH PRIVATE KEY-----\n" +
		strings.Repeat(sshLine+"\n", 20) +
		"-----END OPENSSH PRIVATE KEY-----"
	got := redact.Secrets("log line\n" + block + "\ntrailing")
	if strings.Contains(got, "b3BlbnNzaC") {
		t.Fatalf("key material survived: %q", got)
	}
	if !strings.Contains(got, redact.Marker+"private-key]") {
		t.Fatalf("no private-key marker in %q", got)
	}
	if !strings.Contains(got, "log line") || !strings.Contains(got, "trailing") {
		t.Fatalf("the surrounding log was eaten: %q", got)
	}
}

// Two keys in one payload must not be merged into a single span by a greedy
// match that runs from the first BEGIN to the last END.
func TestTwoPrivateKeysDoNotSwallowWhatIsBetweenThem(t *testing.T) {
	key := "-----BEGIN RSA PRIVATE KEY-----\n" + pemBody(2) + "-----END RSA PRIVATE KEY-----"
	got := redact.Secrets(key + "\nIMPORTANT LOG\n" + key)
	if !strings.Contains(got, "IMPORTANT LOG") {
		t.Fatalf("a greedy match ate the text between two keys: %q", got)
	}
	if strings.Count(got, redact.Marker+"private-key]") != 2 {
		t.Fatalf("want two markers, got %q", got)
	}
}

// A PASSWORD'S VALUE IS ALL ITS RULE REPLACES: the key and the separator stay
// as they were, line breaks and all, real or escaped in a string, and the value
// ends where no password goes on — at an armour line's armour, a marker, and
// an escaped line break that what follows to the next space says is the
// string's: punctuation alone, the quote closing it. An escaped line break
// with a letter or a digit after it stays where it is, the value redacted on
// either side of it, so a password holding a backslash and an `n` or an `r`
// is not published past them, nor one ending in a backslash and punctuation.
// A token's match that runs into an armour stops at it. Each comes out as
// given, and redacting it again changes nothing.
//
// Mutation: replace the whole match, as the rule did, and the key goes with
// its value, its line break too; replace the value whole, and the escaped
// break in it goes with it; end it at every escaped break, and what follows
// the break is published; at none, and the closing quote is taken; read an
// escaped CRLF as its `\n` alone, and the `\r` goes into the marker; end it
// at a backslash with nothing after it but punctuation, as at a break, and
// `\!` is published; through an armour, and the END goes with it; through a
// marker, and a value already redacted is replaced again; let a token run into
// an armour, and the END is taken with it.
func TestAPasswordsValueIsAllItsRuleReplaces(t *testing.T) {
	m := redact.Marker + "password]"
	key := "-----BEGIN RSA PRIVATE KEY----- " + strings.Repeat("Ab1", 22) + " -----END RSA PRIVATE KEY-----"
	for name, c := range map[string]struct{ text, want string }{
		"on its line":             {"password: hunter2 rest", "password: " + m + " rest"},
		"on the next line":        {"login\npassword:\n  hunter2\nnext\n", "login\npassword:\n  " + m + "\nnext\n"},
		"after an escaped break":  {`{"k": "password:\n  hunter2 x"}`, `{"k": "password:\n  ` + m + ` x"}`},
		"after an escaped CRLF":   {`"pwd=\r\nhunter2 x"`, `"pwd=\r\n` + m + ` x"`},
		"across an escaped break": {`"pwd=hunter2\nnext" x`, `"pwd=` + m + `\n` + m + ` x`},
		"across an escaped CRLF":  {`pwd=Xk9\r\nQ2#vL8 x`, `pwd=` + m + `\r\n` + m + ` x`},
		"holding a backslash-n":   {`export DB_PASSWORD=Xk9\nQ2#vL8pZ`, `export DB_PASSWORD=` + m + `\n` + m},
		"holding a backslash-r":   {`mysql -u root --password=r00t\rT0ps3cret -e x`, `mysql -u root --password=` + m + ` -e x`},
		"in a string escaped twice, its break straight after the key": {
			`{"v": "pwd=\\nabc"}`, `{"v": "pwd=\\n` + m,
		},
		"up to an escaped break and the quote that closes the string": {`{"v": "pwd=x\n"}`, `{"v": "pwd=` + m + `\n"}`},
		"up to an escaped CRLF and the quote":                         {`{"v": "pwd=x\r\n"}`, `{"v": "pwd=` + m + `\r\n"}`},
		"ending in a backslash and punctuation":                       {`pwd=P4ss\!`, "pwd=" + m},
		"up to a marker":                                              {"pwd=x" + redact.Marker + "api-key] y", "pwd=" + m + redact.Marker + "api-key] y"},
		"already redacted":                                            {"password: " + m, "password: " + m},
		"a key redacted":                                              {"PASSWORD=" + key, "PASSWORD=" + redact.Marker + "private-key]"},
		"an armour line":                                              {"password:\n-----END RSA PRIVATE KEY-----\n", "password:\n-----END RSA PRIVATE KEY-----\n"},
		"up to an armour":                                             {"pwd=hunter2-----END RSA PRIVATE KEY-----\n", "pwd=" + m + "-----END RSA PRIVATE KEY-----\n"},
		"a token written up to an armour": {
			"sk-" + strings.Repeat("q", 21) + "-----END RSA PRIVATE KEY-----\n",
			redact.Marker + "api-key]-----END RSA PRIVATE KEY-----\n",
		},
		"a token too short without the armour": {
			"sk-" + strings.Repeat("q", 15) + "-----END RSA PRIVATE KEY-----\n",
			"sk-" + strings.Repeat("q", 15) + "-----END RSA PRIVATE KEY-----\n",
		},
	} {
		got := redact.Secrets(c.text)
		if got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, c.want)
		}
		if again := redact.Secrets(got); again != got {
			t.Errorf("%s: a second pass changed %q to %q", name, got, again)
		}
		if redact.Contains(got) {
			t.Errorf("%s: %q still holds something to replace", name, got)
		}
	}
}

// A PASSWORD'S KEY IS READ AS THE TEXT WRITES IT: closed by the quote or the
// markup it is written in, and separated from its value by the operator the
// language assigns or compares with. A JSON object's key is quoted, a
// Markdown label bold, Go declares with `:=` and PHP maps with `=>`, and read
// with a bare `:` or `=` straight after the key, the quoted key and the bold
// one were no key at all and their values published, and an operator's second
// character was taken for the value. The markup that closes what the key's
// line opened before its label — a bold label's `**`, a code span's backtick,
// a table cell's `|` — is the key's, and the value is read after it: on its
// line, or where nothing follows it, on a line below, as after a separator;
// read as the value, it was replaced and the value beside it or under it
// published. A quote is no such markup, since a logged string closes with
// one and what follows it is the next field. And a quote closes the key only
// where it closes the key's own string, a name ending in the key: a longer
// string ending in the key's word is a string, and what follows it no value.
// Each comes out as given, redacting it again changes nothing, and shown as
// it settles, a byte at a time or in pieces, it is the same.
//
// Mutation: read no quote or markup closing the key, and the JSON, PHP and
// bold-key cases publish their values; read the operators as their first
// character, or try the one character before the two, and `:=`, `==` and
// `=>` lose their second character to the marker; read no markup past the
// separator, and the bold label's and the table cell's values are published;
// take a quote for markup, and the logged prompt's next field is redacted;
// take any markup after the separator, whatever the key's line opened, and
// the word after a prompt's pipe is; read the opening only where it stands
// against the key, and the value of a label of two words is published, or
// read it past a word's own `_`, and an identifier's `_` after its
// separator is passed over, or past any `_`, and an underscored bold label's
// value is published, or over letters and spaces alone, and so is a label's
// with a parenthesis in it; read punctuation after the markup from the
// markup's own start — the value where no line repeats it, before another
// key on its line, or before one past an escaped break — and the markup is
// replaced and the punctuation kept; take any key on an echoed line for one
// that names its value, a string ending in the key's word too, and the echo's
// quote is taken for the value and the value published; read past markup that is a word's start only
// where the whole word is markup, and the value under a list's bold label is
// published; read the markup past the separator before the key's own
// close has taken its opening, and a bold value after a bold key keeps its
// opening `**`; read the opening as written rather than as the rule leaves
// it, and a second pass reads a table row's last pipe as a value; take the
// markup alone as no suffix, and a compact table row loses its closing pipe,
// or as the suffix every line must end in, and the value under a bold label
// is published, or the whole of it as the suffix rather than its outermost
// closing, and a table's blank row under a bold label in a cell is read as
// the value's; take any quote after the key for its own, and the string
// after a route's and a choice's is; hold the name before a quoted key to
// letters alone, and the `db_password` key's value is published; read a key
// at the end of a text whose separator is still to come as only a bare key,
// and shown in pieces, an arrow on the line below publishes its value.
func TestAPasswordsKeyIsReadAsTheTextWritesIt(t *testing.T) {
	t.Parallel()
	m := redact.Marker + "password]"
	for name, c := range map[string]struct{ text, want string }{
		"a JSON object's key":           {"{\"password\": \"hunter2x9\", \"user\": \"bob\"}\n", "{\"password\": " + m + " \"user\": \"bob\"}\n"},
		"a quoted key a name runs into": {"{\"db_password\": \"hunter2x9\"}\n", "{\"db_password\": " + m + "\n"},
		"a quoted key and PHP's arrow":  {"'password' => 'hunter2x9',\n", "'password' => " + m + "\n"},
		"Go's short declaration":        {"\tpassword := \"hunter2x9\"\n", "\tpassword := " + m + "\n"},
		"a make variable":               {"DB_PASSWORD := hunter2x9\n", "DB_PASSWORD := " + m + "\n"},
		"a comparison":                  {"if password == \"hunter2x9\":\n", "if password == " + m + "\n"},
		"a bold key":                    {"**Password**: hunter2x9\n", "**Password**: " + m + "\n"},
		"a bold label":                  {"**Password:** hunter2x9\n", "**Password:** " + m + "\n"},
		"a code span":                   {"`password:` hunter2x9\n", "`password:` " + m + "\n"},
		"a table's cell":                {"| Password: | hunter2x9 |\n", "| Password: | " + m + " |\n"},
		"a quoted key on the next line": {"{\n  \"password\":\n    \"hunter2x9\"\n}\n", "{\n  \"password\":\n    \"" + m + "\n}\n"},
		"a quoted key and its arrow on the next line": {
			"    'password'\n        => 'hunter2x9',\n", "    'password'\n        => " + m + "\n",
		},
		"a bold label of two words":      {"**Root password:** hunter2x9\n", "**Root password:** " + m + "\n"},
		"a bold label in a table's cell": {"| **Password:** | hunter2x9 |\n", "| **Password:** | " + m + " |\n"},
		"a bold label and its value on the next line": {
			"**Password:**\nhunter2x9\n", "**Password:**\n" + m + "\n",
		},
		"a list item's bold label and its value indented under it": {
			"- **Root password:**\n  hunter2x9\n", "- **Root password:**\n  " + m + "\n",
		},
		"a code span and its value on the next line": {"`password:`\n\nhunter2x9\n", "`password:`\n\n" + m + "\n"},
		"a bold label in a list of strings": {
			"\"**Password:**\",\n\"hunter2x9\",\n", "\"**Password:**\",\n\"" + m + "\",\n",
		},
		"a bold label echoed into a file": {
			"echo \"**Password:** \" >> notes.md\necho \"hunter2x9\" >> notes.md\n",
			"echo \"**Password:** \" >> notes.md\necho \"" + m + "\" >> notes.md\n",
		},
		"a compact table's rows": {"|Password:|\n|hunter2x9|\n", "|Password:|\n|" + m + "|\n"},
		"a bold label in a table's cell over a blank row": {
			"| **Password:** |\n|  |\n| hunter2x9 |\n", "| **Password:** |\n|  |\n| " + m + " |\n",
		},
		"a bold key and a bold value":                {"**Root password**: **hunter2x9**\n", "**Root password**: " + m + "\n"},
		"a key a name's underscore opens":            {"**db_password:** hunter2x9\n", "**db_password:** " + m + "\n"},
		"a bold label in underscores":                {"__Root password:__ hunter2x9\n", "__Root password:__ " + m + "\n"},
		"a label with a parenthesis":                 {"* **Root (prod) password:** hunter2x9\n", "* **Root (prod) password:** " + m + "\n"},
		"punctuation for a value after a bold label": {"**Password:** ****\n", "**Password:** " + m + "\n"},
		"punctuation for a value and another key on its line": {
			"**Password:** -- Retype password: x\n", "**Password:** " + m + " Retype password: " + m + "\n",
		},
		"a bold label and another key after an escaped break": {
			"\"**Password:**\\n-- password: hunter2x9\"", "\"**Password:**\\n-- password: " + m,
		},
		"an identifier's underscore and an underscore after it": {
			"DB_PASSWORD: _\nnext\n", "DB_PASSWORD: " + m + "\nnext\n",
		},
		"a table row under a key that took its first pipe": {
			"password:\n| Password: |\nnext\n", "password:\n" + m + " Password: " + m + "\nnext\n",
		},
		// The row below holds a key of its own, whose value may be the pipe
		// that said the row was the table's: the key above takes its own
		// cell's closing, as it takes punctuation.
		"a table of prompts, the second's key after punctuation": {
			"| Password: |\n| > Retype password: |\n", "| Password: " + m + "\n| > Retype password: " + m + "\n",
		},
		"a logged prompt and the field after it": {
			"msg=\"password:\" level=info\n", "msg=\"password:" + m + " level=info\n",
		},
		"a pipe after a prompt that opened nothing": {"echo Password: | tee -a log\n", "echo Password: " + m + " tee -a log\n"},
		"a string that ends in the key's word": {
			"\"POST /auth/password\": () => ok,\n", "\"POST /auth/password\": () => ok,\n",
		},
		"echoed beside a string that ends in the key's word": {
			"echo \"Password:\" # see \"the password\": below\necho \"hunter2x9\" # see \"the password\": below\n",
			"echo \"Password:\" # see \"the password\": below\necho \"" + m + "\" # see \"the password\": below\n",
		},
		"still published: a quoted key whose name has a space in it": {
			"{\"Admin Password\": \"hunter2x9\"}\n", "{\"Admin Password\": \"hunter2x9\"}\n",
		},
		"a choice between two strings": {
			"x ? \"Choose your password\" : \"Choose a new one\"\n", "x ? \"Choose your password\" : \"Choose a new one\"\n",
		},
	} {
		got := redact.Secrets(c.text)
		if got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, c.want)
		}
		if again := redact.Secrets(got); again != got {
			t.Errorf("%s: a second pass changed %q to %q", name, got, again)
		}
		for _, sizes := range [][]int{{1}, {5, 400}} {
			if shown, _ := settle(c.text, sizes); shown != c.want {
				t.Errorf("%s in pieces of %v: shown as it settled\n%q\nwant\n%q", name, sizes, shown, c.want)
			}
		}
	}
}

// This pass runs at more than one layer and a transcript can cross both.
func TestRedactionIsIdempotent(t *testing.T) {
	text := "token " + strings.Repeat("k", 40) + " and sk-" + strings.Repeat("q", 40)
	once := redact.Secrets(text)
	if twice := redact.Secrets(once); twice != once {
		t.Fatalf("a second pass changed the text:\n once: %q\ntwice: %q", once, twice)
	}
}

func TestOrdinaryTextIsUntouched(t *testing.T) {
	for _, text := range []string{
		"",
		"the build passed",
		"sk-short", // below the length floor: not a key shape
		"see docs/concepts/code-sandbox.md",
		"password rotation is documented in SECURITY.md",
	} {
		if got := redact.Secrets(text); got != text {
			t.Fatalf("Secrets(%q) = %q, want it unchanged", text, got)
		}
	}
}

func TestContainsAnswersWithoutRewriting(t *testing.T) {
	if redact.Contains("nothing here") {
		t.Fatal("Contains flagged clean text")
	}
	if !redact.Contains("glpat-" + strings.Repeat("e", 20)) {
		t.Fatal("Contains missed a known shape")
	}
}

// AND DIRTY TEXT IS STILL REDACTED WHOLE. The guard must not turn into a
// short-circuit that stops at the first rule that matched.
func TestEveryMatchingRuleStillFiresWhenSeveralDo(t *testing.T) {
	text := "glpat-" + strings.Repeat("e", 20) +
		" and sk-ant-" + strings.Repeat("a", 40) +
		" and password: hunter2"
	got := redact.Secrets(text)
	if redact.Contains(got) {
		t.Errorf("Secrets left something behind: %q", got)
	}
	for _, want := range []string{"gitlab-token", "api-key", "password"} {
		if !strings.Contains(got, want) {
			t.Errorf("Secrets(%q) = %q, missing the %s marker", text, got, want)
		}
	}
}

// A PASSWORD'S VALUE IS READ WHERE ITS WRAPPING PUTS IT. Its key and its value
// a line apart are two lines of whatever wraps every line of the text, and
// the value is on the key's line only when what follows the separator there
// holds one: the quote an `echo` closes, the `\n" +` of a string concatenated
// across lines, the next line's number, a blank numbered line and a diff's
// `+` are the wrapping's, and the value is on the line after them, behind the
// gutter every line repeats, moving its numbers on, and before the suffix it
// ends in — or before what a list's or a concatenation's last line closes
// with instead, or the `>>` that appends to the file the key's line opened
// with `>`. Read from the first word past the separator, as the rule read it,
// most of these published hunter2x9 with no key anywhere in the text, and
// others redacted it together with the line's number or mark.
// The value alone is replaced: the gutter, the suffix and the blank lines
// stay. Each comes out as given, redacting it again changes nothing, and shown
// as it settles, a byte at a time or in pieces, it is the same.
//
// Mutation: take the value from the first word past the separator, as the
// rule did, and the wrapped cases fail; read no line past one that opens with
// punctuation alone (no suffix) and the `echo`, the lists, the table, JSON
// lines and the double-quoted scalar fail; read past no rest of a line an
// escaped break ended the key's line on, and the cases written with `\n`
// after the key in a wrapping that closes the line fail; read no closing for
// a list's last line, and the list's last element is published; read no
// line appending to a file as the line that opened it, and the value under
// `> creds.txt` is published; read a timestamp's fraction as runs of its
// own, and the stamps with and without one redact the stamp; read no
// gutter, and the numbered, grep, diff, timestamp and nl cases redact the
// gutter; read the gutter off the key's piece of its string where the read
// crossed a real line break, and the concatenations whose key follows an
// earlier break redact the line's number, `+` or quote; read no quote after
// it, and the concatenation's first line fails; read any punctuation as that
// quote, and a list's `(9)` loses its `(`; take the gutter with its line's
// trailing spaces trimmed, and the nl and timestamp cases redact the blank
// line; pass no punctuation that closes a real line after an escaped break,
// and the concatenations fail; read the rest of the key's line as the suffix
// where another escaped break follows it on its real line, and the value
// under a quoted scalar whose key's line goes on to `--\n` is published; end
// a value at no escaped break with punctuation alone after it, and the quoted scalar, the JSON string and the
// string escaped twice replace the quote that closes them; end a value on a
// later line at its line's escaped break, as a value on the key's own line is
// not, and what follows the break is published; take an escaped break written
// at the end of a real line for the string's, and the diff's `+` on the line
// after is a suffix no line ends in, so the value is published; stop the
// gutter at every marker, and the values under a token on every line are
// published; match a marker by its shape, and a second pass cuts the value's
// marker where the token's began; match a password's marker in the gutter by
// the one a first pass wrote below it, and a second pass reads that line as a
// blank one of the wrapping and redacts the line after; take a word a key's
// word opens as a key and its separator there, and a file named
// passwords.txt makes the `echo`'s quote the value; read the key's line as
// written rather than as the rule leaves it, and a second pass replaces the
// quote the first kept under `new_password=`; end a gutter inside a rune, and
// the value's marker starts on a broken one; read a value of digits whole as the shape of the number a
// key's line opens with, and the numbered list's value is published; read the
// gutter from past a key's block on the key's line, as though what stands
// before it were the key's, and the timestamp under a key's END is the value.
func TestAPasswordsValueIsReadWhereItsWrappingPutsIt(t *testing.T) {
	t.Parallel()
	m := redact.Marker + "password]"
	tok, api := "sk-"+strings.Repeat("A", 24), redact.Marker+"api-key]"
	for name, c := range map[string]struct{ text, want string }{
		"a line echoed into a file at a time": {
			"echo \"Password:\" >> key.pem\necho \"hunter2x9\" >> key.pem\n",
			"echo \"Password:\" >> key.pem\necho \"" + m + "\" >> key.pem\n",
		},
		"echoed into a file the key's line opened, a blank line appended": {
			"echo \"Password:\" > creds.txt\necho \"\" >> creds.txt\necho \"hunter2x9\" >> creds.txt\n",
			"echo \"Password:\" > creds.txt\necho \"\" >> creds.txt\necho \"" + m + "\" >> creds.txt\n",
		},
		"echoed into a file whose name opens with a key's word": {
			"echo \"Password:\" >> passwords.txt\necho \"hunter2x9\" >> passwords.txt\n",
			"echo \"Password:\" >> passwords.txt\necho \"" + m + "\" >> passwords.txt\n",
		},
		"echoed, words before the key and a blank line echoed between": {
			"echo \"Enter the password:\" >> key.pem\necho \"\" >> key.pem\necho \"hunter2x9\" >> key.pem\n",
			"echo \"Enter the password:\" >> key.pem\necho \"\" >> key.pem\necho \"" + m + "\" >> key.pem\n",
		},
		"echoed, an escaped break after the key": {
			"echo \"password:\\n\" >> key.pem\necho \"hunter2x9\" >> key.pem\n",
			"echo \"password:\\n\" >> key.pem\necho \"" + m + "\" >> key.pem\n",
		},
		"a string concatenated across lines": {
			"const s = \"login\\n\" +\n\t\"Password:\\n\" +\n\t\"hunter2x9\\n\" +\n\t\"done\\n\"\n",
			"const s = \"login\\n\" +\n\t\"Password:\\n\" +\n\t\"" + m + "\\n\" +\n\t\"done\\n\"\n",
		},
		"on a concatenation's first line, a blank string between": {
			"const s = \"Password:\\n\" +\n\t\"\\n\" +\n\t\"hunter2x9\\n\"\n",
			"const s = \"Password:\\n\" +\n\t\"\\n\" +\n\t\"" + m + "\\n\"\n",
		},
		"concatenated, the key's string ending in a blank line of its own": {
			"const s = \"password:\\n\\n\" +\n\t\"hunter2x9\\n\"\n",
			"const s = \"password:\\n\\n\" +\n\t\"" + m + "\\n\"\n",
		},
		"concatenated under a reader's line numbers": {
			"    12\u2192const s = \"pwd:\\n\" +\n    13\u2192\t\"hunter2x9\\n\"\n",
			"    12\u2192const s = \"pwd:\\n\" +\n    13\u2192\t\"" + m + "\\n\"\n",
		},
		"concatenated, the key after an earlier break of its string, under a diff's marks": {
			"+\tconst creds = \"login: admin\\nPassword:\\n\" +\n+\t\t\"hunter2x9\\n\" +\n",
			"+\tconst creds = \"login: admin\\nPassword:\\n\" +\n+\t\t\"" + m + "\\n\" +\n",
		},
		"the same, under a reader's line numbers": {
			"    12\u2192const s = \"login\\npwd:\\n\" +\n    13\u2192\t\"hunter2x9\\n\"\n",
			"    12\u2192const s = \"login\\npwd:\\n\" +\n    13\u2192\t\"" + m + "\\n\"\n",
		},
		"a reader's line numbers": {
			"     9\u2192Password:\n    10\u2192hunter2x9\n",
			"     9\u2192Password:\n    10\u2192" + m + "\n",
		},
		"a reader's line numbers, a blank line between": {
			"     9\u2192Password:\n    10\u2192\n    11\u2192hunter2x9\n",
			"     9\u2192Password:\n    10\u2192\n    11\u2192" + m + "\n",
		},
		"a numbered list item under a reader's line numbers, a value of digits": {
			"     1\u21921. Password:\n     2\u2192839201\n", "     1\u21921. Password:\n     2\u2192" + m + "\n",
		},
		"the same under a diff's marks": {"+1. Password:\n+839201\n", "+1. Password:\n+" + m + "\n"},
		"the same under a grep's marks": {"f.go-1-1. Password:\nf.go-2-839201\n", "f.go-1-1. Password:\nf.go-2-" + m + "\n"},
		"a grep's match and context lines": {
			"a.pem:9:password:\na.pem-10-\na.pem-11-hunter2x9\n",
			"a.pem:9:password:\na.pem-10-\na.pem-11-" + m + "\n",
		},
		"a diff, an escaped break written at the end of the key's line": {
			"+password:\\n\n+\n+hunter2x9\n", "+password:\\n\n+\n+" + m + "\n",
		},
		"a token on every line, alone on the blank one": {
			tok + " password:\n" + tok + "\n" + tok + " hunter2x9\n",
			api + " password:\n" + api + "\n" + api + " " + m + "\n",
		},
		"a token before the key, the value alone on its line": {
			tok + " password:\n" + tok + "\nhunter2x9\n", api + " password:\n" + api + "\n" + m + "\n",
		},
		"a diff, words before the key": {
			"+Enter the passwd=\n+\n+hunter2x9\n",
			"+Enter the passwd=\n+\n+" + m + "\n",
		},
		"a log's timestamps, the key's line with its message": {
			"2026-10-06T09:00:00.120Z loaded key: password:\n2026-10-06T09:00:00.121Z \n2026-10-06T09:00:00.122Z hunter2x9\n",
			"2026-10-06T09:00:00.120Z loaded key: password:\n2026-10-06T09:00:00.121Z \n2026-10-06T09:00:00.122Z " + m + "\n",
		},
		"a log's timestamps, their fraction written where it is not zero": {
			"2026-10-06T09:00:00Z Password:\n2026-10-06T09:00:00.5Z hunter2x9\n",
			"2026-10-06T09:00:00Z Password:\n2026-10-06T09:00:00.5Z " + m + "\n",
		},
		"and the other way round, a blank line between": {
			"2026-10-06T09:00:00.25Z password:\n2026-10-06T09:00:01Z \n2026-10-06T09:00:01.125Z hunter2x9\n",
			"2026-10-06T09:00:00.25Z password:\n2026-10-06T09:00:01Z \n2026-10-06T09:00:01.125Z " + m + "\n",
		},
		"nl's numbers and a tab": {
			"    31\tEnter the password:\n    32\t\n    33\thunter2x9\n",
			"    31\tEnter the password:\n    32\t\n    33\t" + m + "\n",
		},
		"a gutter of a rune that shares its first byte with the value's": {
			"\u2192password:\n\u2014hunter2x9\n", "\u2192password:\n" + m + "\n",
		},
		"a YAML list of strings": {
			"  - \"PWD:\"\n  - \"\"\n  - \"hunter2x9\"\n",
			"  - \"PWD:\"\n  - \"\"\n  - \"" + m + "\"\n",
		},
		"a list across lines, an escaped break after the key": {
			"lines = [\n    \"password:\\n\",\n    \"\",\n    \"hunter2x9\",\n]\n",
			"lines = [\n    \"password:\\n\",\n    \"\",\n    \"" + m + "\",\n]\n",
		},
		"the last element of a list": {
			"[\n  \"Password:\",\n  \"hunter2x9\"\n]\n", "[\n  \"Password:\",\n  \"" + m + "\"\n]\n",
		},
		"a table's rows": {
			"| Password: |\n|  |\n| hunter2x9 |\n",
			"| Password: |\n|  |\n| " + m + " |\n",
		},
		"JSON lines": {
			"{\"log\": \"password:\\n\"}\n{\"log\": \"hunter2x9\"}\n",
			"{\"log\": \"password:\\n\"}\n{\"log\": \"" + m + "\"}\n",
		},
		"a quoted scalar broken across lines, its breaks kept": {
			"key: \"password:\\n\n  \\n\n  hunter2x9\\n\"\n",
			"key: \"password:\\n\n  \\n\n  " + m + "\\n\"\n",
		},
		// The rest of the key's line is a line of its string where another
		// escaped break follows it there: the punctuation closing that real
		// line is passed, and the value read below it.
		"a quoted scalar broken across lines, a line of punctuation after the key's": {
			"key: \"Password:\\n--\\n\n  hunter2x9\\n\"\n",
			"key: \"Password:\\n--\\n\n  " + m + "\\n\"\n",
		},
		"a YAML double-quoted scalar": {
			"password: \"\n  hunter2x9\"\n",
			"password: \"\n  " + m + "\"\n",
		},
		"a JSON string, a blank line in it": {
			"{\"k\": \"login\\npassword:\\n\\nhunter2x9\\n\"}\n",
			"{\"k\": \"login\\npassword:\\n\\n" + m + "\\n\"}\n",
		},
		"a JSON string escaped twice": {
			"\"{\\\"k\\\": \\\"password:\\\\nhunter2x9\\\\n\\\"}\"\n",
			"\"{\\\"k\\\": \\\"password:\\\\n" + m + "\\\\n\\\"}\"\n",
		},
		"a value on a later line with an escaped break in it, run on across it as on the key's line": {
			"Password:\nXk9\\nQ2#vL8\n", "Password:\n" + m + "\\n" + m + "\n",
		},
		"under a key beside another's value, its quote the value's": {
			"    change_password(old_password=\"hunter2\", new_password=\n                    \"hunter3x9\")\n",
			"    change_password(old_password=" + m + " new_password=\n                    " + m + "\n",
		},
		"the same in single quotes": {
			"    client.login(password='hunter2', confirm_password=\n                 'hunter3x9')\n",
			"    client.login(password=" + m + " confirm_password=\n                 " + m + "\n",
		},
		"under a key beside a masked value": {
			"Password: *** Retype password:\n***\n", "Password: " + m + " Retype password:\n" + m + "\n",
		},
		"under a key after an escaped break beside punctuation": {
			"prompt: Password:\\n- Confirm password:\nhunter2 ok\n", "prompt: Password:\\n" + m + " Confirm password:\n" + m + " ok\n",
		},
		"under a key beside a quote that is its own value": {"Pwd:\" pwd=\n\"\n", "Pwd:" + m + " pwd=\n" + m + "\n"},
		"a key whose line opens with the value a key above it named": {
			"Password:\n{\"log\": \"password:\\n\"}\nnext\n", "Password:\n" + m + " \"password:\\n" + m + "\nnext\n",
		},
		"under timestamps, after a key's END on its line": {
			"2026-10-06T09:00:00.120Z -----BEGIN RSA PRIVATE KEY-----\n2026-10-06T09:00:00.121Z " + keyLine +
				"\n2026-10-06T09:00:00.122Z -----END RSA PRIVATE KEY----- Password:\n2026-10-06T09:00:00.123Z hunter2x9\n",
			"2026-10-06T09:00:00.120Z " + redact.Marker + "private-key] Password:\n2026-10-06T09:00:00.123Z " + m + "\n",
		},
	} {
		got := redact.Secrets(c.text)
		if got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, c.want)
		}
		if again := redact.Secrets(got); again != got {
			t.Errorf("%s: a second pass changed %q to %q", name, got, again)
		}
		if redact.Contains(got) {
			t.Errorf("%s: %q still holds something to replace", name, got)
		}
		for _, sizes := range [][]int{{1}, {5, 400}} {
			if shown, _ := settle(c.text, sizes); shown != c.want {
				t.Errorf("%s in pieces of %v: shown as it settled\n%q\nwant\n%q", name, sizes, shown, c.want)
			}
		}
	}
}

// A PASSWORD'S VALUE IS READ NO FURTHER THAN A VALUE IS. The first line after
// the key that is no blank line of the wrapping ends the read: its first word
// past the gutter is the value, unless the line holds a password's key where
// that value would be (`password:` over `  password: hunter2x9`, which
// published hunter2x9 when the first key took the second for its value), or a
// value already redacted or an armour stands there. A key further along the
// line is that line's own: the value before it is the first key's, and the
// key reads its own beside it, as the rule leaves it. Punctuation that no line
// after it ends in is a value on the key's line (`***`), an empty quoted
// string is one, in either quote, a word in any script is one, and so is the
// punctuation after an escaped break that no line after it closes with. A
// gutter ends where a word of both lines does: read by shape alone, `4567`
// under a key whose line opens with a number was that number's shape and
// published, `4567x9` lost its digits, and `2024.Summer!` under `1. Password:`
// its `2024.`. And a line ends the way the key's did only by the same suffix,
// or by what a list's last line closes with instead: a call closing a string
// after an escaped break, under a key whose string closed there, is prose.
//
// What is still published: a wrapping whose gutter holds an escaped break of
// its own (each line `printf`ed with `'%s\n'`), which every line is broken
// at; a blank line of a wrapping that lost the space or the tab after its
// gutter — nl's number, a log's timestamp — read as the value; a value shaped
// like the word the key's line opens with, a space after it on both
// (`pass2 next` under `pass1 Retype password:`), read as a gutter, as one
// timestamp is under another; the first word of a YAML block scalar, after
// the `|` its key's line ends in; the value under a blank line of the
// wrapping whose gutter holds a password's key, which ends the read as any
// line holding one does; under a wrapping's suffix, a value beside a key of
// its own, whose line the key's value would no longer end as the wrapping's
// once redacted, so the key's own punctuation is its value; and punctuation
// alone after an escaped break inside a value (`pwd=abc\n!!`), which is the
// string's.
//
// Mutation: read past a line holding a key of its own, and the nested key's
// value is the first key's, published; end the read at every line holding a
// key, and the value before that key is published; read past a key of its own
// on a line under a suffix, and a second pass takes the `echo`'s quote the
// first left; read a value of digits whole as the gutter's shape,
// and the numbered cases publish; end a gutter inside a word of the key's
// line the value's line runs on past, and the list's and the timestamp's
// values lose their start; take punctuation alone after the separator as a
// suffix whatever the line after it, and `***` is published, and so is
// `!@#$%` after an escaped break; read only ASCII letters and digits as a
// value's, and `Пароль` is taken for punctuation and the value below
// published; take any line closing a string after an escaped break as the key's wrapping, and
// the call on the line after loses its first word; pass over punctuation
// alone wherever it stands, or after an escaped break whether or not the real
// line ends there, not only where it closes a real line after an escaped
// break, and `!@#$%^` on the line after the key is published; read an empty
// quoted string, in either quote, as punctuation, and the value is taken off
// the next line that ends in one; take punctuation as a suffix with another
// key after it on its line, and the value is taken off the line after; read a
// gutter into an armour, and `-----END` loses a dash to the value's marker; end
// a value at every escaped break before a line holding a key, and the half of
// `Xk9\nQ2#vL8` after the break is published, and `cd` before `pwd="`; run it
// into a key in the word after the break, and the key goes into the value's
// marker; read a line's ending off punctuation after an escaped break that a
// further escaped break follows, or read punctuation closing a real line
// after an escaped break as a line with a value in it where an escaped break
// ends that line, and the punctuation there is taken for the value, so a
// second pass, the line before the break no longer ending as the key's,
// takes the key's own.
func TestAPasswordsValueIsReadNoFurtherThanAValueIs(t *testing.T) {
	t.Parallel()
	m := redact.Marker + "password]"
	for name, c := range map[string]struct{ text, want string }{
		"a key of its own on the next line": {
			"password:\n  password: hunter2x9\n", "password:\n  password: " + m + "\n",
		},
		"a value with another key after it on its line": {
			"Password:\nhunter2x9 (the old password: hunter1)\n", "Password:\n" + m + " (the old password: " + m + "\n",
		},
		"a value beside a key naming one further down": {
			"Password:\nhunter2x9 Retype password:\nhunter2x9\n", "Password:\n" + m + " Retype password:\n" + m + "\n",
		},
		"under a diff's marks, the value under that key too": {
			"+Password:\n+hunter2x9 Retype password:\n+hunter2x9\n", "+Password:\n+" + m + " Retype password:\n+" + m + "\n",
		},
		"a value whose line ends in a key with nothing after it": {
			"Password:\nhunter2x9  # rotate the password:\n", "Password:\n" + m + "  # rotate the password:\n",
		},
		"a value whose line ends in a key with punctuation after it": {
			"+Password:\n+hunter2x9 (the admin password:)\n", "+Password:\n+" + m + " (the admin password:" + m + "\n",
		},
		"a key on the next line naming its value on a later one": {
			"     9\u2192password:\n    10\u2192PASSWORD:\n    11\u2192hunter2x9\n",
			"     9\u2192password:\n    10\u2192PASSWORD:\n    11\u2192" + m + "\n",
		},
		"an empty quoted value": {"password=\"\"\ngo build ./...\n", "password=" + m + "\ngo build ./...\n"},
		"an empty quoted value over a line ending as it does": {
			"password=\"\"\nuser=\"\"\n", "password=" + m + "\nuser=\"\"\n",
		},
		"an empty single-quoted value over a line ending as it does": {
			"password: ''\nusername: ''\n", "password: " + m + "\nusername: ''\n",
		},
		"a value in another script over a line ending as it does": {
			"password: \u041f\u0430\u0440\u043e\u043b\u044c\n\u0434\u0440\u0443\u0433\u043e\u0439 \u041f\u0430\u0440\u043e\u043b\u044c\n",
			"password: " + m + "\n\u0434\u0440\u0443\u0433\u043e\u0439 \u041f\u0430\u0440\u043e\u043b\u044c\n",
		},
		"punctuation alone on the line after the key": {"password:\n!@#$%^\nnext\n", "password:\n" + m + "\nnext\n"},
		"punctuation alone on the next line of a string": {
			"{\"k\": \"password:\\n!@#$%^\\nnext\\n\"}\n", "{\"k\": \"password:\\n" + m + "\\n" + m + "\\n\"}\n",
		},
		"punctuation before another key on its line": {"password: \" pwd: x\n\"y\"\n", "password: " + m + " pwd: " + m + "\n\"y\"\n"},
		"an escaped break in a value, a key with its own value after it": {
			"export PASSWORD=Xk9\\nQ2#vL8 pwd=abc\n", "export PASSWORD=" + m + "\\n" + m + " pwd=" + m + "\n",
		},
		"an escaped break in a value, a key with punctuation after it": {
			"pwd=ab\\ncd pwd=\" next\n", "pwd=" + m + "\\n" + m + " pwd=" + m + " next\n",
		},
		"an escaped break in a value, a key straight after it": {
			"\"pwd=hunter2\\npassword: x\"\n", "\"pwd=" + m + "\\npassword: " + m + "\n",
		},
		"punctuation after an escaped break before another key": {
			"\"password:\\n\" + pwd: x\n", "\"password:\\n" + m + " + pwd: " + m + "\n",
		},
		"an armour under a gutter a dash ends":         {"- password:\n-----END CERTIFICATE-----\n", "- password:\n-----END CERTIFICATE-----\n"},
		"an armour under a gutter it would end inside": {"-password:\n-----END CERTIFICATE-----\n", "-password:\n-----END CERTIFICATE-----\n"},
		"punctuation no line after it ends in":         {"password: ***\nok\n", "password: " + m + "\nok\n"},
		"a value already redacted": {
			"echo \"Password:\" >> key.pem\necho \"" + m + "\" >> key.pem\n",
			"echo \"Password:\" >> key.pem\necho \"" + m + "\" >> key.pem\n",
		},
		"an armour where the value would begin": {
			"     9\u2192password:\n    10\u2192-----END CERTIFICATE-----\n",
			"     9\u2192password:\n    10\u2192-----END CERTIFICATE-----\n",
		},
		"punctuation after an escaped break that no line closes with": {
			"{\"msg\": \"password:\\n!@#$%\"}\nnext\n", "{\"msg\": \"password:\\n" + m + "\nnext\n",
		},
		"a call closing a string after an escaped break, under a key whose string closed": {
			"\ttext := \"Enter password:\\n\"\n\tfmt.Fprintf(w, \"done\\n\")\n",
			"\ttext := \"Enter password:\\n" + m + "\n\tfmt.Fprintf(w, \"done\\n\")\n",
		},
		"the same under a reader's line numbers, the key after an earlier break": {
			"     1\u2192\ttext := \"user: bob\\npassword:\\n\"\n     2\u2192\tfmt.Println(\"x\\n\")\n",
			"     1\u2192\ttext := \"user: bob\\npassword:\\n" + m + "\n     2\u2192\tfmt.Println(\"x\\n\")\n",
		},
		"a value of digits under a key whose line opens with a number": {"3 password:\n4567\n", "3 password:\n" + m + "\n"},
		"and one opening with digits":                                  {"3 password:\n4567x9\n", "3 password:\n" + m + "\n"},
		"a value opening as the key's line does":                       {"Enter the password:\nEnter2x9\n", "Enter the password:\n" + m + "\n"},
		"a value opening with the shape of a numbered list's item":     {"1. Password:\n   2024.Summer!\n", "1. Password:\n   " + m + "\n"},
		"and with a list item's parentheses":                           {"(1) password:\n(9)Xk9Q2vL8\n", "(1) password:\n" + m + "\n"},
		"and with a time's":                                            {"10:00:01 password:\n10:00:02Xk9Q2vL8\n", "10:00:01 password:\n" + m + "\n"},
		"still published: each line printf'd with its own break": {
			"printf '%s\\n' 'password:'\nprintf '%s\\n' 'hunter2x9'\n",
			"printf '%s\\n' 'password:" + m + "\nprintf '%s\\n' 'hunter2x9'\n",
		},
		"still published: a blank numbered line that lost its tab": {
			"     3\tpassword:\n     4\n     5\thunter2x9\n", "     3\tpassword:\n     " + m + "\n     5\thunter2x9\n",
		},
		"still published: a log's blank line that lost its space": {
			"2026-10-06T09:00:00.120Z loaded key: password:\n2026-10-06T09:00:00.121Z\n2026-10-06T09:00:00.122Z hunter2x9\n",
			"2026-10-06T09:00:00.120Z loaded key: password:\n2026-10-06T09:00:" + m + "\n2026-10-06T09:00:00.122Z hunter2x9\n",
		},
		"still published: a value shaped like the word the key's line opens with": {
			"pass1 Retype password:\npass2 next\n", "pass1 Retype password:\npass2 " + m + "\n",
		},
		"still published: the wrapping's blank line holding a key in its gutter": {
			"pwd=x password:\npwd=x\nhunter2x9\n", "pwd=" + m + " password:\npwd=" + m + "\nhunter2x9\n",
		},
		"still published: a YAML block scalar": {"password: |\n  hunter2x9\n", "password: " + m + "\n  hunter2x9\n"},
		"still published: under an echo's suffix, a value beside a key of its own": {
			"echo \"Password:\" >> f\necho \"hunter2 password: x\" >> f\n",
			"echo \"Password:" + m + " >> f\necho \"hunter2 password: " + m + " >> f\n",
		},
		"still published: punctuation after an escaped break in a value": {"pwd=abc\\n!!\n", "pwd=" + m + "\\n!!\n"},
		// Punctuation after an escaped break ends a line only at the real
		// line's end, an escaped break written there included, and there
		// it closes the line rather than holding its value.
		"punctuation after an escaped break that an escaped break ends": {
			"a.pem-0-Passwd:+\na.pem-1-\\n\"+\\n\n", "a.pem-0-Passwd:" + m + "\na.pem-1-\\n\"+\\n\n",
		},
		"punctuation after an escaped break that another one follows": {"#passwd:+\n#\\n+\\n+\n", "#passwd:" + m + "\n#\\n+\\n+\n"},
	} {
		got := redact.Secrets(c.text)
		if got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, c.want)
		}
		if again := redact.Secrets(got); again != got {
			t.Errorf("%s: a second pass changed %q to %q", name, got, again)
		}
		for _, sizes := range [][]int{{1}, {5, 400}} {
			if shown, _ := settle(c.text, sizes); shown != c.want {
				t.Errorf("%s in pieces of %v: shown as it settled\n%q\nwant\n%q", name, sizes, shown, c.want)
			}
		}
	}
}
