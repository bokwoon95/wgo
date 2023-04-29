//go:build windows
// +build windows

package main

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

func Test_stop(t *testing.T) {
	t.Parallel()

	// Ensure no notepad.exe is running.
	_ = exec.Command("taskkill.exe", "/f", "/im", "notepad.exe").Run()

	// Start notepad.exe as a child process, then kill the parent process with
	// Process.Kill().
	cmd := exec.Command("cmd.exe", "/c", "notepad.exe")
	err := cmd.Start()
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond) // Give time for it to start before killing.
	_ = cmd.Process.Kill()
	_ = cmd.Wait()

	// Assert that Process.Kill() did not kill the child notepad.exe process by
	// checking that it exists with tasklist.
	b, err := exec.Command("tasklist.exe", "/nh", "/fi", "imagename eq notepad.exe").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	output := string(b)
	if !strings.Contains(output, "notepad.exe") {
		t.Fatalf("notepad.exe was successfully killed by Process.Kill(): %s", output)
	}

	// Ensure no notepad.exe is running.
	_ = exec.Command("taskkill.exe", "/f", "/im", "notepad.exe").Run()

	// Start notepad.exe as a child process again, then kill the parent process
	// with kill().
	cmd = exec.Command("cmd.exe", "/c", "notepad.exe")
	err = cmd.Start()
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond) // Give time for it to start before killing.
	stop(cmd)

	// Assert that kill() killed the child notepad.exe process.
	b, err = exec.Command("tasklist.exe", "/nh", "/fi", "imagename eq notepad.exe").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	output = string(b)
	if strings.Contains(output, "notepad.exe") {
		t.Fatalf("notepad.exe was not successfully killed by cleanup(): %s", output)
	}
}

func Test_joinArgs(t *testing.T) {
	type TestTable struct {
		description string
		args        []string
		want        string
	}

	tests := []TestTable{{
		description: "bare string",
		args:        []string{"echo", "test"},
		want:        "echo test",
	}, {
		description: "contains spaces",
		args:        []string{"echo", "hello goodbye"},
		want:        "echo 'hello goodbye'",
	}, {
		description: "simple args",
		args:        []string{"echo", "hello", "goodbye"},
		want:        "echo hello goodbye",
	}, {
		description: "single quote",
		args:        []string{"echo", "don't you know the dewey decimal system?"},
		want:        "echo 'don''t you know the dewey decimal system?'",
	}, {
		description: "args with single quote",
		args:        []string{"echo", "don't", "you", "know", "the", "dewey", "decimal", "system?"},
		want:        "echo 'don''t' you know the dewey decimal system?",
	}, {
		description: "tilde bang",
		args:        []string{"echo", "~user", "u~ser", " ~user", "!~user"},
		want:        "echo ~user u~ser ' ~user' !~user",
	}, {
		description: "glob brackets",
		args:        []string{"echo", "foo*", "M{ovies,usic}", "ab[cd]", "%3"},
		want:        "echo 'foo*' 'M{ovies,usic}' ab[cd] %3",
	}, {
		description: "empty string",
		args:        []string{"echo", "one", "", "three"},
		want:        "echo one '' three",
	}, {
		description: "parens",
		args:        []string{"echo", "some(parentheses)"},
		want:        "echo 'some(parentheses)'",
	}, {
		description: "special chars",
		args:        []string{"echo", "$some_ot~her_)spe!cial_*_characters"},
		want:        "echo '$some_ot~her_)spe!cial_*_characters'",
	}, {
		description: "quote space",
		args:        []string{"echo", "' "},
		want:        "echo ''' '",
	}}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.description, func(t *testing.T) {
			t.Parallel()
			got := joinArgs(tt.args)
			if got != tt.want {
				t.Errorf("\ngot:  %q\nwant: %q", got, tt.want)
			}
		})
	}
}
