// Command zerock shares a local port on a subdomain of your own domain, and is
// also the server that makes that possible.
package main

import (
	"os"

	"github.com/erickdsama/zerock/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
