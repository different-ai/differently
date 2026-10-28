//go:build !linux

package main

import (
	"errors"
	"net"
)

func listenVsock(uint32) (net.Listener, error) {
	return nil, errors.New("vsock is only available on Linux; use --tcp for local testing")
}
