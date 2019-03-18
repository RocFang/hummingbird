package objectserver

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"math/bits"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http2"

	"go.uber.org/zap"

	"github.com/RocFang/hummingbird/common"
	"github.com/RocFang/hummingbird/common/conf"
	"github.com/RocFang/hummingbird/common/ring"
	"github.com/RocFang/hummingbird/common/srv"
	"github.com/uber-go/tally"
)

const (
	roShard = 0
)

func init() {
	RegisterObjectEngine("repng", repEngineConstructor)
}

var _ ObjectEngineConstructor = repEngineConstructor

func repEngineConstructor(config conf.Config, policy *conf.Policy, flags *flag.FlagSet) (ObjectEngine, error) {
	hashPathPrefix, hashPathSuffix, err := conf.GetHashPrefixAndSuffix()
	if err != nil {
		return nil, err
	}
	driveRoot := config.GetDefault("app:object-server", "devices", "/srv/node")
	rng, err := ring.GetRing("object", hashPathPrefix, hashPathSuffix, policy.Index)
	if err != nil {
		return nil, err
	}
	dbPartPower, err := policy.GetDbPartPower()
	if err != nil {
		return nil, err
	}
	subdirs, err := policy.GetDbSubDirs()
	if err != nil {
		return nil, err
	}
	logLevelString := config.GetDefault("app:object-server", "log_level", "INFO")
	logLevel := zap.NewAtomicLevel()
	logLevel.UnmarshalText([]byte(strings.ToLower(logLevelString)))
	certFile := config.GetDefault("app:object-server", "cert_file", "")
	keyFile := config.GetDefault("app:object-server", "key_file", "")
	transport := &http.Transport{
		MaxIdleConnsPerHost: 256,
		MaxIdleConns:        0,
		IdleConnTimeout:     5 * time.Second,
		DisableCompression:  true,
		Dial: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 5 * time.Second,
		}).Dial,
		ExpectContinueTimeout: 10 * time.Minute,
	}
	if certFile != "" && keyFile != "" {
		tlsConf, err := common.NewClientTLSConfig(certFile, keyFile)
		if err != nil {
			return nil, err
		}
		transport.TLSClientConfig = tlsConf
		if err = http2.ConfigureTransport(transport); err != nil {
			return nil, err
		}
	}
	re := &repEngine{
		driveRoot:      driveRoot,
		hashPathPrefix: hashPathPrefix,
		hashPathSuffix: hashPathSuffix,
		reserve:        config.GetInt("app:object-server", "fallocate_reserve", 0),
		policy:         policy.Index,
		ring:           rng,
		idbs:           map[string]*IndexDB{},
		dbPartPower:    int(dbPartPower),
		numSubDirs:     subdirs,
		client: &http.Client{
			Timeout:   120 * time.Minute,
			Transport: transport,
		},
	}
	if re.logger, err = srv.SetupLogger("repobjengine", &logLevel, flags); err != nil {
		return nil, fmt.Errorf("Error setting up logger: %v", err)
	}
	return re, nil
}

var _ ObjectEngine = &repEngine{}

type repEngine struct {
	driveRoot      string
	hashPathPrefix string
	hashPathSuffix string
	reserve        int64
	policy         int
	ring           ring.Ring
	logger         srv.LowLevelLogger
	idbs           map[string]*IndexDB
	dblock         sync.Mutex
	dbPartPower    int
	numSubDirs     int
	client         *http.Client
}

func (re *repEngine) getDB(device string) (*IndexDB, error) {
	re.dblock.Lock()
	defer re.dblock.Unlock()
	if idb, ok := re.idbs[device]; ok && idb != nil {
		return idb, nil
	}
	var err error
	dbpath := filepath.Join(re.driveRoot, device, PolicyDir(re.policy), "repng.db")
	path := filepath.Join(re.driveRoot, device, PolicyDir(re.policy), "repng")
	temppath := filepath.Join(re.driveRoot, device, "tmp")
	ringPartPower := bits.Len64(re.ring.PartitionCount() - 1)
	re.idbs[device], err = NewIndexDB(dbpath, path, temppath, ringPartPower, re.dbPartPower, re.numSubDirs, re.reserve, re.logger, repAuditor{})
	if err != nil {
		return nil, err
	}
	return re.idbs[device], nil
}

func (re *repEngine) New(vars map[string]string, needData bool, asyncWG *sync.WaitGroup) (Object, error) {
	//TODO: not sure if here- but need to show x-backend timestamp on deleted objects
	hash := ObjHash(vars, re.hashPathPrefix, re.hashPathSuffix)
	obj := &repObject{
		IndexDBItem: IndexDBItem{
			Hash: hash,
		},
		ring:     re.ring,
		policy:   re.policy,
		reserve:  re.reserve,
		metadata: map[string]string{},
		asyncWG:  asyncWG,
		client:   re.client,
		txnId:    vars["txnId"],
	}
	if idb, err := re.getDB(vars["device"]); err == nil {
		obj.idb = idb
		if item, err := idb.Lookup(hash, roShard, false); err == nil && item != nil {
			obj.IndexDBItem = *item
			if err = json.Unmarshal(item.Metabytes, &obj.metadata); err != nil {
				return nil, fmt.Errorf("Error parsing metadata: %v", err)
			}
			if !item.Deletion {
				if fi, err := os.Stat(item.Path); err != nil {
					obj.Quarantine()
					return nil, err
				} else if contentLength, err := strconv.ParseInt(obj.metadata["Content-Length"], 10, 64); err != nil {
					obj.Quarantine()
					return nil, fmt.Errorf("Unable to parse content-length: %s %s", obj.metadata["Content-Length"], err)
				} else if fi.Size() != contentLength {
					obj.Quarantine()
					return nil, fmt.Errorf("File size doesn't match content-length: %d vs %d", fi.Size(), contentLength)
				}
			}
		} else if err != nil {
			return nil, err
		}
		return obj, nil
	} else {
		return nil, err
	}
}

func (re *repEngine) GetReplicationDevice(oring ring.Ring, dev *ring.Device, r *Replicator) (ReplicationDevice, error) {
	return GetNurseryDevice(oring, dev, re.policy, r, re)
}

func (re *repEngine) GetObjectsToReplicate(prirep PriorityRepJob, c chan ObjectStabilizer, cancel chan struct{}) {
	defer close(c)
	idb, err := re.getDB(prirep.FromDevice.Device)
	if err != nil {
		re.logger.Error("error getting local db", zap.Error(err))
		return
	}
	startHash, stopHash := idb.RingPartRange(int(prirep.Partition))
	items, err := idb.List(startHash, stopHash, "", 0)
	if len(items) == 0 {
		return
	}
	url := fmt.Sprintf("%s://%s:%d/rep-partition/%s/%d", prirep.ToDevice.Scheme, prirep.ToDevice.Ip, prirep.ToDevice.Port, prirep.ToDevice.Device, prirep.Partition)
	req, err := http.NewRequest("GET", url, nil)
	req.Header.Set("X-Backend-Storage-Policy-Index", strconv.Itoa(prirep.Policy))
	req.Header.Set("User-Agent", "nursery-stabilizer")
	resp, err := re.client.Do(req)

	var remoteItems []*IndexDBItem
	if err == nil && (resp.StatusCode/100 == 2 || resp.StatusCode == 404) {
		if data, err := ioutil.ReadAll(resp.Body); err == nil {
			if err = json.Unmarshal(data, &remoteItems); err != nil {
				re.logger.Error("error unmarshaling partition list", zap.Error(err))
			}
		} else {
			re.logger.Error("error reading partition list", zap.Error(err))
		}
	}
	if err != nil {
		re.logger.Error("error getting local partition list", zap.Error(err))
		return
	}
	rii := 0
	for _, item := range items {
		if item.Nursery {
			continue
		}
		sendItem := true
		for rii < len(remoteItems) {
			if remoteItems[rii].Hash > item.Hash {
				break
			}
			if remoteItems[rii].Hash < item.Hash {
				rii++
				continue
			}
			if remoteItems[rii].Hash == item.Hash &&
				remoteItems[rii].Timestamp == item.Timestamp &&
				remoteItems[rii].Nursery == item.Nursery &&
				remoteItems[rii].Deletion == item.Deletion {
				sendItem = false
			}
			rii++
			break
		}
		obj := &repObject{
			IndexDBItem: *item,
			reserve:     re.reserve,
			ring:        re.ring,
			policy:      re.policy,
			idb:         idb,
			metadata:    map[string]string{},
			client:      re.client,
			txnId:       fmt.Sprintf("%s-%s", common.UUID(), prirep.FromDevice.Device),
		}
		if err = json.Unmarshal(item.Metabytes, &obj.metadata); err != nil {
			//TODO: this should prob quarantine- also in ec thing that does this too
			continue
		}
		if obj.Path, err = idb.WholeObjectPath(item.Hash, item.Shard, item.Timestamp, item.Nursery); err != nil {
			continue // TODO: quarantine here too
		}
		if sendItem {
			select {
			case c <- obj:
			case <-cancel:
				return
			}
		}
	}
}

func (re *repEngine) GetObjectsToStabilize(device *ring.Device) (c chan ObjectStabilizer, cancel chan struct{}) {
	c = make(chan ObjectStabilizer, numStabilizeObjects)
	cancel = make(chan struct{})
	go re.getObjectsToStabilize(device, c, cancel)
	return c, cancel
}

func (re *repEngine) getObjectsToStabilize(device *ring.Device, c chan ObjectStabilizer, cancel chan struct{}) {
	defer close(c)
	idb, err := re.getDB(device.Device)
	if err != nil {
		return
	}
	idb.ExpireObjects()

	idbItems, err := idb.ListObjectsToStabilize()
	if err != nil {
		re.logger.Error("ListObjectsToStabilize error", zap.Error(err))
		return
	}

	//TODO: do we add the skip stuff here? stabilize is a lot easier here
	for _, item := range idbItems {
		obj := &repObject{
			IndexDBItem: *item,
			reserve:     re.reserve,
			ring:        re.ring,
			policy:      re.policy,
			idb:         idb,
			metadata:    map[string]string{},
			client:      re.client,
			txnId:       fmt.Sprintf("%s-%s", common.UUID(), device.Device),
		}
		if err = json.Unmarshal(item.Metabytes, &obj.metadata); err != nil {
			continue
		}
		select {
		case c <- obj:
		case <-cancel:
			return
		}
	}
}

func (re *repEngine) UpdateItemStabilized(device, hash, ts string, stabilized bool) bool {
	//TODO: this for some stabilization optimization later
	return false
}

func (re *repEngine) listPartitionHandler(writer http.ResponseWriter, request *http.Request) {
	vars := srv.GetVars(request)
	idb, err := re.getDB(vars["device"])
	if err != nil {
		srv.StandardResponse(writer, http.StatusBadRequest)
		return
	}
	part, err := strconv.Atoi(vars["partition"])
	if err != nil {
		srv.StandardResponse(writer, http.StatusBadRequest)
		return
	}
	startHash, stopHash := idb.RingPartRange(part)
	items, err := idb.List(startHash, stopHash, "", 0)
	if err != nil {
		re.logger.Error("error listing idb", zap.Error(err))
		srv.StandardResponse(writer, http.StatusInternalServerError)
		return
	}
	if data, err := json.Marshal(items); err == nil {
		writer.WriteHeader(http.StatusOK)
		writer.Write(data)
		return
	} else {
		re.logger.Error("error marshaling listing idb", zap.Error(err))
	}
	srv.StandardResponse(writer, http.StatusInternalServerError)
	return
}

func (re *repEngine) putStableObject(writer http.ResponseWriter, request *http.Request) {
	vars := srv.GetVars(request)
	idb, err := re.getDB(vars["device"])
	if err != nil {
		srv.StandardResponse(writer, http.StatusBadRequest)
		return
	}
	if err := idb.StablePut(vars["hash"], roShard, request); err != nil {
		srv.ErrorResponse(writer, err)
		return
	}
	srv.StandardResponse(writer, http.StatusCreated)
	return
}

func (re *repEngine) postStableObject(writer http.ResponseWriter, request *http.Request) {
	vars := srv.GetVars(request)
	idb, err := re.getDB(vars["device"])
	if err != nil {
		srv.StandardResponse(writer, http.StatusBadRequest)
		return
	}
	if err := idb.StablePost(vars["hash"], roShard, request); err != nil {
		srv.ErrorResponse(writer, err)
		return
	}
	srv.StandardResponse(writer, http.StatusAccepted)
	return
}

func (re *repEngine) deleteStableObject(writer http.ResponseWriter, request *http.Request) {
	vars := srv.GetVars(request)
	idb, err := re.getDB(vars["device"])
	if err != nil {
		srv.StandardResponse(writer, http.StatusBadRequest)
		return
	}
	reqTimeStamp, err := common.ParseDate(request.Header.Get("X-Timestamp"))
	if err != nil {
		srv.StandardResponse(writer, http.StatusBadRequest)
	}
	item, err := idb.Lookup(vars["hash"], roShard, true)
	if err != nil || item == nil {
		srv.StandardResponse(writer, http.StatusNotFound)
		return
	}
	if reqTimeStamp.UnixNano() < item.Timestamp {
		srv.StandardResponse(writer, http.StatusConflict)
		return
	}

	if _, err := idb.Remove(item.Hash, item.Shard, item.Timestamp, item.Nursery, item.Metahash); err != nil {
		srv.StandardResponse(writer, http.StatusInternalServerError)
	} else {
		srv.StandardResponse(writer, http.StatusNoContent)
	}
}

func (re *repEngine) RegisterHandlers(addRoute func(method, path string, handler http.HandlerFunc), metScope tally.Scope) {
	addRoute("GET", "/rep-partition/:device/:partition", re.listPartitionHandler)
	addRoute("PUT", "/rep-obj/:device/:hash", re.putStableObject)
	addRoute("POST", "/rep-obj/:device/:hash", re.postStableObject)
	addRoute("DELETE", "/rep-obj/:device/:hash", re.deleteStableObject)
}
