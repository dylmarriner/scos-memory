package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeBWS replaces bws with this test binary (TestBWSHelperProcess), which
// answers `secret get` with value when given token.
func fakeBWS(t *testing.T, token, secretID, value string) *[]string {
	t.Helper()
	var calls []string
	prevCmd, prevLook := commandContext, lookPath
	commandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		calls = append(calls, name+" "+strings.Join(args, " "))
		cmd := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=TestBWSHelperProcess", "--"}, args...)...)
		cmd.Env = append(os.Environ(), "SCOS_BWS_HELPER_PROCESS=1", "FAKE_BWS_TOKEN="+token, "FAKE_BWS_ID="+secretID, "FAKE_BWS_VALUE="+value)
		return cmd
	}
	lookPath = func(name string) (string, error) { return "/usr/local/bin/" + name, nil }
	t.Cleanup(func() { commandContext, lookPath = prevCmd, prevLook })
	return &calls
}

func TestBWSHelperProcess(t *testing.T) {
	if os.Getenv("SCOS_BWS_HELPER_PROCESS") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	args = args[1:]
	if os.Getenv("BWS_ACCESS_TOKEN") != os.Getenv("FAKE_BWS_TOKEN") {
		fmt.Fprintln(os.Stderr, "Error: Access token is not in a valid format")
		os.Exit(1)
	}
	if len(args) < 3 || args[0] != "secret" || args[1] != "get" || args[2] != os.Getenv("FAKE_BWS_ID") {
		fmt.Fprintln(os.Stderr, "Error: Resource not found")
		os.Exit(1)
	}
	out, _ := json.Marshal(map[string]string{"id": args[2], "organizationId": "org", "key": "SCOS_BACKUP_ENCRYPTION_KEY",
		"value": os.Getenv("FAKE_BWS_VALUE"), "revisionDate": "2026-10-17T00:00:00Z"})
	fmt.Println(string(out))
	os.Exit(0)
}

func TestSecretRecoveryDrillRecoversTheKeyAndOpensABackup(t *testing.T) {
	ctx, now := context.Background(), time.Now()
	s, _, _ := objectStoreTestServer(t, "Enabled")
	key := strings.Repeat("k", 32)
	const secretID = "be8e0ad8-d545-4017-a55a-b02f014d4158"

	if c := s.secretRecoveryCoverage(now); c.Covered || !strings.Contains(c.Reason, "no secret manager") {
		t.Fatalf("unconfigured = %+v", c)
	}
	if s.secretDrillDue(now) {
		t.Fatal("an unconfigured drill is never due")
	}
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("0.machine.token:secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCOS_BACKUP_KEY_SECRET_ID", secretID)
	t.Setenv("SCOS_BWS_ACCESS_TOKEN_FILE", tokenFile)
	if !s.secretDrillDue(now) {
		t.Fatal("a configured drill that never ran is due")
	}
	if c := s.secretRecoveryCoverage(now); c.Covered || !strings.Contains(c.Reason, "no secret-recovery drill") {
		t.Fatalf("never drilled = %+v", c)
	}

	// No backup yet: the key is recovered but nothing proves it opens one.
	fakeBWS(t, "0.machine.token:secret", secretID, key)
	if d, err := s.secretRecoveryDrill(ctx); err != nil || d.Status != "failed" || !strings.Contains(d.Detail, "no encrypted backup") {
		t.Fatalf("no backup = %+v, %v", d, err)
	}

	fakePostgresTools(t)
	b := takeBackup(t, s)
	calls := fakeBWS(t, "0.machine.token:secret", secretID, key)
	d, err := s.secretRecoveryDrill(ctx)
	if err != nil || d.Status != "verified" || d.BackupID != b.ID || !strings.Contains(d.Detail, "decrypted backup "+b.ID) {
		t.Fatalf("drill = %+v, %v", d, err)
	}
	if len(*calls) != 1 || strings.Contains((*calls)[0], "token") {
		t.Fatalf("bws must get the token from its environment, not argv: %v", *calls)
	}
	if s.secretDrillDue(now) {
		t.Fatal("a fresh drill satisfies the interval")
	}
	c := s.secretRecoveryCoverage(time.Now())
	if !c.Covered || !strings.Contains(c.Evidence, secretID) {
		t.Fatalf("coverage = %+v", c)
	}

	// A secret holding another key fails, and withdraws coverage.
	fakeBWS(t, "0.machine.token:secret", secretID, strings.Repeat("x", 32))
	if d, _ := s.secretRecoveryDrill(ctx); d.Status != "failed" || !strings.Contains(d.Detail, "different key") {
		t.Fatalf("wrong key = %+v", d)
	}
	if c := s.secretRecoveryCoverage(time.Now()); c.Covered {
		t.Fatalf("coverage after a failed drill = %+v", c)
	}

	// A token Bitwarden refuses fails with bws's own message.
	fakeBWS(t, "another-token", secretID, key)
	if d, _ := s.secretRecoveryDrill(ctx); d.Status != "failed" || !strings.Contains(d.Detail, "Access token") {
		t.Fatalf("bad token = %+v", d)
	}

	// Coverage follows the configured secret, and goes stale.
	fakeBWS(t, "0.machine.token:secret", secretID, key)
	if d, _ := s.secretRecoveryDrill(ctx); d.Status != "verified" {
		t.Fatalf("re-drill = %+v", d)
	}
	if c := s.secretRecoveryCoverage(time.Now().Add(s.config.coverageMaxAge + time.Hour)); c.Covered || !strings.Contains(c.Reason, "freshness") {
		t.Fatalf("stale = %+v", c)
	}
	t.Setenv("SCOS_BACKUP_KEY_SECRET_ID", "another-secret")
	if c := s.secretRecoveryCoverage(time.Now()); c.Covered || !strings.Contains(c.Reason, "not the configured") {
		t.Fatalf("other secret = %+v", c)
	}
}
