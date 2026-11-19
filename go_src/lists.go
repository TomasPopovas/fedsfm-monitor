package main

// Разбор скачанных перечней для проверки клиентов.
//
// Три источника, три формата:
//   terrorism — «Перечень» версии 2.x (XML 82 МБ+), субъекты в АктуальныйПеречень
//   mvk       — «СписокРешений», субъекты в СписокАктуальныхРешений/Решение/СписокСубъектов
//   oon       — CONSOLIDATED_LIST СБ ООН (русская версия: имена кириллицей)
//
// Разбор потоковый (encoding/xml Token), в память попадают только нужные поля
// физических лиц. Юридические лица пропускаются: проверяем людей.

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ListPerson — физическое лицо из перечня в нормализованном виде.
type ListPerson struct {
	ID       string
	Display  string   // ФИО как в перечне (для отчёта)
	Names    []string // нормализованные варианты имени: основное + другие
	Surnames []string // нормализованные фамилии (если перечень их выделяет)
	DOBs     []string // ГГГГ-ММ-ДД
	Years    []string // ГГГГ (когда известен только год)
	INNs     []string
	SNILSs   []string
	Docs     []string // серия+номер документа: только буквы и цифры, верхний регистр
}

// ListData — один разобранный перечень.
type ListData struct {
	Key     string // terrorism | mvk | oon
	Title   string // полное название для отчёта
	Short   string // короткое название для колонки
	Date    string // дата перечня ДД.ММ.ГГГГ
	File    string // путь к файлу, из которого читали
	Persons []ListPerson
	Err     error // перечень не найден или не разобран
}

// listSpec описывает, где искать файл перечня в папке раздела.
type listSpec struct {
	Key, Section, Title, Short string
	// match выбирает файл в папке раздела по имени (по приоритету).
	match []func(name string) bool
}

var checkLists = []listSpec{
	{
		Key: "terrorism", Section: "terrorism",
		Title: "Перечень организаций и физических лиц, в отношении которых имеются сведения об их причастности к экстремистской деятельности или терроризму",
		Short: "Перечень террористов и экстремистов",
		match: []func(string) bool{
			func(n string) bool {
				l := strings.ToLower(n)
				return strings.HasSuffix(l, ".xml") && !strings.HasPrefix(l, "perechen_")
			},
			func(n string) bool { return strings.EqualFold(n, "perechen_xml.zip") },
			func(n string) bool { return strings.EqualFold(n, "perechen_xml.xml") },
		},
	},
	{
		Key: "mvk", Section: "mvk",
		Title: "Перечень организаций и физических лиц, в отношении которых действует решение Межведомственной комиссии по противодействию финансированию терроризма о замораживании (блокировании) денежных средств или иного имущества",
		Short: "Перечень МВК",
		match: []func(string) bool{
			func(n string) bool { return strings.EqualFold(n, "perechen_mvk.xml") },
			func(n string) bool { return strings.EqualFold(n, "perechen_mvk.zip") },
		},
	},
	{
		Key: "oon", Section: "oon",
		Title: "Сводный санкционный перечень Совета Безопасности ООН",
		Short: "Перечень СБ ООН",
		match: []func(string) bool{
			func(n string) bool { return strings.EqualFold(n, "perechen_oon-rus.xml") },
			func(n string) bool { return strings.EqualFold(n, "perechen_oon-rus.zip") },
		},
	},
}

// isRunDirName — папка прогона вида 2026-08-17_09-00.
func isRunDirName(name string) bool {
	_, err := time.Parse("2006-01-02_15-04", name)
	return err == nil
}

// runDirsDesc возвращает папки прогонов от новых к старым.
func runDirsDesc(base string) ([]string, error) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil, err
	}
	var runs []string
	for _, e := range entries {
		if e.IsDir() && isRunDirName(e.Name()) {
			runs = append(runs, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(runs)))
	return runs, nil
}

// FindLatestListFile ищет самый свежий файл перечня: папки прогонов
// создаются только при изменениях, поэтому каждый раздел может лежать
// в своей папке (МВК — в старой, перечень террористов — в свежей).
func FindLatestListFile(base string, spec listSpec) (string, error) {
	runs, err := runDirsDesc(base)
	if err != nil {
		return "", err
	}
	for _, run := range runs {
		dir := filepath.Join(base, run, spec.Section)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, m := range spec.match {
			var found []string
			for _, e := range entries {
				if !e.IsDir() && !strings.HasSuffix(e.Name(), ".tmp") && m(e.Name()) {
					found = append(found, e.Name())
				}
			}
			if len(found) > 0 {
				sort.Strings(found)
				return filepath.Join(dir, found[len(found)-1]), nil
			}
		}
	}
	return "", fmt.Errorf("файл перечня «%s» не найден ни в одной папке выгрузки", spec.Short)
}

// openListXML возвращает содержимое XML: сам файл или первый .xml из zip.
func openListXML(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(filepath.Ext(path), ".zip") {
		return data, nil
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	for _, f := range zr.File {
		if strings.EqualFold(filepath.Ext(f.Name), ".xml") {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(io.LimitReader(rc, 1<<30))
		}
	}
	return nil, fmt.Errorf("%s: в архиве нет XML", filepath.Base(path))
}

// LoadCheckLists находит и разбирает все перечни. Ошибка по одному перечню
// не останавливает остальные: она попадёт в отчёт.
func LoadCheckLists(base string) []*ListData {
	var out []*ListData
	for _, spec := range checkLists {
		ld := &ListData{Key: spec.Key, Title: spec.Title, Short: spec.Short}
		out = append(out, ld)
		path, err := FindLatestListFile(base, spec)
		if err != nil {
			ld.Err = err
			continue
		}
		ld.File = path
		data, err := openListXML(path)
		if err != nil {
			ld.Err = fmt.Errorf("чтение %s: %w", path, err)
			continue
		}
		switch spec.Key {
		case "oon":
			ld.Persons, ld.Date, err = parseOON(data)
		default:
			ld.Persons, ld.Date, err = parseRosfinList(data)
		}
		if err != nil {
			ld.Err = fmt.Errorf("разбор %s: %w", filepath.Base(path), err)
			continue
		}
		if len(ld.Persons) == 0 {
			ld.Err = fmt.Errorf("в файле %s не найдено ни одного физического лица — формат изменился?", filepath.Base(path))
		}
	}
	return out
}

// isoToRu: 2026-10-23 -> 23.10.2026.
func isoToRu(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 10 {
		if t, err := time.Parse("2006-01-02", s[:10]); err == nil {
			return t.Format("02.01.2006")
		}
	}
	return s
}

func newXMLDecoder(data []byte) *xml.Decoder {
	dec := xml.NewDecoder(bytes.NewReader(data))
	// Файлы портала в UTF-8; на случай windows-1251 не падаем, а читаем как есть.
	dec.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) { return input, nil }
	return dec
}

// parseRosfinList разбирает перечень террористов и перечень МВК: у них общая
// структура субъекта (ИдСубъекта, ФЛ/ФИО, ДатаРождения, ИНН, СНИЛС,
// СписокДокументов, СписокДрНаименований).
func parseRosfinList(data []byte) ([]ListPerson, string, error) {
	dec := newXMLDecoder(data)
	var (
		stack    []string
		persons  []ListPerson
		listDate string
		cur      *ListPerson
		isFL     bool
		text     strings.Builder
		docSer   string
		docNum   string
	)
	parent := func(n int) string { // n=1 — родитель текущего элемента
		if len(stack)-1-n >= 0 {
			return stack[len(stack)-1-n]
		}
		return ""
	}
	inActual := func() bool {
		for _, s := range stack {
			if s == "АктуальныйПеречень" || s == "СписокАктуальныхРешений" {
				return true
			}
		}
		return false
	}
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			name := t.Name.Local
			stack = append(stack, name)
			text.Reset()
			switch name {
			case "Субъект":
				if inActual() && cur == nil {
					cur = &ListPerson{}
					isFL = false
				}
			case "ФЛ":
				if cur != nil {
					isFL = true
				}
			case "Документ":
				docSer, docNum = "", ""
			}
		case xml.CharData:
			text.Write(t)
		case xml.EndElement:
			name := t.Name.Local
			val := strings.TrimSpace(text.String())
			text.Reset()
			p := parent(1)
			switch {
			case cur == nil:
				if (name == "ДатаПеречня" || name == "ДатаСписка") && listDate == "" {
					listDate = isoToRu(val)
				}
			case name == "Субъект" && p != "ВклИсклСубъект":
				if isFL && (len(cur.Names) > 0 || len(cur.Surnames) > 0) {
					persons = append(persons, *cur)
				}
				cur = nil
			case name == "ИдСубъекта" && p == "Субъект":
				cur.ID = val
			case name == "ФИО" && p == "ФЛ":
				cur.Display = val
				addUnique(&cur.Names, normName(val))
			case name == "ФИО" && p == "ДрНаименование":
				addUnique(&cur.Names, normName(val))
			case name == "Фамилия" && p == "ФЛ":
				addUnique(&cur.Surnames, normName(val))
			case name == "ДатаРождения" && (p == "ФЛ" || p == "ДрНаименование"):
				if len(val) >= 10 {
					addUnique(&cur.DOBs, val[:10])
				}
			case name == "ГодРождения" && (p == "ФЛ" || p == "ДрНаименование"):
				addUnique(&cur.Years, val)
			case name == "ИНН" && p == "ФЛ":
				addUnique(&cur.INNs, digitsOnly(val))
			case name == "СНИЛС" && p == "ФЛ":
				addUnique(&cur.SNILSs, digitsOnly(val))
			case name == "Серия" && p == "Документ":
				docSer = val
			case name == "Номер" && p == "Документ":
				docNum = val
			case name == "Документ":
				if d := normDoc(docSer + docNum); d != "" {
					addUnique(&cur.Docs, d)
				}
			}
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}
	// Если в перечне нет отдельной фамилии — берём первое слово ФИО.
	for i := range persons {
		if len(persons[i].Surnames) == 0 {
			for _, n := range persons[i].Names {
				if f := strings.Fields(n); len(f) > 0 {
					addUnique(&persons[i].Surnames, f[0])
				}
			}
		}
		if persons[i].Display == "" && len(persons[i].Names) > 0 {
			persons[i].Display = persons[i].Names[0]
		}
	}
	return persons, listDate, nil
}

// parseOON разбирает сводный перечень СБ ООН (только INDIVIDUAL).
func parseOON(data []byte) ([]ListPerson, string, error) {
	dec := newXMLDecoder(data)
	var (
		stack    []string
		persons  []ListPerson
		listDate string
		cur      *ListPerson
		text     strings.Builder
		parts    [4]string
	)
	parent := func() string {
		if len(stack) >= 2 {
			return stack[len(stack)-2]
		}
		return ""
	}
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			name := t.Name.Local
			stack = append(stack, name)
			text.Reset()
			if name == "CONSOLIDATED_LIST" {
				for _, a := range t.Attr {
					if a.Name.Local == "dateGenerated" {
						listDate = oonDate(a.Value)
					}
				}
			}
			if name == "INDIVIDUAL" {
				cur = &ListPerson{}
				parts = [4]string{}
			}
		case xml.CharData:
			text.Write(t)
		case xml.EndElement:
			name := t.Name.Local
			val := strings.TrimSpace(text.String())
			text.Reset()
			p := parent()
			if cur != nil {
				switch {
				case name == "INDIVIDUAL":
					full := strings.TrimSpace(strings.Join(parts[:], " "))
					if full != "" {
						cur.Display = strings.Join(strings.Fields(full), " ")
						addUnique(&cur.Names, normName(full))
					}
					if len(cur.Names) > 0 {
						persons = append(persons, *cur)
					}
					cur = nil
				case name == "REFERENCE_NUMBER" && p == "INDIVIDUAL":
					cur.ID = val
				case name == "FIRST_NAME" && p == "INDIVIDUAL":
					parts[0] = val
				case name == "SECOND_NAME" && p == "INDIVIDUAL":
					parts[1] = val
				case name == "THIRD_NAME" && p == "INDIVIDUAL":
					parts[2] = val
				case name == "FOURTH_NAME" && p == "INDIVIDUAL":
					parts[3] = val
				case name == "NAME_ORIGINAL_SCRIPT" && p == "INDIVIDUAL":
					addUnique(&cur.Names, normName(val))
				case name == "ALIAS_NAME" && p == "INDIVIDUAL_ALIAS":
					addUnique(&cur.Names, normName(val))
				case name == "DATE" && p == "INDIVIDUAL_DATE_OF_BIRTH":
					if len(val) >= 10 {
						addUnique(&cur.DOBs, val[:10])
					}
				case (name == "YEAR" || name == "FROM_YEAR" || name == "TO_YEAR") && p == "INDIVIDUAL_DATE_OF_BIRTH":
					addUnique(&cur.Years, val)
				case name == "NUMBER" && p == "INDIVIDUAL_DOCUMENT":
					if d := normDoc(val); d != "" {
						addUnique(&cur.Docs, d)
					}
				}
			}
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}
	return persons, listDate, nil
}

// oonDate: время генерации перечня в UTC -> дата по Москве.
func oonDate(s string) string {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			if loc, err := time.LoadLocation("Europe/Moscow"); err == nil {
				t = t.In(loc)
			}
			return t.Format("02.01.2006")
		}
	}
	return isoToRu(s)
}

func addUnique(list *[]string, v string) {
	if v == "" {
		return
	}
	for _, x := range *list {
		if x == v {
			return
		}
	}
	*list = append(*list, v)
}
