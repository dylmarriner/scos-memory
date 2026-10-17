package main

import (
	"context"
	"fmt"
	"os"
)

// runCommand runs backup-manager as a one-shot command instead of the
// service, for operators inside the container:
//
//	backup-manager restore-config <backup-id> <target-dir>
//	    writes a configuration backup's files under target-dir, each at its
//	    original absolute path below it, after checking every digest.
//	backup-manager wal-fetch <wal-file> <destination>
//	    decrypts a shipped WAL file to destination: the restore_command of
//	    point-in-time recovery from the encrypted off-host copies.
func runCommand(args []string) int {
	switch args[0] {
	case "restore-config":
		if len(args) != 3 {
			fmt.Fprintln(os.Stderr, "usage: backup-manager restore-config <backup-id> <target-dir>")
			return 2
		}
		s := newBackupServer()
		n, err := s.restoreConfig(context.Background(), args[1], args[2])
		if err != nil {
			fmt.Fprintln(os.Stderr, "restore-config:", err)
			return 1
		}
		fmt.Printf("restored %d files from %s under %s\n", n, args[1], args[2])
		return 0
	case "wal-fetch":
		if len(args) != 3 {
			fmt.Fprintln(os.Stderr, "usage: backup-manager wal-fetch <wal-file> <destination>")
			return 2
		}
		if err := newBackupServer().fetchShippedWAL(args[1], args[2]); err != nil {
			fmt.Fprintln(os.Stderr, "wal-fetch:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
	return 2
}
