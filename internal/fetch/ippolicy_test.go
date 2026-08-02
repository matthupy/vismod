package fetch

import (
	"net/netip"
	"testing"
)

func TestDenyPrivate(t *testing.T) {
	for _, tc := range []struct {
		addr    string
		allowed bool
		why     string
	}{
		// Allowed: ordinary public addresses.
		{"8.8.8.8", true, "public v4"},
		{"1.1.1.1", true, "public v4"},
		{"93.184.216.34", true, "public v4"},
		{"2606:4700:4700::1111", true, "public v6"},

		// Loopback.
		{"127.0.0.1", false, "v4 loopback"},
		{"127.1.2.3", false, "v4 loopback range"},
		{"::1", false, "v6 loopback"},

		// RFC 1918.
		{"10.0.0.1", false, "rfc1918 10/8"},
		{"172.16.0.1", false, "rfc1918 172.16/12"},
		{"172.31.255.254", false, "rfc1918 172.16/12 upper"},
		{"192.168.1.1", false, "rfc1918 192.168/16"},

		// Cloud metadata — the range where a miss becomes credential theft.
		{"169.254.169.254", false, "aws/gcp metadata"},
		{"169.254.0.1", false, "v4 link-local"},
		{"fe80::1", false, "v6 link-local"},

		// IPv6 ULA.
		{"fc00::1", false, "v6 unique local"},
		{"fd12:3456::1", false, "v6 unique local"},

		// Unspecified, multicast, CGNAT.
		{"0.0.0.0", false, "unspecified v4"},
		{"::", false, "unspecified v6"},
		{"224.0.0.1", false, "v4 multicast"},
		{"ff02::1", false, "v6 multicast"},
		{"100.64.0.1", false, "cgnat 100.64/10"},
		{"100.127.255.255", false, "cgnat upper"},

		// v4-mapped v6 must not smuggle a private v4 past the check.
		{"::ffff:127.0.0.1", false, "v4-mapped loopback"},
		{"::ffff:10.0.0.1", false, "v4-mapped rfc1918"},
		{"::ffff:169.254.169.254", false, "v4-mapped metadata"},
	} {
		t.Run(tc.addr+" "+tc.why, func(t *testing.T) {
			ip, err := netip.ParseAddr(tc.addr)
			if err != nil {
				t.Fatalf("bad test address: %v", err)
			}
			err = DenyPrivate(ip)
			if tc.allowed && err != nil {
				t.Errorf("%s (%s) must be allowed, got %v", tc.addr, tc.why, err)
			}
			if !tc.allowed && err == nil {
				t.Errorf("%s (%s) must be denied, got nil", tc.addr, tc.why)
			}
		})
	}
}

func TestDenyPrivateRejectsInvalidAddr(t *testing.T) {
	if err := DenyPrivate(netip.Addr{}); err == nil {
		t.Fatal("zero Addr must be denied, got nil")
	}
}
