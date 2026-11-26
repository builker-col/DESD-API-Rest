package converter

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"desd-api/internal/config"
)

type PageCallback func(pageNum int, imagePath string) error

func ConvertDocumentStreaming(
	ctx context.Context,
	cfg config.ConverterConfig,
	inputPath string,
	filename string,
	outputDir string,
	pagePrefix string,
	onPage PageCallback,
) (int, error) {
	if err := os.MkdirAll(outputDir, 0o777); err != nil {
		return 0, err
	}
	lower := strings.ToLower(filename)
	start := time.Now()

	var totalPages int
	var mode string
	var err error

	switch {
	case strings.HasSuffix(lower, ".pdf"):
		totalPages, mode, err = convertPDFStreaming(ctx, cfg, inputPath, outputDir, pagePrefix, onPage)
	case strings.HasSuffix(lower, ".tif") || strings.HasSuffix(lower, ".tiff"):
		mode = "vips-tiff"
		totalPages, err = convertTIFFWithVips(ctx, cfg, inputPath, outputDir, pagePrefix, onPage)
	default:
		return 0, fmt.Errorf("extensión de documento no soportada: %s", filename)
	}

	if err != nil {
		return totalPages, err
	}
	fmt.Printf("[converter] %s -> %d pages (%s, max=%dpx) in %.3fs\n",
		filename, totalPages, mode, cfg.MaxDim, time.Since(start).Seconds())
	return totalPages, nil
}

func OptimizeImageToDir(ctx context.Context, cfg config.ConverterConfig, srcPath, dstPath string) error {
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o777); err != nil {
		return err
	}
	info, err := os.Stat(srcPath)
	if err != nil {
		return err
	}
	lower := strings.ToLower(srcPath)
	isJPEG := strings.HasSuffix(lower, ".jpg") || strings.HasSuffix(lower, ".jpeg")

	if isJPEG && info.Size() <= 300*1024 {
		return CopyFile(srcPath, dstPath)
	}
	if cfg.MaxDim > 0 {
		outSpec := fmt.Sprintf("%s[Q=%d,strip]", dstPath, cfg.JPEGQuality)
		dim := strconv.Itoa(cfg.MaxDim)
		if err := exec.CommandContext(ctx, "vips", "thumbnail", srcPath, outSpec, dim, "--height", dim).Run(); err == nil {
			return nil
		}
	}
	return CopyFile(srcPath, dstPath)
}

func inspectPDF(ctx context.Context, pdfPath string) (numPages int, pageRot int, ok bool) {
	infoCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	out, err := exec.CommandContext(infoCtx, "pdfinfo", pdfPath).Output()
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "Pages:") {
			numPages, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "Pages:")))
		} else if strings.HasPrefix(line, "Page rot:") {
			pageRot, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "Page rot:")))
		}
	}
	return numPages, pageRot, numPages > 0
}

func isFastPDFImagesCandidate(ctx context.Context, pdfPath string, numPages int, pageRot int) bool {
	if numPages < 1 || pageRot != 0 {
		return false
	}
	listCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	out, err := exec.CommandContext(listCtx, "pdfimages", "-list", pdfPath).Output()
	if err != nil {
		return false
	}
	lines := strings.Split(string(out), "\n")
	if len(lines) <= 2 {
		return false
	}

	pagesSeen := make([]int, 0, numPages)
	for _, line := range lines[2:] {
		fields := strings.Fields(line)
		if len(fields) < 9 {
			continue
		}
		pNum, err1 := strconv.Atoi(fields[0])
		imgType := strings.ToLower(fields[2])
		w, err2 := strconv.Atoi(fields[3])
		h, err3 := strconv.Atoi(fields[4])
		enc := strings.ToLower(fields[8])
		if err1 != nil || err2 != nil || err3 != nil {
			return false
		}
		if imgType != "image" || enc != "jpeg" || w < 400 || h < 400 {
			return false
		}
		pagesSeen = append(pagesSeen, pNum)
	}
	if len(pagesSeen) != numPages {
		return false
	}
	for i, p := range pagesSeen {
		if p != i+1 {
			return false
		}
	}
	return true
}

func convertPDFStreaming(
	ctx context.Context,
	cfg config.ConverterConfig,
	pdfPath, outputDir, pagePrefix string,
	onPage PageCallback,
) (int, string, error) {
	numPages, pageRot, ok := inspectPDF(ctx, pdfPath)

	if ok && cfg.FastPDFImages && isFastPDFImagesCandidate(ctx, pdfPath, numPages, pageRot) {
		rawPrefix := filepath.Join(outputDir, "__fast_"+pagePrefix)
		cmdCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		err := exec.CommandContext(cmdCtx, "pdfimages", "-j", pdfPath, rawPrefix).Run()
		cancel()

		if err == nil {
			matches, _ := filepath.Glob(rawPrefix + "-*.jpg")
			sort.Strings(matches)
			if len(matches) == numPages {
				for i, src := range matches {
					dst := filepath.Join(outputDir, fmt.Sprintf("%s_%d.jpg", pagePrefix, i+1))
					if err := os.Rename(src, dst); err != nil {
						cleanupGlob(rawPrefix + "-*")
						return i, "pdfimages-fast", err
					}
					if onPage != nil {
						if err := onPage(i+1, dst); err != nil {
							return i, "pdfimages-fast", err
						}
					}
				}
				return numPages, "pdfimages-fast", nil
			}
		}
		cleanupGlob(rawPrefix + "-*")
	}

	if !ok || numPages < 1 {
		return convertPDFSingleShot(ctx, cfg, pdfPath, outputDir, pagePrefix, onPage)
	}

	chunkSize := cfg.PDFChunkSize
	if chunkSize < 1 {
		chunkSize = 8
	}

	type pageRange struct{ start, end int }
	var chunks []pageRange
	for s := 1; s <= numPages; s += chunkSize {
		e := s + chunkSize - 1
		if e > numPages {
			e = numPages
		}
		chunks = append(chunks, pageRange{start: s, end: e})
	}

	concurrency := runtime.NumCPU()
	if len(chunks) < concurrency {
		concurrency = len(chunks)
	}
	if concurrency < 1 {
		concurrency = 1
	}

	sem := make(chan struct{}, concurrency)
	errCh := make(chan error, len(chunks))
	var wg sync.WaitGroup

	for _, ch := range chunks {
		wg.Add(1)
		go func(r pageRange) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			paths, err := renderPDFRangeWithMutool(ctx, cfg, pdfPath, outputDir, pagePrefix, r.start, r.end)
			if err != nil {
				paths, err = renderPDFRangeWithPdftoppm(ctx, cfg, pdfPath, outputDir, pagePrefix, r.start, r.end)
				if err != nil {
					errCh <- err
					return
				}
			}
			if onPage != nil {
				for idx, p := range paths {
					if cbErr := onPage(r.start+idx, p); cbErr != nil {
						errCh <- cbErr
						return
					}
				}
			}
		}(ch)
	}

	wg.Wait()
	close(errCh)
	for e := range errCh {
		if e != nil {
			return 0, "mutool-parallel", e
		}
	}
	return numPages, "mutool-parallel", nil
}

func renderPDFRangeWithMutool(ctx context.Context, cfg config.ConverterConfig, pdfPath, outputDir, pagePrefix string, startPage, endPage int) ([]string, error) {
	chunkTag := fmt.Sprintf("__m_%s_%d_%d", pagePrefix, startPage, endPage)
	outPattern := filepath.Join(outputDir, chunkTag+"_%05d.jpg")
	pageSpec := fmt.Sprintf("%d-%d", startPage, endPage)

	args := []string{"draw", "-q"}
	if cfg.MaxDim > 0 {
		dim := strconv.Itoa(cfg.MaxDim)
		args = append(args, "-w", dim, "-h", dim)
	} else {
		args = append(args, "-r", strconv.Itoa(cfg.DPI))
	}
	args = append(args, "-o", outPattern, pdfPath, pageSpec)

	if out, err := exec.CommandContext(ctx, "mutool", args...).CombinedOutput(); err != nil {
		cleanupGlob(filepath.Join(outputDir, chunkTag+"_*.jpg"))
		return nil, fmt.Errorf("mutool error: %v (%s)", err, strings.TrimSpace(string(out)))
	}

	matches, _ := filepath.Glob(filepath.Join(outputDir, chunkTag+"_*.jpg"))
	sort.Strings(matches)
	expected := endPage - startPage + 1
	if len(matches) != expected {
		cleanupGlob(filepath.Join(outputDir, chunkTag+"_*.jpg"))
		return nil, fmt.Errorf("mutool produjo %d imágenes, esperaba %d", len(matches), expected)
	}

	finalPaths := make([]string, 0, len(matches))
	for i, src := range matches {
		dst := filepath.Join(outputDir, fmt.Sprintf("%s_%d.jpg", pagePrefix, startPage+i))
		if err := os.Rename(src, dst); err != nil {
			return nil, err
		}
		finalPaths = append(finalPaths, dst)
	}
	return finalPaths, nil
}

func renderPDFRangeWithPdftoppm(ctx context.Context, cfg config.ConverterConfig, pdfPath, outputDir, pagePrefix string, startPage, endPage int) ([]string, error) {
	rawPrefix := filepath.Join(outputDir, fmt.Sprintf("__p_%s_%d_%d", pagePrefix, startPage, endPage))
	jpegOpt := fmt.Sprintf("quality=%d,progressive=n,optimize=n", cfg.JPEGQuality)

	args := []string{"-jpeg", "-jpegopt", jpegOpt, "-f", strconv.Itoa(startPage), "-l", strconv.Itoa(endPage)}
	if cfg.MaxDim > 0 {
		args = append(args, "-scale-to", strconv.Itoa(cfg.MaxDim))
	} else {
		args = append(args, "-r", strconv.Itoa(cfg.DPI))
	}
	args = append(args, pdfPath, rawPrefix)

	if out, err := exec.CommandContext(ctx, "pdftoppm", args...).CombinedOutput(); err != nil {
		cleanupGlob(rawPrefix + "-*")
		return nil, fmt.Errorf("pdftoppm error: %v (%s)", err, strings.TrimSpace(string(out)))
	}

	matches, _ := filepath.Glob(rawPrefix + "-*.jpg")
	sort.Strings(matches)
	finalPaths := make([]string, 0, len(matches))
	for i, src := range matches {
		dst := filepath.Join(outputDir, fmt.Sprintf("%s_%d.jpg", pagePrefix, startPage+i))
		if err := os.Rename(src, dst); err != nil {
			return nil, err
		}
		finalPaths = append(finalPaths, dst)
	}
	return finalPaths, nil
}

func convertPDFSingleShot(ctx context.Context, cfg config.ConverterConfig, pdfPath, outputDir, pagePrefix string, onPage PageCallback) (int, string, error) {
	rawPrefix := filepath.Join(outputDir, "__s_"+pagePrefix)
	args := []string{"-jpeg", "-jpegopt", fmt.Sprintf("quality=%d,progressive=n,optimize=n", cfg.JPEGQuality)}
	if cfg.MaxDim > 0 {
		args = append(args, "-scale-to", strconv.Itoa(cfg.MaxDim))
	} else {
		args = append(args, "-r", strconv.Itoa(cfg.DPI))
	}
	args = append(args, pdfPath, rawPrefix)
	if out, err := exec.CommandContext(ctx, "pdftoppm", args...).CombinedOutput(); err != nil {
		return 0, "pdftoppm", fmt.Errorf("pdftoppm falló: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	matches, _ := filepath.Glob(rawPrefix + "-*.jpg")
	sort.Strings(matches)
	for i, src := range matches {
		dst := filepath.Join(outputDir, fmt.Sprintf("%s_%d.jpg", pagePrefix, i+1))
		if err := os.Rename(src, dst); err != nil {
			return i, "pdftoppm", err
		}
		if onPage != nil {
			if err := onPage(i+1, dst); err != nil {
				return i, "pdftoppm", err
			}
		}
	}
	return len(matches), "pdftoppm", nil
}

func convertTIFFWithVips(ctx context.Context, cfg config.ConverterConfig, tiffPath, outputDir, pagePrefix string, onPage PageCallback) (int, error) {
	nPages := 1
	if out, err := exec.CommandContext(ctx, "vipsheader", "-f", "n-pages", tiffPath).Output(); err == nil {
		if n, parseErr := strconv.Atoi(strings.TrimSpace(string(out))); parseErr == nil && n > 0 {
			nPages = n
		}
	}

	concurrency := runtime.NumCPU()
	if nPages < concurrency {
		concurrency = nPages
	}
	if concurrency < 1 {
		concurrency = 1
	}

	sem := make(chan struct{}, concurrency)
	errCh := make(chan error, nPages)
	var wg sync.WaitGroup

	for p := 0; p < nPages; p++ {
		wg.Add(1)
		go func(pageIdx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			srcSpec := fmt.Sprintf("%s[page=%d]", tiffPath, pageIdx)
			dst := filepath.Join(outputDir, fmt.Sprintf("%s_%d.jpg", pagePrefix, pageIdx+1))
			outSpec := fmt.Sprintf("%s[Q=%d,strip]", dst, cfg.JPEGQuality)

			var cmd *exec.Cmd
			if cfg.MaxDim > 0 {
				dim := strconv.Itoa(cfg.MaxDim)
				cmd = exec.CommandContext(ctx, "vips", "thumbnail", srcSpec, outSpec, dim, "--height", dim)
			} else {
				cmd = exec.CommandContext(ctx, "vips", "copy", srcSpec, outSpec)
			}
			if out, err := cmd.CombinedOutput(); err != nil {
				errCh <- fmt.Errorf("vips falló en página %d: %v (%s)", pageIdx+1, err, strings.TrimSpace(string(out)))
				return
			}
			if onPage != nil {
				if cbErr := onPage(pageIdx+1, dst); cbErr != nil {
					errCh <- cbErr
				}
			}
		}(p)
	}

	wg.Wait()
	close(errCh)
	for e := range errCh {
		if e != nil {
			return 0, e
		}
	}
	return nPages, nil
}

func cleanupGlob(pattern string) {
	matches, _ := filepath.Glob(pattern)
	for _, m := range matches {
		_ = os.Remove(m)
	}
}

func CopyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func MoveFileSafe(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o777); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := CopyFile(src, dst); err != nil {
		return err
	}
	return os.Remove(src)
}
