// Copyright 2023 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package objstorageprovider

import "github.com/chris124567/pebble/internal/invariants"

const (
	// Constants for dynamic readahead of data blocks. Note that the size values
	// make sense as some multiple of the default block size; and they should
	// both be larger than the default block size.
	minFileReadsForReadahead = 2
	// TODO(bilal): Have the initial size value be a factor of the block size,
	// as opposed to a hardcoded value.
	initialReadaheadSize = 64 << 10 /* 64KB */
)

// readaheadState contains state variables related to readahead. Updated on
// file reads.
type readaheadState struct {
	// Number of sequential reads.
	numReads         int64
	maxReadaheadSize int64
	// Size issued to the next call to Prefetch. Starts at or above
	// initialReadaheadSize and grows exponentially until maxReadaheadSize.
	size int64
	// prevSize is the size used in the last Prefetch call.
	prevSize int64
	// The byte offset up to which the OS has been asked to read ahead / cached.
	// When reading ahead, reads up to this limit should not incur an IO
	// operation. Reads after this limit can benefit from a new call to
	// Prefetch.
	limit int64
}

func makeReadaheadState(maxReadaheadSize int64) readaheadState {
	return readaheadState{
		size:             initialReadaheadSize,
		maxReadaheadSize: maxReadaheadSize,
	}
}

func (rs *readaheadState) recordCacheHit(offset, blockLength int64) {
	_ = rs.maybeReadaheadOrCacheHit(offset, blockLength, false)
}

// maybeReadahead updates state and determines whether to issue a readahead /
// prefetch call for a block read at offset for blockLength bytes.
// Returns a size value (greater than 0) that should be prefetched if readahead
// would be beneficial.
func (rs *readaheadState) maybeReadahead(offset, blockLength int64) int64 {
	return rs.maybeReadaheadOrCacheHit(offset, blockLength, true)
}

// The return value should be ignored if !readahead.
func (rs *readaheadState) maybeReadaheadOrCacheHit(
	offset, blockLength int64, readahead bool,
) int64 {
	if invariants.Enabled && rs.maxReadaheadSize == 0 {
		panic("readaheadState not initialized")
	}
	currentReadEnd := offset + blockLength
	if rs.numReads >= minFileReadsForReadahead {
		// The minimum threshold of sequential reads to justify reading ahead
		// has been reached.
		// There are two intervals: the interval being read:
		// [offset, currentReadEnd]
		// as well as the interval where a read would benefit from read ahead:
		// [rs.limit, rs.limit + rs.size]
		// We increase the latter interval to
		// [rs.limit, rs.limit + rs.maxReadaheadSize] to account for cases where
		// readahead may not be beneficial with a small readahead size, but over
		// time the readahead size would increase exponentially to make it
		// beneficial.
		if currentReadEnd >= rs.limit && offset <= rs.limit+rs.maxReadaheadSize {
			// We are doing a read in the interval ahead of
			// the last readahead range. In the diagrams below, ++++ is the last
			// readahead range, ==== is the range represented by
			// [rs.limit, rs.limit + rs.maxReadaheadSize], and ---- is the range
			// being read.
			//
			//               rs.limit           rs.limit + rs.maxReadaheadSize
			//         ++++++++++|===========================|
			//
			//              |-------------|
			//            offset       currentReadEnd
			//
			// This case is also possible, as are all cases with an overlap
			// between [rs.limit, rs.limit + rs.maxReadaheadSize] and [offset,
			// currentReadEnd]:
			//
			//               rs.limit           rs.limit + rs.maxReadaheadSize
			//         ++++++++++|===========================|
			//
			//                                            |-------------|
			//                                         offset       currentReadEnd
			//
			//
			rs.numReads++
			if readahead {
				rs.limit = offset + rs.size
				rs.prevSize = rs.size
				// Increase rs.size for the next read.
				rs.size *= 2
				if rs.size > rs.maxReadaheadSize {
					rs.size = rs.maxReadaheadSize
				}
			} else {
				// This is a read that would have resulted in a readahead, had it
				// not been a cache hit.
				rs.limit = currentReadEnd
			}
			return rs.prevSize
		}
		if currentReadEnd < rs.limit-rs.prevSize || offset > rs.limit+rs.maxReadaheadSize {
			// We read too far away from rs.limit to benefit from readahead in
			// any scenario. Reset all variables.
			// The case where we read too far ahead:
			//
			// (rs.limit - rs.prevSize)    (rs.limit)   (rs.limit + rs.maxReadaheadSize)
			//                    |+++++++++++++|=============|
			//
			//                                                  |-------------|
			//                                             offset       currentReadEnd
			//
			// Or too far behind:
			//
			// (rs.limit - rs.prevSize)    (rs.limit)   (rs.limit + rs.maxReadaheadSize)
			//                    |+++++++++++++|=============|
			//
			//    |-------------|
			// offset       currentReadEnd
			//
			rs.numReads = 1
			rs.limit = currentReadEnd
			rs.size = initialReadaheadSize
			rs.prevSize = 0

			return 0
		}
		// The previous if-block predicates were all false. This mechanically implies:
		//
		// INVARIANT:
		//  !(currentReadEnd >= rs.limit && offset <= rs.limit+rs.maxReadaheadSize) &&
		//  !(currentReadEnd < rs.limit-rs.prevSize || offset > rs.limit+rs.maxReadaheadSize)
		// Which mechanically simplifies to:
		//  currentReadEnd < rs.limit  && currentReadEnd >= rs.limit-rs.prevSize &&
		//  offset <= rs.limit+rs.maxReadaheadSize
		//
		// So reads in the range [rs.limit - rs.prevSize, rs.limit] end up
		// here. This is a read that is potentially benefitting from a past
		// readahead, but there's no reason to issue a readahead call at the
		// moment.
		//
		// (rs.limit - rs.prevSize)            (rs.limit + rs.maxReadaheadSize)
		//                    |+++++++++++++|===============|
		//                             (rs.limit)
		//
		//                        |-------|
		//                     offset    currentReadEnd
		//
		rs.numReads++
		return 0
	}
	// Not yet at the numReads threshold to justify readahead. But we want to
	// capture whether readahead will be beneficial in the future.
	if currentReadEnd >= rs.limit && offset <= rs.limit+rs.maxReadaheadSize {
		// Blocks are being read sequentially and would benefit from readahead
		// down the line.
		//
		//                       (rs.limit)   (rs.limit + rs.maxReadaheadSize)
		//                         |=============|
		//
		//                    |-------|
		//                offset    currentReadEnd
		//
		rs.numReads++
		// It is possible to fall here when rs.limit has not been initialized. If
		// we don't initialize, rs.limit, it is possible that the first read
		// offset was at rs.limit+rs.maxReadaheadSize-delta and the enclosing
		// if-block predicate was true, and the next read is sequential but has
		// offset > rs.limit+rs.maxReadaheadSize (if we left rs.limit at 0), and
		// the enclosing if-block predicate will be false and we will incorrectly
		// think that readahead is not beneficial. The same issue arises if
		// rs.limit has been initialized and currentReadEnd is advancing past
		// rs.limit.
		rs.limit = currentReadEnd
		return 0
	}
	// We read too far ahead of the last read, or before it. This indicates
	// a random read, where readahead is not desirable. Reset all variables.
	//
	// (rs.limit - rs.maxReadaheadSize)  (rs.limit)   (rs.limit + rs.maxReadaheadSize)
	//                     |+++++++++++++|=============|
	//
	//                                                    |-------|
	//                                                offset    currentReadEnd
	//
	rs.numReads = 1
	rs.limit = currentReadEnd
	rs.size = initialReadaheadSize
	rs.prevSize = 0
	return 0
}
