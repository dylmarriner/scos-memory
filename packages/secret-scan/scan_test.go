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
