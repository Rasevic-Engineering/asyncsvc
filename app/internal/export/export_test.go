package export

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"
)

var meta = Meta{Title: "Events by type", Subtitle: "2026-10-01 – 2026-10-07 (UTC) · 3 rows", Generated: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}

func TestXLSX(t *testing.T) {
	src := "event_type,events,users,code,ratio\npage_view,1200,87,007,0.5\nsignup,\"3,4\",2,,1e3\n"
	var out bytes.Buffer
	if err := XLSX(strings.NewReader(src), &out, meta); err != nil {
		t.Fatal(err)
	}
	f, err := excelize.OpenReader(&out)
	if err != nil {
		t.Fatalf("not a valid workbook: %v", err)
	}
	defer f.Close()

	rows, _ := f.GetRows("Report")
	if len(rows) != 3 || rows[0][0] != "event_type" || rows[1][1] != "1200" || rows[2][1] != "3,4" {
		t.Fatalf("rows: %v", rows)
	}
	// Numbers are numbers (in xlsx, a cell without a type attribute is
	// numeric); leading zeros, quoted commas and "1e3" stay text.
	isNumber := func(c excelize.CellType) bool { return c == excelize.CellTypeUnset || c == excelize.CellTypeNumber }
	for _, cell := range []string{"B2", "C2", "E2"} {
		if got, _ := f.GetCellType("Report", cell); !isNumber(got) {
			t.Errorf("%s should be a number, got type %v", cell, got)
		}
	}
	for _, cell := range []string{"A2", "D2", "B3", "E3"} {
		if got, _ := f.GetCellType("Report", cell); isNumber(got) {
			t.Errorf("%s should be text, got type %v", cell, got)
		}
	}
	if p, _ := f.GetPanes("Report"); !p.Freeze || p.YSplit != 1 {
		t.Errorf("header row should be frozen: %+v", p)
	}
	about, _ := f.GetRows("About")
	if len(about) < 3 || about[0][0] != "Events by type" {
		t.Errorf("about sheet: %v", about)
	}
}

func TestPDF(t *testing.T) {
	var b strings.Builder
	b.WriteString("day,active_users,events,properties\n")
	for i := 0; i < MaxPDFRows+500; i++ {
		fmt.Fprintf(&b, "2026-10-%02d,%d,%d,\"{\"\"path\"\":\"\"/pricing/a/very/long/path/that/needs/truncating/in/the/table\"\"}\"\n", i%28+1, i, i*7)
	}
	var out bytes.Buffer
	if err := PDF(strings.NewReader(b.String()), &out, meta); err != nil {
		t.Fatal(err)
	}
	pdf := out.String()
	if !strings.HasPrefix(pdf, "%PDF-") || !strings.Contains(pdf, "%%EOF") {
		t.Fatal("not a PDF")
	}
	if pages := strings.Count(pdf, "/Type /Page\n"); pages < 20 {
		t.Fatalf("expected the table to span many pages, got %d", pages)
	}
}

func TestEmptyReports(t *testing.T) {
	for name, fn := range map[string]func() error{
		"xlsx":           func() error { return XLSX(strings.NewReader("a,b\n"), &bytes.Buffer{}, meta) },
		"pdf":            func() error { return PDF(strings.NewReader("a,b\n"), &bytes.Buffer{}, meta) },
		"xlsx-no-header": func() error { return XLSX(strings.NewReader(""), &bytes.Buffer{}, meta) },
		"pdf-no-header":  func() error { return PDF(strings.NewReader(""), &bytes.Buffer{}, meta) },
	} {
		if err := fn(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
