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
	}
	fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
	return 2
}
