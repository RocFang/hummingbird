package accountserver

import (
	"github.com/RocFang/hummingbird/common"
	"github.com/RocFang/hummingbird/common/ring"
)

type PriorityRepJob struct {
	Partition  uint64       `json:"partition"`
	FromDevice *ring.Device `json:"from_device"`
	ToDevice   *ring.Device `json:"to_device"`
}

// TODO
func SendPriRepJob(job *PriorityRepJob, client common.HTTPClient, userAgent string) (string, bool) {
	return "pretending to do priority replication; normal replication should be fast enough for now", true
}
