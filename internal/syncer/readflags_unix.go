//go:build unix

package syncer

import "syscall"

// Nonblocking and no-follow also protect against replacements after Lstat.
const readFileFlags = syscall.O_RDONLY | syscall.O_NONBLOCK | syscall.O_NOFOLLOW
