package tgproxy

import "testing"

func TestLogFilterShouldLog(t *testing.T) {
	tests := []struct {
		name     string
		excluded []string
		method   string
		status   int
		want     bool
	}{
		{
			name:     "успех исключённого метода молчит",
			excluded: []string{"getUpdates"},
			method:   "getupdates",
			status:   200,
			want:     false,
		},
		{
			name:     "перенаправление исключённого метода молчит: 3xx отказом не является",
			excluded: []string{"getUpdates"},
			method:   "getupdates",
			status:   302,
			want:     false,
		},
		{
			name:     "502 исключённого метода логируется всегда",
			excluded: []string{"getUpdates"},
			method:   "getupdates",
			status:   502,
			want:     true,
		},
		{
			name:     "401 логируется, хотя метод неизвестен",
			excluded: []string{"getUpdates"},
			method:   "",
			status:   401,
			want:     true,
		},
		{
			name:     "успех неисключённого метода логируется",
			excluded: []string{"getUpdates"},
			method:   "getme",
			status:   200,
			want:     true,
		},
		{
			name:     "регистр в списке исключений не важен",
			excluded: []string{"GETUPDATES"},
			method:   "getupdates",
			status:   200,
			want:     false,
		},
		{
			name:     "пустой список исключений логирует всё",
			excluded: nil,
			method:   "getupdates",
			status:   200,
			want:     true,
		},
		{
			name:     "несколько исключённых методов",
			excluded: []string{"getUpdates", "getMe"},
			method:   "getme",
			status:   200,
			want:     false,
		},
		{
			name:     "400 исключённого метода логируется: это ровно порог",
			excluded: []string{"getUpdates"},
			method:   "getupdates",
			status:   400,
			want:     true,
		},
		{
			name:     "399 исключённого метода молчит: на единицу ниже порога",
			excluded: []string{"getUpdates"},
			method:   "getupdates",
			status:   399,
			want:     false,
		},
		{
			name:     "ShouldLog не нормализует сам: ненормализованное имя не совпадёт со списком",
			excluded: []string{"getUpdates"},
			method:   "GetUpdates",
			status:   200,
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := NewLogFilter(tt.excluded)
			if got := f.ShouldLog(tt.method, tt.status); got != tt.want {
				t.Errorf("ShouldLog(%q, %d) = %v, want %v", tt.method, tt.status, got, tt.want)
			}
		})
	}
}

func TestLogFilterNilLogsEverything(t *testing.T) {
	var f *LogFilter
	if !f.ShouldLog("getupdates", 200) {
		t.Error("nil-фильтр обязан логировать всё: иначе забытая инициализация даст тихую слепоту")
	}
}
