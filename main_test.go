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
	stdout := os.Stdout
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		panic(err)
	}
	os.Stdout = devNull
	main()
	os.Stdout = stdout
	_ = devNull.Close()
	os.Args = temp
	os.Exit(m.Run())
}
