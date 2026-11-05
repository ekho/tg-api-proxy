package tgproxy

import "testing"

func TestIsLongPoll(t *testing.T) {
	tests := []struct {
		name string
		path string
		want bool
	}{
		{
			name: "getUpdates",
			path: "/bot123456:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw/getUpdates",
			want: true,
		},
		{
			name: "имена методов Bot API регистронезависимы — верхний регистр",
			path: "/bot123456:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw/GETUPDATES",
			want: true,
		},
		{
			name: "имена методов Bot API регистронезависимы — нижний регистр",
			path: "/bot123456:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw/getupdates",
			want: true,
		},
		{
			name: "завершающий слеш",
			path: "/bot123456:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw/getUpdates/",
			want: true,
		},
		{
			name: "другой метод",
			path: "/bot123456:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw/sendMessage",
			want: false,
		},
		{
			name: "метод с похожим префиксом",
			path: "/bot123456:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw/getUpdatesFoo",
			want: false,
		},
		{
			name: "file-путь методом не является",
			path: "/file/bot123456:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw/getUpdates",
			want: false,
		},
		{
			name: "без метода",
			path: "/bot123456:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw/",
			want: false,
		},
		{
			name: "не путь Bot API",
			path: "/healthz",
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsLongPoll(tt.path); got != tt.want {
				t.Errorf("IsLongPoll(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}
