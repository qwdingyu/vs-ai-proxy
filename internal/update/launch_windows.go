//go:build windows

package update

import (
	"os"
	"os/exec"
)

func LaunchReplacement(executablePath string, args []string) error {
	cmd := exec.Command(executablePath, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = os.Environ()
	return cmd.Start()
}
