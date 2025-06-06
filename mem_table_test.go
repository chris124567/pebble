// Copyright 2011 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package pebble

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/cockroachdb/datadriven"
	"github.com/cockroachdb/errors"
	"github.com/chris124567/pebble/internal/arenaskl"
	"github.com/chris124567/pebble/internal/base"
	"github.com/chris124567/pebble/internal/itertest"
	"github.com/chris124567/pebble/internal/rangekey"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
)

// get gets the value for the given key. It returns ErrNotFound if the DB does
// not contain the key.
func (m *memTable) get(key []byte) (value []byte, err error) {
	it := m.skl.NewIter(nil, nil)
	kv := it.SeekGE(key, base.SeekGEFlagsNone)
	if kv == nil {
		return nil, ErrNotFound
	}
	if !m.equal(key, kv.K.UserKey) {
		return nil, ErrNotFound
	}
	switch kv.Kind() {
	case InternalKeyKindDelete, InternalKeyKindSingleDelete, InternalKeyKindDeleteSized:
		return nil, ErrNotFound
	default:
		return kv.InPlaceValue(), nil
	}
}

// Set sets the value for the given key. It overwrites any previous value for
// that key; a DB is not a multi-map. NB: this might have unexpected
// interaction with prepare/apply. Caveat emptor!
func (m *memTable) set(key InternalKey, value []byte) error {
	if key.Kind() == InternalKeyKindRangeDelete {
		if err := m.rangeDelSkl.Add(key, value); err != nil {
			return err
		}
		m.tombstones.invalidate(1)
		return nil
	}
	if rangekey.IsRangeKey(key.Kind()) {
		if err := m.rangeKeySkl.Add(key, value); err != nil {
			return err
		}
		m.rangeKeys.invalidate(1)
		return nil
	}
	return m.skl.Add(key, value)
}

// count returns the number of entries in a DB.
func (m *memTable) count() (n int) {
	x := m.newIter(nil)
	for kv := x.First(); kv != nil; kv = x.Next() {
		n++
	}
	if x.Close() != nil {
		return -1
	}
	return n
}

func ikey(s string) InternalKey {
	return base.MakeInternalKey([]byte(s), 0, InternalKeyKindSet)
}

func TestMemTableBasic(t *testing.T) {
	// Check the empty DB.
	m := newMemTable(memTableOptions{})
	if got, want := m.count(), 0; got != want {
		t.Fatalf("0.count: got %v, want %v", got, want)
	}
	v, err := m.get([]byte("cherry"))
	if string(v) != "" || err != ErrNotFound {
		t.Fatalf("1.get: got (%q, %v), want (%q, %v)", v, err, "", ErrNotFound)
	}
	// Add some key/value pairs.
	m.set(ikey("cherry"), []byte("red"))
	m.set(ikey("peach"), []byte("yellow"))
	m.set(ikey("grape"), []byte("red"))
	m.set(ikey("grape"), []byte("green"))
	m.set(ikey("plum"), []byte("purple"))
	if got, want := m.count(), 4; got != want {
		t.Fatalf("2.count: got %v, want %v", got, want)
	}
	// Get keys that are and aren't in the DB.
	v, err = m.get([]byte("plum"))
	if string(v) != "purple" || err != nil {
		t.Fatalf("6.get: got (%q, %v), want (%q, %v)", v, err, "purple", error(nil))
	}
	v, err = m.get([]byte("lychee"))
	if string(v) != "" || err != ErrNotFound {
		t.Fatalf("7.get: got (%q, %v), want (%q, %v)", v, err, "", ErrNotFound)
	}
	// Check an iterator.
	s, x := "", m.newIter(nil)
	for kv := x.SeekGE([]byte("mango"), base.SeekGEFlagsNone); kv != nil; kv = x.Next() {
		v, _, err := kv.Value(nil)
		require.NoError(t, err)
		s += fmt.Sprintf("%s/%s.", kv.K.UserKey, v)
	}
	if want := "peach/yellow.plum/purple."; s != want {
		t.Fatalf("8.iter: got %q, want %q", s, want)
	}
	if err = x.Close(); err != nil {
		t.Fatalf("9.close: %v", err)
	}
	// Check some more sets and deletes.
	if err := m.set(ikey("apricot"), []byte("orange")); err != nil {
		t.Fatalf("12.set: %v", err)
	}
	if got, want := m.count(), 5; got != want {
		t.Fatalf("13.count: got %v, want %v", got, want)
	}
}

func TestMemTableCount(t *testing.T) {
	m := newMemTable(memTableOptions{})
	for i := 0; i < 200; i++ {
		if j := m.count(); j != i {
			t.Fatalf("count: got %d, want %d", j, i)
		}
		m.set(InternalKey{UserKey: []byte{byte(i)}}, nil)
	}
}

func TestMemTableEmpty(t *testing.T) {
	m := newMemTable(memTableOptions{})
	if !m.empty() {
		t.Errorf("got !empty, want empty")
	}
	// Add one key/value pair with an empty key and empty value.
	m.set(InternalKey{}, nil)
	if m.empty() {
		t.Errorf("got empty, want !empty")
	}
}

func TestMemTable1000Entries(t *testing.T) {
	// Initialize the DB.
	const N = 1000
	m0 := newMemTable(memTableOptions{})
	for i := 0; i < N; i++ {
		k := ikey(strconv.Itoa(i))
		v := []byte(strings.Repeat("x", i))
		m0.set(k, v)
	}
	// Check the DB count.
	if got, want := m0.count(), 1000; got != want {
		t.Fatalf("count: got %v, want %v", got, want)
	}
	// Check random-access lookup.
	r := rand.New(rand.NewPCG(0, 0))
	for i := 0; i < 3*N; i++ {
		j := r.IntN(N)
		k := []byte(strconv.Itoa(j))
		v, err := m0.get(k)
		require.NoError(t, err)
		if len(v) != cap(v) {
			t.Fatalf("get: j=%d, got len(v)=%d, cap(v)=%d", j, len(v), cap(v))
		}
		var c uint8
		if len(v) != 0 {
			c = v[0]
		} else {
			c = 'x'
		}
		if len(v) != j || c != 'x' {
			t.Fatalf("get: j=%d, got len(v)=%d,c=%c, want %d,%c", j, len(v), c, j, 'x')
		}
	}
	// Check that iterating through the middle of the DB looks OK.
	// Keys are in lexicographic order, not numerical order.
	// Multiples of 3 are not present.
	wants := []string{
		"499",
		"5",
		"50",
		"500",
		"501",
		"502",
		"503",
		"504",
		"505",
		"506",
		"507",
	}
	x := m0.newIter(nil)
	kv := x.SeekGE([]byte(wants[0]), base.SeekGEFlagsNone)
	for _, want := range wants {
		if kv == nil {
			t.Fatalf("iter: next failed, want=%q", want)
		}
		if got := string(kv.K.UserKey); got != want {
			t.Fatalf("iter: got %q, want %q", got, want)
		}
		if k := kv.K.UserKey; len(k) != cap(k) {
			t.Fatalf("iter: len(k)=%d, cap(k)=%d", len(k), cap(k))
		}
		v, _, err := kv.Value(nil)
		require.NoError(t, err)
		if len(v) != cap(v) {
			t.Fatalf("iter: len(v)=%d, cap(v)=%d", len(v), cap(v))
		}
		x.Next()
	}
	if err := x.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestMemTableIter(t *testing.T) {
	var mem *memTable
	for _, testdata := range []string{
		"testdata/internal_iter_next", "testdata/internal_iter_bounds"} {
		datadriven.RunTest(t, testdata, func(t *testing.T, d *datadriven.TestData) string {
			switch d.Cmd {
			case "define":
				mem = newMemTable(memTableOptions{})
				for _, key := range strings.Split(d.Input, "\n") {
					j := strings.Index(key, ":")
					if err := mem.set(base.ParseInternalKey(key[:j]), []byte(key[j+1:])); err != nil {
						return err.Error()
					}
				}
				return ""

			case "iter":
				var options IterOptions
				for _, arg := range d.CmdArgs {
					switch arg.Key {
					case "lower":
						if len(arg.Vals) != 1 {
							return fmt.Sprintf(
								"%s expects at most 1 value for lower", d.Cmd)
						}
						options.LowerBound = []byte(arg.Vals[0])
					case "upper":
						if len(arg.Vals) != 1 {
							return fmt.Sprintf(
								"%s expects at most 1 value for upper", d.Cmd)
						}
						options.UpperBound = []byte(arg.Vals[0])
					default:
						return fmt.Sprintf("unknown arg: %s", arg.Key)
					}
				}
				iter := mem.newIter(&options)
				defer iter.Close()
				return itertest.RunInternalIterCmd(t, d, iter)

			default:
				return fmt.Sprintf("unknown command: %s", d.Cmd)
			}
		})
	}
}

func TestMemTableDeleteRange(t *testing.T) {
	var mem *memTable
	var seqNum base.SeqNum

	datadriven.RunTest(t, "testdata/delete_range", func(t *testing.T, td *datadriven.TestData) string {
		switch td.Cmd {
		case "clear":
			mem = nil
			seqNum = 0
			return ""

		case "define":
			b := newBatch(nil)
			if err := runBatchDefineCmd(td, b); err != nil {
				return err.Error()
			}
			if mem == nil {
				mem = newMemTable(memTableOptions{})
			}
			if err := mem.apply(b, seqNum); err != nil {
				return err.Error()
			}
			seqNum += base.SeqNum(b.Count())
			return ""

		case "scan":
			var buf bytes.Buffer
			if td.HasArg("range-del") {
				iter := mem.newRangeDelIter(nil)
				defer iter.Close()
				scanKeyspanIterator(&buf, iter)
			} else {
				iter := mem.newIter(nil)
				defer iter.Close()
				scanInternalIter(&buf, iter)
			}
			return buf.String()

		default:
			return fmt.Sprintf("unknown command: %s", td.Cmd)
		}
	})
}

func TestMemTableConcurrentDeleteRange(t *testing.T) {
	// Concurrently write and read range tombstones. Workers add range
	// tombstones, and then immediately retrieve them verifying that the
	// tombstones they've added are all present.

	m := newMemTable(memTableOptions{Options: &Options{MemTableSize: 64 << 20}})

	const workers = 10
	eg, _ := errgroup.WithContext(context.Background())
	var seqNum base.AtomicSeqNum
	seqNum.Store(1)
	for i := 0; i < workers; i++ {
		i := i
		eg.Go(func() error {
			start := ([]byte)(fmt.Sprintf("%03d", i))
			end := ([]byte)(fmt.Sprintf("%03d", i+1))
			for j := 0; j < 100; j++ {
				b := newBatch(nil)
				b.DeleteRange(start, end, nil)
				n := seqNum.Add(1) - 1
				require.NoError(t, m.apply(b, n))
				b.Close()

				var count int
				it := m.newRangeDelIter(nil)
				s, err := it.SeekGE(start)
				for ; s != nil; s, err = it.Next() {
					if m.cmp(s.Start, end) >= 0 {
						break
					}
					count += len(s.Keys)
				}
				if err != nil {
					return err
				}
				if j+1 != count {
					return errors.Errorf("%d: expected %d tombstones, but found %d", i, j+1, count)
				}
			}
			return nil
		})
	}
	err := eg.Wait()
	if err != nil {
		t.Error(err)
	}
}

func TestMemTableReserved(t *testing.T) {
	m := newMemTable(memTableOptions{size: 5000})
	// Increase to 2 references.
	m.writerRef()
	// The initial reservation accounts for the already allocated bytes from the
	// arena.
	require.Equal(t, m.reserved, m.skl.Arena().Size())
	b := newBatch(nil)
	b.Set([]byte("blueberry"), []byte("pie"), nil)
	require.NotEqual(t, 0, int(b.memTableSize))
	prevReserved := m.reserved
	m.prepare(b)
	require.Equal(t, int(m.reserved), int(b.memTableSize)+int(prevReserved))
}

func TestMemTable(t *testing.T) {
	var m *memTable
	var buf bytes.Buffer
	batches := map[string]*Batch{}

	summary := func() string {
		return fmt.Sprintf("%d of %d bytes available",
			m.availBytes(), m.totalBytes())
	}

	datadriven.RunTest(t, "testdata/mem_table", func(t *testing.T, td *datadriven.TestData) string {
		buf.Reset()
		switch td.Cmd {
		case "new":
			var o memTableOptions
			td.MaybeScanArgs(t, "size", &o.size)
			m = newMemTable(o)
			return ""
		case "prepare":
			var name string
			td.ScanArgs(t, "name", &name)
			b := newBatch(nil)
			if err := runBatchDefineCmd(td, b); err != nil {
				return err.Error()
			}
			batches[name] = b
			if err := m.prepare(b); err != nil {
				return err.Error()
			}
			return summary()
		case "apply":
			var name string
			var seqNum uint64
			td.ScanArgs(t, "name", &name)
			td.ScanArgs(t, "seq", &seqNum)
			if err := m.apply(batches[name], base.SeqNum(seqNum)); err != nil {
				return err.Error()
			}
			delete(batches, name)
			return summary()
		case "computePossibleOverlaps":
			stopAfterFirst := td.HasArg("stop-after-first")

			var keyRanges []bounded
			for _, l := range strings.Split(td.Input, "\n") {
				s := strings.FieldsFunc(l, func(r rune) bool { return unicode.IsSpace(r) || r == '-' })
				keyRanges = append(keyRanges, KeyRange{Start: []byte(s[0]), End: []byte(s[1])})
			}

			m.computePossibleOverlaps(func(b bounded) shouldContinue {
				fmt.Fprintf(&buf, "%s\n", b)
				if stopAfterFirst {
					return stopIteration
				}
				return continueIteration
			}, keyRanges...)

			return buf.String()
		default:
			return fmt.Sprintf("unrecognized command %q", td.Cmd)
		}
	})
}

func buildMemTable(b *testing.B) (*memTable, [][]byte) {
	m := newMemTable(memTableOptions{})
	var keys [][]byte
	var ikey InternalKey
	for i := 0; ; i++ {
		key := []byte(fmt.Sprintf("%08d", i))
		keys = append(keys, key)
		ikey = base.MakeInternalKey(key, 0, InternalKeyKindSet)
		if m.set(ikey, nil) == arenaskl.ErrArenaFull {
			break
		}
	}
	return m, keys
}

func BenchmarkMemTableIterSeekGE(b *testing.B) {
	m, keys := buildMemTable(b)
	iter := m.newIter(nil)
	rng := rand.New(rand.NewPCG(0, uint64(time.Now().UnixNano())))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		iter.SeekGE(keys[rng.IntN(len(keys))], base.SeekGEFlagsNone)
	}
}

func BenchmarkMemTableIterSeqSeekGEWithBounds(b *testing.B) {
	m, keys := buildMemTable(b)
	rng := rand.New(rand.NewPCG(0, uint64(17136275210000)))
	// Set bounds to restrict iteration to the middle 50% of keys.
	iter := m.newIter(&IterOptions{
		LowerBound: keys[len(keys)/4],
		UpperBound: keys[3*len(keys)/4],
	})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		iter.SeekGE(keys[rng.IntN(len(keys))], base.SeekGEFlagsNone)
	}
}

// BenchmarkMemTableIterSeekGESuccessiveWithBounds benchmarks a particular case
// where an upper bound excludes the majority of the memtable keys and the user
// seeks the iterator with successively increasing keys. This pattern is
// expected to be common in CockroachDB: eg, intent resolution with an upper
// bound at the end of the lock table span, or a MVCC iterator with an upper
// bound restricting constraining iteration to a single CockroachDB Range.
func BenchmarkMemTableIterSeekGESuccessiveWithBounds(b *testing.B) {
	m, keys := buildMemTable(b)
	iter := m.newIter(&IterOptions{
		UpperBound: keys[1],
	})
	flags := base.SeekGEFlagsNone.EnableTrySeekUsingNext()

	seekKeys := make([][]byte, 256)
	for i := 1; i < len(seekKeys); i++ {
		seekKeys[i] = append(append([]byte(nil), keys[0]...), byte(i-1))
	}

	b.ResetTimer()
	iter.SeekGE(keys[0], base.SeekGEFlagsNone)
	for i := 0; i < b.N-1; i++ {
		iter.SeekGE(seekKeys[i%len(seekKeys)], flags)
	}
}

func BenchmarkMemTableIterNext(b *testing.B) {
	m, _ := buildMemTable(b)
	iter := m.newIter(nil)
	_ = iter.First()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		kv := iter.Next()
		if kv == nil {
			kv = iter.First()
		}
		_ = kv
	}
}

func BenchmarkMemTableIterNextWithBounds(b *testing.B) {
	m, keys := buildMemTable(b)
	// Set bounds to restrict iteration to the middle 50% of keys.
	opts := &IterOptions{
		LowerBound: keys[len(keys)/4],
		UpperBound: keys[3*len(keys)/4],
	}
	iter := m.newIter(opts)
	_ = iter.SeekGE(opts.LowerBound, base.SeekGEFlagsNone)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		kv := iter.Next()
		if kv == nil {
			kv = iter.SeekGE(opts.LowerBound, base.SeekGEFlagsNone)
		}
		_ = kv
	}
}

func BenchmarkMemTableIterPrev(b *testing.B) {
	m, _ := buildMemTable(b)
	iter := m.newIter(nil)
	_ = iter.Last()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		kv := iter.Prev()
		if kv == nil {
			kv = iter.Last()
		}
		_ = kv
	}
}

func BenchmarkMemTableIterPrevWithBounds(b *testing.B) {
	m, keys := buildMemTable(b)
	// Set bounds to restrict iteration to the middle 50% of keys.
	opts := &IterOptions{
		LowerBound: keys[len(keys)/4],
		UpperBound: keys[3*len(keys)/4],
	}
	iter := m.newIter(opts)
	_ = iter.SeekLT(opts.UpperBound, base.SeekLTFlagsNone)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		kv := iter.Prev()
		if kv == nil {
			kv = iter.SeekLT(opts.UpperBound, base.SeekLTFlagsNone)
		}
		_ = kv
	}
}
