//go:build unix

package main

import "golang.org/x/sys/unix"

func pathWritable(path string) error {
	return unix.Access(path, unix.W_OK)
}
