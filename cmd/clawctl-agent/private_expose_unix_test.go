//go:build unix

package main

import "os"

func exposePrivateCredential(path string) error {
	return os.Chmod(path, 0o644)
}
