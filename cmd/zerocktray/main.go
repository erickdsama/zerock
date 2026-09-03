// Command zerock-tray is the menu bar / system tray widget for zerock. It is a
// client: it opens tunnels with the same agent as "zerock http" and talks to the
// server's API, and carries none of the server code.
package main

import (
	"os"

	"github.com/erickdsama/zerock/internal/tray"
)

func main() {
	os.Exit(tray.Main(os.Args[1:]))
}
