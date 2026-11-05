package tgproxy

import "testing"

func TestRedact(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "путь вызова метода",
			in:   "/bot123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw/sendMessage",
			want: "/bot123456789:REDACTED/sendMessage",
		},
		{
			name: "путь скачивания файла",
			in:   "/file/bot123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw/photos/file_1.jpg",
			want: "/file/bot123456789:REDACTED/photos/file_1.jpg",
		},
		{
			name: "токен с дефисами и подчёркиваниями",
			in:   "/bot42:AA-Hdq_TcvCH1vGWJxfSeofSAs0K5PALDs/getMe",
			want: "/bot42:REDACTED/getMe",
		},
		{
			name: "два токена в одной строке",
			in:   "from /bot1:AAAbbbCCC to /bot2:DDDeeeFFF",
			want: "from /bot1:REDACTED to /bot2:REDACTED",
		},
		{
			name: "строка без токена не меняется",
			in:   "/healthz",
			want: "/healthz",
		},
		{
			name: "пустая строка",
			in:   "",
			want: "",
		},
		{
			name: "токен в query-строке тоже маскируется",
			in:   "/bot777:SECRETVALUE/getUpdates?timeout=50",
			want: "/bot777:REDACTED/getUpdates?timeout=50",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Redact(tt.in); got != tt.want {
				t.Errorf("Redact(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
