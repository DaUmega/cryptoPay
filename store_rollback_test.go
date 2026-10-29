package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A store whose directory is a regular file, not a directory, makes
// every save fail with ENOTDIR regardless of who runs the test
// (chmod-based read-only dirs do not fail for root).
func brokenStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0644); err != nil {
		t.Fatal(err)
	}
	s := &Store{dir: blocker, Invoices: map[string]*Invoice{}, Pool: map[string][]*PoolEntry{}}
	return s
}

func TestPoolAddSaveFailureRollsBack(t *testing.T) {
	s := brokenStore(t)
	if _, _, err := s.addToPool("BTC", []string{testBTCAddr}); err == nil {
		t.Fatal("addToPool with a broken data dir must error")
	}
	if len(s.Pool["BTC"]) != 0 {
		t.Fatalf("pool has %d entries after a failed add, want 0: the API said it failed, so the in-memory pool must not show it as added", len(s.Pool["BTC"]))
	}
	// A retired address re-add must also roll back, not flip to available.
	s2 := &Store{dir: s.dir, Invoices: map[string]*Invoice{}, Pool: map[string][]*PoolEntry{
		"BTC": {{Address: testBTCAddr, Status: "retired"}},
	}}
	if _, _, err := s2.addToPool("BTC", []string{testBTCAddr}); err == nil {
		t.Fatal("re-add with a broken data dir must error")
	}
	if got := s2.Pool["BTC"][0].Status; got != "retired" {
		t.Fatalf("retired entry became %q after a failed re-add, want retired", got)
	}
}

func TestAssignSaveFailureRollsBack(t *testing.T) {
	s := brokenStore(t)
	s.Pool["BTC"] = []*PoolEntry{{Address: testBTCAddr, Status: "available"}}
	if _, err := s.assign("BTC", "test-invoice-id"); err == nil {
		t.Fatal("assign with a broken data dir must error")
	}
	if got := s.Pool["BTC"][0].Status; got != "available" {
		t.Fatalf("address became %q after a failed assign, want available (no invoice exists for it)", got)
	}
	if s.Pool["BTC"][0].InvoiceID != "" {
		t.Fatalf("address kept invoice id %q after a failed assign", s.Pool["BTC"][0].InvoiceID)
	}
}

func TestCreateInvoiceSaveFailureFreesAddress(t *testing.T) {
	// A store that saves the assign step but fails the invoice save
	// is hard to arrange with a broken dir alone (assign would fail
	// first), so this test covers the free() primitive directly:
	// an assigned address must return to available, not retired.
	s := brokenStore(t)
	s.Pool["BTC"] = []*PoolEntry{{Address: testBTCAddr, Status: "assigned", InvoiceID: "doomed-id"}}
	s.free(testBTCAddr)
	if got := s.Pool["BTC"][0].Status; got != "available" {
		t.Fatalf("free() left address %q, want available", got)
	}
	if s.Pool["BTC"][0].InvoiceID != "" {
		t.Fatalf("free() left invoice id %q", s.Pool["BTC"][0].InvoiceID)
	}
}
