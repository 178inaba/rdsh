// Command rdsh runs ad-hoc SQL and manages saved queries on Redash.
package main

import (
	"os"

	"github.com/178inaba/rdsh/internal/cmd"
)

func main() {
	os.Exit(cmd.Execute())
}
