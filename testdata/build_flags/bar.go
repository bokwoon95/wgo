//go:build bar
// +build bar

package main

func init() {
	words = append(words, "bar")
}
