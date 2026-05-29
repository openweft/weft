package filesystem_ext4

import "sync"

var (
	seqNameMu  sync.Mutex
	seqNameMap = map[uint64]string{}
)

// registerSeqName records a mapping from a committed seq to a filename
// for diagnostic correlation. Tests/instrumentation may register names
// in commit callbacks so the commit path can inspect applied entries.
func registerSeqName(seq uint64, name string) {
	seqNameMu.Lock()
	seqNameMap[seq] = name
	seqNameMu.Unlock()
}

func getSeqName(seq uint64) (string, bool) {
	seqNameMu.Lock()
	n, ok := seqNameMap[seq]
	seqNameMu.Unlock()
	return n, ok
}
