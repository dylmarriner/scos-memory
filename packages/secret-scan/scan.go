// Package secretscan does write-time pattern matching for secret- and
// PII-shaped content (AWS keys, JWTs, PEM private key headers, SSNs,
// credit-card-shaped digit sequences, generic high-entropy tokens). Pattern
// shapes are adapted from gitleaks' default gitleaks.toml (MIT-licensed) --
// gitleaks itself is a CLI tool, not embedded here, only its pattern shapes
// are borrowed.
package secretscan

import (
	"regexp"
	"sort"
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
	KindGenericSecret Kind = "generic_secret"
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
	// group, when non-zero, selects the capture group that is the secret
	// itself (the keyword before it is context, not sensitive).
	group int
}

var patterns = []pattern{
	{kind: KindAWSAccessKey, re: regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{kind: KindJWT, re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`)},
	{kind: KindPrivateKeyPEM, re: regexp.MustCompile(`-----BEGIN (RSA |EC |OPENSSH |DSA |)PRIVATE KEY-----`)},
	{kind: KindSSN, re: regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)},
	{kind: KindCreditCard, re: regexp.MustCompile(`\b(?:\d[ -]?){13,16}\b`)},
	// Keyword-anchored secret (gitleaks' generic-api-key shape): a value
	// assigned to a credential-named key, e.g. api_key="3f9a...". This is
	// what catches single-case hex keys; bare hex runs are left alone
	// because git SHAs and content hashes are ordinary memory content.
	{kind: KindGenericSecret, group: 1, re: regexp.MustCompile(`(?i)\b(?:api[_-]?key|access[_-]?key|secret(?:[_-]?key)?|client[_-]?secret|auth[_-]?token|access[_-]?token|token|passw(?:or)?d|pwd|bearer)\b["']?\s*(?:[:=]|\s)\s*["']?([A-Za-z0-9+/_.=~-]{16,})`)},
	// Generic high-entropy token: a long run of base64/hex-alphabet
	// characters mixing case and digits, the shape gitleaks' generic-api-key
	// rule targets. Deliberately conservative (32+ chars, must mix letter
	// case and digits) to keep false positives on ordinary prose low.
	{kind: KindHighEntropy, re: regexp.MustCompile(`\b(?:[A-Za-z0-9+/_-]{32,})\b`)},
}

// Scan returns every pattern match in content. Findings are ordered by
// start offset. A high-entropy match that overlaps an already-classified
// finding (e.g. the base64 body of a matched JWT) is suppressed -- it is
// redundant, not a second problem.
func Scan(content string) []Finding {
	var findings []Finding
	var claimed [][2]int
	for _, p := range patterns {
		for _, l := range p.re.FindAllStringSubmatchIndex(content, -1) {
			loc := [2]int{l[2*p.group], l[2*p.group+1]}
			if loc[0] < 0 {
				continue
			}
			if (p.kind == KindHighEntropy || p.kind == KindGenericSecret) && overlapsAny(loc, claimed) {
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
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].Span[0] != findings[j].Span[0] {
			return findings[i].Span[0] < findings[j].Span[0]
		}
		return findings[i].Span[1] > findings[j].Span[1] // longer span first
	})
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
	sorted := append([]Finding(nil), findings...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Span[0] < sorted[j].Span[0] })
	var b strings.Builder
	last := 0
	for i := 0; i < len(sorted); {
		// Merge every span overlapping this one into a single redaction so
		// no secret byte survives, whatever order the findings came in.
		start, end, kind := sorted[i].Span[0], sorted[i].Span[1], sorted[i].Kind
		for i++; i < len(sorted) && sorted[i].Span[0] < end; i++ {
			end = max(end, sorted[i].Span[1])
		}
		if start < last { // fully nested in an already-redacted span
			start = last
		}
		if end <= start {
			continue
		}
		b.WriteString(content[last:start])
		b.WriteString("[REDACTED:")
		b.WriteString(string(kind))
		b.WriteString("]")
		last = end
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
