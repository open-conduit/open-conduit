package storage

import "time"

// TransactionTiming is how long one write transaction waited and held the
// store's writer.
type TransactionTiming struct {
	// Wait runs from asking for the writer until the transaction began.
	Wait time.Duration
	// Hold runs from the transaction beginning until it committed or
	// rolled back.
	Hold time.Duration
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
