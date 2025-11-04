// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package digest

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

func TestStore(t *testing.T) {
	cmpOpts := []cmp.Option{
		cmp.AllowUnexported(Data{}),
		cmp.AllowUnexported(byteSource{}),
	}

	ds := NewStore()
	d1 := FromBytes("123", []byte{1, 2, 3})
	dg1 := d1.Digest()

	// It should fail because the Data is not set yet.
	_, ok := ds.Get(dg1)
	if ok {
		t.Errorf("ds.Get(%v) = _, true, want false", dg1)
	}

	// Set the 1st data.
	ds.Set(d1)

	// This time, it should be able to retrieve the stored Data.
	dGot, ok := ds.Get(dg1)
	if !ok {
		t.Errorf("ds.Get(%v) = _, false, want true", dg1)
	}
	if diff := cmp.Diff(d1, dGot, cmpOpts...); diff != "" {
		t.Errorf("ds.Get(%v): diff -want +got:\n%s", dg1, diff)
	}
	sGot, ok := ds.GetSource(dg1)
	if !ok {
		t.Errorf("ds.GetSource(%v) = _, false, want true", dg1)
	}
	if diff := cmp.Diff(d1.source, sGot, cmpOpts...); diff != "" {
		t.Errorf("ds.GetSource(%v): diff -want +got:\n%s", dg1, diff)
	}

	// Set the 2nd data.
	d2 := FromBytes("abc", []byte("abc"))
	dg2 := d2.Digest()
	ds.Set(d2)

	// check size
	sizeGot := ds.Size()
	sizeWant := 2
	if sizeGot != sizeWant {
		t.Errorf("ds.Size() = %d, want %d", sizeGot, sizeWant)
	}

	// list digests
	listGot := ds.List()
	listWant := []Digest{dg1, dg2}
	ignoreOrder := cmpopts.SortSlices(func(x, y Digest) bool { return x.String() < y.String() })
	if diff := cmp.Diff(listWant, listGot, ignoreOrder); diff != "" {
		t.Errorf("ds.List(): diff -want +got:\n%s", diff)
	}
}
