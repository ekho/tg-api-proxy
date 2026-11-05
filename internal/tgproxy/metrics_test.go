package tgproxy

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	nooptrace "go.opentelemetry.io/otel/trace/noop"
)

// collectMetrics снимает показания и возвращает имена собранных метрик с их
// атрибутами.
func collectMetrics(t *testing.T, reader *sdkmetric.ManualReader) map[string][]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	out := map[string][]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = append(out[m.Name], m)
		}
	}
	return out
}

// TestBotLabel — исчерпывающая матрица по всем восьми комбинациям трёх
// признаков {client, pathParsed, botDenied}.
//
// Часть комбинаций недостижима через реальный проход запроса по core (см.
// proxy.go): client выставляется сразу после успешного Lookup, до разбора
// пути в core, а botDenied — только после того, как ParseBotID внутри core
// же и Client.Allows успели отработать. Поэтому botDenied=true при
// pathParsed=false, а также botDenied=true или pathParsed=true при пустом
// client, в проде не возникают: 401 обрывает обработку раньше, чем core
// доходит до разбора пути и проверки бота. Ожидание задано для каждой
// комбинации явно, включая недостижимые: именно непокрытая комбинация
// {client: "", pathParsed: true, botDenied: false} — валидный по форме путь
// без предъявленного ключа — была дырой, через которую анонимный клиент
// управлял лейблом bot произвольно.
func TestBotLabel(t *testing.T) {
	const botID = "123456"

	tests := []struct {
		name       string
		client     string
		pathParsed bool
		botDenied  bool
		want       string
	}{
		{
			name: "аноним, путь не разобран, бот не при делах",
			want: botUnknown,
		},
		{
			name:      "аноним, путь не разобран, denied (недостижимо)",
			botDenied: true,
			want:      botUnknown,
		},
		{
			name:       "аноним с валидным по форме путём — дыра, обязана схлопнуться в unknown",
			pathParsed: true,
			want:       botUnknown,
		},
		{
			name:       "аноним, путь разобран, denied (недостижимо)",
			pathParsed: true,
			botDenied:  true,
			want:       botUnknown,
		},
		{
			name:   "ключ верный, путь не разобран (400)",
			client: testClientName,
			want:   botUnknown,
		},
		{
			name:      "ключ верный, путь не разобран, denied (недостижимо)",
			client:    testClientName,
			botDenied: true,
			want:      botUnknown,
		},
		{
			name:       "ключ верный, бот принадлежит клиенту — лейбл как есть",
			client:     testClientName,
			pathParsed: true,
			want:       botID,
		},
		{
			name:       "ключ верный, бот принадлежит другому клиенту — denied",
			client:     testClientName,
			pathParsed: true,
			botDenied:  true,
			want:       botDeniedLabel,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := &obsCtx{
				botID:      botID,
				pathParsed: tt.pathParsed,
				info:       &reqInfo{client: tt.client, botDenied: tt.botDenied},
			}
			if got := o.botLabel(); got != tt.want {
				t.Errorf("botLabel() при client=%q pathParsed=%v botDenied=%v = %q, want %q",
					tt.client, tt.pathParsed, tt.botDenied, got, tt.want)
			}
		})
	}
}

func TestMetricsRecordBothInstruments(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	m, err := newMetrics(mp)
	if err != nil {
		t.Fatalf("newMetrics: %v", err)
	}

	o := &obsCtx{
		method:     "getme",
		botID:      "123456",
		pathParsed: true,
		start:      time.Now(),
		info:       &reqInfo{},
	}
	m.record(context.Background(), o, 200)

	got := collectMetrics(t, reader)
	for _, name := range []string{"tgproxy.requests", "tgproxy.request.duration"} {
		if _, ok := got[name]; !ok {
			t.Errorf("метрика %q не собрана; собраны: %v", name, keys(got))
		}
	}
}

// TestRecordIgnoresLogFilter закрывает пункт брифа «метрики считают ВСЕ
// запросы всегда, независимо от LOG_EXCLUDE_METHODS»: исключённый из лога
// метод с успешным статусом не должен оставить прокси слепым по объёму и
// латентности.
func TestRecordIgnoresLogFilter(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m, err := newMetrics(mp)
	if err != nil {
		t.Fatalf("newMetrics: %v", err)
	}

	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buf, nil))
	// client выставляется вручную, как это делает core в proxy.go
	// после успешного Lookup: без этого methodLabel() увидел бы
	// анонимный запрос и вернул бы "" вместо реального имени метода.
	h := Observe(logger, NewLogFilter([]string{"getUpdates"}), m, nooptrace.NewTracerProvider().Tracer("test"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if info := infoFrom(r.Context()); info != nil {
			info.client = testClientName
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
	}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/bot"+tok+"/getUpdates", nil))

	// Проверяется отсутствие строки ACCESS-лога, а не пустота буфера: разовую
	// диагностику края фильтр методов намеренно не касается.
	if got := logLines(t, buf); len(got) != 0 {
		t.Fatalf("фильтр должен молчать на успешном исключённом методе, а лог получил: %v", got)
	}

	metricsList, ok := collectMetrics(t, reader)["tgproxy.requests"]
	if !ok || len(metricsList) == 0 {
		t.Fatal("tgproxy.requests не собран: фильтр логов не должен влиять на метрику")
	}
	data, ok := metricsList[0].Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("Data имеет тип %T, want metricdata.Sum[int64]", metricsList[0].Data)
	}
	if len(data.DataPoints) != 1 {
		t.Fatalf("точек данных %d, want 1", len(data.DataPoints))
	}
	dp := data.DataPoints[0]
	if dp.Value != 1 {
		t.Errorf("значение счётчика = %d, want 1", dp.Value)
	}
	if v, ok := dp.Attributes.Value(attribute.Key("method")); !ok || v.AsString() != "getupdates" {
		t.Errorf("атрибут method = %v (ok=%v), want getupdates", v, ok)
	}
	if v, ok := dp.Attributes.Value(attribute.Key("status")); !ok || v.AsInt64() != 200 {
		t.Errorf("атрибут status = %v (ok=%v), want 200", v, ok)
	}
}

// TestRecordUsesDeniedLabelNotBotID проверяет ограничение кардинальности
// сквозным путём: хендлер выставляет info.botDenied ровно так же, как это
// делает core в proxy.go при отказе allowlist'а, а не через прямое
// конструирование obsCtx, как в TestBotLabel.
func TestRecordUsesDeniedLabelNotBotID(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m, err := newMetrics(mp)
	if err != nil {
		t.Fatalf("newMetrics: %v", err)
	}

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := Observe(logger, nil, m, nooptrace.NewTracerProvider().Tracer("test"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if info := infoFrom(r.Context()); info != nil {
			// В реальном проходе по core (proxy.go) botDenied выставляется
			// только после успешного Lookup — принадлежность бота
			// проверяется уже известному клиенту.
			info.client = testClientName
			info.botDenied = true
		}
		w.WriteHeader(http.StatusForbidden)
	}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/bot"+tok+"/getMe", nil))

	metricsList, ok := collectMetrics(t, reader)["tgproxy.requests"]
	if !ok || len(metricsList) == 0 {
		t.Fatal("tgproxy.requests не собран")
	}
	data, ok := metricsList[0].Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("Data имеет тип %T, want metricdata.Sum[int64]", metricsList[0].Data)
	}
	if len(data.DataPoints) != 1 {
		t.Fatalf("точек данных %d, want 1", len(data.DataPoints))
	}
	v, ok := data.DataPoints[0].Attributes.Value(attribute.Key("bot"))
	if !ok {
		t.Fatal("атрибут bot отсутствует")
	}
	if v.AsString() != botDeniedLabel {
		t.Errorf("bot = %q, want %q: id бота не должен утекать в лейбл с неограниченной кардинальностью",
			v.AsString(), botDeniedLabel)
	}
}

// TestRecordWritesMetricOnPanic закрывает пункт брифа «запись метрики идёт до
// всякой фильтрации и в обеих ветвях — и при панике, и в норме»: без этого
// самый неблагополучный момент сервиса был бы не виден и метрикам, а не
// только логу с выключенным по фильтру методом.
func TestRecordWritesMetricOnPanic(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m, err := newMetrics(mp)
	if err != nil {
		t.Fatalf("newMetrics: %v", err)
	}

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := Observe(logger, nil, m, nooptrace.NewTracerProvider().Tracer("test"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("что-то сломалось")
	}))

	// Паника происходит до отправки заголовков, поэтому Observe гасит её и
	// возвращается штатно — recover() на уровне теста не нужен (см.
	// TestObserveRecoversPanicBeforeHeaders в observe_test.go).
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/bot"+tok+"/getMe", nil))

	metricsList, ok := collectMetrics(t, reader)["tgproxy.requests"]
	if !ok || len(metricsList) == 0 {
		t.Fatal("tgproxy.requests не собран: паника не должна оставлять метрику слепой")
	}
	data, ok := metricsList[0].Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("Data имеет тип %T, want metricdata.Sum[int64]", metricsList[0].Data)
	}
	if len(data.DataPoints) != 1 {
		t.Fatalf("точек данных %d, want 1", len(data.DataPoints))
	}
	if v, ok := data.DataPoints[0].Attributes.Value(attribute.Key("status")); !ok || v.AsInt64() != http.StatusInternalServerError {
		t.Errorf("атрибут status = %v (ok=%v), want %d", v, ok, http.StatusInternalServerError)
	}
}

// TestRecordHistogramHasNoClampedAttribute закрывает два пункта брифа:
// «атрибут clamped есть только у счётчика, но не у гистограммы» и «гистограмма
// несёт свои базовые атрибуты method/status/bot». Второе не следует из
// первого само по себе: если убрать WithAttributes(base...) из вызова
// duration.Record, гистограмма останется вообще без атрибутов, и проверка
// «clamped отсутствует» останется верной вакуумно, ничего не заметив.
// Последствие потери атрибутов в бою: tgproxy_request_duration_seconds
// превращается в одну неразличимую кривую, где латентность getUpdates
// (до 45 с) смешана с латентностью getMe (десятки миллисекунд).
func TestRecordHistogramHasNoClampedAttribute(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m, err := newMetrics(mp)
	if err != nil {
		t.Fatalf("newMetrics: %v", err)
	}

	o := &obsCtx{
		method:     "getupdates",
		botID:      "123456",
		pathParsed: true,
		start:      time.Now(),
		// client непустой — запрос дошёл до обработки, то есть ключ уже
		// проверен: без этого methodLabel()/botLabel() увидели бы анонимный
		// запрос и лейблы method/bot ниже не совпали бы с o.method/o.botID.
		info: &reqInfo{clamped: true, client: testClientName},
	}
	m.record(context.Background(), o, 200)

	got := collectMetrics(t, reader)

	histList := got["tgproxy.request.duration"]
	if len(histList) == 0 {
		t.Fatal("tgproxy.request.duration не собрана")
	}
	hist, ok := histList[0].Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("Data имеет тип %T, want metricdata.Histogram[float64]", histList[0].Data)
	}
	if len(hist.DataPoints) != 1 {
		t.Fatalf("точек данных %d, want 1", len(hist.DataPoints))
	}
	dp := hist.DataPoints[0]
	if _, ok := dp.Attributes.Value(attribute.Key("clamped")); ok {
		t.Error("clamped не должен попадать в гистограмму: он удвоил бы число серий без пользы для вопроса о длительности")
	}
	// Проверяем не только присутствие атрибутов, но и их значения: тест обязан
	// падать и тогда, когда атрибут есть, но пустой (например, лейбл bot
	// сброшен в ""), а не только когда его нет вовсе.
	if v, ok := dp.Attributes.Value(attribute.Key("method")); !ok || v.AsString() != o.method {
		t.Errorf("атрибут method у гистограммы = %v (ok=%v), want %q: без него латентность разных методов Bot API слилась бы в одну неразличимую кривую",
			v, ok, o.method)
	}
	if v, ok := dp.Attributes.Value(attribute.Key("status")); !ok || v.AsInt64() != 200 {
		t.Errorf("атрибут status у гистограммы = %v (ok=%v), want 200: без него латентность успешных и ошибочных запросов слилась бы в одну кривую",
			v, ok)
	}
	if v, ok := dp.Attributes.Value(attribute.Key("bot")); !ok || v.AsString() != o.botLabel() {
		t.Errorf("атрибут bot у гистограммы = %v (ok=%v), want %q: без него латентность разных ботов слилась бы в одну кривую",
			v, ok, o.botLabel())
	}

	counterList := got["tgproxy.requests"]
	if len(counterList) == 0 {
		t.Fatal("tgproxy.requests не собран")
	}
	sum, ok := counterList[0].Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("Data имеет тип %T, want metricdata.Sum[int64]", counterList[0].Data)
	}
	if v, ok := sum.DataPoints[0].Attributes.Value(attribute.Key("clamped")); !ok || !v.AsBool() {
		t.Errorf("clamped у счётчика = %v (ok=%v), want true", v, ok)
	}
}

// TestNewMetricsUnits закрывает пробел: ни один тест не читал поле Unit у
// собранных метрик. Единицы задаются один раз в newMetrics как соседние
// строковые литералы, и их легко перепутать местами при правке — компилятор
// и тесты на значения атрибутов этого не заметят. Последствие: перепутанные
// единицы нарушают соглашение имён Prometheus — tgproxy_request_duration_seconds
// оказался бы с единицей "1", а безразмерный tgproxy_requests — с "s".
func TestNewMetricsUnits(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m, err := newMetrics(mp)
	if err != nil {
		t.Fatalf("newMetrics: %v", err)
	}

	// Unit — свойство инструмента, а не конкретного измерения; запись нужна
	// только чтобы оба инструмента наверняка попали в срез collectMetrics.
	o := &obsCtx{
		method:     "getme",
		botID:      "123456",
		pathParsed: true,
		start:      time.Now(),
		info:       &reqInfo{},
	}
	m.record(context.Background(), o, 200)

	got := collectMetrics(t, reader)

	requests, ok := got["tgproxy.requests"]
	if !ok || len(requests) == 0 {
		t.Fatal("tgproxy.requests не собран")
	}
	if requests[0].Unit != "1" {
		t.Errorf("Unit у tgproxy.requests = %q, want %q: счётчик обращений должен остаться безразмерным",
			requests[0].Unit, "1")
	}

	duration, ok := got["tgproxy.request.duration"]
	if !ok || len(duration) == 0 {
		t.Fatal("tgproxy.request.duration не собрана")
	}
	if duration[0].Unit != "s" {
		t.Errorf("Unit у tgproxy.request.duration = %q, want %q: без секунд tgproxy_request_duration_seconds нарушит соглашение имён Prometheus",
			duration[0].Unit, "s")
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestMetricsCarryClientLabel — лейбл client есть на обоих инструментах и
// схлопывается в unknown до аутентификации.
func TestMetricsCarryClientLabel(t *testing.T) {
	tests := []struct {
		name   string
		client string
		want   string
	}{
		{name: "клиент известен", client: testClientName, want: testClientName},
		{name: "аноним", client: "", want: clientUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := sdkmetric.NewManualReader()
			mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
			m, err := newMetrics(mp)
			if err != nil {
				t.Fatalf("newMetrics: %v", err)
			}

			o := &obsCtx{
				method:     "getme",
				botID:      "123456",
				pathParsed: true,
				start:      time.Now(),
				info:       &reqInfo{client: tt.client},
			}
			m.record(context.Background(), o, 200)

			got := collectMetrics(t, reader)
			for _, name := range []string{"tgproxy.requests", "tgproxy.request.duration"} {
				metrics, ok := got[name]
				if !ok {
					t.Fatalf("метрика %q не собрана; собраны: %v", name, keys(got))
				}
				if !hasClientAttr(t, metrics, tt.want) {
					t.Errorf("метрика %q без лейбла client=%q", name, tt.want)
				}
			}
		})
	}
}

// hasClientAttr ищет среди точек метрики хотя бы одну с ожидаемым client.
func hasClientAttr(t *testing.T, metrics []metricdata.Metrics, want string) bool {
	t.Helper()
	found := false
	for _, m := range metrics {
		var sets []attribute.Set
		switch data := m.Data.(type) {
		case metricdata.Sum[int64]:
			for _, dp := range data.DataPoints {
				sets = append(sets, dp.Attributes)
			}
		case metricdata.Histogram[float64]:
			for _, dp := range data.DataPoints {
				sets = append(sets, dp.Attributes)
			}
		default:
			t.Fatalf("неожиданный тип данных метрики %q: %T", m.Name, m.Data)
		}
		for _, set := range sets {
			if v, ok := set.Value(attribute.Key("client")); ok && v.AsString() == want {
				found = true
			}
		}
	}
	return found
}
