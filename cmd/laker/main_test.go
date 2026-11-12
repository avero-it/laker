package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/avero-it/laker/pkg/config"
)

var excludedDirs = map[string]struct{}{
	"vendor":   {},
	".git":     {},
	"testdata": {},
}

// Every log line carries one of these, whatever its level, and so does every
// error value built with fmt.Errorf. Each prefix numbers independently, so a
// code only identifies a call site together with its prefix.
var codeRE = regexp.MustCompile(`(ERR|WRN|DBG|INF|FTL|TRC|PNC|LOG)[0-9a-fA-F]{3}`)

func findModuleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found from %s", dir)
		}
		dir = parent
	}
}

func TestErrorCodesAreUnique(t *testing.T) {
	root, err := findModuleRoot()
	if err != nil {
		t.Fatalf("cannot determine module root: %v", err)
	}

	seen := make(map[string]token.Position)
	dup := make(map[string][]token.Position)
	counts := make(map[string]int)

	fset := token.NewFileSet()

	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if _, skip := excludedDirs[d.Name()]; skip {
				return filepath.SkipDir
			}
			return nil
		}
		// tests quote the codes they expect, which is not a second call site
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}

		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}

			matches := codeRE.FindAllString(s, -1)
			if matches == nil {
				return true
			}

			pos := fset.Position(lit.Pos())
			for _, code := range matches {
				counts[code]++
				if first, exists := seen[code]; exists {
					if len(dup[code]) == 0 {
						dup[code] = append(dup[code], first)
					}
					dup[code] = append(dup[code], pos)
				} else {
					seen[code] = pos
				}
			}
			return true
		})

		return nil
	})
	if err != nil {
		t.Fatalf("error walking repository: %v", err)
	}

	// Find max unique code
	var maxUniqueCode string
	var maxUniqueValue int64 = -1
	for code, count := range counts {
		if count == 1 {
			val, err := strconv.ParseInt(code[3:], 16, 64)
			if err == nil && val > maxUniqueValue {
				maxUniqueValue = val
				maxUniqueCode = code
			}
		}
	}

	if maxUniqueCode != "" {
		t.Logf("Highest unique error code: %s", maxUniqueCode)
	} else {
		t.Logf("No unique error codes found")
	}

	t.Logf("codes in use: %d", len(counts))

	if len(dup) > 0 {
		for code, positions := range dup {
			t.Errorf("duplicate error code %s found at:", code)
			for _, p := range positions {
				t.Errorf("  - %s", p.String())
			}
		}
		t.Fatalf("duplicate error codes detected")
	}
}

// levelPrefix maps a zerolog level to the code prefix its lines must carry.
var levelPrefix = map[string]string{
	"Error": "ERR", "Warn": "WRN", "Debug": "DBG", "Info": "INF",
	"Fatal": "FTL", "Trace": "TRC", "Panic": "PNC", "Log": "LOG",
}

// logLevelOf walks back up a zerolog call chain - .Error().Str(...).Msgf(...) -
// to the call that set the level, and returns the prefix that level requires.
func logLevelOf(e ast.Expr) string {
	for {
		call, ok := e.(*ast.CallExpr)
		if !ok {
			return ""
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return ""
		}
		if p, ok := levelPrefix[sel.Sel.Name]; ok {
			return p
		}
		e = sel.X
	}
}

// A log line without a code cannot be traced back to its call site, and a code
// whose prefix disagrees with the level sends whoever greps it to the wrong
// place. TestErrorCodesAreUnique only proves the codes that exist are unique,
// so without this one a new log line can silently arrive with no code at all.
func TestEveryLogLineHasACode(t *testing.T) {
	root, err := findModuleRoot()
	if err != nil {
		t.Fatalf("cannot determine module root: %v", err)
	}

	checked := 0

	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if _, skip := excludedDirs[d.Name()]; skip {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		fbase := fset.File(file.Pos()).Base()
		rel, _ := filepath.Rel(root, path)

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name != "Msg" && sel.Sel.Name != "Msgf" && sel.Sel.Name != "Send" {
				return true
			}
			want := logLevelOf(sel.X)
			if want == "" {
				return true
			}
			checked++

			pos := fset.Position(call.Pos())
			if len(call.Args) == 0 {
				t.Errorf("%s:%d: %s log line has no message, so it can carry no code", rel, pos.Line, want)
				return true
			}

			// The raw source of the whole first argument, so a concatenation
			// or a call such as strings.Repeat is checked too.
			arg := call.Args[0]
			text := string(src[int(arg.Pos())-fbase : int(arg.End())-fbase])

			got := codeRE.FindString(text)
			if got == "" {
				t.Errorf("%s:%d: log line has no %sxxx code: %.60s", rel, pos.Line, want, text)
				return true
			}
			if !strings.HasPrefix(got, want) {
				t.Errorf("%s:%d: code %s does not match the log level, want %sxxx", rel, pos.Line, got, want)
			}

			return true
		})

		return nil
	})
	if err != nil {
		t.Fatalf("error walking repository: %v", err)
	}

	t.Logf("log lines checked: %d", checked)
}

// writeConfig writes the example configuration switched to an fs lake.
func writeConfig(t *testing.T, lakeDir string) string {
	t.Helper()
	t.Setenv("LAKER_S3_ACCESS_KEY", "ak")
	t.Setenv("LAKER_S3_SECRET_KEY", "sk")
	t.Setenv("LAKER_WEB_TOKEN", "0123456789abcdef")

	dir := t.TempDir()
	y := strings.Replace(string(config.Example), "use: s3 ", "use: fs ", 1)
	y = strings.Replace(y, "path: ./lake", "path: "+lakeDir, 1)
	y = strings.Replace(y, "datadir: /var/lib/laker", "datadir: "+filepath.Join(dir, "data"), 1)
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCheckOK(t *testing.T) {
	res := runCheck(t.Context(), writeConfig(t, t.TempDir()))
	if !res.OK {
		t.Fatalf("check failed: %+v", res)
	}
	if res.Lake == nil || res.Lake.Kind != "fs" || res.ConfigHash == "" {
		t.Errorf("result = %+v", res)
	}
}

func TestCheckReportsPhaseAndCode(t *testing.T) {
	res := runCheck(t.Context(), writeConfig(t, filepath.Join(t.TempDir(), "missing")))
	if res.OK || res.Phase != "lake" || res.Code != "ERR308" {
		t.Fatalf("result = %+v, want lake/ERR308", res)
	}

	res = runCheck(t.Context(), filepath.Join(t.TempDir(), "nope.yaml"))
	if res.OK || res.Phase != "load" || res.Code != "ERR021" {
		t.Fatalf("result = %+v, want load/ERR021", res)
	}
}

// laker reads no source: a configuration that still lists adapters is
// refused when it is loaded, with the key named.
func TestAdaptersAreRefused(t *testing.T) {
	path := writeConfig(t, t.TempDir())
	b, _ := os.ReadFile(path)
	b = append(b, []byte("adapters:\n  - {name: mail, enabled: true, type: imap, schedule: manual, params: {}}\n")...)
	os.WriteFile(path, b, 0o600)

	res := runCheck(t.Context(), path)
	if res.OK || res.Phase != "load" || !strings.Contains(res.Message, "adapters") {
		t.Fatalf("result = %+v", res)
	}
}
