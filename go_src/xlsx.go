package main

// Минимальная работа с xlsx на стандартной библиотеке: чтение первого листа
// (список клиентов) и запись простого оформленного отчёта.
//
// Чтение возвращает ячейки как текст плюс признак «ячейка была числом»:
// это нужно, чтобы распознать даты (Excel хранит их числом дней) и
// заметить потерянные ведущие нули у ИНН, СНИЛС и паспорта.

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// XCell — одна прочитанная ячейка.
type XCell struct {
	Text    string // значение как текст (числа — без экспоненты)
	Numeric bool   // ячейка хранилась числом
	Num     float64
}

// XSheet — прочитанный лист: строки по номерам Excel (с 1).
type XSheet struct {
	Name     string
	Rows     map[int]map[int]XCell // строка -> колонка (с 0) -> ячейка
	MaxRow   int
	Date1904 bool
}

// Cell возвращает ячейку или пустую.
func (s *XSheet) Cell(row, col int) XCell {
	if r, ok := s.Rows[row]; ok {
		return r[col]
	}
	return XCell{}
}

// ReadFirstSheet читает первый лист книги.
func ReadFirstSheet(data []byte) (*XSheet, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("файл не является xlsx (zip): %w", err)
	}
	files := map[string]*zip.File{}
	for _, f := range zr.File {
		files[strings.TrimPrefix(f.Name, "/")] = f
	}
	readFile := func(name string) ([]byte, error) {
		f, ok := files[name]
		if !ok {
			return nil, fmt.Errorf("в xlsx нет %s", name)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(io.LimitReader(rc, 64<<20))
	}

	// workbook.xml: первый лист и система дат.
	wbData, err := readFile("xl/workbook.xml")
	if err != nil {
		return nil, err
	}
	var wb struct {
		Pr struct {
			Date1904 string `xml:"date1904,attr"`
		} `xml:"workbookPr"`
		Sheets []struct {
			Name string `xml:"name,attr"`
			RID  string `xml:"http://schemas.openxmlformats.org/officeDocument/2006/relationships id,attr"`
		} `xml:"sheets>sheet"`
	}
	if err := xml.Unmarshal(wbData, &wb); err != nil {
		return nil, fmt.Errorf("workbook.xml: %w", err)
	}
	if len(wb.Sheets) == 0 {
		return nil, fmt.Errorf("в книге нет листов")
	}

	sheetPath := "xl/worksheets/sheet1.xml"
	if relData, err := readFile("xl/_rels/workbook.xml.rels"); err == nil {
		var rels struct {
			Rel []struct {
				ID     string `xml:"Id,attr"`
				Target string `xml:"Target,attr"`
			} `xml:"Relationship"`
		}
		if xml.Unmarshal(relData, &rels) == nil {
			for _, r := range rels.Rel {
				if r.ID == wb.Sheets[0].RID {
					t := r.Target
					if strings.HasPrefix(t, "/") {
						sheetPath = strings.TrimPrefix(t, "/")
					} else {
						sheetPath = path.Clean("xl/" + t)
					}
				}
			}
		}
	}

	// sharedStrings.xml необязателен.
	var shared []string
	if ssData, err := readFile("xl/sharedStrings.xml"); err == nil {
		shared, err = parseSharedStrings(ssData)
		if err != nil {
			return nil, fmt.Errorf("sharedStrings.xml: %w", err)
		}
	}

	shData, err := readFile(sheetPath)
	if err != nil {
		return nil, err
	}
	sheet := &XSheet{
		Name:     wb.Sheets[0].Name,
		Rows:     map[int]map[int]XCell{},
		Date1904: wb.Pr.Date1904 == "1" || strings.EqualFold(wb.Pr.Date1904, "true"),
	}
	if err := parseSheet(shData, shared, sheet); err != nil {
		return nil, fmt.Errorf("лист %s: %w", sheet.Name, err)
	}
	return sheet, nil
}

func parseSharedStrings(data []byte) ([]string, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	var out []string
	var cur strings.Builder
	inSI, inT, inRPh := false, false, false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "si":
				inSI = true
				cur.Reset()
			case "t":
				inT = true
			case "rPh": // фонетические подсказки не являются текстом ячейки
				inRPh = true
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "si":
				inSI = false
				out = append(out, cur.String())
			case "t":
				inT = false
			case "rPh":
				inRPh = false
			}
		case xml.CharData:
			if inSI && inT && !inRPh {
				cur.Write(t)
			}
		}
	}
	return out, nil
}

func parseSheet(data []byte, shared []string, sheet *XSheet) error {
	dec := xml.NewDecoder(bytes.NewReader(data))
	var (
		rowNum              int
		ref, typ            string
		val, inl            strings.Builder
		inV, inIS, inT, inC bool
	)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "row":
				rowNum = 0
				for _, a := range t.Attr {
					if a.Name.Local == "r" {
						rowNum, _ = strconv.Atoi(a.Value)
					}
				}
				if rowNum == 0 {
					rowNum = sheet.MaxRow + 1
				}
			case "c":
				inC = true
				ref, typ = "", ""
				val.Reset()
				inl.Reset()
				for _, a := range t.Attr {
					switch a.Name.Local {
					case "r":
						ref = a.Value
					case "t":
						typ = a.Value
					}
				}
			case "v":
				inV = true
			case "is":
				inIS = true
			case "t":
				inT = true
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "v":
				inV = false
			case "is":
				inIS = false
			case "t":
				inT = false
			case "c":
				inC = false
				col := colIndex(ref)
				if col < 0 {
					continue
				}
				cell := XCell{}
				raw := val.String()
				switch typ {
				case "s":
					if i, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && i >= 0 && i < len(shared) {
						cell.Text = shared[i]
					}
				case "inlineStr":
					cell.Text = inl.String()
				case "str", "e":
					cell.Text = raw
				case "b":
					cell.Text = raw
				default: // число
					raw = strings.TrimSpace(raw)
					if raw != "" {
						if f, err := strconv.ParseFloat(raw, 64); err == nil {
							cell.Numeric = true
							cell.Num = f
							cell.Text = formatNumber(f)
						} else {
							cell.Text = raw
						}
					}
				}
				if cell.Text == "" && !cell.Numeric {
					continue
				}
				if sheet.Rows[rowNum] == nil {
					sheet.Rows[rowNum] = map[int]XCell{}
				}
				sheet.Rows[rowNum][col] = cell
				if rowNum > sheet.MaxRow {
					sheet.MaxRow = rowNum
				}
			}
		case xml.CharData:
			if inC && inV {
				val.Write(t)
			}
			if inC && inIS && inT {
				inl.Write(t)
			}
		}
	}
	return nil
}

// formatNumber печатает число без экспоненты и лишних нулей.
func formatNumber(f float64) string {
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return strconv.FormatFloat(f, 'f', 0, 64)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// colIndex превращает "AB12" в номер колонки с нуля.
func colIndex(ref string) int {
	n := 0
	seen := false
	for _, r := range ref {
		if r >= 'A' && r <= 'Z' {
			n = n*26 + int(r-'A'+1)
			seen = true
		} else if r >= 'a' && r <= 'z' {
			n = n*26 + int(r-'a'+1)
			seen = true
		} else {
			break
		}
	}
	if !seen {
		return -1
	}
	return n - 1
}

func colName(i int) string {
	s := ""
	i++
	for i > 0 {
		i--
		s = string(rune('A'+i%26)) + s
		i /= 26
	}
	return s
}

// ExcelSerialToDate переводит число дней Excel в дату.
func ExcelSerialToDate(serial float64, date1904 bool) (time.Time, bool) {
	if serial < 1 || serial > 2958465 {
		return time.Time{}, false
	}
	base := time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)
	if date1904 {
		base = time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	days := int(math.Floor(serial))
	return base.AddDate(0, 0, days), true
}

// ---------------- запись ----------------

// Стили отчёта (индексы в cellXfs, см. reportStylesXML).
const (
	StDefault  = 0
	StTitle    = 1 // крупный жирный
	StHeader   = 2 // шапка таблицы: жирный, серый фон, рамка, перенос
	StCell     = 3 // обычная ячейка таблицы: рамка, перенос
	StMatch    = 4 // совпадение: красный фон
	StPossible = 5 // требует проверки: жёлтый фон
	StOK       = 6 // нет совпадений: зелёный фон
	StBold     = 7 // жирный без рамки, с переносом
	StWrap     = 8 // обычный с переносом
)

// WCell — ячейка для записи.
type WCell struct {
	Text  string
	Style int
}

// WSheet — лист для записи.
type WSheet struct {
	Name    string
	Widths  []float64 // ширины колонок
	Rows    [][]WCell
	Heights map[int]float64 // номер строки (с 0) -> высота
	Merges  []string        // диапазоны вида "A1:E1"
}

// Add добавляет строку и возвращает её индекс (с 0).
func (s *WSheet) Add(cells ...WCell) int {
	s.Rows = append(s.Rows, cells)
	return len(s.Rows) - 1
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func (s *WSheet) xml() []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n")
	b.WriteString(`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">`)
	// Печать: альбомная, вписать по ширине страницы.
	b.WriteString(`<sheetPr><pageSetUpPr fitToPage="1"/></sheetPr>`)
	if len(s.Widths) > 0 {
		b.WriteString("<cols>")
		for i, w := range s.Widths {
			fmt.Fprintf(&b, `<col min="%d" max="%d" width="%.2f" customWidth="1"/>`, i+1, i+1, w)
		}
		b.WriteString("</cols>")
	}
	b.WriteString("<sheetData>")
	for ri, row := range s.Rows {
		if h, ok := s.Heights[ri]; ok {
			fmt.Fprintf(&b, `<row r="%d" ht="%.1f" customHeight="1">`, ri+1, h)
		} else {
			fmt.Fprintf(&b, `<row r="%d">`, ri+1)
		}
		for ci, c := range row {
			if c.Text == "" && c.Style == 0 {
				continue
			}
			ref := colName(ci) + strconv.Itoa(ri+1)
			if c.Text == "" {
				fmt.Fprintf(&b, `<c r="%s" s="%d"/>`, ref, c.Style)
				continue
			}
			fmt.Fprintf(&b, `<c r="%s" s="%d" t="inlineStr"><is><t xml:space="preserve">%s</t></is></c>`,
				ref, c.Style, xmlEscape(c.Text))
		}
		b.WriteString("</row>")
	}
	b.WriteString("</sheetData>")
	if len(s.Merges) > 0 {
		fmt.Fprintf(&b, `<mergeCells count="%d">`, len(s.Merges))
		for _, m := range s.Merges {
			fmt.Fprintf(&b, `<mergeCell ref="%s"/>`, m)
		}
		b.WriteString("</mergeCells>")
	}
	b.WriteString(`<pageMargins left="0.5" right="0.5" top="0.6" bottom="0.6" header="0.3" footer="0.3"/>`)
	b.WriteString(`<pageSetup paperSize="9" orientation="landscape" fitToWidth="1" fitToHeight="0"/>`)
	b.WriteString("</worksheet>")
	return []byte(b.String())
}

const reportStylesXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
<fonts count="3">
<font><sz val="11"/><name val="Calibri"/><family val="2"/></font>
<font><b/><sz val="14"/><name val="Calibri"/><family val="2"/></font>
<font><b/><sz val="11"/><name val="Calibri"/><family val="2"/></font>
</fonts>
<fills count="6">
<fill><patternFill patternType="none"/></fill>
<fill><patternFill patternType="gray125"/></fill>
<fill><patternFill patternType="solid"><fgColor rgb="FFD9D9D9"/><bgColor indexed="64"/></patternFill></fill>
<fill><patternFill patternType="solid"><fgColor rgb="FFFFC7CE"/><bgColor indexed="64"/></patternFill></fill>
<fill><patternFill patternType="solid"><fgColor rgb="FFFFEB9C"/><bgColor indexed="64"/></patternFill></fill>
<fill><patternFill patternType="solid"><fgColor rgb="FFC6EFCE"/><bgColor indexed="64"/></patternFill></fill>
</fills>
<borders count="2">
<border><left/><right/><top/><bottom/><diagonal/></border>
<border><left style="thin"><color auto="1"/></left><right style="thin"><color auto="1"/></right><top style="thin"><color auto="1"/></top><bottom style="thin"><color auto="1"/></bottom><diagonal/></border>
</borders>
<cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs>
<cellXfs count="9">
<xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/>
<xf numFmtId="0" fontId="1" fillId="0" borderId="0" xfId="0" applyFont="1"/>
<xf numFmtId="0" fontId="2" fillId="2" borderId="1" xfId="0" applyFont="1" applyFill="1" applyBorder="1" applyAlignment="1"><alignment horizontal="center" vertical="center" wrapText="1"/></xf>
<xf numFmtId="0" fontId="0" fillId="0" borderId="1" xfId="0" applyBorder="1" applyAlignment="1"><alignment vertical="center" wrapText="1"/></xf>
<xf numFmtId="0" fontId="2" fillId="3" borderId="1" xfId="0" applyFont="1" applyFill="1" applyBorder="1" applyAlignment="1"><alignment vertical="center" wrapText="1"/></xf>
<xf numFmtId="0" fontId="0" fillId="4" borderId="1" xfId="0" applyFill="1" applyBorder="1" applyAlignment="1"><alignment vertical="center" wrapText="1"/></xf>
<xf numFmtId="0" fontId="0" fillId="5" borderId="1" xfId="0" applyFill="1" applyBorder="1" applyAlignment="1"><alignment vertical="center" wrapText="1"/></xf>
<xf numFmtId="0" fontId="2" fillId="0" borderId="0" xfId="0" applyFont="1" applyAlignment="1"><alignment vertical="top" wrapText="1"/></xf>
<xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0" applyAlignment="1"><alignment vertical="top" wrapText="1"/></xf>
</cellXfs>
<cellStyles count="1"><cellStyle name="Normal" xfId="0" builtinId="0"/></cellStyles>
</styleSheet>`

// WriteWorkbook собирает xlsx из листов.
func WriteWorkbook(sheets []*WSheet) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name, content string) error {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = io.WriteString(w, content)
		return err
	}

	var ct, wbSheets, wbRels strings.Builder
	ct.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n")
	ct.WriteString(`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">`)
	ct.WriteString(`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>`)
	ct.WriteString(`<Default Extension="xml" ContentType="application/xml"/>`)
	ct.WriteString(`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>`)
	ct.WriteString(`<Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>`)
	ct.WriteString(`<Override PartName="/docProps/core.xml" ContentType="application/vnd.openxmlformats-package.core-properties+xml"/>`)
	ct.WriteString(`<Override PartName="/docProps/app.xml" ContentType="application/vnd.openxmlformats-officedocument.extended-properties+xml"/>`)

	usedNames := map[string]bool{}
	for i, s := range sheets {
		n := i + 1
		fmt.Fprintf(&ct, `<Override PartName="/xl/worksheets/sheet%d.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`, n)
		name := sanitizeSheetName(s.Name, n, usedNames)
		fmt.Fprintf(&wbSheets, `<sheet name="%s" sheetId="%d" r:id="rId%d"/>`, xmlEscape(name), n, n)
		fmt.Fprintf(&wbRels, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet%d.xml"/>`, n, n)
		if err := add(fmt.Sprintf("xl/worksheets/sheet%d.xml", n), string(s.xml())); err != nil {
			return nil, err
		}
	}
	ct.WriteString(`</Types>`)
	stylesRID := len(sheets) + 1
	fmt.Fprintf(&wbRels, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>`, stylesRID)

	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	parts := []struct{ name, content string }{
		{"[Content_Types].xml", ct.String()},
		{"_rels/.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/><Relationship Id="rId2" Type="http://schemas.openxmlformats.org/package/2006/relationships/metadata/core-properties" Target="docProps/core.xml"/><Relationship Id="rId3" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/extended-properties" Target="docProps/app.xml"/></Relationships>`},
		{"xl/workbook.xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets>` + wbSheets.String() + `</sheets></workbook>`},
		{"xl/_rels/workbook.xml.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` + wbRels.String() + `</Relationships>`},
		{"xl/styles.xml", reportStylesXML},
		{"docProps/core.xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:dcterms="http://purl.org/dc/terms/" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"><dc:title>Отчёт о проверке клиентов</dc:title><dc:creator>rosfin</dc:creator><dcterms:created xsi:type="dcterms:W3CDTF">` + now + `</dcterms:created><dcterms:modified xsi:type="dcterms:W3CDTF">` + now + `</dcterms:modified></cp:coreProperties>`},
		{"docProps/app.xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Properties xmlns="http://schemas.openxmlformats.org/officeDocument/2006/extended-properties"><Application>rosfin</Application></Properties>`},
	}
	for _, p := range parts {
		if err := add(p.name, p.content); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// sanitizeSheetName приводит имя листа к правилам Excel: до 31 символа,
// без []:*?/\ и без повторов.
func sanitizeSheetName(name string, n int, used map[string]bool) string {
	name = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`[]:*?/\`, r) {
			return '_'
		}
		return r
	}, strings.TrimSpace(name))
	if r := []rune(name); len(r) > 31 {
		name = string(r[:31])
	}
	if name == "" {
		name = fmt.Sprintf("Лист%d", n)
	}
	base := name
	for i := 2; used[strings.ToLower(name)]; i++ {
		suffix := fmt.Sprintf(" (%d)", i)
		r := []rune(base)
		if len(r)+len([]rune(suffix)) > 31 {
			r = r[:31-len([]rune(suffix))]
		}
		name = string(r) + suffix
	}
	used[strings.ToLower(name)] = true
	return name
}

// sortedKeys — вспомогательная функция для детерминированного обхода.
func sortedKeys(m map[int]map[int]XCell) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}
