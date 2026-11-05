package tgproxy

import (
	"strings"
	"testing"
	"time"
)

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func validEnv() map[string]string {
	return map[string]string{
		"LLM_PROXY_URL": "http://llm-proxy.platform.svc:3128",
		"PROXY_CLIENTS": "codo:3f8a1c9e5b7d2f4a6c8e0b1d3f5a7c9e:123456,789012",
	}
}

func TestLoadSettingsValid(t *testing.T) {
	s, err := LoadSettings(envFrom(validEnv()))
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}

	if s.Addr != ":8080" {
		t.Errorf("Addr = %q, want %q", s.Addr, ":8080")
	}
	if s.Handler.LongPollMax != 45*time.Second {
		t.Errorf("LongPollMax = %v, want 45s", s.Handler.LongPollMax)
	}
	if s.Handler.Upstream == nil || s.Handler.Upstream.String() != "https://api.telegram.org" {
		t.Errorf("Upstream = %v, want https://api.telegram.org", s.Handler.Upstream)
	}
	if s.Handler.ProxyURL == nil || s.Handler.ProxyURL.Host != "llm-proxy.platform.svc:3128" {
		t.Errorf("ProxyURL = %v", s.Handler.ProxyURL)
	}
	client := s.Handler.Clients.Lookup("3f8a1c9e5b7d2f4a6c8e0b1d3f5a7c9e")
	if client == nil {
		t.Fatal("ключ клиента не принимается")
	}
	if client.Name != "codo" {
		t.Errorf("имя клиента = %q, want %q", client.Name, "codo")
	}
	if !client.Allows("789012") {
		t.Error("клиент не пускает бота 789012")
	}
}

func TestLoadSettingsRequiresVars(t *testing.T) {
	for _, missing := range []string{"LLM_PROXY_URL", "PROXY_CLIENTS"} {
		t.Run("без "+missing, func(t *testing.T) {
			env := validEnv()
			delete(env, missing)

			_, err := LoadSettings(envFrom(env))
			if err == nil {
				t.Fatalf("ожидалась ошибка без %s, получили nil", missing)
			}
			if !strings.Contains(err.Error(), missing) {
				t.Errorf("ошибка не называет переменную %s: %v", missing, err)
			}
		})
	}
}

func TestLoadSettingsRejectsBadValues(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "неразбираемый адрес прокси", key: "LLM_PROXY_URL", value: "://нет-схемы"},
		{name: "прокси без схемы", key: "LLM_PROXY_URL", value: "llm-proxy:3128"},
		{name: "нечисловой таймаут", key: "LONG_POLL_MAX_TIMEOUT", value: "сорок"},
		{name: "нулевой таймаут", key: "LONG_POLL_MAX_TIMEOUT", value: "0"},
		{name: "нечисловой порт", key: "PORT", value: "восемь"},
		{name: "привилегированный порт", key: "PORT", value: "80"},
		{name: "нечисловой bot_id", key: "PROXY_CLIENTS", value: "codo:3f8a1c9e5b7d2f4a6c8e0b1d3f5a7c9e:123,abc"},
		{name: "короткий ключ", key: "PROXY_CLIENTS", value: "codo:short:123456"},
		{name: "запись без списка ботов", key: "PROXY_CLIENTS", value: "codo:3f8a1c9e5b7d2f4a6c8e0b1d3f5a7c9e"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := validEnv()
			env[tt.key] = tt.value

			if _, err := LoadSettings(envFrom(env)); err == nil {
				t.Fatalf("ожидалась ошибка при %s=%q, получили nil", tt.key, tt.value)
			}
		})
	}
}

func TestLoadSettingsOptionalOverrides(t *testing.T) {
	env := validEnv()
	env["PORT"] = "9090"
	env["LONG_POLL_MAX_TIMEOUT"] = "30"

	s, err := LoadSettings(envFrom(env))
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if s.Addr != ":9090" {
		t.Errorf("Addr = %q, want %q", s.Addr, ":9090")
	}
	if s.Handler.LongPollMax != 30*time.Second {
		t.Errorf("LongPollMax = %v, want 30s", s.Handler.LongPollMax)
	}
}

func TestLoadSettingsLogExcludeMethods(t *testing.T) {
	// baseEnv — минимальное окружение, при котором LoadSettings успешен.
	baseEnv := map[string]string{
		"LLM_PROXY_URL": "http://llm-proxy.ai-platform-egress.svc.cluster.local:3128",
		"PROXY_CLIENTS": "codo:0123456789abcdef0123456789abcdef:123456",
	}

	envWith := func(raw string) func(string) string {
		return func(k string) string {
			if k == "LOG_EXCLUDE_METHODS" {
				return raw
			}
			return baseEnv[k]
		}
	}

	t.Run("по умолчанию исключён getUpdates", func(t *testing.T) {
		s, err := LoadSettings(envWith(""))
		if err != nil {
			t.Fatalf("LoadSettings: %v", err)
		}
		f := s.Handler.LogFilter
		if f.ShouldLog("getupdates", 200) {
			t.Error("getUpdates должен быть исключён по умолчанию")
		}
		if !f.ShouldLog("getme", 200) {
			t.Error("остальные методы должны логироваться")
		}
	})

	t.Run("список с пробелами и разным регистром", func(t *testing.T) {
		s, err := LoadSettings(envWith(" getMe , SENDMESSAGE "))
		if err != nil {
			t.Fatalf("LoadSettings: %v", err)
		}
		f := s.Handler.LogFilter
		if f.ShouldLog("getme", 200) || f.ShouldLog("sendmessage", 200) {
			t.Error("перечисленные методы должны быть исключены")
		}
		if !f.ShouldLog("getupdates", 200) {
			t.Error("явный список заменяет значение по умолчанию целиком")
		}
	})

	t.Run("none отключает фильтр", func(t *testing.T) {
		// strings.EqualFold делает сравнение регистронезависимым, а
		// strings.TrimSpace — нечувствительным к пробелам вокруг значения;
		// оба варианта закреплены отдельными кейсами, а не одним "none".
		tests := []struct {
			name string
			raw  string
		}{
			{name: "нижний регистр", raw: "none"},
			{name: "верхний регистр", raw: "NONE"},
			{name: "пробелы и смешанный регистр", raw: " None "},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				s, err := LoadSettings(envWith(tt.raw))
				if err != nil {
					t.Fatalf("LoadSettings: %v", err)
				}
				if !s.Handler.LogFilter.ShouldLog("getupdates", 200) {
					t.Error("при none не исключается ничего")
				}
			})
		}
	})

	tests := []struct {
		name  string
		value string
	}{
		{name: "подчёркивание вместо camelCase", value: "get_updates"},
		{name: "висячая запятая", value: "getMe,"},
		{name: "пустой элемент между запятыми", value: "a,,b"},
		{name: "недопустимый символ", value: "getUpdates!"},
		{name: "длина больше 32 символов", value: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	}
	for _, tt := range tests {
		t.Run("опечатка: "+tt.name, func(t *testing.T) {
			if _, err := LoadSettings(envWith(tt.value)); err == nil {
				t.Fatalf("LOG_EXCLUDE_METHODS=%q: ожидалась ошибка, а не тихое бездействие", tt.value)
			}
		})
	}
}
