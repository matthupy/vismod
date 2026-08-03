// Package fetch downloads allow-listed remote media to local files.
//
// The destination is chosen by an untrusted job payload, so the address
// policy is enforced against the address actually dialed rather than the
// hostname parsed. DenyPrivate is the default for every host; the weaker
// DenyMetadata applies only to hosts the operator named in
// source.url.allow_private_hosts.
package fetch

import (
	"fmt"
	"net/netip"
)

// cgnat is RFC 6598 shared address space. netip has no predicate for it,
// and it routes to carrier infrastructure, so it is denied explicitly.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// imdsV6 is the IPv6 instance-metadata address (AWS fd00:ec2::254). It
// sits inside the IPv6 ULA range, which DenyMetadata otherwise permits,
// so it is denied by its own prefix.
var imdsV6 = netip.MustParsePrefix("fd00:ec2::/32")

// DenyPrivate is the default address policy for media-source URLs.
//
// It is deliberately a deny-list of everything non-public rather than an
// allow-list of public ranges: a new special-use range added by IANA
// should fail closed here only if it is also non-routable, and the
// predicates below track the stdlib's view of that.
//
// Addr is unmapped first so a v4-mapped v6 address (::ffff:127.0.0.1)
// cannot smuggle a private v4 past the v4 predicates.
func DenyPrivate(ip netip.Addr) error {
	if !ip.IsValid() {
		return fmt.Errorf("fetch: invalid address")
	}
	ip = ip.Unmap()
	switch {
	case ip.IsLoopback():
		return fmt.Errorf("fetch: %s is loopback", ip)
	case ip.IsPrivate():
		// Covers RFC 1918 and IPv6 ULA (fc00::/7).
		return fmt.Errorf("fetch: %s is a private address", ip)
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		// 169.254.0.0/16 and fe80::/10 — the cloud metadata range.
		return fmt.Errorf("fetch: %s is link-local (cloud metadata range)", ip)
	case ip.IsUnspecified():
		return fmt.Errorf("fetch: %s is the unspecified address", ip)
	case ip.IsMulticast(), ip.IsInterfaceLocalMulticast():
		return fmt.Errorf("fetch: %s is multicast", ip)
	case cgnat.Contains(ip):
		return fmt.Errorf("fetch: %s is CGNAT shared address space", ip)
	}
	return nil
}

// DenyMetadata is the address policy for hosts named in
// source.url.allow_private_hosts.
//
// Those hosts exist so an operator can scan media served from the box or
// LAN running vismod, so loopback, RFC 1918 and IPv6 ULA are permitted
// here — that is the whole point of the list, and naming a host on it is
// an explicit operator decision, never something a job can request.
//
// What stays denied is what is never a media server and is what an SSRF
// chain actually wants: the cloud instance-metadata endpoints (IPv4
// 169.254.169.254 via the link-local range, IPv6 fd00:ec2::254 via its
// own prefix, since ULA is otherwise allowed above), plus the
// unspecified and multicast addresses. CGNAT is denied too: it routes to
// carrier infrastructure, not to anything the operator runs.
func DenyMetadata(ip netip.Addr) error {
	if !ip.IsValid() {
		return fmt.Errorf("fetch: invalid address")
	}
	ip = ip.Unmap()
	switch {
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		return fmt.Errorf("fetch: %s is link-local (cloud metadata range)", ip)
	case imdsV6.Contains(ip):
		return fmt.Errorf("fetch: %s is the IPv6 instance-metadata range", ip)
	case ip.IsUnspecified():
		return fmt.Errorf("fetch: %s is the unspecified address", ip)
	case ip.IsMulticast(), ip.IsInterfaceLocalMulticast():
		return fmt.Errorf("fetch: %s is multicast", ip)
	case cgnat.Contains(ip):
		return fmt.Errorf("fetch: %s is CGNAT shared address space", ip)
	}
	return nil
}
