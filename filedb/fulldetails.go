package filedb

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// allVoicesLower holds precomputed lowercase versions of allVoices to avoid
// repeated strings.ToLower() calls inside UpdateFullDetails hot path.
var allVoicesLower []string

func init() {
	allVoicesLower = make([]string, len(allVoices))
	for i, v := range allVoices {
		allVoicesLower[i] = strings.ToLower(v)
	}
}

// FFStreamLite is the minimal projection of an ffprobe audio stream consumed by
// UpdateFullDetails for voice mining. Defined here (instead of in tracks/) to
// avoid a tracks→filedb→tracks import cycle. The tracks package registers an
// adapter via SetFFProbeLookup at startup.
type FFStreamLite struct {
	CodecType string
	TagsTitle string
}

var ffprobeVoiceLookup func(magnet string, types []string) []FFStreamLite

// SetFFProbeLookup wires an ffprobe stream provider used by UpdateFullDetails
// to mine extra voice studios out of audio stream tags.title (port of C#
// FileDB.cs:537-561). Pass nil to disable. Safe to call once at startup.
func SetFFProbeLookup(fn func(magnet string, types []string) []FFStreamLite) {
	ffprobeVoiceLookup = fn
}

// UpdateFullDetails computes quality, videotype, voices, languages, seasons and size
// for a torrent entry, modifying the map in-place. Port of C# FileDB.updateFullDetails.
//
// Skips processing if the torrent was already fully processed in a previous save
// (quality and _sn are set). New/updated torrents arrive from parsers via ToMap()
// which never sets these fields, so they are always processed.
func UpdateFullDetails(t TorrentDetails) {
	title := asString(t["title"])
	if title == "" {
		return
	}
	// size is cheap and may need to be recomputed even on already-processed
	// rows (e.g. after fixing a parser bug that produced size=0 with non-empty
	// sizeName). Run before the skip-check below.
	if sz := computeSize(asString(t["sizeName"])); sz > 0 {
		t["size"] = sz
	}
	// Already processed: quality is set and search name is populated.
	if t["quality"] != nil && asString(t["_sn"]) != "" {
		return
	}
	titleLower := strings.ToLower(title)
	trackerName := asString(t["trackerName"])

	// quality
	quality := 480
	if strings.Contains(title, "720p") {
		quality = 720
	} else if strings.Contains(title, "1080p") {
		quality = 1080
	} else if reQ4K.MatchString(titleLower) || strings.Contains(title, "2160p") {
		quality = 2160
	}
	t["quality"] = quality

	// videotype
	videotype := "sdr"
	if (reHDR.MatchString(titleLower) || reHDRBit.MatchString(titleLower)) && !reSDR.MatchString(titleLower) {
		videotype = "hdr"
	}
	t["videotype"] = videotype

	// voices
	voices := map[string]struct{}{}
	if trackerName == "lostfilm" {
		voices["LostFilm"] = struct{}{}
	} else if trackerName == "hdrezka" {
		voices["HDRezka"] = struct{}{}
	}
	if reDub.MatchString(titleLower) {
		voices["Дубляж"] = struct{}{}
	}
	for i, vLow := range allVoicesLower {
		if len(allVoices[i]) > 4 && strings.Contains(titleLower, vLow) {
			voices[allVoices[i]] = struct{}{}
		}
	}
	if ffprobeVoiceLookup != nil {
		magnet := strings.TrimSpace(asString(t["magnet"]))
		if magnet != "" {
			streams := ffprobeVoiceLookup(magnet, asStringSlice(t["types"]))
			for _, s := range streams {
				if s.CodecType != "audio" || s.TagsTitle == "" {
					continue
				}
				tlow := strings.ToLower(s.TagsTitle)
				for i, vLow := range allVoicesLower {
					if len(allVoices[i]) > 4 && strings.Contains(tlow, vLow) {
						voices[allVoices[i]] = struct{}{}
					}
				}
				if reDub.MatchString(tlow) {
					voices["Дубляж"] = struct{}{}
				}
			}
		}
	}
	voiceList := setToSlice(voices)
	if len(voiceList) > 0 {
		t["voices"] = voiceList
	}

	// languages
	langs := map[string]struct{}{}
	if strings.Contains(titleLower, "ukr") || strings.Contains(titleLower, "українськ") ||
		strings.Contains(titleLower, "украинск") || trackerName == "toloka" {
		langs["ukr"] = struct{}{}
	}
	if trackerName == "lostfilm" {
		langs["rus"] = struct{}{}
	}
	if _, hasUkr := langs["ukr"]; !hasUkr {
		for _, v := range ukrVoices {
			if _, ok := voices[v]; ok {
				langs["ukr"] = struct{}{}
				break
			}
		}
	}
	if _, hasRus := langs["rus"]; !hasRus {
		for _, v := range rusVoices {
			if _, ok := voices[v]; ok {
				langs["rus"] = struct{}{}
				break
			}
		}
	}
	langList := setToSlice(langs)
	if len(langList) > 0 {
		t["languages"] = langList
	}

	// seasons
	types := asStringSlice(t["types"])
	if isSerialType(types) {
		seasons := computeSeasons(title)
		if len(seasons) > 0 {
			t["seasons"] = seasons
		}
	}
}

func computeSize(sizeName string) int64 {
	if strings.TrimSpace(sizeName) == "" {
		return 0
	}
	// Some trackers (notably rutracker) emit the size cell with `&nbsp;` between
	// the number and the unit ("15.62&nbsp;GB"). After html.UnescapeString that
	// becomes a Unicode NBSP (U+00A0), and reSizeInfo's literal " " (ASCII
	// 0x20) won't match it — the regex returns no groups and we silently
	// produce size=0. Normalize NBSP to a regular space before matching.
	sizeName = strings.ReplaceAll(sizeName, " ", " ")
	m := reSizeInfo.FindStringSubmatch(sizeName)
	if len(m) < 3 || m[2] == "" {
		return 0
	}
	numStr := strings.ReplaceAll(m[1], ",", ".")
	size, err := strconv.ParseFloat(numStr, 64)
	if err != nil || size == 0 {
		return 0
	}
	unit := strings.ToLower(m[2])
	switch unit {
	case "gb", "гб":
		size *= 1024
	case "tb", "тб":
		size *= 1048576
	}
	return int64(size * 1048576)
}

func isSerialType(types []string) bool {
	for _, t := range types {
		switch t {
		case "serial", "multserial", "docuserial", "tvshow", "anime":
			return true
		}
	}
	return false
}

// computeSeasons runs up to a dozen regex passes per title, so identical
// titles processed across re-parse cycles share a result via seasonCache.
// The cache is bounded; ~10% of entries get evicted (in random map-iteration
// order) when the size cap is hit. Returned slices are shared read-only —
// callers must not mutate them.
const (
	seasonCacheMax  = 50000
	seasonCacheDrop = 5000
)

var (
	seasonCacheMu sync.RWMutex
	seasonCache   = make(map[string][]int, seasonCacheMax)
)

func computeSeasons(title string) []int {
	seasonCacheMu.RLock()
	if v, ok := seasonCache[title]; ok {
		seasonCacheMu.RUnlock()
		return v
	}
	seasonCacheMu.RUnlock()

	out := computeSeasonsUncached(title)

	seasonCacheMu.Lock()
	if _, exists := seasonCache[title]; !exists {
		if len(seasonCache) >= seasonCacheMax {
			i := 0
			for k := range seasonCache {
				delete(seasonCache, k)
				i++
				if i >= seasonCacheDrop {
					break
				}
			}
		}
		seasonCache[title] = out
	}
	seasonCacheMu.Unlock()
	return out
}

func computeSeasonsUncached(title string) []int {
	seasons := map[int]struct{}{}
	ti := title // original case for regex matching (C# uses IgnoreCase)

	if !reSeasonCheck.MatchString(ti) {
		return nil
	}

	// Multi-season range
	if reMultiSeason.MatchString(ti) {
		start, end := 0, 0
		if reNxN.MatchString(ti) {
			if m := reMultiNxN.FindStringSubmatch(ti); len(m) == 3 {
				start, _ = strconv.Atoi(m[1])
				end, _ = strconv.Atoi(m[2])
			}
		} else if reSezonWord.MatchString(ti) {
			if m := reMultiSezon.FindStringSubmatch(ti); len(m) == 3 {
				start, _ = strconv.Atoi(m[1])
				end, _ = strconv.Atoi(m[2])
			}
		} else if reSNum.MatchString(ti) {
			if m := reMultiSNum.FindStringSubmatch(ti); len(m) == 3 {
				start, _ = strconv.Atoi(m[1])
				end, _ = strconv.Atoi(m[2])
			}
		}
		if start > 0 && end > start {
			for s := start; s <= end; s++ {
				seasons[s] = struct{}{}
			}
		}
	}

	// Single season (Russian сезон)
	if reSezonWord.MatchString(ti) {
		if m := reOneSezon.FindStringSubmatch(ti); len(m) == 2 {
			if s, _ := strconv.Atoi(m[1]); s > 0 {
				seasons[s] = struct{}{}
			}
		}
	} else if reSezonColon.MatchString(ti) {
		// сезон(ы|и): start-end
		if reSezonColonRange.MatchString(ti) {
			if m := reSezonColonRangeG.FindStringSubmatch(ti); len(m) == 4 {
				start, _ := strconv.Atoi(m[2])
				end, _ := strconv.Atoi(m[3])
				if start > 0 && end > start {
					for s := start; s <= end; s++ {
						seasons[s] = struct{}{}
					}
				}
			}
		} else {
			if m := reSezonColonOne.FindStringSubmatch(ti); len(m) == 3 {
				if s, _ := strconv.Atoi(m[2]); s > 0 {
					seasons[s] = struct{}{}
				}
			}
		}
	} else if reNxN.MatchString(ti) {
		if m := reOneNxN.FindStringSubmatch(ti); len(m) == 2 {
			if s, _ := strconv.Atoi(m[1]); s > 0 {
				seasons[s] = struct{}{}
			}
		}
	} else if reSNum.MatchString(ti) {
		if m := reOneSNum.FindStringSubmatch(ti); len(m) == 2 {
			if s, _ := strconv.Atoi(m[1]); s > 0 {
				seasons[s] = struct{}{}
			}
		}
	}

	out := make([]int, 0, len(seasons))
	for s := range seasons {
		out = append(out, s)
	}
	return out
}

func setToSlice(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ---- regexps ----

var (
	reQ4K   = regexp.MustCompile(`(4k|uhd)( |\]|,|$)`)
	reHDR   = regexp.MustCompile(`(\[|,| )hdr(10| |\]|,|$)`)
	reHDRBit = regexp.MustCompile(`(10-bit|10 bit|10-бит|10 бит|hdr10)`)
	reSDR   = regexp.MustCompile(`(\[|,| )sdr( |\]|,|$)`)
	reDub   = regexp.MustCompile(`( |x)(d|dub|дб|дуб|дубляж)(,| )`)

	reSizeInfo = regexp.MustCompile(`(?i)([0-9.,]+) (Mb|МБ|GB|ГБ|TB|ТБ)`)

	reSeasonCheck  = regexp.MustCompile(`(?i)([0-9]+(\-[0-9]+)?x[0-9]+|сезон|s[0-9]+)`)
	reMultiSeason  = regexp.MustCompile(`(?i)([0-9]+\-[0-9]+x[0-9]+|[0-9]+\-[0-9]+ сезон|s[0-9]+\-s?[0-9]+)`)
	reNxN          = regexp.MustCompile(`(?i)[0-9]+x[0-9]+`)
	reSezonWord    = regexp.MustCompile(`(?i)[0-9]+ сезон`)
	reSNum         = regexp.MustCompile(`(?i)s[0-9]+`)
	reMultiNxN     = regexp.MustCompile(`(?i)([0-9]+)\-([0-9]+)x`)
	reMultiSezon   = regexp.MustCompile(`(?i)([0-9]+)\-([0-9]+) сезон`)
	reMultiSNum    = regexp.MustCompile(`(?i)s([0-9]+)\-s?([0-9]+)`)
	reOneSezon     = regexp.MustCompile(`(?i)([0-9]+) сезон`)
	reSezonColon      = regexp.MustCompile(`(?i)сезон(ы|и)?:? [0-9]+`)
	reSezonColonRange = regexp.MustCompile(`(?i)сезон(ы|и)?:? [0-9]+\-[0-9]+`)
	reSezonColonRangeG = regexp.MustCompile(`(?i)сезон(ы|и)?:? ([0-9]+)\-([0-9]+)`)
	reSezonColonOne = regexp.MustCompile(`(?i)сезон(ы|и)?:? ([0-9]+)`)
	reOneNxN       = regexp.MustCompile(`(?i)([0-9]+)x`)
	reOneSNum      = regexp.MustCompile(`(?i)s([0-9]+)`)
)

// ---- voice lists (ported from C# FileDB.cs) ----

var allVoices = []string{"Ozz", "Laci", "Kerob", "LE-Production", "Parovoz Production", "Paradox", "Omskbird", "LostFilm", "Причудики", "BaibaKo", "NewStudio", "AlexFilm", "FocusStudio", "Gears Media", "Jaskier", "ViruseProject", "Кубик в Кубе", "IdeaFilm", "Sunshine Studio", "Ozz.tv", "Hamster Studio", "Сербин", "To4ka", "Кравец", "Victory-Films", "SNK-TV", "GladiolusTV", "Jetvis Studio", "ApofysTeam", "ColdFilm", "Agatha Studdio", "KinoView", "Jimmy J.", "Shadow Dub Project", "Amedia", "Red Media", "Selena International", "Гоблин", "Universal Russia", "Kiitos", "Paramount Comedy", "Кураж-Бамбей", "Студия Пиратского Дубляжа", "Чадов", "Карповский", "RecentFilms", "Первый канал", "Alternative Production", "NEON Studio", "Колобок", "Дольский", "Синема УС", "Гаврилов", "Живов", "SDI Media", "Алексеев", "GreenРай Studio", "Михалев", "Есарев", "Визгунов", "Либергал", "Кузнецов", "Санаев", "ДТВ", "Дохалов", "Горчаков", "LevshaFilm", "CasStudio", "Володарский", "Шварко", "Карцев", "ETV+", "ВГТРК", "Gravi-TV", "1001cinema", "Zone Vision Studio", "Хихикающий доктор", "Murzilka", "turok1990", "FOX", "STEPonee", "Elrom", "HighHopes", "SoftBox", "NovaFilm", "Четыре в квадрате", "Greb&Creative", "MUZOBOZ", "ZM-Show", "Kerems13", "New Dream Media", "Игмар", "Котов", "DeadLine Studio", "РенТВ", "Андрей Питерский", "Fox Life", "Рыбин", "Trdlo.studio", "Studio Victory Аsia", "Ozeon", "НТВ", "CP Digital", "AniLibria", "Levelin", "FanStudio", "Cmert", "Интерфильм", "SunshineStudio", "Kulzvuk Studio", "Кашкин", "Вартан Дохалов", "Немахов", "Sedorelli", "СТС", "Яроцкий", "ICG", "ТВЦ", "Штейн", "AzOnFilm", "SorzTeam", "Гаевский", "Мудров", "Воробьев Сергей", "Студия Райдо", "DeeAFilm Studio", "zamez", "Иванов", "СВ-Дубль", "BadBajo", "Комедия ТВ", "Мастер Тэйп", "5-й канал СПб", "Гланц", "Ох! Студия", "СВ-Кадр", "2x2", "Котова", "Позитив", "RusFilm", "Назаров", "XDUB Dorama", "Реальный перевод", "Kansai", "Sound-Group", "Николай Дроздов", "ZEE TV", "MTV", "Сыендук", "GoldTeam", "Белов", "Dream Records", "Яковлев", "Vano", "SilverSnow", "Lord32x", "Filiza Studio", "Sony Sci-Fi", "Flux-Team", "NewStation", "DexterTV", "Good People", "AniDUB", "SHIZA Project", "AniLibria.TV", "StudioBand", "AniMedia", "Onibaku", "JWA Project", "MC Entertainment", "Oni", "Jade", "Ancord", "ANIvoice", "Nika Lenina", "Bars MacAdams", "JAM", "Anika", "Berial", "Kobayashi", "Cuba77", "RiZZ_fisher", "OSLIKt", "Lupin", "Ryc99", "Nazel & Freya", "Trina_D", "JeFerSon", "Vulpes Vulpes", "Hamster", "KinoGolos", "Fox Crime", "Денис Шадинский", "AniFilm", "Rain Death", "New Records", "Первый ТВЧ", "RG.Paravozik", "Profix Media", "Tycoon", "RealFake", "HDRezka", "Discovery", "Viasat History", "HiWayGrope", "GREEN TEA", "AlphaProject", "AnimeReactor", "Animegroup", "Shachiburi", "Persona99", "3df voice", "CactusTeam", "AniMaunt", "ShinkaDan", "ShowJet", "RAIM", "АрхиТеатр", "Project Web Mania", "ko136", "КураСгречей", "AMS", "СВ-Студия", "Храм Дорам ТВ", "TurkStar", "Медведев", "Рябов", "BukeDub", "FilmGate", "FilmsClub", "Sony Turbo", "AXN Sci-Fi", "DIVA Universal", "Курдов", "Неоклассика", "fiendover", "SomeWax", "Логинофф", "Cartoon Network", "Loginoff", "CrezaStudio", "Воротилин", "LakeFilms", "Andy", "XDUB Dorama + Колобок", "KosharaSerials", "Екатеринбург Арт", "Julia Prosenuk", "АРК-ТВ Studio", "Т.О Друзей", "Animedub", "Paramount Channel", "Кириллица", "AniPLague", "Видеосервис", "JoyStudio", "TVShows", "GostFilm", "West Video", "Формат AB", "Film Prestige", "SovetRomantica", "РуФилмс", "AveBrasil", "BTI Studios", "Пифагор", "Eurochannel", "Кармен Видео", "Кошкин", "Rainbow World", "Варус-Видео", "ClubFATE", "HiWay Grope", "Banyan Studio", "Mallorn Studio", "Asian Miracle Group", "Эй Би Видео", "AniStar", "Korean Craze", "Невафильм", "Hallmark", "Sony Channel", "East Dream", "Bonsai Studio", "Lucky Production", "Octopus", "TUMBLER Studio", "CrazyCatStudio", "Amber", "Train Studio", "Анастасия Гайдаржи", "Мадлен Дюваль", "Sound Film", "Cowabunga Studio", "Фильмэкспорт", "VO-Production", "Nickelodeon", "MixFilm", "Back Board Cinema", "Кирилл Сагач", "Stevie", "OnisFilms", "MaxMeister", "Syfy Universal", "Neo-Sound", "Муравский", "Рутилов", "Тимофеев", "Лагута", "Дьяконов", "Voice Project", "VoicePower", "StudioFilms", "Elysium", "BeniAffet", "Paul Bunyan", "CoralMedia", "Кондор", "ViP Premiere", "FireDub", "AveTurk", "Янкелевич", "Киреев", "Багичев", "Лексикон", "Нота", "Arisu", "Superbit", "AveDorama", "VideoBIZ", "Киномания", "DDV", "WestFilm", "Анастасия Гайдаржи + Андрей Юрченко", "VSI Moscow", "Horizon Studio", "Flarrow Films", "Amazing Dubbing", "Видеопродакшн", "VGM Studio", "FocusX", "CBS Drama", "Novamedia", "Дасевич", "Анатолий Гусев", "Twister", "Морозов", "NewComers", "kubik&ko", "DeMon", "Анатолий Ашмарин", "Inter Video", "Пронин", "AMC", "Велес", "Volume-6 Studio", "Хоррор Мэйкер", "Ghostface", "Sephiroth", "Акира", "Деваль Видео", "RussianGuy27", "neko64", "Shaman", "Franek Monk", "Ворон", "Andre1288", "GalVid", "Другое кино", "Студия NLS", "Sam2007", "HaseRiLLoPaW", "Севастьянов", "D.I.M.", "Марченко", "Журавлев", "Н-Кино", "Lazer Video", "SesDizi", "Рудой", "Товбин", "Сергей Дидок", "Хуан Рохас", "binjak", "Карусель", "Lizard Cinema", "Акцент", "Max Nabokov", "Barin101", "Васька Куролесов", "Фортуна-Фильм", "Amalgama", "AnyFilm", "Козлов", "Zoomvision Studio", "Urasiko", "VIP Serial HD", "НСТ", "Кинолюкс", "Завгородний", "AB-Video", "Universal Channel", "Wakanim", "SnowRecords", "С.Р.И", "Старый Бильбо", "Mystery Film", "Латышев", "Ващенко", "Лайко", "Сонотек", "Psychotronic", "Gremlin Creative Studio", "Нева-1", "Максим Жолобов", "Мобильное телевидение", "IVI", "DoubleRec", "Milvus", "RedDiamond Studio", "Astana TV", "Никитин", "КТК", "D2Lab", "Black Street Records", "Останкино", "TatamiFilm", "Видеобаза", "Crunchyroll", "RedRussian1337", "КонтентикOFF", "Creative Sound", "HelloMickey Production", "Пирамида", "CLS Media", "Сонькин", "Garsu Pasaulis", "Gold Cinema", "Че!", "Нарышкин", "Intra Communications", "Кипарис", "Королёв", "visanti-vasaer", "Готлиб", "диктор CDV", "Pazl Voice", "Прямостанов", "Zerzia", "MGM", "Дьяков", "Вольга", "Дубровин", "МИР", "Jetix", "RUSCICO", "Seoul Bay", "Филонов", "Махонько", "Строев", "Саня Белый", "Говинда Рага", "Ошурков", "Horror Maker", "Хлопушка", "Хрусталев", "Антонов Николай", "Золотухин", "АрхиАзия", "Попов", "Ultradox", "Мост-Видео", "Альтера Парс", "Огородников", "Твин", "Хабар", "AimaksaLTV", "ТНТ", "FDV", "The Kitchen Russia", "Ульпаней Эльром", "Видеоимпульс", "GoodTime Media", "Alezan", "True Dubbing Studio", "Интер", "Contentica", "Мельница", "ИДДК", "Инфо-фильм", "Мьюзик-трейд", "Кирдин | Stalk", "ДиоНиК", "Стасюк", "TV1000", "Тоникс Медиа", "Бессонов", "Бахурани", "NewDub", "Cinema Prestige", "Набиев", "ТВ3", "Малиновский Сергей", "Кенс Матвей", "Voiz", "Светла", "LDV", "Videogram", "Индия ТВ", "Герусов", "Элегия фильм", "Nastia", "Семыкина Юлия", "Электричка", "Штамп Дмитрий", "Пятница", "Oneinchnales", "Кинопремьера", "Бусов Глеб", "Emslie", "1+1", "100 ТВ", "1001 cinema", "2+2", "2х2", "4u2ges", "5 канал", "A. Lazarchuk", "AAA-Sound", "AdiSound", "ALEKS KV", "Amalgam", "AnimeSpace Team", "AniUA", "AniWayt", "Anything-group", "AOS", "Arasi project", "ARRU Workshop", "AuraFilm", "AvePremier", "Azazel", "BadCatStudio", "BBC Saint-Petersburg", "BD CEE", "Boльгa", "Brain Production", "BraveSound", "Bubble Dubbing Company", "Byako Records", "Cactus Team", "CDV", "CinemaSET GROUP", "CinemaTone", "CPIG", "D1", "datynet", "DeadLine", "DeadSno", "den904", "Description", "Dice", "DniproFilm", "DreamRecords", "DVD Classic", "Eladiel", "Elegia", "ELEKTRI4KA", "Epic Team", "eraserhead", "erogg", "Extrabit", "F-TRAIN", "Family Fan Edition", "Fox Russia", "FoxLife", "Foxlight", "Gala Voices", "Gemini", "General Film", "GetSmart", "Gezell Studio", "Gits", "GoodVideo", "Gramalant", "HamsterStudio", "hungry_inri", "ICTV", "IgVin & Solncekleshka", "ImageArt", "INTERFILM", "Ivnet Cinema", "IНТЕР", "Jakob Bellmann", "Janetta", "jept", "Jetvis", "JimmyJ", "KIHO", "Kinomania", "Kолобок", "L0cDoG", "LeDoyen", "LeXiKC", "Liga HQ", "Line", "Lisitz", "Lizard Cinema Trade", "lord666", "Macross", "madrid", "Marclail", "MCA", "McElroy", "Mega-Anime", "Melodic Voice Studio", "metalrus", "MifSnaiper", "Mikail", "Milirina", "MiraiDub", "MOYGOLOS", "MrRose", "National Geographic", "NemFilm", "Neoclassica", "Nice-Media", "No-Future", "Oghra-Brown", "OpenDub", "Ozz TV", "PaDet", "Paramount Pictures", "PashaUp", "PCB Translate", "PiratVoice", "Postmodern", "Prolix", "QTV", "R5", "Radamant", "RainDeath", "RATTLEBOX", "Reanimedia", "Rebel Voice", "RedDog", "Renegade Team", "RG Paravozik", "RinGo", "RoxMarty", "Rumble", "Saint Sound", "SakuraNight", "Satkur", "Sawyer888", "Sci-Fi Russia", "Selena", "seqw0", "SGEV", "SHIZA", "Sky Voices", "SkyeFilmTV", "SmallFilm", "SOLDLUCK2", "Solod", "SpaceDust", "ssvss", "st.Elrom", "Suzaku", "sweet couple", "TB5", "TF-AniGroup", "The Mike Rec.", "Timecraft", "To4kaTV", "Tori", "Total DVD", "TrainStudio", "Troy", "TV 1000", "Twix", "VashMax2", "VendettA", "VHS", "VicTeam", "VictoryFilms", "Video-BIZ", "VIZ Media", "Voice Project Studio", "VulpesVulpes", "Wayland team", "WiaDUB", "WVoice", "XL Media", "XvidClub Studio", "Zendos", "Zone Studio", "Zone Vision", "Агапов", "Акопян", "Артемьев", "Васильев", "Васильцев", "Григорьев", "Клюквин", "Костюкевич", "Матвеев", "Мишин", "Савченко", "Смирнов", "Толстобров", "Чуев", "Шуваев", "ААА-sound", "АБыГДе", "Акалит", "Альянс", "Амальгама", "АМС", "АнВад", "Анубис", "Anubis", "Арк-ТВ", "Б. Федоров", "Бибиков", "Бигыч", "Бойков", "Абдулов", "Вихров", "Воронцов", "Данилов", "Рукин", "Варус Видео", "Ващенко С.", "Векшин", "Весельчак", "Витя <говорун>", "Войсовер", "Г. Либергал", "Г. Румянцев", "Гей Кино Гид", "ГКГ", "Глуховский", "Гризли", "Гундос", "Деньщиков", "Нурмухаметов", "Пучков", "Шадинский", "Штамп", "sf@irat", "Держиморда", "Домашний", "Е. Гаевский", "Е. Гранкин", "Е. Лурье", "Е. Рудой", "Е. Хрусталёв", "ЕА Синема", "Живаго", "Жучков", "З Ранку До Ночі", "Зебуро", "Зереницын", "И. Еремеев", "И. Клушин", "И. Сафронов", "И. Степанов", "ИГМ", "Имидж-Арт", "Инис", "Ирэн", "Ист-Вест", "К. Поздняков", "К. Филонов", "К9", "Карапетян", "Квадрат Малевича", "Килька", "Королев", "Л. Володарский", "Лазер Видео", "ЛанселаП", "Лапшин", "Ленфильм", "Леша Прапорщик", "Лизард", "Люсьена", "Заугаров", "Иванова и П. Пашут", "Максим Логинофф", "Малиновский", "Машинский", "Медиа-Комплекс", "Мика Бондарик", "Миняев", "Мительман", "Мост Видео", "Мосфильм", "Н. Антонов", "Н. Дроздов", "Н. Золотухин", "Н.Севастьянов seva1988", "Наталья Гурзо", "НЕВА 1", "НеЗупиняйПродакшн", "Несмертельное оружие", "НЛО-TV", "Новый диск", "Новый Дубляж", "НТН", "Оверлорд", "Омикрон", "Парадиз", "Пепелац", "Первый канал ОРТ", "Переводман", "Перец", "Петербургский дубляж", "Петербуржец", "Позитив-Мультимедиа", "Прайд Продакшн", "Премьер Видео", "Премьер Мультимедиа", "Р. Янкелевич", "Райдо", "Ракурс", "Россия", "РТР", "Русский дубляж", "Русский Репортаж", "Рыжий пес", "С. Визгунов", "С. Дьяков", "С. Казаков", "С. Кузнецов", "С. Кузьмичёв", "С. Лебедев", "С. Макашов", "С. Рябов", "С. Щегольков", "С.Р.И.", "Сolumbia Service", "Самарский", "СВ Студия", "Селена Интернешнл", "Синема Трейд", "Синта Рурони", "Синхрон", "Советский", "Сокуров", "Солодухин", "Союз Видео", "Союзмультфильм", "СПД - Сладкая парочка", "Студии Суверенного Лепрозория", "Студия <Стартрек>", "KOleso", "Студия Горького", "Студия Колобок", "Студия Трёх", "Гуртом", "Супербит", "Так Треба Продакшн", "ТВ XXI век", "ТВ СПб", "ТВ-3", "ТВ6", "ТВЧ 1", "ТО Друзей", "Толмачев", "Точка Zрения", "Трамвай-фильм", "ТРК", "Уолт Дисней Компани", "Хихидок", "Цікава ідея", "Швецов", "Ю. Живов", "Ю. Немахов", "Ю. Сербин", "Ю. Товбин", "Я. Беллманн", "RHS", "Red Head Sound", "Postmodern Postproduction", "MelodicVoiceStudio", "FanVoxUA", "UkraineFastDUB", "UFDUB", "CHAS.UA", "Струґачка", "StorieS man", "UATeam", "UkrDub", "UAVoice", "Три крапки", "Сокира", "FlameStudio", "HATOSHI", "SkiDub", "Sengoku", "AdrianZP", "Cikava Ideya", "КiT", "Inter", "NLO", "ТакТребаПродакшн", "Новий Канал", "BambooUA", "Тоніс", "UA-DUB", "ТеТ", "СТБ", "НЛО", "Колодій", "В одне рило", "інтер", "DubLiCat", "AAASound", "НеЗупиняйПродакшн", "Омікрон", "Omicron", "Omikron", "3 крапки", "Tak Treba Production", "TET", "ПлюсПлюс", "Дніпрофільм", "ArtymKo", "Cinemaker", "sweet.tv", "DreamCast"}

var rusVoices = []string{"LostFilm", "Горчаков", "Кириллица", "TVShows", "datynet", "Gears Media", "Ленфильм", "Пифагор", "Jaskier", "Сербин", "Ю. Сербин", "Superbit", "Гланц", "Королев", "Мосфильм", "Яроцкий", "Немахов", "Ю. Немахов", "Визгунов", "Премьер Мультимедиа", "СВ-Дубль", "Рябов", "Яковлев", "Велес", "Хлопушка", "Марченко", "Живов", "Либергал", "Г. Либергал", "Tycoon", "1001cinema", "1001 cinema", "FOX", "LakeFilms", "zamez", "SNK-TV", "С. Визгунов", "SDI Media", "Первый канал", "NewComers", "Гоблин", "Карусель", "Иванов", "Карповский", "Twister", "ДТВ", "ТВЦ", "НТВ", "Королёв", "Ю. Живов", "Видеосервис", "Санаев", "Варус Видео", "Кашкин", "Кубик в Кубе", "Варус-Видео", "BadBajo", "Flarrow Films", "Нева-1", "Пучков", "Pazl Voice", "Есарев", "Завгородний", "Латышев", "Red Head Sound", "Чадов", "Союз Видео", "Film Prestige", "Михалев", "Кравец", "GREEN TEA", "Позитив", "Позитив-Мультимедиа", "ТВ3", "Карцев", "CactusTeam", "Cactus Team", "D2Lab", "Vano", "Воротилин", "Супербит", "С. Рябов", "STEPonee", "DeadSno", "den904", "Back Board Cinema", "AlexFilm", "Рутилов", "Zone Vision", "Смирнов", "Янкелевич", "Колобок", "NewStation", "MUZOBOZ", "Алексеев", "NewStudio", "RusFilm", "2x2", "VO-Production", "Ivnet Cinema", "Володарский", "Дохалов", "Вартан Дохалов", "Медведев", "Amedia", "Novamedia", "TV1000", "TV 1000", "Мика Бондарик", "Amalgama", "Amalgam", "Дьяконов", "Игмар", "ИГМ", "ВГТРК", "Л. Володарский", "Гаевский", "Е. Гаевский", "Garsu Pasaulis", "Кузнецов", "Премьер Видео", "AB-Video", "CP Digital", "Селена Интернешнл", "Махонько", "GoodTime Media", "Рукин", "НСТ", "Филонов", "Деваль Видео", "Екатеринбург Арт", "DoubleRec", "Твин", "Синхрон", "Русский дубляж", "СТС", "ViruseProject", "ТВ-3", "Ворон", "АрхиАзия", "Светла", "Котов", "West Video", "IdeaFilm", "С.Р.И", "С.Р.И.", "Кипарис", "С. Кузнецов", "BTI Studios", "NovaFilm", "Horror Maker", "ТНТ", "Огородников", "Б. Федоров", "ICG", "Solod", "ColdFilm", "ViP Premiere", "CinemaTone", "FDV", "RussianGuy27", "Hamster", "AMS", "РТР", "Багичев", "JimmyJ", "Cinema Prestige", "RHS", "AniMaunt", "Штейн", "Амальгама", "Пирамида", "Н. Антонов", "Товбин", "Ю. Товбин", "Матвеев", "Советский", "Кармен Видео", "Paradox", "ZM-Show", "Saint Sound", "Попов", "GladiolusTV", "RUSCICO", "RealFake", "SesDizi", "Мишин", "Киреев", "Good People", "Мост-Видео", "Сонькин", "AlphaProject", "Останкино", "DDV", "Назаров", "Пронин", "FocusStudio", "Хрусталев", "SomeWax", "Строев", "Дасевич", "Лазер Видео", "К. Поздняков", "Весельчак", "Прямостанов", "Видеопродакшн", "Логинофф", "Максим Логинофф", "Козлов", "Ващенко", "СВ Студия", "ETV+", "диктор CDV", "CDV", "Кондор", "Мост Видео", "Gemini", "Lucky Production", "Дубровин", "ShowJet", "lord666", "Солодухин", "Gravi-TV", "Gramalant", "Акцент", "seqw0", "Profix Media", "АРК-ТВ", "Mallorn Studio", "Причудики", "Sawyer888", "Ист-Вест", "FanStudio", "CrazyCatStudio", "PashaUp", "Сонотек", "Синта Рурони", "Видеоимпульс", "Белов", "Сыендук", "Sony Turbo", "РенТВ", "Инис", "Воронцов", "Р. Янкелевич", "Zone Vision Studio", "Anubis", "Ошурков", "Asian Miracle Group", "Sephiroth", "Вихров", "Elrom", "Русский Репортаж", "SoftBox", "DIVA Universal", "Hallmark", "Другое кино", "SkyeFilmTV", "Е. Гранкин", "Levelin", "Omskbird", "Синема УС", "Герусов", "New Dream Media", "RAIM", "Кураж-Бамбей", "Фильмэкспорт", "Савченко", "Парадиз", "Севастьянов", "Васильев", "SHIZA", "Рудой", "Е. Рудой", "CBS Drama", "Толстобров", "Lord32x", "Bonsai Studio", "KosharaSerials", "Selena", "Дьяков", "HiWayGrope", "HiWay Grope", "BraveSound", "Синема Трейд", "CPIG", "Ирэн", "Котова", "Н-Кино", "Andy", "Лагута", "Райдо", "AniPLague", "AdiSound", "visanti-vasaer", "Держиморда", "GreenРай Studio", "ZEE TV", "VoicePower", "Хуан Рохас", "Фортуна-Фильм", "Войсовер", "Ozz.tv", "Векшин", "RATTLEBOX", "Ракурс", "Radamant", "RecentFilms", "Anika", "True Dubbing Studio", "Штамп", "BadCatStudio", "Kiitos", "VictoryFilms", "Кошкин", "SpaceDust", "DeMon", "sf@irat", "Данилов", "Переводман", "Nice-Media", "Никитин", "Акира", "Я. Беллманн", "С. Дьяков", "Новый диск", "ALEKS KV", "Videogram", "Морозов", "Хоррор Мэйкер", "CinemaSET GROUP", "Деньщиков", "Мобильное телевидение", "ИДДК", "Intra Communications", "Петербуржец", "Greb&Creative", "Стасюк", "СВ-Кадр", "Готлиб", "Тоникс Медиа", "fiendover", "RoxMarty", "Project Web Mania", "Золотухин", "madrid", "Нурмухаметов", "Lazer Video", "OnisFilms", "FilmGate", "JAM", "Григорьев", "Ульпаней Эльром", "Мудров", "Альянс", "Filiza Studio", "XDUB Dorama", "Жучков", "eraserhead", "Комедия ТВ", "Леша Прапорщик", "WestFilm", "Швецов", "Хихидок", "The Kitchen Russia", "Psychotronic", "ДиоНиК", "PiratVoice", "Штамп Дмитрий", "Red Media", "Саня Белый", "Мельница", "Бибиков", "Urasiko", "XL Media", "kubik&ko", "Jakob Bellmann", "Зереницын", "Мастер Тэйп", "Лизард", "Agatha Studdio", "MOYGOLOS", "Нарышкин", "Franek Monk", "Train Studio", "TrainStudio", "D.I.M.", "AniStar", "Клюквин", "Бойков", "Voiz", "Amber", "MrRose", "AniLibria", "RG.Paravozik", "Гей Кино Гид", "НЕВА 1", "Машинский", "Т.О Друзей", "Cmert", "Parovoz Production", "VideoBIZ", "Oneinchnales", "Васька Куролесов", "Живаго", "Lizard Cinema", "Lizard Cinema Trade", "Ghostface", "РуФилмс", "Kansai", "АнВад", "Liga HQ", "AniMedia", "Reanimedia", "LeXiKC", "Зебуро", "Артемьев", "Самарский", "Перец", "To4ka", "Лексикон", "McElroy", "Муравский", "Zerzia", "Первый канал ОРТ", "RedDiamond Studio", "Sky Voices", "Creative Sound", "Jetvis Studio", "Jetvis", "CLS Media", "СВ-Студия", "Васильцев", "MGM", "L0cDoG", "RedRussian1337", "Black Street Records", "Видеобаза", "С. Макашов", "Mystery Film", "Arasi project", "Петербургский дубляж", "Толмачев", "Kerob", "SorzTeam", "Flux-Team", "Трамвай-фильм", "NemFilm", "Эй Би Видео", "Alternative Production", "Кинопремьера", "Wayland team", "Первый ТВЧ", "AuraFilm", "Gala Voices", "Sunshine Studio", "SunshineStudio", "GostFilm", "Точка Zрения", "Cowabunga Studio", "AzOnFilm", "AniDUB", "Murzilka", "MaxMeister", "ЕА Синема", "Contentica", "К. Филонов", "Tori", "Inter Video", "Victory-Films", "Тимофеев", "Студия NLS", "Храм Дорам ТВ", "Е. Хрусталёв", "Карапетян", "Наталья Гурзо", "Janetta", "Universal Russia", "HighHopes", "Чуев", "Voice Project", "Emslie", "Lisitz", "Barin101", "Animegroup", "Dream Records", "DreamRecords", "Имидж-Арт", "ko136", "Nastia", "Медиа-Комплекс", "Sound-Group", "Малиновский", "sweet couple", "Sam2007", "SnowRecords", "Horizon Studio", "Family Fan Edition", "TatamiFilm", "Николай Дроздов", "Ultradox", "ImageArt", "Квадрат Малевича", "binjak", "Инфо-фильм", "Мьюзик-трейд", "Сolumbia Service", "Extrabit", "Andre1288", "Максим Жолобов", "turok1990", "Уолт Дисней Компани", "RedDog", "Хабар", "neko64", "Gremlin Creative Studio", "VGM Studio", "Иванова и П. Пашут", "Н. Золотухин", "Костюкевич", "Video-BIZ", "Акалит", "Byako Records", "Агапов", "Мительман", "Persona99", "East Dream", "Epic Team", "VicTeam", "ЛанселаП", "Студия Горького", "Бигыч", "AvePremier", "Jade", "Cuba77", "MifSnaiper", "И. Еремеев", "Акопян", "ssvss", "Индия ТВ", "GoldTeam", "Альтера Парс", "Eurochannel", "OSLIKt", "Eladiel", "TF-AniGroup", "Boльгa", "Azazel", "Студия Пиратского Дубляжа", "FireDub", "XvidClub Studio", "Foxlight", "Гундос", "Paul Bunyan", "OpenDub", "Бахурани", "Абдулов", "The Mike Rec.", "Заугаров", "Amazing Dubbing", "Сергей Дидок", "Студия Колобок", "Ancord", "Анатолий Ашмарин", "Бессонов", "Sedorelli", "DexterTV", "Прайд Продакшн", "AveTurk", "MC Entertainment", "Loginoff", "JoyStudio", "Старый Бильбо", "ГКГ", "NEON Studio", "st.Elrom", "Max Nabokov", "Sound Film", "Twix", "Zone Studio"}

var ukrVoices = []string{"QTV", "DniproFilm", "AdrianZP", "LeDoyen", "Цікава Ідея", "Cikava Ideya", "КiT", "Inter", "NLO", "Так Треба Продакшн", "ТакТребаПродакшн", "Новий Канал", "Новый Канал", "BambooUA", "ICTV", "Тоніс", "UA-DUB", "ТеТ", "СТБ", "Postmodern", "НЛО", "Колодій", "В одне рило", "SkiDUB", "Інтер", "DubLiCat", "AAA-Sound", "AAASound", "НеЗупиняйПродакшн", "Ozz TV", "1+1", "Три Крапки", "3 крапки", "Tak Treba Production", "UAVoice", "Интер", "TET", "ПлюсПлюс", "Дніпрофільм", "ArtymKo", "Cinemaker", "sweet.tv", "MelodicVoiceStudio", "FanVoxUA", "UkraineFastDUB", "UFDUB", "CHAS.UA", "Струґачка", "StorieS man", "UATeam", "Гуртом", "UkrDub", "AniUA", "Сокира", "FlameStudio", "HATOSHI", "Sengoku"}
