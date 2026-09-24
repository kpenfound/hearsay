//go:build integration

package api

import "time"

// ScratchDB is a freshly migrated database of a test's own, for the tests in
// api_test that read or move state the whole database shares: the change
// feed's head and the distiller's cursor.
var ScratchDB = cacheTestDB

// AnswerWithin sets how long a command runs before its answer is deferred.
func (h *Interactions) AnswerWithin(d time.Duration) { h.answerWithin = d }
