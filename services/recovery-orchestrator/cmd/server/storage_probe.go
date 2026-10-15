package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// probeWritablePath proves path is a directory that accepts a durable write
// (create, write, fsync, remove) and returns the bytes available to this
// process on its filesystem.
func probeWritablePath(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return 0, fmt.Errorf("not a directory")
	}
	f, err := os.CreateTemp(path, ".scos-recovery-probe-*")
	if err != nil {
		return 0, err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err := f.Write([]byte("scos recovery storage probe\n")); err != nil {
		f.Close()
		return 0, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(filepath.Clean(path), &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
