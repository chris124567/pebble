// Copyright 2012 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package pebble

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cockroachdb/crlib/fifo"
	"github.com/cockroachdb/datadriven"
	"github.com/cockroachdb/errors"
	"github.com/chris124567/pebble/internal/base"
	"github.com/chris124567/pebble/internal/cache"
	"github.com/chris124567/pebble/internal/testkeys"
	"github.com/chris124567/pebble/internal/testutils"
	"github.com/chris124567/pebble/objstorage/objstorageprovider"
	"github.com/chris124567/pebble/sstable"
	"github.com/chris124567/pebble/sstable/block"
	"github.com/chris124567/pebble/vfs"
	"github.com/chris124567/pebble/vfs/errorfs"
	"github.com/chris124567/pebble/wal"
	"github.com/stretchr/testify/require"
)

// try repeatedly calls f, sleeping between calls with exponential back-off,
// until f returns a nil error or the total sleep time is greater than or equal
// to maxTotalSleep. It always calls f at least once.
func try(initialSleep, maxTotalSleep time.Duration, f func() error) error {
	totalSleep := time.Duration(0)
	for d := initialSleep; ; d *= 2 {
		time.Sleep(d)
		totalSleep += d
		if err := f(); err == nil || totalSleep >= maxTotalSleep {
			return err
		}
	}
}

func TestTry(t *testing.T) {
	c := make(chan struct{})
	go func() {
		time.Sleep(1 * time.Millisecond)
		close(c)
	}()

	attemptsMu := sync.Mutex{}
	attempts := 0

	err := try(100*time.Microsecond, 20*time.Second, func() error {
		attemptsMu.Lock()
		attempts++
		attemptsMu.Unlock()

		select {
		default:
			return errors.New("timed out")
		case <-c:
			return nil
		}
	})
	require.NoError(t, err)

	attemptsMu.Lock()
	a := attempts
	attemptsMu.Unlock()

	if a == 0 {
		t.Fatalf("attempts: got 0, want > 0")
	}
}

func TestBasicReads(t *testing.T) {
	testCases := []struct {
		dirname string
		wantMap map[string]string
	}{
		{
			"db-stage-1",
			map[string]string{
				"aaa":  "",
				"bar":  "",
				"baz":  "",
				"foo":  "",
				"quux": "",
				"zzz":  "",
			},
		},
		{
			"db-stage-2",
			map[string]string{
				"aaa":  "",
				"bar":  "",
				"baz":  "three",
				"foo":  "four",
				"quux": "",
				"zzz":  "",
			},
		},
		{
			"db-stage-3",
			map[string]string{
				"aaa":  "",
				"bar":  "",
				"baz":  "three",
				"foo":  "four",
				"quux": "",
				"zzz":  "",
			},
		},
		{
			"db-stage-4",
			map[string]string{
				"aaa":  "",
				"bar":  "",
				"baz":  "",
				"foo":  "five",
				"quux": "six",
				"zzz":  "",
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.dirname, func(t *testing.T) {
			fs := vfs.NewMem()
			_, err := vfs.Clone(vfs.Default, fs, filepath.Join("testdata", tc.dirname), tc.dirname)
			if err != nil {
				t.Fatalf("%s: cloneFileSystem failed: %v", tc.dirname, err)
			}
			d, err := Open(tc.dirname, testingRandomized(t, &Options{
				FS: fs,
			}))
			if err != nil {
				t.Fatalf("%s: Open failed: %v", tc.dirname, err)
			}
			for key, want := range tc.wantMap {
				got, closer, err := d.Get([]byte(key))
				if err != nil && err != ErrNotFound {
					t.Fatalf("%s: Get(%q) failed: %v", tc.dirname, key, err)
				}
				if string(got) != string(want) {
					t.Fatalf("%s: Get(%q): got %q, want %q", tc.dirname, key, got, want)
				}
				if closer != nil {
					closer.Close()
				}
			}
			err = d.Close()
			if err != nil {
				t.Fatalf("%s: Close failed: %v", tc.dirname, err)
			}
		})
	}
}

func TestBasicWrites(t *testing.T) {
	d, err := Open("", testingRandomized(t, &Options{
		FS: vfs.NewMem(),
	}))
	require.NoError(t, err)

	names := []string{
		"Alatar",
		"Gandalf",
		"Pallando",
		"Radagast",
		"Saruman",
		"Joe",
	}
	wantMap := map[string]string{}

	inBatch, batch, pending := false, &Batch{}, [][]string(nil)
	set0 := func(k, v string) error {
		return d.Set([]byte(k), []byte(v), nil)
	}
	del0 := func(k string) error {
		return d.Delete([]byte(k), nil)
	}
	set1 := func(k, v string) error {
		batch.Set([]byte(k), []byte(v), nil)
		return nil
	}
	del1 := func(k string) error {
		batch.Delete([]byte(k), nil)
		return nil
	}
	set, del := set0, del0

	testCases := []string{
		"set Gandalf Grey",
		"set Saruman White",
		"set Radagast Brown",
		"delete Saruman",
		"set Gandalf White",
		"batch",
		"  set Alatar AliceBlue",
		"apply",
		"delete Pallando",
		"set Alatar AntiqueWhite",
		"set Pallando PapayaWhip",
		"batch",
		"apply",
		"set Pallando PaleVioletRed",
		"batch",
		"  delete Alatar",
		"  set Gandalf GhostWhite",
		"  set Saruman Seashell",
		"  delete Saruman",
		"  set Saruman SeaGreen",
		"  set Radagast RosyBrown",
		"  delete Pallando",
		"apply",
		"delete Radagast",
		"delete Radagast",
		"delete Radagast",
		"set Gandalf Goldenrod",
		"set Pallando PeachPuff",
		"batch",
		"  delete Joe",
		"  delete Saruman",
		"  delete Radagast",
		"  delete Pallando",
		"  delete Gandalf",
		"  delete Alatar",
		"apply",
		"set Joe Plumber",
	}
	for i, tc := range testCases {
		s := strings.Split(strings.TrimSpace(tc), " ")
		switch s[0] {
		case "set":
			if err := set(s[1], s[2]); err != nil {
				t.Fatalf("#%d %s: %v", i, tc, err)
			}
			if inBatch {
				pending = append(pending, s)
			} else {
				wantMap[s[1]] = s[2]
			}
		case "delete":
			if err := del(s[1]); err != nil {
				t.Fatalf("#%d %s: %v", i, tc, err)
			}
			if inBatch {
				pending = append(pending, s)
			} else {
				delete(wantMap, s[1])
			}
		case "batch":
			inBatch, batch, set, del = true, &Batch{}, set1, del1
		case "apply":
			if err := d.Apply(batch, nil); err != nil {
				t.Fatalf("#%d %s: %v", i, tc, err)
			}
			for _, p := range pending {
				switch p[0] {
				case "set":
					wantMap[p[1]] = p[2]
				case "delete":
					delete(wantMap, p[1])
				}
			}
			inBatch, pending, set, del = false, nil, set0, del0
		default:
			t.Fatalf("#%d %s: bad test case: %q", i, tc, s)
		}

		fail := false
		for _, name := range names {
			g, closer, err := d.Get([]byte(name))
			if err != nil && err != ErrNotFound {
				t.Errorf("#%d %s: Get(%q): %v", i, tc, name, err)
				fail = true
			}
			got, gOK := string(g), err == nil
			want, wOK := wantMap[name]
			if got != want || gOK != wOK {
				t.Errorf("#%d %s: Get(%q): got %q, %t, want %q, %t",
					i, tc, name, got, gOK, want, wOK)
				fail = true
			}
			if closer != nil {
				closer.Close()
			}
		}
		if fail {
			return
		}
	}

	require.NoError(t, d.Close())
}

func TestRandomWrites(t *testing.T) {
	d, err := Open("", testingRandomized(t, &Options{
		FS:           vfs.NewMem(),
		MemTableSize: 8 * 1024,
	}))
	require.NoError(t, err)

	keys := [64][]byte{}
	wants := [64]int{}
	for k := range keys {
		keys[k] = []byte(strconv.Itoa(k))
		wants[k] = -1
	}
	xxx := bytes.Repeat([]byte("x"), 512)

	rng := rand.New(rand.NewPCG(0, 123))
	const N = 1000
	for i := 0; i < N; i++ {
		k := rng.IntN(len(keys))
		if rng.IntN(20) != 0 {
			wants[k] = rng.IntN(len(xxx) + 1)
			if err := d.Set(keys[k], xxx[:wants[k]], nil); err != nil {
				t.Fatalf("i=%d: Set: %v", i, err)
			}
		} else {
			wants[k] = -1
			if err := d.Delete(keys[k], nil); err != nil {
				t.Fatalf("i=%d: Delete: %v", i, err)
			}
		}

		if i != N-1 || rng.IntN(50) != 0 {
			continue
		}
		for k := range keys {
			got := -1
			if v, closer, err := d.Get(keys[k]); err != nil {
				if err != ErrNotFound {
					t.Fatalf("Get: %v", err)
				}
			} else {
				got = len(v)
				closer.Close()
			}
			if got != wants[k] {
				t.Errorf("i=%d, k=%d: got %d, want %d", i, k, got, wants[k])
			}
		}
	}

	require.NoError(t, d.Close())
}

func TestLargeBatch(t *testing.T) {
	d, err := Open("", testingRandomized(t, &Options{
		FS:                          vfs.NewMem(),
		MemTableSize:                1400,
		MemTableStopWritesThreshold: 100,
	}))
	require.NoError(t, err)

	verifyLSM := func(expected string) func() error {
		return func() error {
			d.mu.Lock()
			s := d.mu.versions.currentVersion().String()
			d.mu.Unlock()
			if expected != s {
				if testing.Verbose() {
					fmt.Println(strings.TrimSpace(s))
				}
				return errors.Errorf("expected %s, but found %s", expected, s)
			}
			return nil
		}
	}

	getLatestLog := func() wal.LogicalLog {
		d.mu.Lock()
		defer d.mu.Unlock()
		logs := d.mu.log.manager.List()
		return logs[len(logs)-1]
	}
	memTableCreationSeqNum := func() base.SeqNum {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.mu.mem.mutable.logSeqNum
	}

	startLog := getLatestLog()
	startLogStartSize, err := startLog.PhysicalSize()
	require.NoError(t, err)
	startSeqNum := d.mu.versions.logSeqNum.Load()

	// Write a key with a value larger than the memtable size.
	require.NoError(t, d.Set([]byte("a"), bytes.Repeat([]byte("a"), 512), nil))

	// Verify that the large batch was written to the WAL that existed before it
	// was committed. We verify that WAL rotation occurred, where the large batch
	// was written to, and that the new WAL is empty.
	endLog := getLatestLog()
	if startLog.Num == endLog.Num {
		t.Fatal("expected WAL rotation")
	}
	startLogEndSize, err := startLog.PhysicalSize()
	require.NoError(t, err)
	if startLogEndSize == startLogStartSize {
		t.Fatalf("expected large batch to be written to %s.log, but file size unchanged at %d",
			startLog.Num, startLogEndSize)
	}
	endLogSize, err := endLog.PhysicalSize()
	require.NoError(t, err)
	if endLogSize != 0 {
		t.Fatalf("expected %s to be empty, but found %d", endLog, endLogSize)
	}
	if creationSeqNum := memTableCreationSeqNum(); creationSeqNum <= startSeqNum {
		t.Fatalf("expected memTable.logSeqNum=%d > largeBatch.seqNum=%d", creationSeqNum, startSeqNum)
	}

	// Verify this results in one L0 table being created.
	require.NoError(t, try(100*time.Microsecond, 20*time.Second,
		verifyLSM("L0.0:\n  000005:[a#10,SET-a#10,SET]\n")))

	require.NoError(t, d.Set([]byte("b"), bytes.Repeat([]byte("b"), 512), nil))

	// Verify this results in a second L0 table being created.
	require.NoError(t, try(100*time.Microsecond, 20*time.Second,
		verifyLSM("L0.0:\n  000005:[a#10,SET-a#10,SET]\n  000007:[b#11,SET-b#11,SET]\n")))

	// Allocate a bunch of batches to exhaust the batchPool. None of these
	// batches should have a non-zero count.
	for i := 0; i < 10; i++ {
		b := d.NewBatch()
		require.EqualValues(t, 0, b.Count())
	}

	require.NoError(t, d.Close())
}

func TestGetNoCache(t *testing.T) {
	cache := NewCache(0)
	defer cache.Unref()

	d, err := Open("", testingRandomized(t, &Options{
		Cache: cache,
		FS:    vfs.NewMem(),
	}))
	require.NoError(t, err)

	require.NoError(t, d.Set([]byte("a"), []byte("aa"), nil))
	require.NoError(t, d.Flush())
	verifyGet(t, d, []byte("a"), []byte("aa"))

	require.NoError(t, d.Close())
}

func TestGetMerge(t *testing.T) {
	d, err := Open("", testingRandomized(t, &Options{
		FS: vfs.NewMem(),
	}))
	require.NoError(t, err)

	key := []byte("a")
	verify := func(expected string) {
		val, closer, err := d.Get(key)
		require.NoError(t, err)

		if expected != string(val) {
			t.Fatalf("expected %s, but got %s", expected, val)
		}
		closer.Close()
	}

	const val = "1"
	for i := 1; i <= 3; i++ {
		require.NoError(t, d.Merge(key, []byte(val), nil))

		expected := strings.Repeat(val, i)
		verify(expected)

		require.NoError(t, d.Flush())
		verify(expected)
	}

	require.NoError(t, d.Close())
}

func TestMergeOrderSameAfterFlush(t *testing.T) {
	// Ensure compaction iterator (used by flush) and user iterator process merge
	// operands in the same order
	d, err := Open("", testingRandomized(t, &Options{
		FS: vfs.NewMem(),
	}))
	require.NoError(t, err)

	key := []byte("a")
	verify := func(expected string) {
		iter, _ := d.NewIter(nil)
		if !iter.SeekGE([]byte("a")) {
			t.Fatal("expected one value, but got empty iterator")
		}
		if expected != string(iter.Value()) {
			t.Fatalf("expected %s, but got %s", expected, string(iter.Value()))
		}
		if !iter.SeekLT([]byte("b")) {
			t.Fatal("expected one value, but got empty iterator")
		}
		if expected != string(iter.Value()) {
			t.Fatalf("expected %s, but got %s", expected, string(iter.Value()))
		}
		require.NoError(t, iter.Close())
	}

	require.NoError(t, d.Merge(key, []byte("0"), nil))
	require.NoError(t, d.Merge(key, []byte("1"), nil))

	verify("01")
	require.NoError(t, d.Flush())
	verify("01")

	require.NoError(t, d.Close())
}

type closableMerger struct {
	lastBuf []byte
	closed  bool
}

func (m *closableMerger) MergeNewer(value []byte) error {
	m.lastBuf = append(m.lastBuf[:0], value...)
	return nil
}

func (m *closableMerger) MergeOlder(value []byte) error {
	m.lastBuf = append(m.lastBuf[:0], value...)
	return nil
}

func (m *closableMerger) Finish(includesBase bool) ([]byte, io.Closer, error) {
	return m.lastBuf, m, nil
}

func (m *closableMerger) Close() error {
	m.closed = true
	return nil
}

func TestMergerClosing(t *testing.T) {
	m := &closableMerger{}

	d, err := Open("", testingRandomized(t, &Options{
		FS: vfs.NewMem(),
		Merger: &Merger{
			Merge: func(key, value []byte) (base.ValueMerger, error) {
				return m, m.MergeNewer(value)
			},
		},
	}))
	require.NoError(t, err)

	defer func() {
		require.NoError(t, d.Close())
	}()

	err = d.Merge([]byte("a"), []byte("b"), nil)
	require.NoError(t, err)
	require.False(t, m.closed)

	val, closer, err := d.Get([]byte("a"))
	require.NoError(t, err)
	require.Equal(t, []byte("b"), val)
	require.NotNil(t, closer)
	require.False(t, m.closed)
	_ = closer.Close()
	require.True(t, m.closed)
}

func TestLogData(t *testing.T) {
	d, err := Open("", testingRandomized(t, &Options{
		FS: vfs.NewMem(),
	}))
	require.NoError(t, err)

	defer func() {
		require.NoError(t, d.Close())
	}()

	require.NoError(t, d.LogData([]byte("foo"), Sync))
	require.NoError(t, d.LogData([]byte("bar"), Sync))
	// TODO(itsbilal): Confirm that we wrote some bytes to the WAL.
	// For now, LogData proceeding ahead without a panic is good enough.
}

func TestSingleDeleteGet(t *testing.T) {
	d, err := Open("", testingRandomized(t, &Options{
		FS: vfs.NewMem(),
	}))
	require.NoError(t, err)
	defer func() {
		require.NoError(t, d.Close())
	}()

	key := []byte("key")
	val := []byte("val")

	require.NoError(t, d.Set(key, val, nil))
	verifyGet(t, d, key, val)

	key2 := []byte("key2")
	val2 := []byte("val2")

	require.NoError(t, d.Set(key2, val2, nil))
	verifyGet(t, d, key2, val2)

	require.NoError(t, d.SingleDelete(key2, nil))
	verifyGetNotFound(t, d, key2)
}

func TestSingleDeleteFlush(t *testing.T) {
	d, err := Open("", testingRandomized(t, &Options{
		FS: vfs.NewMem(),
	}))
	require.NoError(t, err)
	defer func() {
		require.NoError(t, d.Close())
	}()

	key := []byte("key")
	valFirst := []byte("first")
	valSecond := []byte("second")
	key2 := []byte("key2")
	val2 := []byte("val2")

	require.NoError(t, d.Set(key, valFirst, nil))
	require.NoError(t, d.Set(key2, val2, nil))
	require.NoError(t, d.Flush())

	require.NoError(t, d.SingleDelete(key, nil))
	require.NoError(t, d.Set(key, valSecond, nil))
	require.NoError(t, d.Delete(key2, nil))
	require.NoError(t, d.Set(key2, val2, nil))
	require.NoError(t, d.Flush())

	require.NoError(t, d.SingleDelete(key, nil))
	require.NoError(t, d.Delete(key2, nil))
	require.NoError(t, d.Flush())

	verifyGetNotFound(t, d, key)
	verifyGetNotFound(t, d, key2)
}

func TestUnremovableSingleDelete(t *testing.T) {
	d, err := Open("", testingRandomized(t, &Options{
		FS:                    vfs.NewMem(),
		L0CompactionThreshold: 8,
	}))
	require.NoError(t, err)
	defer func() {
		require.NoError(t, d.Close())
	}()

	key := []byte("key")
	valFirst := []byte("valFirst")
	valSecond := []byte("valSecond")

	require.NoError(t, d.Set(key, valFirst, nil))
	ss := d.NewSnapshot()
	defer ss.Close()
	require.NoError(t, d.SingleDelete(key, nil))
	require.NoError(t, d.Set(key, valSecond, nil))
	require.NoError(t, d.Flush())

	verifyGet(t, ss, key, valFirst)
	verifyGet(t, d, key, valSecond)

	require.NoError(t, d.SingleDelete(key, nil))

	verifyGet(t, ss, key, valFirst)
	verifyGetNotFound(t, d, key)

	require.NoError(t, d.Flush())

	verifyGet(t, ss, key, valFirst)
	verifyGetNotFound(t, d, key)
}

func TestIterLeak(t *testing.T) {
	for _, leak := range []bool{true, false} {
		t.Run(fmt.Sprintf("leak=%t", leak), func(t *testing.T) {
			for _, flush := range []bool{true, false} {
				t.Run(fmt.Sprintf("flush=%t", flush), func(t *testing.T) {
					fc := NewFileCache(10, 100)
					d, err := Open("", testingRandomized(t, &Options{
						FS:        vfs.NewMem(),
						FileCache: fc,
					}))
					require.NoError(t, err)

					require.NoError(t, d.Set([]byte("a"), []byte("a"), nil))
					if flush {
						require.NoError(t, d.Flush())
					}
					iter, _ := d.NewIter(nil)
					iter.First()
					if !leak {
						require.NoError(t, iter.Close())
						require.NoError(t, d.Close())
					} else {
						if err := d.Close(); err == nil {
							t.Fatalf("expected failure, but found success")
						} else if !strings.HasPrefix(err.Error(), "leaked iterators:") {
							t.Fatalf("expected leaked iterators, but found %+v", err)
						} else {
							t.Log(err.Error())
						}
						iter.Close()
					}
					fc.Unref()
				})
			}
		})
	}
}

// Make sure that we detect an iter leak when only one DB closes
// while the second db still holds a reference to the FileCache.
func TestIterLeakSharedCache(t *testing.T) {
	for _, leak := range []bool{true, false} {
		t.Run(fmt.Sprintf("leak=%t", leak), func(t *testing.T) {
			for _, flush := range []bool{true, false} {
				t.Run(fmt.Sprintf("flush=%t", flush), func(t *testing.T) {
					fc := NewFileCache(10, 100)
					d1, err := Open("", &Options{
						FS:        vfs.NewMem(),
						FileCache: fc,
					})
					require.NoError(t, err)

					d2, err := Open("", &Options{
						FS:        vfs.NewMem(),
						FileCache: fc,
					})
					require.NoError(t, err)

					require.NoError(t, d1.Set([]byte("a"), []byte("a"), nil))
					if flush {
						require.NoError(t, d1.Flush())
					}

					require.NoError(t, d2.Set([]byte("a"), []byte("a"), nil))
					if flush {
						require.NoError(t, d2.Flush())
					}

					// Check if leak detection works with only one db closing.
					{
						iter1, _ := d1.NewIter(nil)
						iter1.First()
						if !leak {
							require.NoError(t, iter1.Close())
							require.NoError(t, d1.Close())
						} else {
							defer iter1.Close()
							if err := d1.Close(); err == nil {
								t.Fatalf("expected failure, but found success")
							} else if !strings.HasPrefix(err.Error(), "leaked iterators:") {
								t.Fatalf("expected leaked iterators, but found %+v", err)
							} else {
								t.Log(err.Error())
							}
						}
					}

					{
						iter2, _ := d2.NewIter(nil)
						iter2.First()
						if !leak {
							require.NoError(t, iter2.Close())
							require.NoError(t, d2.Close())
						} else {
							defer iter2.Close()
							if err := d2.Close(); err == nil {
								t.Fatalf("expected failure, but found success")
							} else if !strings.HasPrefix(err.Error(), "leaked iterators:") {
								t.Fatalf("expected leaked iterators, but found %+v", err)
							} else {
								t.Log(err.Error())
							}
						}
					}

					if !leak {
						fc.Unref()
					} else if flush {
						require.Panics(t, func() {
							fc.Unref()
						})
					} else {
						// When we're not flushing and we leak an iterator, Unref might or
						// might not panic, depending on whether there was a file involved.
						func() {
							defer func() {
								recover()
							}()
							fc.Unref()
						}()
					}
				})
			}
		})
	}
}

func TestMemTableReservation(t *testing.T) {
	opts := &Options{
		Cache: NewCache(128 << 10 /* 128 KB */),
		// We're going to be looking at and asserting the global memtable reservation
		// amount below so we don't want to race with any triggered stats collections.
		DisableTableStats: true,
		MemTableSize:      initialMemTableSize,
		FS:                vfs.NewMem(),
	}
	defer opts.Cache.Unref()
	opts.testingRandomized(t)
	opts.EnsureDefaults()

	// Add a block to the cache. Note that the memtable size is larger than the
	// cache size, so opening the DB should cause this block to be evicted.
	helloWorld := []byte("hello world")
	value := cache.Alloc(len(helloWorld))
	copy(value.RawBuffer(), helloWorld)
	tmpHandle := opts.Cache.NewHandle()
	defer tmpHandle.Close()
	tmpHandle.Set(base.DiskFileNum(0), 0, value)
	value.Release()

	d, err := Open("", opts)
	require.NoError(t, err)

	checkReserved := func(expected int64) {
		t.Helper()
		if reserved := d.memTableReserved.Load(); expected != reserved {
			t.Fatalf("expected %d reserved, but found %d", expected, reserved)
		}
	}

	checkReserved(int64(opts.MemTableSize))
	if refs := d.mu.mem.queue[len(d.mu.mem.queue)-1].readerRefs.Load(); refs != 2 {
		t.Fatalf("expected 2 refs, but found %d", refs)
	}
	// Verify the memtable reservation has caused our test block to be evicted.
	if cv := tmpHandle.Get(base.DiskFileNum(0), 0); cv != nil {
		t.Fatalf("expected failure, but found success: %#v", cv)
	}

	// Flush the memtable. The memtable reservation should double because old
	// memtable will be recycled, saved for the next memtable allocation.
	require.NoError(t, d.Flush())
	checkReserved(int64(2 * opts.MemTableSize))
	// Flush again. The memtable reservation should be unchanged because at most
	// 1 memtable may be preserved for recycling.

	// Flush in the presence of an active iterator. The iterator will hold a
	// reference to a readState which will in turn hold a reader reference to the
	// memtable.
	iter, _ := d.NewIter(nil)
	require.NoError(t, d.Flush())
	// The flush moved the recycled memtable into position as an active mutable
	// memtable. There are now two allocated memtables: 1 mutable and 1 pinned
	// by the iterator's read state.
	checkReserved(2 * int64(opts.MemTableSize))

	// Flushing again should increase the reservation total to 3x: 1 active
	// mutable, 1 for recycling, 1 pinned by iterator's read state.
	require.NoError(t, d.Flush())
	checkReserved(3 * int64(opts.MemTableSize))

	// Closing the iterator will release the iterator's read state, and the old
	// memtable will be moved into position as the next memtable to recycle.
	// There was already a memtable ready to be recycled, so that memtable will
	// be freed and the overall reservation total is reduced to 2x.
	require.NoError(t, iter.Close())
	checkReserved(2 * int64(opts.MemTableSize))

	require.NoError(t, d.Close())
}

func TestMemTableReservationLeak(t *testing.T) {
	d, err := Open("", &Options{FS: vfs.NewMem()})
	require.NoError(t, err)

	d.mu.Lock()
	last := d.mu.mem.queue[len(d.mu.mem.queue)-1]
	last.readerRef()
	defer func() {
		last.readerUnref(true)
	}()
	d.mu.Unlock()
	if err := d.Close(); err == nil {
		t.Fatalf("expected failure, but found success")
	} else if !strings.HasPrefix(err.Error(), "leaked memtable reservation:") {
		t.Fatalf("expected leaked memtable reservation, but found %+v", err)
	} else {
		t.Log(err.Error())
	}
}

func TestCacheEvict(t *testing.T) {
	cache := NewCache(10 << 20)
	defer cache.Unref()

	d, err := Open("", &Options{
		Cache: cache,
		FS:    vfs.NewMem(),
	})
	require.NoError(t, err)

	for i := 0; i < 1000; i++ {
		key := []byte(fmt.Sprintf("%04d", i))
		require.NoError(t, d.Set(key, key, nil))
	}

	require.NoError(t, d.Flush())
	iter, _ := d.NewIter(nil)
	for iter.First(); iter.Valid(); iter.Next() {
	}
	require.NoError(t, iter.Close())

	if size := cache.Size(); size == 0 {
		t.Fatalf("expected non-zero cache size")
	}

	for i := 0; i < 1000; i++ {
		key := []byte(fmt.Sprintf("%04d", i))
		require.NoError(t, d.Delete(key, nil))
	}

	require.NoError(t, d.Compact(context.Background(), []byte("0"), []byte("1"), false))

	require.NoError(t, d.Close())

	if size := cache.Size(); size != 0 {
		t.Fatalf("expected empty cache, but found %d", size)
	}
}

func TestFlushEmpty(t *testing.T) {
	d, err := Open("", testingRandomized(t, &Options{
		FS: vfs.NewMem(),
	}))
	require.NoError(t, err)

	// Flushing an empty memtable should not fail.
	require.NoError(t, d.Flush())
	require.NoError(t, d.Close())
}

func TestRollManifest(t *testing.T) {
	toPreserve := rand.Int32N(5) + 1
	opts := &Options{
		MaxManifestFileSize:   1,
		L0CompactionThreshold: 10,
		L0StopWritesThreshold: 1000,
		FS:                    vfs.NewMem(),
		NumPrevManifest:       int(toPreserve),
	}
	opts.DisableAutomaticCompactions = true
	opts.testingRandomized(t)
	d, err := Open("", opts)
	require.NoError(t, err)

	manifestFileNumber := func() base.DiskFileNum {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.mu.versions.manifestFileNum
	}
	sizeRolloverState := func() (int64, int64) {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.mu.versions.rotationHelper.DebugInfo()
	}

	current := func() string {
		desc, err := Peek(d.dirname, d.opts.FS)
		require.NoError(t, err)
		return desc.ManifestFilename
	}

	lastManifestNum := manifestFileNumber()
	manifestNums := []base.DiskFileNum{lastManifestNum}
	for i := 0; i < 5; i++ {
		// MaxManifestFileSize is 1, but the rollover logic also counts edits
		// since the last snapshot to decide on rollover, so do as many flushes as
		// it demands.
		lastSnapshotCount, editsSinceSnapshotCount := sizeRolloverState()
		var expectedLastSnapshotCount, expectedEditsSinceSnapshotCount int64
		switch i {
		case 0:
			// DB is empty.
			expectedLastSnapshotCount, expectedEditsSinceSnapshotCount = 0, 0
		case 1:
			// First edit that caused rollover is not in the snapshot.
			expectedLastSnapshotCount, expectedEditsSinceSnapshotCount = 0, 1
		case 2:
			// One flush is in the snapshot. One flush in the edit.
			expectedLastSnapshotCount, expectedEditsSinceSnapshotCount = 1, 1
		case 3:
			// Two flushes in the snapshot. One flush in the edit. Will need to do
			// two more flushes, the first of which will be in the next snapshot.
			expectedLastSnapshotCount, expectedEditsSinceSnapshotCount = 2, 1
		case 4:
			// Four flushes in the snapshot. One flush in the edit. Will need to do
			// four more flushes, three of which will be in the snapshot.
			expectedLastSnapshotCount, expectedEditsSinceSnapshotCount = 4, 1
		}
		require.Equal(t, expectedLastSnapshotCount, lastSnapshotCount)
		require.Equal(t, expectedEditsSinceSnapshotCount, editsSinceSnapshotCount)
		// Number of flushes to do to trigger the rollover.
		steps := int(lastSnapshotCount - editsSinceSnapshotCount + 1)
		// Steps can be <= 0, but we need to do at least one edit to trigger the
		// rollover logic.
		if steps <= 0 {
			steps = 1
		}
		for j := 0; j < steps; j++ {
			require.NoError(t, d.Set([]byte("a"), nil, nil))
			require.NoError(t, d.Flush())
		}
		d.TestOnlyWaitForCleaning()
		num := manifestFileNumber()
		if lastManifestNum == num {
			t.Fatalf("manifest failed to roll %d: %d == %d", i, lastManifestNum, num)
		}

		manifestNums = append(manifestNums, num)
		lastManifestNum = num

		expectedCurrent := fmt.Sprintf("MANIFEST-%s", lastManifestNum)
		if v := current(); expectedCurrent != v {
			t.Fatalf("expected %s, but found %s", expectedCurrent, v)
		}
	}
	lastSnapshotCount, editsSinceSnapshotCount := sizeRolloverState()
	require.EqualValues(t, 8, lastSnapshotCount)
	require.EqualValues(t, 1, editsSinceSnapshotCount)

	files, err := d.opts.FS.List("")
	require.NoError(t, err)

	var manifests []string
	for _, filename := range files {
		fileType, _, ok := base.ParseFilename(d.opts.FS, filename)
		if !ok {
			continue
		}
		if fileType == base.FileTypeManifest {
			manifests = append(manifests, filename)
		}
	}
	slices.Sort(manifests)

	var expected []string
	for i := len(manifestNums) - int(toPreserve) - 1; i < len(manifestNums); i++ {
		expected = append(
			expected,
			fmt.Sprintf("MANIFEST-%s", manifestNums[i]),
		)
	}
	require.EqualValues(t, expected, manifests)

	// Test the logic that uses the future snapshot size to rollover.
	// Reminder: we have a snapshot with 8 files and the manifest has 1 edit
	// (flush) with 1 file.
	// Add 8 more files with a different key.
	lastManifestNum = manifestFileNumber()
	for j := 0; j < 8; j++ {
		require.NoError(t, d.Set([]byte("c"), nil, nil))
		require.NoError(t, d.Flush())
	}
	lastSnapshotCount, editsSinceSnapshotCount = sizeRolloverState()
	// Need 16 more files in edits to trigger a rollover.
	require.EqualValues(t, 16, lastSnapshotCount)
	require.EqualValues(t, 1, editsSinceSnapshotCount)
	require.NotEqual(t, manifestFileNumber(), lastManifestNum)
	lastManifestNum = manifestFileNumber()
	// Do a compaction that moves 8 of the files from L0 to 1 file in L6. This
	// adds 9 files in edits. We still need 6 more files in edits based on the
	// last snapshot. But the current version has only 9 L0 files and 1 L6 file,
	// for a total of 10 files. So 1 flush should push us over that threshold.
	d.Compact(context.Background(), []byte("c"), []byte("d"), false)
	lastSnapshotCount, editsSinceSnapshotCount = sizeRolloverState()
	require.EqualValues(t, 16, lastSnapshotCount)
	require.EqualValues(t, 10, editsSinceSnapshotCount)
	require.Equal(t, manifestFileNumber(), lastManifestNum)
	require.NoError(t, d.Set([]byte("c"), nil, nil))
	require.NoError(t, d.Flush())
	lastSnapshotCount, editsSinceSnapshotCount = sizeRolloverState()
	require.EqualValues(t, 10, lastSnapshotCount)
	require.EqualValues(t, 1, editsSinceSnapshotCount)
	require.NotEqual(t, manifestFileNumber(), lastManifestNum)

	require.NoError(t, d.Close())
}

func TestDBClosed(t *testing.T) {
	d, err := Open("", &Options{
		FS: vfs.NewMem(),
	})
	require.NoError(t, err)
	require.NoError(t, d.Close())

	catch := func(f func()) (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = r.(error)
			}
		}()
		f()
		return nil
	}

	require.True(t, errors.Is(catch(func() { _ = d.Close() }), ErrClosed))

	require.True(t, errors.Is(catch(func() { _ = d.Compact(context.Background(), nil, nil, false) }), ErrClosed))
	require.True(t, errors.Is(catch(func() { _ = d.Flush() }), ErrClosed))
	require.True(t, errors.Is(catch(func() { _, _ = d.AsyncFlush() }), ErrClosed))

	require.True(t, errors.Is(catch(func() { _, _, _ = d.Get(nil) }), ErrClosed))
	require.True(t, errors.Is(catch(func() { _ = d.Delete(nil, nil) }), ErrClosed))
	require.True(t, errors.Is(catch(func() { _ = d.DeleteRange(nil, nil, nil) }), ErrClosed))
	require.True(t, errors.Is(catch(func() { _ = d.Ingest(context.Background(), nil) }), ErrClosed))
	require.True(t, errors.Is(catch(func() { _ = d.LogData(nil, nil) }), ErrClosed))
	require.True(t, errors.Is(catch(func() { _ = d.Merge(nil, nil, nil) }), ErrClosed))
	require.True(t, errors.Is(catch(func() { _ = d.RatchetFormatMajorVersion(internalFormatNewest) }), ErrClosed))
	require.True(t, errors.Is(catch(func() { _ = d.Set(nil, nil, nil) }), ErrClosed))

	require.True(t, errors.Is(catch(func() { _ = d.NewSnapshot() }), ErrClosed))

	b := d.NewIndexedBatch()
	require.True(t, errors.Is(catch(func() { _ = b.Commit(nil) }), ErrClosed))
	require.True(t, errors.Is(catch(func() { _ = d.Apply(b, nil) }), ErrClosed))
	require.True(t, errors.Is(catch(func() { _, _ = b.NewIter(nil) }), ErrClosed))
}

func TestDBConcurrentCommitCompactFlush(t *testing.T) {
	d, err := Open("", testingRandomized(t, &Options{
		FS: vfs.NewMem(),
	}))
	require.NoError(t, err)

	// Concurrently commit, compact, and flush in order to stress the locking around
	// those operations.
	const n = 1000
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_ = d.Set([]byte(fmt.Sprint(i)), nil, nil)
			var err error
			switch i % 3 {
			case 0:
				err = d.Compact(context.Background(), nil, []byte("\xff"), false)
			case 1:
				err = d.Flush()
			case 2:
				_, err = d.AsyncFlush()
			}
			require.NoError(t, err)
		}(i)
	}
	wg.Wait()

	require.NoError(t, d.Close())
}

func TestDBConcurrentCompactClose(t *testing.T) {
	// Test closing while a compaction is ongoing. This ensures compaction code
	// detects the close and finishes cleanly.
	mem := vfs.NewMem()
	for i := 0; i < 100; i++ {
		opts := &Options{
			FS: mem,
			CompactionConcurrencyRange: func() (int, int) {
				return 1, 2
			},
		}
		d, err := Open("", testingRandomized(t, opts))
		require.NoError(t, err)

		// Ingest a series of files containing a single key each. As the outer
		// loop progresses, these ingestions will build up compaction debt
		// causing compactions to be running concurrently with the close below.
		for j := 0; j < 10; j++ {
			path := fmt.Sprintf("ext%d", j)
			f, err := mem.Create(path, vfs.WriteCategoryUnspecified)
			require.NoError(t, err)
			w := sstable.NewWriter(objstorageprovider.NewFileWritable(f), sstable.WriterOptions{
				TableFormat: d.TableFormat(),
			})
			require.NoError(t, w.Set([]byte(fmt.Sprint(j)), nil))
			require.NoError(t, w.Close())
			require.NoError(t, d.Ingest(context.Background(), []string{path}))
		}

		require.NoError(t, d.Close())
	}
}

func TestDBApplyBatchNilDB(t *testing.T) {
	d, err := Open("", &Options{FS: vfs.NewMem()})
	require.NoError(t, err)

	b1 := &Batch{}
	b1.Set([]byte("test"), nil, nil)

	b2 := &Batch{}
	b2.Apply(b1, nil)
	if b2.memTableSize != 0 {
		t.Fatalf("expected memTableSize to not be set")
	}
	require.NoError(t, d.Apply(b2, nil))
	if b1.memTableSize != b2.memTableSize {
		t.Fatalf("expected memTableSize %d, but found %d", b1.memTableSize, b2.memTableSize)
	}

	require.NoError(t, d.Close())
}

func TestDBApplyBatchMismatch(t *testing.T) {
	srcDB, err := Open("", &Options{FS: vfs.NewMem()})
	require.NoError(t, err)

	applyDB, err := Open("", &Options{FS: vfs.NewMem()})
	require.NoError(t, err)

	err = func() (err error) {
		defer func() {
			if v := recover(); v != nil {
				err = errors.Errorf("%v", v)
			}
		}()

		b := srcDB.NewBatch()
		b.Set([]byte("test"), nil, nil)
		return applyDB.Apply(b, nil)
	}()
	if err == nil || !strings.Contains(err.Error(), "pebble: batch db mismatch:") {
		t.Fatalf("expected error, but found %v", err)
	}

	require.NoError(t, srcDB.Close())
	require.NoError(t, applyDB.Close())
}

func TestCloseCleanerRace(t *testing.T) {
	mem := vfs.NewMem()
	for i := 0; i < 20; i++ {
		db, err := Open("", testingRandomized(t, &Options{FS: mem}))
		require.NoError(t, err)
		require.NoError(t, db.Set([]byte("a"), []byte("something"), Sync))
		require.NoError(t, db.Flush())
		// Ref the sstables so cannot be deleted.
		it, _ := db.NewIter(nil)
		require.NotNil(t, it)
		require.NoError(t, db.DeleteRange([]byte("a"), []byte("b"), Sync))
		require.NoError(t, db.Compact(context.Background(), []byte("a"), []byte("b"), false))
		// Only the iterator is keeping the sstables alive.
		files, err := mem.List("/")
		require.NoError(t, err)
		var found bool
		for _, f := range files {
			if strings.HasSuffix(f, ".sst") {
				found = true
				break
			}
		}
		require.True(t, found)
		// Close the iterator and the db in succession so file cleaning races with DB.Close() --
		// latter should wait for file cleaning to finish.
		require.NoError(t, it.Close())
		require.NoError(t, db.Close())
		files, err = mem.List("/")
		require.NoError(t, err)
		for _, f := range files {
			if strings.HasSuffix(f, ".sst") {
				t.Fatalf("found sst: %s", f)
			}
		}
	}
}

func TestSSTablesWithApproximateSpanBytes(t *testing.T) {
	d, err := Open("", &Options{
		FS: vfs.NewMem(),
	})
	require.NoError(t, err)
	defer func() {
		if d != nil {
			require.NoError(t, d.Close())
		}
	}()

	// Create two sstables.
	// sstable is contained within keyspan (fileNum = 5).
	require.NoError(t, d.Set([]byte("c"), nil, nil))
	require.NoError(t, d.Set([]byte("d"), nil, nil))
	require.NoError(t, d.Flush())

	// sstable partially overlaps keyspan (fileNum = 7).
	require.NoError(t, d.Set([]byte("d"), nil, nil))
	require.NoError(t, d.Set([]byte("g"), nil, nil))
	require.NoError(t, d.Flush())

	// cannot use WithApproximateSpanBytes without WithKeyRangeFilter.
	_, err = d.SSTables(WithProperties(), WithApproximateSpanBytes())
	require.Error(t, err)

	tableInfos, err := d.SSTables(WithProperties(), WithKeyRangeFilter([]byte("a"), []byte("e")), WithApproximateSpanBytes())
	require.NoError(t, err)

	for _, levelTables := range tableInfos {
		for _, table := range levelTables {
			if table.FileNum == 5 {
				require.Equal(t, table.ApproximateSpanBytes, table.Size)
			}
			if table.FileNum == 7 {
				require.Less(t, table.ApproximateSpanBytes, table.Size)
			}
		}
	}
}

func TestFilterSSTablesWithOption(t *testing.T) {
	d, err := Open("", &Options{
		FS: vfs.NewMem(),
	})
	require.NoError(t, err)
	defer func() {
		if d != nil {
			require.NoError(t, d.Close())
		}
	}()

	// Create two sstables.
	require.NoError(t, d.Set([]byte("/Table/5"), nil, nil))
	require.NoError(t, d.Flush())
	require.NoError(t, d.Set([]byte("/Table/10"), nil, nil))
	require.NoError(t, d.Flush())

	tableInfos, err := d.SSTables(WithKeyRangeFilter([]byte("/Table/5"), []byte("/Table/6")))
	require.NoError(t, err)

	totalTables := 0
	for _, levelTables := range tableInfos {
		totalTables += len(levelTables)
	}

	// with filter second sstable should not be returned
	require.EqualValues(t, 1, totalTables)

	tableInfos, err = d.SSTables()
	require.NoError(t, err)

	totalTables = 0
	for _, levelTables := range tableInfos {
		totalTables += len(levelTables)
	}

	// without filter
	require.EqualValues(t, 2, totalTables)
}

func TestSSTables(t *testing.T) {
	d, err := Open("", &Options{
		FS: vfs.NewMem(),
	})
	require.NoError(t, err)
	defer func() {
		if d != nil {
			require.NoError(t, d.Close())
		}
	}()

	// Create two sstables.
	require.NoError(t, d.Set([]byte("hello"), nil, nil))
	require.NoError(t, d.Flush())
	require.NoError(t, d.Set([]byte("world"), nil, nil))
	require.NoError(t, d.Flush())

	// By default, returned table infos should not contain Properties.
	tableInfos, err := d.SSTables()
	require.NoError(t, err)
	for _, levelTables := range tableInfos {
		for _, info := range levelTables {
			require.Nil(t, info.Properties)
		}
	}

	// With opt `WithProperties()` the `Properties` in table info should not be
	// nil.
	tableInfos, err = d.SSTables(WithProperties())
	require.NoError(t, err)
	for _, levelTables := range tableInfos {
		for _, info := range levelTables {
			require.NotNil(t, info.Properties)
		}
	}
}

func TestVirtualSSTables(t *testing.T) {
	d, err := Open("", &Options{
		FS:                 vfs.NewMem(),
		FormatMajorVersion: FormatTableFormatV6,
	})
	require.NoError(t, err)
	defer func() {
		if d != nil {
			require.NoError(t, d.Close())
		}
	}()

	require.NoError(t, d.Set([]byte("aaa"), []byte("mai"), nil))
	require.NoError(t, d.Set([]byte("aab"), []byte("calico"), nil))
	require.NoError(t, d.Set([]byte("aac"), []byte("october"), nil))
	require.NoError(t, d.Set([]byte("aad"), []byte("rowdy"), nil))
	require.NoError(t, d.Set([]byte("aae"), []byte("hairballs"), nil))
	require.NoError(t, d.Flush())

	// Get the original properties.
	tableInfos, err := d.SSTables(WithProperties())
	require.NoError(t, err)
	originalProps := tableInfos[0][0].Properties

	// Excise to create virtual ssts.
	exciseSpan := KeyRange{
		Start: []byte("aab"),
		End:   []byte("aac"),
	}
	err = d.Excise(context.Background(), exciseSpan)
	require.NoError(t, err)

	tableInfos, err = d.SSTables(WithProperties())
	require.NoError(t, err)

	// Find the virtual sstables and ensure that some of its common properties
	// have been scaled down correctly.
	for _, levelTables := range tableInfos {
		for _, info := range levelTables {
			if info.Virtual {
				virtualProps := info.Properties
				require.NotNil(t, virtualProps)
				require.Less(t, virtualProps.NumEntries, originalProps.NumEntries)
				require.Less(t, virtualProps.RawKeySize, originalProps.RawKeySize)
				require.Less(t, virtualProps.RawValueSize, originalProps.RawValueSize)
			}
		}
	}
}

type testTracer struct {
	enabledOnlyForNonBackgroundContext bool
	buf                                strings.Builder
}

func (t *testTracer) Infof(format string, args ...interface{})  {}
func (t *testTracer) Errorf(format string, args ...interface{}) {}
func (t *testTracer) Fatalf(format string, args ...interface{}) {}

func (t *testTracer) Eventf(ctx context.Context, format string, args ...interface{}) {
	if t.enabledOnlyForNonBackgroundContext && ctx == context.Background() {
		return
	}
	str := fmt.Sprintf(format, args...)
	// Redact known strings that depend on source code line numbers.
	str = regexp.MustCompile(`\(fileNum=[^)]+\)$`).ReplaceAllString(str, "(<redacted>)")
	t.buf.WriteString(str)
	t.buf.WriteString("\n")
}

func (t *testTracer) IsTracingEnabled(ctx context.Context) bool {
	if t.enabledOnlyForNonBackgroundContext && ctx == context.Background() {
		return false
	}
	return true
}

func TestTracing(t *testing.T) {
	defer block.DeterministicReadBlockDurationForTesting()()

	var tracer testTracer
	buf := &tracer.buf
	var db *DB
	defer func() {
		if db != nil {
			db.Close()
		}
	}()
	cache := NewCache(0)
	defer cache.Unref()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	datadriven.RunTest(t, "testdata/tracing", func(t *testing.T, td *datadriven.TestData) string {
		buf.Reset()
		tracer.enabledOnlyForNonBackgroundContext = td.HasArg("disable-background-tracing")
		ctx := ctx
		switch td.Cmd {
		case "build":
			if db != nil {
				db.Close()
			}
			db = testutils.CheckErr(Open("", &Options{
				FS:              vfs.NewMem(),
				Comparer:        testkeys.Comparer,
				Cache:           cache,
				LoggerAndTracer: &tracer,
			}))

			b := db.NewBatch()
			require.NoError(t, runBatchDefineCmd(td, b))
			require.NoError(t, b.Commit(nil))
			require.NoError(t, db.Flush())
			return ""

		case "get":
			for _, key := range strings.Split(td.Input, "\n") {
				v, closer, err := db.Get([]byte(key))
				require.NoError(t, err)
				fmt.Fprintf(buf, "%s:%s\n", key, v)
				closer.Close()
			}

		case "iter":
			iter, _ := db.NewIterWithContext(ctx, &IterOptions{
				KeyTypes: IterKeyTypePointsAndRanges,
			})
			buf.WriteString(runIterCmd(td, iter, true))

		case "snapshot-iter":
			snap := db.NewSnapshot()
			defer snap.Close()
			iter, _ := snap.NewIterWithContext(ctx, &IterOptions{
				KeyTypes: IterKeyTypePointsAndRanges,
			})
			buf.WriteString(runIterCmd(td, iter, true))

		case "indexed-batch-iter":
			b := db.NewIndexedBatch()
			defer b.Close()
			iter, _ := b.NewIterWithContext(ctx, &IterOptions{
				KeyTypes: IterKeyTypePointsAndRanges,
			})
			buf.WriteString(runIterCmd(td, iter, true))

		default:
			td.Fatalf(t, "unknown command: %s", td.Cmd)
		}
		return buf.String()
	})
}

func TestMemtableIngestInversion(t *testing.T) {
	memFS := vfs.NewMem()
	opts := &Options{
		FS:                          memFS,
		MemTableSize:                256 << 10, // 4KB
		MemTableStopWritesThreshold: 1000,
		L0StopWritesThreshold:       1000,
		L0CompactionThreshold:       2,
		CompactionConcurrencyRange: func() (int, int) {
			return 1, 1000
		},
	}

	const channelTimeout = 5 * time.Second

	// We induce delay in compactions by passing in an EventListener that stalls on
	// the first TableCreated event for a compaction job we want to block.
	// FlushBegin and CompactionBegin has info on compaction start/output levels
	// which is what we need to identify what compactions to block. However
	// FlushBegin and CompactionBegin are called while holding db.mu, so we cannot
	// block those events forever. Instead, we grab the job ID from those events
	// and store it. Then during TableCreated, we check if we're creating an output
	// for a job we have identified earlier as one to block, and then hold on a
	// semaphore there until there's a signal from the test code to resume with the
	// compaction.
	//
	// If nextBlockedCompaction is non-zero, we must block the next compaction
	// out of the nextBlockedCompaction - 3 start level. 1 means block the next
	// intra-L0 compaction and 2 means block the next flush (as flushes have
	// a -1 start level).
	var nextBlockedCompaction, blockedJobID int
	var blockedCompactionsMu sync.Mutex // protects the above two variables.
	nextSem := make(chan chan struct{}, 1)
	var el EventListener
	el.EnsureDefaults(testLogger{t: t})
	el.FlushBegin = func(info FlushInfo) {
		blockedCompactionsMu.Lock()
		defer blockedCompactionsMu.Unlock()
		if nextBlockedCompaction == 2 {
			nextBlockedCompaction = 0
			blockedJobID = info.JobID
		}
	}
	el.CompactionBegin = func(info CompactionInfo) {
		// 0 = block nothing, 1 = block intra-L0 compaction, 2 = block flush,
		// 3 = block L0 -> LBase compaction, 4 = block compaction out of L1, and so on.
		blockedCompactionsMu.Lock()
		defer blockedCompactionsMu.Unlock()
		blockValue := info.Input[0].Level + 3
		if info.Input[0].Level == 0 && info.Output.Level == 0 {
			// Intra L0 compaction, denoted by casValue of 1.
			blockValue = 1
		}
		if nextBlockedCompaction == blockValue {
			nextBlockedCompaction = 0
			blockedJobID = info.JobID
		}
	}
	el.TableCreated = func(info TableCreateInfo) {
		blockedCompactionsMu.Lock()
		if info.JobID != blockedJobID {
			blockedCompactionsMu.Unlock()
			return
		}
		blockedJobID = 0
		blockedCompactionsMu.Unlock()
		sem := make(chan struct{})
		nextSem <- sem
		<-sem
	}
	tel := TeeEventListener(MakeLoggingEventListener(testLogger{t: t}), el)
	opts.EventListener = &tel
	opts.Experimental.L0CompactionConcurrency = 1
	d, err := Open("", opts)
	require.NoError(t, err)
	defer func() {
		if d != nil {
			require.NoError(t, d.Close())
		}
	}()

	printLSM := func() {
		d.mu.Lock()
		s := d.mu.versions.currentVersion().String()
		d.mu.Unlock()
		t.Logf("%s", s)
	}

	// Create some sstables. These should go into L6. These are irrelevant for
	// the rest of the test.
	require.NoError(t, d.Set([]byte("b"), []byte("foo"), nil))
	require.NoError(t, d.Flush())
	require.NoError(t, d.Set([]byte("d"), []byte("bar"), nil))
	require.NoError(t, d.Flush())
	require.NoError(t, d.Compact(context.Background(), []byte("a"), []byte("z"), true))

	var baseCompactionSem, flushSem, intraL0Sem chan struct{}
	// Block an L0 -> LBase compaction. This is necessary to induce intra-L0
	// compactions later on.
	blockedCompactionsMu.Lock()
	nextBlockedCompaction = 3
	blockedCompactionsMu.Unlock()
	timeoutSem := time.After(channelTimeout)
	t.Log("blocking an L0 -> LBase compaction")
	// Write an sstable to L0 until we're blocked on an L0 -> LBase compaction.
	breakLoop := false
	for !breakLoop {
		select {
		case sem := <-nextSem:
			baseCompactionSem = sem
			breakLoop = true
		case <-timeoutSem:
			t.Fatal("did not get blocked on an LBase compaction")
		default:
			require.NoError(t, d.Set([]byte("b"), []byte("foo"), nil))
			require.NoError(t, d.Set([]byte("g"), []byte("bar"), nil))
			require.NoError(t, d.Flush())
			time.Sleep(100 * time.Millisecond)
		}
	}
	printLSM()

	// Do 4 ingests, one with the key cc, one with bb and cc, and two with just bb.
	// The purpose of the sstable containing cc is to inflate the L0 sublevel
	// count of the interval at cc, as that's where we want the intra-L0 compaction
	// to be seeded. However we also need a file left of that interval to have
	// the same (or higher) sublevel to trigger the bug in
	// cockroachdb/cockroach#101896. That's why we ingest a file after it to
	// "bridge" the bb/cc intervals, and then ingest a file at bb. These go
	// into sublevels like this:
	//
	//    bb
	//    bb
	//    bb-----cc
	//           cc
	//
	// Eventually, we'll drop an ingested file containing a range del starting at
	// cc around here:
	//
	//    bb
	//    bb     cc---...
	//    bb-----cc
	//           cc
	{
		path := "ingest1.sst"
		f, err := memFS.Create(path, vfs.WriteCategoryUnspecified)
		require.NoError(t, err)
		w := sstable.NewWriter(objstorageprovider.NewFileWritable(f), sstable.WriterOptions{
			TableFormat: d.TableFormat(),
		})
		require.NoError(t, w.Set([]byte("cc"), []byte("foo")))
		require.NoError(t, w.Close())
		require.NoError(t, d.Ingest(context.Background(), []string{path}))
	}
	{
		path := "ingest2.sst"
		f, err := memFS.Create(path, vfs.WriteCategoryUnspecified)
		require.NoError(t, err)
		w := sstable.NewWriter(objstorageprovider.NewFileWritable(f), sstable.WriterOptions{
			TableFormat: d.TableFormat(),
		})
		require.NoError(t, w.Set([]byte("bb"), []byte("foo2")))
		require.NoError(t, w.Set([]byte("cc"), []byte("foo2")))
		require.NoError(t, w.Close())
		require.NoError(t, d.Ingest(context.Background(), []string{path}))
	}
	{
		path := "ingest3.sst"
		f, err := memFS.Create(path, vfs.WriteCategoryUnspecified)
		require.NoError(t, err)
		w := sstable.NewWriter(objstorageprovider.NewFileWritable(f), sstable.WriterOptions{
			TableFormat: d.TableFormat(),
		})
		require.NoError(t, w.Set([]byte("bb"), []byte("foo3")))
		require.NoError(t, w.Close())
		require.NoError(t, d.Ingest(context.Background(), []string{path}))
	}
	{
		path := "ingest4.sst"
		f, err := memFS.Create(path, vfs.WriteCategoryUnspecified)
		require.NoError(t, err)
		w := sstable.NewWriter(objstorageprovider.NewFileWritable(f), sstable.WriterOptions{
			TableFormat: d.TableFormat(),
		})
		require.NoError(t, w.Set([]byte("bb"), []byte("foo4")))
		require.NoError(t, w.Close())
		require.NoError(t, d.Ingest(context.Background(), []string{path}))
	}

	// We now have a base compaction blocked. Block a memtable flush to cause
	// memtables to queue up.
	//
	// Memtable (stuck):
	//
	//   b-----------------g
	//
	// Relevant L0 ssstables
	//
	//    bb
	//    bb
	//    bb-----cc
	//           cc
	blockedCompactionsMu.Lock()
	nextBlockedCompaction = 2
	blockedCompactionsMu.Unlock()
	t.Log("blocking a flush")
	require.NoError(t, d.Set([]byte("b"), []byte("foo2"), nil))
	require.NoError(t, d.Set([]byte("g"), []byte("bar2"), nil))
	_, _ = d.AsyncFlush()
	select {
	case sem := <-nextSem:
		flushSem = sem
	case <-time.After(channelTimeout):
		t.Fatal("did not get blocked on a flush")
	}
	// Add one memtable to flush queue, and finish it off.
	//
	// Memtables (stuck):
	//
	//   b-----------------g (waiting to flush)
	//   b-----------------g (flushing, blocked)
	//
	// Relevant L0 ssstables
	//
	//    bb
	//    bb
	//    bb-----cc
	//           cc
	require.NoError(t, d.Set([]byte("b"), []byte("foo3"), nil))
	require.NoError(t, d.Set([]byte("g"), []byte("bar3"), nil))
	// note: this flush will wait for the earlier, blocked flush, but it closes
	// off the memtable which is what we want.
	_, _ = d.AsyncFlush()

	// Open a new mutable memtable. This gets us an earlier earlierUnflushedSeqNum
	// than the ingest below it.
	require.NoError(t, d.Set([]byte("c"), []byte("somethingbigishappening"), nil))
	// Block an intra-L0 compaction, as one might happen around this time.
	blockedCompactionsMu.Lock()
	nextBlockedCompaction = 1
	blockedCompactionsMu.Unlock()
	t.Log("blocking an intra-L0 compaction")
	// Ingest a file containing a cc-e rangedel.
	//
	// Memtables:
	//
	//         c             (mutable)
	//   b-----------------g (waiting to flush)
	//   b-----------------g (flushing, blocked)
	//
	// Relevant L0 ssstables
	//
	//    bb
	//    bb     cc-----e (just ingested)
	//    bb-----cc
	//           cc
	{
		path := "ingest5.sst"
		f, err := memFS.Create(path, vfs.WriteCategoryUnspecified)
		require.NoError(t, err)
		w := sstable.NewWriter(objstorageprovider.NewFileWritable(f), sstable.WriterOptions{
			TableFormat: d.TableFormat(),
		})
		require.NoError(t, w.DeleteRange([]byte("cc"), []byte("e")))
		require.NoError(t, w.Close())
		require.NoError(t, d.Ingest(context.Background(), []string{path}))
	}
	t.Log("main ingest complete")
	printLSM()
	t.Logf("%s", d.Metrics().String())

	require.NoError(t, d.Set([]byte("d"), []byte("ThisShouldNotBeDeleted"), nil))

	// Do another ingest with a seqnum newer than d. The purpose of this is to
	// increase the LargestSeqNum of the intra-L0 compaction output *beyond*
	// the flush that contains d=ThisShouldNotBeDeleted, therefore causing
	// that point key to be deleted (in the buggy code).
	//
	// Memtables:
	//
	//         c-----d       (mutable)
	//   b-----------------g (waiting to flush)
	//   b-----------------g (flushing, blocked)
	//
	// Relevant L0 ssstables
	//
	//    bb     cc
	//    bb     cc-----e (just ingested)
	//    bb-----cc
	//           cc
	{
		path := "ingest6.sst"
		f, err := memFS.Create(path, vfs.WriteCategoryUnspecified)
		require.NoError(t, err)
		w := sstable.NewWriter(objstorageprovider.NewFileWritable(f), sstable.WriterOptions{
			TableFormat: d.TableFormat(),
		})
		require.NoError(t, w.Set([]byte("cc"), []byte("doesntmatter")))
		require.NoError(t, w.Close())
		require.NoError(t, d.Ingest(context.Background(), []string{path}))
	}

	// Unblock earlier flushes. We will first finish flushing the blocked
	// memtable, and end up in this state:
	//
	// Memtables:
	//
	//         c-----d       (mutable)
	//   b-----------------g (waiting to flush)
	//
	// Relevant L0 ssstables
	//
	//  b-------------------g (irrelevant, just flushed)
	//    bb     cc (has LargestSeqNum > earliestUnflushedSeqNum)
	//    bb     cc-----e (has a rangedel)
	//    bb-----cc
	//           cc
	//
	// Note that while b----g is relatively old (and so has a low LargestSeqNum),
	// it bridges a bunch of intervals. Had we regenerated sublevels from scratch,
	// it'd have gone below the cc-e sstable. But due to #101896, we just slapped
	// it on top. Now, as long as our seed interval is the one at cc and our seed
	// file is the just-flushed L0 sstable, we will go down and include anything
	// in that interval even if it has a LargestSeqNum > earliestUnflushedSeqNum.
	//
	// All asterisked L0 sstables should now get picked in an intra-L0 compaction
	// right after the flush finishes, that we then block:
	//
	//  b-------------------g*
	//    bb*    cc*
	//    bb*    cc-----e*
	//    bb-----cc*
	//           cc*
	t.Log("unblocking flush")
	flushSem <- struct{}{}
	printLSM()

	select {
	case sem := <-nextSem:
		intraL0Sem = sem
	case <-time.After(channelTimeout):
		t.Fatal("did not get blocked on an intra L0 compaction")
	}

	// Ensure all memtables are flushed. This will mean d=ThisShouldNotBeDeleted
	// will land in L0 and since that was the last key written to a memtable,
	// and the ingestion at cc came after it, the output of the intra-L0
	// compaction will elevate the cc-e rangedel above it and delete it
	// (if #101896 is not fixed).
	ch, _ := d.AsyncFlush()
	<-ch

	// Unblock earlier intra-L0 compaction.
	t.Log("unblocking intraL0")
	intraL0Sem <- struct{}{}
	printLSM()

	// Try reading d a couple times.
	for i := 0; i < 2; i++ {
		val, closer, err := d.Get([]byte("d"))
		require.NoError(t, err)
		require.Equal(t, []byte("ThisShouldNotBeDeleted"), val)
		if closer != nil {
			closer.Close()
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Unblock everything.
	baseCompactionSem <- struct{}{}
}

func BenchmarkDelete(b *testing.B) {
	rng := rand.New(rand.NewPCG(0, uint64(time.Now().UnixNano())))
	const keyCount = 10000
	var keys [keyCount][]byte
	for i := 0; i < keyCount; i++ {
		keys[i] = []byte(strconv.Itoa(rng.Int()))
	}
	val := bytes.Repeat([]byte("x"), 10)

	benchmark := func(b *testing.B, useSingleDelete bool) {
		d, err := Open(
			"",
			&Options{
				FS: vfs.NewMem(),
			})
		if err != nil {
			b.Fatal(err)
		}
		defer func() {
			if err := d.Close(); err != nil {
				b.Fatal(err)
			}
		}()

		b.StartTimer()
		for _, key := range keys {
			_ = d.Set(key, val, nil)
			if useSingleDelete {
				_ = d.SingleDelete(key, nil)
			} else {
				_ = d.Delete(key, nil)
			}
		}
		// Manually flush as it is flushing/compaction where SingleDelete
		// performance shows up. With SingleDelete, we can elide all of the
		// SingleDelete and Set records.
		if err := d.Flush(); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
	}

	b.Run("delete", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			benchmark(b, false)
		}
	})

	b.Run("single-delete", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			benchmark(b, true)
		}
	})
}

func BenchmarkNewIterReadAmp(b *testing.B) {
	for _, readAmp := range []int{10, 100, 1000} {
		b.Run(strconv.Itoa(readAmp), func(b *testing.B) {
			opts := &Options{
				FS:                    vfs.NewMem(),
				L0StopWritesThreshold: 1000,
			}
			opts.DisableAutomaticCompactions = true

			d, err := Open("", opts)
			require.NoError(b, err)

			for i := 0; i < readAmp; i++ {
				require.NoError(b, d.Set([]byte("a"), []byte("b"), NoSync))
				require.NoError(b, d.Flush())
			}

			require.Equal(b, d.Metrics().ReadAmp(), readAmp)

			b.StopTimer()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StartTimer()
				iter, _ := d.NewIter(nil)
				b.StopTimer()
				require.NoError(b, iter.Close())
			}

			require.NoError(b, d.Close())
		})
	}
}

func verifyGet(t *testing.T, r Reader, key, expected []byte) {
	val, closer, err := r.Get(key)
	require.NoError(t, err)
	if !bytes.Equal(expected, val) {
		t.Fatalf("expected %s, but got %s", expected, val)
	}
	closer.Close()
}

func verifyGetNotFound(t *testing.T, r Reader, key []byte) {
	val, _, err := r.Get(key)
	if err != base.ErrNotFound {
		t.Fatalf("expected nil, but got %s", val)
	}
}

func BenchmarkRotateMemtables(b *testing.B) {
	o := &Options{FS: vfs.NewMem(), MemTableSize: 64 << 20 /* 64 MB */}
	d, err := Open("", o)
	require.NoError(b, err)

	// We want to jump to full-sized memtables.
	d.mu.Lock()
	d.mu.mem.nextSize = o.MemTableSize
	d.mu.Unlock()
	require.NoError(b, d.Flush())

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := d.Flush(); err != nil {
			b.Fatal(err)
		}
	}
}

// TODO(sumeer): rewrite test when LogRecycler is hidden from this package.
func TestRecycleLogs(t *testing.T) {
	mem := vfs.NewMem()
	d, err := Open("", &Options{
		FS: mem,
	})
	require.NoError(t, err)

	logNum := func() base.DiskFileNum {
		d.mu.Lock()
		defer d.mu.Unlock()
		walNums := d.mu.log.manager.List()
		return base.DiskFileNum(walNums[len(walNums)-1].Num)
	}
	logCount := func() int {
		d.mu.Lock()
		defer d.mu.Unlock()
		walNums := d.mu.log.manager.List()
		return len(walNums)
	}

	recycler := d.mu.log.manager.RecyclerForTesting()
	// Flush the memtable a few times, forcing rotation of the WAL. We should see
	// the recycled logs change as expected.
	require.EqualValues(t, []base.DiskFileNum(nil), recycler.LogNumsForTesting())
	curLog := logNum()

	require.NoError(t, d.Flush())

	require.EqualValues(t, []base.DiskFileNum{curLog}, recycler.LogNumsForTesting())
	curLog = logNum()

	require.NoError(t, d.Flush())

	require.EqualValues(t, []base.DiskFileNum{curLog}, recycler.LogNumsForTesting())

	require.NoError(t, d.Close())

	d, err = Open("", &Options{
		FS:     mem,
		Logger: testLogger{t},
	})
	require.NoError(t, err)
	recycler = d.mu.log.manager.RecyclerForTesting()
	metrics := d.Metrics()
	if n := logCount(); n != int(metrics.WAL.Files) {
		t.Fatalf("expected %d WAL files, but found %d", n, metrics.WAL.Files)
	}
	if n, sz := recycler.Stats(); n != int(metrics.WAL.ObsoleteFiles) {
		t.Fatalf("expected %d obsolete WAL files, but found %d", n, metrics.WAL.ObsoleteFiles)
	} else if sz != metrics.WAL.ObsoletePhysicalSize {
		t.Fatalf("expected %d obsolete physical WAL size, but found %d", sz, metrics.WAL.ObsoletePhysicalSize)
	}
	if recycled := recycler.LogNumsForTesting(); len(recycled) != 0 {
		t.Fatalf("expected no recycled WAL files after recovery, but found %d", recycled)
	}
	require.NoError(t, d.Close())
}

type sstAndLogFileBlockingFS struct {
	vfs.FS
	unblocker sync.WaitGroup
}

var _ vfs.FS = &sstAndLogFileBlockingFS{}

func (fs *sstAndLogFileBlockingFS) Create(
	name string, category vfs.DiskWriteCategory,
) (vfs.File, error) {
	if strings.HasSuffix(name, ".log") || strings.HasSuffix(name, ".sst") {
		fs.unblocker.Wait()
	}
	return fs.FS.Create(name, category)
}

func (fs *sstAndLogFileBlockingFS) unblock() {
	fs.unblocker.Done()
}

func newBlockingFS(fs vfs.FS) *sstAndLogFileBlockingFS {
	lfbfs := &sstAndLogFileBlockingFS{FS: fs}
	lfbfs.unblocker.Add(1)
	return lfbfs
}

func TestWALFailoverAvoidsWriteStall(t *testing.T) {
	mem := vfs.NewMem()
	// All sst and log creation is blocked.
	primaryFS := newBlockingFS(mem)
	// Secondary for WAL failover can do log creation.
	secondary := wal.Dir{FS: mem, Dirname: "secondary"}
	walFailover := &WALFailoverOptions{Secondary: secondary, FailoverOptions: wal.FailoverOptions{
		UnhealthySamplingInterval:          100 * time.Millisecond,
		UnhealthyOperationLatencyThreshold: func() (time.Duration, bool) { return time.Second, true },
	}}
	o := &Options{
		FS:                          primaryFS,
		MemTableSize:                4 << 20,
		MemTableStopWritesThreshold: 2,
		WALFailover:                 walFailover,
	}
	d, err := Open("", o)
	require.NoError(t, err)
	defer d.Close()
	value := make([]byte, 1<<20)
	for i := range value {
		value[i] = byte(rand.Uint32())
	}
	// After ~8 writes, the default write stall threshold is exceeded, but the
	// writes will not block indefinitely since failover has or will happen, and
	// wal.Manager.ElevateWriteStallThresholdForFailover() will return true.
	for i := 0; i < 200; i++ {
		require.NoError(t, d.Set([]byte(fmt.Sprintf("%d", i)), value, nil))
	}
	// Validate that the default write stall threshold was exceeded.
	require.Greater(
		t, d.Metrics().MemTable.Size, o.MemTableSize*uint64(o.MemTableStopWritesThreshold))
	// Unblock the writes to allow the DB to close.
	primaryFS.unblock()
}

// TestDeterminism is a datadriven test intended to validate determinism of
// operations in the face of concurrency or randomizing of operations. The test
// data defines a sequence of commands run sequentially. Then the test may
// re-run the sequence introducing latencies, reorderings, parallelism, etc,
// ensuring that all re-runs produce the same output.
func TestDeterminism(t *testing.T) {
	var d *DB
	var fs vfs.FS = vfs.NewMem()
	defer func() {
		if d != nil {
			require.NoError(t, d.Close())
		}
	}()

	type step struct {
		fn     func(td *datadriven.TestData) string
		td     datadriven.TestData
		output string
	}
	var sequence []step
	addStep := func(td *datadriven.TestData, fn func(td *datadriven.TestData) string) string {
		s := strings.TrimSpace(fn(td))
		sequence = append(sequence, step{
			fn:     fn,
			td:     *td,
			output: s,
		})
		if len(s) > 0 {
			s = s + "\n"
		}
		return s + fmt.Sprintf("%d:%s", len(sequence)-1, td.Cmd)
	}

	datadriven.RunTest(t, "testdata/determinism",
		func(t *testing.T, td *datadriven.TestData) string {
			switch td.Cmd {
			case "reset":
				fs = vfs.NewMem()
				sequence = nil
				return ""
			case "define":
				return addStep(td, func(td *datadriven.TestData) string {
					opts := &Options{
						FS:                          fs,
						DebugCheck:                  DebugCheckLevels,
						Logger:                      testLogger{t},
						FormatMajorVersion:          FormatNewest,
						DisableAutomaticCompactions: true,
					}
					opts.Experimental.IngestSplit = func() bool { return rand.IntN(2) == 1 }
					var err error
					if d, err = runDBDefineCmdReuseFS(td, opts); err != nil {
						return err.Error()
					}
					return d.mu.versions.currentVersion().String()
				})
			case "batch":
				return addStep(td, func(td *datadriven.TestData) string {
					b := d.NewBatch()
					require.NoError(t, runBatchDefineCmd(td, b))
					require.NoError(t, b.Commit(nil))
					return ""
				})
			case "build":
				return addStep(td, func(td *datadriven.TestData) string {
					require.NoError(t, runBuildCmd(td, d, fs))
					return ""
				})
			case "excise":
				return addStep(td, func(td *datadriven.TestData) string {
					if err := runExciseCmd(td, d); err != nil {
						return err.Error()
					}
					return ""
				})
			case "flush":
				return addStep(td, func(td *datadriven.TestData) string {
					_, err := d.AsyncFlush()
					if err != nil {
						return err.Error()
					}
					return ""
				})
			case "ingest-and-excise":
				return addStep(td, func(td *datadriven.TestData) string {
					if err := runIngestAndExciseCmd(td, d); err != nil {
						return err.Error()
					}
					return ""
				})
			case "maybe-compact":
				return addStep(td, func(td *datadriven.TestData) string {
					d.mu.Lock()
					defer d.mu.Unlock()
					d.opts.DisableAutomaticCompactions = false
					d.maybeScheduleCompaction()
					d.opts.DisableAutomaticCompactions = true
					return ""
				})
			case "memtable-info":
				return addStep(td, func(td *datadriven.TestData) string {
					d.commit.mu.Lock()
					defer d.commit.mu.Unlock()
					d.mu.Lock()
					defer d.mu.Unlock()
					var buf bytes.Buffer
					fmt.Fprintf(&buf, "flushable queue: %d entries\n", len(d.mu.mem.queue))
					fmt.Fprintf(&buf, "mutable:\n")
					fmt.Fprintf(&buf, "  alloced:  %d\n", d.mu.mem.mutable.totalBytes())
					if td.HasArg("reserved") {
						fmt.Fprintf(&buf, "  reserved: %d\n", d.mu.mem.mutable.reserved)
					}
					if td.HasArg("in-use") {
						fmt.Fprintf(&buf, "  in-use:   %d\n", d.mu.mem.mutable.inuseBytes())
					}
					return buf.String()
				})
			case "run":
				var mkfs func() vfs.FS = func() vfs.FS { return vfs.NewMem() }
				var beforeStep func() = func() {}
				for _, cmdArg := range td.CmdArgs {
					switch cmdArg.Key {
					case "io-latency", "step-latency":
						p, err := strconv.ParseFloat(cmdArg.Vals[0], 64)
						require.NoError(t, err)
						mean, err := time.ParseDuration(cmdArg.Vals[1])
						require.NoError(t, err)
						if cmdArg.Key == "io-latency" {
							prevMkfs := mkfs
							mkfs = func() vfs.FS {
								return errorfs.Wrap(prevMkfs(), errorfs.RandomLatency(
									errorfs.Randomly(p, 0), mean, 0 /* seed */, 0 /* no limit */))
							}
						} else if cmdArg.Key == "step-latency" {
							beforeStep = func() {
								if rand.Float64() < p {
									time.Sleep(time.Duration(min(rand.ExpFloat64(), 20.0) * float64(mean)))
								}
							}
						}
					}
				}
				ordering := parseOrdering(td.Input)

				var sb strings.Builder
				rerunSequence := func() string {
					sb.Reset()
					fs = mkfs()
					output := make([]string, len(sequence))
					ordering.visit(func(i int) {
						beforeStep()
						output[i] = strings.TrimSpace(sequence[i].fn(&sequence[i].td))
					})
					for i := range output {
						if output[i] != sequence[i].output {
							fmt.Fprintf(&sb, "# step %d: %s\n", i, sequence[i].td.Cmd)
							fmt.Fprintf(&sb, "expected:\n%s\ngot:\n%s", sequence[i].output, output[i])
							fmt.Fprintln(&sb)
						}
					}
					return sb.String()
				}
				retries := 10
				td.MaybeScanArgs(t, "count", &retries)
				for i := 0; i < retries; i++ {
					if diff := rerunSequence(); len(diff) > 0 {
						return diff
					}
				}
				return "ok"
			default:
				return fmt.Sprintf("unknown command: %s", td.Cmd)
			}
		})
}

type orderingNode interface {
	visit(func(int))
}

type sequential []orderingNode

func (s sequential) visit(fn func(int)) {
	for _, n := range s {
		n.visit(fn)
	}
}

type reorder []orderingNode

func (r reorder) visit(fn func(int)) {
	for _, i := range rand.Perm(len(r)) {
		r[i].visit(fn)
	}
}

type parallel []orderingNode

func (p parallel) visit(fn func(int)) {
	var wg sync.WaitGroup
	wg.Add(len(p))
	for i := range p {
		go func(i int) {
			defer wg.Done()
			p[i].visit(fn)
		}(i)
	}
	wg.Wait()
}

type leaf int

func (l leaf) visit(fn func(int)) { fn(int(l)) }

func parseOrdering(s string) orderingNode {
	n, _ := parseOrderingTokens(strings.Fields(s))
	return n
}

func parseOrderingTokens(tokens []string) (orderingNode, int) {
	if len(tokens) == 0 {
		return nil, 0
	}
	switch tokens[0] {
	case ")":
		panic("unexpected )")
	case "sequential(", "reorder(", "parallel(":
		var nodes []orderingNode
		i := 1
		for i < len(tokens) {
			if tokens[i] == ")" {
				i++
				break
			}
			n, m := parseOrderingTokens(tokens[i:])
			nodes = append(nodes, n)
			i += m
		}
		switch tokens[0] {
		case "sequential(":
			return sequential(nodes), i
		case "reorder(":
			return reorder(nodes), i
		case "parallel(":
			return parallel(nodes), i
		default:
			panic("unreachable")
		}
	default:
		n := strings.IndexByte(tokens[0], ':')
		if n == -1 {
			n = len(tokens[0])
		}
		v, err := strconv.Atoi(tokens[0][:n])
		if err != nil {
			panic(err)
		}
		return leaf(v), 1
	}
}

type readTrackFS struct {
	vfs.FS

	currReadCount atomic.Int32
	maxReadCount  atomic.Int32
}

type readTrackFile struct {
	vfs.File
	fs *readTrackFS
}

func (fs *readTrackFS) Open(name string, opts ...vfs.OpenOption) (vfs.File, error) {
	file, err := fs.FS.Open(name, opts...)
	if err != nil || !strings.HasSuffix(name, ".sst") {
		return file, err
	}
	return &readTrackFile{
		File: file,
		fs:   fs,
	}, nil
}

func (f *readTrackFile) ReadAt(p []byte, off int64) (n int, err error) {
	val := f.fs.currReadCount.Add(1)
	defer f.fs.currReadCount.Add(-1)
	for maxVal := f.fs.maxReadCount.Load(); val > maxVal; maxVal = f.fs.maxReadCount.Load() {
		if f.fs.maxReadCount.CompareAndSwap(maxVal, val) {
			break
		}
	}
	return f.File.ReadAt(p, off)
}

func TestLoadBlockSema(t *testing.T) {
	fs := &readTrackFS{FS: vfs.NewMem()}
	sema := fifo.NewSemaphore(100)
	db, err := Open("", testingRandomized(t, &Options{
		Cache:         cache.New(1),
		FS:            fs,
		LoadBlockSema: sema,
	}))
	require.NoError(t, err)

	key := func(i, j int) []byte {
		return []byte(fmt.Sprintf("%02d/%02d", i, j))
	}

	// Create 20 regions and compact them separately, so we end up with 20
	// disjoint tables.
	const numRegions = 20
	const numKeys = 20
	for i := 0; i < numRegions; i++ {
		for j := 0; j < numKeys; j++ {
			require.NoError(t, db.Set(key(i, j), []byte("value"), nil))
		}
		require.NoError(t, db.Compact(context.Background(), key(i, 0), key(i, numKeys-1), false))
	}

	// Read all regions to warm up the file cache.
	for i := 0; i < numRegions; i++ {
		val, closer, err := db.Get(key(i, 1))
		require.NoError(t, err)
		require.Equal(t, []byte("value"), val)
		if closer != nil {
			closer.Close()
		}
	}

	for _, n := range []int64{1, 2, 4} {
		t.Run(fmt.Sprintf("%d", n), func(t *testing.T) {
			sema.UpdateCapacity(n)
			fs.maxReadCount.Store(0)
			var wg sync.WaitGroup
			// Spin up workers that perform random reads.
			const numWorkers = 20
			for i := 0; i < numWorkers; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					const numQueries = 100
					for i := 0; i < numQueries; i++ {
						val, closer, err := db.Get(key(rand.IntN(numRegions), rand.IntN(numKeys)))
						require.NoError(t, err)
						require.Equal(t, []byte("value"), val)
						if closer != nil {
							closer.Close()
						}
						runtime.Gosched()
					}
				}()
			}
			wg.Wait()
			// Verify the maximum read count did not exceed the limit.
			maxReadCount := fs.maxReadCount.Load()
			require.Greater(t, maxReadCount, int32(0))
			require.LessOrEqual(t, maxReadCount, int32(n))
		})
	}
}
