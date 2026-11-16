// Package endpoint checks the base URLs kiseki sends a bearer token to.
package endpoint

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Check accepts https URLs, and http URLs only on a loopback host, so that a
// token is never sent in clear text across a network.
func Check(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%q: %w", raw, err)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("%q: no host", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if loopback(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("%q: http is allowed only for loopback hosts; use https", raw)
	}
	return fmt.Errorf("%q: scheme must be https, or http for a loopback host", raw)
}

func loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
