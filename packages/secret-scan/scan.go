// Package secretscan does write-time pattern matching for secret- and
// PII-shaped content (AWS keys, JWTs, PEM private key headers, SSNs,
// credit-card-shaped digit sequences, generic high-entropy tokens). Pattern
// shapes are adapted from gitleaks' default gitleaks.toml (MIT-licensed) --
// gitleaks itself is a CLI tool, not embedded here, only its pattern shapes
// are borrowed.
package secretscan

import (
	"regexp"
	"strings"
)

// Kind names which pattern a Finding matched.
type Kind string

const (
	KindAWSAccessKey  Kind = "aws_access_key"
	KindJWT           Kind = "jwt"
	KindPrivateKeyPEM Kind = "private_key_pem"
	KindSSN           Kind = "ssn"
	KindCreditCard    Kind = "credit_card"
	KindHighEntropy   Kind = "high_entropy_token"
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
}

var patterns = []pattern{
	{KindAWSAccessKey, regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{KindJWT, regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`)},
	{KindPrivateKeyPEM, regexp.MustCompile(`-----BEGIN (RSA |EC |OPENSSH |DSA |)PRIVATE KEY-----`)},
	{KindSSN, regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)},
	{KindCreditCard, regexp.MustCompile(`\b(?:\d[ -]?){13,16}\b`)},
	// Generic high-entropy token: a long run of base64/hex-alphabet
	// characters mixing case and digits, the shape gitleaks' generic-api-key
	// rule targets. Deliberately conservative (32+ chars, must mix letter
	// case and digits) to keep false positives on ordinary prose low.
	{KindHighEntropy, regexp.MustCompile(`\b(?:[A-Za-z0-9+/_-]{32,})\b`)},
}

// Scan returns every pattern match in content. Findings are ordered by
// start offset. A high-entropy match that overlaps an already-classified
// finding (e.g. the base64 body of a matched JWT) is suppressed -- it is
// redundant, not a second problem.
func Scan(content string) []Finding {
	var findings []Finding
	var claimed [][2]int
	for _, p := range patterns {
		for _, l := range p.re.FindAllStringIndex(content, -1) {
			loc := [2]int{l[0], l[1]}
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
	return findings
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
