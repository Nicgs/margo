package kimporter

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"go/types"
	"golang.org/x/crypto/blake2b"
	"golang.org/x/net/context"
	"golang.org/x/sync/errgroup"
	"golang.org/x/tools/go/gcexportdata"
	"margo.sh/golang/gopkg"
	"margo.sh/golang/goutil"
	"margo.sh/memo"
	"margo.sh/mg"
	"margo.sh/mgutil"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

var (
	pkgC = func() *Package {
		p := types.NewPackage("C", "C")
		p.MarkComplete()
		return NewPackage(p, nil, nil, nil, nil)
	}()
	pkgUnsafe = func() *Package {
		return NewPackage(types.Unsafe, nil, nil, nil, nil)
	}()
	builtinPkgs = struct {
		builtin *builtinPkg
		unsafe  *builtinPkg
	}{
		builtin: &builtinPkg{
			name:  "builtin",
			ipath: "builtin",
			grDir: "builtin",
			scope: types.Universe,
		},
		unsafe: &builtinPkg{
			name:  "unsafe",
			ipath: "unsafe",
			grDir: "unsafe",
			scope: types.Unsafe.Scope(),
		},
	}
)

// TypesInfo specifies what, if any, types.Info to load
type TypesInfo uint

const (
	// TypesInfoTypes loads types.Info.Types
	TypesInfoTypes TypesInfo = 1 << iota
	// TypesInfoDefs loads types.Info.Defs
	TypesInfoDefs
	// TypesInfoUses loads types.Info.Uses
	TypesInfoUses
	// TypesInfoImplicits loads types.Info.Implicits
	TypesInfoImplicits
	// TypesInfoSelections loads types.Info.Selections
	TypesInfoSelections
	// TypesInfoScopes loads types.Info.Scopes
	TypesInfoScopes
	// TypesInfoInitOrder loads types.Info.InitOrder
	TypesInfoInitOrder
	// TypesInfoAll loads all types.Info fields
	TypesInfoAll = TypesInfoTypes |
		TypesInfoDefs |
		TypesInfoUses |
		TypesInfoImplicits |
		TypesInfoSelections |
		TypesInfoScopes |
		TypesInfoInitOrder
)

func (ti TypesInfo) New() *types.Info {
	m := &types.Info{}
	if ti&TypesInfoTypes != 0 {
		m.Types = map[ast.Expr]types.TypeAndValue{}
	}
	if ti&TypesInfoDefs != 0 {
		m.Defs = map[*ast.Ident]types.Object{}
	}
	if ti&TypesInfoUses != 0 {
		m.Uses = map[*ast.Ident]types.Object{}
	}
	if ti&TypesInfoImplicits != 0 {
		m.Implicits = map[ast.Node]types.Object{}
	}
	if ti&TypesInfoSelections != 0 {
		m.Selections = map[*ast.SelectorExpr]*types.Selection{}
	}
	if ti&TypesInfoScopes != 0 {
		m.Scopes = map[ast.Node]*types.Scope{}
	}
	if ti&TypesInfoInitOrder != 0 {
		m.InitOrder = []*types.Initializer{}
	}
	return m
}

// Package holds type and package info for a type-checked package.
// NOTE: All fields except the underlying types.Package are optional.
type Package struct {
	*types.Package

	// Fset is the FileSet used for parsing
	Fset *token.FileSet

	// Info holds type info about the package
	Info *types.Info

	// Package holds type and package info for type-checked imports
	Imports map[string]*Package

	// Files maps the package file tbasenames to their parsed ast files
	Files map[string]*ast.File
}

// NewPackage is equivalent to &Package{Package: pkg, Fset: fset, Types: info, Imports: imports}
// All arguments except pkg are optional.
// NewPackage panics if pkg is nil.
func NewPackage(pkg *types.Package, fset *token.FileSet, files map[string]*ast.File, info *types.Info, imports map[string]*Package) *Package {
	if pkg == nil {
		panic("NewPackage: pkg==nil")
	}
	return &Package{
		Package: pkg,
		Fset:    fset,
		Info:    info,
		Files:   files,
		Imports: imports,
	}
}

type stateKey struct {
	ImportPath   string
	Dir          string
	CheckFuncs   bool
	CheckImports bool
	Tests        bool
	Tags         string
	GOARCH       string
	GOOS         string
	GOROOT       string
	GOPATH       string
	NoHash       bool
	TypesInfo    TypesInfo
}

func globalState(mx *mg.Ctx, k stateKey) *state {
	type K struct{ stateKey }
	return mx.VFS.ReadMemo(k.Dir, K{k}, func() memo.V {
		return &state{stateKey: k}
	}).(*state)
}

type state struct {
	stateKey
	chkAt mgutil.AtomicInt
	invAt mgutil.AtomicInt
	imby  struct {
		sync.Mutex
		l []*state
	}
	mu   sync.Mutex
	err  error
	pkg  *Package
	hash string
}

func (ks *state) invalidate(invAt int64) {
	ks.invAt.Set(invAt)
	ks.imby.Lock()
	l := ks.imby.l
	ks.imby.Unlock()
	for _, p := range l {
		p.invalidate(invAt)
	}
}

func (ks *state) InvalidateMemo(invAt int64) {
	ks.invalidate(invAt)
}

func (ks *stateKey) targets(pp *gopkg.PkgPath) bool {
	return ks.ImportPath == pp.ImportPath || ks.Dir == pp.Dir
}

func (ks *state) importedBy(p *state) {
	ks.imby.Lock()
	defer ks.imby.Unlock()

	for _, q := range ks.imby.l {
		if p == q {
			return
		}
	}
	ks.imby.l = append(ks.imby.l[:len(ks.imby.l):len(ks.imby.l)], p)
}

func (ks *state) valid(hash string) bool {
	return ks.chkAt.N() > ks.invAt.N() && (ks.NoHash || ks.hash == hash)
}

func (ks *state) result() (*Package, error) {
	switch {
	case ks.err != nil:
		return nil, ks.err
	case !ks.pkg.Complete():
		// Package exists but is not complete - we cannot handle this
		// at the moment since the source importer replaces the package
		// wholesale rather than augmenting it (see #19337 for details).
		// Return incomplete package with error (see #16088).
		return ks.pkg, fmt.Errorf("reimported partially imported package %q", ks.ImportPath)
	default:
		return ks.pkg, nil
	}
}

type Config struct {
	PackageSrc map[string][]byte

	SrcMap        map[string][]byte
	CheckFuncs    bool
	CheckImports  bool
	NoConcurrency bool
	Tests         bool

	// TypesInfo specifies what, if any, package info to load
	TypesInfo TypesInfo

	// ImportsTypesInfo speifies whether or not to also load type info for imported packages
	ImportsTypesInfo bool
}

type Importer struct {
	cfg  Config
	mx   *mg.Ctx
	bld  *build.Context
	ks   *state
	mp   *gopkg.ModPath
	par  *Importer
	tags string
	hash string
}

func (kp *Importer) Import(path string) (*types.Package, error) {
	return kp.ImportFrom(path, ".", 0)
}

func (kp *Importer) ImportFrom(ipath, srcDir string, mode types.ImportMode) (*types.Package, error) {
	// TODO: add support for unsaved-files without a package
	if mode != 0 {
		panic("non-zero import mode")
	}
	p, err := kp.ImportPackage(ipath, srcDir)
	if err != nil {
		return nil, err
	}
	return p.Package, nil
}

// ImportPackage import package with import path ipath relative to srcDir
// NOTE: All Package fields except the underlying types.Package are optional.
func (kp *Importer) ImportPackage(ipath, srcDir string) (*Package, error) {
	if pkg := kp.importFakePkg(ipath); pkg != nil {
		return pkg, nil
	}
	if p, err := filepath.Abs(srcDir); err == nil {
		srcDir = p
	}
	if !filepath.IsAbs(srcDir) {
		return nil, fmt.Errorf("srcDir is not absolute: %s", srcDir)
	}
	pp, err := kp.findPkg(ipath, srcDir)
	if err != nil {
		return nil, err
	}
	return kp.importPkg(pp)
}

func (kp *Importer) findPkg(ipath, srcDir string) (*gopkg.PkgPath, error) {
	kp.mx.Profile.Push(`Kim-Porter: findPkg(` + ipath + `)`).Pop()
	pp, err := kp.mp.FindPkg(kp.mx, ipath, srcDir)
	return pp, err
}

func (kp *Importer) stateKey(pp *gopkg.PkgPath) stateKey {
	cfg := kp.cfg
	return stateKey{
		ImportPath:   pp.ImportPath,
		Dir:          pp.Dir,
		CheckFuncs:   cfg.CheckFuncs,
		CheckImports: cfg.CheckImports,
		Tests:        cfg.Tests,
		Tags:         kp.tags,
		GOOS:         kp.bld.GOOS,
		GOARCH:       kp.bld.GOARCH,
		GOROOT:       kp.bld.GOROOT,
		GOPATH:       strings.Join(mgutil.PathList(kp.bld.GOPATH), string(filepath.ListSeparator)),
		NoHash:       kp.hash == "",
		TypesInfo:    cfg.TypesInfo,
	}
}

func (kp *Importer) state(pp *gopkg.PkgPath) *state {
	return globalState(kp.mx, kp.stateKey(pp))
}

func (kp *Importer) detectCycle(pp *gopkg.PkgPath) error {
	defer kp.mx.Profile.Start(`Kim-Porter: detectCycle()`).Stop()

	for p := kp; p != nil; p = p.par {
		if p.ks == nil || !p.ks.targets(pp) {
			continue
		}
		l := []string{pp.ImportPath + "(" + pp.Dir + ")"}
		for p := kp; ; p = p.par {
			if p.ks == nil {
				continue
			}
			l = append(l, p.ks.ImportPath+"("+p.ks.Dir+")")
			if p.ks.targets(pp) {
				return fmt.Errorf("import cycle: %s", strings.Join(l, " <~ "))
			}
		}
	}
	return nil
}

func (kp *Importer) importPkg(pp *gopkg.PkgPath) (pkg *Package, err error) {
	title := `Kim-Porter: import(` + pp.ImportPath + `)`
	defer kp.mx.Profile.Push(title).Pop()
	defer kp.mx.Begin(mg.Task{Title: title}).Done()

	if err := kp.detectCycle(pp); err != nil {
		return nil, err
	}
	// TODO: maybe lookup the state w/o TypesInfo.
	// everything should be the same except one has types.Info
	ks := kp.state(pp)
	kx := kp.branch(ks, pp)
	ks.mu.Lock()
	defer ks.mu.Unlock()

	if ks.valid(kp.hash) {
		return ks.result()
	}
	chkAt := memo.InvAt()
	ks.pkg, ks.err = kx.check(ks, pp, kp.cfg.PackageSrc)
	ks.hash = kp.hash
	ks.chkAt.Set(chkAt)
	return ks.result()
}

func (kp *Importer) check(ks *state, pp *gopkg.PkgPath, pkgSrc map[string][]byte) (*Package, error) {
	fset := token.NewFileSet()
	bp, filesMap, filesList, err := parseDir(kp.mx, kp.bld, fset, pp, kp.cfg.SrcMap, ks, pkgSrc)
	if err != nil {
		return nil, err
	}

	imports, err := kp.importDeps(ks, bp, fset, filesList)
	if err != nil {
		return nil, err
	}

	if len(bp.CgoFiles) != 0 {
		// TODO: fill in the type info. maybe we can just merge this into the pure-go check.
		pkg, err := kp.importCgoPkg(pp, imports)
		if err == nil {
			return NewPackage(pkg, fset, filesMap, nil, imports), err
		}
	}

	defer kp.mx.Profile.Push(`Kim-Porter: typecheck(` + ks.ImportPath + `)`).Pop()
	var hardErr error
	tc := types.Config{
		FakeImportC:              true,
		IgnoreFuncBodies:         !ks.CheckFuncs,
		DisableUnusedImportCheck: !ks.CheckImports,
		Error: func(err error) {
			if te, ok := err.(types.Error); ok && !te.Soft && hardErr == nil {
				hardErr = err
			}
		},
		Importer: kp,
		Sizes:    types.SizesFor(kp.bld.Compiler, kp.bld.GOARCH),
	}
	var inf *types.Info
	if ks.TypesInfo != 0 {
		inf = ks.TypesInfo.New()
	}
	pkg, err := tc.Check(bp.ImportPath, fset, filesList, inf)
	if err == nil && hardErr != nil {
		err = hardErr
	}
	switch {
	case pkg == nil:
		return nil, err
	case ks.TypesInfo != 0:
		return NewPackage(pkg, fset, filesMap, inf, imports), err
	default:
		return NewPackage(pkg, fset, filesMap, nil, imports), err
	}
}

func (kp *Importer) importCgoPkg(pp *gopkg.PkgPath, imports map[string]*Package) (*types.Package, error) {
	name := `go`
	args := []string{`list`, `-e`, `-export`, `-f={{.Export}}`, pp.Dir}
	ctx, cancel := context.WithCancel(context.Background())
	title := `Kim-Porter: importCgoPkg` + mgutil.QuoteCmd(name, args...) + `)`
	defer kp.mx.Profile.Push(title).Pop()
	defer kp.mx.Begin(mg.Task{Title: title, Cancel: cancel}).Done()

	buf := &bytes.Buffer{}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = pp.Dir
	cmd.Stdout = buf
	cmd.Env = kp.mx.Env.Environ()
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s: %s", title, err)
	}
	fn := string(bytes.TrimSpace(buf.Bytes()))
	f, err := os.Open(fn)
	if err != nil {
		return nil, fmt.Errorf("cannot open %s.a: %s", pp.ImportPath, err)
	}
	defer f.Close()
	rd, err := gcexportdata.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("cannot create export data reader for %s from %s: %s", pp.ImportPath, fn, err)
	}
	m := make(map[string]*types.Package, len(imports))
	for k, v := range imports {
		m[k] = v.Package
	}
	pkg, err := gcexportdata.Read(rd, token.NewFileSet(), m, pp.ImportPath)
	if err != nil {
		return nil, fmt.Errorf("cannot read export data for %s from %s: %s", pp.ImportPath, fn, err)
	}
	return pkg, nil
}

func (kp *Importer) importFakePkg(ipath string) *Package {
	switch ipath {
	case "unsafe":
		return pkgUnsafe
	case "C":
		return pkgC
	}
	return nil
}

func (kp *Importer) importDeps(ks *state, bp *build.Package, fset *token.FileSet, astFiles []*ast.File) (map[string]*Package, error) {
	defer kp.mx.Profile.Push(`Kim-Porter: importDeps(` + ks.ImportPath + `)`).Pop()

	paths := mgutil.StrSet(bp.Imports)
	if ks.Tests {
		paths = paths.Add(bp.TestImports...)
	}
	mu := sync.Mutex{}
	imports := make(map[string]*Package, len(paths))
	doImport := func(ipath string) error {
		pkg, err := kp.ImportPackage(ipath, bp.Dir)
		if err == nil {
			mu.Lock()
			imports[ipath] = pkg
			mu.Unlock()
			return nil
		}
		for _, af := range astFiles {
			for _, spec := range af.Imports {
				if spec.Path == nil {
					continue
				}
				s, _ := strconv.Unquote(spec.Path.Value)
				if ipath != s {
					continue
				}
				tp := fset.Position(spec.Pos())
				return mg.Issue{
					Path:    tp.Filename,
					Row:     tp.Line - 1,
					Col:     tp.Column - 1,
					Message: err.Error(),
				}
			}
		}
		return err
	}
	if kp.cfg.NoConcurrency || len(paths) < 2 {
		for _, ipath := range paths {
			if err := doImport(ipath); err != nil {
				return imports, err
			}
		}
		return imports, nil
	}
	imps := make(chan string, len(paths))
	for _, ipath := range paths {
		imps <- ipath
	}
	close(imps)
	errg := &errgroup.Group{}
	for i := 0; i < mgutil.MinNumCPU(len(paths)); i++ {
		errg.Go(func() error {
			for ipath := range imps {
				if err := doImport(ipath); err != nil {
					return err
				}
			}
			return nil
		})
	}
	return imports, errg.Wait()
}

func (kp *Importer) setupJs(pp *gopkg.PkgPath) {
	fs := kp.mx.VFS
	nd := fs.Poke(kp.bld.GOROOT).Poke("src/syscall/js")
	if fs.Poke(pp.Dir) != nd && fs.Poke(kp.mx.View.Dir()) != nd {
		return
	}
	bld := *kp.bld
	bld.GOOS = "js"
	bld.GOARCH = "wasm"
	kp.bld = &bld
}

func (kp *Importer) branch(ks *state, pp *gopkg.PkgPath) *Importer {
	kx := *kp
	if pp.Mod != nil {
		kx.mp = pp.Mod
	}
	if kp.ks != nil {
		// TODO: we need clear this if it's no longer true
		ks.importedBy(kp.ks)
	}
	if !kp.cfg.ImportsTypesInfo {
		kx.cfg.TypesInfo = 0
	}
	// user settings don't apply when checking deps
	kx.cfg.PackageSrc = nil
	kx.cfg.CheckFuncs = false
	kx.cfg.CheckImports = false
	kx.cfg.Tests = false
	kx.hash = ""
	kx.ks = ks
	kx.par = kp
	kx.setupJs(pp)
	return &kx
}

func New(mx *mg.Ctx, cfg *Config) *Importer {
	bld := goutil.BuildContext(mx)
	bld.BuildTags = append(bld.BuildTags, "netgo", "osusergo")
	kp := &Importer{
		mx:   mx,
		bld:  bld,
		tags: tagsStr(bld.BuildTags),
	}
	if cfg != nil {
		kp.cfg = *cfg
		kp.hash = srcMapHash(cfg.SrcMap)
	}
	return kp
}

func srcMapHash(m map[string][]byte) string {
	if len(m) == 0 {
		return ""
	}
	fns := make(sort.StringSlice, len(m))
	for fn, _ := range m {
		fns = append(fns, fn)
	}
	fns.Sort()
	b2, _ := blake2b.New256(nil)
	for _, fn := range fns {
		b2.Write([]byte(fn))
		b2.Write(m[fn])
	}
	return hex.EncodeToString(b2.Sum(nil))
}

func tagsStr(l []string) string {
	switch len(l) {
	case 0:
		return ""
	case 1:
		return l[0]
	}
	s := append(sort.StringSlice{}, l...)
	s.Sort()
	return strings.Join(s, " ")
}

type Builtin struct {
	types.Object
	pos token.Pos
}

func (b *Builtin) Pos() token.Pos {
	return b.pos
}

type builtinPkg struct {
	name  string
	ipath string
	grDir string
	scope *types.Scope

	mu  sync.Mutex
	pkg *Package
}

func (p *builtinPkg) load() *Package {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.pkg != nil {
		return p.pkg
	}
	pkg := types.NewPackage(p.ipath, p.name)
	psc := pkg.Scope()
	insert := func(nm string, o *ast.Object) {
		if nm == "" || o == nil {
			return
		}
		obj := p.scope.Lookup(nm)
		if obj == nil {
			return
		}
		psc.Insert(&Builtin{Object: obj, pos: o.Pos()})
	}
	dir := filepath.Join(build.Default.GOROOT, "src", p.grDir)
	fset := token.NewFileSet()
	m, _ := parser.ParseDir(fset, dir, nil, parser.ParseComments)
	files := map[string]*ast.File{}
	for _, p := range m {
		for fn, f := range p.Files {
			files[fn] = f
			if f.Scope == nil {
				continue
			}
			for k, v := range f.Scope.Objects {
				insert(k, v)
			}
		}
	}
	p.pkg = NewPackage(pkg, fset, files, nil, nil)
	return p.pkg
}

func PkgBuiltin() *Package {
	return builtinPkgs.builtin.load()
}

func PkgUnsafe() *Package {
	return builtinPkgs.unsafe.load()
}
