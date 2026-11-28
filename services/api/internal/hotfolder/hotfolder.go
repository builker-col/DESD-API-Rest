package hotfolder

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"desd-api/internal/config"
	"desd-api/internal/converter"
	"desd-api/internal/db"
	"desd-api/internal/export"
	"desd-api/internal/ingest"
	"desd-api/internal/queue"
)

type activeJob struct {
	TaskID        string
	Action        string
	Destination   string
	ExportMode    string
	ProcessingDir string
	IsDuplicate   bool
	StartedAt     time.Time
}

type triggerLocation struct {
	SourceDir   string
	TriggerPath string
	IsSubdir    bool
}

var triggerNames = []string{"trigger.txt", "trigger.json"}

func Run(ctx context.Context, cfg config.Config, store *db.Store, pub *queue.Publisher) {
	for _, sub := range []string{"inbox", "processing", "done", "failed", "results"} {
		_ = os.MkdirAll(filepath.Join(cfg.HotfolderRoot, sub), 0o777)
	}

	inboxDir := filepath.Join(cfg.HotfolderRoot, "inbox")
	var activeJobs []*activeJob

	fmt.Printf("[hotfolder] Activo en '%s' (poll=%v, workers=%d, max_dim=%dpx)\n",
		inboxDir, cfg.HotfolderPoll, cfg.IngestWorkers, cfg.Converter.MaxDim)

	ticker := time.NewTicker(cfg.HotfolderPoll)
	defer ticker.Stop()

	for {
		for _, loc := range findReadyTriggers(inboxDir) {
			if job := ingestBatch(ctx, cfg, store, pub, loc); job != nil {
				activeJobs = append(activeJobs, job)
			}
		}

		if len(activeJobs) > 0 {
			remaining := make([]*activeJob, 0, len(activeJobs))
			for _, job := range activeJobs {
				status, err := store.GetTaskStatusString(ctx, job.TaskID)
				if err != nil {
					remaining = append(remaining, job)
					continue
				}
				if status == "COMPLETED" || status == "FAILED" {
					finalizeJob(ctx, cfg, store, job, status)
				} else {
					remaining = append(remaining, job)
				}
			}
			activeJobs = remaining
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func parseTriggerFile(path string) (map[string]string, error) {
	rawBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw := strings.TrimSpace(strings.TrimPrefix(string(rawBytes), "\ufeff"))
	cfg := make(map[string]string)
	if raw == "" {
		return cfg, nil
	}

	if strings.HasSuffix(strings.ToLower(path), ".json") || strings.HasPrefix(raw, "{") {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			return nil, err
		}
		for k, v := range parsed {
			key := strings.ToLower(strings.TrimSpace(k))
			switch val := v.(type) {
			case []any:
				parts := make([]string, 0, len(val))
				for _, item := range val {
					parts = append(parts, strings.TrimSpace(fmt.Sprintf("%v", item)))
				}
				cfg[key] = strings.Join(parts, ",")
			case nil:
			default:
				cfg[key] = strings.TrimSpace(fmt.Sprintf("%v", val))
			}
		}
		return cfg, nil
	}

	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if idx := strings.Index(line, "="); idx >= 0 {
			cfg[strings.ToLower(strings.TrimSpace(line[:idx]))] = strings.TrimSpace(line[idx+1:])
		} else if idx := strings.Index(line, ":"); idx >= 0 {
			cfg[strings.ToLower(strings.TrimSpace(line[:idx]))] = strings.TrimSpace(line[idx+1:])
		} else if _, ok := cfg["models"]; !ok {
			cfg["models"] = line
		}
	}
	return cfg, nil
}

func findReadyTriggers(inboxDir string) []triggerLocation {
	var found []triggerLocation
	entries, err := os.ReadDir(inboxDir)
	if err != nil {
		return found
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "._") {
			continue
		}
		full := filepath.Join(inboxDir, name)
		if fi, err := os.Stat(full); err == nil && fi.IsDir() {
			for _, trig := range triggerNames {
				tpath := filepath.Join(full, trig)
				if info, err := os.Stat(tpath); err == nil && !info.IsDir() {
					found = append(found, triggerLocation{SourceDir: full, TriggerPath: tpath, IsSubdir: true})
					break
				}
			}
		}
	}

	for _, trig := range triggerNames {
		tpath := filepath.Join(inboxDir, trig)
		if info, err := os.Stat(tpath); err == nil && !info.IsDir() {
			found = append(found, triggerLocation{SourceDir: inboxDir, TriggerPath: tpath, IsSubdir: false})
			break
		}
	}
	return found
}

func collectInputFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() || ingest.IsIgnoredFile(e.Name()) {
			continue
		}
		files = append(files, filepath.Join(dir, e.Name()))
	}
	sort.Strings(files)
	return files
}

func moveToFailed(failedDir, srcPath, taskID, reason string) {
	dir := filepath.Join(failedDir, taskID)
	_ = os.MkdirAll(dir, 0o777)
	if _, err := os.Stat(srcPath); err == nil {
		_ = converter.MoveFileSafe(srcPath, filepath.Join(dir, filepath.Base(srcPath)))
	}
	f, err := os.OpenFile(filepath.Join(dir, "error.txt"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o666)
	if err == nil {
		_, _ = fmt.Fprintf(f, "[%s] %s: %s\n", time.Now().Format(time.RFC3339), filepath.Base(srcPath), reason)
		_ = f.Close()
	}
}

func ingestBatch(ctx context.Context, cfg config.Config, store *db.Store, pub *queue.Publisher, loc triggerLocation) *activeJob {
	processingRoot := filepath.Join(cfg.HotfolderRoot, "processing")
	doneRoot := filepath.Join(cfg.HotfolderRoot, "done")
	failedRoot := filepath.Join(cfg.HotfolderRoot, "failed")

	tCfg, err := parseTriggerFile(loc.TriggerPath)
	if err != nil {
		moveToFailed(failedRoot, loc.TriggerPath, fmt.Sprintf("invalid_trigger_%d", time.Now().Unix()), err.Error())
		return nil
	}

	rawModels := tCfg["models"]
	if rawModels == "" {
		rawModels = "cude,rode,legibility,tilde"
	}
	models := ingest.UniqueModels(rawModels)

	action := strings.ToLower(tCfg["action"])
	if action != "move" && action != "delete" && action != "keep" {
		action = "move"
	}

	exportMode := strings.ToLower(tCfg["export"])
	if exportMode == "" {
		exportMode = strings.ToLower(tCfg["export_csv"])
	}
	if exportMode != "none" && exportMode != "normal" && exportMode != "no_tecnico" && exportMode != "both" {
		exportMode = "both"
	}

	destVal := tCfg["destination"]
	if destVal == "" {
		destVal = tCfg["output_dir"]
	}
	destination := doneRoot
	if destVal != "" {
		if filepath.IsAbs(destVal) {
			destination = destVal
		} else {
			destination = filepath.Join(cfg.HotfolderRoot, destVal)
		}
	}

	username := tCfg["user"]
	if username == "" {
		username = cfg.HotfolderUser
	}

	dupFlag := strings.ToLower(tCfg["duplicate"])
	isDuplicate := (len(models) == 1 && (models[0] == "dude" || models[0] == "duplicate")) ||
		dupFlag == "1" || dupFlag == "true" || dupFlag == "yes" || dupFlag == "si"

	if !isDuplicate {
		var invalid []string
		for _, m := range models {
			if !ingest.ValidWorkerModels[m] {
				invalid = append(invalid, m)
			}
		}
		if len(models) == 0 || len(invalid) > 0 {
			moveToFailed(failedRoot, loc.TriggerPath, fmt.Sprintf("invalid_models_%d", time.Now().Unix()), "Modelos inválidos")
			return nil
		}
	}

	reqID := strings.TrimSpace(tCfg["task_id"])
	if reqID == "" && loc.IsSubdir {
		reqID = filepath.Base(loc.SourceDir)
	}
	taskID, err := store.AllocateUniqueTaskID(ctx, reqID)
	if err != nil {
		return nil
	}
	userID, err := store.ResolveUserID(ctx, username)
	if err != nil {
		return nil
	}

	batchProcDir := filepath.Join(processingRoot, taskID)
	_ = os.MkdirAll(batchProcDir, 0o777)
	if err := converter.MoveFileSafe(loc.TriggerPath, filepath.Join(batchProcDir, filepath.Base(loc.TriggerPath))); err != nil {
		return nil
	}

	sourceFiles := collectInputFiles(loc.SourceDir)
	if len(sourceFiles) == 0 {
		_ = os.RemoveAll(batchProcDir)
		if loc.IsSubdir {
			_ = os.Remove(loc.SourceDir)
		}
		return nil
	}

	staged := make([]ingest.StagedInput, 0, len(sourceFiles))
	if action == "keep" {
		for i, src := range sourceFiles {
			var sz int64
			if fi, err := os.Stat(src); err == nil {
				sz = fi.Size()
			}
			staged = append(staged, ingest.StagedInput{
				Index:      i + 1,
				Path:       src,
				Filename:   filepath.Base(src),
				SourcePath: src,
				SizeBytes:  sz,
			})
		}
	} else {
		for i, src := range sourceFiles {
			var sz int64
			if fi, err := os.Stat(src); err == nil {
				sz = fi.Size()
			}
			dst := filepath.Join(batchProcDir, filepath.Base(src))
			if err := converter.MoveFileSafe(src, dst); err != nil {
				continue
			}
			staged = append(staged, ingest.StagedInput{
				Index:      i + 1,
				Path:       dst,
				Filename:   filepath.Base(dst),
				SourcePath: src,
				SizeBytes:  sz,
			})
		}
		if loc.IsSubdir {
			_ = os.Remove(loc.SourceDir)
		}
	}

	modelsCSV := strings.Join(models, ",")
	if isDuplicate {
		modelsCSV = "dude"
	}
	resultID, err := store.CreateTask(ctx, taskID, userID, len(staged), modelsCSV)
	if err != nil {
		return nil
	}

	start := time.Now()
	validFiles, totalPages, ingestedFiles := ingest.FilesStreaming(ctx, cfg, pub, taskID, models, isDuplicate, staged, func(in ingest.StagedInput, fErr error) {
		fmt.Printf("[hotfolder] Omitiendo %s: %v\n", in.Filename, fErr)
		if action != "keep" {
			moveToFailed(failedRoot, in.Path, taskID, fErr.Error())
		}
	})

	if validFiles == 0 || (!isDuplicate && totalPages == 0) {
		_ = store.SetTaskStatus(ctx, resultID, "FAILED", 0)
		_ = os.RemoveAll(filepath.Join(cfg.SharedDataDir, taskID))
		_ = os.RemoveAll(batchProcDir)
		return nil
	}

	if len(ingestedFiles) > 0 {
		dbFiles := make([]db.ResultFile, 0, len(ingestedFiles))
		for _, f := range ingestedFiles {
			dbFiles = append(dbFiles, db.ResultFile{
				Filename:  f.Filename,
				FilePath:  f.FilePath,
				Source:    "hotfolder",
				Pages:     f.Pages,
				SizeBytes: f.SizeBytes,
			})
		}
		_ = store.InsertResultFiles(ctx, resultID, taskID, dbFiles)
	}

	if isDuplicate {
		if err := pub.PublishJSON(ctx, cfg.DuplicateQueue, ingest.DuplicateTaskMessage{
			FolderPath: filepath.Join(cfg.SharedDataDir, taskID),
			TaskID:     taskID,
		}); err != nil {
			_ = store.SetTaskStatus(ctx, resultID, "FAILED", validFiles)
			return nil
		}
		_ = store.SetTaskStatus(ctx, resultID, "PENDING", validFiles)
	} else {
		_, _ = store.FinalizeIngestion(ctx, resultID, validFiles, totalPages*len(models))
	}

	fmt.Printf("[hotfolder] Lote '%s' encolado (%d archivos, %d págs, %s) en %.3fs | action=%s, export=%s\n",
		taskID, validFiles, totalPages, modelsCSV, time.Since(start).Seconds(), action, exportMode)

	return &activeJob{
		TaskID:        taskID,
		Action:        action,
		Destination:   destination,
		ExportMode:    exportMode,
		ProcessingDir: batchProcDir,
		IsDuplicate:   isDuplicate,
		StartedAt:     start,
	}
}

func finalizeJob(ctx context.Context, cfg config.Config, store *db.Store, job *activeJob, finalStatus string) {
	resultsRoot := filepath.Join(cfg.HotfolderRoot, "results")
	failedRoot := filepath.Join(cfg.HotfolderRoot, "failed")

	var exported []string
	if finalStatus == "COMPLETED" && job.ExportMode != "none" {
		formats := []string{job.ExportMode}
		if job.IsDuplicate {
			formats = []string{"normal"}
		} else if job.ExportMode == "both" {
			formats = []string{"normal", "no_tecnico"}
		}

		for _, fmtName := range formats {
			suffix := ""
			if fmtName != "normal" {
				suffix = "_" + fmtName
			}
			destCSV := filepath.Join(resultsRoot, fmt.Sprintf("%s%s.csv", job.TaskID, suffix))
			ok, err := export.ToFile(ctx, store, job.TaskID, fmtName, destCSV)
			if err != nil {
				fmt.Printf("[hotfolder] Error exportando CSV %s de '%s': %v\n", fmtName, job.TaskID, err)
			} else if ok {
				exported = append(exported, destCSV)
			}
		}
	}

	if fi, err := os.Stat(job.ProcessingDir); err == nil && fi.IsDir() {
		if finalStatus != "COMPLETED" {
			failDest := filepath.Join(failedRoot, job.TaskID)
			_ = os.MkdirAll(failDest, 0o777)
			if entries, err := os.ReadDir(job.ProcessingDir); err == nil {
				for _, e := range entries {
					_ = converter.MoveFileSafe(filepath.Join(job.ProcessingDir, e.Name()), filepath.Join(failDest, e.Name()))
				}
			}
			_ = os.RemoveAll(job.ProcessingDir)
			_ = store.UpdateResultFilesDir(ctx, job.TaskID, failDest)
		} else if job.Action == "delete" || job.Action == "keep" {
			_ = os.RemoveAll(job.ProcessingDir)
		} else if job.Action == "move" {
			destDir := filepath.Join(job.Destination, job.TaskID)
			_ = os.MkdirAll(destDir, 0o777)
			if entries, err := os.ReadDir(job.ProcessingDir); err == nil {
				for _, e := range entries {
					_ = converter.MoveFileSafe(filepath.Join(job.ProcessingDir, e.Name()), filepath.Join(destDir, e.Name()))
				}
			}
			_ = os.RemoveAll(job.ProcessingDir)
			_ = store.UpdateResultFilesDir(ctx, job.TaskID, destDir)
		}
	}

	fmt.Printf("[hotfolder] Lote '%s' finalizado (%s) en %.3fs | action=%s | csv=%v\n",
		job.TaskID, finalStatus, time.Since(job.StartedAt).Seconds(), job.Action, exported)
}
