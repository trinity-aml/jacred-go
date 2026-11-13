package tracks

import (
	"io"
	"sync"
	"testing"
)

func TestProbePieceBudgetShared(t *testing.T) {
	budget := &pieceBudget{limit: 8}
	first := &memPiece{size: 8, budget: budget}
	second := &memPiece{size: 8, budget: budget}
	if _, err := first.WriteAt([]byte{1}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := first.WriteAt([]byte{2}, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := second.WriteAt([]byte{3}, 0); err == nil {
		t.Fatal("accepted over-budget allocation")
	}
	if second.data != nil || budget.used != 8 {
		t.Fatal("budget not enforced")
	}
}

func TestProbePieceBudgetConcurrent(t *testing.T) {
	budget := &pieceBudget{limit: 8}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); p := &memPiece{size: 8, budget: budget}; _, _ = p.WriteAt([]byte{1}, 0) }()
	}
	wg.Wait()
	if budget.used != 8 {
		t.Fatalf("allocated %d", budget.used)
	}
}

func TestProbePieceInvalidWriteDoesNotAllocate(t *testing.T) {
	p := &memPiece{size: 8, budget: &pieceBudget{limit: 8}}
	if _, err := p.WriteAt([]byte{1}, -1); err != io.ErrShortWrite {
		t.Fatal(err)
	}
	if p.data != nil {
		t.Fatal("invalid write allocated")
	}
}
