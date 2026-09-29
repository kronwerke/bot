// linkcli stands in for the bot on a test machine: it takes a launcher's link, accepts
// any key, and runs a list of steps against it.
//
// Usage: go run ./tools/linkcli -addr 127.0.0.1:13031 step...
//
//	wait <state>          until the launcher reports that state (running, stopped, ...)
//	<op> [json args]      one request, like: command '{"cmd":"list"}'
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/kronwerke/bot/internal/link"
)

type anyKey struct {
	mu sync.Mutex
	m  map[string]string
}

func (a *anyKey) LinkKey(fp string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	h, ok := a.m[fp]
	return h, ok
}

func main() {
	addr := flag.String("addr", "127.0.0.1:13031", "listen address")
	timeout := flag.Duration("timeout", 10*time.Minute, "per step")
	flag.Parse()

	keys := &anyKey{m: map[string]string{}}
	states := make(chan string, 64)
	connected := make(chan struct{}, 8)
	h := &link.Hub{Keys: keys}
	h.Hooks = link.Hooks{
		Pending: func(i link.Info) {
			fmt.Printf("pending %s from %s, accepting\n", i.Fingerprint, i.Remote)
			hash, _, ok := h.Accept(i.Fingerprint)
			if ok {
				keys.mu.Lock()
				keys.m[i.Fingerprint] = hash
				keys.mu.Unlock()
			}
		},
		Connected: func(i link.Info) {
			fmt.Printf("connected %s launcher %s pack %s state %s\n", i.Name, i.Launcher, i.Pack, i.State)
			states <- i.State
			connected <- struct{}{}
		},
		Disconnected: func(i link.Info, err error) { fmt.Printf("disconnected: %v\n", err) },
		State: func(i link.Info, s, d string) {
			fmt.Printf("state %s %s\n", s, d)
			states <- s
		},
	}
	mux := http.NewServeMux()
	mux.Handle("GET /link", h)
	go func() {
		if err := http.ListenAndServe(*addr, mux); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}()

	select {
	case <-connected:
	case <-time.After(*timeout):
		fmt.Fprintln(os.Stderr, "no launcher connected")
		os.Exit(1)
	}
	for _, step := range flag.Args() {
		op, args, _ := strings.Cut(step, " ")
		if op == "wait" {
			fmt.Printf("> wait %s\n", args)
			deadline := time.After(*timeout)
		loop:
			for {
				select {
				case s := <-states:
					if s == args {
						break loop
					}
				case <-deadline:
					fmt.Fprintln(os.Stderr, "timed out")
					os.Exit(1)
				}
			}
			continue
		}
		var a any
		if args != "" {
			if err := json.Unmarshal([]byte(args), &a); err != nil {
				fmt.Fprintln(os.Stderr, "bad args:", err)
				os.Exit(2)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		raw, err := h.Request(ctx, op, a)
		cancel()
		if err != nil {
			fmt.Printf("> %s\nerror: %v\n", step, err)
			continue
		}
		out := string(raw)
		if len(out) > 1500 {
			out = out[:1500] + "..."
		}
		fmt.Printf("> %s\n%s\n", step, out)
	}
}
