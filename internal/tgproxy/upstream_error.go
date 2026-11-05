package tgproxy

import (
	"encoding/json"
	"net/http"
)

// upstreamErrorBody — конверт ошибки в форме Telegram Bot API, дополненный полем
// proxy_error.
//
// Форма выбрана ради чужого кода: клиентские библиотеки (python-telegram-bot,
// aiogram, telebot) разбирают именно этот конверт и превращают его в своё
// обычное исключение, так что существующая у потребителя обработка ошибок и
// ретраи продолжают работать без правок. Незнакомое поле proxy_error библиотеки
// игнорируют, зато оно отличает сбой прокси от отказа самого Telegram при
// разборе инцидента.
type upstreamErrorBody struct {
	OK          bool   `json:"ok"`
	ErrorCode   int    `json:"error_code"`
	Description string `json:"description"`
	ProxyError  bool   `json:"proxy_error"`
}

// writeUpstreamErrorBody пишет тело ответа, когда запрос к Telegram не удался.
// Статус и Content-Type уже отправлены вызывающей стороной.
//
// Сама ошибка сюда намеренно не передаётся: она содержит адрес апстрима и
// детали сетевой топологии, которым в ответе клиенту не место. Для диагностики
// она уже записана в лог с замаскированным путём.
func writeUpstreamErrorBody(w http.ResponseWriter) {
	_ = json.NewEncoder(w).Encode(upstreamErrorBody{
		OK:          false,
		ErrorCode:   http.StatusBadGateway,
		Description: "Bad Gateway: proxy could not reach the Telegram API",
		ProxyError:  true,
	})
}
