//go:build integration

package api

// ScratchDB is a freshly migrated database of a test's own, for the tests in
// api_test that read or move state the whole database shares: the change
// feed's head and the distiller's cursor.
var ScratchDB = cacheTestDB
