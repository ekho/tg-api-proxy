package tgproxy

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestConfiguredAuthSchemes(t *testing.T) {
	const policy = `clients:
 - {name: header, bot_ids: ['1'], allowed_ips: ['192.0.2.1'], auth: {type: header, header: x-api-key, value: test-header-secret}}
 - {name: bearer, bot_ids: ['2'], auth: {type: bearer, token: test-bearer-secret}}
 - {name: basic, bot_ids: ['3'], auth: {type: basic, username: test-user, password: 'test:basic-secret'}}
 - {name: open, bot_ids: ['4']}
`
	access := testAccess(t, policy)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, header := range []string{"X-Api-Key", "Authorization", "Proxy-Authorization", "X-Proxy-Key"} {
			if len(r.Header.Values(header)) != 0 {
				t.Errorf("credential header %s reached upstream", header)
			}
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Error("content type was lost")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	var logs bytes.Buffer
	h, err := NewHandler(Config{Upstream: u, Access: access, Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, bot, ip string
		headers       http.Header
		basicPassword string
		want          int
		challenge     string
	}{
		{name: "header", bot: "1", ip: "192.0.2.1", headers: http.Header{"X-Api-Key": {"test-header-secret"}}, want: 200},
		{name: "header and IP both required", bot: "1", ip: "192.0.2.2", headers: http.Header{"X-Api-Key": {"test-header-secret"}}, want: 403},
		{name: "bearer", bot: "2", headers: http.Header{"Authorization": {"bEaReR  test-bearer-secret"}}, want: 200},
		{name: "bearer wrong token", bot: "2", headers: http.Header{"Authorization": {"Bearer wrong-token"}}, want: 401, challenge: "Bearer "},
		{name: "bearer repeated", bot: "2", headers: http.Header{"Authorization": {"Bearer test-bearer-secret", "Bearer test-bearer-secret"}}, want: 401, challenge: "Bearer "},
		{name: "basic", bot: "3", basicPassword: "test:basic-secret", want: 200},
		{name: "basic wrong password", bot: "3", basicPassword: "wrong-password", want: 401, challenge: "Basic "},
		{name: "basic malformed", bot: "3", headers: http.Header{"Authorization": {"Basic !"}}, want: 401, challenge: "Basic "},
		{name: "basic missing", bot: "3", want: 401, challenge: "Basic "},
		{name: "unrelated credentials stripped", bot: "4", headers: http.Header{"X-Api-Key": {"test-header-secret"}, "Authorization": {"Bearer test-bearer-secret"}, "X-Proxy-Key": {"test-legacy-secret"}}, want: 200},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/bot"+tt.bot+strings.TrimPrefix(testToken, testBotID)+"/getMe", nil)
			req.Header = tt.headers.Clone()
			if req.Header == nil {
				req.Header = make(http.Header)
			}
			req.Header.Set("Content-Type", "application/json")
			if tt.ip != "" {
				req.RemoteAddr = tt.ip + ":1234"
			}
			if tt.basicPassword != "" {
				req.SetBasicAuth("test-user", tt.basicPassword)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Fatalf("status=%d want=%d", rec.Code, tt.want)
			}
			if tt.challenge != "" && !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), tt.challenge) {
				t.Errorf("expected %s challenge", tt.challenge)
			}
		})
	}
	for _, secret := range []string{"test-header-secret", "test-bearer-secret", "test:basic-secret"} {
		if strings.Contains(logs.String(), secret) {
			t.Error("credential appeared in request logs")
		}
	}
}

func TestRejectInvalidAuthConfig(t *testing.T) {
	for _, tt := range []struct{ name, fields string }{
		{"explicit null", "auth: null"},
		{"unknown scheme", "auth: {type: digest}"},
		{"mixed scheme fields", "auth: {type: bearer, token: test-token, password: null}"},
		{"bad bearer", "auth: {type: bearer, token: 'two words'}"},
		{"boolean token", "auth: {type: bearer, token: false}"},
		{"missing basic password", "auth: {type: basic, username: alice}"},
		{"invalid basic username", "auth: {type: basic, username: 'a:b', password: secret}"},
		{"diagnostic header", "auth: {type: header, header: x-real-ip, value: secret}"},
		{"forwarding header", "auth: {type: header, header: X-Forwarded-For, value: secret}"},
		{"authorization header", "auth: {type: header, header: Authorization, value: secret}"},
		{"invalid header name", "auth: {type: header, header: 'X Api Key', value: secret}"},
		{"legacy conflict", "auth: {type: bearer, token: test-token}, proxy_key: '" + testProxyKey + "'"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseAccessConfig([]byte("clients: [{name: test, bot_ids: ['1'], " + tt.fields + "}]")); err == nil {
				t.Fatal("invalid authentication configuration accepted")
			}
		})
	}
}
