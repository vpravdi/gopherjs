package build

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/scanner"
	"go/token"
	"go/types"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gopherjs/gopherjs/compiler"
	"github.com/gopherjs/gopherjs/compiler/gopherjspkg"
	"github.com/gopherjs/gopherjs/compiler/natives"
	"github.com/neelance/sourcemap"
	"github.com/shurcooL/httpfs/vfsutil"
	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/buildutil"
)

// DefaultGOROOT is the default GOROOT value for builds.
//
// It uses the GOPHERJS_GOROOT environment variable if it is set,
// or else the default GOROOT value of the system Go distrubtion.
var DefaultGOROOT = func() string {
	if goroot, ok := os.LookupEnv("GOPHERJS_GOROOT"); ok {
		// GopherJS-specific GOROOT value takes precedence.
		return goroot
	}
	// The usual default GOROOT.
	return build.Default.GOROOT
}()

type ImportCError struct {
	pkgPath string
}

func (e *ImportCError) Error() string {
	return e.pkgPath + `: importing "C" is not supported by GopherJS`
}

// NewBuildContext creates a build context for building Go packages
// with GopherJS compiler.
//
// Core GopherJS packages (i.e., "github.com/gopherjs/gopherjs/js", "github.com/gopherjs/gopherjs/nosync")
// are loaded from gopherjspkg.FS virtual filesystem rather than GOPATH.
func NewBuildContext(installSuffix string, buildTags []string) *build.Context {
	gopherjsRoot := filepath.Join(DefaultGOROOT, "src", "github.com", "gopherjs", "gopherjs")
	return &build.Context{
		GOROOT:        DefaultGOROOT,
		GOPATH:        build.Default.GOPATH,
		GOOS:          build.Default.GOOS,
		GOARCH:        "js",
		InstallSuffix: installSuffix,
		Compiler:      "gc",
		BuildTags: append(buildTags,
			"netgo",  // See https://godoc.org/net#hdr-Name_Resolution.
			"purego", // See https://golang.org/issues/23172.
		),
		ReleaseTags: build.Default.ReleaseTags[:compiler.GoVersion],
		CgoEnabled:  true, // detect `import "C"` to throw proper error

		IsDir: func(path string) bool {
			if strings.HasPrefix(path, gopherjsRoot+string(filepath.Separator)) {
				path = filepath.ToSlash(path[len(gopherjsRoot):])
				if fi, err := vfsutil.Stat(gopherjspkg.FS, path); err == nil {
					return fi.IsDir()
				}
			}
			fi, err := os.Stat(path)
			return err == nil && fi.IsDir()
		},
		ReadDir: func(path string) ([]os.FileInfo, error) {
			if strings.HasPrefix(path, gopherjsRoot+string(filepath.Separator)) {
				path = filepath.ToSlash(path[len(gopherjsRoot):])
				if fis, err := vfsutil.ReadDir(gopherjspkg.FS, path); err == nil {
					return fis, nil
				}
			}
			return ioutil.ReadDir(path)
		},
		OpenFile: func(path string) (io.ReadCloser, error) {
			if strings.HasPrefix(path, gopherjsRoot+string(filepath.Separator)) {
				path = filepath.ToSlash(path[len(gopherjsRoot):])
				if f, err := gopherjspkg.FS.Open(path); err == nil {
					return f, nil
				}
			}
			return os.Open(path)
		},
	}
}

// statFile returns an os.FileInfo describing the named file.
// For files in "$GOROOT/src/github.com/gopherjs/gopherjs" directory,
// gopherjspkg.FS is consulted first.
func statFile(path string) (os.FileInfo, error) {
	gopherjsRoot := filepath.Join(DefaultGOROOT, "src", "github.com", "gopherjs", "gopherjs")
	if strings.HasPrefix(path, gopherjsRoot+string(filepath.Separator)) {
		path = filepath.ToSlash(path[len(gopherjsRoot):])
		if fi, err := vfsutil.Stat(gopherjspkg.FS, path); err == nil {
			return fi, nil
		}
	}
	return os.Stat(path)
}

// Import returns details about the Go package named by the import path. If the
// path is a local import path naming a package that can be imported using
// a standard import path, the returned package will set p.ImportPath to
// that path.
//
// In the directory containing the package, .go and .inc.js files are
// considered part of the package except for:
//
//    - .go files in package documentation
//    - files starting with _ or . (likely editor temporary files)
//    - files with build constraints not satisfied by the context
//
// If an error occurs, Import returns a non-nil error and a nil
// *PackageData.
func Import(path string, mode build.ImportMode, installSuffix string, buildTags []string) (*PackageData, error) {
	wd, err := os.Getwd()
	if err != nil {
		// Getwd may fail if we're in GOARCH=js mode. That's okay, handle
		// it by falling back to empty working directory. It just means
		// Import will not be able to resolve relative import paths.
		wd = ""
	}
	bctx := NewBuildContext(installSuffix, buildTags)
	return importWithSrcDir(*bctx, path, wd, mode, installSuffix)
}

func importWithSrcDir(bctx build.Context, path string, srcDir string, mode build.ImportMode, installSuffix string) (*PackageData, error) {
	// bctx is passed by value, so it can be modified here.
	var isVirtual bool
	switch path {
	case "syscall":
		// syscall needs to use a typical GOARCH like amd64 to pick up definitions for _Socklen, BpfInsn, IFNAMSIZ, Timeval, BpfStat, SYS_FCNTL, Flock_t, etc.
		bctx.GOARCH = build.Default.GOARCH
		bctx.InstallSuffix = "js"
		if installSuffix != "" {
			bctx.InstallSuffix += "_" + installSuffix
		}
	case "syscall/js":
		// There are no buildable files in this package, but we need to use files in the virtual directory.
		mode |= build.FindOnly
	case "math/big":
		// Use pure Go version of math/big; we don't want non-Go assembly versions.
		bctx.BuildTags = append(bctx.BuildTags, "math_big_pure_go")
	case "crypto/x509", "os/user":
		// These stdlib packages have cgo and non-cgo versions (via build tags); we want the latter.
		bctx.CgoEnabled = false
	case "github.com/gopherjs/gopherjs/js", "github.com/gopherjs/gopherjs/nosync":
		// These packages are already embedded via gopherjspkg.FS virtual filesystem (which can be
		// safely vendored). Don't try to use vendor directory to resolve them.
		mode |= build.IgnoreVendor
		isVirtual = true
	}
	pkg, err := bctx.Import(path, srcDir, mode)
	if err != nil {
		return nil, err
	}

	switch path {
	case "os":
		pkg.GoFiles = excludeExecutable(pkg.GoFiles) // Need to exclude executable implementation files, because some of them contain package scope variables that perform (indirectly) syscalls on init.
		// Prefer the dirent_${GOOS}.go version, to make the build pass on both linux
		// and darwin.
		// In the long term, our builds should produce the same output regardless
		// of the host OS: https://github.com/gopherjs/gopherjs/issues/693.
		pkg.GoFiles = exclude(pkg.GoFiles, "dirent_js.go")
	case "runtime":
		pkg.GoFiles = []string{} // Package sources are completely replaced in natives.
	case "runtime/internal/sys":
		pkg.GoFiles = []string{fmt.Sprintf("zgoos_%s.go", bctx.GOOS), "zversion.go"}
	case "runtime/pprof":
		pkg.GoFiles = nil
	case "internal/poll":
		pkg.GoFiles = exclude(pkg.GoFiles, "fd_poll_runtime.go")
	case "sync":
		// GopherJS completely replaces sync.Pool implementation with a simpler one,
		// since it always executes in a single-threaded environment.
		pkg.GoFiles = exclude(pkg.GoFiles, "pool.go")
	case "crypto/rand":
		pkg.GoFiles = []string{"rand.go", "util.go"}
		pkg.TestGoFiles = exclude(pkg.TestGoFiles, "rand_linux_test.go") // Don't want linux-specific tests (since linux-specific package files are excluded too).
	case "crypto/x509":
		// GopherJS doesn't support loading OS root certificates regardless of the
		// OS. The substitution below allows to avoid build dependency on Mac OS
		// implementation, which won't be used anyway.
		//
		// Just like above, https://github.com/gopherjs/gopherjs/issues/693 is
		// probably the best long-term option.
		pkg.GoFiles = include(
			exclude(pkg.GoFiles, fmt.Sprintf("root_%s.go", bctx.GOOS)),
			"root_unix.go", "root_js.go")
	case "syscall/js":
		// Reuse upstream tests to ensure conformance, but completely replace
		// implementation.
		pkg.XTestGoFiles = append(pkg.TestGoFiles, "js_test.go")
	}

	if len(pkg.CgoFiles) > 0 {
		return nil, &ImportCError{path}
	}

	if pkg.IsCommand() {
		pkg.PkgObj = filepath.Join(pkg.BinDir, filepath.Base(pkg.ImportPath)+".js")
	}

	if _, err := os.Stat(pkg.PkgObj); os.IsNotExist(err) && strings.HasPrefix(pkg.PkgObj, DefaultGOROOT) {
		// fall back to GOPATH
		firstGopathWorkspace := filepath.SplitList(build.Default.GOPATH)[0] // TODO: Need to check inside all GOPATH workspaces.
		gopathPkgObj := filepath.Join(firstGopathWorkspace, pkg.PkgObj[len(DefaultGOROOT):])
		if _, err := os.Stat(gopathPkgObj); err == nil {
			pkg.PkgObj = gopathPkgObj
		}
	}

	jsFiles, err := jsFilesFromDir(&bctx, pkg.Dir)
	if err != nil {
		return nil, err
	}

	return &PackageData{Package: pkg, JSFiles: jsFiles, IsVirtual: isVirtual}, nil
}

// excludeExecutable excludes all executable implementation .go files.
// They have "executable_" prefix.
func excludeExecutable(goFiles []string) []string {
	var s []string
	for _, f := range goFiles {
		if strings.HasPrefix(f, "executable_") {
			continue
		}
		s = append(s, f)
	}
	return s
}

// exclude returns files, excluding specified files.
func exclude(files []string, exclude ...string) []string {
	var s []string
Outer:
	for _, f := range files {
		for _, e := range exclude {
			if f == e {
				continue Outer
			}
		}
		s = append(s, f)
	}
	return s
}

func include(files []string, includes ...string) []string {
	files = exclude(files, includes...) // Ensure there won't be duplicates.
	files = append(files, includes...)
	return files
}

// ImportDir is like Import but processes the Go package found in the named
// directory.
func ImportDir(dir string, mode build.ImportMode, installSuffix string, buildTags []string) (*PackageData, error) {
	bctx := NewBuildContext(installSuffix, buildTags)
	pkg, err := bctx.ImportDir(dir, mode)
	if err != nil {
		return nil, err
	}

	jsFiles, err := jsFilesFromDir(bctx, pkg.Dir)
	if err != nil {
		return nil, err
	}

	return &PackageData{Package: pkg, JSFiles: jsFiles}, nil
}

// parseAndAugment parses and returns all .go files of given pkg.
// Standard Go library packages are augmented with files in compiler/natives folder.
// If isTest is true and pkg.ImportPath has no _test suffix, package is built for running internal tests.
// If isTest is true and pkg.ImportPath has _test suffix, package is built for running external tests.
//
// The native packages are augmented by the contents of natives.FS in the following way.
// The file names do not matter except the usual `_test` suffix. The files for
// native overrides get added to the package (even if they have the same name
// as an existing file from the standard library). For all identifiers that exist
// in the original AND the overrides, the original identifier in the AST gets
// replaced by `_`. New identifiers that don't exist in original package get added.
func parseAndAugment(bctx *build.Context, pkg *build.Package, isTest bool, fileSet *token.FileSet) ([]*ast.File, error) {
	var files []*ast.File
	replacedDeclNames := make(map[string]bool)
	funcName := func(d *ast.FuncDecl) string {
		if d.Recv == nil || len(d.Recv.List) == 0 {
			return d.Name.Name
		}
		recv := d.Recv.List[0].Type
		if star, ok := recv.(*ast.StarExpr); ok {
			recv = star.X
		}
		return recv.(*ast.Ident).Name + "." + d.Name.Name
	}
	isXTest := strings.HasSuffix(pkg.ImportPath, "_test")
	importPath := pkg.ImportPath
	if isXTest {
		importPath = importPath[:len(importPath)-5]
	}

	nativesContext := &build.Context{
		GOROOT:   "/",
		GOOS:     bctx.GOOS,
		GOARCH:   bctx.GOARCH,
		Compiler: "gc",
		JoinPath: path.Join,
		SplitPathList: func(list string) []string {
			if list == "" {
				return nil
			}
			return strings.Split(list, "/")
		},
		IsAbsPath: path.IsAbs,
		IsDir: func(name string) bool {
			dir, err := natives.FS.Open(name)
			if err != nil {
				return false
			}
			defer dir.Close()
			info, err := dir.Stat()
			if err != nil {
				return false
			}
			return info.IsDir()
		},
		HasSubdir: func(root, name string) (rel string, ok bool) {
			panic("not implemented")
		},
		ReadDir: func(name string) (fi []os.FileInfo, err error) {
			dir, err := natives.FS.Open(name)
			if err != nil {
				return nil, err
			}
			defer dir.Close()
			return dir.Readdir(0)
		},
		OpenFile: func(name string) (r io.ReadCloser, err error) {
			return natives.FS.Open(name)
		},
	}

	if importPath == "syscall" {
		// Special handling for the syscall package, which uses OS native
		// GOOS/GOARCH pair. This will no longer be necessary after
		// https://github.com/gopherjs/gopherjs/issues/693.
		nativesContext.GOARCH = build.Default.GOARCH
		nativesContext.BuildTags = append(nativesContext.BuildTags, "js")
	}

	if nativesPkg, err := nativesContext.Import(importPath, "", 0); err == nil {
		names := nativesPkg.GoFiles
		if isTest {
			names = append(names, nativesPkg.TestGoFiles...)
		}
		if isXTest {
			names = nativesPkg.XTestGoFiles
		}
		for _, name := range names {
			fullPath := path.Join(nativesPkg.Dir, name)
			r, err := nativesContext.OpenFile(fullPath)
			if err != nil {
				panic(err)
			}
			file, err := parser.ParseFile(fileSet, fullPath, r, parser.ParseComments)
			if err != nil {
				panic(err)
			}
			r.Close()
			for _, decl := range file.Decls {
				switch d := decl.(type) {
				case *ast.FuncDecl:
					replacedDeclNames[funcName(d)] = true
				case *ast.GenDecl:
					switch d.Tok {
					case token.TYPE:
						for _, spec := range d.Specs {
							replacedDeclNames[spec.(*ast.TypeSpec).Name.Name] = true
						}
					case token.VAR, token.CONST:
						for _, spec := range d.Specs {
							for _, name := range spec.(*ast.ValueSpec).Names {
								replacedDeclNames[name.Name] = true
							}
						}
					}
				}
			}
			files = append(files, file)
		}
	}
	delete(replacedDeclNames, "init")

	var errList compiler.ErrorList
	for _, name := range pkg.GoFiles {
		if !filepath.IsAbs(name) { // name might be absolute if specified directly. E.g., `gopherjs build /abs/file.go`.
			name = filepath.Join(pkg.Dir, name)
		}
		r, err := buildutil.OpenFile(bctx, name)
		if err != nil {
			return nil, err
		}
		file, err := parser.ParseFile(fileSet, name, r, parser.ParseComments)
		r.Close()
		if err != nil {
			if list, isList := err.(scanner.ErrorList); isList {
				if len(list) > 10 {
					list = append(list[:10], &scanner.Error{Pos: list[9].Pos, Msg: "too many errors"})
				}
				for _, entry := range list {
					errList = append(errList, entry)
				}
				continue
			}
			errList = append(errList, err)
			continue
		}

		switch pkg.ImportPath {
		case "crypto/rand", "encoding/gob", "encoding/json", "expvar", "go/token", "log", "math/big", "math/rand", "regexp", "time":
			for _, spec := range file.Imports {
				path, _ := strconv.Unquote(spec.Path.Value)
				if path == "sync" {
					if spec.Name == nil {
						spec.Name = ast.NewIdent("sync")
					}
					spec.Path.Value = `"github.com/gopherjs/gopherjs/nosync"`
				}
			}
		}

		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if replacedDeclNames[funcName(d)] {
					d.Name = ast.NewIdent("_")
					// Prune function bodies, since it may contain code invalid for
					// GopherJS and pin unwanted imports.
					d.Body = nil
				}
			case *ast.GenDecl:
				switch d.Tok {
				case token.TYPE:
					for _, spec := range d.Specs {
						s := spec.(*ast.TypeSpec)
						if replacedDeclNames[s.Name.Name] {
							s.Name = ast.NewIdent("_")
						}
					}
				case token.VAR, token.CONST:
					for _, spec := range d.Specs {
						s := spec.(*ast.ValueSpec)
						for i, name := range s.Names {
							if replacedDeclNames[name.Name] {
								s.Names[i] = ast.NewIdent("_")
							}
						}
					}
				}
			}
		}

		// Prune any imports that might have become unused after pruning function bodies.
		for _, impGroup := range astutil.Imports(fileSet, file) {
			for _, imp := range impGroup {
				path, _ := strconv.Unquote(imp.Path.Value)
				if !astutil.UsesImport(file, path) {
					name := ""
					if imp.Name != nil {
						name = imp.Name.Name
					}
					astutil.DeleteNamedImport(fileSet, file, name, path)
				}
			}
		}

		files = append(files, file)
	}

	if errList != nil {
		return nil, errList
	}
	return files, nil
}

type Options struct {
	GOROOT         string
	GOPATH         string
	Verbose        bool
	Quiet          bool
	Watch          bool
	CreateMapFile  bool
	MapToLocalDisk bool
	Minify         bool
	Color          bool
	BuildTags      []string
}

func (o *Options) PrintError(format string, a ...interface{}) {
	if o.Color {
		format = "\x1B[31m" + format + "\x1B[39m"
	}
	fmt.Fprintf(os.Stderr, format, a...)
}

func (o *Options) PrintSuccess(format string, a ...interface{}) {
	if o.Color {
		format = "\x1B[32m" + format + "\x1B[39m"
	}
	fmt.Fprintf(os.Stderr, format, a...)
}

type PackageData struct {
	*build.Package
	JSFiles    []string
	IsTest     bool // IsTest is true if the package is being built for running tests.
	SrcModTime time.Time
	UpToDate   bool
	IsVirtual  bool // If true, the package does not have a corresponding physical directory on disk.
}

type Session struct {
	options  *Options
	bctx     *build.Context
	Archives map[string]*compiler.Archive
	Types    map[string]*types.Package
	Watcher  *fsnotify.Watcher
}

func NewSession(options *Options) (*Session, error) {
	if options.GOROOT == "" {
		options.GOROOT = DefaultGOROOT
	}
	if options.GOPATH == "" {
		options.GOPATH = build.Default.GOPATH
	}
	options.Verbose = options.Verbose || options.Watch

	// Go distribution version check.
	if err := compiler.CheckGoVersion(options.GOROOT); err != nil {
		return nil, err
	}

	s := &Session{
		options:  options,
		Archives: make(map[string]*compiler.Archive),
	}
	s.bctx = NewBuildContext(s.InstallSuffix(), s.options.BuildTags)
	s.Types = make(map[string]*types.Package)
	if options.Watch {
		if out, err := exec.Command("ulimit", "-n").Output(); err == nil {
			if n, err := strconv.Atoi(strings.TrimSpace(string(out))); err == nil && n < 1024 {
				fmt.Printf("Warning: The maximum number of open file descriptors is very low (%d). Change it with 'ulimit -n 8192'.\n", n)
			}
		}

		var err error
		s.Watcher, err = fsnotify.NewWatcher()
		if err != nil {
			return nil, err
		}
	}
	return s, nil
}

// BuildContext returns the session's build context.
func (s *Session) BuildContext() *build.Context { return s.bctx }

func (s *Session) InstallSuffix() string {
	if s.options.Minify {
		return "min"
	}
	return ""
}

func (s *Session) BuildDir(packagePath string, importPath string, pkgObj string) error {
	if s.Watcher != nil {
		s.Watcher.Add(packagePath)
	}
	buildPkg, err := s.bctx.ImportDir(packagePath, 0)
	if err != nil {
		return err
	}
	pkg := &PackageData{Package: buildPkg}
	jsFiles, err := jsFilesFromDir(s.bctx, pkg.Dir)
	if err != nil {
		return err
	}
	pkg.JSFiles = jsFiles
	archive, err := s.BuildPackage(pkg)
	if err != nil {
		return err
	}
	if pkgObj == "" {
		pkgObj = filepath.Base(packagePath) + ".js"
	}
	if pkg.IsCommand() && !pkg.UpToDate {
		if err := s.WriteCommandPackage(archive, pkgObj); err != nil {
			return err
		}
	}
	return nil
}

func (s *Session) BuildFiles(filenames []string, pkgObj string, packagePath string) error {
	pkg := &PackageData{
		Package: &build.Package{
			Name:       "main",
			ImportPath: "main",
			Dir:        packagePath,
		},
	}

	for _, file := range filenames {
		if strings.HasSuffix(file, ".inc.js") {
			pkg.JSFiles = append(pkg.JSFiles, file)
			continue
		}
		pkg.GoFiles = append(pkg.GoFiles, file)
	}

	archive, err := s.BuildPackage(pkg)
	if err != nil {
		return err
	}
	if s.Types["main"].Name() != "main" {
		return fmt.Errorf("cannot build/run non-main package")
	}
	return s.WriteCommandPackage(archive, pkgObj)
}

func (s *Session) BuildImportPath(path string) (*compiler.Archive, error) {
	_, archive, err := s.buildImportPathWithSrcDir(path, "")
	return archive, err
}

func (s *Session) buildImportPathWithSrcDir(path string, srcDir string) (*PackageData, *compiler.Archive, error) {
	pkg, err := importWithSrcDir(*s.bctx, path, srcDir, 0, s.InstallSuffix())
	if s.Watcher != nil && pkg != nil { // add watch even on error
		s.Watcher.Add(pkg.Dir)
	}
	if err != nil {
		return nil, nil, err
	}

	archive, err := s.BuildPackage(pkg)
	if err != nil {
		return nil, nil, err
	}

	return pkg, archive, nil
}

func (s *Session) BuildPackage(pkg *PackageData) (*compiler.Archive, error) {
	if archive, ok := s.Archives[pkg.ImportPath]; ok {
		return archive, nil
	}

	if pkg.PkgObj != "" {
		var fileInfo os.FileInfo
		gopherjsBinary, err := os.Executable()
		if err == nil {
			fileInfo, err = os.Stat(gopherjsBinary)
			if err == nil {
				pkg.SrcModTime = fileInfo.ModTime()
			}
		}
		if err != nil {
			os.Stderr.WriteString("Could not get GopherJS binary's modification timestamp. Please report issue.\n")
			pkg.SrcModTime = time.Now()
		}

		for _, importedPkgPath := range pkg.Imports {
			// Ignore all imports that aren't mentioned in import specs of pkg.
			// For example, this ignores imports such as runtime/internal/sys and runtime/internal/atomic.
			ignored := true
			for _, pos := range pkg.ImportPos[importedPkgPath] {
				importFile := filepath.Base(pos.Filename)
				for _, file := range pkg.GoFiles {
					if importFile == file {
						ignored = false
						break
					}
				}
				if !ignored {
					break
				}
			}

			if importedPkgPath == "unsafe" || ignored {
				continue
			}
			importedPkg, _, err := s.buildImportPathWithSrcDir(importedPkgPath, pkg.Dir)
			if err != nil {
				return nil, err
			}
			impModTime := importedPkg.SrcModTime
			if impModTime.After(pkg.SrcModTime) {
				pkg.SrcModTime = impModTime
			}
		}

		for _, name := range append(pkg.GoFiles, pkg.JSFiles...) {
			fileInfo, err := statFile(filepath.Join(pkg.Dir, name))
			if err != nil {
				return nil, err
			}
			if fileInfo.ModTime().After(pkg.SrcModTime) {
				pkg.SrcModTime = fileInfo.ModTime()
			}
		}

		pkgObjFileInfo, err := os.Stat(pkg.PkgObj)
		if err == nil && !pkg.SrcModTime.After(pkgObjFileInfo.ModTime()) {
			// package object is up to date, load from disk if library
			pkg.UpToDate = true
			if pkg.IsCommand() {
				return nil, nil
			}

			objFile, err := os.Open(pkg.PkgObj)
			if err != nil {
				return nil, err
			}
			defer objFile.Close()

			archive, err := compiler.ReadArchive(pkg.PkgObj, pkg.ImportPath, objFile, s.Types)
			if err != nil {
				return nil, err
			}

			s.Archives[pkg.ImportPath] = archive
			return archive, err
		}
	}

	fileSet := token.NewFileSet()
	files, err := parseAndAugment(s.bctx, pkg.Package, pkg.IsTest, fileSet)
	if err != nil {
		return nil, err
	}

	localImportPathCache := make(map[string]*compiler.Archive)
	importContext := &compiler.ImportContext{
		Packages: s.Types,
		Import: func(path string) (*compiler.Archive, error) {
			if archive, ok := localImportPathCache[path]; ok {
				return archive, nil
			}
			_, archive, err := s.buildImportPathWithSrcDir(path, pkg.Dir)
			if err != nil {
				return nil, err
			}
			localImportPathCache[path] = archive
			return archive, nil
		},
	}
	archive, err := compiler.Compile(pkg.ImportPath, files, fileSet, importContext, s.options.Minify)
	if err != nil {
		return nil, err
	}

	for _, jsFile := range pkg.JSFiles {
		code, err := ioutil.ReadFile(filepath.Join(pkg.Dir, jsFile))
		if err != nil {
			return nil, err
		}
		archive.IncJSCode = append(archive.IncJSCode, []byte("\t(function() {\n")...)
		archive.IncJSCode = append(archive.IncJSCode, code...)
		archive.IncJSCode = append(archive.IncJSCode, []byte("\n\t}).call($global);\n")...)
	}

	if s.options.Verbose {
		fmt.Println(pkg.ImportPath)
	}

	s.Archives[pkg.ImportPath] = archive

	if pkg.PkgObj == "" || pkg.IsCommand() {
		return archive, nil
	}

	if err := s.writeLibraryPackage(archive, pkg.PkgObj); err != nil {
		if strings.HasPrefix(pkg.PkgObj, s.options.GOROOT) {
			// fall back to first GOPATH workspace
			firstGopathWorkspace := filepath.SplitList(s.options.GOPATH)[0]
			if err := s.writeLibraryPackage(archive, filepath.Join(firstGopathWorkspace, pkg.PkgObj[len(s.options.GOROOT):])); err != nil {
				return nil, err
			}
			return archive, nil
		}
		return nil, err
	}

	return archive, nil
}

func (s *Session) writeLibraryPackage(archive *compiler.Archive, pkgObj string) error {
	if err := os.MkdirAll(filepath.Dir(pkgObj), 0777); err != nil {
		return err
	}

	objFile, err := os.Create(pkgObj)
	if err != nil {
		return err
	}
	defer objFile.Close()

	return compiler.WriteArchive(archive, objFile)
}

func (s *Session) WriteCommandPackage(archive *compiler.Archive, pkgObj string) error {
	if err := os.MkdirAll(filepath.Dir(pkgObj), 0777); err != nil {
		return err
	}
	codeFile, err := os.Create(pkgObj)
	if err != nil {
		return err
	}
	defer codeFile.Close()

	sourceMapFilter := &compiler.SourceMapFilter{Writer: codeFile}
	if s.options.CreateMapFile {
		m := &sourcemap.Map{File: filepath.Base(pkgObj)}
		mapFile, err := os.Create(pkgObj + ".map")
		if err != nil {
			return err
		}

		defer func() {
			m.WriteTo(mapFile)
			mapFile.Close()
			fmt.Fprintf(codeFile, "//# sourceMappingURL=%s.map\n", filepath.Base(pkgObj))
		}()

		sourceMapFilter.MappingCallback = NewMappingCallback(m, s.options.GOROOT, s.options.GOPATH, s.options.MapToLocalDisk)
	}

	deps, err := compiler.ImportDependencies(archive, func(path string) (*compiler.Archive, error) {
		if archive, ok := s.Archives[path]; ok {
			return archive, nil
		}
		_, archive, err := s.buildImportPathWithSrcDir(path, "")
		return archive, err
	})
	if err != nil {
		return err
	}
	return compiler.WriteProgramCode(deps, sourceMapFilter)
}

func NewMappingCallback(m *sourcemap.Map, goroot, gopath string, localMap bool) func(generatedLine, generatedColumn int, originalPos token.Position) {
	return func(generatedLine, generatedColumn int, originalPos token.Position) {
		if !originalPos.IsValid() {
			m.AddMapping(&sourcemap.Mapping{GeneratedLine: generatedLine, GeneratedColumn: generatedColumn})
			return
		}

		file := originalPos.Filename

		switch hasGopathPrefix, prefixLen := hasGopathPrefix(file, gopath); {
		case localMap:
			// no-op:  keep file as-is
		case hasGopathPrefix:
			file = filepath.ToSlash(file[prefixLen+4:])
		case strings.HasPrefix(file, goroot):
			file = filepath.ToSlash(file[len(goroot)+4:])
		default:
			file = filepath.Base(file)
		}

		m.AddMapping(&sourcemap.Mapping{GeneratedLine: generatedLine, GeneratedColumn: generatedColumn, OriginalFile: file, OriginalLine: originalPos.Line, OriginalColumn: originalPos.Column})
	}
}

func jsFilesFromDir(bctx *build.Context, dir string) ([]string, error) {
	files, err := buildutil.ReadDir(bctx, dir)
	if err != nil {
		return nil, err
	}
	var jsFiles []string
	for _, file := range files {
		if strings.HasSuffix(file.Name(), ".inc.js") && file.Name()[0] != '_' && file.Name()[0] != '.' {
			jsFiles = append(jsFiles, file.Name())
		}
	}
	return jsFiles, nil
}

// hasGopathPrefix returns true and the length of the matched GOPATH workspace,
// iff file has a prefix that matches one of the GOPATH workspaces.
func hasGopathPrefix(file, gopath string) (hasGopathPrefix bool, prefixLen int) {
	gopathWorkspaces := filepath.SplitList(gopath)
	for _, gopathWorkspace := range gopathWorkspaces {
		gopathWorkspace = filepath.Clean(gopathWorkspace)
		if strings.HasPrefix(file, gopathWorkspace) {
			return true, len(gopathWorkspace)
		}
	}
	return false, 0
}

func (s *Session) WaitForChange() {
	s.options.PrintSuccess("watching for changes...\n")
	for {
		select {
		case ev := <-s.Watcher.Events:
			if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) == 0 || filepath.Base(ev.Name)[0] == '.' {
				continue
			}
			if !strings.HasSuffix(ev.Name, ".go") && !strings.HasSuffix(ev.Name, ".inc.js") {
				continue
			}
			s.options.PrintSuccess("change detected: %s\n", ev.Name)
		case err := <-s.Watcher.Errors:
			s.options.PrintError("watcher error: %s\n", err.Error())
		}
		break
	}

	go func() {
		for range s.Watcher.Events {
			// consume, else Close() may deadlock
		}
	}()
	s.Watcher.Close()
}
