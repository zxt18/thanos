// Copyright (c) The Thanos Authors.
// Licensed under the Apache License 2.0.

package block

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/oklog/ulid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/thanos-io/thanos/pkg/block/metadata"
	"github.com/thanos-io/thanos/pkg/objstore"
	"github.com/thanos-io/thanos/pkg/testutil"
	"github.com/thanos-io/thanos/pkg/testutil/e2eutil"
)

func TestIsBlockDir(t *testing.T) {
	for _, tc := range []struct {
		input string
		id    ulid.ULID
		bdir  bool
	}{
		{
			input: "",
			bdir:  false,
		},
		{
			input: "something",
			bdir:  false,
		},
		{
			id:    ulid.MustNew(1, nil),
			input: ulid.MustNew(1, nil).String(),
			bdir:  true,
		},
		{
			id:    ulid.MustNew(2, nil),
			input: "/" + ulid.MustNew(2, nil).String(),
			bdir:  true,
		},
		{
			id:    ulid.MustNew(3, nil),
			input: "some/path/" + ulid.MustNew(3, nil).String(),
			bdir:  true,
		},
		{
			input: ulid.MustNew(4, nil).String() + "/something",
			bdir:  false,
		},
	} {
		t.Run(tc.input, func(t *testing.T) {
			id, ok := IsBlockDir(tc.input)
			testutil.Equals(t, tc.bdir, ok)

			if id.Compare(tc.id) != 0 {
				t.Errorf("expected %s got %s", tc.id, id)
				t.FailNow()
			}
		})
	}
}

func TestUpload(t *testing.T) {
	defer testutil.TolerantVerifyLeak(t)

	ctx := context.Background()

	tmpDir, err := ioutil.TempDir("", "test-block-upload")
	testutil.Ok(t, err)
	defer func() { testutil.Ok(t, os.RemoveAll(tmpDir)) }()

	bkt := objstore.NewInMemBucket()
	b1, err := e2eutil.CreateBlock(ctx, tmpDir, []labels.Labels{
		{{Name: "a", Value: "1"}},
		{{Name: "a", Value: "2"}},
		{{Name: "a", Value: "3"}},
		{{Name: "a", Value: "4"}},
		{{Name: "b", Value: "1"}},
	}, 100, 0, 1000, labels.Labels{{Name: "ext1", Value: "val1"}}, 124)
	testutil.Ok(t, err)
	testutil.Ok(t, os.MkdirAll(path.Join(tmpDir, "test", b1.String()), os.ModePerm))

	{
		// Wrong dir.
		err := Upload(ctx, log.NewNopLogger(), bkt, path.Join(tmpDir, "not-existing"))
		testutil.NotOk(t, err)
		testutil.Assert(t, strings.HasSuffix(err.Error(), "/not-existing: no such file or directory"), "")
	}
	{
		// Wrong existing dir (not a block).
		err := Upload(ctx, log.NewNopLogger(), bkt, path.Join(tmpDir, "test"))
		testutil.NotOk(t, err)
		testutil.Equals(t, "not a block dir: ulid: bad data size when unmarshaling", err.Error())
	}
	{
		// Empty block dir.
		err := Upload(ctx, log.NewNopLogger(), bkt, path.Join(tmpDir, "test", b1.String()))
		testutil.NotOk(t, err)
		testutil.Assert(t, strings.HasSuffix(err.Error(), "/meta.json: no such file or directory"), "")
	}
	e2eutil.Copy(t, path.Join(tmpDir, b1.String(), MetaFilename), path.Join(tmpDir, "test", b1.String(), MetaFilename))
	{
		// Missing chunks.
		err := Upload(ctx, log.NewNopLogger(), bkt, path.Join(tmpDir, "test", b1.String()))
		testutil.NotOk(t, err)
		testutil.Assert(t, strings.HasSuffix(err.Error(), "/chunks: no such file or directory"), err.Error())
	}
	testutil.Ok(t, os.MkdirAll(path.Join(tmpDir, "test", b1.String(), ChunksDirname), os.ModePerm))
	e2eutil.Copy(t, path.Join(tmpDir, b1.String(), ChunksDirname, "000001"), path.Join(tmpDir, "test", b1.String(), ChunksDirname, "000001"))
	{
		// Missing index file.
		err := Upload(ctx, log.NewNopLogger(), bkt, path.Join(tmpDir, "test", b1.String()))
		testutil.NotOk(t, err)
		testutil.Assert(t, strings.HasSuffix(err.Error(), "/index: no such file or directory"), "")
	}
	e2eutil.Copy(t, path.Join(tmpDir, b1.String(), IndexFilename), path.Join(tmpDir, "test", b1.String(), IndexFilename))
	testutil.Ok(t, os.Remove(path.Join(tmpDir, "test", b1.String(), MetaFilename)))
	{
		// Missing meta.json file.
		err := Upload(ctx, log.NewNopLogger(), bkt, path.Join(tmpDir, "test", b1.String()))
		testutil.NotOk(t, err)
		testutil.Assert(t, strings.HasSuffix(err.Error(), "/meta.json: no such file or directory"), "")
	}
	e2eutil.Copy(t, path.Join(tmpDir, b1.String(), MetaFilename), path.Join(tmpDir, "test", b1.String(), MetaFilename))
	{
		// Full block.
		testutil.Ok(t, Upload(ctx, log.NewNopLogger(), bkt, path.Join(tmpDir, "test", b1.String())))
		testutil.Equals(t, 4, len(bkt.Objects()))
		testutil.Equals(t, 3751, len(bkt.Objects()[path.Join(b1.String(), ChunksDirname, "000001")]))
		testutil.Equals(t, 401, len(bkt.Objects()[path.Join(b1.String(), IndexFilename)]))
		testutil.Equals(t, 562, len(bkt.Objects()[path.Join(b1.String(), MetaFilename)]))

		// File stats are gathered.
		testutil.Equals(t, fmt.Sprintf(`{
	"ulid": "%s",
	"minTime": 0,
	"maxTime": 1000,
	"stats": {
		"numSamples": 500,
		"numSeries": 5,
		"numChunks": 5
	},
	"compaction": {
		"level": 1,
		"sources": [
			"%s"
		]
	},
	"version": 1,
	"thanos": {
		"version": 1,
		"labels": {
			"ext1": "val1"
		},
		"downsample": {
			"resolution": 124
		},
		"source": "test",
		"files": [
			{
				"rel_path": "meta.json"
			},
			{
				"rel_path": "index",
				"size_bytes": 401
			},
			{
				"rel_path": "chunks/000001",
				"size_bytes": 3751
			}
		]
	}
}
`, b1.String(), b1.String()), string(bkt.Objects()[path.Join(b1.String(), MetaFilename)]))
	}
	{
		// Test Upload is idempotent.
		testutil.Ok(t, Upload(ctx, log.NewNopLogger(), bkt, path.Join(tmpDir, "test", b1.String())))
		testutil.Equals(t, 4, len(bkt.Objects()))
		testutil.Equals(t, 3751, len(bkt.Objects()[path.Join(b1.String(), ChunksDirname, "000001")]))
		testutil.Equals(t, 401, len(bkt.Objects()[path.Join(b1.String(), IndexFilename)]))
		testutil.Equals(t, 562, len(bkt.Objects()[path.Join(b1.String(), MetaFilename)]))
	}
	{
		// Upload with no external labels should be blocked.
		b2, err := e2eutil.CreateBlock(ctx, tmpDir, []labels.Labels{
			{{Name: "a", Value: "1"}},
			{{Name: "a", Value: "2"}},
			{{Name: "a", Value: "3"}},
			{{Name: "a", Value: "4"}},
			{{Name: "b", Value: "1"}},
		}, 100, 0, 1000, nil, 124)
		testutil.Ok(t, err)
		err = Upload(ctx, log.NewNopLogger(), bkt, path.Join(tmpDir, b2.String()))
		testutil.NotOk(t, err)
		testutil.Equals(t, "empty external labels are not allowed for Thanos block.", err.Error())
		testutil.Equals(t, 4, len(bkt.Objects()))
	}
}

func TestDelete(t *testing.T) {
	defer testutil.TolerantVerifyLeak(t)
	ctx := context.Background()

	tmpDir, err := ioutil.TempDir("", "test-block-delete")
	testutil.Ok(t, err)
	defer func() { testutil.Ok(t, os.RemoveAll(tmpDir)) }()

	bkt := objstore.NewInMemBucket()
	{
		b1, err := e2eutil.CreateBlock(ctx, tmpDir, []labels.Labels{
			{{Name: "a", Value: "1"}},
			{{Name: "a", Value: "2"}},
			{{Name: "a", Value: "3"}},
			{{Name: "a", Value: "4"}},
			{{Name: "b", Value: "1"}},
		}, 100, 0, 1000, labels.Labels{{Name: "ext1", Value: "val1"}}, 124)
		testutil.Ok(t, err)
		testutil.Ok(t, Upload(ctx, log.NewNopLogger(), bkt, path.Join(tmpDir, b1.String())))
		testutil.Equals(t, 4, len(bkt.Objects()))

		// Full delete.
		testutil.Ok(t, Delete(ctx, log.NewNopLogger(), bkt, b1))
		// Still debug meta entry is expected.
		testutil.Equals(t, 1, len(bkt.Objects()))
	}
	{
		b2, err := e2eutil.CreateBlock(ctx, tmpDir, []labels.Labels{
			{{Name: "a", Value: "1"}},
			{{Name: "a", Value: "2"}},
			{{Name: "a", Value: "3"}},
			{{Name: "a", Value: "4"}},
			{{Name: "b", Value: "1"}},
		}, 100, 0, 1000, labels.Labels{{Name: "ext1", Value: "val1"}}, 124)
		testutil.Ok(t, err)
		testutil.Ok(t, Upload(ctx, log.NewNopLogger(), bkt, path.Join(tmpDir, b2.String())))
		testutil.Equals(t, 5, len(bkt.Objects()))

		// Remove meta.json and check if delete can delete it.
		testutil.Ok(t, bkt.Delete(ctx, path.Join(b2.String(), MetaFilename)))
		testutil.Ok(t, Delete(ctx, log.NewNopLogger(), bkt, b2))
		// Still 2 debug meta entries are expected.
		testutil.Equals(t, 2, len(bkt.Objects()))
	}
}

func TestMarkForDeletion(t *testing.T) {
	defer testutil.TolerantVerifyLeak(t)
	ctx := context.Background()

	tmpDir, err := ioutil.TempDir("", "test-block-mark-for-delete")
	testutil.Ok(t, err)
	defer func() { testutil.Ok(t, os.RemoveAll(tmpDir)) }()

	for _, tcase := range []struct {
		name      string
		preUpload func(t testing.TB, id ulid.ULID, bkt objstore.Bucket)

		blocksMarked int
	}{
		{
			name:         "block marked for deletion",
			preUpload:    func(t testing.TB, id ulid.ULID, bkt objstore.Bucket) {},
			blocksMarked: 1,
		},
		{
			name: "block with deletion mark already, expected log and no metric increment",
			preUpload: func(t testing.TB, id ulid.ULID, bkt objstore.Bucket) {
				deletionMark, err := json.Marshal(metadata.DeletionMark{
					ID:           id,
					DeletionTime: time.Now().Unix(),
					Version:      metadata.DeletionMarkVersion1,
				})
				testutil.Ok(t, err)
				testutil.Ok(t, bkt.Upload(ctx, path.Join(id.String(), metadata.DeletionMarkFilename), bytes.NewReader(deletionMark)))
			},
			blocksMarked: 0,
		},
	} {
		t.Run(tcase.name, func(t *testing.T) {
			bkt := objstore.NewInMemBucket()
			id, err := e2eutil.CreateBlock(ctx, tmpDir, []labels.Labels{
				{{Name: "a", Value: "1"}},
				{{Name: "a", Value: "2"}},
				{{Name: "a", Value: "3"}},
				{{Name: "a", Value: "4"}},
				{{Name: "b", Value: "1"}},
			}, 100, 0, 1000, labels.Labels{{Name: "ext1", Value: "val1"}}, 124)
			testutil.Ok(t, err)

			tcase.preUpload(t, id, bkt)

			testutil.Ok(t, Upload(ctx, log.NewNopLogger(), bkt, path.Join(tmpDir, id.String())))

			c := promauto.With(nil).NewCounter(prometheus.CounterOpts{})
			err = MarkForDeletion(ctx, log.NewNopLogger(), bkt, id, c)
			testutil.Ok(t, err)
			testutil.Equals(t, float64(tcase.blocksMarked), promtest.ToFloat64(c))
		})
	}
}
