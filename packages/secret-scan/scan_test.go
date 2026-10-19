package secretscan

import "testing"

func TestScan(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    Kind // "" means expect no findings
	}{
		{"aws key", "key is AKIAABCDEFGHIJKLMNOP here", KindAWSAccessKey},
		{"jwt", "token: eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c", KindJWT},
		{"pem", "-----BEGIN RSA PRIVATE KEY-----\nMIIB...", KindPrivateKeyPEM},
		{"ssn", "ssn 123-45-6789 on file", KindSSN},
		{"credit card", "card 4111 1111 1111 1111 expires", KindCreditCard},
		{"clean prose", "the deploy finished at 10am and looked fine", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			findings := Scan(c.content)
			if c.want == "" {
				if len(findings) != 0 {
					t.Fatalf("expected no findings, got %+v", findings)
				}
				return
			}
			var found bool
			for _, f := range findings {
				if f.Kind == c.want {
					found = true
				}
			}
			if !found {
				t.Fatalf("expected a %s finding, got %+v", c.want, findings)
			}
		})
	}
}

func TestRedact(t *testing.T) {
	content := "key is AKIAABCDEFGHIJKLMNOP here"
	findings := Scan(content)
	redacted := Redact(content, findings)
	if redacted == content {
		t.Fatal("redact did not change content")
	}
	if got := Scan(redacted); len(got) != 0 {
		t.Fatalf("redacted content still matches a pattern: %+v", got)
	}
}

// Credentials written out in text are found, and only the secret itself is
// redacted.
func TestCredentialsInText(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Arcadyan router (192.168.1.254): admin/Zaleap1per@2008 (the @ matters).", "Arcadyan router (192.168.1.254): admin/[REDACTED:credential_pair] (the @ matters)."},
		{"db login root:Tr0ub4dor&3 on the box", "db login root:[REDACTED:credential_pair] on the box"},
		{"password: hunter22 for the wifi", "password: [REDACTED:password] for the wifi"},
		{"door passcode = blue-42 until Friday", "door passcode = [REDACTED:password] until Friday"},
		{"pull from https://dylan:p4ss-w0rd@git.example.com/repo.git", "pull from https://dylan:[REDACTED:url_credential]@git.example.com/repo.git"},
	}
	for _, c := range cases {
		if got := Redact(c.in, Scan(c.in)); got != c.want {
			t.Errorf("Redact(%q)\n got  %q\n want %q", c.in, got, c.want)
		}
		// Redacted text is clean: the scanner does not take its own
		// markers for secrets, so redacting twice changes nothing.
		if f := Scan(c.want); len(f) != 0 {
			t.Errorf("Scan(%q) = %+v, want no findings in redacted text", c.want, f)
		}
	}
	for _, clean := range []string{
		"the token is required before deploy",
		"clone dylmarriner/scos-memory and read /root/.hermes/MEMORY.md",
		"password: required",
		"user/repo layout follows GitHub",
		"meeting at 10:30 with the admin team",
	} {
		if f := Scan(clean); len(f) != 0 {
			t.Errorf("Scan(%q) = %+v, want no findings", clean, f)
		}
	}
}

// Findings come back in content order, so Redact replaces every one: a
// secret found by a later pattern earlier in the text is not skipped.
func TestFindingsAreInContentOrder(t *testing.T) {
	in := "password: s3cr3t-value then ssn 123-45-6789"
	f := Scan(in)
	for i := 1; i < len(f); i++ {
		if f[i].Span[0] < f[i-1].Span[0] {
			t.Fatalf("findings out of order: %+v", f)
		}
	}
	if got := Redact(in, f); got != "password: [REDACTED:password] then ssn [REDACTED:ssn]" {
		t.Fatalf("Redact = %q", got)
	}
}
