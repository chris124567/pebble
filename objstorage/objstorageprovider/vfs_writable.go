// Copyright 2023 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package objstorageprovider

import (
	"bufio"

	"github.com/chris124567/pebble/internal/invariants"
	"github.com/chris124567/pebble/objstorage"
	"github.com/chris124567/pebble/vfs"
)

// NewFileWritable returns a Writable that uses a file as underlying storage.
func NewFileWritable(file vfs.File) objstorage.Writable {
	return newFileBufferedWritable(file)
}

type fileBufferedWritable struct {
	file vfs.File
	bw   *bufio.Writer
}

var _ objstorage.Writable = (*fileBufferedWritable)(nil)

func newFileBufferedWritable(file vfs.File) *fileBufferedWritable {
	return &fileBufferedWritable{
		file: file,
		bw:   bufio.NewWriter(file),
	}
}

// Write is part of the objstorage.Writable interface.
func (w *fileBufferedWritable) Write(p []byte) error {
	// Ignoring the length written since bufio.Writer.Write is guaranteed to
	// return an error if the length written is < len(p).
	_, err := w.bw.Write(p)

	// Write is allowed to mangle the buffer. Do it sometimes in invariant builds
	// to catch callers that don't handle this.
	if invariants.Enabled && invariants.Sometimes(1) {
		for i := range p {
			p[i] = 0xFF
		}
	}
	return err
}

// Finish is part of the objstorage.Writable interface.
func (w *fileBufferedWritable) Finish() error {
	err := w.bw.Flush()
	if err == nil {
		err = w.file.Sync()
	}
	err = firstError(err, w.file.Close())
	w.bw = nil
	w.file = nil
	return err
}

// Abort is part of the objstorage.Writable interface.
func (w *fileBufferedWritable) Abort() {
	_ = w.file.Close()
	w.bw = nil
	w.file = nil
}

func firstError(err0, err1 error) error {
	if err0 != nil {
		return err0
	}
	return err1
}
