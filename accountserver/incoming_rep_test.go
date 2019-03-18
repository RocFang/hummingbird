//  Copyright (c) 2016-2017 Rackspace
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
//  implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.

package accountserver

import (
	"bytes"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/RocFang/hummingbird/common"
	"github.com/RocFang/hummingbird/common/test"
)

func TestServerReplicateSync(t *testing.T) {
	handler, cleanup, err := makeTestServer()
	require.Nil(t, err)
	defer cleanup()

	rsp := test.MakeCaptureResponse()
	req, err := http.NewRequest("PUT", "/device/1/a", nil)
	require.Nil(t, err)
	req.Header.Set("X-Timestamp", common.CanonicalTimestamp(100))
	handler.ServeHTTP(rsp, req)
	require.Equal(t, 201, rsp.Status)

	h := md5.New()
	fmt.Fprintf(h, "%s/%s%s", "changeme", "a", "changeme")
	accountHash := fmt.Sprintf("%032x", h.Sum(nil))
	pretendLocalID := common.UUID()

	syncRequest := func(maxRow int64, hash, pretendLocalID, createdAt, putTimestamp, deleteTimestamp, metadata string) AccountInfo {
		replRequest := []interface{}{"sync", maxRow, hash, pretendLocalID, createdAt, putTimestamp, deleteTimestamp, metadata}
		msg, err := json.Marshal(replRequest)
		require.Nil(t, err)
		rsp = test.MakeCaptureResponse()
		req, err = http.NewRequest("REPLICATE", "/device/1/"+accountHash, bytes.NewBuffer(msg))
		require.Nil(t, err)
		handler.ServeHTTP(rsp, req)
		require.Equal(t, http.StatusOK, rsp.Status)
		var response AccountInfo
		require.Nil(t, json.Unmarshal(rsp.Body.Bytes(), &response))
		return response
	}

	// sync update point if hashes match
	info := syncRequest(10, "00000000000000000000000000000000", pretendLocalID, common.CanonicalTimestamp(100), common.CanonicalTimestamp(100), "", "{}")
	require.Equal(t, int64(10), info.Point)

	// sync update metadata
	info = syncRequest(10, "00000000000000000000000000000000", pretendLocalID, common.CanonicalTimestamp(100), common.CanonicalTimestamp(100), "", "{\"X-Account-Meta-Key\": [\"value\", \"0000000100.00000\"]}")
	rsp = test.MakeCaptureResponse()
	req, err = http.NewRequest("HEAD", "/device/1/a", nil)
	require.Nil(t, err)
	handler.ServeHTTP(rsp, req)
	require.Equal(t, "value", rsp.Header().Get("X-Account-Meta-Key"))

	// sync update delete timestamp
	info = syncRequest(10, "00000000000000000000000000000000", pretendLocalID, common.CanonicalTimestamp(100), common.CanonicalTimestamp(100), common.CanonicalTimestamp(101), "{}")
	rsp = test.MakeCaptureResponse()
	req, err = http.NewRequest("HEAD", "/device/1/a", nil)
	require.Nil(t, err)
	handler.ServeHTTP(rsp, req)
	require.Equal(t, http.StatusNotFound, rsp.Status)
}

func TestServerReplicateMergeItems(t *testing.T) {
	handler, cleanup, err := makeTestServer()
	require.Nil(t, err)
	defer cleanup()

	// create a container
	rsp := test.MakeCaptureResponse()
	req, err := http.NewRequest("PUT", "/device/1/a", nil)
	require.Nil(t, err)
	req.Header.Set("X-Timestamp", common.CanonicalTimestamp(100))
	handler.ServeHTTP(rsp, req)
	require.Equal(t, http.StatusCreated, rsp.Status)

	h := md5.New()
	fmt.Fprintf(h, "%s/%s%s", "changeme", "a", "changeme")
	accountHash := fmt.Sprintf("%032x", h.Sum(nil))
	pretendLocalID := common.UUID()

	mergeRequest := func(records []ContainerRecord) {
		replRequest := []interface{}{"merge_items", records, pretendLocalID}
		msg, err := json.Marshal(replRequest)
		require.Nil(t, err)
		rsp = test.MakeCaptureResponse()
		req, err = http.NewRequest("REPLICATE", "/device/1/"+accountHash, bytes.NewBuffer(msg))
		require.Nil(t, err)
		handler.ServeHTTP(rsp, req)
		require.Equal(t, http.StatusAccepted, rsp.Status)
	}

	// merge a new object
	mergeRequest([]ContainerRecord{
		{
			Rowid:        0,
			Name:         "a container",
			PutTimestamp: common.CanonicalTimestamp(100),
			ObjectCount:  1,
			BytesUsed:    100,
		},
	})
	rsp = test.MakeCaptureResponse()
	req, err = http.NewRequest("HEAD", "/device/1/a", nil)
	require.Nil(t, err)
	handler.ServeHTTP(rsp, req)
	require.Equal(t, "1", rsp.Header().Get("X-Account-Container-Count"))
	require.Equal(t, "100", rsp.Header().Get("X-Account-Bytes-Used"))

	// merge a delete for that object
	mergeRequest([]ContainerRecord{
		{
			Rowid:           0,
			Name:            "a container",
			PutTimestamp:    common.CanonicalTimestamp(101),
			DeleteTimestamp: common.CanonicalTimestamp(102),
			ObjectCount:     0,
			BytesUsed:       0,
			Deleted:         1,
		},
	})
	rsp = test.MakeCaptureResponse()
	req, err = http.NewRequest("HEAD", "/device/1/a", nil)
	require.Nil(t, err)
	handler.ServeHTTP(rsp, req)
	require.Equal(t, "0", rsp.Header().Get("X-Account-Object-Count"))
	require.Equal(t, "0", rsp.Header().Get("X-Account-Bytes-Used"))
}

func TestServerReplicateMergeItemsNotFound(t *testing.T) {
	handler, cleanup, err := makeTestServer()
	require.Nil(t, err)
	defer cleanup()

	records := []ContainerRecord{}

	replRequest := []interface{}{"merge_items", records, common.UUID()}
	msg, err := json.Marshal(replRequest)
	require.Nil(t, err)
	rsp := test.MakeCaptureResponse()
	req, err := http.NewRequest("REPLICATE", "/device/1/ffffffffffffffffffffffffffffffff", bytes.NewBuffer(msg))
	require.Nil(t, err)
	handler.ServeHTTP(rsp, req)
	require.Equal(t, http.StatusNotFound, rsp.Status)
}

func TestServerReplicateMergeSyncsNotFound(t *testing.T) {
	handler, cleanup, err := makeTestServer()
	require.Nil(t, err)
	defer cleanup()

	records := []*SyncRecord{}

	replRequest := []interface{}{"merge_syncs", records}
	msg, err := json.Marshal(replRequest)
	require.Nil(t, err)
	rsp := test.MakeCaptureResponse()
	req, err := http.NewRequest("REPLICATE", "/device/1/ffffffffffffffffffffffffffffffff", bytes.NewBuffer(msg))
	require.Nil(t, err)
	handler.ServeHTTP(rsp, req)
	require.Equal(t, http.StatusNotFound, rsp.Status)
}

func TestServerReplicateCompleteRsyncNotFound(t *testing.T) {
	handler, cleanup, err := makeTestServer()
	require.Nil(t, err)
	defer cleanup()

	replRequest := []interface{}{"complete_rsync", "tmpfilename"}
	msg, err := json.Marshal(replRequest)
	require.Nil(t, err)
	rsp := test.MakeCaptureResponse()
	req, err := http.NewRequest("REPLICATE", "/device/1/ffffffffffffffffffffffffffffffff", bytes.NewBuffer(msg))
	require.Nil(t, err)
	handler.ServeHTTP(rsp, req)
	require.Equal(t, http.StatusNotFound, rsp.Status)
}

func TestServerReplicateSyncNotFound(t *testing.T) {
	handler, cleanup, err := makeTestServer()
	require.Nil(t, err)
	defer cleanup()

	replRequest := []interface{}{"sync", 1, "", "", "", "", "", ""}
	msg, err := json.Marshal(replRequest)
	require.Nil(t, err)
	rsp := test.MakeCaptureResponse()
	req, err := http.NewRequest("REPLICATE", "/device/1/ffffffffffffffffffffffffffffffff", bytes.NewBuffer(msg))
	require.Nil(t, err)
	handler.ServeHTTP(rsp, req)
	require.Equal(t, http.StatusNotFound, rsp.Status)
}

func TestServerReplicateMergeSyncs(t *testing.T) {
	handler, cleanup, err := makeTestServer()
	require.Nil(t, err)
	defer cleanup()

	// create a container
	rsp := test.MakeCaptureResponse()
	req, err := http.NewRequest("PUT", "/device/1/a", nil)
	require.Nil(t, err)
	req.Header.Set("X-Timestamp", common.CanonicalTimestamp(100))
	handler.ServeHTTP(rsp, req)
	require.Equal(t, 201, rsp.Status)

	h := md5.New()
	fmt.Fprintf(h, "%s/%s%s", "changeme", "a", "changeme")
	accountHash := fmt.Sprintf("%032x", h.Sum(nil))
	pretendLocalID := common.UUID()

	syncRequest := func(maxRow int64, hash, pretendLocalID, createdAt, putTimestamp, deleteTimestamp, metadata string) AccountInfo {
		replRequest := []interface{}{"sync", maxRow, hash, pretendLocalID, createdAt, putTimestamp, deleteTimestamp, metadata}
		msg, err := json.Marshal(replRequest)
		require.Nil(t, err)
		rsp = test.MakeCaptureResponse()
		req, err = http.NewRequest("REPLICATE", "/device/1/"+accountHash, bytes.NewBuffer(msg))
		require.Nil(t, err)
		handler.ServeHTTP(rsp, req)
		require.Equal(t, http.StatusOK, rsp.Status)
		var response AccountInfo
		require.Nil(t, json.Unmarshal(rsp.Body.Bytes(), &response))
		return response
	}

	// set our sync point via matching hashes
	info := syncRequest(10, "00000000000000000000000000000000", pretendLocalID, common.CanonicalTimestamp(100), common.CanonicalTimestamp(100), "", "{}")
	require.Equal(t, int64(10), info.Point)

	// send merge_syncs REPLICATE request with a new sync point for us
	replRequest := []interface{}{"merge_syncs", []SyncRecord{{SyncPoint: 15, RemoteID: pretendLocalID}}}
	msg, err := json.Marshal(replRequest)
	require.Nil(t, err)
	rsp = test.MakeCaptureResponse()
	req, err = http.NewRequest("REPLICATE", "/device/1/"+accountHash, bytes.NewBuffer(msg))
	require.Nil(t, err)
	handler.ServeHTTP(rsp, req)
	require.Equal(t, http.StatusAccepted, rsp.Status)

	// use a sync request to make sure our sync point was updated
	info = syncRequest(0, "f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0", pretendLocalID, common.CanonicalTimestamp(100), common.CanonicalTimestamp(100), "", "{}")
	require.Equal(t, int64(15), info.Point)
}

func TestServerReplicateCompleteRsync(t *testing.T) {
	handler, cleanup, err := makeTestServer()
	require.Nil(t, err)
	defer cleanup()

	h := md5.New()
	fmt.Fprintf(h, "%s/%s%s", "changeme", "a", "changeme")
	accountHash := fmt.Sprintf("%032x", h.Sum(nil))

	// create a local database
	db, _, cleanup, err := createTestDatabase(common.GetTimestamp())
	require.Nil(t, err)
	defer cleanup()
	require.Nil(t, mergeItemsByName(db, []string{"a", "b", "c"}))
	require.Nil(t, err)

	tmpFilename := common.UUID()

	// upload the local database to the server
	fp, cleanup, err := db.OpenDatabaseFile()
	require.Nil(t, err)
	defer cleanup()
	rsp := test.MakeCaptureResponse()
	req, err := http.NewRequest("PUT", "/device/tmp/"+tmpFilename, fp)
	require.Nil(t, err)
	handler.ServeHTTP(rsp, req)
	require.Equal(t, 201, rsp.Status)

	// send complete_rsync replicate request
	replRequest := []interface{}{"complete_rsync", tmpFilename}
	msg, err := json.Marshal(replRequest)
	require.Nil(t, err)
	rsp = test.MakeCaptureResponse()
	req, err = http.NewRequest("REPLICATE", "/device/1/"+accountHash, bytes.NewBuffer(msg))
	require.Nil(t, err)
	handler.ServeHTTP(rsp, req)
	require.Equal(t, http.StatusNoContent, rsp.Status)

	// HEAD the container and make sure it has 3 shiny new objects
	rsp = test.MakeCaptureResponse()
	req, err = http.NewRequest("HEAD", "/device/1/a", nil)
	require.Nil(t, err)
	handler.ServeHTTP(rsp, req)
	require.Equal(t, "3", rsp.Header().Get("X-Account-Container-Count"))
}

func TestServerReplicateBadOp(t *testing.T) {
	handler, cleanup, err := makeTestServer()
	require.Nil(t, err)
	defer cleanup()

	h := md5.New()
	fmt.Fprintf(h, "%s/%s%s", "changeme", "a", "changeme")
	accountHash := fmt.Sprintf("%032x", h.Sum(nil))

	replRequest := []interface{}{"made_up_op", 1, 2, 3}
	msg, err := json.Marshal(replRequest)
	require.Nil(t, err)
	rsp := test.MakeCaptureResponse()
	req, err := http.NewRequest("REPLICATE", "/device/1/"+accountHash, bytes.NewBuffer(msg))
	require.Nil(t, err)
	handler.ServeHTTP(rsp, req)
	require.Equal(t, http.StatusBadRequest, rsp.Status)
}

func TestIsOkayFilename(t *testing.T) {
	require.True(t, isOkayFilename("d850f04cdb48312a9be171e214c0b4ee"))
	require.True(t, isOkayFilename("2308f201-9642-48ad-bb61-cc89cc84f258"))
	require.False(t, isOkayFilename(""))
	require.False(t, isOkayFilename("abcdefg!"))
	require.False(t, isOkayFilename("../../../somefile"))
	require.False(t, isOkayFilename(strings.Repeat("F", 101)))
}

func TestServerReplicateBadFilename(t *testing.T) {
	handler, cleanup, err := makeTestServer()
	require.Nil(t, err)
	defer cleanup()
	tmpFilename := "one!bad!filename"
	rsp := test.MakeCaptureResponse()
	req, err := http.NewRequest("PUT", "/device/tmp/"+tmpFilename, nil)
	require.Nil(t, err)
	handler.ServeHTTP(rsp, req)
	require.Equal(t, 400, rsp.Status)
}

func TestServerReplicateBadPayload(t *testing.T) {
	handler, cleanup, err := makeTestServer()
	require.Nil(t, err)
	defer cleanup()
	rsp := test.MakeCaptureResponse()
	req, err := http.NewRequest("REPLICATE", "/device/1/d850f04cdb48312a9be171e214c0b4ee", bytes.NewBuffer([]byte("I AM A BAD JSON")))
	require.Nil(t, err)
	handler.ServeHTTP(rsp, req)
	require.Equal(t, 400, rsp.Status)
}

func TestServerReplicateBadJsonOp(t *testing.T) {
	handler, cleanup, err := makeTestServer()
	require.Nil(t, err)
	defer cleanup()
	for _, op := range []string{"complete_rsync", "merge_items", "merge_syncs", "sync"} {
		msg, err := json.Marshal([]string{op})
		require.Nil(t, err)
		rsp := test.MakeCaptureResponse()
		req, err := http.NewRequest("REPLICATE", "/device/1/d850f04cdb48312a9be171e214c0b4ee", bytes.NewBuffer(msg))
		require.Nil(t, err)
		handler.ServeHTTP(rsp, req)
		require.Equal(t, 400, rsp.Status)
	}
}
