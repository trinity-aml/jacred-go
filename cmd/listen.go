package main

import (
	"net"
	"strconv"
	"strings"
)

func listenAddress(host string, port int) string {
	host = strings.TrimSpace(host)
	if strings.EqualFold(host, "any") {
		host = ""
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}
