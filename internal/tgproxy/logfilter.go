package tgproxy

import "strings"

// logErrorThreshold — статус, начиная с которого запрос логируется независимо
// от списка исключений.
//
// Порог 400, а не «строго 2xx»: перенаправление отказом не является, и ломать
// из-за 3xx тишину по getUpdates было бы неожиданно. Практически Bot API
// отвечает 200 даже на свои прикладные ошибки ({"ok":false}), так что 3xx —
// редкий случай, но правило должно быть однозначным.
const logErrorThreshold = 400

// LogFilter решает, попадает ли запрос в access-лог.
//
// Фильтр действует ТОЛЬКО на логи: метрики и трейсы считают все запросы всегда.
// Иначе тишина по основному методу сервиса означала бы и слепоту по нему, а
// логи вдобавок не переживают перезапуск пода — Loki в платформе нет.
type LogFilter struct {
	// excluded — нормализованные имена методов, чьи успехи не логируются.
	excluded map[string]struct{}
}

// NewLogFilter собирает фильтр. Имена приводятся к нижнему регистру, потому что
// имена методов Bot API регистронезависимы.
func NewLogFilter(methods []string) *LogFilter {
	excluded := make(map[string]struct{}, len(methods))
	for _, m := range methods {
		excluded[strings.ToLower(m)] = struct{}{}
	}
	return &LogFilter{excluded: excluded}
}

// ShouldLog сообщает, писать ли строку access-лога.
//
// Имя метода ожидается уже нормализованным — таким, каким его вернул Method.
//
// Нулевой приёмник логирует всё: забытая инициализация должна давать шум, а не
// тишину. Тихая слепота обнаруживается тогда, когда логи уже нужны.
func (f *LogFilter) ShouldLog(method string, status int) bool {
	if status >= logErrorThreshold {
		return true
	}
	if f == nil {
		return true
	}
	_, excluded := f.excluded[method]
	return !excluded
}
