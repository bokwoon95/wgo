package main

import (
	"bytes"
	"io/fs"
	"log"
	"os"
	"path/filepath"
)

func main() {
	dir := "."
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		log.Fatal(err)
	}
	buf := &bytes.Buffer{}
	buf.WriteString("---")
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == dir {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		relpath, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		buf.WriteString("\n" + filepath.ToSlash(relpath))
		ext := filepath.Ext(path)
		if ext == ".txt" || ext == ".md" {
			buf.WriteString(":")
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if s := string(bytes.TrimSpace(b)); s != "" {
				buf.WriteString(" " + s)
			}
		}
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}
	buf.WriteString("\n")
	_, _ = buf.WriteTo(os.Stdout)
}
