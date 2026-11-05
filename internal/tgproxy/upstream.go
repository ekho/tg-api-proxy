package tgproxy

import (
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// upstreamTransport оборачивает вызов Telegram дочерним спаном.
//
// Спан показывает, какая доля времени запроса принадлежит Telegram, а какая нам,
// — на сервисе, где основной метод висит в long poll до 45 секунд, это
// единственный способ отличить «Telegram молчит» от «мы тормозим».
type upstreamTransport struct {
	base   http.RoundTripper
	tracer trace.Tracer
}

func (t *upstreamTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	ctx, span := t.tracer.Start(r.Context(), "upstream", trace.WithSpanKind(trace.SpanKindClient))
	defer span.End()

	// Контекст трассировки в запрос к Telegram НЕ инжектится — по той же
	// причине, по которой туда не уходит X-Forwarded-For: телеметрия наших
	// потребителей третьей стороне не нужна. Обёртка лишь переносит контекст
	// внутрь нашего процесса.
	resp, err := t.base.RoundTrip(r.WithContext(ctx))
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		return nil, err
	}
	span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
	if resp.StatusCode >= 500 {
		span.SetStatus(codes.Error, http.StatusText(resp.StatusCode))
	}
	return resp, nil
}
