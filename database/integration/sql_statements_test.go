//go:build integration

package integration

import (
	"context"
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestAllSQLStatementsPrepare asks PostgreSQL to PREPARE every static SQL
// statement the fleet sends -- each string literal passed to a database/sql
// or pgx query method in non-test Go code, plus the Python workers' SQL --
// against a database with every migration applied. Unit tests of these
// callers mock the driver, so a statement naming a column that does not
// exist, or binding one parameter as two types, passes them all and then
// fails on every production call. PREPARE parses and type-checks without
// executing anything.
//
// DATABASE_URL must point at a database with database/migrations applied
// (the p4-hybrid-retrieval postgres-integration job provides one).
func TestAllSQLStatementsPrepare(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Fatal("DATABASE_URL must be set for PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	root := filepath.Join("..", "..")
	statements := append(goStatements(t, root), pythonStatements(t, filepath.Join(root, "workers"))...)
	if len(statements) < 100 {
		t.Fatalf("extracted only %d statements; the extractor is broken", len(statements))
	}

	checked := 0
	for i, st := range statements {
		if goFormatVerb.MatchString(st.sql) {
			continue // fmt.Sprintf template: completed at runtime, not checkable statically
		}
		_, err := conn.Prepare(ctx, "scos_sql_check_"+strconv.Itoa(i), st.sql)
		if err != nil {
			// A parameter used only in a context that fixes no type is a
			// property of the query shape, not a schema mismatch; the driver
			// supplies the type at execution.
			if strings.Contains(err.Error(), "could not determine data type of parameter") {
				continue
			}
			t.Errorf("%s:%d does not prepare against the migrated schema: %v\n  %s", st.file, st.line, err, oneLine(st.sql))
			continue
		}
		checked++
	}
	t.Logf("prepared %d of %d extracted statements", checked, len(statements))
}

type sqlStatement struct {
	file string
	line int
	sql  string
}

var (
	sqlStart     = regexp.MustCompile(`(?s)^\s*(SELECT|INSERT|UPDATE|DELETE|WITH)\s`)
	goFormatVerb = regexp.MustCompile(`%[svdqw]`)
	pySQL        = regexp.MustCompile(`(?s)"""(.*?)"""|execute\(\s*"([^"]+)"`)
	pyParam      = regexp.MustCompile(`%\(\w+\)s|%s`)
	// Index of the SQL argument; pgx methods take a ctx first, so the next
	// argument is tried as well.
	queryMethods = map[string]int{
		"Exec": 0, "Query": 0, "QueryRow": 0, "Prepare": 0,
		"ExecContext": 1, "QueryContext": 1, "QueryRowContext": 1, "PrepareContext": 1,
	}
)

func goStatements(t *testing.T, root string) []sqlStatement {
	t.Helper()
	var out []sqlStatement
	fset := gotoken.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "_to_delete", "audit-results":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil // not this test's concern; go build reports it
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			idx, ok := queryMethods[sel.Sel.Name]
			if !ok {
				return true
			}
			for _, i := range []int{idx, idx + 1} {
				if i >= len(call.Args) {
					continue
				}
				lit, ok := call.Args[i].(*ast.BasicLit)
				if !ok || lit.Kind != gotoken.STRING {
					continue
				}
				s, err := strconv.Unquote(lit.Value)
				if err == nil && sqlStart.MatchString(strings.ToUpper(s)) {
					out = append(out, sqlStatement{path, fset.Position(lit.Pos()).Line, s})
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

func pythonStatements(t *testing.T, root string) []sqlStatement {
	t.Helper()
	var out []sqlStatement
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".py") {
			return err
		}
		if strings.Contains(path, string(filepath.Separator)+"tests"+string(filepath.Separator)) || strings.HasPrefix(d.Name(), "test_") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range pySQL.FindAllSubmatchIndex(src, -1) {
			start, end := m[2], m[3]
			if start < 0 {
				start, end = m[4], m[5]
			}
			s := string(src[start:end])
			if !sqlStart.MatchString(s) { // case-sensitive: skips docstrings
				continue
			}
			n := 0
			s = pyParam.ReplaceAllStringFunc(s, func(string) string { n++; return "$" + strconv.Itoa(n) })
			out = append(out, sqlStatement{path, 1 + strings.Count(string(src[:start]), "\n"), s})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
