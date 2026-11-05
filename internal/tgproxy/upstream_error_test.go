package tgproxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestUpstreamFailureReturnsTelegramShapedError(t *testing.T) {
	// Порт 1 гарантированно никем не слушается — соединение не установится.
	h := newTestHandler(t, "http://127.0.0.1:1", time.Second)

	req := httptest.NewRequest(http.MethodPost, "/bot"+testToken+"/sendMessage", nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("код = %d, want %d", rec.Code, http.StatusBadGateway)
	}

	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var body struct {
		OK          bool   `json:"ok"`
		ErrorCode   int    `json:"error_code"`
		Description string `json:"description"`
		ProxyError  bool   `json:"proxy_error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("тело не разбирается как JSON: %v (%q)", err, rec.Body.String())
	}

	t.Run("конверт Telegram — библиотеки клиентов разберут его как обычную ошибку", func(t *testing.T) {
		if body.OK {
			t.Error("ok = true, want false")
		}
		if body.ErrorCode != http.StatusBadGateway {
			t.Errorf("error_code = %d, want %d", body.ErrorCode, http.StatusBadGateway)
		}
		if body.Description == "" {
			t.Error("description пуст — клиент не поймёт, что произошло")
		}
	})

	t.Run("своё поле отличает сбой прокси от отказа Telegram", func(t *testing.T) {
		if !body.ProxyError {
			t.Error("proxy_error = false, want true")
		}
	})

	t.Run("детали сетевой топологии не утекают клиенту", func(t *testing.T) {
		for _, leak := range []string{"127.0.0.1", "dial", "tcp"} {
			if strings.Contains(strings.ToLower(body.Description), leak) {
				t.Errorf("description содержит %q: %q", leak, body.Description)
			}
		}
	})
}
