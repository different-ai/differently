// Command differently-guest runs inside each microVM and executes commands
// and file operations for the node agent. Inside Firecracker it listens on
// vsock port 10000; --tcp is for local testing only.
package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/different-ai/differently/internal/guest"
)

func main() {
	tcp := flag.String("tcp", "", "listen on a TCP address instead of vsock (testing only)")
	port := flag.Uint("vsock-port", 10000, "vsock port")
	cwd := flag.String("cwd", "/root", "default working directory")
	flag.Parse()
	var ln net.Listener
	var err error
	if *tcp != "" {
		ln, err = net.Listen("tcp", *tcp)
	} else {
		ln, err = listenVsock(uint32(*port))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "differently-guest:", err)
		os.Exit(1)
	}
	srv := &http.Server{Handler: guest.Handler(&guest.Runner{Root: "/", DefaultCwd: *cwd}), ReadHeaderTimeout: 10 * time.Second}
	if err := srv.Serve(ln); err != nil {
		fmt.Fprintln(os.Stderr, "differently-guest:", err)
		os.Exit(1)
	}
}
