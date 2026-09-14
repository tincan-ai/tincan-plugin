package main

import "golang.org/x/sys/windows"

// Windows uses write-through replacement for the next canonical state write.
// Directory handles do not support the Unix directory fsync operation.
func syncPrivateDirectory(string) error { return nil }

func replacePrivate(from, to string) error {
	old, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	next, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(old, next, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
