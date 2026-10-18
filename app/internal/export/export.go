// Package export renders a report's CSV as an Excel workbook or a PDF.
//
// The CSV is the source of truth; these are convenience formats generated
// from it right after the report runs. Both read the CSV as a stream.
package export

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/go-pdf/fpdf"
	"github.com/xuri/excelize/v2"
)

// Meta describes the report for titles and notes.
type Meta struct {
	Title     string // e.g. "Events by type"
	Subtitle  string // e.g. "2026-10-01 – 2026-10-07 (UTC) · 1,234 rows"
	Generated time.Time
}

const (
	// Excel's limit is 1,048,576 rows including the header.
	MaxXLSXRows = 1_048_575
	// A PDF of millions of rows isn't useful; summaries fit easily.
	MaxPDFRows = 2_000
)

// XLSX writes a workbook: bold, frozen header row with filters, numbers
// stored as numbers. Rows past MaxXLSXRows are dropped with a note.
func XLSX(src io.Reader, dst io.Writer, m Meta) error {
	r := csv.NewReader(src)
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if errors.Is(err, io.EOF) {
		header = []string{"(no data)"}
	} else if err != nil {
		return fmt.Errorf("read csv header: %w", err)
	}

	f := excelize.NewFile()
	defer f.Close()
	const sheet = "Report"
	if err := f.SetSheetName("Sheet1", sheet); err != nil {
		return err
	}
	headerStyle, err := f.NewStyle(&excelize.Style{
		Font:   &excelize.Font{Bold: true},
		Fill:   excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"E8EEFC"}},
		Border: []excelize.Border{{Type: "bottom", Color: "9AA6C4", Style: 1}},
	})
	if err != nil {
		return err
	}

	sw, err := f.NewStreamWriter(sheet)
	if err != nil {
		return err
	}
	// Panes and widths must be set before the first row.
	if err := sw.SetPanes(&excelize.Panes{Freeze: true, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft"}); err != nil {
		return err
	}
	for i, h := range header {
		w := float64(len(h) + 4)
		if w < 12 {
			w = 12
		}
		if err := sw.SetColWidth(i+1, i+1, w); err != nil {
			return err
		}
	}

	row := make([]any, len(header))
	for i, h := range header {
		row[i] = excelize.Cell{StyleID: headerStyle, Value: h}
	}
	if err := sw.SetRow("A1", row); err != nil {
		return err
	}

	n, truncated := 0, false
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read csv: %w", err)
		}
		if n == MaxXLSXRows {
			truncated = true
			break
		}
		vals := make([]any, len(rec))
		for i, v := range rec {
			vals[i] = typed(v)
		}
		n++
		cell, _ := excelize.CoordinatesToCellName(1, n+1)
		if err := sw.SetRow(cell, vals); err != nil {
			return err
		}
	}
	if err := sw.Flush(); err != nil {
		return err
	}

	last, _ := excelize.CoordinatesToCellName(len(header), n+1)
	if err := f.AutoFilter(sheet, "A1:"+last, nil); err != nil {
		return err
	}

	notes := []string{m.Title, m.Subtitle, "Generated " + m.Generated.UTC().Format("2006-01-02 15:04 UTC")}
	if truncated {
		notes = append(notes, fmt.Sprintf("Truncated to %d rows (Excel's limit). Download the CSV for all rows.", MaxXLSXRows))
	}
	if _, err := f.NewSheet("About"); err != nil {
		return err
	}
	for i, line := range notes {
		cell, _ := excelize.CoordinatesToCellName(1, i+1)
		if err := f.SetCellValue("About", cell, line); err != nil {
			return err
		}
	}
	return f.Write(dst)
}

// typed stores numbers as numbers so they sort and sum in Excel. Values
// with leading zeros stay text (IDs, codes).
func typed(v string) any {
	if v == "" {
		return nil
	}
	if len(v) > 1 && v[0] == '0' && v[1] != '.' {
		return v
	}
	if i, err := strconv.ParseInt(v, 10, 64); err == nil {
		return i
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil && !strings.ContainsAny(v, "eEnN") {
		return f
	}
	return v
}

// PDF writes a landscape A4 table with a title, the header repeated on every
// page and page numbers. Rows past MaxPDFRows are dropped with a note.
func PDF(src io.Reader, dst io.Writer, m Meta) error {
	r := csv.NewReader(src)
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if errors.Is(err, io.EOF) {
		header = []string{"(no data)"}
	} else if err != nil {
		return fmt.Errorf("read csv header: %w", err)
	}
	var rows [][]string
	truncated := false
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read csv: %w", err)
		}
		if len(rows) == MaxPDFRows {
			truncated = true
			break
		}
		rows = append(rows, rec)
	}

	const (
		margin    = 10.0
		rowH      = 6.0
		fontSize  = 8.0
		maxColW   = 80.0
		pageW     = 297.0
		pageH     = 210.0
		usableW   = pageW - 2*margin
		bottomGap = 14.0
	)

	pdf := fpdf.New("L", "mm", "A4", "")
	pdf.SetMargins(margin, margin, margin)
	pdf.SetAutoPageBreak(false, 0) // breaks are handled per row, to repeat the header
	pdf.SetTitle(m.Title, true)
	pdf.SetCreator("asyncsvc", true)
	tr := pdf.UnicodeTranslatorFromDescriptor("") // UTF-8 -> the core fonts' cp1252

	pdf.SetFooterFunc(func() {
		pdf.SetY(-10)
		pdf.SetFont("Helvetica", "", 7)
		pdf.SetTextColor(120, 120, 120)
		pdf.CellFormat(0, 4, fmt.Sprintf("%s · page %d", tr(m.Title), pdf.PageNo()), "", 0, "R", false, 0, "")
	})

	// Column widths from content, capped, then scaled to the page if needed.
	pdf.SetFont("Helvetica", "", fontSize)
	widths := make([]float64, len(header))
	measure := func(i int, s string) {
		if i >= len(widths) {
			return
		}
		if w := pdf.GetStringWidth(tr(s)) + 4; w > widths[i] {
			widths[i] = min(w, maxColW)
		}
	}
	pdf.SetFont("Helvetica", "B", fontSize)
	for i, h := range header {
		measure(i, h)
	}
	pdf.SetFont("Helvetica", "", fontSize)
	for _, rec := range rows {
		for i, v := range rec {
			measure(i, v)
		}
	}
	total := 0.0
	for _, w := range widths {
		total += w
	}
	if total > usableW {
		for i := range widths {
			widths[i] *= usableW / total
		}
	}

	fit := func(s string, w float64) string {
		s = tr(s)
		if pdf.GetStringWidth(s) <= w-2 {
			return s
		}
		for len(s) > 0 && pdf.GetStringWidth(s+"...") > w-2 {
			s = s[:len(s)-1]
		}
		return s + "..."
	}
	drawHeader := func() {
		pdf.SetFont("Helvetica", "B", fontSize)
		pdf.SetFillColor(232, 238, 252)
		pdf.SetTextColor(20, 20, 20)
		for i, h := range header {
			pdf.CellFormat(widths[i], rowH, fit(h, widths[i]), "B", 0, "L", true, 0, "")
		}
		pdf.Ln(rowH)
		pdf.SetFont("Helvetica", "", fontSize)
	}

	pdf.AddPage()
	pdf.SetFont("Helvetica", "B", 14)
	pdf.CellFormat(0, 8, tr(m.Title), "", 1, "L", false, 0, "")
	pdf.SetFont("Helvetica", "", 9)
	pdf.SetTextColor(100, 100, 100)
	pdf.CellFormat(0, 5, tr(m.Subtitle), "", 1, "L", false, 0, "")
	pdf.CellFormat(0, 5, "Generated "+m.Generated.UTC().Format("2006-01-02 15:04 UTC"), "", 1, "L", false, 0, "")
	pdf.Ln(3)
	drawHeader()

	for n, rec := range rows {
		if pdf.GetY()+rowH > pageH-bottomGap {
			pdf.AddPage()
			drawHeader()
		}
		pdf.SetFillColor(246, 247, 249)
		for i := range header {
			v := ""
			if i < len(rec) {
				v = rec[i]
			}
			align := "L"
			if _, err := strconv.ParseFloat(v, 64); err == nil && v != "" {
				align = "R"
			}
			pdf.CellFormat(widths[i], rowH, fit(v, widths[i]), "", 0, align, n%2 == 1, 0, "")
		}
		pdf.Ln(rowH)
	}
	if len(rows) == 0 {
		pdf.SetTextColor(100, 100, 100)
		pdf.CellFormat(0, rowH, "No rows for this date range.", "", 1, "L", false, 0, "")
	}
	if truncated {
		pdf.Ln(2)
		pdf.SetFont("Helvetica", "I", 8)
		pdf.SetTextColor(150, 90, 0)
		pdf.CellFormat(0, 5, fmt.Sprintf("Showing the first %d rows. Download the CSV or Excel file for all rows.", MaxPDFRows), "", 1, "L", false, 0, "")
	}
	return pdf.Output(dst)
}
