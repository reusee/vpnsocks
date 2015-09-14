//+build ignore

package main

import (
	"crypto/rand"
	"fmt"
	"io"
	"log"
	"os"
)

func main() {
	if len(os.Args) <= 1 {
		fmt.Printf("usage: %s [key file path]\n", os.Args[0])
		return
	}
	file, err := os.Create(os.Args[1])
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()
	n, err := io.CopyN(file, rand.Reader, 65536)
	if err != nil {
		log.Fatal(err)
	}
	if n != 65536 {
		log.Fatal(fmt.Errorf("invalid read length"))
	}
}
