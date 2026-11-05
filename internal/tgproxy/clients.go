package tgproxy

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"
)

// HeaderProxyKey — заголовок, которым клиент предъявляет свой ключ.
// Вырезается перед отправкой запроса в Telegram (см. proxy.go).
const HeaderProxyKey = "X-Proxy-Key"

// minKeyLen — нижняя граница длины ключа клиента. Рекомендуемое значение
// генерируется как openssl rand -hex 32 (64 символа).
const minKeyLen = 16

// Разделители PROXY_CLIENTS.
//
// Список bot_id разделён запятой не по совпадению: ровно так это выглядело в
// прежней переменной ALLOWED_BOT_IDS, и привычка операторов сохраняется.
const (
	clientSep = ";"
	fieldSep  = ":"
	botSep    = ","
)

// clientFields — сколько полей в записи: имя, ключ, список bot_id.
const clientFields = 3

// Client — один потребитель прокси: имя, ключ и боты, которыми ему разрешено
// ходить.
//
// Ключ хранится хешем: значение в памяти процесса не нужно ни для чего, кроме
// сравнения, а хеш нельзя случайно напечатать в лог осмысленным.
type Client struct {
	Name    string
	keyHash [sha256.Size]byte
	bots    map[string]struct{}
}

// Clients — все настроенные клиенты.
//
// Слайс, а не map по хешу: поиск обязан проходить всех кандидатов за одинаковое
// время, а map этого не обещает (см. Lookup).
type Clients struct {
	list []*Client
}

// ParseClients разбирает PROXY_CLIENTS вида
// <имя>:<ключ>:<bot_id>[,<bot_id>...][;<имя>:<ключ>:<bot_id>...].
//
// Fail-closed: любая проблема — ошибка, а не значение по умолчанию. Прокси
// стоит на публичном auth: none роуте, и одна кривая переменная окружения не
// должна молча превращать его в открытый релей или в живой под, не пускающий
// никого.
//
// Пустые записи между разделителями пропускаются — висящая ';' не ошибка.
// Ошибкой является отсутствие хотя бы одной записи по итогу разбора.
func ParseClients(raw string) (*Clients, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("PROXY_CLIENTS empty: proxy won't start without a list of clients")
	}

	clients := &Clients{}
	names := make(map[string]struct{})
	keys := make(map[[sha256.Size]byte]struct{})

	for i, entry := range strings.Split(raw, clientSep) {
		if strings.TrimSpace(entry) == "" {
			continue
		}
		client, err := parseClient(entry, i+1)
		if err != nil {
			return nil, err
		}
		// Одинаковые имена сделали бы двух разных клиентов неотличимыми в логе
		// и в лейбле метрики.
		if _, dup := names[client.Name]; dup {
			return nil, fmt.Errorf("PROXY_CLIENTS: client name %q occurs more than once", client.Name)
		}
		// Одинаковые ключи сделали бы Lookup неоднозначным: два клиента с
		// разными наборами ботов предъявляли бы один ключ.
		if _, dup := keys[client.keyHash]; dup {
			return nil, fmt.Errorf("PROXY_CLIENTS: client %q reuses a key already given to another client", client.Name)
		}
		names[client.Name] = struct{}{}
		keys[client.keyHash] = struct{}{}
		clients.list = append(clients.list, client)
	}

	if len(clients.list) == 0 {
		return nil, errors.New("PROXY_CLIENTS holds no clients: proxy won't start without at least one")
	}
	return clients, nil
}

// parseClient разбирает одну запись списка.
//
// num — номер записи, начиная с 1. Пока запись не разобрана, неизвестно, какое
// поле является именем, а печатать её содержимое нельзя: в нём ключ. Поэтому до
// успешной проверки charset запись адресуется только номером, а дальше — уже
// проверенным именем.
func parseClient(entry string, num int) (*Client, error) {
	fields := strings.Split(strings.TrimSpace(entry), fieldSep)
	if len(fields) != clientFields {
		return nil, fmt.Errorf("PROXY_CLIENTS: entry #%d is not in <name>:<key>:<bot_id>[,<bot_id>...] form", num)
	}

	name := strings.TrimSpace(fields[0])
	if name == "" {
		return nil, fmt.Errorf("PROXY_CLIENTS: entry #%d has an empty client name", num)
	}
	if !isSafeToken(name) {
		return nil, fmt.Errorf("PROXY_CLIENTS: client name in entry #%d has characters outside A-Za-z0-9_-", num)
	}

	key := strings.TrimSpace(fields[1])
	if len(key) < minKeyLen {
		return nil, fmt.Errorf("PROXY_CLIENTS: key of client %q is shorter than %d characters", name, minKeyLen)
	}
	if !isSafeToken(key) {
		return nil, fmt.Errorf("PROXY_CLIENTS: key of client %q has characters outside A-Za-z0-9_-", name)
	}

	bots := make(map[string]struct{})
	for id := range strings.SplitSeq(fields[2], botSep) {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if !isDigits(id) {
			return nil, fmt.Errorf("PROXY_CLIENTS: %q in the bot list of client %q is not a numeric bot_id", id, name)
		}
		bots[id] = struct{}{}
	}
	// Клиент без ботов — живая запись, не пускающая никого: ошибка
	// конфигурации, а не рабочее состояние.
	if len(bots) == 0 {
		return nil, fmt.Errorf("PROXY_CLIENTS: client %q has no bot_id", name)
	}

	return &Client{Name: name, keyHash: sha256.Sum256([]byte(key)), bots: bots}, nil
}

// Lookup находит клиента по предъявленному ключу. nil — ключ не принадлежит
// никому.
//
// Сравниваются SHA-256 значений, а не сами строки: ConstantTimeCompare на
// аргументах разной длины возвращается немедленно и тем самым выдаёт длину
// ключа. Хеши всегда одного размера, поэтому сравнение постоянно по времени
// целиком.
//
// Раннего выхода при совпадении нет намеренно: число сравнений не должно
// зависеть от того, какой именно клиент предъявил ключ. При десятке клиентов
// это бесплатно.
func (c *Clients) Lookup(providedKey string) *Client {
	got := sha256.Sum256([]byte(providedKey))
	var found *Client
	for _, client := range c.list {
		if subtle.ConstantTimeCompare(got[:], client.keyHash[:]) == 1 {
			found = client
		}
	}
	return found
}

// Allows сообщает, разрешён ли этому клиенту бот с таким идентификатором.
func (c *Client) Allows(botID string) bool {
	_, ok := c.bots[botID]
	return ok
}

// isSafeToken ограничивает набор символов имени клиента и ключа.
//
// Для ключа это обязательное требование, а не гигиена: ':' ';' и ',' — наши
// разделители, и ключ с любым из них разъехался бы с числом полей. Для имени —
// защита строки лога и лейбла метрики от произвольного значения из
// конфигурации.
func isSafeToken(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return s != ""
}

// isDigits сообщает, что строка состоит только из цифр и непуста.
func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}
