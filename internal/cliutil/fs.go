// Package cliutil holds small helpers shared across the firefork
// binaries and library packages.
package cliutil

import "os"

// MkPrivateDir creates a directory at path with 0o700 perms. Use this
// for any directory that will hold a Firecracker API UDS, a vsock UDS,
// key material, or other per-process secret state — Go's typical 0o755
// default lets any local UID connect to the socket / read the secret.
func MkPrivateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}
