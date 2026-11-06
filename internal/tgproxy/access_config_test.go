package tgproxy

import (
	"net/netip"
	"strings"
	"testing"
)

const accessFixture = `trusted_proxies: ["10.0.0.2"]
clients:
  - name: service-a
    bot_ids: ["123456"]
    allowed_ips: ["192.0.2.10", "198.51.100.0/24", "203.0.113.10-203.0.113.20", "2001:db8::10-2001:db8::20", "2001:db8:1::/48"]
`

func TestParseAccessConfigMatchesClient(t *testing.T) {
	p, err := ParseAccessConfig([]byte(accessFixture))
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ ip, bot, want string }{
		{"192.0.2.10", "123456", "service-a"},
		{"::ffff:192.0.2.10", "123456", "service-a"},
		{"192.0.2.11", "123456", ""},
		{"198.51.100.255", "123456", "service-a"},
		{"198.51.101.0", "123456", ""},
		{"203.0.113.10", "123456", "service-a"},
		{"203.0.113.20", "123456", "service-a"},
		{"203.0.113.9", "123456", ""},
		{"203.0.113.21", "123456", ""},
		{"2001:db8::10", "123456", "service-a"},
		{"2001:db8::20", "123456", "service-a"},
		{"2001:db8::21", "123456", ""},
		{"2001:db8:1::abcd", "123456", "service-a"},
		{"192.0.2.10", "999999", ""},
	} {
		t.Run(tt.ip+"/"+tt.bot, func(t *testing.T) {
			got := ""
			if c, _ := p.Authorize(tt.bot, netip.MustParseAddr(tt.ip), nil); c != nil {
				got = c.Name
			}
			if got != tt.want {
				t.Fatalf("client=%q want=%q", got, tt.want)
			}
		})
	}
}

func TestParseAccessConfigRejectsInvalidPolicy(t *testing.T) {
	for _, tt := range []struct{ name, raw string }{
		{"empty", ""}, {"empty clients", "clients: []"},
		{"unknown root field", accessFixture + "typo: true\n"},
		{"unknown client field", strings.Replace(accessFixture, "bot_ids:", "bot_idz:", 1)},
		{"empty IPs", "clients: [{name: a, bot_ids: ['1'], allowed_ips: []}]"},
		{"null IPs", "clients: [{name: a, bot_ids: ['1'], allowed_ips: null}]"},
		{"empty proxy key", "clients: [{name: a, bot_ids: ['1'], proxy_key: ''}]"},
		{"null proxy key", "clients: [{name: a, bot_ids: ['1'], proxy_key: null}]"},
		{"short proxy key", "clients: [{name: a, bot_ids: ['1'], proxy_key: 'short'}]"},
		{"invalid proxy key characters", "clients: [{name: a, bot_ids: ['1'], proxy_key: 'test key with spaces'}]"},
		{"duplicate proxy key", "clients: [{name: a, bot_ids: ['1'], proxy_key: '" + testProxyKey + "'}, {name: b, bot_ids: ['2'], proxy_key: '" + testProxyKey + "'}]"},
		{"empty bots", "clients: [{name: a, bot_ids: []}]"},
		{"bad bot", strings.Replace(accessFixture, "123456", "12abc", 1)},
		{"bad name", strings.Replace(accessFixture, "service-a", "bad!name", 1)},
		{"duplicate client", accessFixture + "  - name: service-a\n    bot_ids: ['2']\n    allowed_ips: ['192.0.2.3']\n"},
		{"bad IP", strings.Replace(accessFixture, "192.0.2.10", "999.1.1.1", 1)},
		{"bad prefix", strings.Replace(accessFixture, "198.51.100.0/24", "198.51.100.0/33", 1)},
		{"reversed range", strings.Replace(accessFixture, "203.0.113.10-203.0.113.20", "203.0.113.20-203.0.113.10", 1)},
		{"mixed families", strings.Replace(accessFixture, "203.0.113.10-203.0.113.20", "203.0.113.10-2001:db8::20", 1)},
		{"scoped IP", strings.Replace(accessFixture, "192.0.2.10", "fe80::1%eth0", 1)},
		{"scoped range", strings.Replace(accessFixture, "203.0.113.10-203.0.113.20", "fe80::1%eth0-fe80::2%eth0", 1)},
		{"bad proxy", strings.Replace(accessFixture, "10.0.0.2", "not-an-IP", 1)},
		{"multiple docs", accessFixture + "---\nclients: []\n"},
		{"duplicate YAML key", accessFixture + "trusted_proxies: []\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseAccessConfig([]byte(tt.raw)); err == nil {
				t.Fatal("invalid policy accepted")
			}
		})
	}
}

func TestInvalidYAMLDoesNotExposeProxyKey(t *testing.T) {
	_, err := ParseAccessConfig([]byte("clients: [{name: a, proxy_key: '" + testProxyKey + "', bot_ids: [}"))
	if err == nil {
		t.Fatal("invalid YAML accepted")
	}
	if strings.Contains(err.Error(), testProxyKey) {
		t.Error("YAML error exposed a proxy key")
	}
}

func TestAccessWithoutAllowedIPs(t *testing.T) {
	p, err := ParseAccessConfig([]byte(`clients:
 - {name: unrestricted, bot_ids: ['1', '2']}
 - {name: restricted, bot_ids: ['2'], allowed_ips: ['192.0.2.1']}
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, bot string
		ip        netip.Addr
		allowed   bool
	}{
		{"IPv4", "1", netip.MustParseAddr("198.51.100.1"), true},
		{"IPv6", "1", netip.MustParseAddr("2001:db8::1"), true},
		{"unknown IP", "1", netip.Addr{}, true},
		{"unknown bot", "3", netip.MustParseAddr("192.0.2.1"), false},
		{"overlapping clients", "2", netip.MustParseAddr("192.0.2.1"), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got, _ := p.Authorize(tt.bot, tt.ip, nil); (got != nil) != tt.allowed {
				t.Fatalf("client=%v want allowed=%t", got, tt.allowed)
			}
		})
	}
}

func TestAccessSharedBotsAndAmbiguousClients(t *testing.T) {
	p, err := ParseAccessConfig([]byte(`clients:
 - {name: a, bot_ids: ['1'], allowed_ips: ['192.0.2.0/24']}
 - {name: b, bot_ids: ['1'], allowed_ips: ['198.51.100.1', '192.0.2.1']}
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ ip, want string }{{"192.0.2.2", "a"}, {"198.51.100.1", "b"}, {"192.0.2.1", ""}} {
		c, _ := p.Authorize("1", netip.MustParseAddr(tt.ip), nil)
		got := ""
		if c != nil {
			got = c.Name
		}
		if got != tt.want {
			t.Fatalf("%s: %q want %q", tt.ip, got, tt.want)
		}
	}
}
