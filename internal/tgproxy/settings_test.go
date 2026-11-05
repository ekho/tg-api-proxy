package tgproxy

import (
	"testing"
	"time"
)

func envFrom(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLoadSettingsValid(t *testing.T) {
	s, err := LoadSettings(envFrom(nil))
	if err != nil {
		t.Fatal(err)
	}
	if s.Addr != ":8080" || s.ConfigFile != "config.yaml" || s.Handler.LongPollMax != 45*time.Second || s.Handler.Upstream.String() != "https://api.telegram.org" {
		t.Fatalf("unexpected defaults: %+v", s)
	}
}

func TestLoadSettingsOptionalOverrides(t *testing.T) {
	s, err := LoadSettings(envFrom(map[string]string{"CONFIG_FILE": "/config/config.yaml", "PORT": "9090", "LONG_POLL_MAX_TIMEOUT": "30"}))
	if err != nil {
		t.Fatal(err)
	}
	if s.ConfigFile != "/config/config.yaml" || s.Addr != ":9090" || s.Handler.LongPollMax != 30*time.Second {
		t.Fatalf("overrides ignored: %+v", s)
	}
}

func TestLoadSettingsRejectsBadValues(t *testing.T) {
	for _, tt := range []struct{ key, value string }{{"PORT", "text"}, {"PORT", "80"}, {"PORT", "65536"}, {"LONG_POLL_MAX_TIMEOUT", "zero"}, {"LONG_POLL_MAX_TIMEOUT", "0"}, {"LONG_POLL_MAX_TIMEOUT", "9223372036854775807"}} {
		t.Run(tt.key+"/"+tt.value, func(t *testing.T) {
			if _, err := LoadSettings(envFrom(map[string]string{tt.key: tt.value})); err == nil {
				t.Fatal("invalid setting accepted")
			}
		})
	}
}

func TestLoadSettingsLogExcludeMethods(t *testing.T) {
	// baseEnv — минимальное окружение, при котором LoadSettings успешен.
	baseEnv := map[string]string{}

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
