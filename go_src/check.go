package main

// Проверка клиентов по перечням.
//
// Когда запускается: при старте демона и после каждого прогона скачивания,
// но реально работает, только если изменился «отпечаток» входных данных —
// файл клиентов или любой из трёх перечней. Так новая выгрузка перечня или
// новая версия списка клиентов дают ровно одну проверку и одно письмо.
//
// Уровни результата по клиенту и перечню:
//   СОВПАДЕНИЕ        — совпал ИНН, СНИЛС или паспорт; либо ФИО целиком + дата
//                       рождения (или год, если в перечне есть только год).
//   ТРЕБУЕТ ПРОВЕРКИ  — ФИО совпало, но дату сравнить нельзя (нет у клиента
//                       или в перечне); фамилия + имя + дата рождения при
//                       другом отчестве; фамилия + дата рождения при другом
//                       имени (типичная опечатка).
//   нет               — всё остальное. Полные тёзки с другой датой рождения
//                       совпадением не считаются, но перечисляются на листе
//                       «Совпадения» для сведения.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	LevelNone     = 0
	LevelNamesake = 1 // тёзка, ДР отличается — только для сведения
	LevelPossible = 2
	LevelMatch    = 3
)

// Hit — найденная связь клиента с лицом из перечня.
type Hit struct {
	List   *ListData
	Person ListPerson
	Level  int
	Why    []string
}

// ClientResult — итог по одному клиенту.
type ClientResult struct {
	Customer Customer
	Hits     []Hit
}

// Level — наихудший уровень по перечню key ("" — по всем).
func (r ClientResult) Level(key string) int {
	lvl := LevelNone
	for _, h := range r.Hits {
		if (key == "" || h.List.Key == key) && h.Level > lvl {
			lvl = h.Level
		}
	}
	return lvl
}

// CheckResult — итог проверки.
type CheckResult struct {
	At          time.Time
	ClientsFile string
	OutputDir   string
	Lists       []*ListData
	Results     []ClientResult
	Issues      []Issue
	Matches     int // клиентов с совпадением
	Possible    int // клиентов, требующих проверки
	ReportPath  string
}

// ---------- сопоставление ----------

func tokens(s string) []string { return strings.Fields(s) }

func containsAll(have []string, want ...string) bool {
	set := map[string]int{}
	for _, h := range have {
		set[h]++
	}
	for _, w := range want {
		if w == "" {
			continue
		}
		if set[w] == 0 {
			return false
		}
		set[w]--
	}
	return true
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// MatchClient сверяет клиента с одним лицом из перечня.
func MatchClient(c Customer, p ListPerson, oon bool) (int, []string) {
	var why []string
	strong := false
	if c.INN != "" && contains(p.INNs, c.INN) {
		why = append(why, "ИНН")
		strong = true
	}
	if c.SNILS != "" && contains(p.SNILSs, c.SNILS) {
		why = append(why, "СНИЛС")
		strong = true
	}
	if len(c.Passport) >= 6 && contains(p.Docs, c.Passport) {
		why = append(why, "паспорт")
		strong = true
	}

	// Имя.
	nameExact := false   // ФИО целиком
	nameSurName := false // фамилия + имя (отчество другое, отсутствует или порядок иной)
	for _, n := range p.Names {
		if c.normFull != "" && n == c.normFull {
			nameExact = true
			break
		}
	}
	if !nameExact && c.normSurname != "" && c.normName != "" {
		for _, n := range p.Names {
			t := tokens(n)
			if oon {
				// В перечне ООН порядок частей имени произвольный.
				if containsAll(t, c.normSurname, c.normName) {
					nameSurName = true
				}
			} else if len(t) >= 2 && t[0] == c.normSurname && t[1] == c.normName {
				nameSurName = true
			}
			if nameSurName {
				break
			}
		}
	}
	surnameEq := c.normSurname != "" && (contains(p.Surnames, c.normSurname) ||
		(oon && func() bool {
			for _, n := range p.Names {
				if contains(tokens(n), c.normSurname) {
					return true
				}
			}
			return false
		}()))

	// Дата рождения.
	dobEq, yearEq, dobKnownDiff := false, false, false
	if c.DOB != "" {
		if contains(p.DOBs, c.DOB) {
			dobEq = true
		} else if len(p.DOBs) > 0 {
			dobKnownDiff = true
			// год совпал, а дата нет — всё равно «другая дата»
		} else if len(p.Years) > 0 {
			if contains(p.Years, c.DOB[:4]) {
				yearEq = true
			} else {
				dobKnownDiff = true
			}
		}
	}
	dobUnknown := !dobEq && !yearEq && !dobKnownDiff

	switch {
	case nameExact:
		why = append(why, "ФИО")
	case nameSurName:
		why = append(why, "фамилия и имя")
	case surnameEq && dobEq:
		why = append(why, "фамилия")
	}
	if dobEq {
		why = append(why, "дата рождения")
	} else if yearEq {
		why = append(why, "год рождения")
	}

	switch {
	case strong:
		return LevelMatch, why
	case nameExact && (dobEq || yearEq):
		return LevelMatch, why
	case nameSurName && dobEq:
		return LevelPossible, why
	case nameExact && dobUnknown:
		return LevelPossible, append(why, "дату рождения сравнить нельзя")
	case nameSurName && (yearEq || dobUnknown):
		return LevelPossible, append(why, "дату рождения сравнить нельзя")
	case surnameEq && dobEq:
		return LevelPossible, why
	case nameExact && dobKnownDiff:
		return LevelNamesake, append(why, "дата рождения другая")
	}
	return LevelNone, nil
}

// listIndex — индекс перечня: ключ -> номера лиц. Ключи — слова имён
// (покрывают фамилию в любом порядке и другие имена), ИНН, СНИЛС, документы.
type listIndex map[string][]int

func buildIndex(persons []ListPerson) listIndex {
	idx := listIndex{}
	add := func(k string, i int) {
		if k == "" {
			return
		}
		if l := idx[k]; len(l) > 0 && l[len(l)-1] == i {
			return
		}
		idx[k] = append(idx[k], i)
	}
	for i, p := range persons {
		for _, n := range p.Names {
			for _, w := range tokens(n) {
				add("n:"+w, i)
			}
		}
		for _, sn := range p.Surnames {
			add("n:"+sn, i)
		}
		for _, v := range p.INNs {
			add("i:"+v, i)
		}
		for _, v := range p.SNILSs {
			add("s:"+v, i)
		}
		for _, v := range p.Docs {
			add("d:"+v, i)
		}
	}
	return idx
}

func (idx listIndex) candidates(c Customer) []int {
	seen := map[int]bool{}
	var out []int
	first := ""
	if f := tokens(c.normSurname); len(f) > 0 {
		first = f[0] // «ПЕТРОВ ВОДКИН» ищем по «ПЕТРОВ»: в индексе отдельные слова
	}
	for _, k := range []string{"n:" + first, "i:" + c.INN, "s:" + c.SNILS, "d:" + c.Passport} {
		if len(k) <= 2 {
			continue
		}
		for _, i := range idx[k] {
			if !seen[i] {
				seen[i] = true
				out = append(out, i)
			}
		}
	}
	sort.Ints(out)
	return out
}

// RunMatching сверяет всех клиентов со всеми перечнями.
func RunMatching(clients []Customer, lists []*ListData) []ClientResult {
	indexes := make([]listIndex, len(lists))
	for i, l := range lists {
		if l.Err == nil {
			indexes[i] = buildIndex(l.Persons)
		}
	}
	results := make([]ClientResult, len(clients))
	for i, c := range clients {
		results[i].Customer = c
		for li, l := range lists {
			if l.Err != nil {
				continue
			}
			seen := map[string]bool{}
			for _, pi := range indexes[li].candidates(c) {
				p := l.Persons[pi]
				lvl, why := MatchClient(c, p, l.Key == "oon")
				if lvl == LevelNone {
					continue
				}
				key := p.ID + "|" + p.Display
				if seen[key] { // одно лицо в нескольких решениях МВК
					continue
				}
				seen[key] = true
				results[i].Hits = append(results[i].Hits, Hit{List: l, Person: p, Level: lvl, Why: why})
			}
		}
		sort.SliceStable(results[i].Hits, func(a, b int) bool {
			return results[i].Hits[a].Level > results[i].Hits[b].Level
		})
	}
	return results
}

// ---------- отпечаток входных данных ----------

func fileFingerprint(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return path + ":нет"
	}
	return fmt.Sprintf("%s:%d:%d", path, st.Size(), st.ModTime().Unix())
}

// checkFingerprint учитывает содержимое файла клиентов и версии перечней.
func checkFingerprint(clientsFile string, base string) string {
	h := sha256.New()
	if data, err := os.ReadFile(clientsFile); err == nil {
		h.Write(data)
	}
	for _, spec := range checkLists {
		p, err := FindLatestListFile(base, spec)
		if err != nil {
			p = spec.Key + ":нет"
		}
		h.Write([]byte("|" + fileFingerprint(p)))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ---------- запуск ----------

// RunClientCheck выполняет проверку, если изменились входные данные (или force).
// Состояние читает и сохраняет сам: вызывается после runOnce, которое
// state.json уже записало.
func RunClientCheck(cfg Config, force bool) error {
	if !cfg.CheckEnabled {
		return nil
	}
	if _, err := os.Stat(cfg.CheckFile); err != nil {
		if os.IsNotExist(err) {
			log.Printf("проверка клиентов: файл %s не найден, пропускаем", cfg.CheckFile)
			return nil
		}
		return fmt.Errorf("проверка клиентов: %w", err)
	}

	state, err := LoadState(cfg.OutputDir)
	if err != nil {
		log.Printf("проверка клиентов: state.json не прочитан (%v)", err)
	}
	fp := checkFingerprint(cfg.CheckFile, cfg.OutputDir)
	if !force && fp == state.Check.Fingerprint {
		log.Printf("проверка клиентов: ни список клиентов, ни перечни не менялись — проверка не нужна")
		return nil
	}

	started := time.Now()
	res, err := CheckClients(cfg)
	if err != nil {
		if merr := sendMail(cfg, "rosfin: ОШИБКА проверки клиентов",
			"Проверку клиентов выполнить не удалось:\r\n"+err.Error()+"\r\n\r\nФайл: "+cfg.CheckFile+"\r\n"); merr != nil {
			log.Printf("почта: %v", merr)
		}
		// Отпечаток не сохраняем: при следующем запуске попробуем снова.
		return err
	}
	log.Printf("проверка клиентов: клиентов %d, совпадений %d, требуют проверки %d, замечаний %d, отчёт %s (%s)",
		len(res.Results), res.Matches, res.Possible, len(res.Issues), res.ReportPath, time.Since(started).Round(time.Millisecond))

	subject, body := checkMail(res)
	var atts []MailAttachment
	if cfg.CheckAttach {
		if data, err := os.ReadFile(res.ReportPath); err == nil {
			atts = append(atts, MailAttachment{
				Name:        filepath.Base(res.ReportPath),
				ContentType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
				Data:        data,
			})
		}
	}
	if err := sendMailWith(cfg, subject, body, atts); err != nil {
		log.Printf("почта: не удалось отправить итог проверки клиентов: %v", err)
	}

	// Перечитываем состояние перед записью — его могли обновить.
	state, _ = LoadState(cfg.OutputDir)
	state.Check = CheckState{
		Fingerprint: fp,
		LastRun:     res.At.Format(time.RFC3339),
		LastReport:  res.ReportPath,
		Matches:     res.Matches,
		Possible:    res.Possible,
	}
	if err := SaveState(cfg.OutputDir, state); err != nil {
		log.Printf("предупреждение: не удалось сохранить state.json: %v", err)
	}
	return nil
}

// CheckClients читает данные, сверяет и пишет отчёт.
func CheckClients(cfg Config) (*CheckResult, error) {
	res := &CheckResult{At: time.Now(), ClientsFile: cfg.CheckFile, OutputDir: cfg.OutputDir}

	clients, issues, err := ReadClients(cfg.CheckFile)
	if err != nil {
		return nil, fmt.Errorf("список клиентов %s: %w", filepath.Base(cfg.CheckFile), err)
	}
	if len(clients) == 0 {
		return nil, fmt.Errorf("в файле %s не найдено ни одного клиента", filepath.Base(cfg.CheckFile))
	}
	res.Issues = issues

	res.Lists = LoadCheckLists(cfg.OutputDir)
	okLists := 0
	for _, l := range res.Lists {
		if l.Err != nil {
			log.Printf("проверка клиентов: %s: %v", l.Short, l.Err)
		} else {
			okLists++
			log.Printf("проверка клиентов: %s от %s — физлиц %d (%s)", l.Short, l.Date, len(l.Persons), l.File)
		}
	}
	if okLists == 0 {
		return nil, fmt.Errorf("не удалось загрузить ни один перечень")
	}

	res.Results = RunMatching(clients, res.Lists)
	for _, r := range res.Results {
		switch r.Level("") {
		case LevelMatch:
			res.Matches++
		case LevelPossible:
			res.Possible++
		}
	}

	data, err := BuildReport(res)
	if err != nil {
		return nil, fmt.Errorf("отчёт: %w", err)
	}
	dir := cfg.CheckReportDir
	if dir == "" {
		// Отчёт кладём в самую свежую папку прогона — рядом с перечнями,
		// по которым проверяли. Если папок прогонов ещё нет — рядом со списком.
		if runs, err := runDirsDesc(cfg.OutputDir); err == nil && len(runs) > 0 {
			dir = filepath.Join(cfg.OutputDir, runs[0])
		} else {
			dir = filepath.Dir(cfg.CheckFile)
		}
	}
	res.ReportPath = reportPath(dir, res.At)
	if err := writeFileAtomic(res.ReportPath, data); err != nil {
		return nil, err
	}
	return res, nil
}

// reportPath: Отчёт_о_проверке_ДД-ММ-ГГГГ.xlsx, при повторе за день — с временем.
func reportPath(dir string, at time.Time) string {
	p := filepath.Join(dir, "Отчёт_о_проверке_"+at.Format("02-01-2006")+".xlsx")
	if _, err := os.Stat(p); err == nil {
		p = filepath.Join(dir, "Отчёт_о_проверке_"+at.Format("02-01-2006_15-04")+".xlsx")
	}
	return p
}

// ---------- отчёт ----------

func ruDate(iso string) string {
	if t, err := time.Parse("2006-01-02", iso); err == nil {
		return t.Format("02.01.2006")
	}
	return iso
}

func hitText(h Hit) string {
	var b strings.Builder
	b.WriteString(h.Person.Display)
	if len(h.Person.DOBs) > 0 {
		var ds []string
		for _, d := range h.Person.DOBs {
			ds = append(ds, ruDate(d))
		}
		b.WriteString(", ДР " + strings.Join(ds, " / "))
	} else if len(h.Person.Years) > 0 {
		b.WriteString(", год рожд. " + strings.Join(h.Person.Years, " / "))
	}
	if h.Person.ID != "" {
		b.WriteString(" (№ " + h.Person.ID + ")")
	}
	b.WriteString("; совпало: " + strings.Join(h.Why, ", "))
	return b.String()
}

func cellFor(r ClientResult, l *ListData) WCell {
	if l.Err != nil {
		return WCell{Text: "не проверено: перечень недоступен", Style: StPossible}
	}
	var parts []string
	lvl := r.Level(l.Key)
	for _, h := range r.Hits {
		if h.List.Key != l.Key || h.Level < LevelPossible {
			continue
		}
		parts = append(parts, hitText(h))
	}
	switch lvl {
	case LevelMatch:
		return WCell{Text: "СОВПАДЕНИЕ: " + strings.Join(parts, "\n"), Style: StMatch}
	case LevelPossible:
		return WCell{Text: "Требует проверки: " + strings.Join(parts, "\n"), Style: StPossible}
	}
	return WCell{Text: "Совпадений не выявлено", Style: StOK}
}

// BuildReport собирает xlsx-отчёт.
func BuildReport(res *CheckResult) ([]byte, error) {
	main := &WSheet{Name: "Результаты проверки", Heights: map[int]float64{}}
	ncols := 4 + len(res.Lists)
	main.Widths = []float64{5, 34, 15, 13}
	for range res.Lists {
		main.Widths = append(main.Widths, 38)
	}
	last := colName(ncols - 1)

	r := main.Add(WCell{Text: "Отчёт о проверке клиентов по перечням Росфинмониторинга", Style: StTitle})
	main.Merges = append(main.Merges, fmt.Sprintf("A%d:%s%d", r+1, last, r+1))
	main.Heights[r] = 21
	r = main.Add(WCell{Text: "Дата проверки: " + res.At.Format("02.01.2006 15:04") +
		"     Список клиентов: " + filepath.Base(res.ClientsFile) + "     Проверено клиентов: " + fmt.Sprint(len(res.Results)), Style: StWrap})
	main.Merges = append(main.Merges, fmt.Sprintf("A%d:%s%d", r+1, last, r+1))
	main.Add()

	header := []WCell{{Text: "№", Style: StHeader}, {Text: "ФИО", Style: StHeader}, {Text: "ИНН", Style: StHeader}, {Text: "Дата рождения", Style: StHeader}}
	for _, l := range res.Lists {
		t := l.Title
		if l.Date != "" {
			t += " от " + l.Date
		}
		header = append(header, WCell{Text: t, Style: StHeader})
	}
	hr := main.Add(header...)
	main.Heights[hr] = 120

	for i, cr := range res.Results {
		c := cr.Customer
		row := []WCell{
			{Text: fmt.Sprint(i + 1), Style: StCell},
			{Text: c.FullName(), Style: StCell},
			{Text: c.INN, Style: StCell},
			{Text: ruDate(c.DOB), Style: StCell},
		}
		for _, l := range res.Lists {
			row = append(row, cellFor(cr, l))
		}
		main.Add(row...)
	}
	main.Add()

	var concl strings.Builder
	concl.WriteString(fmt.Sprintf("Заключение. Проверено клиентов: %d. ", len(res.Results)))
	switch {
	case res.Matches > 0:
		concl.WriteString(fmt.Sprintf("ВЫЯВЛЕНЫ СОВПАДЕНИЯ: %d клиент(а/ов) — см. красные ячейки и лист «Совпадения». ", res.Matches))
	case res.Possible > 0:
		concl.WriteString("Полных совпадений не выявлено. ")
	default:
		concl.WriteString("Совпадений не выявлено. ")
	}
	if res.Possible > 0 {
		concl.WriteString(fmt.Sprintf("Требуют ручной проверки: %d клиент(а/ов) — см. жёлтые ячейки. ", res.Possible))
	}
	concl.WriteString("Сверка выполнена по ФИО (включая другие имена из перечня), дате рождения, ИНН, СНИЛС и серии/номеру паспорта.")
	for _, l := range res.Lists {
		if l.Err != nil {
			concl.WriteString(" ВНИМАНИЕ: " + l.Short + " не проверен — " + l.Err.Error() + ".")
		}
	}
	severe := 0
	for _, is := range res.Issues {
		if is.Severe {
			severe++
		}
	}
	if severe > 0 {
		concl.WriteString(fmt.Sprintf(" К списку клиентов есть существенные замечания (%d) — см. лист «Замечания к списку».", severe))
	}
	cr := main.Add(WCell{Text: concl.String(), Style: StBold})
	main.Merges = append(main.Merges, fmt.Sprintf("A%d:%s%d", cr+1, last, cr+1))
	main.Heights[cr] = 90

	main.Add()
	for _, l := range res.Lists {
		src := "не найден"
		if l.File != "" {
			src = l.File
			if rel, err := filepath.Rel(res.OutputDir, l.File); err == nil && !strings.HasPrefix(rel, "..") {
				src = "downloads/" + filepath.ToSlash(rel)
			}
		}
		rr := main.Add(WCell{Text: l.Short + (func() string {
			if l.Date != "" {
				return " от " + l.Date
			}
			return ""
		}()) + ": " + src, Style: StWrap})
		main.Merges = append(main.Merges, fmt.Sprintf("A%d:%s%d", rr+1, last, rr+1))
	}

	sheets := []*WSheet{main}

	// Лист совпадений.
	var hitsSheet *WSheet
	for i, cr := range res.Results {
		for _, h := range cr.Hits {
			if hitsSheet == nil {
				hitsSheet = &WSheet{Name: "Совпадения", Widths: []float64{5, 30, 13, 22, 16, 38, 18, 30}, Heights: map[int]float64{}}
				hitsSheet.Add(WCell{Text: "Найденные связи с перечнями", Style: StTitle})
				hitsSheet.Add()
				hr := hitsSheet.Add(
					WCell{Text: "№ клиента", Style: StHeader}, WCell{Text: "Клиент", Style: StHeader},
					WCell{Text: "ДР клиента", Style: StHeader}, WCell{Text: "Перечень", Style: StHeader},
					WCell{Text: "№ в перечне", Style: StHeader}, WCell{Text: "Лицо в перечне", Style: StHeader},
					WCell{Text: "Результат", Style: StHeader}, WCell{Text: "Что совпало", Style: StHeader})
				hitsSheet.Heights[hr] = 30
			}
			st, lvl := StCell, "для сведения: тёзка"
			switch h.Level {
			case LevelMatch:
				st, lvl = StMatch, "СОВПАДЕНИЕ"
			case LevelPossible:
				st, lvl = StPossible, "требует проверки"
			}
			who := h.Person.Display
			if len(h.Person.DOBs) > 0 {
				var ds []string
				for _, d := range h.Person.DOBs {
					ds = append(ds, ruDate(d))
				}
				who += ", ДР " + strings.Join(ds, " / ")
			}
			hitsSheet.Add(
				WCell{Text: fmt.Sprint(i + 1), Style: StCell}, WCell{Text: cr.Customer.FullName(), Style: StCell},
				WCell{Text: ruDate(cr.Customer.DOB), Style: StCell}, WCell{Text: h.List.Short + " от " + h.List.Date, Style: StCell},
				WCell{Text: h.Person.ID, Style: StCell}, WCell{Text: who, Style: StCell},
				WCell{Text: lvl, Style: st}, WCell{Text: strings.Join(h.Why, ", "), Style: StCell})
		}
	}
	if hitsSheet != nil {
		sheets = append(sheets, hitsSheet)
	}

	// Лист замечаний.
	if len(res.Issues) > 0 {
		is := &WSheet{Name: "Замечания к списку", Widths: []float64{9, 32, 15, 45, 50, 14}, Heights: map[int]float64{}}
		is.Add(WCell{Text: "Замечания к файлу «" + filepath.Base(res.ClientsFile) + "»", Style: StTitle})
		is.Add(WCell{Text: "Существенные замечания влияют на точность проверки — их нужно исправить в файле. Остальные — к сведению или уже исправлены автоматически.", Style: StWrap})
		is.Merges = append(is.Merges, "A2:F2")
		is.Add()
		hr := is.Add(WCell{Text: "Строка в файле", Style: StHeader}, WCell{Text: "Клиент", Style: StHeader},
			WCell{Text: "Поле", Style: StHeader}, WCell{Text: "Замечание", Style: StHeader},
			WCell{Text: "Что сделано / последствия", Style: StHeader}, WCell{Text: "Важность", Style: StHeader})
		is.Heights[hr] = 30
		sorted := append([]Issue(nil), res.Issues...)
		sort.SliceStable(sorted, func(a, b int) bool {
			if sorted[a].Severe != sorted[b].Severe {
				return sorted[a].Severe
			}
			return sorted[a].Row < sorted[b].Row
		})
		for _, x := range sorted {
			imp, st := "к сведению", StCell
			if x.Severe {
				imp, st = "существенное", StPossible
			}
			is.Add(WCell{Text: fmt.Sprint(x.Row), Style: StCell}, WCell{Text: x.Customer, Style: StCell},
				WCell{Text: x.Field, Style: StCell}, WCell{Text: x.Problem, Style: StCell},
				WCell{Text: x.Action, Style: StCell}, WCell{Text: imp, Style: st})
		}
		sheets = append(sheets, is)
	}

	return WriteWorkbook(sheets)
}

// checkMail — тема и текст письма по итогам проверки.
func checkMail(res *CheckResult) (string, string) {
	var subject string
	switch {
	case res.Matches > 0:
		subject = fmt.Sprintf("ВНИМАНИЕ! rosfin: СОВПАДЕНИЕ при проверке клиентов (%d)", res.Matches)
	case res.Possible > 0:
		subject = fmt.Sprintf("rosfin: проверка клиентов — требуется ручная проверка (%d)", res.Possible)
	default:
		subject = "rosfin: проверка клиентов — совпадений нет"
	}
	var b strings.Builder
	b.WriteString("Проверка клиентов по перечням Росфинмониторинга\r\n")
	b.WriteString("Дата: " + res.At.Format("02.01.2006 15:04") + "\r\n")
	b.WriteString("Список: " + res.ClientsFile + "\r\n")
	b.WriteString("Отчёт: " + res.ReportPath + "\r\n\r\n")
	b.WriteString("Перечни:\r\n")
	for _, l := range res.Lists {
		if l.Err != nil {
			b.WriteString("  " + l.Short + ": НЕ ПРОВЕРЕН — " + l.Err.Error() + "\r\n")
		} else {
			b.WriteString(fmt.Sprintf("  %s от %s (физлиц: %d)\r\n", l.Short, l.Date, len(l.Persons)))
		}
	}
	b.WriteString(fmt.Sprintf("\r\nПроверено клиентов: %d\r\nСовпадений: %d\r\nТребуют ручной проверки: %d\r\n",
		len(res.Results), res.Matches, res.Possible))
	if res.Matches+res.Possible > 0 {
		b.WriteString("\r\nПодробности:\r\n")
		for i, r := range res.Results {
			for _, h := range r.Hits {
				if h.Level < LevelPossible {
					continue
				}
				lvl := "требует проверки"
				if h.Level == LevelMatch {
					lvl = "СОВПАДЕНИЕ"
				}
				b.WriteString(fmt.Sprintf("  %d. %s — %s, %s: %s\r\n", i+1, r.Customer.FullName(), lvl, h.List.Short, hitText(h)))
			}
		}
	}
	severe := 0
	for _, is := range res.Issues {
		if is.Severe {
			severe++
		}
	}
	if len(res.Issues) > 0 {
		b.WriteString(fmt.Sprintf("\r\nЗамечания к списку клиентов: %d (существенных: %d) — лист «Замечания к списку» в отчёте.\r\n", len(res.Issues), severe))
	}
	return subject, b.String()
}
