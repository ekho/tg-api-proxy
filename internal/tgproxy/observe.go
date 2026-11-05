package tgproxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// maxUALen — предел длины User-Agent в строке лога: длину заголовка задаёт
// клиент, а не мы.
const maxUALen = 128

// reqInfo — то, что нижние слои сообщают middleware о запросе.
//
// Передаётся через контекст, а не через приведение ResponseWriter к *recorder:
// ErrorHandler у httputil.ReverseProxy получает исходящий клон запроса, чей
// контекст унаследован от входящего, поэтому контекст доезжает, а обёртку
// writer'а легко потерять, вставив между слоями ещё одну.
type reqInfo struct {
	// client — имя клиента, чей ключ предъявлен. Выставляется в core
	// (proxy.go) сразу после успешного Lookup, до этого пусто.
	//
	// Отдельного флага «аутентифицирован» нет намеренно: непустое имя
	// эквивалентно успешной проверке ключа, потому что появиться иначе как
	// результатом удачного Lookup оно не может, а два поля об одном факте
	// можно выставить несогласованно.
	//
	// Признак нужен потому, что иначе лейблы bot и method после 401 брались
	// бы из пути и метода, которые разобраны до всех проверок и полностью
	// подконтрольны анонимному клиенту, ключ не предъявившему.
	client string
	// clamped — long poll оборван нашим дедлайном, а не Telegram.
	clamped bool
	// botDenied — бот отвергнут allowlist'ом. Отдельный флаг, а не вывод из
	// статуса 403: такой же статус может прийти и от самого Telegram.
	botDenied bool
	// upstreamErr — причина отказа апстрима. Без неё в логе остался бы только
	// статус 502 без объяснения.
	upstreamErr error
}

// authenticated сообщает, прошла ли проверка ключа. Обёртка над client
// существует ради читаемости мест, которым важен только сам факт.
func (i *reqInfo) authenticated() bool { return i.client != "" }

type reqInfoKey struct{}

// infoFrom достаёт reqInfo из контекста. Возвращает nil, если запрос пришёл в
// обход Observe — например, в юнит-тесте нижнего слоя.
func infoFrom(ctx context.Context) *reqInfo {
	info, _ := ctx.Value(reqInfoKey{}).(*reqInfo)
	return info
}

// obsCtx — состояние наблюдения за одним запросом.
//
// Собрано в одну структуру намеренно: иначе логирование, метрики и атрибуты
// спана принимали бы по девять позиционных параметров, среди которых два
// соседних string (method и botID) — перепутать их было бы нечем поймать.
type obsCtx struct {
	// Вычисляется один раз на входе.
	method     string
	botID      string
	pathParsed bool
	start      time.Time

	// Заполняется по ходу обработки нижними слоями.
	rec  *recorder
	info *reqInfo
}

// newObsCtx разбирает путь один раз за запрос.
func newObsCtx(w http.ResponseWriter, path string) *obsCtx {
	botID, pathParsed := ParseBotID(path)
	return &obsCtx{
		method:     Method(path),
		botID:      botID,
		pathParsed: pathParsed,
		start:      time.Now(),
		rec:        &recorder{ResponseWriter: w},
		info:       &reqInfo{},
	}
}

// botUnknown и botDeniedLabel — значения лейбла bot, не являющиеся id.
//
// Лейбл обязан быть ограничен по кардинальности. Клиент с общим секретом мог бы
// иначе выпускать сколько угодно значений, запрашивая случайные bot_id, поэтому
// отвергнутый allowlist'ом бот скрывается за denied. В лог его id пишется как
// есть: лог не является time series.
const (
	botUnknown     = "unknown"
	botDeniedLabel = "denied"
)

// clientUnknown — значение лейбла client до аутентификации.
//
// Совпадает по значению с botUnknown, но константа отдельная: это разные
// лейблы, и схождение значений здесь случайно.
const clientUnknown = "unknown"

// botLabel выбирает значение лейбла bot с ограниченной кардинальностью.
//
// Признак отказа берётся из reqInfo, а не выводится из статуса 403: такой же
// статус может прийти и от самого Telegram, и тогда лейбл соврал бы.
//
// Проверка authenticated идёт первой и безусловно: путь разбирается в
// newObsCtx до аутентификации, поэтому pathParsed и botID уже известны даже
// анонимному запросу. На 401 проверка allowlist не выполняется вовсе, и
// botDenied остаётся false — без этой проверки анонимный клиент с общим
// секретом, который он не предъявил, управлял бы лейблом произвольно, задавая
// bot_id в пути.
func (o *obsCtx) botLabel() string {
	switch {
	case !o.info.authenticated():
		return botUnknown
	case !o.pathParsed:
		return botUnknown
	case o.info.botDenied:
		return botDeniedLabel
	default:
		return o.botID
	}
}

// methodLabel выбирает значение лейбла/атрибута method с ограниченной
// кардинальностью.
//
// methodNameRe ограничивает только форму имени метода (~26^32 вариантов), а
// не количество. До аутентификации это не защита от кардинальности: пустая
// строка — тот же синтетический случай «метода нет», что уже используется
// для tg.method и spanName при 401/400 с неразобранным путём.
func (o *obsCtx) methodLabel() string {
	if !o.info.authenticated() {
		return ""
	}
	return o.method
}

// clientLabel выбирает значение лейбла/атрибута client.
//
// В отличие от bot, защищать этот лейбл от произвольных значений не нужно: имя
// не приходит из запроса, а берётся из конфигурации по результату удачного
// Lookup, и потому ограничено по кардинальности само по себе.
func (o *obsCtx) clientLabel() string {
	if !o.info.authenticated() {
		return clientUnknown
	}
	return o.info.client
}

// status — статус, который увидел клиент.
//
// При панике до отправки заголовков клиенту уходит 500, и наблюдаемость обязана
// называть тот же статус: иначе лог с метрикой разошлись бы с ответом. Единая
// точка вычисления существует ровно для того, чтобы лог, метрика и спан не
// начали считать статус по-разному.
func (o *obsCtx) status(panicked bool) int {
	if panicked && o.rec.status == 0 {
		return http.StatusInternalServerError
	}
	return o.rec.statusOrOK()
}

// isAbortPanic сообщает, что recover() поймал panic(http.ErrAbortHandler) —
// сигнал, которым ReverseProxy штатно прерывает раздачу ответа, когда
// копирование тела не удалось, а не аварию в нашем коде.
func isAbortPanic(p any) bool {
	err, ok := p.(error)
	return ok && errors.Is(err, http.ErrAbortHandler)
}

// Observe оборачивает хендлер access-логом и защитой от паники.
//
// Пробы /healthz оборачивать нельзя: readinessProbe (каждые 5 с) и
// livenessProbe (каждые 10 с) дают около 18 запросов в минуту на под против
// примерно 3 запросов реального трафика, и в наблюдаемости остались бы почти
// только они. Поэтому /healthz обслуживается снаружи — см. NewHandler.
func Observe(logger *slog.Logger, filter *LogFilter, m *metrics, tracer trace.Tracer, next http.Handler) http.Handler {
	// Once живёт в замыкании, а не в пакете: пакетная переменная сделала бы
	// тесты зависимыми от порядка запуска — первый же из них выжигал бы
	// диагностику для всех остальных.
	var edgeDiag sync.Once

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o := newObsCtx(w, r.URL.Path)

		// Раньше фильтра методов и намеренно мимо него: основной трафик сервиса —
		// getUpdates, он же исключён из лога по умолчанию, поэтому первым
		// запросом пода почти наверняка окажется именно он. Диагностика,
		// уважающая фильтр, не сработала бы практически никогда. На объём лога
		// это не влияет: строка одна на процесс, а не на запрос.
		edgeDiag.Do(func() { logEdgeHeaders(logger, r) })

		// Входящий traceparent извлекается пропагатором, который
		// регистрирует telemetry.Setup, — без этого каждый серверный
		// спан был бы корневым, и трейсы не сшивались бы с вызывающими
		// сервисами.
		//
		// Контекст трассировки принимается и от неаутентифицированных
		// вызовов: секрет проверяется позже, в core (proxy.go), а до
		// этого момента спан всё равно один на весь запрос и называется
		// "unauthorized", лейблы метрик схлопнуты в botUnknown. Худшее
		// следствие — спан, привязанный к выбранному клиентом trace id;
		// это осознанно принятая цена за сшивание трейсов с нашими же
		// сервисами.
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))

		// Спан открывается с временным именем: окончательное зависит от
		// статуса, который известен только после обработки.
		ctx = context.WithValue(ctx, reqInfoKey{}, o.info)
		ctx, span := tracer.Start(ctx, o.method, trace.WithSpanKind(trace.SpanKindServer))
		// ВАЖНО: этот defer регистрируется РАНЬШЕ большого defer'а ниже,
		// поэтому по правилу LIFO выполнится ПОСЛЕ него — то есть после
		// SetName и SetAttributes. Перестановка этих двух defer'ов молча
		// обнулит имя и атрибуты спанов: закрытый спан их уже не принимает.
		defer span.End()
		r = r.WithContext(ctx)

		defer func() {
			p := recover()
			// http.ErrAbortHandler — штатный сигнал ReverseProxy о том, что
			// копирование тела ответа не удалось (клиент отвалился или
			// апстрим оборвал соединение посреди стриминга), а не авария
			// нашего кода. См. isAbortPanic.
			aborted := isAbortPanic(p)
			status := o.status(p != nil)

			// Метрика пишется всегда и до всякой фильтрации: фильтр действует
			// только на логи. Именно это делает тишину по getUpdates безопасной.
			m.record(r.Context(), o, status)

			span.SetName(spanName(o.methodLabel(), status))
			span.SetAttributes(
				// Путь не попадает в атрибуты ни в каком виде: он несёт токен
				// бота. Отсюда и отказ от otelhttp — тот кладёт url.path сам.
				attribute.String("tg.method", o.methodLabel()),
				attribute.String("tg.bot_id", o.botLabel()),
				attribute.String("tg.client", o.clientLabel()),
				attribute.Bool("tg.clamped", o.info.clamped),
				attribute.Int("http.response.status_code", status),
				attribute.String("client.address", clientIP(r)),
			)
			switch {
			case p != nil && !aborted:
				span.SetStatus(codes.Error, "паника в хендлере")
			case status >= 500:
				span.SetStatus(codes.Error, http.StatusText(status))
			}
			if o.info.upstreamErr != nil {
				span.RecordError(o.info.upstreamErr)
			}

			if p != nil {
				streaming := o.rec.status != 0
				if !streaming {
					o.rec.WriteHeader(http.StatusInternalServerError)
				}
				if aborted {
					o.logAborted(logger, r, status)
				} else {
					// Паника логируется в обход фильтра: при обрыве посреди
					// стриминга статус остаётся 200, и фильтр спрятал бы аварию.
					o.log(logger, r, status, p)
				}
				if streaming {
					// Заголовки уже ушли, подменить статус невозможно. Тихо
					// отдать обрезанное тело как валидный ответ хуже, чем
					// оборвать соединение: ErrAbortHandler закрывает его и не
					// добавляет вторую запись в лог от net/http.
					panic(http.ErrAbortHandler)
				}
				return
			}
			if filter.ShouldLog(o.method, status) {
				o.log(logger, r, status, nil)
			}
		}()

		next.ServeHTTP(o.rec, r)
	})
}

// spanName даёт спану читаемое имя даже там, где метода нет.
//
// Синтетические имена живут только в имени спана: атрибут tg.method при 401 и
// 400 остаётся пустым, потому что метода в этих случаях действительно нет. Так
// список трейсов остаётся читаемым, а атрибут не начинает врать.
func spanName(method string, status int) string {
	if method != "" {
		return method
	}
	switch status {
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusBadRequest:
		return "badrequest"
	default:
		return "unknown"
	}
}

// baseLogAttrs собирает поля, общие для обычной строки лога и для строки
// logAborted. Вынесено отдельно, чтобы у двух функций не разъехался набор
// базовых полей.
func (o *obsCtx) baseLogAttrs(r *http.Request, status int) []any {
	attrs := []any{
		"status", status,
		"dur_ms", time.Since(o.start).Milliseconds(),
		"bytes", o.rec.written,
		"client_ip", clientIP(r),
		"ua", truncate(r.UserAgent(), maxUALen),
	}
	// Цепочка целиком — только когда в ней есть что показать сверх client_ip.
	// См. forwardedChain.
	if chain := forwardedChain(r); chain != "" {
		attrs = append(attrs, "forwarded_chain", chain)
	}
	// Только после аутентификации: до неё имени нет, а пустое поле в каждой
	// строке 401 засоряло бы лог.
	if o.info.authenticated() {
		attrs = append(attrs, "client", o.info.client)
	}
	if o.method != "" {
		attrs = append(attrs, "method", o.method)
	}
	if o.pathParsed {
		// Redact маскирует секрет целиком только на путях, прошедших botPathRe:
		// обе регулярки требуют от секрета charset [A-Za-z0-9_-]. Путь,
		// отвергнутый на 401 (проверка секрета идёт раньше разбора), валидации
		// не проходил, и на /bot1:se!cret/getMe в лог попал бы хвост секрета.
		attrs = append(attrs, "bot_id", o.botID, "path", Redact(r.URL.Path))
	}
	if o.info.clamped {
		attrs = append(attrs, "clamped", true)
	}
	if o.info.upstreamErr != nil {
		attrs = append(attrs, "upstream_err", o.info.upstreamErr.Error())
	}
	return attrs
}

// log пишет одну строку access-лога.
func (o *obsCtx) log(logger *slog.Logger, r *http.Request, status int, panicVal any) {
	attrs := o.baseLogAttrs(r, status)
	if panicVal != nil {
		attrs = append(attrs, "panic", fmt.Sprint(panicVal))
	}
	logger.Log(r.Context(), levelFor(status, panicVal), "request", attrs...)
}

// logAborted пишет строку лога для panic(http.ErrAbortHandler) — штатного
// способа ReverseProxy сообщить, что копирование тела ответа не удалось
// (клиент отвалился или апстрим оборвал соединение посреди стриминга).
// Настоящая причина (например, unexpected EOF) уже ушла в stderr через
// ReverseProxy.ErrorLog (см. proxy.go), поэтому здесь не поле panic — оно
// подразумевало бы дефект нашего хендлера — и не уровень ERROR, а WARN и
// отдельное поле aborted.
func (o *obsCtx) logAborted(logger *slog.Logger, r *http.Request, status int) {
	attrs := append(o.baseLogAttrs(r, status), "aborted", true)
	logger.Log(r.Context(), slog.LevelWarn, "request", attrs...)
}

// levelFor выводит уровень записи из статуса ответа.
//
// Так сохраняется семантика прежних точечных логов (403 был Warn, 502 — Error),
// но она больше не прописана вручную в каждой ветви хендлера.
func levelFor(status int, panicVal any) slog.Level {
	switch {
	case panicVal != nil, status >= 500:
		return slog.LevelError
	case status >= 400:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

// clientIP возвращает адрес клиента для диагностики.
//
// Значение берётся из X-Forwarded-For, потому что за nginx-ingress RemoteAddr —
// это адрес пода ingress-контроллера. Доверять ему нельзя: при
// use-forwarded-headers: true входящий заголовок дописывается, а не затирается,
// и левое значение подделывается тривиально. Поэтому поле идёт только в лог, и
// никаких решений — блокировок, лимитов, ветвлений — на нём не строится.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first, _, found := strings.Cut(xff, ",")
		if !found {
			first = xff
		}
		return strings.TrimSpace(first)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// maxChainLen — предел длины X-Forwarded-For в строке лога.
//
// Ограничение по той же причине, что и у maxUALen: если край когда-нибудь
// начнёт дописывать заголовок к клиентскому, а не затирать его, длину цепочки
// начнёт задавать клиент. Реальная цепочка из восьми хопов укладывается
// примерно в 120 байт.
const maxChainLen = 256

// forwardedChain возвращает X-Forwarded-For целиком — но только тогда, когда в
// цепочке больше одного адреса.
//
// Единственный адрес дословно повторяет client_ip, и писать его вторым полем
// значит платить объёмом лога за нулевую информацию. Сейчас край шлёт ровно
// один адрес (свой), поэтому поля в логе не видно; оно появится в тот момент,
// когда перед нами окажется настоящая цепочка — и это ровно тот момент, когда
// на неё нужно смотреть.
func forwardedChain(r *http.Request) string {
	xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For"))
	if !strings.Contains(xff, ",") {
		return ""
	}
	return truncate(xff, maxChainLen)
}

// edgeDiagMsg — msg строки, которой процесс один раз описывает, что край
// сообщает ему об адресе клиента.
const edgeDiagMsg = "edge headers"

// maxHeaderLen — предел длины значения заголовка в диагностике: длину задаёт не
// наш код.
const maxHeaderLen = 256

// addrHeaders — заголовки, чьи ЗНАЧЕНИЯ разрешено писать в лог.
//
// Список намеренно шире того, что шлёт наш ingress: смысл диагностики в том,
// чтобы увидеть в том числе заголовок, которого мы не ждали. Но он именно
// список, а не «всё подряд»: через X-Proxy-Key приходит ключ клиента, и дамп
// всех значений вынес бы его в лог целиком.
var addrHeaders = []string{
	"X-Forwarded-For",
	"X-Real-Ip",
	"X-Original-Forwarded-For",
	"X-Client-Ip",
	"X-Forwarded-Host",
	"X-Forwarded-Proto",
	"True-Client-Ip",
	"Cf-Connecting-Ip",
	"Forwarded",
}

// logEdgeHeaders один раз за процесс записывает, что именно край рассказывает
// нам о клиенте.
//
// Разово, а не на каждый запрос: адресные заголовки проставляет ingress, и для
// всех запросов их набор одинаков — одной выборки хватает, чтобы прочитать
// конфигурацию края, которой мы не владеем (см. спеку наблюдаемости). Перезапуск
// пода перечитывает её заново, поэтому после правок на платформе ответ виден
// сразу, без выката приложения.
//
// Имена заголовков пишутся все, значения — только для addrHeaders. Имя секретом
// не является, и именно список имён покажет заголовок, о котором мы не знали;
// значение же способно оказаться ключом.
func logEdgeHeaders(logger *slog.Logger, r *http.Request) {
	names := make([]string, 0, len(r.Header))
	for name := range r.Header {
		names = append(names, name)
	}
	// Порядок обхода map случаен, а строку сравнивают между перезапусками.
	slices.Sort(names)

	attrs := []any{
		// Адрес пода ingress-контроллера: показывает, кто наш непосредственный
		// сосед, и отличается ли он от того, что край назвал клиентом.
		"remote_addr", r.RemoteAddr,
		"headers", strings.Join(names, " "),
	}
	for _, name := range addrHeaders {
		if v := r.Header.Get(name); v != "" {
			attrs = append(attrs, headerLogKey(name), truncate(v, maxHeaderLen))
		}
	}
	logger.Info(edgeDiagMsg, attrs...)
}

// headerLogKey превращает имя заголовка в имя поля лога: X-Real-Ip → x_real_ip.
func headerLogKey(header string) string {
	return strings.ReplaceAll(strings.ToLower(header), "-", "_")
}

// truncate обрезает строку по границе байтов.
//
// Разрезанная многобайтная руна не портит вывод: JSON-обработчик slog заменяет
// невалидный UTF-8 на U+FFFD. Практически User-Agent состоит из ASCII.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
