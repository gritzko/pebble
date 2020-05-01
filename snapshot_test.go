// Copyright 2012 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package pebble

import (
	"bytes"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/cockroachdb/pebble/internal/datadriven"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/stretchr/testify/require"
)

func TestSnapshotListToSlice(t *testing.T) {
	testCases := []struct {
		vals []uint64
	}{
		{nil},
		{[]uint64{1}},
		{[]uint64{1, 2, 3}},
		{[]uint64{3, 2, 1}},
	}
	for _, c := range testCases {
		t.Run("", func(t *testing.T) {
			var l snapshotList
			l.init()
			for _, v := range c.vals {
				l.pushBack(&Snapshot{seqNum: v})
			}
			slice := l.toSlice()
			if !reflect.DeepEqual(c.vals, slice) {
				t.Fatalf("expected %d, but got %d", c.vals, slice)
			}
		})
	}
}

func TestSnapshot(t *testing.T) {
	var d *DB
	var snapshots map[string]*Snapshot

	close := func() {
		for _, s := range snapshots {
			require.NoError(t, s.Close())
		}
		snapshots = nil
		if d != nil {
			require.NoError(t, d.Close())
			d = nil
		}
	}
	defer close()

	datadriven.RunTest(t, "testdata/snapshot", func(td *datadriven.TestData) string {
		switch td.Cmd {
		case "define":
			close()

			var err error
			d, err = Open("", &Options{
				FS: vfs.NewMem(),
			})
			if err != nil {
				return err.Error()
			}
			snapshots = make(map[string]*Snapshot)

			for _, line := range strings.Split(td.Input, "\n") {
				parts := strings.Fields(line)
				if len(parts) == 0 {
					continue
				}
				var err error
				switch parts[0] {
				case "set":
					if len(parts) != 3 {
						return fmt.Sprintf("%s expects 2 arguments", parts[0])
					}
					err = d.Set([]byte(parts[1]), []byte(parts[2]), nil)
				case "del":
					if len(parts) != 2 {
						return fmt.Sprintf("%s expects 1 argument", parts[0])
					}
					err = d.Delete([]byte(parts[1]), nil)
				case "merge":
					if len(parts) != 3 {
						return fmt.Sprintf("%s expects 2 arguments", parts[0])
					}
					err = d.Merge([]byte(parts[1]), []byte(parts[2]), nil)
				case "snapshot":
					if len(parts) != 2 {
						return fmt.Sprintf("%s expects 1 argument", parts[0])
					}
					snapshots[parts[1]] = d.NewSnapshot()
				case "compact":
					if len(parts) != 2 {
						return fmt.Sprintf("%s expects 1 argument", parts[0])
					}
					keys := strings.Split(parts[1], "-")
					if len(keys) != 2 {
						return fmt.Sprintf("malformed key range: %s", parts[1])
					}
					err = d.Compact([]byte(keys[0]), []byte(keys[1]))
				default:
					return fmt.Sprintf("unknown op: %s", parts[0])
				}
				if err != nil {
					return err.Error()
				}
			}
			return ""

		case "iter":
			var iter *Iterator
			if len(td.CmdArgs) == 1 {
				if td.CmdArgs[0].Key != "snapshot" {
					return fmt.Sprintf("unknown argument: %s", td.CmdArgs[0])
				}
				if len(td.CmdArgs[0].Vals) != 1 {
					return fmt.Sprintf("%s expects 1 value: %s", td.CmdArgs[0].Key, td.CmdArgs[0])
				}
				name := td.CmdArgs[0].Vals[0]
				snapshot := snapshots[name]
				if snapshot == nil {
					return fmt.Sprintf("unable to find snapshot \"%s\"", name)
				}
				iter = snapshot.NewIter(nil)
			} else {
				iter = d.NewIter(nil)
			}
			defer iter.Close()

			var b bytes.Buffer
			for _, line := range strings.Split(td.Input, "\n") {
				parts := strings.Fields(line)
				if len(parts) == 0 {
					continue
				}
				switch parts[0] {
				case "first":
					iter.First()
				case "last":
					iter.Last()
				case "seek-ge":
					if len(parts) != 2 {
						return fmt.Sprintf("seek-ge <key>\n")
					}
					iter.SeekGE([]byte(strings.TrimSpace(parts[1])))
				case "seek-lt":
					if len(parts) != 2 {
						return fmt.Sprintf("seek-lt <key>\n")
					}
					iter.SeekLT([]byte(strings.TrimSpace(parts[1])))
				case "next":
					iter.Next()
				case "prev":
					iter.Prev()
				default:
					return fmt.Sprintf("unknown op: %s", parts[0])
				}
				if iter.Valid() {
					fmt.Fprintf(&b, "%s:%s\n", iter.Key(), iter.Value())
				} else if err := iter.Error(); err != nil {
					fmt.Fprintf(&b, "err=%v\n", err)
				} else {
					fmt.Fprintf(&b, ".\n")
				}
			}
			return b.String()

		default:
			return fmt.Sprintf("unknown command: %s", td.Cmd)
		}
	})
}

func TestSnapshotClosed(t *testing.T) {
	d, err := Open("", &Options{
		FS: vfs.NewMem(),
	})
	require.NoError(t, err)

	catch := func(f func()) (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = r.(error)
			}
		}()
		f()
		return nil
	}

	snap := d.NewSnapshot()
	require.NoError(t, snap.Close())
	require.EqualValues(t, ErrClosed, catch(func() { _ = snap.Close() }))
	require.EqualValues(t, ErrClosed, catch(func() { _, _, _ = snap.Get(nil) }))
	require.EqualValues(t, ErrClosed, catch(func() { snap.NewIter(nil) }))

	require.NoError(t, d.Close())
}

func TestSnapshotRangeDeletionStress(t *testing.T) {
	const runs = 200
	const middleKey = runs * runs

	d, err := Open("", &Options{
		FS: vfs.NewMem(),
	})
	require.NoError(t, err)

	mkkey := func(k int) []byte {
		return []byte(fmt.Sprintf("%08d", k))
	}
	v := []byte("hello world")

	snapshots := make([]*Snapshot, 0, runs)
	for r := 0; r < runs; r++ {
		// We use a keyspace that is 2*runs*runs wide. In other words there are
		// 2*runs sections of the keyspace, each with runs elements. On every
		// run, we write to the r-th element of each section of the keyspace.
		for i := 0; i < 2*runs; i++ {
			err := d.Set(mkkey(runs*i+r), v, nil)
			require.NoError(t, err)
		}

		// Now we delete some of the keyspace through a DeleteRange. We delete from
		// the middle of the keyspace outwards. The keyspace is made of 2*runs
		// sections, and we delete an additional two of these sections per run.
		err := d.DeleteRange(mkkey(middleKey-runs*r), mkkey(middleKey+runs*r), nil)
		require.NoError(t, err)

		snapshots = append(snapshots, d.NewSnapshot())
	}

	// Check that all the snapshots contain the expected number of keys.
	// Iterating over so many keys is slow, so do it in parallel.
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.NumCPU())
	for r := range snapshots {
		wg.Add(1)
		sem <- struct{}{}
		go func(r int) {
			defer func() {
				<-sem
				wg.Done()
			}()

			// Count the keys at this snapshot.
			iter := snapshots[r].NewIter(nil)
			var keysFound int
			for iter.First(); iter.Valid(); iter.Next() {
				keysFound++
			}
			err := firstError(iter.Error(), iter.Close())
			if err != nil {
				t.Error(err)
				return
			}

			// At the time that this snapshot was taken, (r+1)*2*runs unique keys
			// were Set (one in each of the 2*runs sections per run).  But this
			// run also deleted the 2*r middlemost sections.  When this snapshot
			// was taken, a Set to each of those sections had been made (r+1)
			// times, so 2*r*(r+1) previously-set keys are now deleted.

			keysExpected := (r+1)*2*runs - 2*r*(r+1)
			if keysFound != keysExpected {
				t.Errorf("%d: found %d keys, want %d", r, keysFound, keysExpected)
			}
			if err := snapshots[r].Close(); err != nil {
				t.Error(err)
			}
		}(r)
	}
	wg.Wait()
	require.NoError(t, d.Close())
}
