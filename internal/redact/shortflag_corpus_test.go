/*
Copyright 2026 The Shadewright Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package redact

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/redact/redacttest"
)

// The short password flags of #1075 (decision of 2026-11-22): a fixed table of tools and the short flag each reads
// a password from, decided by the command word, and the arguments of a sequence with no command word. The corpus
// is built with its answer known: the secret of each input is the value of a password flag, written in one of the
// forms a tool reads it, inside one of the contexts a command reaches the redactor in; the text around it is
// ordinary words that must stay.

// shortTool is a row of the table as the decision gives it, written here apart from the rule.
type shortTool struct {
	name   string
	letter byte
	// glued: the tool reads its password only glued to the flag ("-pX"); "-p X" is a prompt and a database name.
	glued bool
	// colon: the password is the part of the value after its first colon ("-u user:pass").
	colon bool
}

// shortMySQL is the first tool of the table, the one that reads its password glued only.
const shortMySQL = "mysql"

var shortTools = []shortTool{
	{name: shortMySQL, letter: 'p', glued: true}, {name: "mariadb", letter: 'p', glued: true},
	{name: "mongo", letter: 'p'}, {name: "mongosh", letter: 'p'}, {name: "cqlsh", letter: 'p'},
	{name: "rabbitmqadmin", letter: 'p'}, {name: "sshpass", letter: 'p'},
	{name: "redis-cli", letter: 'a'},
	{name: "sqlcmd", letter: 'P'}, {name: "mosquitto_pub", letter: 'P'}, {name: "mosquitto_sub", letter: 'P'}, //nolint:misspell // the MQTT clients' names
	{name: "curl", letter: 'u', colon: true},
}

// shortFlagCase is one input of the corpus: a text, or the strings of a sequence (items), with the secrets that
// must be hidden and the pieces of text around them that must stay.
type shortFlagCase struct {
	form    string
	in      string
	items   []string
	secrets []string
	kept    []string
}

// shortOut is the redacted text of c: String of the text, or Strings of the items joined by line breaks.
func (c shortFlagCase) out() string {
	if c.items != nil {
		return strings.Join(Strings(c.items), "\n")
	}
	return String(c.in)
}

// shortSecret returns a secret no other rule of main takes: 12 to 15 letters and digits, with a symbol inside
// some of them, too short for the long-token rule.
func shortSecret(rng *rand.Rand, i int) string {
	const alnum = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"
	var b strings.Builder
	fmt.Fprintf(&b, "Sw%03d", i%1000)
	for range 7 + rng.IntN(3) {
		b.WriteByte(alnum[rng.IntN(len(alnum))])
	}
	if sym := []string{"", "", "%", "+", ".", "_", "~", "^", "*"}[rng.IntN(9)]; sym != "" {
		b.WriteString(sym)
		b.WriteByte(alnum[rng.IntN(len(alnum))])
	}
	return b.String()
}

// shortArgForm is how a flag and its value are written: the words of the argument list, with the secret where
// the value goes. separated forms put the value in a word of its own.
type shortArgForm struct {
	name      string
	separated bool
	words     func(t shortTool, value string) []string
}

func shortFlag(t shortTool) string { return "-" + string(t.letter) }

var shortArgForms = []shortArgForm{
	{name: "glued", words: func(t shortTool, v string) []string { return []string{shortFlag(t) + v} }},
	{name: "equals", words: func(t shortTool, v string) []string { return []string{shortFlag(t) + "=" + v} }},
	{name: "separated", separated: true, words: func(t shortTool, v string) []string { return []string{shortFlag(t), v} }},
	// Combined short flags: a flag of no value before the password flag in one word ("-vpX", "-sSu user:X").
	{name: "combined glued", words: func(t shortTool, v string) []string { return []string{"-v" + string(t.letter) + v} }},
	{name: "combined separated", separated: true,
		words: func(t shortTool, v string) []string { return []string{"-v" + string(t.letter), v} }},
}

// shortValue is the value of t's flag that holds secret: the user, a colon and the secret for curl.
func shortValue(t shortTool, secret string) string {
	if t.colon {
		return "admin:" + secret
	}
	return secret
}

// Contexts a command reaches the redactor in, each given the command word as written and the argument words:
// the text, and for a sequence its items.
type shortContext struct {
	name string
	text func(cmd string, args []string) (in string, items []string)
}

// shellJoin writes words as a command line.
func shellJoin(cmd string, args []string) string {
	return strings.Join(append([]string{cmd}, args...), " ")
}

// jsonArray writes words as a JSON array of strings.
func jsonArray(words []string, indent string) string {
	b, err := json.Marshal(words)
	if err != nil {
		panic(err)
	}
	if indent == "" {
		return string(b)
	}
	quoted := make([]string, len(words))
	for i, w := range words {
		q, _ := json.Marshal(w)
		quoted[i] = indent + "  " + string(q)
	}
	return "[\n" + strings.Join(quoted, ",\n") + "\n" + indent + "]"
}

var shortContexts = []shortContext{
	{name: "command line", text: func(cmd string, args []string) (string, []string) { return shellJoin(cmd, args), nil }},
	{name: "log line", text: func(cmd string, args []string) (string, []string) {
		return "I1122 10:00:00.000000 1 run.go:42] running " + shellJoin(cmd, args) + " (attempt 2)", nil
	}},
	{name: "absolute path", text: func(cmd string, args []string) (string, []string) {
		return shellJoin("/usr/local/bin/"+cmd, args), nil
	}},
	{name: "env", text: func(cmd string, args []string) (string, []string) {
		return "env TZ=UTC LANG=C " + shellJoin(cmd, args), nil
	}},
	{name: "sh -c single quotes", text: func(cmd string, args []string) (string, []string) {
		return "sh -c '" + shellJoin(cmd, args) + "'", nil
	}},
	{name: "sh -c double quotes", text: func(cmd string, args []string) (string, []string) {
		return `/bin/sh -c "` + shellJoin(cmd, args) + ` && echo done"`, nil
	}},
	{name: "after another command", text: func(cmd string, args []string) (string, []string) {
		return "cd /work; " + shellJoin(cmd, args) + " | tee out.log", nil
	}},
	{name: "JSON array", text: func(cmd string, args []string) (string, []string) {
		return `{"command":` + jsonArray(append([]string{cmd}, args...), "") + `,"name":"app"}`, nil
	}},
	{name: "JSON array, one line each", text: func(cmd string, args []string) (string, []string) {
		return "{\n \"command\": " + jsonArray(append([]string{cmd}, args...), " ") + ",\n \"name\": \"app\"\n}", nil
	}},
	{name: "JSON array after sh -c", text: func(cmd string, args []string) (string, []string) {
		return jsonArray([]string{"sh", "-c", shellJoin(cmd, args)}, ""), nil
	}},
	{name: "escaped JSON array", text: func(cmd string, args []string) (string, []string) {
		inner := jsonArray(append([]string{cmd}, args...), "")
		q, _ := json.Marshal(inner)
		return `{"msg":` + string(q) + `}`, nil
	}},
	{name: "YAML block sequence", text: func(cmd string, args []string) (string, []string) {
		return "command:\n- " + strings.Join(append([]string{cmd}, args...), "\n- ") + "\nname: app\n", nil
	}},
	{name: "sequence of strings", text: func(cmd string, args []string) (string, []string) {
		return "", append([]string{cmd}, args...)
	}},
}

// shortTail is the text after the password flag in a command, and shortHead the argument before it: words that
// must stay. shortLogLevel and shortDebug are the argument before the flag in arguments with no command word.
const (
	shortHead     = "--connect-timeout"
	shortTail     = "--verbose"
	shortLogLevel = "--log-level"
	shortDebug    = "debug"
)

// shortToolCorpus returns, for every tool of the table, every argument form the tool reads its password in
// (the separated forms not for a tool that reads it glued only), in every context: the secret is hidden, and the
// command word, the argument before the flag and the one after the value stay.
func shortToolCorpus() []shortFlagCase {
	rng := rand.New(rand.NewPCG(1075, 1)) //nolint:gosec // a fixed seed keeps the corpus the same on every run
	var out []shortFlagCase
	i := 0
	for _, t := range shortTools {
		for _, f := range shortArgForms {
			if f.separated && t.glued {
				continue
			}
			for _, ctx := range shortContexts {
				secret := shortSecret(rng, i)
				i++
				args := append([]string{shortHead, "9"}, f.words(t, shortValue(t, secret))...)
				args = append(args, shortTail)
				in, items := ctx.text(t.name, args)
				out = append(out, shortFlagCase{
					form: "tool " + f.name + ", " + ctx.name, in: in, items: items,
					secrets: []string{secret}, kept: []string{t.name, shortHead, shortTail},
				})
			}
		}
	}
	return out
}

// Contexts of arguments with no command word: the arguments of a container whose entrypoint is in its image, as
// an object holds them (a JSON or YAML "args"), a sequence whose first item is a flag, and Strings of such items.
var shortArgsContexts = []shortContext{
	{name: "JSON args", text: func(_ string, args []string) (string, []string) {
		return `{"name":"app","args":` + jsonArray(args, "") + `,"image":"db:1"}`, nil
	}},
	{name: "JSON args, one line each", text: func(_ string, args []string) (string, []string) {
		return "{\n \"name\": \"app\",\n \"args\": " + jsonArray(args, " ") + ",\n \"image\": \"db:1\"\n}", nil
	}},
	{name: "escaped JSON args", text: func(_ string, args []string) (string, []string) {
		inner := `{"args":` + jsonArray(args, "") + `}`
		q, _ := json.Marshal(inner)
		return `{"msg":` + string(q) + `}`, nil
	}},
	{name: "YAML block args", text: func(_ string, args []string) (string, []string) {
		return "containers:\n- name: app\n  args:\n  - " + strings.Join(args, "\n  - ") + "\n  image: db:1\n", nil
	}},
	{name: "YAML flow args", text: func(_ string, args []string) (string, []string) {
		return "args: [" + strings.Join(args, ", ") + "]\nimage: db:1\n", nil
	}},
	{name: "sequence starting with a flag", text: func(_ string, args []string) (string, []string) {
		return "starting with arguments " + jsonArray(args, ""), nil
	}},
	{name: "strings starting with a flag", text: func(_ string, args []string) (string, []string) {
		return "", args
	}},
}

// shortArgsFlags are the flags whose value is hidden in arguments with no command word, with the value each
// takes the secret in.
var shortArgsFlags = []shortTool{{letter: 'p'}, {letter: 'P'}, {letter: 'a'}, {letter: 'u', colon: true}}

// shortArgsCorpus returns arguments with no command word: each flag of the decision glued, after "=" and as a
// word of its own, in every such context; the secret is hidden, the flag before it and the argument after it
// stay.
func shortArgsCorpus() []shortFlagCase {
	rng := rand.New(rand.NewPCG(1075, 2)) //nolint:gosec // a fixed seed keeps the corpus the same on every run
	out := make([]shortFlagCase, 0, len(shortArgsFlags)*3*len(shortArgsContexts))
	i := 0
	for _, t := range shortArgsFlags {
		for _, f := range shortArgForms[:3] {
			for _, ctx := range shortArgsContexts {
				secret := shortSecret(rng, i)
				i++
				args := append([]string{shortLogLevel, shortDebug}, f.words(t, shortValue(t, secret))...)
				if strings.HasPrefix(ctx.name, "sequence") || strings.HasPrefix(ctx.name, "strings") {
					args = append(f.words(t, shortValue(t, secret)), shortLogLevel, shortDebug)
				}
				args = append(args, shortTail)
				in, items := ctx.text("", args)
				out = append(out, shortFlagCase{
					form: "args " + f.name + ", " + ctx.name, in: in, items: items,
					secrets: []string{secret}, kept: []string{shortLogLevel, shortTail},
				})
			}
		}
	}
	return out
}

// shortKeptCases are inputs whose flags are a port or another meaning for their command word, or a value that
// is a port in arguments with no command word, and a short flag with no command word in free text: each comes
// out as main redacts it (shortMainTable), and that is whole.
func shortKeptCases() []shortFlagCase {
	var out []shortFlagCase
	add := func(form string, ins ...string) {
		for _, in := range ins {
			out = append(out, shortFlagCase{form: form, in: in})
		}
	}
	add("other tool",
		"psql -h db -p 5432 -U app orders", "pg_dump -p 5433 orders", "ssh -p 22 deploy@bastion uptime",
		"scp -P 2222 file deploy@bastion:/tmp", "redis-server -p 6380", "redis-server --port 6380",
		"docker run -p 80:80 -p 443:443 nginx", "mkdir -p /var/lib/app/data", "kubectl -n prod get pods -o wide",
		"kubectl port-forward -n prod svc/web 8080:80", "nc -p 4000 host 80", "app -p hunter",
		`["psql","-h","db","-p","5432"]`, `["mkdir","-p","/data"]`, `["docker","run","-p","80:80","web"]`,
		"command:\n- ssh\n- -p\n- \"22\"\n- bastion\n", "Usage: mkdir -p DIR; then rerun",
		// A container's arguments after a command that is not in the table: they have a command word.
		`{"command":["/docker-entrypoint.sh"],"args":["-p","Sw9KeptWord"]}`,
		"command:\n- /docker-entrypoint.sh\nargs:\n- -p\n- Sw8KeptWord\n",
	)
	add("other meaning of a table tool's flag",
		"mysql -h db -P 3306 -u app orders", "mysql -u app -p orders", "mariadb -P 3307 -p shop",
		"curl -u admin https://example.com/api", "curl -s -u deploy https://example.com/x",
		"redis-cli -p 6380 -h cache ping", "sqlcmd -S db -p",
		"mosquitto_sub -p 1883 -t sensors/#", //nolint:misspell // the MQTT client's name
	)
	add("port with no command word",
		`{"args":["-p","5432","--log-level","debug"]}`, `{"args":["-p5432"]}`, `{"args":["-P=3306"]}`,
		"args:\n- -p\n- \"8080\"\n- --verbose\n", `{"args":["-u","db:5432"]}`, "[-p 9090 --verbose]",
		`{"args":["-a","65535"]}`, `{"args":["--port","-u","admin"]}`,
	)
	add("short flag with no command word in free text",
		"connection failed: -p S3cretWord is not a valid option", "error near -a token in line 3",
		"-p 9 -P 10 -a 11", "usage: -u user:pw is deprecated",
	)
	return out
}

// shortSequenceKept are sequences of strings that come out as main redacts them, whole: a command word with
// a port, and arguments with no command word whose value is a port.
var shortSequenceKept = [][]string{
	{"psql", "-p", "5432"}, {"ssh", "-p", "22", "host"}, {"mkdir", "-p", "/data"}, {shortMySQL, "-P", "3306"},
	{"-p", "5432", "--verbose"}, {"-P", "3306"}, {"-u", "db:6379"},
}

// shortNeighbourCases put a second secret right after the value of a password flag, with each glue, as another
// password flag of the same tool, a secret-named flag or a secret key: no value may take the start of the next.
func shortNeighbourCases() []shortFlagCase {
	rng := rand.New(rand.NewPCG(1075, 3)) //nolint:gosec // a fixed seed keeps the corpus the same on every run
	glues := []string{" ", "  ", "\t", ",", ", ", ";", "; ", "|", " && ", `"`, `" `, `'`, "' ", "\n", "\\\n  ", "&"}
	var out []shortFlagCase
	i := 0
	for _, t := range shortTools {
		for _, g := range glues {
			for _, next := range []func(v string) string{
				func(v string) string { return t.name + " " + shortFlag(t) + shortValue(t, v) },
				func(v string) string { return "--password " + v },
				func(v string) string { return "DB_PASSWORD=" + v },
			} {
				first, second := shortSecret(rng, i), shortSecret(rng, i+1)
				i += 2
				in := t.name + " " + shortFlag(t) + shortValue(t, first) + g + next(second) + " " + shortTail
				out = append(out, shortFlagCase{form: fmt.Sprintf("neighbour %q", g), in: in,
					secrets: []string{first, second}, kept: []string{t.name}})
			}
		}
	}
	return out
}

// shortHidden reports whether the output out of c hides every secret of c: no secret, and no piece of six bytes
// from either end of one, is left.
func shortHidden(c shortFlagCase, out string) bool {
	for _, s := range c.secrets {
		if strings.Contains(out, s) || strings.Contains(out, s[:6]) || strings.Contains(out, s[len(s)-6:]) {
			return false
		}
	}
	return true
}

// shortKept returns the first piece of c.kept missing from out, or "".
func shortKept(c shortFlagCase, out string) string {
	for _, k := range c.kept {
		if !strings.Contains(out, k) {
			return k
		}
	}
	return ""
}

// Every input of the tool corpus, the args corpus and the neighbours hides its secrets and keeps the text around
// them. On main (a9b4120f) every one of them leaks; the counts by form are in the pull request of #1075.
func TestShortFlagCorpusHidesTheKnownSecret(t *testing.T) {
	parallelUnderRace(t)
	for _, corpus := range []struct {
		name  string
		cases []shortFlagCase
	}{
		{"tools", shortToolCorpus()}, {"args", shortArgsCorpus()}, {"neighbours", shortNeighbourCases()},
	} {
		leaked, lost := 0, 0
		for i, c := range corpus.cases {
			if !raceKeeps(t.Name()+corpus.name, i) {
				continue
			}
			out := c.out()
			if !shortHidden(c, out) {
				leaked++
				if leaked <= 10 {
					t.Errorf("%s: a secret %q is left:\n in: %q %q\nout: %q", c.form, c.secrets, c.in, c.items, out)
				}
			}
			if k := shortKept(c, out); k != "" {
				lost++
				if lost <= 10 {
					t.Errorf("%s: %q is gone:\n in: %q %q\nout: %q", c.form, k, c.in, c.items, out)
				}
			}
		}
		if leaked > 0 || lost > 0 {
			t.Errorf("%s: %d of %d inputs leak a secret, %d lose the text around it", corpus.name, leaked,
				len(corpus.cases), lost)
		}
	}
}

// shortMainTable holds, for every input of the corpora above in order (shortTableInputs), main's output and the
// byte ranges it redacted (commit a9b4120f). It was written once by running TestWriteShortFlagMainTable in a
// checkout of that commit with SHORTFLAG_MAIN_TABLE set to the output path; regenerate it only from that commit.
const shortMainTable = "testdata/shortflag-main.jsonl"

// shortTableInputs are the texts of every case of the corpora above that has one, in order.
func shortTableInputs() []string {
	var out []string
	for _, cs := range [][]shortFlagCase{shortToolCorpus(), shortArgsCorpus(), shortNeighbourCases(), shortKeptCases()} {
		for _, c := range cs {
			if c.items == nil {
				out = append(out, c.in)
			}
		}
	}
	return out
}

// TestWriteShortFlagMainTable writes shortMainTable from the redactor it runs with, when SHORTFLAG_MAIN_TABLE
// names the output path; otherwise it is skipped.
func TestWriteShortFlagMainTable(t *testing.T) {
	path := os.Getenv("SHORTFLAG_MAIN_TABLE")
	if path == "" {
		t.Skip("SHORTFLAG_MAIN_TABLE is not set")
	}
	inputs := shortTableInputs()
	rows := make([]any, 0, len(inputs))
	for _, in := range inputs {
		c := mainRedaction{In: in, Out: String(in)}
		for _, sp := range defaultRedactor.spans(nil, in) {
			c.Spans = append(c.Spans, [2]int{sp.start, sp.end})
		}
		rows = append(rows, c)
	}
	writeJSONLines(t, path, rows)
}

// Nothing main redacted in the corpora is kept now (the property of #1075: no input shows text main hides), and
// every input of shortKeptCases comes out as main redacts it.
func TestShortFlagCorpusAgainstMain(t *testing.T) {
	parallelUnderRace(t)
	gen := shortTableInputs()
	kept := map[string]bool{}
	for _, c := range shortKeptCases() {
		kept[c.in] = true
	}
	keptSeen, whole := 0, 0
	n := readJSONLines(t, shortMainTable, func(i int, line []byte) {
		var row mainRedaction
		if err := json.Unmarshal(line, &row); err != nil {
			t.Fatalf("line %d: %v", i+1, err)
		}
		checkTableInput(t, shortMainTable, i+1, row.In, gen)
		now := defaultRedactor.spans(nil, row.In)
		for _, want := range row.Spans {
			for b := want[0]; b < want[1]; b++ {
				if !coveredBy([2]int{b, b + 1}, now) {
					t.Errorf("main redacted byte %d, now kept:\n in: %q\nmain: %q\n now: %q", b, row.In, row.Out,
						String(row.In))
					break
				}
			}
		}
		if kept[row.In] {
			keptSeen++
			if row.Out == row.In {
				whole++
			}
			if got := String(row.In); got != row.Out {
				t.Errorf("a kept input changed:\n in: %q\nmain: %q\n now: %q", row.In, row.Out, got)
			}
		}
	})
	if n != len(gen) {
		t.Fatalf("%s has %d lines, the corpus %d inputs", shortMainTable, n, len(gen))
	}
	if keptSeen != len(kept) || whole < keptSeen*3/4 {
		t.Fatalf("kept inputs: %d seen of %d, %d whole on main", keptSeen, len(kept), whole)
	}
	for _, items := range shortSequenceKept {
		if got := Strings(items); strings.Join(got, "\n") != strings.Join(items, "\n") {
			t.Errorf("Strings(%q) = %q, want it whole", items, got)
		}
	}
}

// The two limits the decision names: a tool not in the table keeps its short flag's value, and in arguments with
// no command word a numeric password of up to five digits that is a port number (1 to 65535) is shown. And a
// third, of the reading of a flag word: in arguments with no command word a value glued to "-p", "-P", "-a" or
// "-u" that makes the word read as a flag of a longer name with one dash (lower-case letters, digits and dashes:
// "-phunter2", as "-password" and "-port" are such flags) is shown (longFlagName). The test fails when one no
// longer leaks, so the docs are updated with the change that closes it.
func TestShortFlagNamedLimits(t *testing.T) {
	for _, in := range []string{
		"mysqldump -pSw1LimitWord orders", "pgbouncer -p Sw2LimitWord", "influx -p Sw3LimitWord",
		`{"args":["-p","31337"]}`, "args:\n- -a\n- \"4242\"\n", `{"args":["-phunter2","--verbose"]}`,
	} {
		if String(in) != in {
			t.Errorf("no longer leaks; update design 10.3 and the redaction reference:\n in: %q\nout: %q", in, String(in))
		}
	}
}

// shortFlagShapes are inputs of about n bytes that make the reading of #1075 do the most per byte: separated
// values that are the next flag, a run of flags glued by commas that one value takes, the items of a YAML "args"
// sequence, JSON args of flags, a command word on every line, and a curl value with no colon.
func shortFlagShapes(n int) map[string]string {
	rep := func(unit string) string { return strings.Repeat(unit, n/len(unit)+1) }
	return map[string]string{
		"short flag: mongosh -p repeated":      "mongosh " + rep("-p "),
		"short flag: -p, repeated":             "mongosh " + rep("-p,"),
		"short flag: YAML args items":          "args:\n" + rep("- -p\n- \n"),
		"short flag: JSON args of flags":       `{"args":[` + rep(`"-p","-a",`) + `""]}`,
		"short flag: a command on every line":  rep("mysql -vvvvvvvvpX\n"),
		"short flag: curl value with no colon": "curl -u " + rep("a"),
		"short flag: blank lines after -p":     "mongosh -p" + rep("\n   "),
		"short flag: many containers, JSON":    shortManyContainersJSON(n),
		"short flag: many containers, YAML":    shortManyContainersYAML(n),
		// Objects opened and never closed, each with an image and an "args" key: every one is closed at the end.
		"short flag: open objects with images": rep(`{"args":["-p","1"],"image":"mongo",`),
		// A YAML item per line, each one column further right: every mapping stays open to the end.
		"short flag: YAML staircase of images": shortImageStair(n),
	}
}

// shortManyContainersJSON is a pod of about n bytes of containers, each of a table image after its arguments.
func shortManyContainersJSON(n int) string {
	unit := `{"name":"db","args":["-pswabc12","-p","8080"],"image":"registry.example:5000/library/mysql:8.4"},`
	return `{"kind":"Pod","spec":{"containers":[` + strings.Repeat(unit, n/len(unit)+1) + `{}]}}`
}

// shortManyContainersYAML is a pod of about n bytes of containers in YAML, each of a table image after its
// arguments.
func shortManyContainersYAML(n int) string {
	unit := "  - args:\n    - -pswabc12\n    - -p\n    - \"8080\"\n    image: bitnami/redis:7\n    name: db\n"
	return "kind: Pod\nspec:\n  containers:\n" + strings.Repeat(unit, n/len(unit)+1)
}

// shortImageStair is about n bytes of YAML items, each one column right of the one before, with an image and
// arguments.
func shortImageStair(n int) string {
	var b strings.Builder
	for d := 0; b.Len() < n; d++ {
		pad := strings.Repeat(" ", d%512)
		b.WriteString(pad + "- image: mongo\n" + pad + "  args: [-p, x]\n")
	}
	return b.String()
}

// The shapes of shortFlagShapes are decided within the package's budget at 16 KiB, and withheld whole on a low
// budget, with the flag that says so, at every entry point that has one.
func TestShortFlagShapesWithheldOnALowBudget(t *testing.T) {
	low := redacttest.LowBudget(defaultRedactor)
	for name, in := range shortFlagShapes(16 << 10) {
		for _, e := range flaggedEntries {
			// Strings of many short lines are withheld for their heap before any work (tooManyParts), as on main.
			lines := strings.Count(in, "\n") + 1
			heap := e.name == "StringsChecked" && lines > FreePieces+(len(in)+lines)/ItemBytes
			if _, ok := e.run(in); !ok && !heap {
				t.Errorf("%s on %s of %d bytes: withheld on the package's budget", e.name, name, len(in))
			}
			out, ok := e.on(low, in)
			if ok || out[0] != Withheld || strings.Contains(strings.Join(out, ""), "-p") {
				t.Errorf("%s on %s: ok %v, %.80q; want withheld", e.name, name, ok, out)
			}
		}
	}
}
