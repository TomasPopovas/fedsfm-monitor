package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testTerrorXML = `<?xml version="1.0" encoding="UTF-8"?>
<Перечень>
  <ВерсияФормата>2.2</ВерсияФормата>
  <ДатаПеречня>2026-10-23</ДатаПеречня>
  <АктуальныйПеречень>
    <Субъект>
      <ИдСубъекта>1001</ИдСубъекта>
      <ФЛ>
        <ФИО>ПЕТРОВ ПЁТР ПЕТРОВИЧ</ФИО>
        <Фамилия>ПЕТРОВ</Фамилия><Имя>ПЁТР</Имя><Отчество>ПЕТРОВИЧ</Отчество>
        <ДатаРождения>1980-05-01</ДатаРождения><ГодРождения>1980</ГодРождения>
        <ИНН>500100732259</ИНН>
        <СНИЛС>112-233-445 95</СНИЛС>
        <СписокДокументов><Документ><Серия>4510</Серия><Номер>123456</Номер></Документ></СписокДокументов>
        <СписокДрНаименований><ДрНаименование><ФИО>ПЕТРОВСКИЙ ПЁТР ПЕТРОВИЧ</ФИО><ДатаРождения>1980-05-01</ДатаРождения></ДрНаименование></СписокДрНаименований>
      </ФЛ>
    </Субъект>
    <Субъект>
      <ИдСубъекта>1002</ИдСубъекта>
      <ФЛ>
        <ФИО>СИДОРОВ СИДОР СИДОРОВИЧ</ФИО>
        <Фамилия>СИДОРОВ</Фамилия>
        <ГодРождения>1975</ГодРождения>
      </ФЛ>
    </Субъект>
    <Субъект>
      <ИдСубъекта>1003</ИдСубъекта>
      <ФЛ><ФИО>ИВАНОВ ИВАН ИВАНОВИЧ</ФИО><Фамилия>ИВАНОВ</Фамилия><ДатаРождения>1990-01-01</ДатаРождения></ФЛ>
    </Субъект>
    <Субъект>
      <ИдСубъекта>1004</ИдСубъекта>
      <ЮЛ><Наименование>ООО РОМАШКА</Наименование><ИНН>7700000000</ИНН></ЮЛ>
    </Субъект>
  </АктуальныйПеречень>
  <ПоследниеИсключенные>
    <Субъект><ИдСубъекта>9</ИдСубъекта><ФЛ><ФИО>ИСКЛЮЧЁННЫЙ ИЗ ПЕРЕЧНЯ</ФИО></ФЛ></Субъект>
  </ПоследниеИсключенные>
</Перечень>`

const testMvkXML = `<СписокРешений><ДатаСписка>2026-06-25</ДатаСписка><СписокАктуальныхРешений>
<Решение><СписокСубъектов><Субъект><ИдСубъекта>2001</ИдСубъекта><ФЛ><ФИО>КУЗНЕЦОВ АНДРЕЙ</ФИО><Фамилия>КУЗНЕЦОВ</Фамилия><ДатаРождения>1985-03-03</ДатаРождения><СписокДокументов><Документ><Номер>AN3811678</Номер></Документ></СписокДокументов></ФЛ></Субъект></СписокСубъектов></Решение>
<Решение><СписокСубъектов><Субъект><ИдСубъекта>2001</ИдСубъекта><ФЛ><ФИО>КУЗНЕЦОВ АНДРЕЙ</ФИО><Фамилия>КУЗНЕЦОВ</Фамилия><ДатаРождения>1985-03-03</ДатаРождения></ФЛ></Субъект></СписокСубъектов></Решение>
</СписокАктуальныхРешений></СписокРешений>`

const testOonXML = `<?xml version="1.0" encoding="UTF-8"?>
<CONSOLIDATED_LIST dateGenerated="2026-10-15T19:45:09.591Z"><INDIVIDUALS>
<INDIVIDUAL><REFERENCE_NUMBER>QDi.001</REFERENCE_NUMBER><FIRST_NAME>АХМЕД</FIRST_NAME><SECOND_NAME>АЛИЕВ</SECOND_NAME>
<INDIVIDUAL_ALIAS><ALIAS_NAME>Ahmed Aliev</ALIAS_NAME></INDIVIDUAL_ALIAS>
<INDIVIDUAL_DATE_OF_BIRTH><TYPE_OF_DATE>EXACT</TYPE_OF_DATE><DATE>1970-07-07</DATE></INDIVIDUAL_DATE_OF_BIRTH>
<INDIVIDUAL_DOCUMENT><NUMBER>C 123 456</NUMBER></INDIVIDUAL_DOCUMENT></INDIVIDUAL>
</INDIVIDUALS><ENTITIES><ENTITY><FIRST_NAME>НЕ ЧЕЛОВЕК</FIRST_NAME></ENTITY></ENTITIES></CONSOLIDATED_LIST>`

// testClientsXLSX собирает xlsx так, как его сохраняет Excel: строки через
// sharedStrings, даты и паспорт — числами.
func testClientsXLSX(t *testing.T, rows [][]any) []byte {
	t.Helper()
	var shared []string
	idx := map[string]int{}
	var sheet strings.Builder
	sheet.WriteString(`<?xml version="1.0" encoding="UTF-8"?><worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`)
	for ri, row := range rows {
		fmt.Fprintf(&sheet, `<row r="%d">`, ri+1)
		for ci, v := range row {
			ref := colName(ci) + fmt.Sprint(ri+1)
			switch x := v.(type) {
			case nil:
			case string:
				i, ok := idx[x]
				if !ok {
					i = len(shared)
					idx[x] = i
					shared = append(shared, x)
				}
				fmt.Fprintf(&sheet, `<c r="%s" t="s"><v>%d</v></c>`, ref, i)
			case float64:
				fmt.Fprintf(&sheet, `<c r="%s" s="1"><v>%s</v></c>`, ref, formatNumber(x))
			case int:
				fmt.Fprintf(&sheet, `<c r="%s"><v>%d</v></c>`, ref, x)
			}
		}
		sheet.WriteString(`</row>`)
	}
	sheet.WriteString(`</sheetData></worksheet>`)
	var ss strings.Builder
	ss.WriteString(`<?xml version="1.0" encoding="UTF-8"?><sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">`)
	for _, s := range shared {
		fmt.Fprintf(&ss, `<si><t xml:space="preserve">%s</t></si>`, xmlEscape(s))
	}
	ss.WriteString(`</sst>`)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	files := map[string]string{
		"xl/workbook.xml":            `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="Лист1" sheetId="1" r:id="rId1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="x" Target="worksheets/sheet1.xml"/></Relationships>`,
		"xl/worksheets/sheet1.xml":   sheet.String(),
		"xl/sharedStrings.xml":       ss.String(),
	}
	for name, content := range files {
		w, _ := zw.Create(name)
		_, _ = io.WriteString(w, content)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

var testHeader = []any{"Фамилия", "Имя", "Отчество", "Дата рождения", "Паспорт", "место рождения", "ИНН", "СНИЛС"}

// Excel-серийники: 1980-05-01 = 29342, 1990-01-01 = 32874, 1985-03-03 = 31109, 1970-07-07 = 25756
func setupCheckDir(t *testing.T, rows [][]any) Config {
	t.Helper()
	base := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(base, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Разделы в разных папках, как на NAS: МВК — в старой.
	write("2026-09-19_08-41/mvk/perechen_mvk.xml", testMvkXML)
	write("2026-10-16_09-30/oon/perechen_oon-rus.xml", testOonXML)
	write("2026-10-23_17-45/terrorism/23.10.2026-v 2.2.xml", testTerrorXML)
	write("2026-10-23_17-45/terrorism/perechen_xml.zip", "not used")
	write("2026-08-30_17-06/27.08.2026-v 2.2.xml", "<old flat layout/>")

	clients := testClientsXLSX(t, append([][]any{testHeader}, rows...))
	p := filepath.Join(base, "проверка", "Список клиентов на проверку.xlsx")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, clients, 0o644); err != nil {
		t.Fatal(err)
	}
	return Config{OutputDir: base, CheckEnabled: true, CheckFile: p, CheckAttach: true}
}

func TestParseLists(t *testing.T) {
	ps, date, err := parseRosfinList([]byte(testTerrorXML))
	if err != nil {
		t.Fatal(err)
	}
	if date != "23.10.2026" {
		t.Fatalf("date = %q", date)
	}
	if len(ps) != 3 {
		t.Fatalf("ожидалось 3 физлица (ЮЛ и исключённые не считаются), got %d", len(ps))
	}
	p := ps[0]
	if p.ID != "1001" || len(p.Names) != 2 || p.Names[0] != "ПЕТРОВ ПЕТР ПЕТРОВИЧ" ||
		p.INNs[0] != "500100732259" || p.SNILSs[0] != "11223344595" || p.Docs[0] != "4510123456" {
		t.Fatalf("неверный разбор: %+v", p)
	}

	mv, mdate, err := parseRosfinList([]byte(testMvkXML))
	if err != nil || mdate != "25.06.2026" || len(mv) != 2 {
		t.Fatalf("МВК: %v %q %d", err, mdate, len(mv))
	}

	on, odate, err := parseOON([]byte(testOonXML))
	if err != nil || odate != "15.10.2026" || len(on) != 1 {
		t.Fatalf("ООН: %v %q %d", err, odate, len(on))
	}
	if !contains(on[0].Names, "АХМЕД АЛИЕВ") || on[0].Docs[0] != "C123456" {
		t.Fatalf("ООН: %+v", on[0])
	}
}

func TestFindLatestListFile(t *testing.T) {
	cfg := setupCheckDir(t, nil)
	want := map[string]string{
		"terrorism": "2026-10-23_17-45/terrorism/23.10.2026-v 2.2.xml",
		"mvk":       "2026-09-19_08-41/mvk/perechen_mvk.xml",
		"oon":       "2026-10-16_09-30/oon/perechen_oon-rus.xml",
	}
	for _, spec := range checkLists {
		got, err := FindLatestListFile(cfg.OutputDir, spec)
		if err != nil {
			t.Fatal(err)
		}
		if got != filepath.Join(cfg.OutputDir, want[spec.Key]) {
			t.Fatalf("%s: %s", spec.Key, got)
		}
	}
}

func cust(s, n, p, dob, inn, snils, pass string) Customer {
	c := Customer{Surname: s, Name: n, Patronymic: p, DOB: dob, INN: inn, SNILS: snils, Passport: pass}
	c.normSurname, c.normName, c.normFull = normName(s), normName(n), normName(c.FullName())
	return c
}

func TestMatchLevels(t *testing.T) {
	ps, _, _ := parseRosfinList([]byte(testTerrorXML))
	petrov, sidorov, ivanov := ps[0], ps[1], ps[2]
	on, _, _ := parseOON([]byte(testOonXML))

	cases := []struct {
		name string
		c    Customer
		p    ListPerson
		oon  bool
		want int
	}{
		{"ФИО+ДР, Ё/Е и регистр", cust("Петров", "Пётр", "петрович", "1980-05-01", "", "", ""), petrov, false, LevelMatch},
		{"другое имя из перечня", cust("Петровский", "Петр", "Петрович", "1980-05-01", "", "", ""), petrov, false, LevelMatch},
		{"латинская C в фамилии", cust("Петров", "Петр", "Пeтрович", "1980-05-01", "", "", ""), petrov, false, LevelMatch},
		{"только ИНН", cust("Другой", "Человек", "", "", "500100732259", "", ""), petrov, false, LevelMatch},
		{"только СНИЛС", cust("Другой", "Человек", "", "", "", "11223344595", ""), petrov, false, LevelMatch},
		{"только паспорт", cust("Другой", "Человек", "", "", "", "", "4510123456"), petrov, false, LevelMatch},
		{"ФИО без ДР у клиента", cust("Петров", "Петр", "Петрович", "", "", "", ""), petrov, false, LevelPossible},
		{"фамилия+имя+ДР, другое отчество", cust("Петров", "Петр", "Иванович", "1980-05-01", "", "", ""), petrov, false, LevelPossible},
		{"фамилия+ДР, опечатка в имени", cust("Петров", "Птр", "Петрович", "1980-05-01", "", "", ""), petrov, false, LevelPossible},
		{"тёзка с другой ДР", cust("Петров", "Петр", "Петрович", "1981-05-01", "", "", ""), petrov, false, LevelNamesake},
		{"в перечне только год — совпал", cust("Сидоров", "Сидор", "Сидорович", "1975-11-11", "", "", ""), sidorov, false, LevelMatch},
		{"в перечне только год — другой", cust("Сидоров", "Сидор", "Сидорович", "1976-11-11", "", "", ""), sidorov, false, LevelNamesake},
		{"посторонний", cust("Смирнов", "Олег", "Олегович", "1990-01-01", "", "", ""), ivanov, false, LevelNone},
		{"ООН: порядок имени другой", cust("Алиев", "Ахмед", "", "1970-07-07", "", "", ""), on[0], true, LevelPossible},
		{"ООН: имя без ДР", cust("Алиев", "Ахмед", "", "", "", "", ""), on[0], true, LevelPossible},
		{"ООН: паспорт", cust("Х", "Y", "", "", "", "", "C123456"), on[0], true, LevelMatch},
	}
	for _, tc := range cases {
		got, why := MatchClient(tc.c, tc.p, tc.oon)
		if got != tc.want {
			t.Errorf("%s: уровень %d, want %d (%v)", tc.name, got, tc.want, why)
		}
	}
}

func TestValidators(t *testing.T) {
	if !validINN12("500100732259") || validINN12("500100732258") {
		t.Fatal("ИНН")
	}
	if !validSNILS("11223344595") || validSNILS("11223344596") {
		t.Fatal("СНИЛС")
	}
	if normName("  петров-водкин  Кузьма ") != "ПЕТРОВ ВОДКИН КУЗЬМА" {
		t.Fatal(normName("  петров-водкин  Кузьма "))
	}
	if nameProblem("Пeтров") == "" || nameProblem("Петров") != "" || nameProblem("Петров2") == "" {
		t.Fatal("nameProblem")
	}
}

func TestReadClientsIssues(t *testing.T) {
	cfg := setupCheckDir(t, [][]any{
		{"Петров ", "Пётр", "Петрович", 29342.0, 4510123456, "г. Москва", "500100732259", "112-233-445 95"},
		{"Смирнов", "Олег", nil, "1.2.1990", 312345678, nil, 50100732259, "11223344596"},
		{nil, nil, nil, nil, nil, nil, nil, nil},
		{"Кузнeцов", "Андрей", nil, 31109.0, nil, nil, nil, nil},
	})
	cl, issues, err := ReadClients(cfg.CheckFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(cl) != 3 {
		t.Fatalf("клиентов %d", len(cl))
	}
	if cl[0].Surname != "Петров" || cl[0].DOB != "1980-05-01" || cl[0].Passport != "4510123456" {
		t.Fatalf("строка 2: %+v", cl[0])
	}
	if cl[1].DOB != "1990-02-01" || cl[1].Passport != "0312345678" || cl[1].INN != "050100732259" {
		t.Fatalf("строка 3: %+v", cl[1])
	}
	var all []string
	for _, is := range issues {
		all = append(all, fmt.Sprintf("%d %s %s", is.Row, is.Field, is.Problem))
	}
	joined := strings.Join(all, "\n")
	for _, want := range []string{
		"2 Фамилия лишние пробелы",
		"3 Паспорт хранится числом, потерян ведущий ноль",
		"3 ИНН хранится числом, потерян ведущий ноль",
		"3 СНИЛС не сходится контрольное число",
		"3 Дата рождения дата записана текстом",
		"5 Фамилия латинские буквы",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("нет замечания %q в:\n%s", want, joined)
		}
	}
}

func TestRunClientCheckEndToEnd(t *testing.T) {
	cfg := setupCheckDir(t, [][]any{
		{"Петров", "Пётр", "Петрович", 29342.0, nil, nil, nil, nil},              // совпадение (террористы)
		{"Кузнецов", "Андрей", nil, 31109.0, nil, nil, nil, nil},                 // МВК: ФИО без отчества + ДР
		{"Алиев", "Ахмед", nil, 25756.0, nil, nil, nil, nil},                     // ООН: требует проверки
		{"Чистый", "Клиент", "Тестович", 32874.0, nil, nil, "500100732259", nil}, // ИНН совпал — совпадение
		{"Совсем", "Другой", "Человек", 32874.0, nil, nil, nil, nil},             // нет
	})
	if err := RunClientCheck(cfg, false); err != nil {
		t.Fatal(err)
	}
	st, _ := LoadState(cfg.OutputDir)
	if st.Check.Fingerprint == "" || st.Check.Matches != 3 || st.Check.Possible != 1 {
		t.Fatalf("state: %+v", st.Check)
	}
	report, err := os.ReadFile(st.Check.LastReport)
	if err != nil {
		t.Fatal(err)
	}
	sh, err := ReadFirstSheet(report)
	if err != nil {
		t.Fatalf("отчёт не читается как xlsx: %v", err)
	}
	var text []string
	for _, r := range sortedKeys(sh.Rows) {
		for _, c := range sh.Rows[r] {
			text = append(text, c.Text)
		}
	}
	all := strings.Join(text, "\n")
	for _, want := range []string{"СОВПАДЕНИЕ: ПЕТРОВ ПЁТР ПЕТРОВИЧ", "от 23.10.2026", "от 25.06.2026", "от 15.10.2026", "Требует проверки", "Совпадений не выявлено"} {
		if !strings.Contains(all, want) {
			t.Errorf("в отчёте нет %q", want)
		}
	}

	// Второй запуск без изменений — ничего не делает.
	if err := RunClientCheck(cfg, false); err != nil {
		t.Fatal(err)
	}
	st2, _ := LoadState(cfg.OutputDir)
	if st2.Check.LastRun != st.Check.LastRun {
		t.Fatal("повторная проверка без изменений не должна запускаться")
	}
	// Изменился список клиентов — проверка запускается.
	if err := os.WriteFile(cfg.CheckFile, testClientsXLSX(t, [][]any{testHeader, {"Новый", "Клиент", nil, 32874.0}}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RunClientCheck(cfg, false); err != nil {
		t.Fatal(err)
	}
	st3, _ := LoadState(cfg.OutputDir)
	if st3.Check.Fingerprint == st.Check.Fingerprint || st3.Check.Matches != 0 {
		t.Fatalf("после смены списка: %+v", st3.Check)
	}
	if st3.Check.LastReport == st.Check.LastReport {
		t.Fatal("второй отчёт за день должен получить отдельное имя")
	}
}

func TestMultipartMessage(t *testing.T) {
	raw := buildMultipart("a@x.ru", "b@x.ru", "тема", "текст", []MailAttachment{{Name: "Отчёт_о_проверке_24-10-2026.xlsx", ContentType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", Data: []byte("PK\x03\x04data")}})
	msg, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	_, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	var names []string
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, p.FileName())
	}
	if len(names) != 2 || names[1] != "Отчёт_о_проверке_24-10-2026.xlsx" {
		t.Fatalf("части письма: %q", names)
	}
}

func TestPruneKeepsLatestOfEachSection(t *testing.T) {
	base := t.TempDir()
	mk := func(p string) {
		if err := os.MkdirAll(filepath.Join(base, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mk("2026-06-01_09-00/mvk")
	mk("2026-07-01_09-00/terrorism")
	mk("2026-08-01_09-00/terrorism")
	mk("2026-09-01_09-00/terrorism")
	removed, err := PruneOldRuns(base, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 || removed[0] != "2026-07-01_09-00" || removed[1] != "2026-08-01_09-00" {
		t.Fatalf("removed = %v", removed)
	}
	if _, err := os.Stat(filepath.Join(base, "2026-06-01_09-00")); err != nil {
		t.Fatal("единственная папка МВК не должна удаляться")
	}
}

func TestHyphenSurnameFound(t *testing.T) {
	l := &ListData{Key: "terrorism", Persons: []ListPerson{{ID: "7", Display: "ПЕТРОВ-ВОДКИН КУЗЬМА СЕРГЕЕВИЧ",
		Names: []string{normName("ПЕТРОВ-ВОДКИН КУЗЬМА СЕРГЕЕВИЧ")}, Surnames: []string{normName("ПЕТРОВ-ВОДКИН")}, DOBs: []string{"1878-11-05"}}}}
	res := RunMatching([]Customer{cust("Петров-Водкин", "Кузьма", "Сергеевич", "1878-11-05", "", "", "")}, []*ListData{l})
	if res[0].Level("") != LevelMatch {
		t.Fatalf("дефисная фамилия не найдена: %+v", res[0].Hits)
	}
}
