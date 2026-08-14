//go:build !windows

package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"unicode/utf8"
	"unsafe"
)

// NOTE: We shouldn't encounter the macOS file limit of 256 anymore now that
// importing "os" automatically raises it for us in an init function.
// https://github.com/golang/go/issues/46279
// https://go-review.googlesource.com/c/go/+/393354/4/src/os/rlimit.go

const (
	specialChars      = "\\'\"`${[|&;<>()*?!"
	extraSpecialChars = " \t\n"
	prefixChars       = "~"
)

// stop stops the command and all its child processes.
func stop(cmd *exec.Cmd) {
	// https://stackoverflow.com/questions/22470193/why-wont-go-kill-a-child-process-correctly
	// https://medium.com/@felixge/killing-a-child-process-and-all-of-its-children-in-go-54079af94773
	pgid := -cmd.Process.Pid
	_ = syscall.Kill(pgid, syscall.SIGTERM)
}

// https://stackoverflow.com/questions/22470193/why-wont-go-kill-a-child-process-correctly
// https://medium.com/@felixge/killing-a-child-process-and-all-of-its-children-in-go-54079af94773
func setpgid(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
}

// flushStdin tells the OS to flush the text currently buffered in stdin.
//
// Using raw numbers here instead of importing package constants to avoid
// creating a bunch more files to use conditional compilation to select the
// right package constants for the os and arch.
func flushStdin(r io.Reader) {
	f, ok := r.(*os.File)
	if !ok {
		return
	}
	switch runtime.GOOS {
	case "linux":
		// Get syscall.SYS_IOCTL value based on runtime.GOARCH (for
		// runtime.GOOS=linux).
		var SYS_IOCTL uintptr
		switch runtime.GOARCH {
		case "amd64":
			SYS_IOCTL = 16
		case "arm64", "loong64", "riscv64":
			SYS_IOCTL = 29
		case "mips", "mipsle":
			SYS_IOCTL = 4054
		case "mips64", "mips64le":
			SYS_IOCTL = 5015
		default:
			SYS_IOCTL = 54
		}

		// Get golang.org/x/sys/unix.TCFLSH value based on runtime.GOARCH (for
		// runtime.GOOS=linux).
		var TCFLSH uintptr
		switch runtime.GOARCH {
		case "mips", "mips64", "mips64le", "mipsle":
			TCFLSH = 0x5407
		case "ppc", "ppc64", "ppc64le":
			TCFLSH = 0x2000741f
		case "sparc64":
			TCFLSH = 0x20005407
		default:
			TCFLSH = 0x540b
		}

		// golang.org/x/sys/unix.TCIFLUSH value for runtime.GOOS=linux.
		var TCIFLUSH uintptr = 0

		// ioctl(fd, TCFLSH, TCIFLUSH)
		syscall.Syscall(SYS_IOCTL, f.Fd(), TCFLSH, TCIFLUSH)
	case "darwin", "dragonfly", "freebsd", "netbsd", "openbsd":
		// syscall.SYS_IOCTL value for runtime.GOOS IN (darwin, dragonfly,
		// freebsd, netbsd, openbsd).
		var SYS_IOCTL uintptr = 54

		// golang.org/x/sys/unix.TIOCFLUSH value for runtime.GOOS IN (darwin,
		// dragonfly, freebsd, netbsd, openbsd).
		var TIOCFLUSH uintptr = 0x80047410

		// golang.org/x/sys/unix.TCIFLUSH value for runtime.GOOS IN (darwin,
		// dragonfly, freebsd, netbsd, openbsd).
		var TCIFLUSH = 1
		flags := int32(TCIFLUSH)

		// ioctl(fd, TIOCFLUSH, &flags)
		syscall.Syscall(SYS_IOCTL, f.Fd(), TIOCFLUSH, uintptr(unsafe.Pointer(&flags)))
	}
}

// joinArgs joins the arguments of the command into a string which can then be
// passed to `exec.Command("sh", "-c", $STRING)`. Examples:
//
// ["echo", "foo"] => echo foo
//
// ["echo", "hello goodbye"] => echo 'hello goodbye'
func joinArgs(args []string) string {
	// https://github.com/kballard/go-shellquote/blob/master/quote.go
	//
	// Copyright (C) 2014 Kevin Ballard
	//
	// Permission is hereby granted, free of charge, to any person obtaining
	// a copy of this software and associated documentation files (the "Software"),
	// to deal in the Software without restriction, including without limitation
	// the rights to use, copy, modify, merge, publish, distribute, sublicense,
	// and/or sell copies of the Software, and to permit persons to whom the
	// Software is furnished to do so, subject to the following conditions:
	//
	// The above copyright notice and this permission notice shall be included
	// in all copies or substantial portions of the Software.
	//
	// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND,
	// EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES
	// OF MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT.
	// IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM,
	// DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION OF CONTRACT,
	// TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE
	// OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.
	var buf bytes.Buffer
	for i, arg := range args {
		if i != 0 {
			buf.WriteByte(' ')
		}
		quote(arg, &buf)
	}
	return buf.String()
}

func quote(word string, buf *bytes.Buffer) {
	// https://github.com/kballard/go-shellquote/blob/master/quote.go
	//
	// Copyright (C) 2014 Kevin Ballard
	//
	// Permission is hereby granted, free of charge, to any person obtaining
	// a copy of this software and associated documentation files (the "Software"),
	// to deal in the Software without restriction, including without limitation
	// the rights to use, copy, modify, merge, publish, distribute, sublicense,
	// and/or sell copies of the Software, and to permit persons to whom the
	// Software is furnished to do so, subject to the following conditions:
	//
	// The above copyright notice and this permission notice shall be included
	// in all copies or substantial portions of the Software.
	//
	// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND,
	// EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES
	// OF MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT.
	// IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM,
	// DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION OF CONTRACT,
	// TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE
	// OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.

	// We want to try to produce a "nice" output. As such, we will
	// backslash-escape most characters, but if we encounter a space, or if we
	// encounter an extra-special char (which doesn't work with
	// backslash-escaping) we switch over to quoting the whole word. We do this
	// with a space because it's typically easier for people to read multi-word
	// arguments when quoted with a space rather than with ugly backslashes
	// everywhere.
	origLen := buf.Len()

	if len(word) == 0 {
		// oops, no content
		buf.WriteString("''")
		return
	}

	cur, prev := word, word
	atStart := true
	for len(cur) > 0 {
		c, l := utf8.DecodeRuneInString(cur)
		cur = cur[l:]
		if strings.ContainsRune(specialChars, c) || (atStart && strings.ContainsRune(prefixChars, c)) {
			// copy the non-special chars up to this point
			if len(cur) < len(prev) {
				buf.WriteString(prev[0 : len(prev)-len(cur)-l])
			}
			buf.WriteByte('\\')
			buf.WriteRune(c)
			prev = cur
		} else if strings.ContainsRune(extraSpecialChars, c) {
			// start over in quote mode
			buf.Truncate(origLen)
			goto quote
		}
		atStart = false
	}
	if len(prev) > 0 {
		buf.WriteString(prev)
	}
	return

quote:
	// quote mode
	// Use single-quotes, but if we find a single-quote in the word, we need
	// to terminate the string, emit an escaped quote, and start the string up
	// again
	inQuote := false
	for len(word) > 0 {
		i := strings.IndexRune(word, '\'')
		if i == -1 {
			break
		}
		if i > 0 {
			if !inQuote {
				buf.WriteByte('\'')
				inQuote = true
			}
			buf.WriteString(word[0:i])
		}
		word = word[i+1:]
		if inQuote {
			buf.WriteByte('\'')
			inQuote = false
		}
		buf.WriteString("\\'")
	}
	if len(word) > 0 {
		if !inQuote {
			buf.WriteByte('\'')
		}
		buf.WriteString(word)
		buf.WriteByte('\'')
	}
}
