package mods

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

// HooksFileRel is the plugin's hooks file, which names its hooks module.
const HooksFileRel = "hooks/hooks.json"

// codeSuffixes are the file names a hooks module and every file it imports
// may have; a file named otherwise is not loaded.
var codeSuffixes = []string{".ts", ".tsx", ".jsx", ".js", ".mjs", ".cjs", ".mts", ".cts"}

var loaders = map[string]api.Loader{
	".ts": api.LoaderTS, ".mts": api.LoaderTS, ".cts": api.LoaderTS,
	".tsx": api.LoaderTSX, ".jsx": api.LoaderJSX,
	".js": api.LoaderJS, ".mjs": api.LoaderJS, ".cjs": api.LoaderJS,
}

// hasCodeSuffix reports whether name is named like code.
func hasCodeSuffix(name string) bool {
	return slices.Contains(codeSuffixes, strings.ToLower(filepath.Ext(name)))
}

// HooksFileError is a hooks.json the engine refuses.
type HooksFileError struct {
	File, Reason, Todo string
}

func (e *HooksFileError) Error() string {
	return fmt.Sprintf("%s %s. %s", e.File, e.Reason, e.Todo)
}

// ModulePath returns the absolute path of the hooks module the plugin at root
// names in hooks/hooks.json "modules", or "" when it names none.
//
// "modules" is a list of at most one string, the module's path relative to
// hooks.json.
func ModulePath(root string) (string, error) {
	file := filepath.Join(root, filepath.FromSlash(HooksFileRel))
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return "", &HooksFileError{File: file, Reason: "is read as a hooks file and is not a JSON object", Todo: `Write it as {"hooks": {}}, with the hook events inside the inner braces.`}
	}
	mods, has := raw["modules"]
	if !has {
		return "", nil
	}
	list, ok := mods.([]any)
	if !ok || len(list) > 1 {
		return "", &HooksFileError{File: file, Reason: `has a "modules" that is not a list of at most one string`, Todo: `Write "modules" as a list that holds the path of one hooks module, or remove it.`}
	}
	if len(list) == 0 {
		return "", nil
	}
	rel, ok := list[0].(string)
	if !ok || rel == "" {
		return "", &HooksFileError{File: file, Reason: `has a "modules" that is not a list of at most one string`, Todo: `Write "modules" as a list that holds the path of one hooks module, or remove it.`}
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%s: the hooks module %q is not a path relative to hooks.json", file, rel)
	}
	p := filepath.Clean(filepath.Join(filepath.Dir(file), filepath.FromSlash(rel)))
	if !within(root, p) {
		return "", fmt.Errorf("%s: the hooks module %q is outside the plugin's folder", file, rel)
	}
	if _, err := os.Lstat(p); err == nil && !withinReal(root, p) {
		// A symbolic link (the module, or a folder on its way) must not lead
		// the bundler to a file outside the plugin, as imports may not.
		return "", fmt.Errorf("%s: the hooks module %q is outside the plugin's folder", file, rel)
	}
	if !hasCodeSuffix(p) {
		return "", fmt.Errorf("%s: the hooks module %q is not named like code (%s)", file, rel, strings.Join(codeSuffixes, ", "))
	}
	return p, nil
}

// within reports whether p is root or inside it (lexically).
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// withinReal is within after following symbolic links on both sides, so a
// link inside the plugin cannot reach a file outside it.
func withinReal(root, p string) bool {
	rr, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	rp, err := filepath.EvalSymlinks(p)
	if err != nil {
		return false
	}
	return within(rr, rp)
}

// virtualNS is the namespace of the 'gocode' import, whose run-time module is
// empty.
const virtualNS = "gocode-virtual"

// bundleGlobal holds the module namespace in the compiled script.
const bundleGlobal = "__gocodeHooksModule"

// Compile turns a hooks module and the plugin files it imports into one
// script that runs in a hooks environment: TypeScript and JSX are stripped
// (JSX compiles against `h` and `Fragment`), imports are bundled, and the
// module's exports end up on a global.
//
// A module imports only files of its own plugin (named like code, inside the
// plugin folder after links are followed) and 'gocode' (types only). There
// is no require, and a module holding import() does not load. esbuild never
// reads a tsconfig: the options are fixed here.
func Compile(root, entry string) (string, error) {
	return compile(root, entry, "", nil)
}

// compile is Compile with optional extra virtual modules (the test kit) and,
// when stdin is set, a virtual entry module resolved from root in place of
// entry.
func compile(root, entry, stdin string, virtual map[string]string) (string, error) {
	plugin := api.Plugin{
		Name: "gocode-mods",
		Setup: func(b api.PluginBuild) {
			b.OnResolve(api.OnResolveOptions{Filter: ".*"}, func(a api.OnResolveArgs) (api.OnResolveResult, error) {
				switch a.Kind {
				case api.ResolveJSDynamicImport:
					return api.OnResolveResult{}, fmt.Errorf("a module holding import() does not load (import(%q))", a.Path)
				case api.ResolveJSRequireCall, api.ResolveJSRequireResolve:
					return api.OnResolveResult{}, fmt.Errorf("there is no require in a hooks environment (require(%q)); import the file instead", a.Path)
				}
				if a.Kind == api.ResolveEntryPoint {
					if stdin == "" && !withinReal(root, a.Path) {
						return api.OnResolveResult{}, fmt.Errorf("the hooks module %q is outside the plugin's folder", a.Path)
					}
					return api.OnResolveResult{Path: a.Path}, nil
				}
				if a.Path == "gocode" {
					return api.OnResolveResult{Path: "gocode", Namespace: virtualNS}, nil
				}
				if _, ok := virtual[a.Path]; ok {
					return api.OnResolveResult{Path: a.Path, Namespace: virtualNS}, nil
				}
				if !strings.HasPrefix(a.Path, "./") && !strings.HasPrefix(a.Path, "../") && !filepath.IsAbs(a.Path) {
					return api.OnResolveResult{}, fmt.Errorf("cannot import %q: a hooks module imports only files of its plugin and 'gocode'", a.Path)
				}
				p := a.Path
				if !filepath.IsAbs(p) {
					p = filepath.Join(a.ResolveDir, filepath.FromSlash(p))
				}
				p = filepath.Clean(p)
				found := resolveCodeFile(p)
				if found == "" {
					return api.OnResolveResult{}, fmt.Errorf("cannot import %q: no file named like code (%s) there", a.Path, strings.Join(codeSuffixes, ", "))
				}
				if !within(root, found) || !withinReal(root, found) {
					return api.OnResolveResult{}, fmt.Errorf("cannot import %q: it is outside the plugin's folder", a.Path)
				}
				return api.OnResolveResult{Path: found}, nil
			})
			b.OnLoad(api.OnLoadOptions{Filter: ".*", Namespace: virtualNS}, func(a api.OnLoadArgs) (api.OnLoadResult, error) {
				src := "export {};"
				if v, ok := virtual[a.Path]; ok {
					src = v
				}
				return api.OnLoadResult{Contents: &src, Loader: api.LoaderJS}, nil
			})
		},
	}
	opts := api.BuildOptions{
		Bundle:        true,
		Write:         false,
		Format:        api.FormatIIFE,
		GlobalName:    bundleGlobal,
		Platform:      api.PlatformNeutral,
		Target:        api.ES2017,
		JSX:           api.JSXTransform,
		JSXFactory:    "h",
		JSXFragment:   "Fragment",
		Loader:        loaders,
		Plugins:       []api.Plugin{plugin},
		LogLevel:      api.LogLevelSilent,
		Charset:       api.CharsetUTF8,
		AbsWorkingDir: root,
		TsconfigRaw:   "{}",
		MainFields:    []string{},
		Conditions:    []string{},
	}
	if stdin != "" {
		opts.Stdin = &api.StdinOptions{Contents: stdin, ResolveDir: root, Sourcefile: "<gocode plugin test>", Loader: api.LoaderJS}
	} else {
		if !within(root, entry) || !withinReal(root, entry) {
			return "", fmt.Errorf("%s is outside the plugin's folder %s", entry, root)
		}
		if !hasCodeSuffix(entry) {
			return "", fmt.Errorf("%s is not named like code (%s)", entry, strings.Join(codeSuffixes, ", "))
		}
		opts.EntryPoints = []string{entry}
	}
	res := api.Build(opts)
	if len(res.Errors) > 0 {
		var msgs []string
		for _, m := range res.Errors {
			loc := ""
			if m.Location != nil {
				loc = fmt.Sprintf("%s:%d:%d: ", m.Location.File, m.Location.Line, m.Location.Column)
			}
			msgs = append(msgs, loc+m.Text)
		}
		return "", errors.New(strings.Join(msgs, "; "))
	}
	if len(res.OutputFiles) == 0 {
		return "", errors.New("the module compiled to nothing")
	}
	return string(res.OutputFiles[0].Contents), nil
}

// resolveCodeFile finds the file an import names: the path itself when it is
// a regular file named like code, else the path with a code suffix, else an
// index file of that directory.
func resolveCodeFile(p string) string {
	isFile := func(f string) bool {
		st, err := os.Stat(f)
		return err == nil && st.Mode().IsRegular()
	}
	if hasCodeSuffix(p) && isFile(p) {
		return p
	}
	// A ".js" spelling of a TypeScript file (the usual ESM convention).
	if ext := filepath.Ext(p); ext == ".js" || ext == ".mjs" || ext == ".cjs" {
		base := strings.TrimSuffix(p, ext)
		for _, s := range []string{".ts", ".tsx", ".mts", ".cts"} {
			if isFile(base + s) {
				return base + s
			}
		}
	}
	for _, s := range codeSuffixes {
		if isFile(p + s) {
			return p + s
		}
	}
	for _, s := range codeSuffixes {
		if f := filepath.Join(p, "index"+s); isFile(f) {
			return f
		}
	}
	return ""
}
