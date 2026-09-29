import unittest

from scos_execution.reducer import CommandBlockedError, OutputReducer

# Same cases as packages/execution-adapters/adapters_test.go.
BLOCKED = [
    "rm -rf /", "rm -rf /*", "rm -fr ~/", 'rm -r -f "$HOME"', "rm --recursive --force /usr",
    "rm -rf --no-preserve-root /tmp/x", "sudo rm -rf /etc", "FOO=1 nohup rm -Rf /var",
    "cd /tmp && rm -rf /", "true; rm -rf /boot", 'sh -c "rm -rf /"',
    "bash -lc 'echo hi; rm -rf /home'", 'eval "rm -rf /"', "echo $(rm -rf /)",
    "dd if=/dev/zero of=/dev/sda bs=1M", "mkfs.ext4 /dev/nvme0n1p1", "echo x > /dev/sdb",
    "cat img >/dev/mmcblk0", "chmod -R 777 /", "shred /dev/sda", ":(){ :|:& };:",
    "timeout 5s sudo -n rm -rf /", 'echo "unterminated',
]
SAFE = [
    "echo hello", "rm -rf ./build /tmp/scos-cache", "rm /etc/app/stale.lock",
    "ls -la / 2>&1 | head", "go test ./... > /dev/null 2>&1", 'grep -r "rm -rf /" docs/',
    "chmod -R 755 ./dist", "dd if=/dev/zero of=./disk.img bs=1M count=10",
    "git log --oneline && git status",
]


class OutputReducerTests(unittest.TestCase):
    def test_blocks_destructive(self):
        for cmd in BLOCKED:
            with self.subTest(cmd=cmd), self.assertRaises(CommandBlockedError):
                OutputReducer().rewrite_command(cmd)

    def test_allows_safe_unchanged(self):
        for cmd in SAFE:
            with self.subTest(cmd=cmd):
                rw = OutputReducer().rewrite_command(cmd)
                self.assertEqual((rw.rewritten_command, rw.is_rewritten), (cmd, False))

    def test_reduce_output(self):
        red = OutputReducer()
        self.assertEqual(red.reduce_output("short", 100), "short")
        out = red.reduce_output("é" * 50, 21)
        self.assertIn("reduced", out)
        self.assertTrue(out.startswith("ééééé\n") and out.endswith("\nééééé"))


if __name__ == "__main__":
    unittest.main()
