package tgproxy

import "testing"

func TestParseBotID(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		wantID string
		wantOK bool
	}{
		{
			name:   "вызов метода",
			path:   "/bot123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw/sendMessage",
			wantID: "123456789",
			wantOK: true,
		},
		{
			name:   "скачивание файла",
			path:   "/file/bot123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw/photos/file_1.jpg",
			wantID: "123456789",
			wantOK: true,
		},
		{
			name:   "токен с дефисами и подчёркиваниями",
			path:   "/bot42:AA-Hdq_TcvCH1vGWJxfSeofSAs0K5PALDs/getMe",
			wantID: "42",
			wantOK: true,
		},
		{
			name:   "healthz — не путь Bot API",
			path:   "/healthz",
			wantOK: false,
		},
		{
			name:   "корень",
			path:   "/",
			wantOK: false,
		},
		{
			name:   "нет идентификатора",
			path:   "/bot/sendMessage",
			wantOK: false,
		},
		{
			name:   "нечисловой идентификатор",
			path:   "/botabc:AAHdqTcv/getMe",
			wantOK: false,
		},
		{
			name:   "нет двоеточия",
			path:   "/bot123/sendMessage",
			wantOK: false,
		},
		{
			name:   "пустой секрет",
			path:   "/bot123:/sendMessage",
			wantOK: false,
		},
		{
			name:   "префикс не в начале пути не считается",
			path:   "/x/bot123:AAHdqTcv/getMe",
			wantOK: false,
		},
		{
			name:   "file без бота",
			path:   "/file/photos/file_1.jpg",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotID, gotOK := ParseBotID(tt.path)
			if gotOK != tt.wantOK {
				t.Fatalf("ParseBotID(%q) ok = %v, want %v", tt.path, gotOK, tt.wantOK)
			}
			if gotID != tt.wantID {
				t.Errorf("ParseBotID(%q) id = %q, want %q", tt.path, gotID, tt.wantID)
			}
		})
	}
}
