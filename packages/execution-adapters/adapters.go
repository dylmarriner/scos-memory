package executionadapters

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// CommandRewrite represents a command rewritten for safety or performance (§20.3).
type CommandRewrite struct {
	OriginalCommand  string `json:"original_command"`
	RewrittenCommand string `json:"rewritten_command"`
	IsRewritten      bool   `json:"is_rewritten"`
	Reason           string `json:"reason,omitempty"`
}

// ExecutionResult represents captured output and exit status (§20.4).
type ExecutionResult struct {
	ExitCode      int           `json:"exit_code"`
	RawOutput     string        `json:"raw_output"`
	ReducedOutput string        `json:"reduced_output"`
	ExecutionTime time.Duration `json:"execution_time"`
	Truncated     bool          `json:"truncated"`
}

// ExecutionAdapter defines command rewriting and result reduction interface.
type ExecutionAdapter interface {
	RewriteCommand(cmd string) (*CommandRewrite, error)
	ReduceOutput(output string, maxBytes int) string
}

// ErrCommandBlocked is wrapped by the error RewriteCommand returns for a
// command that must not run at all. Callers detect it with errors.Is.
var ErrCommandBlocked = errors.New("command blocked by execution safety policy")

// DefaultAdapter implements execution proxy command adapter.
type DefaultAdapter struct{}

// RewriteCommand parses cmd as a POSIX shell command line and refuses it
// when any simple command in it (including ones nested in sh -c / eval, or
// behind sudo/env/nohup-style wrappers) is destructive at the system level.
// Safe commands are returned unchanged.
func (d *DefaultAdapter) RewriteCommand(cmd string) (*CommandRewrite, error) {
	if reason := destructiveReason(cmd, 0); reason != "" {
		return nil, fmt.Errorf("%w: %s", ErrCommandBlocked, reason)
	}
	return &CommandRewrite{OriginalCommand: cmd, RewrittenCommand: cmd}, nil
}

const reductionMarker = "\n... [output reduced by execution-proxy] ...\n"

// ReduceOutput keeps the head and tail of output within maxBytes (§20.5),
// cutting only on UTF-8 rune boundaries.
func (d *DefaultAdapter) ReduceOutput(output string, maxBytes int) string {
	if maxBytes <= 0 || len(output) <= maxBytes {
		return output
	}
	half := maxBytes / 2
	head := half
	for head > 0 && !utf8.RuneStart(output[head]) {
		head--
	}
	tail := len(output) - half
	for tail < len(output) && !utf8.RuneStart(output[tail]) {
		tail++
	}
	return output[:head] + reductionMarker + output[tail:]
}

var (
	funcDef      = regexp.MustCompile(`([A-Za-z_:][\w:]*)\(\)\{([^}]*)\}`)
	duration     = regexp.MustCompile(`^\d+(\.\d+)?[smhd]?$`)
	blockDevice  = regexp.MustCompile(`^/dev/(sd[a-z]+|hd[a-z]+|vd[a-z]+|xvd[a-z]+|nvme\d+n\d+|mmcblk\d+|disk\d+|dm-\d+|md\d+)`)
	criticalDirs = map[string]bool{
		"/": true, "/bin": true, "/boot": true, "/dev": true, "/etc": true, "/home": true,
		"/lib": true, "/lib64": true, "/opt": true, "/proc": true, "/root": true, "/sbin": true,
		"/srv": true, "/sys": true, "/usr": true, "/var": true, "~": true, "$HOME": true, "${HOME}": true,
	}
	wrappers = map[string]bool{
		"sudo": true, "doas": true, "env": true, "nohup": true, "time": true, "command": true,
		"exec": true, "nice": true, "ionice": true, "busybox": true, "stdbuf": true, "timeout": true,
	}
)

const maxNesting = 4

func destructiveReason(cmdline string, depth int) string {
	if depth > maxNesting {
		return "command nesting too deep to verify"
	}
	if isForkBomb(cmdline) {
		return "fork bomb"
	}
	commands, err := splitCommands(cmdline)
	if err != nil {
		return "unparseable command: " + err.Error()
	}
	for _, words := range commands {
		if reason := commandReason(words, depth); reason != "" {
			return reason
		}
	}
	return ""
}

func commandReason(words []string, depth int) string {
	// Redirections anywhere in the command: writing to a raw disk.
	for i, w := range words {
		target := ""
		switch {
		case w == ">" || w == ">>" || w == "1>" || w == "2>":
			if i+1 < len(words) {
				target = words[i+1]
			}
		case strings.HasPrefix(w, ">"):
			target = strings.TrimLeft(w, ">")
		}
		if blockDevice.MatchString(target) {
			return "redirect onto block device " + target
		}
	}
	words = stripWrappers(words)
	if len(words) == 0 {
		return ""
	}
	name, args := path.Base(words[0]), words[1:]
	switch {
	case name == "sh" || name == "bash" || name == "zsh" || name == "dash" || name == "ksh":
		for i, a := range args {
			if (a == "-c" || (strings.HasPrefix(a, "-") && strings.Contains(a, "c") && !strings.HasPrefix(a, "--"))) && i+1 < len(args) {
				return destructiveReason(args[i+1], depth+1)
			}
		}
	case name == "eval":
		return destructiveReason(strings.Join(args, " "), depth+1)
	case name == "rm":
		recursive, targets := false, []string{}
		for _, a := range args {
			switch {
			case a == "--no-preserve-root":
				return "rm --no-preserve-root"
			case a == "--recursive":
				recursive = true
			case strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--"):
				if strings.ContainsAny(a, "rR") {
					recursive = true
				}
			case !strings.HasPrefix(a, "-"):
				targets = append(targets, a)
			}
		}
		if recursive {
			for _, t := range targets {
				if isCritical(t) {
					return "recursive delete of " + t
				}
			}
		}
	case name == "chmod" || name == "chown" || name == "chgrp":
		recursive := false
		var targets []string
		for _, a := range args {
			if a == "-R" || a == "--recursive" || (strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "R")) {
				recursive = true
			} else if !strings.HasPrefix(a, "-") {
				targets = append(targets, a)
			}
		}
		if recursive && len(targets) > 1 {
			for _, t := range targets[1:] {
				if isCritical(t) {
					return fmt.Sprintf("recursive %s of %s", name, t)
				}
			}
		}
	case name == "mkfs" || strings.HasPrefix(name, "mkfs.") || name == "mke2fs" || name == "mkswap" || name == "wipefs":
		return name + " formats a filesystem"
	case name == "dd":
		for _, a := range args {
			if strings.HasPrefix(a, "of=") && blockDevice.MatchString(strings.TrimPrefix(a, "of=")) {
				return "dd onto block device " + strings.TrimPrefix(a, "of=")
			}
		}
	case name == "shred":
		for _, a := range args {
			if blockDevice.MatchString(a) {
				return "shred of block device " + a
			}
		}
	}
	return ""
}

func isCritical(target string) bool {
	t := strings.TrimRight(target, "/")
	if t == "" {
		return true // "/" or "//"
	}
	t = strings.TrimSuffix(t, "/*")
	if t == "" || t == "*" {
		return true // "/*"
	}
	if criticalDirs[t] {
		return true
	}
	if strings.HasPrefix(t, "/") {
		return criticalDirs[path.Clean(t)]
	}
	return false
}

// stripWrappers removes leading VAR=value assignments and privilege/process
// wrappers (with their own flags) so the effective command is inspected.
func stripWrappers(words []string) []string {
	for len(words) > 0 {
		w := words[0]
		switch {
		case strings.Contains(w, "=") && !strings.HasPrefix(w, "=") && !strings.HasPrefix(w, "-"):
			words = words[1:]
		case wrappers[path.Base(w)]:
			words = words[1:]
			for len(words) > 0 && (strings.HasPrefix(words[0], "-") || (path.Base(w) == "timeout" && isDuration(words[0]))) {
				words = words[1:]
			}
		default:
			return words
		}
	}
	return words
}

func isDuration(s string) bool { return duration.MatchString(s) }

// isForkBomb detects a shell function that pipes into a backgrounded copy of
// itself, e.g. ":(){ :|:& };:", regardless of name or spacing.
func isForkBomb(cmdline string) bool {
	compact := strings.Join(strings.Fields(cmdline), "")
	for _, m := range funcDef.FindAllStringSubmatch(compact, -1) {
		if strings.Contains(m[2], m[1]+"|"+m[1]+"&") {
			return true
		}
	}
	return false
}

// splitCommands tokenises a shell command line into simple commands,
// honouring single/double quotes, backslash escapes and the ; & && || |
// and newline separators. Command substitutions $(...) and `...` are
// returned as additional commands so their contents are inspected too.
func splitCommands(s string) ([][]string, error) {
	var (
		commands [][]string
		words    []string
		cur      strings.Builder
		inWord   bool
	)
	endWord := func() {
		if inWord {
			words = append(words, cur.String())
			cur.Reset()
			inWord = false
		}
	}
	endCommand := func() {
		endWord()
		if len(words) > 0 {
			commands = append(commands, words)
			words = nil
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s):
			cur.WriteByte(s[i+1])
			inWord = true
			i++
		case c == '\'':
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				return nil, errors.New("unterminated single quote")
			}
			cur.WriteString(s[i+1 : i+1+end])
			inWord = true
			i += end + 1
		case c == '"':
			j := i + 1
			for ; j < len(s) && s[j] != '"'; j++ {
				if s[j] == '\\' && j+1 < len(s) {
					j++
				}
			}
			if j >= len(s) {
				return nil, errors.New("unterminated double quote")
			}
			inner := s[i+1 : j]
			cur.WriteString(inner)
			inWord = true
			if strings.Contains(inner, "$(") || strings.Contains(inner, "`") {
				nested, err := substitutions(inner)
				if err != nil {
					return nil, err
				}
				commands = append(commands, nested...)
			}
			i = j
		case c == '$' && i+1 < len(s) && s[i+1] == '(' || c == '`':
			nested, n, err := substitution(s[i:])
			if err != nil {
				return nil, err
			}
			commands = append(commands, nested...)
			cur.WriteString(s[i : i+n])
			inWord = true
			i += n - 1
		case c == ';' || c == '\n' || c == '&' || c == '|':
			endCommand()
		case c == ' ' || c == '\t' || c == '\r':
			endWord()
		case c == '>' || c == '<':
			// Split "cmd>file" so the redirect target is its own word.
			if inWord && !isFD(cur.String()) {
				endWord()
			}
			cur.WriteByte(c)
			inWord = true
			if i+1 < len(s) && s[i+1] == c {
				cur.WriteByte(c)
				i++
			}
			// ">&1" / ">&2" duplicate a descriptor; keep it in this word so
			// the '&' is not taken as a command separator.
			if i+1 < len(s) && s[i+1] == '&' {
				cur.WriteByte('&')
				i++
				for i+1 < len(s) && (s[i+1] >= '0' && s[i+1] <= '9' || s[i+1] == '-') {
					cur.WriteByte(s[i+1])
					i++
				}
			}
			endWord()
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	endCommand()
	return commands, nil
}

func isFD(s string) bool { return s == "1" || s == "2" || s == "&" }

// substitution parses the $(...) or `...` at the start of s, returning its
// inner commands and the number of bytes consumed.
func substitution(s string) ([][]string, int, error) {
	if s[0] == '`' {
		end := strings.IndexByte(s[1:], '`')
		if end < 0 {
			return nil, 0, errors.New("unterminated backtick substitution")
		}
		inner, err := splitCommands(s[1 : 1+end])
		return inner, end + 2, err
	}
	depth := 0
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				inner, err := splitCommands(s[2:i])
				return inner, i + 1, err
			}
		}
	}
	return nil, 0, errors.New("unterminated $( substitution")
}

func substitutions(s string) ([][]string, error) {
	var out [][]string
	for i := 0; i < len(s); i++ {
		if (s[i] == '$' && i+1 < len(s) && s[i+1] == '(') || s[i] == '`' {
			nested, n, err := substitution(s[i:])
			if err != nil {
				return nil, err
			}
			out = append(out, nested...)
			i += n - 1
		}
	}
	return out, nil
}
