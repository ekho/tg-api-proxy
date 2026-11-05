package tgproxy

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// telegramAPIBase — база Bot API. Не выносится в окружение намеренно:
	// апстрим у этого сервиса ровно один.
	telegramAPIBase = "https://api.telegram.org"

	defaultPort = 8080
	// minPort — процесс работает под UID 65534, а непривилегированный
	// пользователь не может занять порт ниже 1024.
	minPort = 1024
)

// defaultLogExcludeMethods — методы, чьи успешные вызовы не логируются по
// умолчанию. getUpdates — основной трафик сервиса: боты опрашивают его
// непрерывно, и в логе он вытеснил бы все остальные события.
var defaultLogExcludeMethods = []string{"getUpdates"}

// logExcludeNone — значение LOG_EXCLUDE_METHODS, отключающее фильтр целиком.
//
// Пустая строка для этого не годится: os.Getenv не различает незаданную
// переменную и заданную пустой, поэтому «логировать всё» было бы неотличимо от
// «применить значение по умолчанию».
const logExcludeNone = "none"

// Settings — всё, что приложение читает из окружения.
type Settings struct {
	Handler Config
	Addr    string
}

// LoadSettings собирает конфигурацию из переменных окружения.
//
// getenv передаётся явно, чтобы конфигурацию можно было проверить тестами, не
// трогая окружение процесса.
//
// Любая проблема — ошибка, а не значение по умолчанию: сервис стоит на
// публичном роуте, и подняться наполовину настроенным он не должен.
func LoadSettings(getenv func(string) string) (*Settings, error) {
	proxyRaw := getenv("LLM_PROXY_URL")
	if proxyRaw == "" {
		return nil, errors.New("LLM_PROXY_URL not set: without the platform proxy, Telegram is unreachable")
	}
	proxyURL, err := url.Parse(proxyRaw)
	if err != nil {
		return nil, fmt.Errorf("LLM_PROXY_URL: %w", err)
	}
	if proxyURL.Scheme == "" || proxyURL.Host == "" {
		return nil, fmt.Errorf("LLM_PROXY_URL: need address with scheme and host, got %q", proxyRaw)
	}

	// Сообщения об ошибках ParseClients уже называют свою переменную.
	clients, err := ParseClients(getenv("PROXY_CLIENTS"))
	if err != nil {
		return nil, err
	}

	upstream, err := url.Parse(telegramAPIBase)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", telegramAPIBase, err)
	}

	longPoll := defaultLongPollMax
	if raw := getenv("LONG_POLL_MAX_TIMEOUT"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("LONG_POLL_MAX_TIMEOUT: need an integer number of seconds, got %q", raw)
		}
		if n <= 0 {
			return nil, fmt.Errorf("LONG_POLL_MAX_TIMEOUT: need a value greater than zero, got %d", n)
		}
		longPoll = time.Duration(n) * time.Second
	}

	excluded := defaultLogExcludeMethods
	if raw := getenv("LOG_EXCLUDE_METHODS"); raw != "" {
		if strings.EqualFold(strings.TrimSpace(raw), logExcludeNone) {
			excluded = nil
		} else {
			excluded = nil
			for _, part := range strings.Split(raw, ",") {
				name := strings.TrimSpace(part)
				// Валидация формы — защита от опечатки: без неё
				// LOG_EXCLUDE_METHODS=getUpdate молча не исключил бы ничего, и
				// причину пришлось бы искать в логах, которых нет.
				if !methodNameRe.MatchString(name) {
					return nil, fmt.Errorf("LOG_EXCLUDE_METHODS: %q does not look like a Bot API method name (expected 1-32 Latin letters)", name)
				}
				excluded = append(excluded, name)
			}
		}
	}

	port := defaultPort
	if raw := getenv("PORT"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("PORT: need an integer, got %q", raw)
		}
		if n < minPort {
			return nil, fmt.Errorf("PORT: need a value not less than %d (the process runs under an unprivileged UID), got %d", minPort, n)
		}
		port = n
	}

	return &Settings{
		Handler: Config{
			Upstream:    upstream,
			ProxyURL:    proxyURL,
			Clients:     clients,
			LongPollMax: longPoll,
			LogFilter:   NewLogFilter(excluded),
		},
		Addr: fmt.Sprintf(":%d", port),
	}, nil
}
