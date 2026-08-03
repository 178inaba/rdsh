package main

import (
	"os"

	"github.com/178inaba/rdsh/internal/cmd"
)

func main() {
	os.Exit(cmd.Execute())
}
