package main

// Список клиентов на проверку: чтение xlsx, нормализация и контроль качества.
//
// Колонки ищутся по заголовкам, а не по позиции, поэтому порядок колонок
// в файле можно менять. Всё, что удалось исправить автоматически
// (лишние пробелы, потерянные ведущие нули), исправляется и попадает
// в лист «Замечания» отчёта; то, что исправить нельзя, — тоже туда, но
// с пометкой, что проверка по этому полю невозможна или неточна.

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Customer — клиент из списка.
type Customer struct {
	Row        int    // номер строки в Excel
	Surname    string // как в файле, без лишних пробелов
	Name       string
	Patronymic string
	DOB        string // ГГГГ-ММ-ДД или пусто
	Passport   string // только буквы и цифры
	BirthPlace string
	INN        string // только цифры
	SNILS      string // только цифры

	normFull    string // нормализованное «ФАМИЛИЯ ИМЯ ОТЧЕСТВО»
	normSurname string
	normName    string
}

// FullName — ФИО для отчёта.
func (c Customer) FullName() string {
	return strings.TrimSpace(strings.Join(strings.Fields(c.Surname+" "+c.Name+" "+c.Patronymic), " "))
}

// Issue — замечание к строке списка.
type Issue struct {
	Row      int
	Customer string
	Field    string
	Problem  string
	Action   string // что сделано или что это значит для проверки
	Severe   bool   // проверка по полю невозможна или может дать ложный результат
}

// заголовки колонок: ключ -> допустимые варианты (после normHeader).
var headerAliases = map[string][]string{
	"surname":    {"фамилия"},
	"name":       {"имя"},
	"patronymic": {"отчество"},
	"fio":        {"фио", "ф и о", "фамилия имя отчество"},
	"dob":        {"дата рождения", "дата рожд", "др", "день рождения"},
	"passport":   {"паспорт", "серия и номер паспорта", "паспорт серия и номер", "серия номер паспорта", "паспортные данные", "документ"},
	"birthplace": {"место рождения"},
	"inn":        {"инн"},
	"snils":      {"снилс"},
}

func normHeader(s string) string {
	s = strings.ToLower(strings.ReplaceAll(s, "ё", "е"))
	s = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		return ' '
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// ReadClients читает список клиентов из xlsx.
func ReadClients(path string) ([]Customer, []Issue, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	sheet, err := ReadFirstSheet(data)
	if err != nil {
		return nil, nil, err
	}

	// Ищем строку заголовков в первых 15 строках.
	headerRow := 0
	cols := map[string]int{}
	for _, r := range sortedKeys(sheet.Rows) {
		if r > 15 {
			break
		}
		found := map[string]int{}
		for c, cell := range sheet.Rows[r] {
			h := normHeader(cell.Text)
			for key, aliases := range headerAliases {
				for _, a := range aliases {
					if h == a {
						if _, dup := found[key]; !dup {
							found[key] = c
						}
					}
				}
			}
		}
		if _, ok := found["surname"]; ok {
			headerRow, cols = r, found
			break
		}
		if _, ok := found["fio"]; ok {
			headerRow, cols = r, found
			break
		}
	}
	if headerRow == 0 {
		return nil, nil, fmt.Errorf("в первых 15 строках листа «%s» не найден заголовок с колонкой «Фамилия» или «ФИО»", sheet.Name)
	}

	get := func(row int, key string) XCell {
		c, ok := cols[key]
		if !ok {
			return XCell{}
		}
		return sheet.Cell(row, c)
	}

	var clients []Customer
	var issues []Issue
	for _, r := range sortedKeys(sheet.Rows) {
		if r <= headerRow {
			continue
		}
		c, iss := parseClientRow(r, get, sheet.Date1904)
		if c == nil {
			continue
		}
		clients = append(clients, *c)
		issues = append(issues, iss...)
	}

	// Повторы одного и того же клиента.
	seen := map[string]int{}
	for _, c := range clients {
		key := c.normFull + "|" + c.DOB
		if first, ok := seen[key]; ok {
			issues = append(issues, Issue{Row: c.Row, Customer: c.FullName(), Field: "строка",
				Problem: fmt.Sprintf("клиент повторяется (уже есть в строке %d)", first),
				Action:  "проверен дважды, на результат не влияет"})
		} else {
			seen[key] = c.Row
		}
	}

	// Отсутствующие колонки — замечание на весь файл.
	for _, need := range []struct{ key, title string }{
		{"dob", "Дата рождения"}, {"inn", "ИНН"}, {"passport", "Паспорт"}, {"snils", "СНИЛС"},
	} {
		if _, ok := cols[need.key]; !ok {
			issues = append(issues, Issue{Row: headerRow, Field: need.title,
				Problem: "в файле нет колонки «" + need.title + "»",
				Action:  "проверка по этому полю не выполнялась", Severe: need.key == "dob"})
		}
	}
	return clients, issues, nil
}

func parseClientRow(row int, get func(int, string) XCell, date1904 bool) (*Customer, []Issue) {
	c := &Customer{Row: row}
	var issues []Issue
	add := func(field, problem, action string, severe bool) {
		issues = append(issues, Issue{Row: row, Field: field, Problem: problem, Action: action, Severe: severe})
	}

	rawSurname := get(row, "surname").Text
	rawName := get(row, "name").Text
	rawPatr := get(row, "patronymic").Text
	if fio := strings.TrimSpace(get(row, "fio").Text); fio != "" && strings.TrimSpace(rawSurname+rawName+rawPatr) == "" {
		f := strings.Fields(fio)
		if len(f) > 0 {
			rawSurname = f[0]
		}
		if len(f) > 1 {
			rawName = f[1]
		}
		if len(f) > 2 {
			rawPatr = strings.Join(f[2:], " ")
		}
	}

	// Совсем пустая строка — не клиент.
	rest := get(row, "dob").Text + get(row, "inn").Text + get(row, "passport").Text + get(row, "snils").Text
	if strings.TrimSpace(rawSurname+rawName+rawPatr+rest) == "" {
		return nil, nil
	}

	clean := func(field, raw string) string {
		v := strings.Join(strings.Fields(raw), " ")
		if v != raw && raw != "" {
			add(field, "лишние пробелы", "убраны автоматически", false)
		}
		return v
	}
	c.Surname = clean("Фамилия", rawSurname)
	c.Name = clean("Имя", rawName)
	c.Patronymic = clean("Отчество", rawPatr)
	c.BirthPlace = strings.Join(strings.Fields(get(row, "birthplace").Text), " ")

	if c.Surname == "" {
		add("Фамилия", "не заполнена", "проверка по ФИО невозможна", true)
	}
	if c.Name == "" {
		add("Имя", "не заполнено", "проверка по ФИО неточная", true)
	}
	if c.Patronymic == "" && c.Surname != "" {
		add("Отчество", "не заполнено", "сверка по фамилии и имени, возможны лишние «требует проверки»", false)
	}
	for _, f := range []struct{ field, v string }{{"Фамилия", c.Surname}, {"Имя", c.Name}, {"Отчество", c.Patronymic}} {
		if p := nameProblem(f.v); p != "" {
			add(f.field, p, "буквы приведены к кириллице для сверки; исправьте в файле", true)
		}
	}

	// Дата рождения.
	dobCell := get(row, "dob")
	switch {
	case dobCell.Numeric:
		if t, ok := ExcelSerialToDate(dobCell.Num, date1904); ok {
			c.DOB = t.Format("2006-01-02")
		} else {
			add("Дата рождения", "число «"+dobCell.Text+"» не похоже на дату", "проверка без даты рождения", true)
		}
	case strings.TrimSpace(dobCell.Text) != "":
		if t, ok := parseRuDate(dobCell.Text); ok {
			c.DOB = t.Format("2006-01-02")
			add("Дата рождения", "дата записана текстом («"+strings.TrimSpace(dobCell.Text)+"»)", "распознана как "+t.Format("02.01.2006")+"; лучше формат ячейки «Дата»", false)
		} else {
			add("Дата рождения", "не распознана: «"+strings.TrimSpace(dobCell.Text)+"»", "проверка без даты рождения", true)
		}
	default:
		add("Дата рождения", "не заполнена", "совпадение по ФИО будет помечено «требует проверки»", true)
	}
	if c.DOB != "" {
		t, _ := time.Parse("2006-01-02", c.DOB)
		if t.After(time.Now()) || t.Year() < time.Now().Year()-120 {
			add("Дата рождения", "неправдоподобная дата "+t.Format("02.01.2006"), "проверьте значение", true)
		}
	}

	// ИНН.
	c.INN, issues = idField(row, get(row, "inn"), "ИНН", 12, issues)
	if c.INN != "" {
		switch {
		case len(c.INN) == 10:
			issues = append(issues, Issue{Row: row, Field: "ИНН", Problem: "10 цифр — это ИНН организации, у физлица 12", Action: "сверка по ИНН выполнена как есть", Severe: true})
		case len(c.INN) != 12:
			issues = append(issues, Issue{Row: row, Field: "ИНН", Problem: fmt.Sprintf("%d цифр вместо 12", len(c.INN)), Action: "сверка по ИНН выполнена как есть", Severe: true})
		case !validINN12(c.INN):
			issues = append(issues, Issue{Row: row, Field: "ИНН", Problem: "не сходится контрольная сумма — вероятна опечатка", Action: "сверка по ИНН выполнена как есть", Severe: true})
		}
	}

	// СНИЛС.
	c.SNILS, issues = idField(row, get(row, "snils"), "СНИЛС", 11, issues)
	if c.SNILS != "" {
		switch {
		case len(c.SNILS) != 11:
			issues = append(issues, Issue{Row: row, Field: "СНИЛС", Problem: fmt.Sprintf("%d цифр вместо 11", len(c.SNILS)), Action: "сверка по СНИЛС выполнена как есть", Severe: true})
		case !validSNILS(c.SNILS):
			issues = append(issues, Issue{Row: row, Field: "СНИЛС", Problem: "не сходится контрольное число — вероятна опечатка", Action: "сверка по СНИЛС выполнена как есть", Severe: true})
		}
	}

	// Паспорт: для паспорта РФ 10 цифр (4 серия + 6 номер). Иностранные
	// документы бывают с буквами — их не трогаем, только нормализуем.
	pc := get(row, "passport")
	if pc.Numeric {
		d := pc.Text
		if len(d) == 9 {
			d = "0" + d
			add("Паспорт", "хранится числом, потерян ведущий ноль серии", "восстановлен: "+d+"; сделайте колонку текстовой", true)
		} else {
			add("Паспорт", "хранится числом", "сделайте колонку текстовой, иначе пропадают ведущие нули", false)
		}
		c.Passport = d
	} else if v := strings.TrimSpace(pc.Text); v != "" {
		c.Passport = normDoc(v)
		if isAllDigits(c.Passport) && len(c.Passport) != 10 {
			add("Паспорт", fmt.Sprintf("%d цифр (у паспорта РФ — 10: серия 4 + номер 6)", len(c.Passport)), "сверка по паспорту выполнена как есть", true)
		}
	}

	c.normSurname = normName(c.Surname)
	c.normName = normName(c.Name)
	c.normFull = normName(c.FullName())

	for i := range issues {
		issues[i].Customer = c.FullName()
	}
	return c, issues
}

// idField читает поле из цифр (ИНН, СНИЛС), восстанавливая ведущие нули,
// если ячейка была числом.
func idField(row int, cell XCell, field string, want int, issues []Issue) (string, []Issue) {
	if cell.Text == "" {
		return "", issues
	}
	d := digitsOnly(cell.Text)
	if cell.Numeric {
		if len(d) < want && len(d) >= want-2 {
			restored := strings.Repeat("0", want-len(d)) + d
			issues = append(issues, Issue{Row: row, Field: field,
				Problem: "хранится числом, потерян ведущий ноль",
				Action:  "восстановлен: " + restored + "; сделайте колонку текстовой", Severe: true})
			return restored, issues
		}
		issues = append(issues, Issue{Row: row, Field: field, Problem: "хранится числом",
			Action: "сделайте колонку текстовой, иначе могут пропасть ведущие нули"})
		return d, issues
	}
	if d != strings.TrimSpace(cell.Text) && !regexp.MustCompile(`^[\d\s\-]+$`).MatchString(strings.TrimSpace(cell.Text)) {
		issues = append(issues, Issue{Row: row, Field: field, Problem: "посторонние символы: «" + strings.TrimSpace(cell.Text) + "»",
			Action: "взяты только цифры: " + d, Severe: true})
	}
	return d, issues
}

var ruDateLayouts = []string{"02.01.2006", "2.1.2006", "02.1.2006", "2.01.2006", "2006-01-02", "02/01/2006", "2/1/2006", "02-01-2006"}

func parseRuDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, " T"); i > 0 { // «1966-10-18 00:00:00»
		s = s[:i]
	}
	s = strings.TrimSuffix(strings.TrimSuffix(s, "г."), "г")
	s = strings.TrimSpace(s)
	for _, l := range ruDateLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// ---------- нормализация ----------

// Латинские буквы, неотличимые на глаз от кириллических.
var latinToCyr = map[rune]rune{
	'A': 'А', 'B': 'В', 'C': 'С', 'E': 'Е', 'H': 'Н', 'K': 'К', 'M': 'М',
	'O': 'О', 'P': 'Р', 'T': 'Т', 'X': 'Х', 'Y': 'У',
}

// normName приводит ФИО к виду для сравнения: верхний регистр, Ё→Е,
// дефисы и прочие знаки — пробелы, латинские «двойники» в кириллических
// словах — кириллицей.
func normName(s string) string {
	s = strings.ToUpper(s)
	s = strings.ReplaceAll(s, "Ё", "Е")
	s = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		return ' '
	}, s)
	words := strings.Fields(s)
	for i, w := range words {
		if hasCyrillic(w) {
			words[i] = strings.Map(func(r rune) rune {
				if c, ok := latinToCyr[r]; ok {
					return c
				}
				return r
			}, w)
		}
	}
	return strings.Join(words, " ")
}

func hasCyrillic(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Cyrillic, r) {
			return true
		}
	}
	return false
}

func hasLatin(s string) bool {
	for _, r := range s {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
			return true
		}
	}
	return false
}

// nameProblem ищет в части ФИО то, что сломает сверку.
func nameProblem(s string) string {
	if s == "" {
		return ""
	}
	for _, r := range s {
		if unicode.IsDigit(r) {
			return "содержит цифры"
		}
	}
	for _, w := range strings.Fields(s) {
		if hasCyrillic(w) && hasLatin(w) {
			return "латинские буквы вперемешку с русскими («" + w + "»)"
		}
	}
	return ""
}

func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isAllDigits(s string) bool {
	return s != "" && digitsOnly(s) == s
}

// normDoc: серия и номер документа — только буквы и цифры, верхний регистр.
func normDoc(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// validINN12 — контрольные цифры ИНН физического лица.
func validINN12(inn string) bool {
	if len(inn) != 12 || !isAllDigits(inn) {
		return false
	}
	d := make([]int, 12)
	for i, r := range inn {
		d[i] = int(r - '0')
	}
	w11 := []int{7, 2, 4, 10, 3, 5, 9, 4, 6, 8}
	w12 := []int{3, 7, 2, 4, 10, 3, 5, 9, 4, 6, 8}
	s := 0
	for i, w := range w11 {
		s += w * d[i]
	}
	if s%11%10 != d[10] {
		return false
	}
	s = 0
	for i, w := range w12 {
		s += w * d[i]
	}
	return s%11%10 == d[11]
}

// validSNILS — контрольное число СНИЛС (для номеров больше 001-001-998).
func validSNILS(snils string) bool {
	if len(snils) != 11 || !isAllDigits(snils) {
		return false
	}
	num, _ := strconv.Atoi(snils[:9])
	if num <= 1001998 {
		return true // для ранних номеров контрольное число не рассчитывается
	}
	sum := 0
	for i := 0; i < 9; i++ {
		sum += int(snils[i]-'0') * (9 - i)
	}
	var cs int
	switch {
	case sum < 100:
		cs = sum
	case sum == 100 || sum == 101:
		cs = 0
	default:
		cs = sum % 101
		if cs == 100 {
			cs = 0
		}
	}
	got, _ := strconv.Atoi(snils[9:])
	return cs == got
}
