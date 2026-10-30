// Command chromesmoke is a throwaway probe: it dials the live chrome-host
// control socket and issues one browser command, printing the JSON reply.
// Usage: go run ./cmd/chromesmoke [method] [paramsJSON]
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/umairge66/gocode/internal/chromehost"
)

func main() {
	method := "tabs_context"
	if len(os.Args) > 1 {
		method = os.Args[1]
	}
	var params any
	if len(os.Args) > 2 {
		if err := json.Unmarshal([]byte(os.Args[2]), &params); err != nil {
			fmt.Fprintf(os.Stderr, "bad params JSON: %v\n", err)
			os.Exit(2)
		}
	}
	cfg := chromehost.DefaultConfigDir(os.Getenv, os.UserHomeDir)
	client := chromehost.NewControlClient(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	res, err := client.Call(ctx, method, params)
	if err != nil {
		fmt.Fprintf(os.Stderr, "CALL FAILED: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("%s -> %s\n", method, string(res))
}
