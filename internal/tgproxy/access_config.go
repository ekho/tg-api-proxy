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
	digest            [sha256.Size]byte
	clients           []accessClient
	credentialHeaders []string
	withClientIP      func(http.Handler) http.Handler
}

type accessClient struct {
	Name string
	bots []string
	ips  *netipx.IPSet
	auth *clientAuth
}

const HeaderProxyKey = "X-Proxy-Key"
const minProxyKeyLength = 16

var safeTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
var botIDPattern = regexp.MustCompile(`^[0-9]+$`)

func ParseAccessConfig(raw []byte) (*AccessConfig, error) {
	var doc struct {
		TrustedProxies []string `yaml:"trusted_proxies"`
		Clients        []struct {
			Name       string            `yaml:"name"`
			BotIDs     []string          `yaml:"bot_ids"`
			AllowedIPs []string          `yaml:"allowed_ips"`
			ProxyKey   string            `yaml:"proxy_key"`
			Auth       *clientAuthConfig `yaml:"auth"`
		} `yaml:"clients"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw), yaml.Strict())
	if err := decoder.Decode(&doc); err != nil {
		return nil, configYAMLError(err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("config must contain exactly one YAML document")
	}
	if len(doc.Clients) == 0 {
		return nil, errors.New("config must contain at least one client")
	}
	config := &AccessConfig{
		digest:            sha256.Sum256(raw),
		credentialHeaders: []string{"Authorization", "Proxy-Authorization", HeaderProxyKey},
		withClientIP:      middleware.ClientIPFromRemoteAddr,
	}
	names := make(map[string]bool)
	credentials := make(map[clientAuth]bool)
	// Keep field presence to distinguish omitted settings from explicit nulls.
	var fields struct {
		Clients []map[string]any `yaml:"clients"`
	}
	if err := yaml.Unmarshal(raw, &fields); err != nil {
		return nil, configYAMLError(err)
	}
	for i, entry := range doc.Clients {
		if !safeTokenPattern.MatchString(entry.Name) || names[entry.Name] {
			return nil, fmt.Errorf("invalid or duplicate client name %q", entry.Name)
		}
		names[entry.Name] = true
		if len(entry.BotIDs) == 0 {
			return nil, fmt.Errorf("client %q requires bot_ids", entry.Name)
		}
		for _, id := range entry.BotIDs {
			if !botIDPattern.MatchString(id) {
				return nil, fmt.Errorf("client %q has a nonnumeric bot_id", entry.Name)
			}
		}
		if _, exists := fields.Clients[i]["allowed_ips"]; exists && len(entry.AllowedIPs) == 0 {
			return nil, fmt.Errorf("client %q allowed_ips must be a nonempty list", entry.Name)
		}
		client := accessClient{Name: entry.Name, bots: entry.BotIDs}
		if len(entry.AllowedIPs) > 0 {
			var err error
			client.ips, err = parseIPSet(entry.AllowedIPs)
			if err != nil {
				return nil, fmt.Errorf("client %q allowed_ips: %w", entry.Name, err)
			}
		}
		rawAuth, hasAuth := fields.Clients[i]["auth"]
		_, hasProxyKey := fields.Clients[i]["proxy_key"]
		if hasAuth && hasProxyKey {
			return nil, fmt.Errorf("client %q cannot specify both auth and proxy_key", entry.Name)
		}
		if hasProxyKey {
			if len(entry.ProxyKey) < minProxyKeyLength || !safeTokenPattern.MatchString(entry.ProxyKey) {
				return nil, fmt.Errorf("client %q proxy_key must contain at least %d characters from A-Za-z0-9_-", entry.Name, minProxyKeyLength)
			}
			client.auth = &clientAuth{kind: authHeader, header: HeaderProxyKey, digest: sha256.Sum256([]byte(entry.ProxyKey))}
		}
		if hasAuth {
			authFields, ok := rawAuth.(map[string]any)
			if !ok || entry.Auth == nil {
				return nil, fmt.Errorf("client %q auth must be a mapping", entry.Name)
			}
			auth, err := entry.Auth.compile(authFields)
			if err != nil {
				return nil, fmt.Errorf("client %q: %w", entry.Name, err)
			}
			client.auth = &auth
		}
		if client.auth != nil {
			if credentials[*client.auth] {
				return nil, fmt.Errorf("client %q reuses another client's credentials", entry.Name)
			}
			credentials[*client.auth] = true
			if !slices.Contains(config.credentialHeaders, client.auth.header) {
				config.credentialHeaders = append(config.credentialHeaders, client.auth.header)
			}
		}
		config.clients = append(config.clients, client)
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

func configYAMLError(err error) error {
	// Decoder errors can quote the YAML source, including credentials.
	var yamlErr yaml.Error
	if errors.As(err, &yamlErr) {
		if tok := yamlErr.GetToken(); tok != nil && tok.Position != nil {
			return fmt.Errorf("invalid configuration YAML at line %d, column %d", tok.Position.Line, tok.Position.Column)
		}
	}
	return errors.New("invalid configuration YAML")
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

func (c *AccessConfig) Authorize(botID string, ip netip.Addr, headers http.Header) (*accessClient, []string) {
	var match *accessClient
	var challenges []string
	for i := range c.clients {
		client := &c.clients[i]
		if !slices.Contains(client.bots, botID) || (client.ips != nil && !client.ips.Contains(ip.Unmap())) {
			continue
		}
		if client.auth != nil && !client.auth.matches(headers) {
			challenge := client.auth.challenge()
			if !slices.Contains(challenges, challenge) {
				challenges = append(challenges, challenge)
			}
			continue
		}
		if match != nil {
			return nil, nil
		}
		match = client
	}
	if match == nil {
		return nil, challenges
	}
	return match, nil
}
