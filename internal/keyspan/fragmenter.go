// Copyright 2018 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package keyspan

import (
	"fmt"

	"github.com/chris124567/pebble/internal/base"
	"github.com/chris124567/pebble/internal/invariants"
)

// Fragmenter fragments a set of spans such that overlapping spans are
// split at their overlap points. The fragmented spans are output to the
// supplied Output function.
type Fragmenter struct {
	Cmp    base.Compare
	Format base.FormatKey
	// Emit is called to emit a fragmented span and its keys. Every key defined
	// within the emitted Span applies to the entirety of the Span's key span.
	// Keys are ordered in decreasing order of their sequence numbers, and if
	// equal, decreasing order of key kind.
	Emit func(Span)
	// pending contains the list of pending fragments that have not been
	// flushed to the block writer. Note that the spans have not been
	// fragmented on the end keys yet. That happens as the spans are
	// flushed. All pending spans have the same Start.
	pending []Span
	// doneBuf is used to buffer completed span fragments when flushing to a
	// specific key (e.g. TruncateAndFlushTo). It is cached in the Fragmenter to
	// allow reuse.
	doneBuf []Span
	// flushBuf is used to sort keys by (seqnum,kind) before emitting.
	flushBuf []Key
	// flushedKey is the key that fragments have been flushed up to. Any
	// additional spans added to the fragmenter must have a start key >=
	// flushedKey. A nil value indicates flushedKey has not been set.
	flushedKey []byte
	finished   bool
}

func (f *Fragmenter) checkInvariants(buf []Span) {
	for i := 1; i < len(buf); i++ {
		if f.Cmp(buf[i].Start, buf[i].End) >= 0 {
			panic(fmt.Sprintf("pebble: empty pending span invariant violated: %s", buf[i]))
		}
		if f.Cmp(buf[i-1].Start, buf[i].Start) != 0 {
			panic(fmt.Sprintf("pebble: pending span invariant violated: %s %s",
				f.Format(buf[i-1].Start), f.Format(buf[i].Start)))
		}
	}
}

// Add adds a span to the fragmenter. Spans may overlap and the
// fragmenter will internally split them. The spans must be presented in
// increasing start key order. That is, Add must be called with a series
// of spans like:
//
//	a---e
//	  c---g
//	  c-----i
//	         j---n
//	         j-l
//
// We need to fragment the spans at overlap points. In the above
// example, we'd create:
//
//	a-c-e
//	  c-e-g
//	  c-e-g-i
//	         j-l-n
//	         j-l
//
// The fragments need to be output sorted by start key, and for equal start
// keys, sorted by descending sequence number. This last part requires a mild
// bit of care as the fragments are not created in descending sequence number
// order.
//
// Once a start key has been seen, we know that we'll never see a smaller
// start key and can thus flush all of the fragments that lie before that
// start key.
//
// Walking through the example above, we start with:
//
//	a---e
//
// Next we add [c,g) resulting in:
//
//	a-c-e
//	  c---g
//
// The fragment [a,c) is flushed leaving the pending spans as:
//
//	c-e
//	c---g
//
// The next span is [c,i):
//
//	c-e
//	c---g
//	c-----i
//
// No fragments are flushed. The next span is [j,n):
//
//	c-e
//	c---g
//	c-----i
//	       j---n
//
// The fragments [c,e), [c,g) and [c,i) are flushed. We sort these fragments
// by their end key, then split the fragments on the end keys:
//
//	c-e
//	c-e-g
//	c-e---i
//
// The [c,e) fragments all get flushed leaving:
//
//	e-g
//	e---i
//
// This process continues until there are no more fragments to flush.
//
// WARNING: the slices backing Start, End, Keys, Key.Suffix and Key.Value are
// all retained after this method returns and should not be modified. This is
// safe for spans that are added from a memtable or batch. It is partially
// unsafe for a span read from an sstable. Specifically, the Keys slice of a
// Span returned during sstable iteration is only valid until the next iterator
// operation. The stability of the user keys depend on whether the block is
// prefix compressed, and in practice Pebble never prefix compresses range
// deletion and range key blocks, so these keys are stable. Because of this key
// stability, typically callers only need to perform a shallow clone of the Span
// before Add-ing it to the fragmenter.
//
// Add requires the provided span's keys are sorted in InternalKeyTrailer descending order.
func (f *Fragmenter) Add(s Span) {
	if f.finished {
		panic("pebble: span fragmenter already finished")
	} else if s.KeysOrder != ByTrailerDesc {
		panic("pebble: span keys unexpectedly not in trailer descending order")
	}
	if f.flushedKey != nil {
		switch c := f.Cmp(s.Start, f.flushedKey); {
		case c < 0:
			panic(fmt.Sprintf("pebble: start key (%s) < flushed key (%s)",
				f.Format(s.Start), f.Format(f.flushedKey)))
		}
	}
	if f.Cmp(s.Start, s.End) >= 0 {
		// An empty span, we can ignore it.
		return
	}
	if invariants.RaceEnabled {
		f.checkInvariants(f.pending)
		defer func() { f.checkInvariants(f.pending) }()
	}

	if len(f.pending) > 0 {
		// Since all of the pending spans have the same start key, we only need
		// to compare against the first one.
		switch c := f.Cmp(f.pending[0].Start, s.Start); {
		case c > 0:
			panic(fmt.Sprintf("pebble: keys must be added in order: %s > %s",
				f.Format(f.pending[0].Start), f.Format(s.Start)))
		case c == 0:
			// The new span has the same start key as the existing pending
			// spans. Add it to the pending buffer.
			f.pending = append(f.pending, s)
			return
		}

		// At this point we know that the new start key is greater than the pending
		// spans start keys.
		f.truncateAndFlush(s.Start)
	}

	f.pending = append(f.pending, s)
}

// Empty returns true if all fragments added so far have finished flushing.
func (f *Fragmenter) Empty() bool {
	return f.finished || len(f.pending) == 0
}

// Start returns the start key of the first span in the pending buffer, or nil
// if there are no pending spans. The start key of all pending spans is the same
// as that of the first one.
func (f *Fragmenter) Start() []byte {
	if len(f.pending) > 0 {
		return f.pending[0].Start
	}
	return nil
}

// Truncate truncates all pending spans up to key (exclusive), flushes them, and
// retains any spans that continue onward for future flushes.
func (f *Fragmenter) Truncate(key []byte) {
	if len(f.pending) > 0 {
		f.truncateAndFlush(key)
	}
}

// Flushes all pending spans up to key (exclusive).
//
// WARNING: The specified key is stored without making a copy, so all callers
// must ensure it is safe.
func (f *Fragmenter) truncateAndFlush(key []byte) {
	f.flushedKey = append(f.flushedKey[:0], key...)
	done := f.doneBuf[:0]
	pending := f.pending
	f.pending = f.pending[:0]

	// pending and f.pending share the same underlying storage. As we iterate
	// over pending we append to f.pending, but only one entry is appended in
	// each iteration, after we have read the entry being overwritten.
	for _, s := range pending {
		if f.Cmp(key, s.End) < 0 {
			//   s: a--+--e
			// new:    c------
			if f.Cmp(s.Start, key) < 0 {
				done = append(done, Span{
					Start: s.Start,
					End:   key,
					Keys:  s.Keys,
				})
			}
			f.pending = append(f.pending, Span{
				Start: key,
				End:   s.End,
				Keys:  s.Keys,
			})
		} else {
			//   s: a-----e
			// new:       e----
			done = append(done, s)
		}
	}

	f.doneBuf = done[:0]
	f.flush(done, nil)
}

// flush a group of range spans to the block. The spans are required to all have
// the same start key. We flush all span fragments until startKey > lastKey. If
// lastKey is nil, all span fragments are flushed. The specification of a
// non-nil lastKey occurs for range deletion tombstones during compaction where
// we want to flush (but not truncate) all range tombstones that start at or
// before the first key in the next sstable. Consider:
//
//	a---e#10
//	a------h#9
//
// If a compaction splits the sstables at key c we want the first sstable to
// contain the tombstones [a,e)#10 and [a,e)#9. Fragmentation would naturally
// produce a tombstone [e,h)#9, but we don't need to output that tombstone to
// the first sstable.
func (f *Fragmenter) flush(buf []Span, lastKey []byte) {
	if invariants.RaceEnabled {
		f.checkInvariants(buf)
	}

	// Sort the spans by end key. This will allow us to walk over the spans and
	// easily determine the next split point (the smallest end-key).
	SortSpansByEndKey(f.Cmp, buf)

	// Loop over the spans, splitting by end key.
	for len(buf) > 0 {
		// A prefix of spans will end at split. remove represents the count of
		// that prefix.
		remove := 1
		split := buf[0].End
		f.flushBuf = append(f.flushBuf[:0], buf[0].Keys...)

		for i := 1; i < len(buf); i++ {
			if f.Cmp(split, buf[i].End) == 0 {
				remove++
			}
			f.flushBuf = append(f.flushBuf, buf[i].Keys...)
		}

		SortKeysByTrailer(f.flushBuf)

		f.Emit(Span{
			Start: buf[0].Start,
			End:   split,
			// Copy the sorted keys to a new slice.
			//
			// This allocation is an unfortunate side effect of the Fragmenter and
			// the expectation that the spans it produces are available in-memory
			// indefinitely.
			//
			// Eventually, we should be able to replace the fragmenter with the
			// keyspanimpl.MergingIter which will perform just-in-time
			// fragmentation, and only guaranteeing the memory lifetime for the
			// current span. The MergingIter fragments while only needing to
			// access one Span per level. It only accesses the Span at the
			// current position for each level. During compactions, we can write
			// these spans to sstables without retaining previous Spans.
			Keys: append([]Key(nil), f.flushBuf...),
		})

		if lastKey != nil && f.Cmp(split, lastKey) > 0 {
			break
		}

		// Adjust the start key for every remaining span.
		buf = buf[remove:]
		for i := range buf {
			buf[i].Start = split
		}
	}
}

// Finish flushes any remaining fragments to the output. It is an error to call
// this if any other spans will be added.
func (f *Fragmenter) Finish() {
	if f.finished {
		panic("pebble: span fragmenter already finished")
	}
	f.flush(f.pending, nil)
	f.finished = true
}
