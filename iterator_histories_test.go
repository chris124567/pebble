// Copyright 2022 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.
package pebble

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/cockroachdb/datadriven"
	"github.com/cockroachdb/errors"
	"github.com/chris124567/pebble/internal/base"
	"github.com/chris124567/pebble/internal/invariants"
	"github.com/chris124567/pebble/internal/testkeys"
	"github.com/chris124567/pebble/sstable"
	"github.com/chris124567/pebble/vfs"
	"github.com/stretchr/testify/require"
)

// TODO(jackson): Add a range keys test with concurrency: the logic to cache
// fragmented spans is susceptible to races.

func TestIterHistories(t *testing.T) {
	datadriven.Walk(t, "testdata/iter_histories", func(t *testing.T, path string) {
		filename := filepath.Base(path)
		switch {
		case invariants.Enabled && strings.Contains(filename, "no_invariants"):
			t.Skip("disabled when run with -tags invariants due to nondeterminism")
		}

		var d *DB
		var refedCache bool
		var buf bytes.Buffer
		iters := map[string]*Iterator{}
		batches := map[string]*Batch{}
		newIter := func(name string, reader Reader, o *IterOptions) *Iterator {
			it, _ := reader.NewIter(o)
			iters[name] = it
			return it
		}
		var opts *Options
		parseOpts := func(td *datadriven.TestData) (*Options, error) {
			opts = &Options{
				FS:                 vfs.NewMem(),
				Comparer:           testkeys.Comparer,
				FormatMajorVersion: FormatMinSupported,
				BlockPropertyCollectors: []func() BlockPropertyCollector{
					sstable.NewTestKeysBlockPropertyCollector,
				},
				Logger: testLogger{t},
			}
			opts.Experimental.EnableColumnarBlocks = func() bool { return true }
			opts.DisableAutomaticCompactions = true
			opts.EnsureDefaults()
			opts.WithFSDefaults()
			originalCache := opts.Cache
			if err := parseDBOptionsArgs(opts, td.CmdArgs); err != nil {
				return nil, err
			}
			// If the test replaced the cache, we'll need to unref the
			// new cache later.
			refedCache = opts.Cache != originalCache
			return opts, nil
		}
		cleanup := func() (err error) {
			for key, batch := range batches {
				err = firstError(err, batch.Close())
				delete(batches, key)
			}
			for key, iter := range iters {
				err = firstError(err, iter.Close())
				delete(iters, key)
			}

			if d != nil {
				// Close all open snapshots.
				d.mu.Lock()
				var ss []*Snapshot
				l := &d.mu.snapshots
				for i := l.root.next; i != &l.root; i = i.next {
					ss = append(ss, i)
				}
				d.mu.Unlock()
				for i := range ss {
					err = firstError(err, ss[i].Close())
				}

				err = firstError(err, d.Close())
				d = nil
			}

			if refedCache {
				opts.Cache.Unref()
				opts.Cache = nil
				refedCache = false
			}
			return err
		}
		defer cleanup()

		datadriven.RunTest(t, path, func(t *testing.T, td *datadriven.TestData) string {
			buf.Reset()
			switch td.Cmd {
			case "define":
				var err error
				if err := cleanup(); err != nil {
					return err.Error()
				}
				opts, err = parseOpts(td)
				if err != nil {
					return err.Error()
				}
				d, err = runDBDefineCmd(td, opts)
				if err != nil {
					return err.Error()
				}
				return runLSMCmd(td, d)
			case "reopen":
				var err error
				if err := cleanup(); err != nil {
					return err.Error()
				}
				originalCache := opts.Cache
				if err := parseDBOptionsArgs(opts, td.CmdArgs); err != nil {
					return err.Error()
				}
				// If the test replaced the cache, we'll need to unref the
				// new cache later.
				refedCache = originalCache != opts.Cache
				d, err = Open("", opts)
				require.NoError(t, err)
				return ""
			case "reset":
				var err error
				if err := cleanup(); err != nil {
					return err.Error()
				}
				opts, err = parseOpts(td)
				if err != nil {
					return err.Error()
				}

				d, err = Open("", opts)
				require.NoError(t, err)
				return ""
			case "populate":
				b := d.NewBatch()
				runPopulateCmd(t, td, b)
				count := b.Count()
				require.NoError(t, b.Commit(nil))
				return fmt.Sprintf("wrote %d keys\n", count)
			case "batch":
				var name string
				td.MaybeScanArgs(t, "name", &name)
				commit := td.HasArg("commit")
				b := d.NewIndexedBatch()
				require.NoError(t, runBatchDefineCmd(td, b))
				var err error
				if commit {
					func() {
						defer func() {
							if r := recover(); r != nil {
								err = errors.New(r.(string))
							}
						}()
						err = b.Commit(nil)
					}()
				} else if name != "" {
					batches[name] = b
				}
				if err != nil {
					return err.Error()
				}
				count := b.Count()
				if commit {
					return fmt.Sprintf("committed %d keys\n", count)
				}
				return fmt.Sprintf("wrote %d keys to batch %q\n", count, name)
			case "compact":
				if err := runCompactCmd(td, d); err != nil {
					return err.Error()
				}
				return runLSMCmd(td, d)
			case "flush":
				err := d.Flush()
				if err != nil {
					return err.Error()
				}
				return ""
			case "disable-flushes":
				d.mu.Lock()
				d.mu.compact.flushing = true
				d.mu.Unlock()
				return ""
			case "enable-flushes":
				d.mu.Lock()
				d.mu.compact.flushing = false
				d.mu.Unlock()
				return ""
			case "get":
				var reader Reader = d
				if arg, ok := td.Arg("reader"); ok {
					if reader, ok = batches[arg.Vals[0]]; !ok {
						return fmt.Sprintf("unknown reader %q", arg.Vals[0])
					}
				}
				for _, l := range strings.Split(td.Input, "\n") {
					v, closer, err := reader.Get([]byte(l))
					if err != nil {
						fmt.Fprintf(&buf, "%s: error: %s\n", l, err)
					} else {
						fmt.Fprintf(&buf, "%s: %s\n", l, v)
					}
					if err := closer.Close(); err != nil {
						fmt.Fprintf(&buf, "close err: %s\n", err)
					}
				}
				return buf.String()
			case "build":
				if err := runBuildCmd(td, d, d.opts.FS); err != nil {
					return err.Error()
				}
				return ""
			case "ingest-existing":
				if err := runIngestCmd(td, d, d.opts.FS); err != nil {
					return err.Error()
				}
				return ""
			case "ingest":
				if err := runBuildCmd(td, d, d.opts.FS); err != nil {
					return err.Error()
				}
				if err := runIngestCmd(td, d, d.opts.FS); err != nil {
					return err.Error()
				}
				return ""
			case "layout":
				return runLayoutCmd(t, td, d)
			case "lsm":
				return runLSMCmd(td, d)
			case "metrics":
				d.mu.Lock()
				d.waitTableStats()
				d.mu.Unlock()
				m := d.Metrics()
				return fmt.Sprintf("Metrics.Keys.RangeKeySetsCount = %d\n", m.Keys.RangeKeySetsCount)
			case "mutate":
				var batchName string
				td.ScanArgs(t, "batch", &batchName)
				mut := newBatch(d)
				if err := runBatchDefineCmd(td, mut); err != nil {
					return err.Error()
				}
				if err := batches[batchName].Apply(mut, nil); err != nil {
					return err.Error()
				}
				return ""
			case "clone":
				var from, to string
				var cloneOpts CloneOptions
				var iterOpts IterOptions
				td.ScanArgs(t, "from", &from)
				td.ScanArgs(t, "to", &to)
				td.ScanArgs(t, "refresh-batch", &cloneOpts.RefreshBatchView)
				fromIter := iters[from]
				if foundAny, err := parseIterOptions(&iterOpts, &fromIter.opts, strings.Fields(td.Input)); err != nil {
					return fmt.Sprintf("clone: %s", err.Error())
				} else if foundAny {
					cloneOpts.IterOptions = &iterOpts
				}
				var err error
				iters[to], err = fromIter.Clone(cloneOpts)
				if err != nil {
					return err.Error()
				}
				return ""
			case "commit":
				name := pluckStringCmdArg(td, "batch")
				b := batches[name]
				defer b.Close()
				count := b.Count()
				require.NoError(t, d.Apply(b, nil))
				delete(batches, name)
				return fmt.Sprintf("committed %d keys\n", count)
			case "combined-iter":
				o := &IterOptions{KeyTypes: IterKeyTypePointsAndRanges}
				var reader Reader = d
				var name string
				for _, arg := range td.CmdArgs {
					switch arg.Key {
					case "mask-suffix":
						o.RangeKeyMasking.Suffix = []byte(arg.Vals[0])
					case "mask-filter":
						o.RangeKeyMasking.Filter = func() BlockPropertyFilterMask {
							return sstable.NewTestKeysMaskingFilter()
						}
					case "lower":
						o.LowerBound = []byte(arg.Vals[0])
					case "upper":
						o.UpperBound = []byte(arg.Vals[0])
					case "name":
						name = arg.Vals[0]
					case "reader":
						reader = batches[arg.Vals[0]]
						if reader == nil {
							return fmt.Sprintf("unknown reader %q", arg.Vals[0])
						}
					case "point-key-filter":
						if len(arg.Vals) != 2 {
							return fmt.Sprintf("blockprop-filter expects 2 arguments, received %d", len(arg.Vals))
						}
						min, err := strconv.ParseUint(arg.Vals[0], 10, 64)
						if err != nil {
							return err.Error()
						}
						max, err := strconv.ParseUint(arg.Vals[1], 10, 64)
						if err != nil {
							return err.Error()
						}
						o.PointKeyFilters = []sstable.BlockPropertyFilter{
							sstable.NewTestKeysBlockPropertyFilter(min, max),
						}
						o.SkipPoint = func(k []byte) bool {
							i := testkeys.Comparer.Split(k)
							if i == len(k) {
								return false
							}
							v, err := testkeys.ParseSuffix(k[i:])
							if err != nil {
								return false
							}
							return uint64(v) < min || uint64(v) >= max
						}
					case "snapshot":
						s := base.ParseSeqNum(arg.Vals[0])
						func() {
							d.mu.Lock()
							defer d.mu.Unlock()
							l := &d.mu.snapshots
							for i := l.root.next; i != &l.root; i = i.next {
								if i.seqNum == s {
									reader = i
									break
								}
							}
						}()
					case "use-l6-filter":
						o.UseL6Filters = true
					}
				}
				var iter *Iterator
				var err error
				func() {
					defer func() {
						if r := recover(); r != nil {
							switch v := r.(type) {
							case string:
								err = errors.New(v)
							case error:
								err = v
							default:
								panic(r)
							}
						}
					}()
					iter = newIter(name, reader, o)
				}()
				if err != nil {
					return err.Error()
				}
				return runIterCmd(td, iter, name == "" /* close iter */)
			case "rangekey-iter":
				name := pluckStringCmdArg(td, "name")
				iter := newIter(name, d, &IterOptions{KeyTypes: IterKeyTypeRangesOnly})
				return runIterCmd(td, iter, name == "" /* close iter */)
			case "scan-rangekeys":
				iter := newIter(
					pluckStringCmdArg(td, "name"),
					d,
					&IterOptions{KeyTypes: IterKeyTypeRangesOnly},
				)
				func() {
					defer iter.Close()
					for iter.First(); iter.Valid(); iter.Next() {
						start, end := iter.RangeBounds()
						fmt.Fprintf(&buf, "[%s, %s)\n", start, end)
						writeRangeKeys(&buf, iter)
						fmt.Fprintln(&buf)
					}
				}()
				return buf.String()
			case "iter":
				var name string
				td.ScanArgs(t, "iter", &name)
				return runIterCmd(td, iters[name], false /* close iter */)
			case "wait-table-stats":
				d.mu.Lock()
				d.waitTableStats()
				d.mu.Unlock()
				return ""
			default:
				return fmt.Sprintf("unknown command %q", td.Cmd)
			}
		})
	})
}

func pluckStringCmdArg(td *datadriven.TestData, key string) string {
	if arg, ok := td.Arg(key); ok {
		return arg.Vals[0]
	}
	return ""
}
