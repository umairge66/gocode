package graphify

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/umairge66/gocode/internal/tools/filetools"
)

// OutDirName is the directory, under the scan root, the pipeline writes to.
const OutDirName = "graphify-out"

// File categories reported by Detect.
const (
	CatCode     = "code"
	CatDocument = "document"
	CatPaper    = "paper"
	CatImage    = "image"
	CatMedia    = "media"
	// CatUnparsed is source code in a language without an extractor: its
	// files become nodes, their contents are not read.
	CatUnparsed = "unparsed"
)

// unparsedCodeExts are source files Detect recognizes but no extractor reads.
var unparsedCodeExts = func() map[string]bool {
	m := map[string]bool{}
	for _, e := range strings.Fields(`.ex .exs .erl .r .jl .ps1 .pl .pm
		.hs .ml .mli .fs .fsx .clj .cljs .zig .nim .m .mm .astro .sol .proto .tf .gd .groovy .gradle .pas
		.f90 .f95 .vb .cr .elm .ipynb .cu .cuh .ets .nix .tcl .v .sv .vhd .asm .s .cbl .apex .cls .trigger .html .css .scss`) {
		m[e] = true
	}
	return m
}()

var categoryByExt = map[string]string{
	".md": CatDocument, ".markdown": CatDocument, ".mdx": CatDocument, ".txt": CatDocument, ".rst": CatDocument,
	".pdf": CatPaper,
	".png": CatImage, ".jpg": CatImage, ".jpeg": CatImage, ".gif": CatImage, ".webp": CatImage,
	".mp4": CatMedia, ".mov": CatMedia, ".webm": CatMedia, ".mp3": CatMedia, ".wav": CatMedia, ".m4a": CatMedia,
}

// skipDirs are pruned wherever they appear, whatever the ignore files say.
var skipDirs = map[string]struct{}{
	OutDirName: {}, "node_modules": {}, "__pycache__": {},
}

// generatedDirMarkers name directories that hold dependencies or build
// output only when a marker file says so: a project directory that merely
// happens to be called vendor, target, venv, build or dist is real code.
// The marker is looked up inside the directory, or (prefix "../") next to it.
var generatedDirMarkers = map[string][]string{
	"vendor": {"modules.txt", "autoload.php"},
	"target": {"../Cargo.toml", "../pom.xml", "CACHEDIR.TAG"},
	"venv":   {"pyvenv.cfg"},
	"env":    {"pyvenv.cfg"},
	"build":  {"CMakeCache.txt", "../build.gradle", "../build.gradle.kts"},
	"dist":   {"../package.json"},
	"out":    {"../package.json"},
}

func isGeneratedDir(abs string) bool {
	for _, marker := range generatedDirMarkers[filepath.Base(abs)] {
		if _, err := os.Stat(filepath.Join(abs, filepath.FromSlash(marker))); err == nil {
			return true
		}
	}
	return false
}

// Detection is the scan summary: which files the pipeline will read.
type Detection struct {
	Root       string              `json:"scan_root"`
	Files      map[string][]string `json:"files"` // category -> relative paths
	TotalFiles int                 `json:"total_files"`
	Warning    string              `json:"warning,omitempty"`
}

// DetectOptions steers Detect.
type DetectOptions struct {
	// NoGitignore scans files the ignore files (.gitignore and friends) hide.
	NoGitignore bool
	// Exclude leaves out the files (paths relative to the root) it reports.
	Exclude func(rel string) bool
	// OutDir is the output directory when it is not the default one; if it
	// lies inside the root it is not scanned.
	OutDir string
}

// Detect walks root and sorts the files it will read into categories. Hidden
// entries, VCS directories, dependency directories and (unless NoGitignore)
// ignored files are left out, as are paths matched by a .graphifyignore file
// in root.
func Detect(ctx context.Context, root string, opt DetectOptions) (*Detection, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	d := &Detection{Root: abs, Files: map[string][]string{}}
	// .graphifyignore uses gitignore syntax, negation included.
	ignore := filetools.LoadIgnoreFile(filepath.Join(abs, ".graphifyignore"), abs)
	var pruned []string // directory prefixes ("a/b/") whose files are left out
	if out, err := filepath.Rel(abs, opt.OutDir); opt.OutDir != "" && err == nil && out != "." && !strings.HasPrefix(out, "..") {
		pruned = append(pruned, filepath.ToSlash(out)+"/")
	}
	_, err = filetools.ListPaths(ctx, abs, filetools.ListOptions{RespectIgnore: !opt.NoGitignore, SkipDirs: skipDirs},
		func(rel string, isDir bool) bool {
			for _, prefix := range pruned {
				if strings.HasPrefix(rel, prefix) {
					return true
				}
			}
			full := filepath.Join(abs, filepath.FromSlash(rel))
			if isDir {
				if ignore.Ignored(full, true) || isGeneratedDir(full) {
					pruned = append(pruned, rel+"/")
				}
				return true
			}
			if ignore.Ignored(full, false) || (opt.Exclude != nil && opt.Exclude(rel)) {
				return true
			}
			ext := strings.ToLower(path.Ext(rel))
			cat := categoryByExt[ext]
			switch {
			case isParsedExt(ext):
				cat = CatCode
			case unparsedCodeExts[ext]:
				cat = CatUnparsed
			}
			if cat != "" {
				d.Files[cat] = append(d.Files[cat], rel)
				d.TotalFiles++
			}
			return true
		})
	if err != nil {
		return nil, err
	}
	for _, list := range d.Files {
		sort.Strings(list)
	}
	if len(d.Files[CatCode])+len(d.Files[CatUnparsed])+len(d.Files[CatDocument]) == 0 {
		d.Warning = "no supported source or document files found"
	}
	return d, nil
}

// Readable lists the files the extractors take, in a stable order: parsed
// code, code in languages without an extractor, then documents.
func (d *Detection) Readable() []string {
	out := append([]string(nil), d.Files[CatCode]...)
	out = append(out, d.Files[CatUnparsed]...)
	return append(out, d.Files[CatDocument]...)
}
