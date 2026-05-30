//go:build unix

// Pulling syscall.SIGTERM through a tiny shim keeps supervisor.go portable
// to platforms where syscall.SIGTERM doesn't exist (windows). weft-agent
// targets darwin + linux so this is realistically the only file, but
// keeping the shim makes accidental cross-builds fail at link time only,
// not in arbitrary parts of the code.

package proxy

import "syscall"

var syscallSIGTERM = syscall.SIGTERM
