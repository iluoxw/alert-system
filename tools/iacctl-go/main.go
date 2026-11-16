// Command iacctl-go is the read-only observability CLI for the Golang
// auth-center on PRO: thanos-inner for the service's own metrics, OpenObserve
// for mysql gauges and the log/trace SQL search.
package main

import (
	"os"

	"github.com/luoxw/iacctl-go/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
