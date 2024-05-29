package main

import (
	"bytes"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

var (
	_, currentFile, _, ok = runtime.Caller(0)
)

func main() {
	if !ok {
		log.Fatal("couldn't get currentFile")
	}
	currentDir := filepath.Dir(currentFile) + string(os.PathSeparator)
	buf := &bytes.Buffer{}
	buf.WriteString("---")
	_ = filepath.WalkDir(currentDir, func(path string, d fs.DirEntry, err error) error {
		if path == currentDir {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		buf.WriteString("\n" + filepath.ToSlash(strings.TrimPrefix(path, currentDir)))
		if strings.HasSuffix(path, ".txt") {
			buf.WriteString(":")
			b, err := os.ReadFile(path)
			if err != nil {
				log.Fatal(err)
			}
			if s := string(bytes.TrimSpace(b)); s != "" {
				buf.WriteString(" " + s)
			}
		}
		return nil
	})
	buf.WriteString("\n")
	buf.WriteTo(os.Stdout)
}
