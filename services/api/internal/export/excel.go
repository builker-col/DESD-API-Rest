package export

import (
	"fmt"
	"io"
	"math"
	"sort"
	"time"

	"desd-api/internal/db"
	"github.com/xuri/excelize/v2"
)

type userSummary struct {
	Username    string
	Role        string
	TaskSet     map[string]bool
	TotalFiles  int
	TotalPages  int
	TotalAlerts int
	LastAt      time.Time
}

// WriteFilesExcelReport genera un libro .xlsx con dos hojas:
//  1. "Archivos Procesados": detalle por archivo (usuario, rol, task_id, nombre, ruta, fecha, hora, origen, modelos, páginas, tamaño KB, estado, alertas)
//  2. "Resumen por Usuario": totales agrupados por usuario + fila TOTAL general
func WriteFilesExcelReport(w io.Writer, rows []db.FileReportRow) error {
	f := excelize.NewFile()
	defer func() { _ = f.Close() }()

	detailSheet := "Archivos Procesados"
	summarySheet := "Resumen por Usuario"

	defaultSheet := f.GetSheetName(0)
	if err := f.SetSheetName(defaultSheet, detailSheet); err != nil {
		return err
	}
	if _, err := f.NewSheet(summarySheet); err != nil {
		return err
	}

	headerStyle, err := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Color: "#FFFFFF", Size: 11},
		Fill:      excelize.Fill{Type: "pattern", Color: []string{"#1F4E78"}, Pattern: 1},
		Alignment: &excelize.Alignment{Horizontal: "center", Vertical: "center", WrapText: true},
		Border: []excelize.Border{
			{Type: "bottom", Color: "#0F2C4C", Style: 2},
		},
	})
	if err != nil {
		return err
	}

	zebraStyle, _ := f.NewStyle(&excelize.Style{
		Fill: excelize.Fill{Type: "pattern", Color: []string{"#F4F7FA"}, Pattern: 1},
	})
	alertStyle, _ := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Color: "#9C0006"},
		Fill:      excelize.Fill{Type: "pattern", Color: []string{"#FFC7CE"}, Pattern: 1},
		Alignment: &excelize.Alignment{Horizontal: "center"},
	})
	totalStyle, _ := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Bold: true, Size: 11},
		Fill: excelize.Fill{Type: "pattern", Color: []string{"#D9E1F2"}, Pattern: 1},
		Border: []excelize.Border{
			{Type: "top", Color: "#1F4E78", Style: 2},
		},
	})

	// --- Hoja 1: Archivos Procesados ---
	headers := []string{
		"Usuario",
		"Rol",
		"Task ID",
		"Nombre de Archivo",
		"Ruta",
		"Fecha",
		"Hora",
		"Origen",
		"Modelos",
		"Páginas",
		"Tamaño (KB)",
		"Estado Tarea",
		"Hallazgos / Alertas",
	}

	for colIdx, h := range headers {
		cell, _ := excelize.CoordinatesToCellName(colIdx+1, 1)
		_ = f.SetCellValue(detailSheet, cell, h)
	}
	_ = f.SetRowHeight(detailSheet, 1, 24)
	_ = f.SetCellStyle(detailSheet, "A1", "M1", headerStyle)

	summaries := make(map[string]*userSummary)

	for i, r := range rows {
		rowNum := i + 2
		fecha := r.ProcessedAt.Format("2006-01-02")
		hora := r.ProcessedAt.Format("15:04:05")
		sizeKB := math.Round((float64(r.SizeBytes)/1024.0)*100) / 100

		values := []any{
			r.Username,
			r.UserRole,
			r.TaskID,
			r.Filename,
			r.FilePath,
			fecha,
			hora,
			r.Source,
			r.Models,
			r.Pages,
			sizeKB,
			r.Status,
			r.AlertsCount,
		}
		for colIdx, val := range values {
			cell, _ := excelize.CoordinatesToCellName(colIdx+1, rowNum)
			_ = f.SetCellValue(detailSheet, cell, val)
		}

		if i%2 == 1 {
			startCell, _ := excelize.CoordinatesToCellName(1, rowNum)
			endCell, _ := excelize.CoordinatesToCellName(len(headers), rowNum)
			_ = f.SetCellStyle(detailSheet, startCell, endCell, zebraStyle)
		}
		if r.AlertsCount > 0 {
			alertCell, _ := excelize.CoordinatesToCellName(13, rowNum)
			_ = f.SetCellStyle(detailSheet, alertCell, alertCell, alertStyle)
		}

		key := r.Username
		if key == "" {
			key = fmt.Sprintf("user_%d", r.UserID)
		}
		s, ok := summaries[key]
		if !ok {
			s = &userSummary{
				Username: key,
				Role:     r.UserRole,
				TaskSet:  make(map[string]bool),
			}
			summaries[key] = s
		}
		s.TaskSet[r.TaskID] = true
		s.TotalFiles++
		s.TotalPages += r.Pages
		s.TotalAlerts += r.AlertsCount
		if r.ProcessedAt.After(s.LastAt) {
			s.LastAt = r.ProcessedAt
		}
	}

	detailWidths := map[string]float64{
		"A": 18, "B": 13, "C": 26, "D": 30, "E": 42,
		"F": 13, "G": 11, "H": 12, "I": 24, "J": 10,
		"K": 14, "L": 15, "M": 19,
	}
	for col, width := range detailWidths {
		_ = f.SetColWidth(detailSheet, col, col, width)
	}
	_ = f.SetPanes(detailSheet, &excelize.Panes{
		Freeze:      true,
		Split:       false,
		XSplit:      0,
		YSplit:      1,
		TopLeftCell: "A2",
		ActivePane:  "bottomLeft",
	})
	lastDetailRow := len(rows) + 1
	if lastDetailRow < 1 {
		lastDetailRow = 1
	}
	_ = f.AutoFilter(detailSheet, fmt.Sprintf("A1:M%d", lastDetailRow), nil)

	// --- Hoja 2: Resumen por Usuario ---
	sumHeaders := []string{
		"Usuario",
		"Rol",
		"Total Lotes",
		"Total Archivos",
		"Total Páginas",
		"Total Alertas",
		"Último Procesamiento",
	}
	for colIdx, h := range sumHeaders {
		cell, _ := excelize.CoordinatesToCellName(colIdx+1, 1)
		_ = f.SetCellValue(summarySheet, cell, h)
	}
	_ = f.SetRowHeight(summarySheet, 1, 24)
	_ = f.SetCellStyle(summarySheet, "A1", "G1", headerStyle)

	usernames := make([]string, 0, len(summaries))
	for k := range summaries {
		usernames = append(usernames, k)
	}
	sort.Strings(usernames)

	grandTasks := 0
	grandFiles := 0
	grandPages := 0
	grandAlerts := 0

	for i, uname := range usernames {
		s := summaries[uname]
		rowNum := i + 2
		lastStr := ""
		if !s.LastAt.IsZero() {
			lastStr = s.LastAt.Format("2006-01-02 15:04:05")
		}
		vals := []any{
			s.Username,
			s.Role,
			len(s.TaskSet),
			s.TotalFiles,
			s.TotalPages,
			s.TotalAlerts,
			lastStr,
		}
		for colIdx, val := range vals {
			cell, _ := excelize.CoordinatesToCellName(colIdx+1, rowNum)
			_ = f.SetCellValue(summarySheet, cell, val)
		}
		grandTasks += len(s.TaskSet)
		grandFiles += s.TotalFiles
		grandPages += s.TotalPages
		grandAlerts += s.TotalAlerts
	}

	totalRow := len(usernames) + 2
	totalVals := []any{"TOTAL", "", grandTasks, grandFiles, grandPages, grandAlerts, ""}
	for colIdx, val := range totalVals {
		cell, _ := excelize.CoordinatesToCellName(colIdx+1, totalRow)
		_ = f.SetCellValue(summarySheet, cell, val)
	}
	_ = f.SetCellStyle(summarySheet, fmt.Sprintf("A%d", totalRow), fmt.Sprintf("G%d", totalRow), totalStyle)

	sumWidths := map[string]float64{
		"A": 20, "B": 14, "C": 14, "D": 16, "E": 16, "F": 16, "G": 22,
	}
	for col, width := range sumWidths {
		_ = f.SetColWidth(summarySheet, col, col, width)
	}
	_ = f.SetPanes(summarySheet, &excelize.Panes{
		Freeze:      true,
		Split:       false,
		XSplit:      0,
		YSplit:      1,
		TopLeftCell: "A2",
		ActivePane:  "bottomLeft",
	})

	f.SetActiveSheet(0)
	return f.Write(w)
}
