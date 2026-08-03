package webpreview

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/supaclank/clank/internal/config"
)

// ModelSlug names the dictation model set, mirroring clank-mobile's
// ModelManager (MODEL_SLUG there): NVIDIA Parakeet TDT 0.6b v3 int8 for
// ASR plus Silero VAD for segmentation, both in sherpa-onnx form.
const ModelSlug = "parakeet-tdt-0.6b-v3-int8"

// modelFile is one downloadable artifact. Size is the approximate
// expected byte count, used only for progress display — the source of
// truth at download time is the response's Content-Length.
type modelFile struct {
	Name string
	URL  string
	Size int64
}

// parakeetFiles lists everything clank-voice needs, same file names the
// mobile module ships. The ASR weights come from the same Hugging Face
// repo mobile downloads from; the VAD model from sherpa-onnx's release
// assets (the canonical URL in sherpa's own docs).
var parakeetFiles = []modelFile{
	{"encoder.int8.onnx", "https://huggingface.co/csukuangfj/sherpa-onnx-nemo-parakeet-tdt-0.6b-v3-int8/resolve/main/encoder.int8.onnx", 652 << 20},
	{"decoder.int8.onnx", "https://huggingface.co/csukuangfj/sherpa-onnx-nemo-parakeet-tdt-0.6b-v3-int8/resolve/main/decoder.int8.onnx", 8 << 20},
	{"joiner.int8.onnx", "https://huggingface.co/csukuangfj/sherpa-onnx-nemo-parakeet-tdt-0.6b-v3-int8/resolve/main/joiner.int8.onnx", 3 << 20},
	{"tokens.txt", "https://huggingface.co/csukuangfj/sherpa-onnx-nemo-parakeet-tdt-0.6b-v3-int8/resolve/main/tokens.txt", 1 << 20},
	{"silero_vad.onnx", "https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/silero_vad.onnx", 2 << 20},
}

// DefaultModelsDir is where EnsureModels materializes the model set:
// <CLANK_DIR>/models/<slug>. Living under config.Dir keeps isolated
// stacks (CLANK_DIR overrides) isolated here too.
func DefaultModelsDir() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "models", ModelSlug), nil
}

// ModelsPresent reports whether every model file exists non-empty in
// dir. Partial downloads never count: files are written to a .tmp name
// and renamed only when complete.
func ModelsPresent(dir string) bool {
	for _, f := range parakeetFiles {
		info, err := os.Stat(filepath.Join(dir, f.Name))
		if err != nil || info.Size() == 0 {
			return false
		}
	}
	return true
}

// ModelDownloadProgress reports EnsureModels progress. done/total are
// bytes for the current file; total is -1 when the server sent no
// Content-Length.
type ModelDownloadProgress func(file string, index, count int, done, total int64)

// EnsureModels downloads any missing model files into dir. Idempotent
// and interrupt-safe: each file streams to <name>.tmp and is renamed
// into place only when complete, so a Ctrl+C mid-encoder re-downloads
// just that file next time.
func EnsureModels(ctx context.Context, dir string, progress ModelDownloadProgress) error {
	return ensureModelFiles(ctx, dir, parakeetFiles, progress)
}

// ensureModelFiles is EnsureModels over an explicit file set (test seam).
func ensureModelFiles(ctx context.Context, dir string, files []modelFile, progress ModelDownloadProgress) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir models dir: %w", err)
	}
	for i, f := range files {
		dst := filepath.Join(dir, f.Name)
		if info, err := os.Stat(dst); err == nil && info.Size() > 0 {
			continue
		}
		if err := downloadModelFile(ctx, f, dst, func(done, total int64) {
			if progress != nil {
				progress(f.Name, i+1, len(files), done, total)
			}
		}); err != nil {
			return fmt.Errorf("download %s: %w", f.Name, err)
		}
	}
	return nil
}

func downloadModelFile(ctx context.Context, f modelFile, dst string, progress func(done, total int64)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return err
	}
	// No client timeout: the encoder is ~650 MB and ctx handles Ctrl+C.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, f.URL)
	}

	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer os.Remove(tmp) // no-op after the successful rename

	pw := &progressWriter{w: out, total: resp.ContentLength, report: progress}
	if _, err := io.Copy(pw, resp.Body); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// progressWriter reports roughly per-MiB so terminal progress lines
// don't spam.
type progressWriter struct {
	w          io.Writer
	done       int64
	total      int64
	lastReport int64
	report     func(done, total int64)
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.done += int64(n)
	if p.report != nil && (p.done-p.lastReport >= 1<<20 || err != nil || p.done == p.total) {
		p.lastReport = p.done
		p.report(p.done, p.total)
	}
	return n, err
}
