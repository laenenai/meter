// Command apilist regenerates api.txt, the module's public API as a golden
// file. CHARTER.md makes that file one of the two mechanisms that keep the
// charter from eroding: growing the public surface shows up as a reviewed
// diff rather than as a quiet addition, so the file has to be generated the
// same way by every developer and by CI.
//
// It lists every exported declaration of every importable package in the
// module — internal packages and main packages are excluded, since neither
// is part of what a consumer can name — with declarations sorted so the
// output depends on the API and not on which file a declaration happens to
// live in. Doc comments are deliberately left out: this is a surface
// inventory, and including prose would make every wording change look like
// an API change.
//
// Run it with `mise run api`; `mise run api-check` fails if the committed
// file is stale.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// outputFile is written relative to the module root, which is this
// command's working directory when run through mise.
const outputFile = "api.txt"

func main() {
	if err := run("."); err != nil {
		fmt.Fprintln(os.Stderr, "apilist:", err) //nolint:forbidigo // a generator must be able to report why it failed
		os.Exit(1)
	}
}

func run(root string) error {
	modPath, err := modulePath(filepath.Join(root, "go.mod"))
	if err != nil {
		return err
	}

	dirs, err := importableDirs(root)
	if err != nil {
		return err
	}

	var out bytes.Buffer
	out.WriteString("# The public API of this module, as a golden file. Regenerate with `mise run api`.\n")
	out.WriteString("# Growing this surface is a reviewed diff: see CHARTER.md.\n")

	for _, dir := range dirs {
		rendered, pkgName, err := renderPackage(filepath.Join(root, dir))
		if err != nil {
			return err
		}
		if pkgName == "" {
			continue
		}
		importPath := modPath
		if dir != "." {
			importPath = modPath + "/" + filepath.ToSlash(dir)
		}
		fmt.Fprintf(&out, "\npackage %s // import %q\n\n", pkgName, importPath)
		for _, line := range rendered {
			out.WriteString(line)
			out.WriteString("\n")
		}
	}

	return os.WriteFile(filepath.Join(root, outputFile), out.Bytes(), 0o644) //nolint:gosec // a golden file is world-readable by design
}

// modulePath reads the module's import path out of go.mod. Parsing the one
// line is cheaper than depending on golang.org/x/mod, and this module has
// no third-party dependencies by charter.
func modulePath(goMod string) (string, error) {
	b, err := os.ReadFile(goMod) //nolint:gosec // the path is this generator's own argument, not user input
	if err != nil {
		return "", fmt.Errorf("read go.mod: %w", err)
	}
	for line := range strings.Lines(string(b)) {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(rest), nil
		}
	}
	return "", errors.New("no module directive in go.mod")
}

// importableDirs returns every directory a consumer could import, sorted:
// the module root and its subdirectories, minus internal packages (which
// Go forbids importing), main packages, and the usual non-package
// directories.
func importableDirs(root string) ([]string, error) {
	var dirs []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		base := d.Name()
		if rel != "." && (strings.HasPrefix(base, ".") || strings.HasPrefix(base, "_") ||
			base == "internal" || base == "testdata" || base == "cmd" || base == "docs") {
			return fs.SkipDir
		}
		dirs = append(dirs, rel)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", root, err)
	}
	slices.Sort(dirs)
	return dirs, nil
}

// renderPackage returns one rendered line (or block, for a struct or
// interface) per exported declaration in dir, sorted, along with the
// package's name. It returns an empty name when dir holds no Go package.
//
// Files are parsed without comment mode, so no doc comment can reach the
// output: the golden file describes the surface, not its documentation.
func renderPackage(dir string) ([]string, string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, "", fmt.Errorf("read %s: %w", dir, err)
	}

	fset := token.NewFileSet()
	var (
		pkgName string
		lines   []string
	)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, "", fmt.Errorf("parse %s: %w", name, err)
		}
		if file.Name.Name == "main" {
			return nil, "", nil
		}
		pkgName = file.Name.Name

		for _, decl := range file.Decls {
			rendered, err := renderDecl(fset, decl)
			if err != nil {
				return nil, "", err
			}
			lines = append(lines, rendered...)
		}
	}

	slices.Sort(lines)
	return lines, pkgName, nil
}

// renderDecl renders the exported part of one top-level declaration. A
// grouped const or var declaration contributes one line per exported spec,
// so regrouping a block does not show up as an API change.
func renderDecl(fset *token.FileSet, decl ast.Decl) ([]string, error) {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		if !isExportedFunc(d) {
			return nil, nil
		}
		sig := *d
		sig.Body = nil
		sig.Doc = nil
		return renderNode(fset, &sig)

	case *ast.GenDecl:
		if d.Tok == token.IMPORT {
			return nil, nil
		}
		var lines []string
		for _, spec := range d.Specs {
			if !isExportedSpec(spec) {
				continue
			}
			strip(spec)
			hideUnexportedFields(spec)
			rendered, err := renderNode(fset, spec)
			if err != nil {
				return nil, err
			}
			for i, line := range rendered {
				if i == 0 {
					line = d.Tok.String() + " " + line
				}
				lines = append(lines, line)
			}
			// Keep a multi-line block together as one sortable unit.
			if len(rendered) > 1 {
				lines = []string{strings.Join(lines, "\n")}
			}
		}
		return lines, nil
	}
	return nil, nil
}

// renderNode formats node with go/printer and splits it into lines.
func renderNode(fset *token.FileSet, node ast.Node) ([]string, error) {
	var buf bytes.Buffer
	cfg := printer.Config{Mode: printer.UseSpaces | printer.TabIndent, Tabwidth: 8}
	if err := cfg.Fprint(&buf, fset, node); err != nil {
		return nil, fmt.Errorf("print declaration: %w", err)
	}
	// An empty struct body prints across two lines once its unexported
	// fields are removed; collapse it, and drop the blank lines left where
	// stripped doc comments used to be, so the golden file stays readable.
	text := strings.ReplaceAll(buf.String(), "struct {\n}", "struct{}")
	var lines []string
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines, nil
}

// hideUnexportedFields removes the unexported fields of a struct type from
// the rendered output. They are not part of the public surface, so a
// refactor that renames one must not read as an API change in the golden
// file.
func hideUnexportedFields(spec ast.Spec) {
	ts, ok := spec.(*ast.TypeSpec)
	if !ok {
		return
	}
	st, ok := ts.Type.(*ast.StructType)
	if !ok || st.Fields == nil {
		return
	}
	st.Fields.List = slices.DeleteFunc(st.Fields.List, func(f *ast.Field) bool {
		if len(f.Names) == 0 {
			// An embedded field is part of the surface when its type is.
			return !exportedTypeName(f.Type)
		}
		f.Names = slices.DeleteFunc(f.Names, func(n *ast.Ident) bool { return !n.IsExported() })
		return len(f.Names) == 0
	})
}

// isExportedFunc reports whether d is part of the public surface: an
// exported function, or an exported method on an exported type. A method
// on an unexported type is unreachable from outside the package even when
// the method name is exported.
func isExportedFunc(d *ast.FuncDecl) bool {
	if !d.Name.IsExported() {
		return false
	}
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return true
	}
	return exportedTypeName(d.Recv.List[0].Type)
}

// exportedTypeName reports whether a receiver type expression names an
// exported type, looking through pointers and generic instantiations.
func exportedTypeName(expr ast.Expr) bool {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return exportedTypeName(t.X)
	case *ast.IndexExpr:
		return exportedTypeName(t.X)
	case *ast.IndexListExpr:
		return exportedTypeName(t.X)
	case *ast.Ident:
		return t.IsExported()
	}
	return false
}

// isExportedSpec reports whether spec declares at least one exported name.
func isExportedSpec(spec ast.Spec) bool {
	switch s := spec.(type) {
	case *ast.TypeSpec:
		return s.Name.IsExported()
	case *ast.ValueSpec:
		return slices.ContainsFunc(s.Names, (*ast.Ident).IsExported)
	}
	return false
}

// strip removes the doc and trailing comments hanging off a node and its
// children. Files are parsed without comment mode, so this is belt and
// braces rather than the primary defence — but a future change to the
// parse mode should not silently start leaking prose into the golden file.
func strip(node ast.Node) {
	ast.Inspect(node, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.Field:
			v.Doc, v.Comment = nil, nil
		case *ast.ValueSpec:
			v.Doc, v.Comment = nil, nil
		case *ast.TypeSpec:
			v.Doc, v.Comment = nil, nil
		case *ast.GenDecl:
			v.Doc = nil
		case *ast.FuncDecl:
			v.Doc = nil
		}
		return true
	})
}
