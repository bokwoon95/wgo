package main

import (
	"fmt"
	"os"
)

func main() {
	for _, key := range []string{"FOO", "BAR", "WGO_RANDOM_NUMBER"} {
		fmt.Println(key + "=" + os.Getenv(key))
	}
}
