package redact_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/redact"
)

// The tests here hold each read [redact.MaxReach] bounds to its bound, at the
// bound and one byte past it, on the side the bound decides: what is read
// within it decides as it did, and what lies past it as though the text ended
// there, or began there for a read back, save where the bound's doc says
// otherwise. Each row is redacted again unchanged, and shown as it settles,
// in pieces that cut its lines anywhere, the same as redacted whole.

// reach is [redact.MaxReach], short for the rows below.
const reach = redact.MaxReach

// atTheBound holds one row: text redacts to want, redacting that again
// changes nothing, and shown as it settles it is the same.
func atTheBound(t *testing.T, name, text, want string) {
	t.Helper()
	got := redact.Secrets(text)
	if got != want {
		t.Errorf("%s: %s", name, parted(got, want))
	}
	if again := redact.Secrets(got); again != got {
		t.Errorf("%s: a second pass changed it: %s", name, parted(again, got))
	}
	// Pieces of a prime size cut the text's lines everywhere but at their
	// breaks, so every line break is settled at, a line half written after
	// it; small ones would cost a 64 KiB text its length squared to put
	// back together.
	for _, sizes := range [][]int{{4093}, {len(text)}} {
		if shown, _ := settle(text, sizes); shown != want {
			t.Errorf("%s in pieces of %v: shown as it settled, %s", name, sizes, parted(shown, want))
		}
	}
}

// parted says where got first parts from want, with the bytes around it, for
// texts too long to print whole.
func parted(got, want string) string {
	if got == want {
		return "they are the same"
	}
	i := 0
	for i < len(got) && i < len(want) && got[i] == want[i] {
		i++
	}
	lo := max(0, i-48)
	return fmt.Sprintf("they part at byte %d of %d and %d:\n got …%q…\nwant …%q…", i, len(got), len(want),
		got[lo:min(len(got), i+48)], want[lo:min(len(want), i+48)])
}

// spaces is n spaces.
func spaces(n int) string { return strings.Repeat(" ", n) }

// AN ARMOUR'S LABEL IS 64 BYTES AT MOST, so a match, a value or a reader cut
// at an armour reads no further past its dashes than that: a label of 64
// capitals is an armour, which ends a token's match and a password's value
// written up to it and cuts a line a reader takes out by value; one of 65 is
// none, and they run on through it as through any other dashes.
//
// Mutation: read a label of any length, or one byte shorter or longer than 64,
// and the rows at that length turn.
func TestAnArmoursLabelIsSixtyFourBytesAtMost(t *testing.T) {
	t.Parallel()
	label := func(n int) string { return strings.Repeat("A", n) }
	end := func(n int) string { return "-----END " + label(n) + "-----" }
	tok := "sk-" + strings.Repeat("q", 21)
	api, pw := redact.Marker+"api-key]", redact.Marker+"password]"
	for name, c := range map[string]struct{ text, want string }{
		"a token up to a label of 64":        {tok + end(64) + "\n", api + end(64) + "\n"},
		"a token up to a label of 65":        {tok + end(65) + "\n", api + " " + label(65) + "-----\n"},
		"a password's value up to one of 64": {"pwd=hunter2" + end(64) + "\n", "pwd=" + pw + end(64) + "\n"},
		"a password's value up to one of 65": {"pwd=hunter2" + end(65) + "\n", "pwd=" + pw + " " + label(65) + "-----\n"},
	} {
		atTheBound(t, name, c.text, c.want)
	}
	for n, want := range map[int][]string{
		64: {"x", "y"},
		65: {"x -----BEGIN " + label(65) + "----- y"},
	} {
		line := "x -----BEGIN " + label(n) + "----- y"
		if got := redact.Unarmoured(line); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("Unarmoured of a label of %d = %q, want %q", n, got, want)
		}
	}
}

// A RUN TOKEN'S SUBJECT IS READ MaxReach AT MOST, and its expiry nineteen
// digits: a token whose subject is that long is redacted, and one a byte
// longer, which runtoken never mints, is no token; an expiry of nineteen
// digits, the widest Unix time a node reads back, is one, and one of twenty
// is not.
//
// Mutation: hold the subject to a byte more or less, or to nothing, or the
// expiry to eighteen digits or to any number, and a row turns.
func TestARunTokensSubjectIsReadToMaxReach(t *testing.T) {
	t.Parallel()
	sig := strings.Repeat("S", 40) + "a_-"
	token := func(subject, expiry string) string {
		return "crewlet_rt1.Tq0d_8-x." + subject + "." + expiry + "." + sig
	}
	url := func(token string) string { return "https://engine.example.com/mcp/" + token + "/x\n" }
	m := redact.Marker + "run-token]"
	subject := func(n int) string { return strings.Repeat("s", n) }
	for name, c := range map[string]struct{ text, want string }{
		"a subject of MaxReach bytes":     {url(token(subject(reach), "1792927401")), url(m)},
		"a subject of one byte more":      {url(token(subject(reach+1), "1792927401")), url(token(subject(reach+1), "1792927401"))},
		"an expiry of nineteen digits":    {url(token("t", strings.Repeat("9", 19))), url(m)},
		"an expiry of twenty digits":      {url(token("t", strings.Repeat("9", 20))), url(token("t", strings.Repeat("9", 20)))},
		"one too long, then one in reach": {url(token(subject(reach+1), "1")) + url(token("t", "1")), url(token(subject(reach+1), "1")) + url(m)},
	} {
		atTheBound(t, name, c.text, c.want)
	}
}

// A JSON WEB TOKEN'S HEADER AND CLAIMS ARE READ MaxReach AT MOST TOGETHER:
// a token whose header, the dot after it and its claims run that long is
// redacted, and one a byte longer is matched as the pattern with that bound
// would match it, from the leftmost `eyJ` from which they fit, the head before
// it left as written, or not at all where none does. One exactly MaxReach
// before the claims' end is where it is matched from, and one a byte before
// that is not.
//
// Mutation: hold them to a byte less, or to nothing, or go on looking a byte
// past the first place one can fit, and a row turns.
func TestAJSONWebTokensHeaderAndClaimsAreReadToMaxReach(t *testing.T) {
	t.Parallel()
	claims := "eyJ" + strings.Repeat("b", 30)
	sig := strings.Repeat("c", 43)
	bearer := func(header string) string { return "Authorization: Bearer " + header + "." + claims + "." + sig + "\n" }
	// header is n bytes of a header, with an `eyJ` at each of at as well as
	// at its start.
	header := func(n int, at ...int) string {
		b := []byte("eyJ" + strings.Repeat("a", n-3))
		for _, i := range at {
			copy(b[i:], "eyJ")
		}
		return string(b)
	}
	m := redact.Marker + "jwt]"
	fit := reach - len(claims) - 1
	// Openings every 3 bytes at its start and 45 to 54 in a header 50 bytes
	// past fitting: the first from which it fits is at 51.
	many := header(fit+50, 3, 6, 9, 12, 45, 48, 51, 54)
	for name, c := range map[string]struct{ text, want string }{
		"header and claims of MaxReach":  {bearer(header(fit)), "Authorization: Bearer " + m + "\n"},
		"of one byte more, and no other": {bearer(header(fit + 1)), bearer(header(fit + 1))},
		"of one byte more, an eyJ inside its header": {
			bearer(header(fit+1, 3)), "Authorization: Bearer eyJ" + m + "\n",
		},
		"an eyJ inside its header MaxReach before the claims' end": {
			bearer(header(fit+3, 3)), "Authorization: Bearer eyJ" + m + "\n",
		},
		"an eyJ inside its header a byte further back": {bearer(header(fit+4, 3)), bearer(header(fit+4, 3))},
		"many an eyJ inside its header":                {bearer(many), "Authorization: Bearer " + many[:51] + m + "\n"},
	} {
		atTheBound(t, name, c.text, c.want)
	}
}

// AN SSH CERTIFICATE'S WHITESPACE IS READ MaxReach AT MOST: a certificate
// whose type's suffix and body that much whitespace parts is redacted, and one
// a byte more parts is not.
//
// Mutation: hold the whitespace to a byte less, or to nothing, and a row
// turns.
func TestAnSSHCertificatesWhitespaceIsReadToMaxReach(t *testing.T) {
	t.Parallel()
	body := "AAAAIHNzaC1lZDI1NTE5LWNlcnQtdjAxQG9wZW5zc2guY29tAAAAIG9wZW5zc2g"
	cert := func(n int) string { return "ssh-ed25519-cert-v01@openssh.com" + spaces(n) + body + " box@host\n" }
	m := redact.Marker + "ssh-certificate]"
	for name, c := range map[string]struct{ text, want string }{
		"MaxReach of whitespace":    {cert(reach), "ssh-ed25519" + m + " box@host\n"},
		"a byte of whitespace more": {cert(reach + 1), cert(reach + 1)},
	} {
		atTheBound(t, name, c.text, c.want)
	}
}

// AN AWS SECRET'S SEPARATOR IS READ MaxReach AT MOST: a value that far from
// its key, in spaces alone or in an operator and the spaces around it, is
// taken out, and one a byte further is not.
//
// Mutation: hold the separator to a byte more, or to nothing, and a row turns.
func TestAnAWSSecretsSeparatorIsReadToMaxReach(t *testing.T) {
	t.Parallel()
	token := strings.Repeat("Ab3+", 60) + "=="
	m := redact.Marker + "session-token]"
	set := func(sep, value string) string { return "aws configure set aws_session_token" + sep + value + "\n" }
	env := func(sep, value string) string { return "AWS_SESSION_TOKEN" + sep + value + "\n" }
	for name, c := range map[string]struct{ text, want string }{
		"MaxReach of spaces":                      {set(spaces(reach), token), set(spaces(reach), m)},
		"a space more":                            {set(spaces(reach+1), token), set(spaces(reach+1), token)},
		"an operator and its spaces, MaxReach":    {env(" ="+spaces(reach-2), token), env(" ="+spaces(reach-2), m)},
		"an operator and its spaces, a byte more": {env(" ="+spaces(reach-1), token), env(" ="+spaces(reach-1), token)},
	} {
		atTheBound(t, name, c.text, c.want)
	}
}

// A PASSWORD'S KEY IS ONE WHERE ITS SEPARATOR BEGINS WITHIN MaxReach OF IT,
// and a key with none yet waits for one no longer than that. Whether a word is
// a key can wait on a separator lines later, so its reads were not bounded by
// its line.
//
// Read beside a key's block, whose lines a password's key and separator are
// never a line of: a key's line ending in `pwd`, a separator below it MaxReach
// less one byte from it, is no line of the key and is published, and with the
// separator a byte further it is the key's line and redacted with the block.
// And as a live view shows it: a key at the end of what is written, the text a
// byte short of MaxReach past it, is held back, since a separator written next
// would make it a key; MaxReach past it, it is not.
//
// Mutation: read a key's separator however far it is, or one byte further,
// and the line ending in `pwd` turns; let a key wait for its separator however
// far, and the live view holds back what no separator can reach.
func TestAPasswordKeysSeparatorIsReadToMaxReach(t *testing.T) {
	t.Parallel()
	block := "-----BEGIN RSA PRIVATE KEY-----\n" + keyLine + "\n"
	ending := keyLine[:61] + "pwd\n"
	// The separator is the key's (3 bytes), the line break's (1) and n
	// spaces' bytes past the key: within MaxReach of it for n < MaxReach-4.
	below := func(n int) string { return spaces(n) + ": x\n" }
	key := redact.Marker + "private-key]"
	for name, c := range map[string]struct{ text, want string }{
		"a separator MaxReach less a byte below": {
			block + ending + below(reach-5), key + "\n" + ending + below(reach-5),
		},
		"a separator MaxReach below": {block + ending + below(reach-4), key + "\n" + below(reach-4)},
	} {
		atTheBound(t, name, c.text, c.want)
	}

	// The key is 8 bytes into the text and its line is the one starting at
	// 2; the text is 9 bytes and n spaces past the key.
	awaited := func(n int) string { return "x\nEnter password" + spaces(n) + "\n" }
	for n, want := range map[int]int{reach - 10: 2, reach - 9: len(awaited(reach - 9))} {
		if got := redact.Settled(awaited(n)); got != want {
			t.Errorf("a key %d bytes before the text's end: Settled = %d, want %d", 9+n, got, want)
		}
		text := awaited(n) + "= hunter2x9\n"
		if shown, _ := settle(text, []int{4093}); shown != redact.Secrets(text) {
			t.Errorf("a key %d bytes before its separator: shown as it settled, %s", 9+n,
				parted(shown, redact.Secrets(text)))
		}
	}
}

// A PASSWORD'S VALUE IS READ MaxReach PAST WHERE ITS KEY BEGINS AT MOST, on
// its line and below it: a value whose first byte is the last within it is
// taken out, and one a byte further is not, read as though the text ended
// before it. And a line under a wrapping's suffix is the value's only where it
// ends within it: ending a byte further, the bound cuts it before its suffix,
// and the key's punctuation is the value, unless a marker stands where the
// value would begin. A pass that took the value wrote one there, longer than
// the value, which carries the line's suffix past the bound: read as a line
// the bound cuts like any other, a second pass took the key's quote too. A
// line the text itself ends on is read as it always was, the key's quote its
// value, and so is a line whose escaped break is followed by a word the bound
// then cuts: however that goes on, the line does not end in the suffix, so the
// bound decided nothing. A line the bound cuts just past bytes that spell the
// suffix ends in none, since it goes on: read as ending in it, its value a
// line long was taken, and a second pass, that value nineteen bytes and the
// line whole within the bound, took the key's quote. And a table's row holding a marker of a value closes
// no key's row: a value a row long that the bound cut on the first pass, which
// took it for the key's value, is nineteen bytes on the second, the row's
// closing inside the bound, and read as a row that closes, it gave the key
// its own closing for its value. Read as cut, with a token's marker where the value
// would begin, the key named no value, and once the pass had replaced the
// value of the key after the break, which ran past the bound, the line after
// the break ended within it and a second pass took the key's quote.
//
// Mutation: read the key's line or the lines below it past the bound, or a
// byte short of it, or read a line the bound cuts as cut whatever stands
// where its value would begin, or a line the text ends on, or a line whose
// escaped break a word the bound cuts follows, as one the bound cuts, or a
// line the bound cuts as ending where its bytes spell the suffix, or a row
// holding a marker as one that closes, and a row turns.
func TestAPasswordsValueIsReadToMaxReachPastItsKey(t *testing.T) {
	t.Parallel()
	m := redact.Marker + "password]"
	// Each key opens its text but the echoed one, 6 bytes in. The value
	// begins 4 bytes and n past `pwd:`'s start, and 11 and n past
	// `password:`'s, a line of n spaces between; and the echoed value's line,
	// 22 bytes and n, ends 37 and n past its key.
	same := func(n int, value string) string { return "pwd:" + spaces(n) + value + " next\n" }
	below := func(n int, value string) string { return "password:\n" + spaces(n) + "\n" + value + "\n" }
	echoed := func(n int, value string) string {
		return "echo \"Password:\" >> f\necho \"" + value + " " + spaces(n) + "\" >> f\n"
	}
	// A token, an escaped break and a key whose value, 9 bytes and n, ends 58
	// and n past the echoed key: the text's last line, which the bound cuts
	// for n past MaxReach-58.
	broken := func(n int) string {
		return "echo \"Password:\" >> f\nsk-" + strings.Repeat("q", 24) + `\npwd=hunter2x9` + strings.Repeat("h", n) + "\n"
	}
	// A value n bytes long on a line under the echoed key, its suffix's last
	// byte the last within the bound for n of MaxReach-28, and two bytes
	// after it on the line.
	goesOn := func(n int) string {
		return "echo \"Password:\" >> f\necho \"" + strings.Repeat("h", n) + "\" >> f!!\n"
	}
	// A table's key, a blank row and a row holding a value n bytes long and
	// another key, closing 27 bytes and n past the first key's start.
	table := func(n int) string { return "| Password: |\n\n" + strings.Repeat("=", n) + " | Password: |\n" }
	brokenWant := "echo \"Password:" + m + " >> f\n" + redact.Marker + `api-key]\npwd=` + m + "\n"
	for name, c := range map[string]struct{ text, want string }{
		"on its line, its first byte the last within": {same(reach-5, "hunter2x9"), same(reach-5, m)},
		"on its line, a byte further":                 {same(reach-4, "hunter2x9"), same(reach-4, "hunter2x9")},
		"below its key, its first byte the last within": {
			below(reach-12, "hunter2x9"), below(reach-12, m),
		},
		"below its key, a byte further": {below(reach-11, "hunter2x9"), below(reach-11, "hunter2x9")},
		"on a line whose suffix ends the last byte within": {
			echoed(reach-38, "hunter2x9"), echoed(reach-38, m),
		},
		"on a line whose suffix ends a byte further": {
			echoed(reach-37, "hunter2x9"),
			"echo \"Password:" + m + " >> f\necho \"hunter2x9 " + spaces(reach-37) + "\" >> f\n",
		},
		"a marker where the value would begin, on a line the bound cuts": {
			echoed(reach-37, m), echoed(reach-37, m),
		},
		"a marker where the value would begin, on the line the text ends on": {
			"echo \"Password:\" >> f\necho \"" + m + " x\"", "echo \"Password:" + m + " >> f\necho \"" + m + " x\"",
		},
		"a line whose bytes up to the bound end in the suffix, going on past it": {
			goesOn(reach - 28), "echo \"Password:" + m + " >> f\necho \"" + strings.Repeat("h", reach-28) + "\" >> f!!\n",
		},
		"a table's value a line long, on a row the bound cuts before its closing": {
			table(reach), "| Password: |\n\n" + m + " | Password: |\n",
		},
		"a table's value on a row closing within the bound": {
			table(reach - 100), "| Password: " + m + "\n\n" + strings.Repeat("=", reach-100) + " | Password: |\n",
		},
		"a word after the line's escaped break, ending within the bound": {
			broken(reach - 60), brokenWant,
		},
		"a word after the line's escaped break, running past it": {
			broken(reach), brokenWant,
		},
	} {
		atTheBound(t, name, c.text, c.want)
	}
}

// INSIDE A PASSWORD'S VALUE, WHAT FOLLOWS AN ESCAPED BREAK IS READ MaxReach AT
// MOST. Punctuation after the break up to a space within it is the string's,
// and published, while punctuation running past it with no letter, digit or
// space is read as a letter following, and the value runs on over it: redacting
// more, where ending the value at the break would publish a password whose
// letters lie past the bound. And a key on the line after the break ends the
// value there only where the key and its separator lie within it: a byte
// further, the value runs on over the key, which then names nothing.
//
// Mutation: read past the bound after a break, or read its end as the text's,
// or read the line after it to its end, and a row turns.
func TestWhatFollowsAnEscapedBreakInAValueIsReadToMaxReach(t *testing.T) {
	t.Parallel()
	m := redact.Marker + "password]"
	bangs := func(n int) string { return strings.Repeat("!", n) }
	keyed := func(n int) string { return `"pwd=abc\n` + strings.Repeat("Q", n) + `password: hunter2x9"` + "\n" }
	for name, c := range map[string]struct{ text, want string }{
		"punctuation up to a space within the bound": {
			`pwd=x\n` + bangs(reach-1) + " next\n", "pwd=" + m + `\n` + bangs(reach-1) + " next\n",
		},
		"punctuation running past it": {
			`pwd=x\n` + bangs(reach) + " next\n", "pwd=" + m + `\n` + m + " next\n",
		},
		"a key and its separator within it": {
			keyed(reach - 9), `"pwd=` + m + `\n` + strings.Repeat("Q", reach-9) + "password: " + m + "\n",
		},
		"a key whose separator is a byte further": {
			keyed(reach - 8), `"pwd=` + m + `\n` + m + ` hunter2x9"` + "\n",
		},
	} {
		atTheBound(t, name, c.text, c.want)
	}
}

// A PASSWORD KEY'S LINE IS READ BACK MaxReach AT MOST. The markup a key's line
// opened before its label is the key's where it lies within it, and a byte
// further is read as though the text began there, so the markup after the key
// is taken for its value and the value published. And a quoted key's name that
// runs back MaxReach is the key's own string and its value taken out, never
// refused as a string closed elsewhere, as a name a byte shorter, with a space
// before it, is.
//
// Mutation: read the label or the name back past the bound, or refuse a name
// that runs back to it, and a row turns.
func TestAPasswordKeysLineIsReadBackToMaxReach(t *testing.T) {
	t.Parallel()
	m := redact.Marker + "password]"
	// The bold label's `**` is the 2 bytes before a label of n.
	bold := func(n int, rest string) string { return "**" + strings.Repeat("x", n) + "password:*" + rest + "\n" }
	named := func(n int, value string) string {
		return "say " + strings.Repeat("a", n) + `password": ` + value + "\n"
	}
	for name, c := range map[string]struct{ text, want string }{
		"a bold label whose markup lies within it":    {bold(reach-2, "* hunter2x9"), bold(reach-2, "* "+m)},
		"a bold label whose markup is a byte further": {bold(reach-1, "* hunter2x9"), bold(reach-1, m+" hunter2x9")},
		"a quoted key's name a byte short of it":      {named(reach-1, "hunter2x9"), named(reach-1, "hunter2x9")},
		"a quoted key's name running back to it":      {named(reach, "hunter2x9"), named(reach, m)},
	} {
		atTheBound(t, name, c.text, c.want)
	}
}

// AN END LINE'S REST IS READ MaxReach PAST ITS ARMOUR AT MOST. A wrapping's
// suffix after the armour is the END line's where the line ends within it, and
// a line going on a byte further has none a line above it ends in, so a key
// echoed into a file a line at a time, its END line run on, is read as under no
// wrapping. Its lines are then a key's only as lines a cut wrote into, which
// asks a digit of their head, so a key whose lines hold none is published. And
// a marker on the END's line is read only within it: one a byte further is
// not, and the key above the END is read back.
//
// Mutation: read the END line to its end, or keep its suffix however far it
// runs, and a row turns.
func TestAnEndLinesRestIsReadToMaxReach(t *testing.T) {
	t.Parallel()
	key := redact.Marker + "private-key]"
	end := "-----END RSA PRIVATE KEY-----"
	echoed := func(n int) string {
		var b strings.Builder
		for range 3 {
			b.WriteString("echo \"" + digitless + "\" >> key.pem\n")
		}
		b.WriteString("echo \"" + end + "\" >> key.pem" + spaces(n) + "\n")
		return b.String()
	}
	// `" >> key.pem` is 12 bytes of the END line's rest, and a marker's
	// opening 10.
	body := keyLine + "\n" + keyLine + "\n" + keyLine + "\n"
	marked := func(n int) string { return body + end + spaces(n) + redact.Marker + "api-key] x\n" }
	for name, c := range map[string]struct{ text, want string }{
		"a suffix the END line ends in within it": {
			echoed(reach - 12), "echo \"" + key + "\" >> key.pem" + spaces(reach-12) + "\n",
		},
		"the END line going on a byte further": {echoed(reach - 11), echoed(reach - 11)},
		"a marker on the END line within it":   {marked(reach - 10), marked(reach - 10)},
		"a marker a byte further":              {marked(reach - 9), key + marked(reach - 9)[len(body)+len(end):]},
	} {
		atTheBound(t, name, c.text, c.want)
	}
}

// A PASSWORD KEY'S LINE IS READ FOR ITS GUTTER ONLY WHERE IT BEGINS WITHIN
// MaxReach OF THE KEY, AS WRITTEN AND AS THE RULE LEAVES IT, every value the
// rule replaced on it before the key written as its marker; past that, it has
// none, and the value under the key is read from the start of its line, past
// the markers it opens with but a password's, the gutter's punctuation
// redacted with it and a gutter's word taken for the value: never from where
// the bound falls, which may be a gutter's start that the line's own is not.
// Read from the bound, the gutter began wherever the bound fell, which a
// marker longer or shorter than the value it replaced moved, so a second pass
// read another gutter and took another value: a value of one byte, its marker
// nineteen, carried a line within the bound past it, and a value a line long,
// its marker nineteen, brought a line past the bound within it. And read from
// a marker a line opens with, a token every line of a log carries, it named
// no value, while a second pass, the key's line within the bound, matched the
// marker as its gutter's and took the value after it.
//
// Mutation: read the bound on the line as written alone, and the rows whose
// markers carry the line past it turn; read the gutter from the bound where
// the line begins past it, and the row whose bound falls at a gutter's start
// turns; take a line beginning exactly at the bound for one that begins
// past it, in a string or on its own line, and the row at the bound turns; or
// read the value from the marker a line opens with where the key's line has
// no gutter, and the row a token opens past the bound turns.
func TestAKeysLineIsReadForItsGutterWithinMaxReach(t *testing.T) {
	t.Parallel()
	m := redact.Marker + "password]"
	// A diff's line holding a value of one byte and, n spaces on, a key
	// whose line begins 14 bytes and n before it, 32 and n once the value
	// is its marker; the key's value is on the next line, behind the
	// diff's `+`.
	grows := func(n int) string { return "+pwd=a" + spaces(n) + " Retype password:\n+hunter2x9\n" }
	// The same with the first value n bytes long, on a line that begins
	// 13 bytes and n before the key, after a line of its own.
	shrinks := func(n int) string {
		return "x\n+pwd=" + strings.Repeat("h", n) + " Retype password:\n+hunter2x9\n"
	}
	// The same in a string, its breaks escaped.
	escaped := func(n int) string {
		return `"x\n+pwd=a` + spaces(n) + ` Retype password:\n+hunter2x9\n"` + "\n"
	}
	// An echoed key, its value the line's quote, and n spaces on another
	// key whose line begins 21 bytes and n before it, 39 and n once that
	// quote is its marker.
	echoed := func(n int) string {
		return "echo \"Password:\" >> f" + spaces(n) + "password:\necho \"hunter2x9\" >> f\n"
	}
	// A token opening every line, the key's line carried past the bound by
	// a value n bytes long before the key.
	tokened := func(n int) string {
		token := "sk-" + strings.Repeat("q", 24)
		return token + " pwd=" + strings.Repeat("h", n) + " pwd:\n" + token + " hunter2x9\n"
	}
	api := redact.Marker + "api-key]"
	for name, c := range map[string]struct{ text, want string }{
		"a line its markers carry to the bound": {
			grows(reach - 32), "+pwd=" + m + spaces(reach-32) + " Retype password:\n+" + m + "\n",
		},
		"a line its markers carry a byte past it": {
			grows(reach - 31), "+pwd=" + m + spaces(reach-31) + " Retype password:\n" + m + "\n",
		},
		"a line beginning at the bound, its value a line long": {
			shrinks(reach - 13), "x\n+pwd=" + m + " Retype password:\n+" + m + "\n",
		},
		"a line beginning a byte past it": {
			shrinks(reach - 12), "x\n+pwd=" + m + " Retype password:\n" + m + "\n",
		},
		"a line beginning past it, the bound at its diff's mark": {
			"xxxxxxxxxx" + shrinks(reach - 13)[2:], "xxxxxxxxxx+pwd=" + m + " Retype password:\n" + m + "\n",
		},
		"a string's line its markers carry to the bound": {
			escaped(reach - 32), `"x\n+pwd=` + m + spaces(reach-32) + ` Retype password:\n+` + m + `\n"` + "\n",
		},
		"a string's line its markers carry a byte past it": {
			escaped(reach - 31), `"x\n+pwd=` + m + spaces(reach-31) + ` Retype password:\n` + m + `\n"` + "\n",
		},
		"an echoed line its marker carries to the bound": {
			echoed(reach - 39), "echo \"Password:" + m + " >> f" + spaces(reach-39) + "password:\necho \"" + m + " >> f\n",
		},
		"an echoed line its marker carries a byte past it": {
			echoed(reach - 38), "echo \"Password:" + m + " >> f" + spaces(reach-38) + "password:\n" + m + " \"hunter2x9\" >> f\n",
		},
		"a line a token opens within the bound": {
			tokened(10), api + " pwd=" + m + " pwd:\n" + api + " " + m + "\n",
		},
		"a line a token opens past it": {
			tokened(reach), api + " pwd=" + m + " pwd:\n" + api + " " + m + "\n",
		},
	} {
		atTheBound(t, name, c.text, c.want)
	}
}

// A KEY ON THE REST OF A LINE AN ESCAPED BREAK ENDED A KEY'S LINE ON NAMES WHAT
// FOLLOWS IT, WHETHER OR NOT THE STRING GOES ON past it to another escaped
// line: the punctuation that rest opens with is no value of the first key's,
// which names none, and the second key reads its own. Read as one of the
// string's lines where it went on, the rest gave the first key its opening
// `|` for a value, and the second key, its line no longer opened, its own
// closing `|`, publishing what followed; and where the bound cut the rest, no
// escaped line followed it within the bound, so the key on it ended the first
// key's read, until a second pass, the second key's value now its marker,
// found the string going on within the bound and took the `|`.
//
// Mutation: read the rest as one of the string's lines before looking for a
// key on it, and both rows turn.
func TestAKeyOnTheRestOfALineNamesItsValueWhereverTheStringGoes(t *testing.T) {
	t.Parallel()
	m := redact.Marker + "password]"
	row := func(n int) string {
		return `x=" | Password: |\n| Password: | x` + strings.Repeat("A", n) + `| hunter2 |\n` + "\n"
	}
	want := `x=" | Password: |\n| Password: | ` + m + ` hunter2 |\n` + "\n"
	for name, n := range map[string]int{
		"a rest the string goes on past": 10,
		"a rest the bound cuts":          reach,
	} {
		atTheBound(t, name, row(n), want)
	}
}
