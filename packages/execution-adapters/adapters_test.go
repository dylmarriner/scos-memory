package executionadapters

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRewriteCommandBlocksDestructive(t *testing.T) {
	blocked := []string{
		"rm -rf /",
		"rm -rf /*",
		"rm -fr ~/",
		`rm -r -f "$HOME"`,
		"rm --recursive --force /usr",
		"rm -rf --no-preserve-root /tmp/x",
		"sudo rm -rf /etc",
		"FOO=1 nohup rm -Rf /var",
		"cd /tmp && rm -rf /",
		"true; rm -rf /boot",
		`sh -c "rm -rf /"`,
		`bash -lc 'echo hi; rm -rf /home'`,
		`eval "rm -rf /"`,
		"echo $(rm -rf /)",
		"dd if=/dev/zero of=/dev/sda bs=1M",
		"mkfs.ext4 /dev/nvme0n1p1",
		"echo x > /dev/sdb",
		"cat img >/dev/mmcblk0",
		"chmod -R 777 /",
		"shred /dev/sda",
		":(){ :|:& };:",
		"timeout 5s sudo -n rm -rf /",
	}
	a := &DefaultAdapter{}
	for _, cmd := range blocked {
		if _, err := a.RewriteCommand(cmd); !errors.Is(err, ErrCommandBlocked) {
			t.Errorf("%q: expected ErrCommandBlocked, got %v", cmd, err)
		}
	}
}

func TestRewriteCommandAllowsSafe(t *testing.T) {
	safe := []string{
		"echo hello",
		"rm -rf ./build /tmp/scos-cache",
		"rm /etc/app/stale.lock",
		"ls -la / 2>&1 | head",
		"go test ./... > /dev/null 2>&1",
		`grep -r "rm -rf /" docs/`,
		"chmod -R 755 ./dist",
		"dd if=/dev/zero of=./disk.img bs=1M count=10",
		"git log --oneline && git status",
	}
	a := &DefaultAdapter{}
	for _, cmd := range safe {
		rw, err := a.RewriteCommand(cmd)
		if err != nil {
			t.Errorf("%q: unexpected block: %v", cmd, err)
			continue
		}
		if rw.RewrittenCommand != cmd || rw.IsRewritten {
			t.Errorf("%q: safe command was altered: %+v", cmd, rw)
		}
	}
}

func TestRewriteCommandRejectsUnparseable(t *testing.T) {
	if _, err := (&DefaultAdapter{}).RewriteCommand(`echo "unterminated`); !errors.Is(err, ErrCommandBlocked) {
		t.Fatalf("expected unparseable command to be blocked, got %v", err)
	}
}

func TestReduceOutput(t *testing.T) {
	a := &DefaultAdapter{}
	if got := a.ReduceOutput("short", 100); got != "short" {
		t.Fatalf("short output altered: %q", got)
	}
	raw := strings.Repeat("é", 50) // 2-byte runes
	reduced := a.ReduceOutput(raw, 21)
	if !strings.Contains(reduced, "output reduced") || !utf8.ValidString(reduced) {
		t.Fatalf("reduction broke UTF-8 or lost marker: %q", reduced)
	}
	if !strings.HasPrefix(reduced, "ééééé\n") || !strings.HasSuffix(reduced, "\nééééé") {
		t.Fatalf("head/tail not preserved: %q", reduced)
	}
}
