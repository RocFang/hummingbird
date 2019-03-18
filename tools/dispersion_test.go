package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/RocFang/hummingbird/accountserver"
	"github.com/RocFang/hummingbird/client"
	"github.com/RocFang/hummingbird/common/ring"
	"github.com/RocFang/hummingbird/containerserver"
	"github.com/troubling/nectar/nectarutil"
)

type FakeRing struct {
	Devs      []*ring.Device
	Ip        string
	Port      int
	nodeCalls int64
	replicas  uint64
}

func (r *FakeRing) GetNodes(partition uint64) (response []*ring.Device) {
	if atomic.LoadInt64(&r.nodeCalls) <= 0 {
		return nil
	}
	atomic.AddInt64(&r.nodeCalls, -1)
	for i := range r.Devs {
		response = append(response, r.Devs[i])
	}
	return response
}

func (r *FakeRing) GetNodesInOrder(partition uint64) (response []*ring.Device) {
	if partition > 3 {
		return nil
	}
	for i := range r.Devs {
		response = append(response, r.Devs[i])
	}
	return response
}

func (r *FakeRing) GetJobNodes(partition uint64, localDevice int) (response []*ring.Device, handoff bool) {
	return nil, false
}

func (r *FakeRing) GetPartition(account string, container string, object string) uint64 {
	objParts := strings.Split(object, "-")
	if p, err := strconv.ParseUint(objParts[0], 10, 64); err == nil {
		return p
	}
	return 0
}

func (r *FakeRing) PartitionForHash(hsh string) (uint64, error) {
	return 0, nil
}

func (r *FakeRing) LocalDevices(localPort int) (devs []*ring.Device, err error) {
	return nil, nil
}

func (r *FakeRing) AllDevices() (devs []*ring.Device) {
	return r.Devs
}

func (r *FakeRing) GetMoreNodes(partition uint64) ring.MoreNodes {
	return nil
}

func (r *FakeRing) PartitionCount() uint64 {
	return 4
}

func (r *FakeRing) ReplicaCount() uint64 {
	if r.replicas > 0 {
		return r.replicas
	}
	return 2
}

type testDispersionClient struct {
	objRing   ring.Ring
	contRing  ring.Ring
	acctRing  ring.Ring
	objPuts   int
	contCalls int
	objCalls  int
}

func (c *testDispersionClient) SetUserAgent(v string) {
}

func (c *testDispersionClient) PutAccount(ctx context.Context, account string, headers http.Header) *http.Response {
	return nectarutil.ResponseStub(200, "")
}

func (c *testDispersionClient) PostAccount(ctx context.Context, account string, headers http.Header) *http.Response {
	return nectarutil.ResponseStub(200, "")
}

func (c *testDispersionClient) GetAccountRaw(ctx context.Context, account string, options map[string]string, headers http.Header) *http.Response {
	if c.contCalls > 0 {
		c.contCalls--
		olrs := []accountserver.ContainerListingRecord{{Name: "disp-objs-0"}}
		out, _ := json.Marshal(olrs)
		return nectarutil.ResponseStub(200, string(out))
	}
	olrs := []accountserver.ContainerListingRecord{}
	out, _ := json.Marshal(olrs)
	return nectarutil.ResponseStub(200, string(out))
}

func (c *testDispersionClient) HeadAccount(ctx context.Context, account string, headers http.Header) *http.Response {
	return nectarutil.ResponseStub(200, "")
}

func (c *testDispersionClient) DeleteAccount(ctx context.Context, account string, headers http.Header) *http.Response {
	return nectarutil.ResponseStub(200, "")
}

func (c *testDispersionClient) PutContainer(ctx context.Context, account string, container string, headers http.Header) *http.Response {
	return nectarutil.ResponseStub(200, "")
}

func (c *testDispersionClient) PostContainer(ctx context.Context, account string, container string, headers http.Header) *http.Response {
	return nectarutil.ResponseStub(200, "")
}

func (c *testDispersionClient) GetContainerRaw(ctx context.Context, account string, container string, options map[string]string, headers http.Header) *http.Response {
	if c.objCalls > 0 {
		c.objCalls--
		olrs := []containerserver.ObjectListingRecord{{Name: "0-12345"}}
		out, _ := json.Marshal(olrs)
		return nectarutil.ResponseStub(200, string(out))
	}
	olrs := []containerserver.ObjectListingRecord{}
	out, _ := json.Marshal(olrs)
	return nectarutil.ResponseStub(200, string(out))
}

func (c *testDispersionClient) GetContainerInfo(ctx context.Context, account string, container string) (*client.ContainerInfo, error) {
	return nil, nil
}

func (c *testDispersionClient) SetContainerInfo(ctx context.Context, account string, container string, resp *http.Response) (*client.ContainerInfo, error) {
	return nil, nil
}

func (c *testDispersionClient) HeadContainer(ctx context.Context, account string, container string, headers http.Header) *http.Response {
	return nectarutil.ResponseStub(200, "")
}

func (c *testDispersionClient) DeleteContainer(ctx context.Context, account string, container string, headers http.Header) *http.Response {
	return nectarutil.ResponseStub(200, "")
}

func (c *testDispersionClient) PutObject(ctx context.Context, account string, container string, obj string, headers http.Header, src io.Reader) *http.Response {
	fmt.Println("PutObject", account, container, obj)
	c.objPuts++
	return nectarutil.ResponseStub(200, "")
}

func (c *testDispersionClient) PostObject(ctx context.Context, account string, container string, obj string, headers http.Header) *http.Response {
	return nectarutil.ResponseStub(200, "")
}

func (c *testDispersionClient) GetObject(ctx context.Context, account string, container string, obj string, headers http.Header) *http.Response {
	return nectarutil.ResponseStub(200, "")
}

func (c *testDispersionClient) HeadObject(ctx context.Context, account string, container string, obj string, headers http.Header) *http.Response {
	if obj == "object-init" {
		return nectarutil.ResponseStub(404, "")
	}
	return nectarutil.ResponseStub(200, "")
}

func (c *testDispersionClient) DeleteObject(ctx context.Context, account string, container string, obj string, headers http.Header) *http.Response {
	return nectarutil.ResponseStub(200, "")
}

func (c *testDispersionClient) ObjectRingFor(ctx context.Context, account string, container string) (ring.Ring, *http.Response) {
	return c.objRing, nil //nectarutil.ResponseStub(200, "")
}

func (c *testDispersionClient) ContainerRing() ring.Ring {
	return c.contRing
}

func (c *testDispersionClient) AccountRing() ring.Ring {
	return c.acctRing
}

func TestGenerateDispersionNames(t *testing.T) {
	fakeDevs := []*ring.Device{
		{Ip: "127.0.0.1", Port: 80, Device: "sda", Scheme: "http"},
		{Ip: "127.0.0.1", Port: 80, Device: "sdb", Scheme: "http"}}

	oring := &FakeRing{Devs: fakeDevs, nodeCalls: 3}
	dObjs := make(chan string)
	container := "objs"
	cancel := make(chan struct{})
	go generateDispersionNames(container, "", oring, dObjs, cancel, 0)
	for val := range dObjs {
		part := oring.GetPartition(AdminAccount, container, val)
		require.Equal(t, strings.Index(val, fmt.Sprintf("%d-", part)), 0)
	}
	close(cancel)
}
