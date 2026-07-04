//go:build !windows

package update

import (
	"os"
	"syscall"
)

func LaunchReplacement(executablePath string, args []string) error {
	execArgs := append([]string{executablePath}, args...)
	return syscall.Exec(executablePath, execArgs, os.Environ())
}
