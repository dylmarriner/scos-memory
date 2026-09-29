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

func TestRedactCoversSecretsFoundOutOfOrder(t *testing.T) {
	in := "ssn 123-45-6789 key AKIAABCDEFGHIJKLMNOP"
	got := Redact(in, Scan(in))
	want := "ssn [REDACTED:ssn] key [REDACTED:aws_access_key]"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	// Findings in any order, with overlaps, must still redact every byte.
	unordered := []Finding{{Kind: KindAWSAccessKey, Span: [2]int{20, 40}}, {Kind: KindSSN, Span: [2]int{4, 15}}, {Kind: KindHighEntropy, Span: [2]int{22, 30}}}
	if got := Redact(in, unordered); got != want {
		t.Fatalf("unordered: got %q want %q", got, want)
	}
}

func TestScanOrdersByOffset(t *testing.T) {
	f := Scan("123-45-6789 then AKIAABCDEFGHIJKLMNOP then 987-65-4321")
	for i := 1; i < len(f); i++ {
		if f[i].Span[0] < f[i-1].Span[0] {
			t.Fatalf("findings not ordered by offset: %+v", f)
		}
	}
}

func TestKeywordAnchoredHexSecret(t *testing.T) {
	in := `config api_key="3f9a1c0b7d2e4f6a8b9c0d1e2f3a4b5c6d7e8f90" and commit 3f9a1c0b7d2e4f6a8b9c0d1e2f3a4b5c6d7e8f90`
	got := Redact(in, Scan(in))
	want := `config api_key="[REDACTED:generic_secret]" and commit 3f9a1c0b7d2e4f6a8b9c0d1e2f3a4b5c6d7e8f90`
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	for _, s := range []string{"Authorization: Bearer 0123456789abcdef0123", "password: hunter2hunter2hunter2"} {
		if f := Scan(s); len(f) != 1 || f[0].Kind != KindGenericSecret {
			t.Errorf("%q: expected one generic_secret finding, got %+v", s, f)
		}
	}
	jwt := "token: eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"
	if f := Scan(jwt); len(f) != 1 || f[0].Kind != KindJWT {
		t.Errorf("JWT behind a keyword must be one jwt finding, got %+v", f)
	}
}
