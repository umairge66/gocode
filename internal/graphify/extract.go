package graphify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// maxFileBytes is the largest file an extractor reads; bigger files are
// almost always generated or data.
const maxFileBytes = 1 << 20

// ExtractOptions steers Extract.
type ExtractOptions struct {
	// CacheDir holds one JSON file per extracted file content; "" disables
	// the cache.
	CacheDir string
	// Force re-extracts every file, ignoring the cache.
	Force bool
}

// ExtractStats counts what Extract did.
type ExtractStats struct {
	Extracted int // read and parsed in this run
	Cached    int // taken from the cache
	Skipped   int // unreadable, too large, binary or minified
}

// Extract reads the files (paths relative to root) and returns one
// FileExtraction per readable file, in the order given. Unchanged files come
// from the cache, which is keyed by the SHA-256 of path and content; entries
// no file used are removed.
func Extract(ctx context.Context, root string, paths []string, opt ExtractOptions) ([]*FileExtraction, ExtractStats, error) {
	if opt.CacheDir != "" {
		if err := os.MkdirAll(opt.CacheDir, 0o755); err != nil {
			return nil, ExtractStats{}, err
		}
	}
	out := make([]*FileExtraction, len(paths))
	keys := make([]string, len(paths))
	cached := make([]bool, len(paths))

	var wg sync.WaitGroup
	jobs := make(chan int)
	for w := 0; w < max(1, runtime.NumCPU()); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				out[i], keys[i], cached[i] = extractOne(root, paths[i], opt)
			}
		}()
	}
feed:
	for i := range paths {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, ExtractStats{}, err
	}

	var stats ExtractStats
	used := map[string]bool{}
	res := out[:0]
	for i, fe := range out {
		switch {
		case fe == nil:
			stats.Skipped++
			continue
		case cached[i]:
			stats.Cached++
		default:
			stats.Extracted++
		}
		used[keys[i]+".json"] = true
		res = append(res, fe)
	}
	if opt.CacheDir != "" {
		if entries, err := os.ReadDir(opt.CacheDir); err == nil {
			for _, e := range entries {
				if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") && !used[e.Name()] {
					_ = os.Remove(filepath.Join(opt.CacheDir, e.Name()))
				}
			}
		}
	}
	return res, stats, nil
}

func extractOne(root, rel string, opt ExtractOptions) (fe *FileExtraction, key string, fromCache bool) {
	file := filepath.Join(root, filepath.FromSlash(rel))
	info, err := os.Stat(file)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxFileBytes {
		return nil, "", false
	}
	src, err := os.ReadFile(file)
	if err != nil || bytes.IndexByte(src[:min(len(src), 8000)], 0) >= 0 {
		return nil, "", false
	}
	if specForExt(strings.ToLower(path.Ext(rel))) != nil && looksMinified(src) {
		return nil, "", false
	}
	h := sha256.New()
	h.Write([]byte(extractorVersion + "\x00" + rel + "\x00"))
	h.Write(src)
	key = hex.EncodeToString(h.Sum(nil))
	cacheFile := ""
	if opt.CacheDir != "" {
		cacheFile = filepath.Join(opt.CacheDir, key+".json")
		if !opt.Force {
			if data, err := os.ReadFile(cacheFile); err == nil {
				var c FileExtraction
				if json.Unmarshal(data, &c) == nil && c.Path == rel {
					return &c, key, true
				}
			}
		}
	}
	fe = ExtractFile(rel, src)
	if cacheFile != "" {
		if data, err := json.Marshal(fe); err == nil {
			_ = os.WriteFile(cacheFile, data, 0o644)
		}
	}
	return fe, key, false
}

// looksMinified reports bundled or minified code: a line no person wrote
// (over 2000 bytes), or lines averaging over 250 bytes. Such files would
// only add thousands of meaningless one-letter definitions.
func looksMinified(src []byte) bool {
	lines, longest, cur := 1, 0, 0
	for _, c := range src {
		if c == '\n' {
			lines++
			longest, cur = max(longest, cur), 0
		} else {
			cur++
		}
	}
	longest = max(longest, cur)
	return longest > 2000 || (len(src) > 2000 && len(src)/lines > 250)
}

// ExtractFile extracts one file's nodes, edges and unresolved references from
// its content. rel is the "/"-separated path used in node ids.
func ExtractFile(rel string, src []byte) *FileExtraction {
	ext := strings.ToLower(path.Ext(rel))
	switch {
	case ext == ".go":
		return extractGo(rel, src)
	case specForExt(ext) != nil:
		return extractGeneric(rel, src, specForExt(ext))
	case ext == ".sql":
		return extractSQL(rel, src)
	case embeddedJSExts[ext]:
		return extractEmbeddedJS(rel, src)
	case unparsedCodeExts[ext]:
		// A language without an extractor: the file is in the graph (so
		// documents can link to it and queries can find it) but not its
		// contents.
		return newFileExtraction(rel, strings.TrimPrefix(ext, "."), "", "code")
	default:
		return extractDocument(rel, src)
	}
}
