package blob

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"
)

// AllowPrivateStorage lets a deployment accept player storage on private, loopback or
// link-local addresses — a MinIO on the same LAN, say. It is off by default: the URL a player
// types is a request OpenPak makes from inside its own network, and the interesting targets
// there (169.254.169.254, a database, an admin port) are exactly the ones nobody outside
// should be able to reach through us.
var AllowPrivateStorage = false

// notStorage are ranges net.IP does not call private but that are not where anyone's storage
// lives either.
var notStorage = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"), // carrier NAT
	netip.MustParsePrefix("192.0.0.0/24"),  // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"), // benchmarking
}

// checkHost refuses a player-configured storage host that does not resolve somewhere public.
// The addresses are judged, not the string, so a name pointed at the metadata service is
// refused the same as the literal.
//
// ponytail: resolved when the backend is opened, so a name that changes after that is not
// caught mid-request; re-checking every connection needs a dialer control hook, which is the
// upgrade if it ever matters.
func checkHost(host string) error {
	if AllowPrivateStorage {
		return nil
	}
	if host == "" {
		return errors.New("storage has no host")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("cannot resolve %s: %w", host, err)
	}
	for _, ip := range ips {
		ip = ip.Unmap()
		bad := !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()
		for _, p := range notStorage {
			bad = bad || p.Contains(ip)
		}
		if bad {
			return fmt.Errorf("%s resolves to %s, which is not on the public internet: OpenPak will not fetch from its own network", host, ip)
		}
	}
	return nil
}

// checkEndpoint takes the host[:port] form the S3 settings carry.
func checkEndpoint(endpoint string) error {
	host := endpoint
	if h, _, err := net.SplitHostPort(endpoint); err == nil {
		host = h
	}
	return checkHost(host)
}
