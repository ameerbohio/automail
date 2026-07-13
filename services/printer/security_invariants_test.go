package main

// Executable guards for the printer-side security invariants in CLAUDE.md /
// plans/02-security.md (Testing Goal T3 / Part 6):
//
//   - Plaintext lives only in tmpfs (/dev/shm): the write target is under
//     /dev/shm, and no code path writes a file anywhere else.
//   - Passphrase hygiene: PRINTER_KEY_PASSPHRASE is unset from the environment
//     as part of loading the key, so it can't be read from /proc/self/environ
//     or inherited by a child process.
//
// (Wiping of the decrypted PDF is covered end-to-end by
// TestHandleDispatch_DeliversAndWipes; these tests guard the static properties.)

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInvariant_TmpfsDirUnderDevShm pins the one directory plaintext may touch.
func TestInvariant_TmpfsDirUnderDevShm(t *testing.T) {
	if !strings.HasPrefix(tmpfsDir, "/dev/shm") {
		t.Fatalf("tmpfsDir = %q; plaintext must live under /dev/shm (RAM-backed), never real disk", tmpfsDir)
	}
}

// fileWriteFuncs are the os calls that create/write a file on disk.
var fileWriteFuncs = map[string]bool{"WriteFile": true, "Create": true, "OpenFile": true}

// scanFileWrites returns "file:line" for every os file-write whose path does NOT
// reference tmpfsDir — i.e. a potential plaintext-to-arbitrary-path write.
func scanFileWrites(fset *token.FileSet, file *ast.File) []string {
	// Local dataflow: names assigned from an expression that references
	// tmpfsDir (e.g. `path := filepath.Join(tmpfsDir, ...)`) are tmpfs-derived,
	// so writing through them is allowed.
	tmpfsVars := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, rhs := range as.Rhs {
			if i < len(as.Lhs) && referencesIdent(rhs, "tmpfsDir") {
				if id, ok := as.Lhs[i].(*ast.Ident); ok {
					tmpfsVars[id.Name] = true
				}
			}
		}
		return true
	})

	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "os" || !fileWriteFuncs[sel.Sel.Name] {
			return true
		}
		ok = len(call.Args) > 0 && referencesIdent(call.Args[0], "tmpfsDir")
		if !ok {
			if id, isID := call.Args[0].(*ast.Ident); isID && tmpfsVars[id.Name] {
				ok = true
			}
		}
		if !ok {
			out = append(out, fset.Position(call.Pos()).String()+": os."+sel.Sel.Name+" path not derived from tmpfsDir")
		}
		return true
	})
	return out
}

func referencesIdent(n ast.Node, name string) bool {
	found := false
	ast.Inspect(n, func(x ast.Node) bool {
		if id, ok := x.(*ast.Ident); ok && id.Name == name {
			found = true
		}
		return !found
	})
	return found
}

// TestInvariant_PlaintextWritesTargetTmpfsOnly asserts every file write in the
// printer's non-test code targets tmpfsDir.
func TestInvariant_PlaintextWritesTargetTmpfsOnly(t *testing.T) {
	var all []string
	fset := token.NewFileSet()
	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		all = append(all, scanFileWrites(fset, f)...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) > 0 {
		t.Fatalf("plaintext-to-disk risk — file write(s) not under tmpfsDir:\n  %s", strings.Join(all, "\n  "))
	}
}

// TestInvariant_ScannerCatchesOffTmpfsWrite proves the write scanner actually
// flags an off-tmpfs write, so a clean run above is meaningful.
func TestInvariant_ScannerCatchesOffTmpfsWrite(t *testing.T) {
	const bad = `package p
import "os"
func leak(b []byte) { os.WriteFile("/var/tmp/leak.pdf", b, 0o600) }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "bad.go", bad, 0)
	if err != nil {
		t.Fatal(err)
	}
	if v := scanFileWrites(fset, f); len(v) != 1 {
		t.Fatalf("scanner should flag the off-tmpfs write, got %d findings", len(v))
	}
}

// TestInvariant_PassphraseEnvUnsetAfterLoad asserts loadDocKey removes
// PRINTER_KEY_PASSPHRASE from the environment even when key loading then fails —
// the unset happens before the key file is read.
func TestInvariant_PassphraseEnvUnsetAfterLoad(t *testing.T) {
	t.Setenv("PRINTER_KEY_PASSPHRASE", "correct-horse-battery-staple")
	if _, err := loadDocKey(filepath.Join(t.TempDir(), "does-not-exist.pem")); err == nil {
		t.Fatal("expected loadDocKey to fail on a missing key file")
	}
	if v := os.Getenv("PRINTER_KEY_PASSPHRASE"); v != "" {
		t.Fatalf("PRINTER_KEY_PASSPHRASE still set after loadDocKey (=%q); passphrase must be unset from env", v)
	}
}
