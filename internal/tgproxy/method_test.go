package tgproxy

import "testing"

// tok — реалистичный по форме токен: bot_id, двоеточие, 35 символов секрета.
const tok = "123456:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw"

func TestMethod(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "обычный вызов приводится к нижнему регистру",
			path: "/bot" + tok + "/getMe",
			want: "getme",
		},
		{
			name: "имена методов Bot API регистронезависимы",
			path: "/bot" + tok + "/GETUPDATES",
			want: "getupdates",
		},
		{
			name: "завершающий слеш игнорируется",
			path: "/bot" + tok + "/getUpdates/",
			want: "getupdates",
		},
		{
			name: "сегменты после метода — не метод: такой путь Telegram не обслуживает",
			path: "/bot" + tok + "/getUpdates/extra",
			want: "",
		},
		{
			name: "скачивание файла получает синтетическое имя",
			path: "/file/bot" + tok + "/photos/file_1.jpg",
			want: "file",
		},
		{
			name: "file-путь с методом в остатке всё равно file",
			path: "/file/bot" + tok + "/getUpdates",
			want: "file",
		},
		{
			name: "file-путь без завершающего слеша после токена не считается file",
			path: "/file/bot" + tok,
			want: "",
		},
		{
			name: "без метода",
			path: "/bot" + tok + "/",
			want: "",
		},
		{
			name: "без остатка пути",
			path: "/bot" + tok,
			want: "",
		},
		{
			name: "метод длиннее 32 символов отвергается",
			path: "/bot" + tok + "/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			want: "",
		},
		{
			name: "неалфавитные символы отвергаются",
			path: "/bot" + tok + "/get_updates",
			want: "",
		},
		{
			name: "не путь Bot API",
			path: "/healthz",
			want: "",
		},
		{
			name: "путь без якоря в начале не проходит",
			path: "/x/bot" + tok + "/getMe",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Method(tt.path); got != tt.want {
				t.Errorf("Method(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}
