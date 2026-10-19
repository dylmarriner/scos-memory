// Package secretscan does write-time pattern matching for secret- and
// PII-shaped content (AWS keys, JWTs, PEM private key headers, SSNs,
// credit-card-shaped digit sequences, generic high-entropy tokens, and
// credentials written out in text: labelled passwords and keys, passwords in
// URLs, and user/password pairs). Pattern
// shapes are adapted from gitleaks' default gitleaks.toml (MIT-licensed) --
// gitleaks itself is a CLI tool, not embedded here, only its pattern shapes
// are borrowed.
package secretscan

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Kind names which pattern a Finding matched.
type Kind string

const (
	KindAWSAccessKey   Kind = "aws_access_key"
	KindJWT            Kind = "jwt"
	KindPrivateKeyPEM  Kind = "private_key_pem"
	KindSSN            Kind = "ssn"
	KindCreditCard     Kind = "credit_card"
	KindHighEntropy    Kind = "high_entropy_token"
	KindPassword       Kind = "password"
	KindURLCredential  Kind = "url_credential"
	KindCredentialPair Kind = "credential_pair"
)

// Finding is one pattern match within scanned content.
type Finding struct {
	Kind     Kind
	Span     [2]int // byte offsets [start, end) into the scanned content
	Redacted string // the matched substring, redacted for logging/display
}

type pattern struct {
	kind Kind
	re   *regexp.Regexp
	// group, when non-zero, is the submatch holding the secret itself: only
	// it is redacted, so "router admin/[REDACTED:credential_pair]" still
	// says what the memory is about.
	group int
}

var patterns = []pattern{
	{KindAWSAccessKey, regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`), 0},
	{KindJWT, regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`), 0},
	{KindPrivateKeyPEM, regexp.MustCompile(`-----BEGIN (RSA |EC |OPENSSH |DSA |)PRIVATE KEY-----`), 0},
	{KindSSN, regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`), 0},
	{KindCreditCard, regexp.MustCompile(`\b(?:\d[ -]?){13,16}\b`), 0},
	// Generic high-entropy token: a long run of base64/hex-alphabet
	// characters mixing case and digits, the shape gitleaks' generic-api-key
	// rule targets. Deliberately conservative (32+ chars, must mix letter
	// case and digits) to keep false positives on ordinary prose low.
	{KindHighEntropy, regexp.MustCompile(`\b(?:[A-Za-z0-9+/_-]{32,})\b`), 0},
	// A value given for a password-, secret-, key- or token-like label
	// ("password: hunter22", "api_key=...", "token is ..."), the same label
	// set security-gate classifies as secret.
	{KindPassword, regexp.MustCompile(`(?i)\b(?:password|passwd|pwd|passphrase|passcode|pin|secret|client[ _-]?secret|api[ _-]?key|access[ _-]?key|secret[ _-]?key|private[ _-]?key|token|access[ _-]?token|auth[ _-]?token|bearer)\b\s*(?:is|:|=|=>)\s*["']?([^\s"',;]{4,})`), 1},
	// The password in a URL's userinfo: scheme://user:password@host.
	{KindURLCredential, regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^/\s:@]+:([^/\s@]+)@`), 1},
	// An account name and its password written together, "admin/s3cret" or
	// "root:s3cret". The account must be a login name, not a path segment,
	// and the password must mix letters with digits or symbols, so paths
	// and repository names ("user/scos-memory") are left alone.
	{KindCredentialPair, regexp.MustCompile(`(?i)(?:^|[\s(\[,;'"])(?:admin|administrator|root|user|username|login|pi|ubuntu|sa|postgres)\s*[/:]\s*([^\s/,;'")\]]{4,})`), 1},
}

// Scan returns every pattern match in content. Findings are ordered by
// start offset. A high-entropy match that overlaps an already-classified
// finding (e.g. the base64 body of a matched JWT) is suppressed -- it is
// redundant, not a second problem.
func Scan(content string) []Finding {
	var findings []Finding
	var claimed [][2]int
	markers := redactionMarker.FindAllStringIndex(content, -1)
	for _, p := range patterns {
		for _, l := range p.re.FindAllStringSubmatchIndex(content, -1) {
			loc := [2]int{l[0], l[1]}
			if p.group > 0 {
				if l[2*p.group] < 0 {
					continue
				}
				loc = [2]int{l[2*p.group], l[2*p.group+1]}
			}
			if onlyRedacted(content, loc, markers) {
				continue
			}
			// A labelled value or account pair is a credential only if it is
			// shaped like one: "token is required" names no secret.
			if (p.kind == KindCredentialPair || p.kind == KindPassword) && !credentialShaped(content[loc[0]:loc[1]]) {
				continue
			}
			if p.group > 0 && overlapsAny(loc, claimed) {
				continue
			}
			if p.kind == KindHighEntropy && overlapsAny(loc, claimed) {
				continue
			}
			if p.kind == KindHighEntropy && !mixedCaseAndDigits(content[loc[0]:loc[1]]) {
				continue
			}
			findings = append(findings, Finding{
				Kind:     p.kind,
				Span:     loc,
				Redacted: redact(content[loc[0]:loc[1]]),
			})
			claimed = append(claimed, loc)
		}
	}
	// Redact walks findings in order and skips a span that starts before
	// the previous one ends, so they must be in content order: in pattern
	// order, a secret a later pattern found earlier in the text was left in.
	sort.SliceStable(findings, func(i, j int) bool { return findings[i].Span[0] < findings[j].Span[0] })
	return findings
}

// redactionMarker is what Redact puts in place of a secret.
var redactionMarker = regexp.MustCompile(`\[REDACTED:[a-z_]+\]`)

// onlyRedacted reports whether content[span] is redaction-marker text: it
// overlaps a marker and every letter and digit in it lies inside one. Text
// the scanner already redacted is not a new secret, so redacting redacted
// text changes nothing; a span that touches no marker, even one made only of
// punctuation, is still a finding.
func onlyRedacted(content string, span [2]int, markers [][]int) bool {
	inMarker := func(at int) bool {
		for _, m := range markers {
			if at >= m[0] && at < m[1] {
				return true
			}
		}
		return false
	}
	touches := false
	for i, r := range content[span[0]:span[1]] {
		at := span[0] + i
		if inMarker(at) {
			touches = true
			continue
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return false
		}
	}
	return touches
}

// credentialShaped reports whether s mixes letters with digits or symbols,
// as a password does and a plain word or name does not.
func credentialShaped(s string) bool {
	var letter, other bool
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			letter = true
		default:
			other = true
		}
	}
	return letter && other
}

// Redact replaces every finding's span in content with its Kind, e.g.
// "[REDACTED:aws_access_key]". Findings must be spans into content (as
// returned by Scan(content)); applying findings from a different string is
// undefined.
func Redact(content string, findings []Finding) string {
	if len(findings) == 0 {
		return content
	}
	var b strings.Builder
	last := 0
	for _, f := range findings {
		if f.Span[0] < last {
			continue // overlapping/out-of-order span, skip rather than corrupt output
		}
		b.WriteString(content[last:f.Span[0]])
		b.WriteString("[REDACTED:")
		b.WriteString(string(f.Kind))
		b.WriteString("]")
		last = f.Span[1]
	}
	b.WriteString(content[last:])
	return b.String()
}

func overlapsAny(span [2]int, spans [][2]int) bool {
	for _, s := range spans {
		if span[0] < s[1] && s[0] < span[1] {
			return true
		}
	}
	return false
}

func mixedCaseAndDigits(s string) bool {
	var hasUpper, hasLower, hasDigit bool
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			hasUpper = true
		case r >= 'a' && r <= 'z':
			hasLower = true
		case r >= '0' && r <= '9':
			hasDigit = true
		}
	}
	return hasUpper && hasLower && hasDigit
}

func redact(s string) string {
	if len(s) <= 8 {
		return strings.Repeat("*", len(s))
	}
	return s[:4] + strings.Repeat("*", len(s)-8) + s[len(s)-4:]
}
