package tgproxy

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"golang.org/x/net/http/httpguts"
)

const (
	authHeader = "header"
	authBearer = "bearer"
	authBasic  = "basic"
)

var bearerTokenPattern = regexp.MustCompile(`^[A-Za-z0-9._~+/-]+=*$`)

type clientAuthConfig struct {
	Type     string `yaml:"type"`
	Header   string `yaml:"header"`
	Value    string `yaml:"value"`
	Token    string `yaml:"token"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type clientAuth struct {
	kind   string
	header string
	digest [sha256.Size]byte
}

func (a clientAuthConfig) compile(fields map[string]any) (clientAuth, error) {
	for _, value := range fields {
		if _, ok := value.(string); !ok {
			return clientAuth{}, errors.New("auth fields must be strings")
		}
	}
	auth := clientAuth{kind: a.Type, header: "Authorization"}
	var secret string
	switch a.Type {
	case authHeader:
		auth.header = http.CanonicalHeaderKey(a.Header)
		if len(fields) != 3 || !validCredentialHeader(auth.header) || a.Value == "" ||
			strings.TrimSpace(a.Value) != a.Value || !httpguts.ValidHeaderFieldValue(a.Value) {
			return clientAuth{}, errors.New("header auth requires a custom header name and a valid nonempty value")
		}
		secret = a.Value
	case authBearer:
		if len(fields) != 2 || !bearerTokenPattern.MatchString(a.Token) {
			return clientAuth{}, errors.New("bearer auth requires only a valid token")
		}
		secret = a.Token
	case authBasic:
		if len(fields) != 3 || a.Username == "" || a.Password == "" || strings.Contains(a.Username, ":") ||
			strings.ContainsFunc(a.Username+a.Password, unicode.IsControl) {
			return clientAuth{}, errors.New("basic auth requires a username without colons and a nonempty password")
		}
		secret = a.Username + ":" + a.Password
	default:
		return clientAuth{}, errors.New("auth type must be header, bearer, or basic")
	}
	auth.digest = sha256.Sum256([]byte(secret))
	return auth, nil
}

func validCredentialHeader(header string) bool {
	// Keep routing, forwarding, framing, and diagnostic headers separate from credentials.
	if !httpguts.ValidHeaderFieldName(header) || slices.Contains(addrHeaders, header) ||
		strings.HasPrefix(header, "X-Forwarded-") || strings.HasPrefix(header, "Content-") {
		return false
	}
	switch header {
	case "Authorization", "Proxy-Authorization", "Host", "Connection", "Proxy-Connection",
		"Keep-Alive", "Transfer-Encoding", "Te", "Trailer", "Upgrade", "Expect",
		"Traceparent", "Tracestate", "Baggage":
		return false
	}
	return true
}

func (a clientAuth) matches(headers http.Header) bool {
	values := headers.Values(a.header)
	if len(values) != 1 {
		return false
	}
	secret := values[0]
	switch a.kind {
	case authBearer:
		scheme, token, ok := strings.Cut(secret, " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") {
			return false
		}
		secret = strings.TrimLeft(token, " ")
	case authBasic:
		req := http.Request{Header: headers}
		username, password, ok := req.BasicAuth()
		if !ok {
			return false
		}
		secret = username + ":" + password
	}
	got := sha256.Sum256([]byte(secret))
	return subtle.ConstantTimeCompare(got[:], a.digest[:]) == 1
}

func (a clientAuth) challenge() string {
	switch a.kind {
	case authBasic:
		return `Basic realm="tg-api-proxy", charset="UTF-8"`
	case authBearer:
		return `Bearer realm="tg-api-proxy"`
	default:
		return `ApiKey realm="tg-api-proxy", header="` + a.header + `"`
	}
}
