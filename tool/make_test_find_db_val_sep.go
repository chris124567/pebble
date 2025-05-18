// Copyright 2025 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

//go:build make_test_find_db

// Run using: go run -tags make_test_find_db_val_sep ./tool/make_test_find_db_val_sep.go
package main

import (
	"log"

	"github.com/chris124567/pebble"
	"github.com/chris124567/pebble/cockroachkvs"
	"github.com/chris124567/pebble/internal/blobtest"
	"github.com/chris124567/pebble/sstable"
	"github.com/chris124567/pebble/vfs"
)

const minSizeForValSep = 3

type db struct {
	db *pebble.DB
	bv blobtest.Values
}

func (d *db) set(key, value string) {
	encodedKey := cockroachkvs.ParseFormattedKey(key)
	if err := d.db.Set(encodedKey, []byte(value), nil); err != nil {
		log.Fatal(err)
	}
}

func (d *db) flush() {
	if err := d.db.Flush(); err != nil {
		log.Fatal(err)
	}
}

func (d *db) close() {
	if err := d.db.Close(); err != nil {
		log.Fatal(err)
	}
}

func main() {
	const dir = "tool/testdata/find-db-val-sep"

	fs := vfs.Default
	if err := fs.RemoveAll(dir); err != nil {
		log.Fatal(err)
	}
	opts := pebble.Options{
		FS:        fs,
		KeySchema: cockroachkvs.KeySchema.Name,
		KeySchemas: sstable.KeySchemas{
			cockroachkvs.KeySchema.Name: &cockroachkvs.KeySchema,
		},
		FormatMajorVersion: pebble.FormatExperimentalValueSeparation,
	}
	opts.Experimental.ValueSeparationPolicy = func() pebble.ValueSeparationPolicy {
		return pebble.ValueSeparationPolicy{
			Enabled:               true,
			MinimumSize:           minSizeForValSep,
			MaxBlobReferenceDepth: 10,
		}
	}
	d, err := pebble.Open(dir, &opts)
	if err != nil {
		return
	}
	tdb := &db{
		db: d,
	}
	defer tdb.close()

	tdb.set("aaa", "yuumi")
	tdb.set("bbb", "mai")
	tdb.set("ccc", "poiandyaya")
	tdb.set("ddd", "6")
	tdb.flush()
}
