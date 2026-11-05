package tgproxy

import "net/http"

// recorder запоминает статус и размер ответа, не мешая стримингу.
type recorder struct {
	http.ResponseWriter

	// status — первый записанный статус; 0 означает «WriteHeader не вызывался».
	status int
	// written — байты тела ответа, без заголовков.
	written int64
}

// Обёртка обязана оставаться Flusher'ом: httputil.ReverseProxy делает
// периодический флаш только через этот интерфейс, а Go не наследует методы,
// которые тип не объявил явно. Без проверки на этапе компиляции регрессия
// прошла бы незамеченной: обычные запросы работают, а скачивание файлов и long
// poll начинают отдавать ответ одним куском в конце.
var _ http.Flusher = (*recorder)(nil)

// Обёртка обязана быть проходимой для http.ResponseController: он ищет метод
// Unwrap, чтобы добраться до Hijacker и SetReadDeadline/SetWriteDeadline
// нижнего writer'а, а ReverseProxy пользуется контроллером сам. Без проверки
// на этапе компиляции регрессия прошла бы так же незаметно, как и потеря
// Flusher выше.
var _ interface{ Unwrap() http.ResponseWriter } = (*recorder)(nil)

// WriteHeader запоминает первый статус и пробрасывает его дальше.
// Повторные вызовы игнорируются: net/http на них ругается в лог, а нам нужен
// именно первый — тот, который увидел клиент.
func (r *recorder) WriteHeader(status int) {
	if r.status != 0 {
		return
	}
	if status >= 100 && status <= 199 && status != http.StatusSwitchingProtocols {
		// Информационный ответ не фиксирует статус: net/http допускает
		// последующий WriteHeader с окончательным кодом, и ReverseProxy
		// ретранслирует 1xx через хук Got1xxResponse. Запомнив 100, мы
		// отбросили бы настоящий статус — клиент получил бы 200 вместо 502.
		r.ResponseWriter.WriteHeader(status)
		return
	}
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		// net/http подразумевает 200 при первой записи без WriteHeader.
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.written += int64(n)
	return n, err
}

func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap возвращает обёрнутый writer.
//
// Без этого метода http.ResponseController не видит сквозь обёртку: Hijack и
// SetReadDeadline/SetWriteDeadline упираются в errNotSupported, хотя нижний
// writer их поддерживает. Сейчас это маскируется тем, что *recorder сам
// реализует http.Flusher, поэтому сброс буфера работает и без Unwrap.
func (r *recorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// statusOrOK возвращает статус, который увидел клиент.
//
// Хендлер, не написавший ни заголовков, ни тела, всё равно приводит к 200 с
// пустым телом — иначе такие ответы попали бы в лог со статусом 0.
func (r *recorder) statusOrOK() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}
