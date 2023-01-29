package main

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	temp := os.Args
	os.Args = []string{
		"wgo", "-exit", "echo", "foo",
		"::", "wgo", "-exit", "echo", "bar",
		"::", "wgo", "-exit", "echo", "baz",
	}
	main()
	os.Args = temp
	os.Exit(m.Run())
}
