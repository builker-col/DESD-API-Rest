package ingest

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"desd-api/internal/config"
	"desd-api/internal/converter"
	"desd-api/internal/queue"
)

type WorkerPageMessage struct {
	Filename  string   `json:"filename"`
	FilePath  string   `json:"file_path"`
	TaskID    string   `json:"task_id"`
	Models    []string `json:"models"`
	Page      int      `json:"page"`
	FileIndex int      `json:"file_index"`
}

type DuplicateTaskMessage struct {
	FolderPath string `json:"folder_path"`
	TaskID     string `json:"task_id"`
}

type StagedInput struct {
	Index    int
	Path     string
	Filename string
}

var (
	ValidWorkerModels = map[string]bool{"rode": true, "cude": true, "tilde": true, "legibility": true}
	validDocExt       = map[string]bool{"pdf": true, "tif": true, "tiff": true}
	validImgExt       = map[string]bool{"jpg": true, "jpeg": true, "png": true}
	ignoredNames      = map[string]bool{".ds_store": true, "thumbs.db": true, "desktop.ini": true, ".gitkeep": true}
	ignoredExt        = []string{".tmp", ".part", ".crdownload", ".partial"}
)

func IsIgnoredFile(name string) bool {
	lower := strings.ToLower(name)
	if ignoredNames[lower] || lower == "trigger.txt" || lower == "trigger.json" {
		return true
	}
	if strings.HasPrefix(name, "._") || strings.HasPrefix(name, ".") {
		return true
	}
	for _, suf := range ignoredExt {
		if strings.HasSuffix(lower, suf) {
			return true
		}
	}
	return false
}

func UniqueModels(raw string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, part := range strings.Split(raw, ",") {
		m := strings.ToLower(strings.TrimSpace(part))
		if m != "" && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

func FilesStreaming(
	ctx context.Context,
	cfg config.Config,
	pub *queue.Publisher,
	taskID string,
	models []string,
	isDuplicate bool,
	inputs []StagedInput,
	onFileError func(in StagedInput, err error),
) (int, int) {
	taskSharedDir := filepath.Join(cfg.SharedDataDir, taskID)
	_ = os.MkdirAll(taskSharedDir, 0o777)

	// Archivo centinela mientras dura la ingesta: impide que os.rmdir() del worker
	// borre taskSharedDir antes de que todos los archivos del lote terminen de convertirse.
	sentinelPath := filepath.Join(taskSharedDir, ".ingesting")
	_ = os.WriteFile(sentinelPath, []byte("1"), 0o666)
	defer func() {
		_ = os.Remove(sentinelPath)
		_ = os.Remove(taskSharedDir) // Solo elimina si ya quedó vacío
	}()

	jobsCh := make(chan StagedInput, len(inputs))
	for _, in := range inputs {
		jobsCh <- in
	}
	close(jobsCh)

	workers := cfg.IngestWorkers
	if len(inputs) < workers {
		workers = len(inputs)
	}
	if workers < 1 {
		workers = 1
	}

	var validFiles int64
	var totalPages int64
	var wg sync.WaitGroup

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for in := range jobsCh {
				vc, pages, err := ingestSingle(ctx, cfg, pub, taskID, taskSharedDir, models, isDuplicate, in)
				if err != nil {
					if onFileError != nil {
						onFileError(in, err)
					} else {
						fmt.Printf("[ingest] Error en %s: %v\n", in.Filename, err)
					}
					continue
				}
				atomic.AddInt64(&validFiles, int64(vc))
				atomic.AddInt64(&totalPages, int64(pages))
			}
		}()
	}
	wg.Wait()

	return int(atomic.LoadInt64(&validFiles)), int(atomic.LoadInt64(&totalPages))
}

func ingestSingle(
	ctx context.Context,
	cfg config.Config,
	pub *queue.Publisher,
	taskID, taskSharedDir string,
	models []string,
	isDuplicate bool,
	in StagedInput,
) (int, int, error) {
	lower := strings.ToLower(filepath.Base(in.Filename))
	ext := strings.TrimPrefix(filepath.Ext(lower), ".")

	if ext == "zip" {
		zr, err := zip.OpenReader(in.Path)
		if err != nil {
			return 0, 0, err
		}
		defer zr.Close()

		tmpDir, err := os.MkdirTemp(cfg.SharedDataDir, "__zip_*")
		if err != nil {
			return 0, 0, err
		}
		defer os.RemoveAll(tmpDir)

		vc := 0
		pages := 0
		innerIdx := 0
		for _, zf := range zr.File {
			if zf.FileInfo().IsDir() || IsIgnoredFile(filepath.Base(zf.Name)) {
				continue
			}
			innerName := strings.ToLower(filepath.Base(zf.Name))
			innerExt := strings.TrimPrefix(filepath.Ext(innerName), ".")
			if !validDocExt[innerExt] && !validImgExt[innerExt] {
				continue
			}
			innerIdx++

			rc, err := zf.Open()
			if err != nil {
				return vc, pages, err
			}
			extractedPath := filepath.Join(tmpDir, fmt.Sprintf("%d_%s", innerIdx, innerName))
			outF, err := os.Create(extractedPath)
			if err != nil {
				_ = rc.Close()
				return vc, pages, err
			}
			_, cpErr := io.Copy(outF, rc)
			_ = rc.Close()
			_ = outF.Close()
			if cpErr != nil {
				return vc, pages, cpErr
			}

			c, p, err := convertAndStreamOne(ctx, cfg, pub, in.Index, fmt.Sprintf("%d_%d", in.Index, innerIdx), extractedPath, innerName, innerExt, taskID, taskSharedDir, models, isDuplicate)
			_ = os.Remove(extractedPath)
			if err != nil {
				return vc, pages, err
			}
			vc += c
			pages += p
		}
		if vc == 0 {
			return 0, 0, fmt.Errorf("ZIP sin archivos soportados: %s", in.Filename)
		}
		return vc, pages, nil
	}

	return convertAndStreamOne(ctx, cfg, pub, in.Index, strconv.Itoa(in.Index), in.Path, lower, ext, taskID, taskSharedDir, models, isDuplicate)
}

func convertAndStreamOne(
	ctx context.Context,
	cfg config.Config,
	pub *queue.Publisher,
	fileIdx int,
	prefixID, srcPath, lowerName, ext, taskID, taskSharedDir string,
	models []string,
	isDuplicate bool,
) (int, int, error) {
	if validDocExt[ext] {
		if isDuplicate {
			docDir := filepath.Join(taskSharedDir, lowerName)
			pages, err := converter.ConvertDocumentStreaming(ctx, cfg.Converter, srcPath, lowerName, docDir, "page", nil)
			if err != nil {
				return 0, 0, err
			}
			return 1, pages, nil
		}

		onPage := func(pageNum int, imgPath string) error {
			return pub.PublishJSON(ctx, cfg.WorkerQueue, WorkerPageMessage{
				Filename:  lowerName,
				FilePath:  imgPath,
				TaskID:    taskID,
				Models:    models,
				Page:      pageNum,
				FileIndex: fileIdx,
			})
		}
		baseNoExt := strings.TrimSuffix(lowerName, filepath.Ext(lowerName))
		pages, err := converter.ConvertDocumentStreaming(
			ctx,
			cfg.Converter,
			srcPath,
			lowerName,
			taskSharedDir,
			fmt.Sprintf("%s_%s_page", prefixID, baseNoExt),
			onPage,
		)
		if err != nil {
			return 0, pages, err
		}
		return 1, pages, nil
	}

	if validImgExt[ext] {
		if isDuplicate {
			return 0, 0, fmt.Errorf("detección de duplicados solo soporta PDF/TIFF: %s", lowerName)
		}
		dst := filepath.Join(taskSharedDir, fmt.Sprintf("%s_%s.jpg", prefixID, strings.TrimSuffix(lowerName, filepath.Ext(lowerName))))
		if err := converter.OptimizeImageToDir(ctx, cfg.Converter, srcPath, dst); err != nil {
			return 0, 0, err
		}
		if err := pub.PublishJSON(ctx, cfg.WorkerQueue, WorkerPageMessage{
			Filename:  lowerName,
			FilePath:  dst,
			TaskID:    taskID,
			Models:    models,
			Page:      1,
			FileIndex: fileIdx,
		}); err != nil {
			return 1, 0, err
		}
		return 1, 1, nil
	}

	return 0, 0, fmt.Errorf("extensión no soportada: %s", lowerName)
}
