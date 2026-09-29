// rconcli runs commands on a Minecraft server with the bot's RCON client.
// Usage: go run ./tools/rconcli <addr> <password> <command>...
package main

import (
	"fmt"
	"os"

	"github.com/kronwerke/bot/internal/rcon"
)

func main() {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: rconcli <addr> <password> <command>...")
		os.Exit(2)
	}
	c := &rcon.Client{Addr: os.Args[1], Password: os.Args[2]}
	for _, cmd := range os.Args[3:] {
		out, err := c.Command(cmd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "> %s\nerror: %v\n", cmd, err)
			os.Exit(1)
		}
		fmt.Printf("> %s\n%s\n", cmd, out)
	}
}
