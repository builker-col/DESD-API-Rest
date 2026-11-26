package export

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"desd-api/internal/db"
)

var problematicClasses = map[string]bool{
	"inclinado":                true,
	"rotado":                   true,
	"con corte de informacion": true,
	"no legible":               true,
}

var nonTechnicalOrder = []string{
	"task_id",
	"nombre_archivo",
	"pagina",
	"nombre_clase",
	"confianza",
}

var preferredWorkerOrder = []string{
	"task_id",
	"nombre_archivo",
	"pagina",
	"confianza",
	"nombre_clase",
	"modelo",
	"tiempo_inferencia",
}

func parseConfidence(val any) float64 {
	switch v := val.(type) {
	case string:
		s := strings.TrimSpace(v)
		if strings.HasSuffix(s, "%") {
			f, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(s, "%")), 64)
			if err != nil {
				return 0
			}
			return f
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0
		}
		if f <= 1.0 {
			return f * 100.0
		}
		return f
	case float64:
		if v <= 1.0 {
			return v * 100.0
		}
		return v
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return 0
		}
		if f <= 1.0 {
			return f * 100.0
		}
		return f
	default:
		return 0
	}
}

func confidenceToLabel(p float64) string {
	if p >= 95.0 {
		return "muy probable"
	}
	if p >= 90.0 {
		return "probable"
	}
	return "quizás con error"
}

func formatCSVCell(val any) string {
	if val == nil {
		return ""
	}
	switch v := val.(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	default:
		return fmt.Sprintf("%v", v)
	}
}

func decodeJSONMap(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

func ResolveFieldnames(ctx context.Context, store *db.Store, taskID, formato string) ([]string, bool, error) {
	if formato == "no_tecnico" {
		var one int
		if err := store.Pool.QueryRow(ctx, `SELECT 1 FROM result_item WHERE task_id = $1 LIMIT 1`, taskID).Scan(&one); err != nil {
			return nil, false, nil
		}
		return nonTechnicalOrder, true, nil
	}

	var firstRaw []byte
	if err := store.Pool.QueryRow(ctx, `SELECT data FROM result_item WHERE task_id = $1 ORDER BY id ASC LIMIT 1`, taskID).Scan(&firstRaw); err != nil {
		return nil, false, nil
	}
	firstDoc, err := decodeJSONMap(firstRaw)
	if err != nil || len(firstDoc) == 0 {
		return nil, false, err
	}

	if _, isWorker := firstDoc["nombre_archivo"]; isWorker {
		fields := make([]string, 0, len(firstDoc))
		seen := make(map[string]bool, len(firstDoc))
		for _, k := range preferredWorkerOrder {
			if _, ok := firstDoc[k]; ok {
				fields = append(fields, k)
				seen[k] = true
			}
		}
		var extras []string
		for k := range firstDoc {
			if !seen[k] {
				extras = append(extras, k)
			}
		}
		sort.Strings(extras)
		return append(fields, extras...), true, nil
	}

	rows, err := store.Pool.Query(ctx, `
		SELECT DISTINCT jsonb_object_keys(data) AS k
		FROM result_item WHERE task_id = $1
	`, taskID)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	keySet := make(map[string]bool)
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err == nil {
			keySet[k] = true
		}
	}

	preferredDup := []string{"task_id", "filename", "pagina1", "pagina2", "similitud", "message"}
	fields := make([]string, 0, len(keySet))
	seen := make(map[string]bool)
	for _, k := range preferredDup {
		if keySet[k] {
			fields = append(fields, k)
			seen[k] = true
		}
	}
	var extras []string
	for k := range keySet {
		if !seen[k] {
			extras = append(extras, k)
		}
	}
	sort.Strings(extras)
	return append(fields, extras...), true, nil
}

func StreamCSV(ctx context.Context, store *db.Store, w io.Writer, taskID, formato string, fieldnames []string) error {
	bw := bufio.NewWriterSize(w, 64*1024)
	cw := csv.NewWriter(bw)

	if err := cw.Write(fieldnames); err != nil {
		return err
	}

	rows, err := store.Pool.Query(ctx, `
		SELECT data FROM result_item WHERE task_id = $1 ORDER BY id ASC
	`, taskID)
	if err != nil {
		return err
	}
	defer rows.Close()

	record := make([]string, len(fieldnames))
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return err
		}
		doc, err := decodeJSONMap(raw)
		if err != nil {
			continue
		}

		if formato == "no_tecnico" {
			cls, _ := doc["nombre_clase"].(string)
			if !problematicClasses[cls] {
				continue
			}
			conf := parseConfidence(doc["confianza"])
			if conf < 80.0 {
				continue
			}
			record[0] = formatCSVCell(doc["task_id"])
			record[1] = formatCSVCell(doc["nombre_archivo"])
			record[2] = formatCSVCell(doc["pagina"])
			record[3] = cls
			record[4] = confidenceToLabel(conf)
			if err := cw.Write(record); err != nil {
				return err
			}
		} else {
			for i, k := range fieldnames {
				record[i] = formatCSVCell(doc[k])
			}
			if err := cw.Write(record); err != nil {
				return err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	cw.Flush()
	if err := cw.Error(); err != nil {
		return err
	}
	return bw.Flush()
}

func ToFile(ctx context.Context, store *db.Store, taskID, formato, destPath string) (bool, error) {
	fieldnames, ok, err := ResolveFieldnames(ctx, store, taskID, formato)
	if err != nil || !ok {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o777); err != nil {
		return false, err
	}

	tmpPath := destPath + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return false, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := StreamCSV(ctx, store, f, taskID, formato, fieldnames); err != nil {
		_ = f.Close()
		return false, err
	}
	if err := f.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		return false, err
	}
	cleanup = false
	return true, nil
}
