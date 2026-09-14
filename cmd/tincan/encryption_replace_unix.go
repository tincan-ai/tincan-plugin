//go:build !windows

package main

import (
	"os"
	"path/filepath"
)

func replacePrivate(from, to string) error {
	if err := os.Rename(from, to); err != nil {
		return err
	}
	return syncPrivateDirectory(to)
}

func syncPrivateDirectory(to string) error {
	dir, err := os.Open(filepath.Dir(to))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
