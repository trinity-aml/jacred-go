package core

import (
	"compress/gzip"
	"io"
	"sync"
)

// Pooled gzip readers and writers. Each gzip.Reader carries ~32 KiB of
// decompression window plus a CRC table; gzip.Writer holds a ~64 KiB deflate
// state plus output buffer. Allocating a fresh one per OpenRead / writeBucket
// dominates GC churn during parse cycles that touch hundreds of buckets in
// quick succession. Reset() is the standard library's intended re-use point.

var gzipReaderPool = sync.Pool{
	New: func() any { return nil }, // gzip.NewReader needs a non-nil reader, so allocate lazily on first Reset
}

// AcquireGzipReader returns a *gzip.Reader bound to r. The caller must call
// ReleaseGzipReader once the reader is drained (typically via defer).
func AcquireGzipReader(r io.Reader) (*gzip.Reader, error) {
	if v := gzipReaderPool.Get(); v != nil {
		gz := v.(*gzip.Reader)
		if err := gz.Reset(r); err != nil {
			// Reader is in an indeterminate state; drop it.
			return nil, err
		}
		return gz, nil
	}
	return gzip.NewReader(r)
}

// ReleaseGzipReader returns gz to the pool after closing it. Safe to call
// with nil. Errors from Close() are ignored — callers that need the error
// should call gz.Close() themselves before releasing.
func ReleaseGzipReader(gz *gzip.Reader) {
	if gz == nil {
		return
	}
	_ = gz.Close()
	gzipReaderPool.Put(gz)
}

var gzipWriterPool = sync.Pool{
	New: func() any { return nil },
}

// AcquireGzipWriter returns a *gzip.Writer bound to w. The caller must call
// ReleaseGzipWriter when finished (after Close()).
func AcquireGzipWriter(w io.Writer) *gzip.Writer {
	if v := gzipWriterPool.Get(); v != nil {
		gz := v.(*gzip.Writer)
		gz.Reset(w)
		return gz
	}
	return gzip.NewWriter(w)
}

// ReleaseGzipWriter returns gz to the pool. The caller must have called Close()
// on the writer before releasing it — pooling an unclosed writer would re-emit
// its tail on the next user's stream.
func ReleaseGzipWriter(gz *gzip.Writer) {
	if gz == nil {
		return
	}
	gzipWriterPool.Put(gz)
}
