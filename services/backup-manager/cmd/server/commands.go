package main

import (
	"context"
	"fmt"
	"os"
	"strings"
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
//	backup-manager object-sync
//	    copies every backup the provider holds that the object store does
//	    not, verifying each copy on read-back.
//	backup-manager object-fetch <backup-id>
//	    verifies a backup's object-store copy and writes it back to the
//	    provider, for when the provider's own copy is lost or damaged.
//	backup-manager secret-drill
//	    recovers the backup key from Bitwarden (Secrets Manager or a vault
//	    item) and decrypts the newest encrypted backup with it, recording
//	    the result.
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
	case "object-sync":
		if len(args) != 1 {
			fmt.Fprintln(os.Stderr, "usage: backup-manager object-sync")
			return 2
		}
		s := newBackupServer()
		res, err := s.syncObjectStore(context.Background())
		if err != nil {
			fmt.Fprintln(os.Stderr, "object-sync:", err)
			return 1
		}
		fmt.Printf("copied %d (%s)\n", len(res.Copied), strings.Join(res.Copied, " "))
		for _, e := range res.Errors {
			fmt.Fprintln(os.Stderr, "object-sync:", e)
		}
		if len(res.Errors) > 0 {
			return 1
		}
		return 0
	case "object-fetch":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: backup-manager object-fetch <backup-id>")
			return 2
		}
		c, err := newBackupServer().restoreFromObjectStore(context.Background(), args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "object-fetch:", err)
			return 1
		}
		fmt.Printf("restored %s (%s, version %s) from the object store to the provider\n", args[1], c.Manifest.Type, c.ArchiveVersion)
		return 0
	case "secret-drill":
		if len(args) != 1 {
			fmt.Fprintln(os.Stderr, "usage: backup-manager secret-drill")
			return 2
		}
		d, err := newBackupServer().secretRecoveryDrill(context.Background())
		if err != nil {
			fmt.Fprintln(os.Stderr, "secret-drill:", err)
			return 1
		}
		fmt.Printf("%s: %s\n", d.Status, d.Detail)
		if d.Status != "verified" {
			return 1
		}
		return 0
	}
	fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
	return 2
}
