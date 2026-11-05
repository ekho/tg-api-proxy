package tgproxy

import (
	"strings"
	"testing"
)

// Ключи фикстур — 32 символа hex, как настоящие (openssl rand -hex 32 даёт 64,
// но проверка требует не менее 16, и короче держать тест читаемее).
const (
	legacyKey  = "0123456789abcdef0123456789abcdef"
	codoKey    = "fedcba9876543210fedcba9876543210"
	vacancyKey = "aaaa1111bbbb2222cccc3333dddd4444"
)

// validClients — канонический список из спеки: legacy владеет всеми ботами,
// codo — своими двумя, vacancy-bot — одним. Пересечение по 8953883505
// намеренное.
const validClients = "legacy:" + legacyKey + ":8953883505,6143991278,7770323132;" +
	"codo:" + codoKey + ":8953883505,6143991278;" +
	"vacancy-bot:" + vacancyKey + ":7770323132"

func TestParseClientsValid(t *testing.T) {
	c, err := ParseClients(validClients)
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if len(c.list) != 3 {
		t.Fatalf("клиентов = %d, want 3", len(c.list))
	}
}

// TestParseClientsSharedBotIsAllowed охраняет НАМЕРЕННО ослабленную проверку.
//
// Один bot_id у нескольких клиентов обязан приниматься: в переходный период бот
// принадлежит и legacy, и своему продукту одновременно, потому что клиент
// раскатывается постепенно и часть его подов ещё ходит со старым ключом. Запрет
// дубликатов ловил бы опечатки копипастой и на ревью выглядел бы разумной
// строгостью — поэтому здесь стоит тест, который упадёт, если её кто-то
// добавит.
func TestParseClientsSharedBotIsAllowed(t *testing.T) {
	c, err := ParseClients(validClients)
	if err != nil {
		t.Fatalf("общий бот у двух клиентов должен приниматься, получили: %v", err)
	}
	for _, key := range []string{legacyKey, codoKey} {
		client := c.Lookup(key)
		if client == nil {
			t.Fatal("клиент по своему ключу не найден")
		}
		if !client.Allows("8953883505") {
			t.Errorf("клиент %q должен пускать общего бота 8953883505", client.Name)
		}
	}
}

func TestParseClientsTolerance(t *testing.T) {
	// Пробелы вокруг разделителей и пустая запись между ';;' — не ошибка:
	// перенос строки в YAML-манифесте не должен менять смысл.
	raw := "  legacy:" + legacyKey + ": 8953883505 , 6143991278 ;; codo:" + codoKey + ":8953883505  "

	c, err := ParseClients(raw)
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if len(c.list) != 2 {
		t.Fatalf("клиентов = %d, want 2", len(c.list))
	}
	legacy := c.Lookup(legacyKey)
	if legacy == nil {
		t.Fatal("клиент legacy не найден")
	}
	if !legacy.Allows("6143991278") {
		t.Error("bot_id с пробелами вокруг должен разбираться")
	}
}

func TestParseClientsErrors(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "пустая строка (fail-closed)", raw: ""},
		{name: "только пробелы", raw: "   "},
		{name: "только разделители", raw: " ; ; "},
		{name: "два поля вместо трёх", raw: "codo:" + codoKey},
		{name: "четыре поля вместо трёх", raw: "codo:" + codoKey + ":123456:extra"},
		{name: "пустое имя", raw: ":" + codoKey + ":123456"},
		{name: "имя вне charset", raw: "co!do:" + codoKey + ":123456"},
		{name: "ключ короче 16 символов", raw: "codo:short:123456"},
		{name: "ключ вне charset", raw: "codo:abcdefghijklmnop!:123456"},
		{name: "нечисловой bot_id", raw: "codo:" + codoKey + ":12a456"},
		{name: "пустой список ботов", raw: "codo:" + codoKey + ":"},
		{name: "повтор имени", raw: "codo:" + codoKey + ":123456;codo:" + legacyKey + ":789012"},
		{name: "повтор ключа", raw: "codo:" + codoKey + ":123456;other:" + codoKey + ":789012"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseClients(tt.raw); err == nil {
				t.Fatalf("ожидалась ошибка на %q, получили nil", tt.raw)
			}
		})
	}
}

// TestParseClientsErrorNeverLeaksKey — тест на ОТСУТСТВИЕ.
//
// Ошибки конфигурации уезжают в лог пода при os.Exit(1). Ключ в такой строке
// означал бы секрет в логах кластера — ровно то, от чего защищает redact.go в
// остальном коде. Обычные тесты остались бы зелёными, если кто-то допишет в
// сообщение содержимое записи «чтобы было понятнее, что не так».
func TestParseClientsErrorNeverLeaksKey(t *testing.T) {
	const key = "SUPERSECRETKEYVALUE0123456789"

	tests := []struct {
		name string
		raw  string
	}{
		{name: "лишнее поле", raw: "codo:" + key + ":123456:extra"},
		{name: "нет списка ботов", raw: "codo:" + key},
		{name: "нечисловой bot_id", raw: "codo:" + key + ":abc"},
		{name: "пустой список ботов", raw: "codo:" + key + ":"},
		{name: "имя вне charset", raw: "co!do:" + key + ":123456"},
		{name: "повтор ключа", raw: "codo:" + key + ":123456;other:" + key + ":789012"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseClients(tt.raw)
			if err == nil {
				t.Fatalf("ожидалась ошибка на %q, получили nil", tt.raw)
			}
			if strings.Contains(err.Error(), key) {
				t.Errorf("ключ утёк в текст ошибки: %v", err)
			}
		})
	}
}

func TestClientsLookup(t *testing.T) {
	c, err := ParseClients(validClients)
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}

	tests := []struct {
		name string
		key  string
		want string // "" = клиент не должен найтись
	}{
		{name: "ключ legacy", key: legacyKey, want: "legacy"},
		{name: "ключ codo", key: codoKey, want: "codo"},
		{name: "ключ vacancy-bot", key: vacancyKey, want: "vacancy-bot"},
		{name: "неизвестный ключ той же длины", key: "00000000000000000000000000000000", want: ""},
		{name: "пустое значение", key: "", want: ""},
		{name: "префикс верного ключа", key: legacyKey[:16], want: ""},
		{name: "верный ключ с лишним символом", key: legacyKey + "x", want: ""},
		{name: "отличается регистром", key: strings.ToUpper(legacyKey), want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ""
			if client := c.Lookup(tt.key); client != nil {
				got = client.Name
			}
			if got != tt.want {
				t.Errorf("Lookup(...).Name = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestClientAllows(t *testing.T) {
	c, err := ParseClients(validClients)
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	codo := c.Lookup(codoKey)
	if codo == nil {
		t.Fatal("клиент codo не найден")
	}

	tests := []struct {
		name  string
		botID string
		want  bool
	}{
		{name: "свой бот", botID: "8953883505", want: true},
		{name: "второй свой бот", botID: "6143991278", want: true},
		{name: "бот другого клиента", botID: "7770323132", want: false},
		{name: "неизвестный бот", botID: "999999", want: false},
		{name: "пустой bot_id", botID: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := codo.Allows(tt.botID); got != tt.want {
				t.Errorf("Allows(%q) = %v, want %v", tt.botID, got, tt.want)
			}
		})
	}
}
