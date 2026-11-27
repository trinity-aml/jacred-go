package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Parse and ParseAllTask must share one run flag. With a guard apiece they
// swept concurrently — the deployed schedule fires parsealltask every few
// minutes while a full sweep runs for hours — so two runs drove the same
// session, the same re-login path and the same rate limit at the tracker.
// toloka is where that was measured: it answered HTTP 429 to download.php 126
// times in a single eight-category run.
//
// This is a drift guard rather than a unit test because the flag is declared
// separately in each of the twelve parsers, and a hand-maintained duplicate
// that nothing checks is exactly how this codebase lost 35 rutracker forums.
func TestParsersKeepOneRunFlag(t *testing.T) {
	dirs, err := filepath.Glob("../cron/*")
	if err != nil {
		t.Fatal(err)
	}
	fieldRe := regexp.MustCompile(`(?m)^\s+(working|allWork|busy)\s+bool\s*$`)

	checked := 0
	for _, dir := range dirs {
		name := filepath.Base(dir)
		src, err := os.ReadFile(filepath.Join(dir, name+".go"))
		if err != nil {
			continue
		}
		body := string(src)
		// Only the parsers that have both entrypoints are in scope.
		if !strings.Contains(body, ") ParseAllTask(") {
			continue
		}
		checked++
		found := fieldRe.FindAllStringSubmatch(body, -1)
		var names []string
		for _, m := range found {
			names = append(names, m[1])
		}
		if len(names) != 1 {
			t.Errorf("%s: ожидался один флаг прогона, найдено %d %v — Parse и ParseAllTask снова могут идти параллельно",
				name, len(names), names)
		}
	}
	if checked < 12 {
		t.Errorf("проверено парсеров: %d, ожидалось не меньше 12 — тест перестал их находить", checked)
	}
}

// A page that a tracker refused must never be reported as an empty page.
//
// Every caller in this tree reads an empty result as "this page holds
// nothing": ParseAllTask logs `empty (marking today)` and settles the page as
// swept, so the cycle rotates having never read it, and the run answers ok
// with failed=0. That is indistinguishable from a quiet day on the tracker.
//
// It has cost real data more than once. mazepa lost 445 pages in one morning
// on 2026-11-27 and showed `running · 36%` while doing it; rutracker lost
// pages the same morning (`cat=208 page=79 returned login form` immediately
// followed by `cat=208 page=79 empty (marking today)`); torrentby discarded
// complete 100-record pages, since its catalog is public; and kinozal,
// korsars, nnmclub, selezen and animelayer all had the same return.
//
// This is a drift guard rather than a unit test because the shape is written
// out separately in each parser, and a hand-maintained duplicate that nothing
// checks is how this codebase lost 35 rutracker forums. It reads the AST so
// that a comment quoting the forbidden code cannot satisfy or trip it — a scan
// over raw text has made both of those mistakes here.
func TestASessionRefusalIsNeverReportedAsAnEmptyPage(t *testing.T) {
	files, err := filepath.Glob("../cron/*/*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			block, ok := n.(*ast.BlockStmt)
			if !ok {
				return true
			}
			// Only blocks that act on a dead session are in scope: the
			// invalidate call is what marks this as the refusal path.
			if !blockInvalidatesTheSession(block) {
				return true
			}
			checked++
			for _, stmt := range block.List {
				ret, ok := stmt.(*ast.ReturnStmt)
				if !ok || len(ret.Results) == 0 {
					continue
				}
				if looksLikeSilentZero(ret.Results) {
					t.Errorf("%s:%d: a refused session returns an empty result with a nil error — "+
						"the caller will read it as an empty page and settle it as swept",
						path, fset.Position(ret.Pos()).Line)
				}
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("found no session-invalidation blocks at all — this guard is no longer looking at anything")
	}
}

// blockInvalidatesTheSession reports whether this block calls one of the
// helpers that drop a session, directly (not in a nested block, which
// ast.Inspect visits separately).
//
// Block scope is deliberate. Widening the scope to the whole function was
// tried and reports seven more returns, every one of them "the body came back
// empty or unrecognised" rather than a session refusal — and one of those,
// mazepa's `len(rows) == 0`, is a category's genuinely empty last page. A
// guard that has to be appeased by contorting correct code stops being read.
// The cost is that it cannot see the shape selezen and animelayer have, where
// the invalidate is inside a retry loop and the silent return is the
// function's tail; those two are pinned by tests in their own packages.
func blockInvalidatesTheSession(block *ast.BlockStmt) bool {
	for _, stmt := range block.List {
		expr, ok := stmt.(*ast.ExprStmt)
		if !ok {
			continue
		}
		call, ok := expr.X.(*ast.CallExpr)
		if !ok {
			continue
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		switch sel.Sel.Name {
		case "invalidateCookie", "InvalidateSession", "clearFlareSession":
			return true
		}
	}
	return false
}

// looksLikeSilentZero reports whether a return hands the caller empty data
// together with a nil error.
//
// Two or more results are required, and the last of them must be the literal
// nil: a bare `return nil` from a function that returns only an error is a
// *success*, which an earlier version of this guard flagged on animelayer's
// authorize(). At least one of the remaining results must be a zero literal —
// one of them being a carried-over value, as in `return lastCookie, "", nil`,
// does not make the return any less silent.
func looksLikeSilentZero(results []ast.Expr) bool {
	if len(results) < 2 {
		return false
	}
	last, ok := results[len(results)-1].(*ast.Ident)
	if !ok || last.Name != "nil" {
		return false
	}
	for _, r := range results[:len(results)-1] {
		if isZeroLiteral(r) {
			return true
		}
	}
	return false
}

func isZeroLiteral(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name == "nil"
	case *ast.BasicLit:
		return v.Value == `""` || v.Value == "0"
	}
	return false
}

// An authorization failure must not spend a page's cycle budget.
//
// settle(cycle, …, false) records a failed attempt, and after
// core.ParseAllFailBudget of them the page settles as swept without ever
// having been read — which is correct for a page that genuinely cannot be
// fetched and wrong for one the tracker refused, because the refusal is not
// the page's fault and every other page in the map will meet it too. The same
// line is already drawn for core.ErrFlareCooldown, whose own comment records
// 3 169 of rutracker's 3 295 lost pages in one night.
//
// Only parsers that actually produce the sentinel are in scope: a tracker with
// no login cannot reach this branch. The check is proximity-based over
// comment-stripped source rather than an AST walk, which is enough because
// every one of these sites is the first statement of an `if err != nil` block
// — but it means a refactor that moves the exemption far from the settle call
// would have to re-satisfy it deliberately.
func TestAnAuthorizationFailureDoesNotSpendTheCycleBudget(t *testing.T) {
	files, err := filepath.Glob("../cron/*/*.go")
	if err != nil {
		t.Fatal(err)
	}
	settleRe := regexp.MustCompile(`p\.settle\(cycle[^)]*,\s*false\)`)
	checked := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		src := stripLineComments(string(raw))
		// A parser that never reports an authorization failure cannot reach
		// this branch, so requiring the exemption of it would be noise.
		if !strings.Contains(src, "core.ErrNotAuthorized") {
			continue
		}
		lines := strings.Split(src, "\n")
		for i, line := range lines {
			if !settleRe.MatchString(line) {
				continue
			}
			checked++
			from := i - 12
			if from < 0 {
				from = 0
			}
			window := strings.Join(lines[from:i], "\n")
			if !strings.Contains(window, "errors.Is(err, core.ErrNotAuthorized)") {
				t.Errorf("%s:%d: settle(..., false) with no core.ErrNotAuthorized exemption above it — "+
					"a refused session will settle pages it never read", path, i+1)
			}
		}
	}
	if checked == 0 {
		t.Fatal("found no settle(cycle, ..., false) calls in a parser that reports authorization failures — this guard is no longer looking at anything")
	}
}

// stripLineComments removes // comments so that an explanation quoting the
// code a guard forbids can neither satisfy nor trip it. Both mistakes are on
// record in this tree.
func stripLineComments(src string) string {
	var out strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return out.String()
}
