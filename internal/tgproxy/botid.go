package tgproxy

import "regexp"

// botPathRe разбирает путь Bot API. Якорь ^ обязателен: без него путь вида
// /x/bot123:SECRET/getMe прошёл бы проверку allowlist, хотя Telegram такой
// маршрут не обслуживает.
//
// Две формы пути: /bot<token>/<method> и /file/bot<token>/<file_path>.
//
// Группа "file" — префикс "file/" (не участвует в совпадении для обычных
// вызовов), группа "id" — bot_id. Группы именованные, а не позиционные:
// добавление или перестановка группы в будущем сдвинет числовые индексы в
// FindStringSubmatchIndex молча, без ошибки компиляции, а SubexpIndex по
// имени — нет. Это единственное место, кодирующее грамматику пути: ParseBotID
// и Method — тонкие обёртки над parseBotPath, чтобы не заводить рядом два
// похожих регэкспа, которые неизбежно разъедутся при расширении грамматики
// токена.
var botPathRe = regexp.MustCompile(`^/(?P<file>file/)?bot(?P<id>\d+):[A-Za-z0-9_-]+(?:/|$)`)

// Номера групп вычисляются один раз при инициализации пакета: parseBotPath —
// горячий путь (вызывается на каждый запрос), а SubexpIndex ищет по имени
// линейным перебором.
var (
	botPathFileGroup = botPathRe.SubexpIndex("file")
	botPathIDGroup   = botPathRe.SubexpIndex("id")
)

// parseBotPath разбирает путь Bot API целиком: bot_id, признак file-маршрута,
// остаток пути после токена (то, что идёт после разделителя, на который
// заканчивается совпадение botPathRe, — либо "/", либо конец строки) и признак
// того, что этот разделитель — литеральный "/", а не нулевая ширина "$".
// Различие важно потребителям вроде Method, которым, в отличие от ParseBotID,
// не всё равно, был ли после токена реальный символ "/".
// ok=false — путь не является путём Bot API.
func parseBotPath(path string) (botID string, isFile bool, rest string, sepConsumed bool, ok bool) {
	loc := botPathRe.FindStringSubmatchIndex(path)
	if loc == nil {
		return "", false, "", false, false
	}
	isFile = loc[2*botPathFileGroup] != -1 // группа "file" участвовала в совпадении
	botID = path[loc[2*botPathIDGroup]:loc[2*botPathIDGroup+1]]
	rest = path[loc[1]:] // конец совпадения уже за разделителем "/" (или конец строки)
	// charset токена [A-Za-z0-9_-]+ никогда не заканчивается на '/', поэтому
	// символ '/' непосредственно перед концом совпадения однозначно означает,
	// что взята ветка "/", а не нулевой ширины "$".
	sepConsumed = loc[1] > 0 && path[loc[1]-1] == '/'
	return botID, isFile, rest, sepConsumed, true
}

// ParseBotID достаёт bot_id из пути запроса Bot API.
// Второе значение — false, если путь не является путём Bot API.
func ParseBotID(path string) (string, bool) {
	botID, _, _, _, ok := parseBotPath(path)
	return botID, ok
}
