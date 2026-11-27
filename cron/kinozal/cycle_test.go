package kinozal

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
	"time"
)

// latestPagesPerCategory is what markLatestPageToday stamps per category. The
// production map held 25 of these buckets, 2500 slots in all.
const latestPagesPerCategory = 100

func sweepTestParser(t *testing.T) *Parser {
	t.Helper()
	p := &Parser{DataDir: t.TempDir(), loc: time.UTC}
	sweepPages := []Task{
		{Page: 0, UpdateTime: "0001-01-01T00:00:00"},
		{Page: 1, UpdateTime: "0001-01-01T00:00:00"},
		{Page: 2, UpdateTime: "0001-01-01T00:00:00"},
	}
	// ParseLatest's bucket: the empty argument, which ParseAllTask never walks.
	latest := make([]Task, 0, latestPagesPerCategory)
	for page := 0; page < latestPagesPerCategory; page++ {
		latest = append(latest, Task{Page: page})
	}
	p.tasks = map[string]map[string][]Task{
		"1": {
			"&d=2026&t=1": sweepPages,
			"":            latest,
		},
	}
	return p
}

// TestTheCycleCountsOnlyThePagesTheSweepVisits is the production bug of
// 2026-11-27: kinozal's cycle counted ParseLatest's 2500 pages, which
// ParseAllTask skips, so pending could never reach zero. The cycle opened on
// 2026-09-26 therefore never completed, never rotated, and every page it had
// already settled stayed settled — 74 runs in one day, each reporting
// `pending=2500->2500/8901 … processed=0/6401`.
func TestTheCycleCountsOnlyThePagesTheSweepVisits(t *testing.T) {
	p := sweepTestParser(t)

	p.mu.Lock()
	cycle, pending, total := p.beginCycleLocked()
	p.mu.Unlock()

	if total != 3 {
		t.Fatalf("цикл насчитал %d слотов, а обход ходит по 3 (пустой аргумент — план ParseLatest)", total)
	}
	if pending != 3 {
		t.Fatalf("pending=%d, ожидалось 3", pending)
	}

	// Sweep every page the loop would visit, exactly as ParseAllTask does.
	for _, page := range []int{0, 1, 2} {
		p.settle(cycle, "1", "&d=2026&t=1", page, true)
	}

	p.mu.Lock()
	next, nextPending, _ := p.beginCycleLocked()
	p.mu.Unlock()

	if next.CycleID == cycle.CycleID {
		t.Fatalf("цикл не провернулся после полного обхода: остался %s — значит он не завершается никогда", cycle.CycleID)
	}
	if nextPending != 3 {
		t.Fatalf("новый цикл открылся с pending=%d, ожидалось 3", nextPending)
	}
}

// TestEveryCycleSlotCallCarriesThePredicate pins the half the behavioural test
// above cannot see on its own: one of the two call sites silently taking every
// argument again is exactly how this broke, and the sweep loop deciding it
// separately is how the two could drift apart.
func TestEveryCycleSlotCallCarriesThePredicate(t *testing.T) {
	const path = "kinozal.go"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("разбор %s: %v", path, err)
	}

	calls := 0
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if !strings.Contains(exprText(t, path, call.Fun), "CycleSlotsFromNestedMap") {
			return true
		}
		calls++
		if len(call.Args) != 3 {
			t.Errorf("%s: CycleSlotsFromNestedMap вызван с %d аргументами — предикат потерян",
				fset.Position(call.Pos()), len(call.Args))
			return true
		}
		if got := exprText(t, path, call.Args[2]); got != "sweptByParseAllTask" {
			t.Errorf("%s: предикат цикла — %q, а не sweptByParseAllTask", fset.Position(call.Pos()), got)
		}
		return true
	})
	if calls == 0 {
		t.Fatal("ни одного вызова CycleSlotsFromNestedMap — охрана проверяет не то")
	}

	// The sweep loop must ask the same function, not re-test the argument.
	// Read from the AST rather than the text: sweptByParseAllTask is itself
	// the one legitimate `arg != ""`, and its own doc comment quotes the rule.
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name == "sweptByParseAllTask" {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			cmp, ok := n.(*ast.BinaryExpr)
			if !ok || (cmp.Op != token.EQL && cmp.Op != token.NEQ) {
				return true
			}
			if comparesArgToEmptyString(cmp) {
				t.Errorf("%s: %s сравнивает arg с \"\" напрямую — решение должно быть одно, в sweptByParseAllTask",
					fset.Position(cmp.Pos()), fn.Name.Name)
			}
			return true
		})
	}

	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(sourceWithoutComments(string(src)), "sweptByParseAllTask(arg)"); n < 2 {
		t.Errorf("обход спрашивает sweptByParseAllTask %d раз — ожидалось в обоих циклах по карте", n)
	}
}

func comparesArgToEmptyString(cmp *ast.BinaryExpr) bool {
	isArg := func(e ast.Expr) bool {
		id, ok := e.(*ast.Ident)
		return ok && id.Name == "arg"
	}
	isEmpty := func(e ast.Expr) bool {
		lit, ok := e.(*ast.BasicLit)
		return ok && lit.Kind == token.STRING && (lit.Value == `""` || lit.Value == "``")
	}
	return (isArg(cmp.X) && isEmpty(cmp.Y)) || (isArg(cmp.Y) && isEmpty(cmp.X))
}

func exprText(t *testing.T, path string, e ast.Expr) string {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(src[e.Pos()-1 : e.End()-1])
}

// sourceWithoutComments strips comments before any source scan. A comment that
// quotes the forbidden code has twice cost this tree a wrong verdict — once
// satisfying a guard whose call had been deleted, once failing a guard against
// correct code.
func sourceWithoutComments(src string) string {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "src.go", src, 0)
	if err != nil {
		return src
	}
	out := []byte(src)
	base := fset.File(file.Pos()).Base()
	for _, group := range file.Comments {
		for i := int(group.Pos()) - base; i < int(group.End())-base && i < len(out); i++ {
			if out[i] != '\n' {
				out[i] = ' '
			}
		}
	}
	return string(out)
}
