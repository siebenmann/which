package which

import (
	"debug/gosym"
	"errors"
	"go/build"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// TODO(rjeczalik): support all platform types

func init() {
	// Add $GOROOT and $GOROOT_FINAL to the filtered paths.
	goroot := runtime.GOROOT()
	filtered[goroot] = struct{}{}
	if err := os.Setenv("GOROOT", ""); err == nil {
		filtered[runtime.GOROOT()] = struct{}{} // $GOROOT_FINAL
		os.Setenv("GOROOT", goroot)
	}
	// Make the order of file factory methods platform-specific.
	switch runtime.GOOS {
	case "darwin":
		alltbl = append(alltbl, newmacho, newelf, newpe)
	case "windows":
		alltbl = append(alltbl, newpe, newmacho, newelf)
	default:
		alltbl = append(alltbl, newelf, newmacho, newpe)
	}
}

type tabler interface {
	Close() error
	Pcln() ([]byte, error)
	Sym() ([]byte, error)
	Text() (uint64, error)
	Type() *PlatformType
}

// All supported symbol table builders.
var alltbl []func(string) (tabler, error)

// A path is discarded if it contains any of the filtered strings.
// TODO(rjeczalik): add $HOME/.gvm/gos?
var filtered = map[string]struct{}{
	filepath.FromSlash("/tmp/makerelease"):  {},
	filepath.FromSlash("<autogenerated>"):   {},
	filepath.FromSlash("c:/go/src"):         {},
	filepath.FromSlash("/usr/local/go/src"): {},
}

var (
	// ErrNotGoExec is an error.
	ErrNotGoExec = errors.New("which: not a Go executable")
	// ErrGuessFail is an error.
	ErrGuessFail = errors.New("which: unable to guess an import path of the main package")
)

// PlatformType represents the target platform of the executable.
type PlatformType struct {
	GOOS   string // target operating system
	GOARCH string // target architecture
}

// String gives Go platform string.
func (typ PlatformType) String() string {
	return typ.GOOS + "_" + typ.GOARCH
}

var (
	// PlatformDarwin386 represents the darwin_386 target arch.
	PlatformDarwin386 = &PlatformType{"darwin", "386"}
	// PlatformDarwinAMD64 represents the darwin_amd64 target arch.
	PlatformDarwinAMD64 = &PlatformType{"darwin", "amd64"}
	// PlatformFreeBSD386 represents the freebsd_386 target arch.
	PlatformFreeBSD386 = &PlatformType{"freebsd", "386"}
	// PlatformFreeBSDAMD64 represents the freebsd_amd64 target arch.
	PlatformFreeBSDAMD64 = &PlatformType{"freebsd", "amd64"}
	// PlatformLinux386 represents the linux_386 target arch.
	PlatformLinux386 = &PlatformType{"linux", "386"}
	// PlatformLinuxAMD64 represents the linux_amd64 target arch.
	PlatformLinuxAMD64 = &PlatformType{"linux", "amd64"}
	// PlatformWindows386 represents the windows_386 target arch.
	PlatformWindows386 = &PlatformType{"windows", "386"}
	// PlatformWindowsAMD64 represents the windows_amd64 target arch.
	PlatformWindowsAMD64 = &PlatformType{"windows", "amd64"}
)

// Exec represents a single Go executable file.
type Exec struct {
	Path  string        // Path to the executable.
	Type  *PlatformType // Fileutable file format.
	table *gosym.Table
}

// NewExec tries to detect executable type for the given path and returns
// a new executable. It fails if file does not exist, is not a Go executable or
// it's unable to parse the file format.
func NewExec(path string) (*Exec, error) {
	typ, symtab, pclntab, text, err := newtbl(path)
	if err != nil {
		return nil, err
	}
	lntab := gosym.NewLineTable(pclntab, text)
	if lntab == nil {
		return nil, ErrNotGoExec
	}
	tab, err := gosym.NewTable(symtab, lntab)
	if err != nil {
		return nil, ErrNotGoExec
	}
	return &Exec{Path: path, Type: typ, table: tab}, nil
}

// Import gives the import path of main package of given executable. It returns
// non-nil error when it fails to guess the exact path.
//
// TODO(cks): support Go modules somehow, since they may not be built
// in local directory trees that we can understand to extract a
// package name from.
//
// rsc.io/goversion/version can extract module information from binaries
// that contain it, and runtime/debug.ReadBuildInfo extracts it from
// the current program, but there doesn't seem to be an official
// interface for getting it from files. The module info is also
// apparently stored purely as a string, and would have to be parsed
// into the runtime/debug.BuildInfo form. Summary: it would be
// fragile.
func (ex *Exec) Import() (string, error) {
	var dirs = make(map[string]struct{})
	name := filepath.Base(ex.Path)
	if ex.Type == PlatformWindows386 || ex.Type == PlatformWindowsAMD64 {
		name = strings.TrimSuffix(name, ".exe")
	}

	// All executables should have a main.main function, which
	// should have line number information, which should reliably
	// give us the file that main.main comes from.
	mainfn := ex.table.LookupFunc("main.main")
	if mainfn == nil {
		// If there is no main.main our heuristic code will fail
		// too, so we give up now.
		return "", ErrGuessFail
	}
	if file, _, fn := ex.table.PCToLine(mainfn.Entry); fn != nil {
		return genpkgpath(name, filepath.Dir(file))
	}

	// If obtaining the line number information fails for some
	// reason we fall back to the original heuristics.
	for file, obj := range ex.table.Files {
		for i := range obj.Funcs {
			// main.main symbol is referenced by every file of each package
			// imported by the main package of the executable.
			if obj.Funcs[i].Sym.Name == "main.main" && !isfiltered(file) {
				dirs[filepath.Dir(file)] = struct{}{}
			}
		}
	}
	if pkg, unique := guesspkg(name, dirs); unique && pkg != "" {
		return pkg, nil
	}
	return "", ErrGuessFail
}

// Import reads the import path of main package of the Go executable given by
// the path.
func Import(path string) (string, error) {
	ex, err := NewExec(path)
	if err != nil {
		return "", err
	}
	return ex.Import()
}

func newtbl(path string) (typ *PlatformType, symtab, pclntab []byte, text uint64, err error) {
	var tbl tabler
	fail := func() {
		err = errors.New("which: unable to read Go symbol table: " + err.Error())
		tbl.Close()
	}
	for _, newt := range alltbl {
		if tbl, err = newt(path); err != nil {
			err = ErrNotGoExec
			continue
		}
		if symtab, err = tbl.Sym(); err != nil {
			fail()
			continue
		}
		if pclntab, err = tbl.Pcln(); err != nil {
			fail()
			continue
		}
		if text, err = tbl.Text(); err != nil {
			fail()
			continue
		}
		typ = tbl.Type()
		tbl.Close()
		break
	}
	return
}

func isfiltered(file string) bool {
	for f := range filtered {
		if strings.Contains(file, f) {
			return true
		}
	}
	return false
}

var src = filepath.FromSlash("/src/")

func guesspkg(name string, dirs map[string]struct{}) (pkg string, unique bool) {
	defer func() {
		pkg = filepath.ToSlash(pkg)
	}()
	for _, s := range []string{"cmd" + string(os.PathSeparator) + name, name} {
		for dir := range dirs {
			if strings.Contains(dir, s) {
				if i := strings.LastIndex(dir, src); i != -1 {
					pkg = dir[i+len(src):]
					if unique {
						unique = false
						return
					}
					unique = true
				}
			}
		}
		if unique {
			return
		}
	}
	return
}

// getgopath gets a slice of directories that are GOPATH.
// $GOPATH can be a colon-separated list of paths, so we must cope.
// This requires Go 1.8+ for go/build's Default.GOPATH.
func getgopath() []string {
	gopath := os.Getenv("GOPATH")
	if gopath == "" {
		gopath = build.Default.GOPATH
	}
	return strings.Split(gopath, ":")
}

// pathtomodule transforms directory paths from underneath
// $GOPATH/pkg/mod/ (with that prefix removed) into package names
// including the module version (the package name may also be the
// module name, but not necessarily; it may be a sub-package within
// the module, such as '.../cmd/program'). There are two cases. A
// program built directly in the module top level has a directory name
// like:
//	a.b/x/fred@v....
// This can be used straight.
// A program built in a subdirectory has a directory name like:
//	a.b/x/fred@v.../cmd/program
//
// We must move the '@...' part to the end.
//
// Including the module version in the result is arguable but useful.
// It can be handed to 'go get' to reproduce the binary and it is part
// of the module specification for how the binary was built. It's also
// the only way to make it clear that this was a pure module build,
// one made without a local source tree.
func pathtomodule(dir string) string {
	ati := strings.LastIndexByte(dir, '@')
	// This shouldn't happen but whatever.
	if ati == -1 {
		return dir
	}
	versuf := dir[ati:]
	tdir := strings.IndexRune(versuf, os.PathSeparator)
	if tdir == -1 {
		return dir
	}
	// We can't .Join() all three components because there can't
	// be a '/' between the last portion of the directory name and
	// the version, ie it's not '.../cmd/program/@v...'.
	return filepath.Join(dir[:ati], versuf[tdir:]) + versuf[:tdir]
}

var mod = filepath.FromSlash("/pkg/mod/")

// genpkgpath generates the package path from a raw directory,
// attempting to determine if it's under either GOROOT or GOPATH.
// If the path does not appear to be under either, we return it
// untouched as the best we can do. To deal with various issues,
// we attempt to canonicalize all symlinks, because built programs
// generally embed the non-symlink path even if $GOPATH or $HOME
// involves a symlink.
func genpkgpath(name, dir string) (string, error) {
	var checkpaths []string
	checkpaths = []string{runtime.GOROOT()}
	checkpaths = append(checkpaths, getgopath()...)

	if nd, err := filepath.EvalSymlinks(dir); err == nil {
		dir = nd
	}
	for _, path := range checkpaths {
		if abs, err := filepath.EvalSymlinks(path); err == nil {
			path = abs
		}
		pth := path + string(os.PathSeparator)
		if !strings.HasPrefix(dir, pth) {
			continue
		}
		// First, look for a straightforward build, which has
		// a directory name under $GOPATH/src/. This may be a
		// non-module build or a module build; we can't
		// actually tell from the directory path
		// alone. However in either case it's been built
		// directly from the source code there.
		spth := pth + "src" + string(os.PathSeparator)
		if strings.HasPrefix(dir, spth) {
			return dir[len(spth):], nil
		}
		// Second, look for a module build that was done
		// directly through 'go get <something>@<version>',
		// which has a directory name under $GOPATH/pkg/mod/.
		// In this case we report the package name with the
		// module version.
		mpth := filepath.Join(pth, "pkg", "mod") + string(os.PathSeparator)
		if strings.HasPrefix(dir, mpth) {
			return pathtomodule(dir[len(mpth):]), nil
		}
	}

	// If we failed to find anything, we fall back to guesspkg
	// in the hopes that it works. This is okay because what we
	// care about here is the *package name*, not the $GOPATH
	// location.
	pkg, unique := guesspkg(name, map[string]struct{}{
		dir: struct{}{},
	})
	if unique {
		return pkg, nil
	}

	// Special hack for a direct module build using a different
	// $GOPATH than our current one, which we detect by looking
	// for '/pkg/mod/' in the directory path.
	if i := strings.LastIndex(dir, mod); i != -1 {
		return pathtomodule(dir[i+len(mod):]), nil
	}

	// At this point we can't determine the package name (what we
	// have is only a directory name), so we must return an error.
	// This could happen if, for example, the binary is the result
	// of building a module-based package in a directory.
	return "", ErrGuessFail
}
