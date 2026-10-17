package executionadapters

import (
	_ "embed"
	"encoding/json"
	"path"
	"regexp"
	"strings"
)

// Risk levels returned by ClassifyRisk, in increasing order of danger.
const (
	RiskRead        = "read"
	RiskWrite       = "write"
	RiskTest        = "test"
	RiskDestructive = "destructive"
)

var riskRank = map[string]int{RiskRead: 0, RiskTest: 1, RiskWrite: 2, RiskDestructive: 3}

// riskPolicyJSON is the shared policy, also loaded by the Python package
// (scos_execution), which implements the same algorithm; both are checked
// against risk_cases.json.
//
//go:embed risk_policy.json
var riskPolicyJSON []byte

type launcherSpec struct {
	SkipFlagsWithValue []string `json:"skip_flags_with_value"`
	SkipPositional     int      `json:"skip_positional"`
}

type subcommandSpec struct {
	Read                 []string                  `json:"read"`
	Test                 []string                  `json:"test"`
	Destructive          []string                  `json:"destructive"`
	DestructiveFlags     []string                  `json:"destructive_flags"`
	GlobalFlagsWithValue []string                  `json:"global_flags_with_value"`
	Nested               map[string]subcommandSpec `json:"nested"`
}

type riskPolicy struct {
	MaxDepth                   int                       `json:"max_depth"`
	PrivilegedLaunchers        []string                  `json:"privileged_launchers"`
	Launchers                  map[string]launcherSpec   `json:"launchers"`
	Shells                     []string                  `json:"shells"`
	Interpreters               []string                  `json:"interpreters"`
	InterpreterReadArgs        []string                  `json:"interpreter_read_args"`
	DestructivePrograms        []string                  `json:"destructive_programs"`
	DestructiveProgramPrefixes []string                  `json:"destructive_program_prefixes"`
	WritePrograms              []string                  `json:"write_programs"`
	ReadPrograms               []string                  `json:"read_programs"`
	TestPrograms               []string                  `json:"test_programs"`
	Subcommands                map[string]subcommandSpec `json:"subcommands"`
	TestCommands               []string                  `json:"test_commands"`
	HTTPClients                []string                  `json:"http_clients"`
	HTTPMutatingMethods        []string                  `json:"http_mutating_methods"`
	HTTPDataFlags              []string                  `json:"http_data_flags"`
	HTTPMethodFlags            []string                  `json:"http_method_flags"`
	HTTPOutputFlags            []string                  `json:"http_output_flags"`
	FindDestructiveFlags       []string                  `json:"find_destructive_flags"`
	FindExecFlags              []string                  `json:"find_exec_flags"`
	SedInplaceFlags            []string                  `json:"sed_inplace_flags"`
	ChmodDestructiveFlags      []string                  `json:"chmod_destructive_flags"`
	BlockDevicePrefixes        []string                  `json:"block_device_prefixes"`
	HarmlessRedirectTargets    []string                  `json:"harmless_redirect_targets"`
	ForkBombMarkers            []string                  `json:"fork_bomb_markers"`
}

var sharedRiskPolicy = mustLoadRiskPolicy()

func mustLoadRiskPolicy() riskPolicy {
	var p riskPolicy
	if err := json.Unmarshal(riskPolicyJSON, &p); err != nil {
		panic("execution-adapters: invalid embedded risk_policy.json: " + err.Error())
	}
	if len(p.DestructivePrograms) == 0 || len(p.ReadPrograms) == 0 || p.MaxDepth <= 0 {
		panic("execution-adapters: risk_policy.json is missing destructive or read programs or max_depth")
	}
	return p
}

// ClassifyRisk grades a shell command as read, test, write or destructive
// (§20.3). It is the single classifier shared by tool governance, the
// execution proxy and the control plane. It fails closed: a command is read
// only when every part of it runs a program the policy names as read-only,
// and any program the policy does not name is write, so it always passes
// through governance.
func ClassifyRisk(command string) string {
	return sharedRiskPolicy.classify(strings.TrimSpace(command), 0)
}

func (p riskPolicy) classify(command string, depth int) string {
	if depth > p.MaxDepth {
		return RiskDestructive
	}
	for _, m := range p.ForkBombMarkers {
		if strings.Contains(command, m) {
			return RiskDestructive
		}
	}
	grade := RiskRead
	for _, seg := range lexShell(command) {
		grade = maxRisk(grade, p.classifySegment(seg, depth))
	}
	return grade
}

func (p riskPolicy) classifySegment(seg shellSegment, depth int) string {
	grade := RiskRead
	for _, target := range seg.redirects {
		switch {
		case hasAnyPrefix(target, p.BlockDevicePrefixes):
			return RiskDestructive
		case strings.HasPrefix(target, "&") || contains(p.HarmlessRedirectTargets, target):
		default:
			grade = maxRisk(grade, RiskWrite)
		}
	}
	return maxRisk(grade, p.classifyWords(seg.words, depth))
}

var shellAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

func (p riskPolicy) classifyWords(words []string, depth int) string {
	if depth > p.MaxDepth {
		return RiskDestructive
	}
	for len(words) > 0 && shellAssignment.MatchString(words[0]) {
		words = words[1:]
	}
	if len(words) == 0 {
		return RiskRead
	}
	prog, args := strings.ToLower(path.Base(words[0])), words[1:]
	if contains(p.PrivilegedLaunchers, prog) {
		return RiskDestructive
	}
	if spec, ok := p.Launchers[prog]; ok {
		return p.classifyWords(unwrapLauncher(prog, spec, args), depth+1)
	}
	if contains(p.Shells, prog) {
		for i, a := range args {
			if a == "-c" && i+1 < len(args) {
				return maxRisk(RiskRead, p.classify(args[i+1], depth+1))
			}
		}
		return RiskWrite
	}
	line := strings.ToLower(strings.Join(append([]string{prog}, args...), " "))
	for _, tc := range p.TestCommands {
		if line == tc || strings.HasPrefix(line, tc+" ") {
			return RiskTest
		}
	}
	if contains(p.Interpreters, prog) {
		if len(args) == 1 && contains(p.InterpreterReadArgs, args[0]) {
			return RiskRead
		}
		return RiskWrite
	}
	if contains(p.DestructivePrograms, prog) || hasAnyPrefix(prog, p.DestructiveProgramPrefixes) {
		return RiskDestructive
	}
	switch {
	case contains(p.HTTPClients, prog):
		return p.classifyHTTP(args)
	case prog == "find":
		return p.classifyFind(args, depth)
	case prog == "sed":
		for _, a := range args {
			if contains(p.SedInplaceFlags, a) || strings.HasPrefix(a, "-i") {
				return RiskWrite
			}
		}
		return RiskRead
	case prog == "chmod":
		for _, a := range args {
			if contains(p.ChmodDestructiveFlags, a) || (strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "R")) {
				return RiskDestructive
			}
		}
		return RiskWrite
	}
	if spec, ok := p.Subcommands[prog]; ok {
		return classifySubcommand(spec, args)
	}
	switch {
	case contains(p.TestPrograms, prog):
		return RiskTest
	case contains(p.WritePrograms, prog):
		return RiskWrite
	case contains(p.ReadPrograms, prog):
		return RiskRead
	}
	return RiskWrite
}

// unwrapLauncher returns the command a launcher (env, nohup, timeout,
// xargs, ...) runs.
func unwrapLauncher(prog string, spec launcherSpec, args []string) []string {
	positional := spec.SkipPositional
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case strings.HasPrefix(a, "-"):
			if contains(spec.SkipFlagsWithValue, a) {
				i += 2
			} else {
				i++
			}
		case prog == "env" && shellAssignment.MatchString(a):
			i++
		case positional > 0:
			positional--
			i++
		default:
			return args[i:]
		}
	}
	return nil
}

func classifySubcommand(spec subcommandSpec, args []string) string {
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") {
		if contains(spec.GlobalFlagsWithValue, args[i]) {
			i += 2
		} else {
			i++
		}
	}
	for _, a := range args {
		if contains(spec.DestructiveFlags, a) {
			return RiskDestructive
		}
	}
	if i >= len(args) {
		return RiskRead
	}
	sub, rest := strings.ToLower(args[i]), args[i+1:]
	if nested, ok := spec.Nested[sub]; ok {
		return classifySubcommand(nested, rest)
	}
	switch {
	case contains(spec.Destructive, sub):
		return RiskDestructive
	case contains(spec.Test, sub):
		return RiskTest
	case contains(spec.Read, sub):
		return RiskRead
	}
	return RiskWrite
}

func (p riskPolicy) classifyHTTP(args []string) string {
	grade := RiskRead
	for i, a := range args {
		method := ""
		for _, f := range p.HTTPMethodFlags {
			switch {
			case a == f && i+1 < len(args):
				method = args[i+1]
			case strings.HasPrefix(f, "--") && strings.HasPrefix(a, f+"="):
				method = strings.TrimPrefix(a, f+"=")
			case !strings.HasPrefix(f, "--") && strings.HasPrefix(a, f) && len(a) > len(f):
				method = a[len(f):]
			}
		}
		method = strings.ToLower(method)
		if contains(p.HTTPMutatingMethods, method) {
			return RiskDestructive
		}
		name := a
		if eq := strings.IndexByte(a, '='); eq > 0 && strings.HasPrefix(a, "--") {
			name = a[:eq]
		}
		if contains(p.HTTPDataFlags, name) {
			return RiskDestructive
		}
		if contains(p.HTTPOutputFlags, name) {
			grade = RiskWrite
		}
	}
	return grade
}

func (p riskPolicy) classifyFind(args []string, depth int) string {
	grade := RiskRead
	for i := 0; i < len(args); i++ {
		if contains(p.FindDestructiveFlags, args[i]) {
			return RiskDestructive
		}
		if contains(p.FindExecFlags, args[i]) {
			j := i + 1
			for j < len(args) && args[j] != ";" && args[j] != "+" {
				j++
			}
			grade = maxRisk(grade, p.classifyWords(args[i+1:j], depth+1))
			i = j
		}
	}
	return grade
}

// shellSegment is one simple command: its words, quotes removed, and the
// targets of its output redirections.
type shellSegment struct {
	words     []string
	redirects []string
}

// lexShell splits a command line into simple commands at unquoted ; | &
// newlines, parentheses, command substitution and backquotes, so a command
// hidden in a subshell or substitution is a segment of its own. Quotes
// group words and are removed; a backslash escapes the next character.
func lexShell(s string) []shellSegment {
	const (
		unquoted = iota
		single
		double
	)
	type frame struct {
		backquote bool
		quote     int
	}
	var (
		segs       []shellSegment
		cur        shellSegment
		word       strings.Builder
		inWord     bool
		quote      = unquoted
		stack      []frame
		pendingOut bool
		pendingIn  bool
	)
	endWord := func() {
		if !inWord {
			return
		}
		w := word.String()
		word.Reset()
		inWord = false
		switch {
		case pendingOut:
			cur.redirects = append(cur.redirects, w)
			pendingOut = false
		case pendingIn:
			pendingIn = false
		default:
			cur.words = append(cur.words, w)
		}
	}
	endSeg := func() {
		endWord()
		pendingOut, pendingIn = false, false
		if len(cur.words) > 0 || len(cur.redirects) > 0 {
			segs = append(segs, cur)
		}
		cur = shellSegment{}
	}
	add := func(c byte) {
		word.WriteByte(c)
		inWord = true
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote == single {
			if c == '\'' {
				quote = unquoted
			} else {
				add(c)
			}
			continue
		}
		if c == '\\' && i+1 < len(s) {
			add(s[i+1])
			i++
			continue
		}
		if c == '$' && i+1 < len(s) && s[i+1] == '(' {
			endSeg()
			stack = append(stack, frame{quote: quote})
			quote = unquoted
			i++
			continue
		}
		if c == '`' {
			endSeg()
			if n := len(stack); n > 0 && stack[n-1].backquote {
				quote = stack[n-1].quote
				stack = stack[:n-1]
			} else {
				stack = append(stack, frame{backquote: true, quote: quote})
				quote = unquoted
			}
			continue
		}
		if quote == double {
			if c == '"' {
				quote = unquoted
			} else {
				add(c)
			}
			continue
		}
		switch c {
		case '\'':
			quote = single
			inWord = true
		case '"':
			quote = double
			inWord = true
		case ' ', '\t', '\r':
			endWord()
		case ';', '\n', '|', '&', '(':
			endSeg()
		case ')':
			endSeg()
			if n := len(stack); n > 0 && !stack[n-1].backquote {
				quote = stack[n-1].quote
				stack = stack[:n-1]
			}
		case '>':
			// A file-descriptor number before > is not a word.
			if inWord && isDigits(word.String()) {
				word.Reset()
				inWord = false
			}
			endWord()
			for i+1 < len(s) && (s[i+1] == '>' || s[i+1] == '|') {
				i++
			}
			if i+1 < len(s) && s[i+1] == '&' {
				// >&2: a duplicated descriptor, not a file.
				i++
				for i+1 < len(s) && s[i+1] != ' ' && s[i+1] != ';' && s[i+1] != '|' && s[i+1] != '\n' {
					i++
				}
				continue
			}
			pendingOut = true
		case '<':
			endWord()
			for i+1 < len(s) && s[i+1] == '<' {
				i++
			}
			pendingIn = true
		default:
			add(c)
		}
	}
	endSeg()
	return segs
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func maxRisk(a, b string) string {
	if riskRank[b] > riskRank[a] {
		return b
	}
	return a
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func hasAnyPrefix(v string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(v, p) {
			return true
		}
	}
	return false
}
