package core

// For the tests of the package, which live in core_test.

var ParquetKeys = parquetKeys

// SetStateBatch changes the size of the transactions of a rebuild and
// returns how to put it back.
func SetStateBatch(n int) (restore func()) {
	old := stateBatch
	stateBatch = n
	return func() { stateBatch = old }
}
