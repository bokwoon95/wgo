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
	_, currfile, _, ok = runtime.Caller(0)
)

func main() {
	if !ok {
		log.Fatal("couldn't get currfile")
	}
	currdir := filepath.Dir(currfile) + string(filepath.Separator)
	buf := &bytes.Buffer{}
	buf.WriteString("---")
	_ = filepath.WalkDir(currdir, func(path string, d fs.DirEntry, err error) error {
		if path == currdir {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		buf.WriteString("\n" + filepath.ToSlash(strings.TrimPrefix(path, currdir)))
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
