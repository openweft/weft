//go:build !test

package filesystem_ext4

// traceInodeWrite is a no-op in non-test builds. The test build provides a
// richer implementation that logs stack traces when inode sizes change in
// suspicious ways.
func traceInodeWrite(f readerWriterAt, fsOffset int64, sb *superblock, ino uint32, oldSize, newSize uint64) {
}

// traceDirOp is a no-op in non-test builds. In test builds it logs directory
// operations (add/zero entries) so we can correlate renames with inode writes.
func traceDirOp(action string, dirIno uint32, childIno uint32, name string) {}

// traceSetSize is a no-op outside of tests; test builds log size changes.
func traceSetSize(ino uint32, oldSize, newSize uint64) {}

// traceRawBlockWrite is a no-op in non-test builds. Test builds inspect raw
// block writes and print stacks when inode-table blocks are written.
func traceRawBlockWrite(f readerWriterAt, fsOffset int64, sb *superblock, blockNum uint64, data []byte) {
}

// traceAllocInode is a no-op in non-test builds; test builds log alloc stacks.
func traceAllocInode(f readerWriterAt, fsOffset int64, sb *superblock, ino uint32) {}

// traceFreeInodeSlot is a no-op in non-test builds; test builds log free stacks.
func traceFreeInodeSlot(f readerWriterAt, fsOffset int64, sb *superblock, ino uint32) {}

// traceWriteAt is a no-op in non-test builds; test builds inspect arbitrary
// WriteAt calls to detect writes that overlap inode-table byte ranges.
func traceWriteAt(f readerWriterAt, fsOffset int64, sb *superblock, off int64, data []byte) {}

// traceLookupParentResolved is a no-op outside of tests. In test builds it
// logs when a parent directory lookup resolved to a directory inode and the
// final filename, which helps correlate path resolution with subsequent
// directory/inode modifications.
func traceLookupParentResolved(f readerWriterAt, fsOffset int64, sb *superblock, path string, parentIno uint32, name string) {
}

// traceRename is a no-op outside of tests; test builds log rename stacks
// for targeted filenames (like "hostname").
func traceRename(f readerWriterAt, fsOffset int64, sb *superblock, oldParent, newParent uint32, oldName, newName string) {
}

// traceDecrementAndFreeInode is a no-op outside of tests; test builds log a
// stack trace when an inode's link count is decremented and the inode is
// freed.
func traceDecrementAndFreeInode(f readerWriterAt, fsOffset int64, sb *superblock, ino uint32, oldLinks, newLinks uint16) {
}

// traceMemFileWriteAt is a no-op outside of tests; test builds capture all
// in-memory HookMemFile WriteAt calls so we can correlate arbitrary writes
// with inode-table corruption during stress tests.
func traceMemFileWriteAt(f readerWriterAt, off int64, data []byte) {}

// traceReadFilePath is a no-op outside of tests. The test build implements a
// diagnostic that prints inode and byte-offset information when reading
// paths of interest (like /etc/hostname or /etc/hosts).
func traceReadFilePath(f readerWriterAt, fsOffset int64, sb *superblock, path string, ino uint32, size uint64) {
}
