// Command zerock (client build) shares a local port on a subdomain of a zerock
// server. It is the same CLI as the full binary minus the commands that run the
// server, so it carries none of the ACME, DNS-provider or database code.
package main

import (
	"os"

	"github.com/erickdsama/zerock/internal/clientcli"
)

func main() {
	os.Exit(clientcli.Main(os.Args[1:]))
}
