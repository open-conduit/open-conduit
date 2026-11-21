package storage

import "time"

// TransactionTiming is how long one write transaction waited and held the
// store's writer.
type TransactionTiming struct {
	// Wait runs from asking for the writer until the transaction began,
	// or until it gave up when it never did.
	Wait time.Duration
	// Hold runs from the transaction beginning until it committed or
	// rolled back. It is zero when the transaction never began.
	Hold time.Duration
	// Began reports whether the transaction got the writer. One that
	// didn't (its context ended in the queue, or the lock stayed busy) is
	// still reported, since those are the longest waits.
	Began bool
	// Committed reports whether it committed.
	Committed bool
}

// TransactionTimer is optional: a Store that serialises its writers can
// report each write transaction's timing, so a host can show how long
// writers queue. fn runs on the transaction's goroutine once the writer is
// released, before Transact returns, so it must return quickly. A nil fn
// stops the reports.
type TransactionTimer interface {
	TimeTransactions(fn func(TransactionTiming))
}
