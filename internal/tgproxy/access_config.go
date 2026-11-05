package tgproxy

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/goccy/go-yaml"
	"go4.org/netipx"
)

// AccessConfig is immutable after validation; requests retain one complete version.
type AccessConfig struct {
	digest       [sha256.Size]byte
	clients      []accessClient
	withClientIP func(http.Handler) http.Handler
}

type accessClient struct {
	Name string
	bots []string
	ips  *netipx.IPSet
}

var clientNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
var botIDPattern = regexp.MustCompile(`^[0-9]+$`)

func ParseAccessConfig(raw []byte) (*AccessConfig, error) {
	var doc struct {
		TrustedProxies []string `yaml:"trusted_proxies"`
		Clients        []struct {
			Name       string   `yaml:"name"`
			BotIDs     []string `yaml:"bot_ids"`
			AllowedIPs []string `yaml:"allowed_ips"`
		} `yaml:"clients"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw), yaml.Strict())
	if err := decoder.Decode(&doc); err != nil {
		return nil, fmt.Errorf("config YAML: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("config must contain exactly one YAML document")
	}
	if len(doc.Clients) == 0 {
		return nil, errors.New("config must contain at least one client")
	}
	config := &AccessConfig{digest: sha256.Sum256(raw), withClientIP: middleware.ClientIPFromRemoteAddr}
	names := make(map[string]bool)
	for _, entry := range doc.Clients {
		if !clientNamePattern.MatchString(entry.Name) || names[entry.Name] {
			return nil, fmt.Errorf("invalid or duplicate client name %q", entry.Name)
		}
		names[entry.Name] = true
		if len(entry.BotIDs) == 0 || len(entry.AllowedIPs) == 0 {
			return nil, fmt.Errorf("client %q requires bot_ids and allowed_ips", entry.Name)
		}
		for _, id := range entry.BotIDs {
			if !botIDPattern.MatchString(id) {
				return nil, fmt.Errorf("client %q has a nonnumeric bot_id", entry.Name)
			}
		}
		ips, err := parseIPSet(entry.AllowedIPs)
		if err != nil {
			return nil, fmt.Errorf("client %q allowed_ips: %w", entry.Name, err)
		}
		config.clients = append(config.clients, accessClient{Name: entry.Name, bots: entry.BotIDs, ips: ips})
	}
	if len(doc.TrustedProxies) > 0 {
		ips, err := parseIPSet(doc.TrustedProxies)
		if err != nil {
			return nil, fmt.Errorf("trusted_proxies: %w", err)
		}
		prefixes := ips.Prefixes()
		values := make([]string, len(prefixes))
		for i, prefix := range prefixes {
			values[i] = prefix.String()
		}
		config.withClientIP = middleware.ClientIPFromXFF(values...)
	}
	return config, nil
}

func parseIPSet(values []string) (*netipx.IPSet, error) {
	var builder netipx.IPSetBuilder
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if strings.Contains(value, "%") {
			return nil, errors.New("scoped IPv6 addresses are not supported")
		}
		switch {
		case strings.Contains(value, "-"):
			r, err := netipx.ParseIPRange(value)
			if err != nil {
				return nil, err
			}
			builder.AddRange(netipx.IPRangeFrom(r.From().Unmap(), r.To().Unmap()))
		case strings.Contains(value, "/"):
			p, err := netip.ParsePrefix(value)
			if err != nil {
				return nil, err
			}
			if p.Addr().Is4In6() {
				return nil, errors.New("use native IPv4 notation for IPv4 prefixes")
			}
			builder.AddPrefix(p)
		default:
			ip, err := netip.ParseAddr(value)
			if err != nil {
				return nil, err
			}
			builder.Add(ip.Unmap())
		}
	}
	return builder.IPSet()
}

func (c *AccessConfig) Authorize(botID string, ip netip.Addr) *accessClient {
	var match *accessClient
	for i := range c.clients {
		client := &c.clients[i]
		if slices.Contains(client.bots, botID) && client.ips.Contains(ip.Unmap()) {
			if match != nil {
				return nil
			}
			match = client
		}
	}
	return match
}
