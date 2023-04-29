//go:build windows
// +build windows

package main

import (
	"os/exec"
	"strconv"
	"strings"
)

// stop stops the command and all its child processes.
func stop(cmd *exec.Cmd) {
	// https://stackoverflow.com/a/44551450
	killCmd := exec.Command("taskkill.exe", "/t", "/f", "/pid", strconv.Itoa(cmd.Process.Pid))
	_ = killCmd.Run()
}

// setpgid is a no-op on windows.
func setpgid(cmd *exec.Cmd) {}

// joinArgs joins the arguments of the command into a string which can then be
// passed to `exec.Command("pwsh.exe", "-command", $STRING)`. Examples:
//
// ["echo", "foo"] => echo foo
//
// ["echo", "hello goodbye"] => echo 'hello goodbye'
func joinArgs(args []string) string {
	// references:
	// https://www.rlmueller.net/PowerShellEscape.htm
	// https://stackoverflow.com/a/11231504
	var b strings.Builder
	for i, arg := range args {
		if i == 0 {
			b.WriteString(arg)
			continue
		}
		b.WriteString(" ")
		if arg == "" {
			b.WriteString("''")
			continue
		}
		if !strings.ContainsAny(arg, " '`$(){}<>|&;*") {
			b.WriteString(arg)
			continue
		}
		b.WriteString("'" + strings.ReplaceAll(arg, "'", "''") + "'")
	}
	return b.String()
}
