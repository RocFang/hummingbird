//  Copyright (c) 2015 Rackspace
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

// In /etc/hummingbird/andrewd-server.conf:
// [andrewd]
// sql_dir = /var/local/hummingbird # path to directory for andrewd data files
// bind_ip = 0.0.0.0                # ip to listen on for http requests
// bind_port = 6003                 # port to listen on for http requests
// cert_file =                      # path to tls certificate, if tls is desired
// key_file =                       # path to tls key, if tls is desired
// service_error_expiration = 3600  # seconds of no errors before error count is cleared
// device_error_expiration = 3600   # seconds of no errors before error count is cleared

package tools

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"math/bits"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/http2"

	"github.com/justinas/alice"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/RocFang/hummingbird/client"
	"github.com/RocFang/hummingbird/common"
	"github.com/RocFang/hummingbird/common/conf"
	"github.com/RocFang/hummingbird/common/ring"
	"github.com/RocFang/hummingbird/common/srv"
	"github.com/RocFang/hummingbird/common/tracing"
	"github.com/RocFang/hummingbird/middleware"
	"github.com/RocFang/hummingbird/objectserver"
	"github.com/uber-go/tally"
	promreporter "github.com/uber-go/tally/prometheus"
	"go.uber.org/zap"
)

const AdminAccount = ".admin"

func getAffixes() (string, string) {
	hashPathPrefix, hashPathSuffix, err := conf.GetHashPrefixAndSuffix()
	if err != nil {
		fmt.Println("Unable to get hash prefix and suffix")
		os.Exit(1)
	}
	return hashPathPrefix, hashPathSuffix
}

func inferRingType(account, container, object string) string {
	if object != "" {
		return "object"
	} else if container != "" {
		return "container"
	} else if account != "" {
		return "account"
	}
	return ""
}

func getRing(ringPath, ringType string, policyNum int) (ring.Ring, string) {
	// if you have a direct path to a ring send it as ringPath. otherwise send
	// a ringType ("" defaults to 'object') and optional policy and this'll
	// try to find it in usual spots
	prefix, suffix := getAffixes()
	if ringPath != "" {
		r, err := ring.LoadRing(ringPath, prefix, suffix)
		if err != nil {
			fmt.Println("Unable to load ring ", ringPath)
			os.Exit(1)
		}
		if strings.Contains(ringPath, "account") {
			return r, "account"
		} else if strings.Contains(ringPath, "container") {
			return r, "container"
		} else if strings.Contains(ringPath, "object") {
			return r, "object"
		} else {
			fmt.Println("Unknown ring type", ringPath)
			os.Exit(1)
		}
	}

	if ringType == "" {
		ringType = "object"
	}

	r, err := ring.GetRing(ringType, prefix, suffix, policyNum)
	if err != nil {
		if policyNum > 0 {
			fmt.Printf("Unable to load %v-%v ring\n", ringType, policyNum)
		} else {
			fmt.Printf("Unable to load %v ring\n", ringType)
		}
		os.Exit(1)
	}
	return r, ringType
}

func curlHeadCommand(scheme string, ipStr string, port int, device string, partNum uint64, target string, policy int) string {
	formatted_ip := ipStr
	ip := net.ParseIP(ipStr)
	if ip != nil && strings.Contains(ipStr, ":") {
		formatted_ip = fmt.Sprintf("[%v]", ipStr)
	}
	policyStr := ""
	if policy > 0 {
		policyStr = fmt.Sprintf(" -H \"X-Backend-Storage-Policy-Index: %v\"", policy)
	}
	return fmt.Sprintf("curl -g -I -XHEAD \"%v://%v:%v/%v/%v/%v\"%v", scheme, formatted_ip, port, device, partNum, common.Urlencode(target), policyStr)
}

func getPathHash(account, container, object string) string {
	prefix, suffix := getAffixes()
	if object != "" && container == "" {
		fmt.Println("container is required if object is provided")
		os.Exit(1)
	}
	paths := prefix + "/" + account
	if container != "" {
		paths = paths + "/" + container
	}
	if object != "" {
		paths = paths + "/" + object
	}
	paths = paths + suffix
	h := md5.New()
	fmt.Fprintf(h, "%v", paths)
	return fmt.Sprintf("%032x", h.Sum(nil))
}

func printSshCommands(r ring.Ring, pathHash string, allHandoffs bool, policy *conf.Policy) error {
	fmt.Printf("\n\nUse your own device location of servers:\n")
	fmt.Printf("such as \"export DEVICE=/srv/node\"\n")

	if pathHash == "" {
		return fmt.Errorf("not implemented: please supply object path")
	}
	partition, err := r.PartitionForHash(pathHash)
	if err != nil {
		return err
	}
	primaries := r.GetNodes(partition)
	handoffLimit := len(primaries)
	if allHandoffs {
		handoffLimit = -1
	}
	ringPartPower := bits.Len64(r.PartitionCount() - 1)
	dbPartPower, err := policy.GetDbPartPower()
	if err != nil {
		return fmt.Errorf("Error getting dbPartPower: %v", err)
	}
	subdirs, err := policy.GetDbSubDirs()
	if err != nil {
		return fmt.Errorf("Error getting subdirs: %v", err)
	}
	_, _, dbPart, dirNum, err := objectserver.ValidateHash(pathHash, uint(ringPartPower), dbPartPower, subdirs)
	if err != nil {
		return fmt.Errorf("Error in ValidateHash: %v", err)
	}
	dbFileName := fmt.Sprintf("index.db.%02x", dbPart)
	odir := fmt.Sprintf("index.db.dir.%02x", dirNum)
	for _, v := range primaries {
		fmt.Printf("ssh %s \"sqlite3 ${DEVICE:-/srv/node*}/%v/%v/hec.db/%v \\\"SELECT * FROM objects WHERE hash = '%v'\\\"\"\n", v.Ip, v.Device, objectserver.PolicyDir(policy.Index), dbFileName, pathHash)
		fmt.Printf("ssh %s \"ls -lah ${DEVICE:-/srv/node*}/%v/%v/hec/%v/%v*\"\n\n", v.Ip, v.Device, objectserver.PolicyDir(policy.Index), odir, pathHash)
	}
	handoffs := r.GetMoreNodes(partition)
	for i, v := 0, handoffs.Next(); v != nil; i, v = i+1, handoffs.Next() {
		if handoffLimit != -1 && i == handoffLimit {
			break
		}
		fmt.Printf("ssh %s \"sqlite3 ${DEVICE:-/srv/node*}/%v/%v/hec.db/%v \\\"SELECT * FROM objects WHERE hash = '%v'\\\"\" #[HANDOFF]\n", v.Ip, v.Device, objectserver.PolicyDir(policy.Index), dbFileName, pathHash)
		fmt.Printf("ssh %s \"ls -lah ${DEVICE:-/srv/node*}/%v/%v/hec/%v/%v*\" #[HANDOFF]\n\n", v.Ip, v.Device, objectserver.PolicyDir(policy.Index), odir, pathHash)
	}
	return nil
}

func printRingLocations(r ring.Ring, ringType, datadir, account, container, object, partition string, allHandoffs bool, policy *conf.Policy) {
	if r == nil {
		fmt.Println("No ring specified")
		os.Exit(1)
	}
	if datadir == "" {
		fmt.Println("No datadir specified")
		os.Exit(1)
	}
	var target string
	if object != "" {
		target = fmt.Sprintf("%v/%v/%v", account, container, object)
	} else if container != "" {
		target = fmt.Sprintf("%v/%v", account, container)
	} else {
		target = fmt.Sprintf("%v", account)
	}
	var partNum uint64
	if partition != "" {
		var err error
		partNum, err = strconv.ParseUint(partition, 10, 64)
		if err != nil {
			fmt.Println("Invalid partition")
			os.Exit(1)
		}
	} else {
		partNum = r.GetPartition(account, container, object)
	}
	primaries := r.GetNodes(partNum)
	handoffLimit := len(primaries)
	if allHandoffs {
		handoffLimit = -1
	}

	pathHash := ""
	if account != "" && partition == "" {
		pathHash = getPathHash(account, container, object)
	}
	fmt.Printf("Partition\t%v\n", partNum)
	fmt.Printf("Hash     \t%v\n\n", pathHash)

	for _, v := range primaries {
		fmt.Printf("Server:Port Device\t%v:%v %v\n", v.Ip, v.Port, v.Device)
	}
	handoffs := r.GetMoreNodes(partNum)
	for i, v := 0, handoffs.Next(); v != nil; i, v = i+1, handoffs.Next() {
		if handoffLimit != -1 && i == handoffLimit {
			break
		}
		fmt.Printf("Server:Port Device\t%v:%v %v\t [Handoff]\n", v.Ip, v.Port, v.Device)
	}
	fmt.Printf("\n\n")
	for _, v := range primaries {
		cmd := curlHeadCommand(v.Scheme, v.Ip, v.Port, v.Device, partNum, target, policy.Index)
		fmt.Println(cmd)
	}
	handoffs = r.GetMoreNodes(partNum)
	for i, v := 0, handoffs.Next(); v != nil; i, v = i+1, handoffs.Next() {
		if handoffLimit != -1 && i == handoffLimit {
			break
		}
		cmd := curlHeadCommand(v.Scheme, v.Ip, v.Port, v.Device, partNum, target, policy.Index)
		fmt.Printf("%v # [Handoff]\n", cmd)
	}

	if policy.Type == "replication" || object == "" {
		fmt.Printf("\n\nUse your own device location of servers:\n")
		fmt.Printf("such as \"export DEVICE=/srv/node\"\n")

		if pathHash != "" {
			stDir := filepath.Join(datadir, strconv.Itoa(int(partNum)), pathHash[len(pathHash)-3:], pathHash)
			for _, v := range primaries {
				fmt.Printf("ssh %s \"ls -lah ${DEVICE:-/srv/node*}/%v/%v\"\n", v.Ip, v.Device, stDir)
			}
			handoffs = r.GetMoreNodes(partNum)
			for i, v := 0, handoffs.Next(); v != nil; i, v = i+1, handoffs.Next() {
				if handoffLimit != -1 && i == handoffLimit {
					break
				}
				fmt.Printf("ssh %s \"ls -lah ${DEVICE:-/srv/node*}/%v/%v\" # [Handoff]\n", v.Ip, v.Device, stDir)
			}
		} else {
			for _, v := range primaries {
				fmt.Printf("ssh %s \"ls -lah ${DEVICE:-/srv/node*}/%v/%v/%v\"\n", v.Ip, v.Device, datadir, partNum)
			}
			handoffs = r.GetMoreNodes(partNum)
			for i, v := 0, handoffs.Next(); v != nil; i, v = i+1, handoffs.Next() {
				if handoffLimit != -1 && i == handoffLimit {
					break
				}
				fmt.Printf("ssh %s \"ls -lah ${DEVICE:-/srv/node*}/%v/%v/%v\" # [Handoff]\n", v.Ip, v.Device, datadir, partNum)
			}
		}
	} else {
		if err := printSshCommands(r, pathHash, allHandoffs, policy); err != nil {
			fmt.Println(err.Error())
			return
		}
	}

	fmt.Printf("\nnote: `/srv/node*` is used as default value of `devices`, the real value is set in the config file on each storage node.\n")
}

func printItemLocations(r ring.Ring, ringType, account, container, object, partition string, allHandoffs bool, policy *conf.Policy) {
	location := ""
	if policy.Index > 0 {
		location = fmt.Sprintf("%vs-%d", ringType, policy.Index)
	} else {
		location = fmt.Sprintf("%vs", ringType)
	}

	printRingLocations(r, ringType, location, account, container, object, partition, allHandoffs, policy)
}

func parseArg0(arg0 string) (string, string, string) {
	arg0 = strings.TrimPrefix(arg0, "/v1/")
	parts := strings.SplitN(arg0, "/", 3)
	if len(parts) == 1 {
		return parts[0], "", ""
	} else if len(parts) == 2 {
		return parts[0], parts[1], ""
	} else if len(parts) == 3 {
		return parts[0], parts[1], parts[2]
	}
	return arg0, "", ""
}

func Nodes(flags *flag.FlagSet, cnf srv.ConfigLoader) {
	var account, container, object string
	ohsh := flags.Lookup("objhash").Value.(flag.Getter).Get().(string)
	if flags.NArg() == 1 {
		account, container, object = parseArg0(flags.Arg(0))
	} else {
		if ohsh == "" {
			account = flags.Arg(0)
			container = flags.Arg(1)
			object = flags.Arg(2)
		}
	}
	partition := flags.Lookup("p").Value.(flag.Getter).Get().(string)
	policyName := flags.Lookup("P").Value.(flag.Getter).Get().(string)
	allHandoffs := flags.Lookup("a").Value.(flag.Getter).Get().(bool)

	policies, err := cnf.GetPolicies()
	if err != nil {
		fmt.Println("Unable to load policies:", err)
		os.Exit(1)
	}
	policy := policyByName(policyName, policies)

	var r ring.Ring
	var ringType string
	inferredType := inferRingType(account, container, object)
	if ohsh != "" {
		inferredType = "object"
	}
	ringPath := flags.Lookup("r").Value.(flag.Getter).Get().(string)
	if ringPath != "" {
		r, ringType = getRing(ringPath, "", policy.Index)
		if inferredType != "" && ringType != inferredType {
			fmt.Printf("Error %v specified but ring type: %v\n", inferredType, ringType)
			os.Exit(1)
		}
		if ringType == "object" && policy.Index == 0 {
			_, ringFileName := path.Split(ringPath)
			if strings.HasPrefix(ringFileName, "object") && strings.Contains(ringFileName, "-") {
				polSuff := strings.Split(ringFileName, "-")[1]
				if polN, err := strconv.ParseInt(polSuff[:(len(polSuff)-len(".ring.gz"))], 10, 64); err == nil {
					policy = policies[int(polN)]
				}
			}
		}
	} else {
		r, ringType = getRing("", inferredType, policy.Index)
	}

	if partition != "" || ohsh != "" {
		account = ""
		container = ""
		object = ""
	} else {
		if account == "" && (container != "" || object != "") {
			fmt.Println("No account specified")
			os.Exit(1)
		}
		if container == "" && object != "" {
			fmt.Println("No container specified")
			os.Exit(1)
		}
		if account == "" {
			fmt.Println("No target specified")
			os.Exit(1)
		}
	}

	if ohsh == "" {
		fmt.Printf("\nAccount  \t%v\n", account)
		fmt.Printf("Container\t%v\n", container)
		fmt.Printf("Object   \t%v\n", object)
		printItemLocations(r, ringType, account, container, object, partition, allHandoffs, policy)
	} else {
		if err := printSshCommands(r, ohsh, allHandoffs, policy); err != nil {
			fmt.Println(err.Error())
		}
	}
}

func getACO(path string) (account, container, object string) {
	stuff := strings.SplitN(path, "/", 4)
	if len(stuff) != 4 {
		fmt.Printf("Path is invalid for object %v\n", path)
		os.Exit(1)
	}
	return stuff[1], stuff[2], stuff[3]
}

func printObjMeta(metadata map[string]string) {
	userMetadata := make(map[string]string)
	sysMetadata := make(map[string]string)
	transientSysMetadata := make(map[string]string)
	otherMetadata := make(map[string]string)

	path := metadata["name"]
	delete(metadata, "name")
	if path != "" {
		account, container, object := getACO(path)
		objHash := getPathHash(account, container, object)
		fmt.Printf("Path: %s\n", path)
		fmt.Printf("  Account: %s\n", account)
		fmt.Printf("  Container: %s\n", container)
		fmt.Printf("  Object: %s\n", object)
		fmt.Printf("  Object hash: %s\n", objHash)
	} else {
		fmt.Printf("Path: Not found in metadata\n")
	}
	contentType := metadata["Content-Type"]
	delete(metadata, "Content-Type")
	if contentType != "" {
		fmt.Printf("Content-Type: %v\n", contentType)
	} else {
		fmt.Printf("Content-Type: Not found in metadata\n")
	}
	timestamp := metadata["X-Timestamp"]
	delete(metadata, "X-Timestamp")
	if timestamp != "" {
		t, timeErr := common.ParseDate(timestamp)
		if timeErr != nil {
			fmt.Printf("Timestamp error: %v\n", timeErr)
			os.Exit(1)
		}
		fmt.Printf("Timestamp: %s (%s)\n", t.Format(time.RFC3339), timestamp)
	} else {
		fmt.Printf("Timestamp: Not found in metadata\n")
	}

	for key, value := range metadata {
		if strings.HasPrefix(key, "X-Object-Meta-") {
			userMetadata[key] = value
		} else if strings.HasPrefix(key, "X-Object-SysMeta-") {
			sysMetadata[key] = value
		} else if strings.HasPrefix(key, "X-Object-Transient-Sysmeta-") {
			transientSysMetadata[key] = value
		} else {
			otherMetadata[key] = value
		}
	}
	printMetadata := func(title string, items map[string]string) {
		fmt.Printf("%s\n", title)
		if len(items) > 0 {
			for key, value := range items {
				fmt.Printf("  %s: %s\n", key, value)
			}
		} else {
			fmt.Printf("  No metadata found\n")
		}
	}

	printMetadata("System Metadata:", sysMetadata)
	printMetadata("Transient System Metadata:", transientSysMetadata)
	printMetadata("User Metadata:", userMetadata)
	printMetadata("Other Metadata:", otherMetadata)
}

func policyByName(name string, policies conf.PolicyList) *conf.Policy {
	if name == "" {
		return policies[0]
	}
	for _, v := range policies {
		if v.Name == name {
			return v
		}
	}
	fmt.Println("No policy named ", name)
	os.Exit(1)
	return nil
}

func ObjectInfo(flags *flag.FlagSet, cnf srv.ConfigLoader) {
	object := flags.Arg(0)
	noEtag := flags.Lookup("n").Value.(flag.Getter).Get().(bool)
	policyName := flags.Lookup("P").Value.(flag.Getter).Get().(string)

	policies, err := cnf.GetPolicies()
	if err != nil {
		fmt.Println("Unable to load policies:", err)
		os.Exit(1)
	}
	namedPolicy := policyByName(policyName, policies)

	stat, statErr := os.Stat(object)
	if statErr != nil {
		fmt.Printf("Error statting file: %v\n", statErr)
		os.Exit(1)
	}

	fullPath, pathErr := filepath.Abs(object)
	if pathErr != nil {
		fmt.Printf("Error getting abs path: %v\n", pathErr)
		os.Exit(1)
	}
	re := regexp.MustCompile(`objects-(\d*)`)
	match := re.FindStringSubmatch(fullPath)
	var policy *conf.Policy
	if match == nil {
		policy = policies[0]
	} else {
		policyIdx, convErr := strconv.Atoi(match[1])
		if convErr != nil {
			fmt.Printf("Invalid policy index: %v\n", match[1])
			os.Exit(1)
		}
		policy = policies[policyIdx]
	}
	if namedPolicy != nil && namedPolicy != policy {
		fmt.Printf("Warning: Ring does not match policy!\n")
		fmt.Printf("Double check your policy name!\n")
	}

	ring, _ := getRing("", "object", policy.Index)

	hashDir := filepath.Dir(fullPath)
	dataFile, metaFile := objectserver.ObjectFiles(hashDir)
	metadata, metaErr := objectserver.ObjectMetadata(dataFile, metaFile)
	if metaErr != nil {
		fmt.Printf("Error fetching metadata: %v\n", metaErr)
		os.Exit(1)
	}

	etag := metadata["ETag"]
	delete(metadata, "ETag")
	length := metadata["Content-Length"]
	delete(metadata, "Content-Length")
	path := metadata["name"]

	printObjMeta(metadata)

	if noEtag == false {
		fp, openErr := os.Open(fullPath)
		if openErr != nil {
			fmt.Printf("Error opening file (%v): %v\n", fullPath, openErr)
			os.Exit(1)
		}
		hasher := md5.New()
		if _, err := io.Copy(hasher, fp); err != nil {
			fmt.Printf("Error copying file: %v\n", err)
			os.Exit(1)
		}
		hash := hex.EncodeToString(hasher.Sum(nil))
		if etag != "" {
			if etag == hash {
				fmt.Printf("ETag: %v (valid)\n", etag)
			} else {
				fmt.Printf("ETag: %v doesn't match file hash of %v!\n", etag, hash)
			}
		} else {
			fmt.Printf("ETag: Not found in metadata\n")
		}
	} else {
		fmt.Printf("ETag: %v (not checked)\n", etag)
	}
	if length != "" {
		l, convErr := strconv.Atoi(length)
		if convErr != nil {
			fmt.Printf("Invalid length: %v\n", length)
			os.Exit(1)
		}
		if int64(l) == stat.Size() {
			fmt.Printf("Content-Length: %v (valid)\n", length)
		} else {
			fmt.Printf("Content-Length: %v doesn't match file length of %v\n", length, stat.Size())
		}
	} else {
		fmt.Printf("Content-Length: Not found in metadata\n")
	}

	account, container, object := getACO(path)
	printItemLocations(ring, "object", account, container, object, "", false, policy)
}

type AutoAdmin struct {
	serverconf        conf.Config
	logger            srv.LowLevelLogger
	logLevel          zap.AtomicLevel
	port              int
	bindIp            string
	client            common.HTTPClient
	hClient           client.RequestClient
	policies          conf.PolicyList
	metricsScope      tally.Scope
	metricsCloser     io.Closer
	pdcCloser         io.Closer
	clientTraceCloser io.Closer
	runningForever    bool
	db                *dbInstance
	fastRingScan      chan struct{}
}

func (server *AutoAdmin) Type() string {
	return "andrewd"
}

func (server *AutoAdmin) Background(flags *flag.FlagSet) chan struct{} {
	once := false
	if f := flags.Lookup("once"); f != nil {
		once = f.Value.(flag.Getter).Get() == true
	}
	if once {
		ch := make(chan struct{})
		go func() {
			defer close(ch)
			server.Run()
		}()
		return ch
	}
	go server.RunForever()
	return nil
}

func (server *AutoAdmin) GetHandler(config conf.Config, metricsPrefix string) http.Handler {
	var metricsScope tally.Scope
	metricsScope, server.metricsCloser = tally.NewRootScope(tally.ScopeOptions{
		Prefix:         metricsPrefix,
		Tags:           map[string]string{},
		CachedReporter: promreporter.NewReporter(promreporter.Options{}),
		Separator:      promreporter.DefaultSeparator,
	}, time.Second)
	commonHandlers := alice.New(
		middleware.NewDebugResponses(config.GetBool("debug", "debug_x_source_code", false)),
		server.LogRequest,
		middleware.RecoverHandler,
		middleware.ValidateRequest,
	)
	router := srv.NewRouter()
	router.Get("/metrics", prometheus.Handler())
	router.Get("/loglevel", server.logLevel)
	router.Put("/loglevel", server.logLevel)
	router.Get("/healthcheck", commonHandlers.ThenFunc(server.HealthcheckHandler))
	router.Get("/debug/pprof/:parm", http.DefaultServeMux)
	router.Post("/debug/pprof/:parm", http.DefaultServeMux)
	return alice.New(middleware.Metrics(metricsScope)).Then(router)
}

func (server *AutoAdmin) Finalize() {
	if server.metricsCloser != nil {
		server.metricsCloser.Close()
	}
	if server.clientTraceCloser != nil {
		server.clientTraceCloser.Close()
	}
	if server.pdcCloser != nil {
		server.pdcCloser.Close()
	}
}

func (server *AutoAdmin) HealthcheckHandler(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Length", "2")
	writer.WriteHeader(http.StatusOK)
	writer.Write([]byte("OK"))
}

func (server *AutoAdmin) LogRequest(next http.Handler) http.Handler {
	return srv.LogRequest(server.logger, next)
}

func (a *AutoAdmin) Run() {
	// TODO: Reimplement run once.
}

func (a *AutoAdmin) RunForever() {
	go newDispersionPopulateContainers(a).runForever()
	go newDispersionPopulateObjects(a).runForever()
	go newDispersionScanContainers(a).runForever()
	go newDispersionScanObjects(a).runForever()
	go newQuarantineHistory(a).runForever()
	go newQuarantineRepair(a).runForever()
	go newUnmountedMonitor(a).runForever()
	go newReplication(a).runForever()
	go newRingMonitor(a).runForever()
	go newRingScan(a).runForever()
}

func NewAdmin(serverconf conf.Config, flags *flag.FlagSet, cnf srv.ConfigLoader) (ipPort *srv.IpPort, server srv.Server, logger srv.LowLevelLogger, err error) {
	if !serverconf.HasSection("andrewd") {
		return ipPort, nil, nil, fmt.Errorf("Unable to find andrewd config section")
	}
	logLevelString := serverconf.GetDefault("andrewd", "log_level", "INFO")
	logLevel := zap.NewAtomicLevel()
	logLevel.UnmarshalText([]byte(strings.ToLower(logLevelString)))
	logger, err = srv.SetupLogger("andrewd", &logLevel, flags)
	if err != nil {
		return ipPort, nil, nil, fmt.Errorf("Error setting up logger: %v", err)
	}
	policies, err := cnf.GetPolicies()
	if err != nil {
		return ipPort, nil, nil, err
	}
	ip := serverconf.GetDefault("andrewd", "bind_ip", "0.0.0.0")
	port := int(serverconf.GetInt("andrewd", "bind_port", common.DefaultAndrewdPort))
	certFile := serverconf.GetDefault("andrewd", "cert_file", "")
	keyFile := serverconf.GetDefault("andrewd", "key_file", "")
	pdc, pdcerr := client.NewProxyClient(policies, srv.DefaultConfigLoader{}, logger, certFile, keyFile, "", "", "", serverconf)
	if pdcerr != nil {
		return ipPort, nil, nil, fmt.Errorf("Could not make client: %v", pdcerr)
	}
	pl := conf.PolicyList{}
	for _, p := range policies {
		if p.Config["andrewd"] == "ignore" {
			continue
		}
		pl[p.Index] = p
	}
	transport := &http.Transport{
		MaxIdleConnsPerHost: 100,
		MaxIdleConns:        0,
	}
	if certFile != "" && keyFile != "" {
		tlsConf, err := common.NewClientTLSConfig(certFile, keyFile)
		if err != nil {
			panic(fmt.Sprintf("Error getting TLS config: %v", err))
		}
		transport.TLSClientConfig = tlsConf
		if err = http2.ConfigureTransport(transport); err != nil {
			panic(fmt.Sprintf("Error setting up http2: %v", err))
		}
	}
	httpClient := &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
	}
	a := &AutoAdmin{
		serverconf:     serverconf,
		client:         httpClient,
		hClient:        pdc.NewRequestClient(nil, nil, logger),
		port:           port,
		bindIp:         ip,
		policies:       pl,
		runningForever: false,
		//containerDispersionGauge: []tally.Gauge{}, TODO- add container disp
		logger:       logger,
		logLevel:     logLevel,
		fastRingScan: make(chan struct{}, 32), // 32 just "because"; gives some room for a bunch of ring changes to get queued up before blocking.
	}
	a.hClient.SetUserAgent("Andrewd")
	a.db, err = newDB(&serverconf, "")
	if err != nil {
		return ipPort, nil, nil, err
	}
	if serverconf.HasSection("tracing") {
		clientTracer, clientTraceCloser, err := tracing.Init("andrewd", zap.NewNop(), serverconf.GetSection("tracing"))
		if err != nil {
			return ipPort, nil, nil, fmt.Errorf("Error setting up tracer: %v", err)
		}
		a.clientTraceCloser = clientTraceCloser
		a.pdcCloser = pdc
		enableHTTPTrace := serverconf.GetBool("tracing", "enable_httptrace", true)
		a.client, err = client.NewTracingClient(clientTracer, httpClient, enableHTTPTrace)
		if err != nil {
			return ipPort, nil, nil, fmt.Errorf("Error setting up tracing client: %v", err)
		}
	}

	a.metricsScope, a.metricsCloser = tally.NewRootScope(tally.ScopeOptions{
		Prefix:         "hb_andrewd",
		Tags:           map[string]string{},
		CachedReporter: promreporter.NewReporter(promreporter.Options{}),
		Separator:      promreporter.DefaultSeparator,
	}, time.Second)

	ipPort = &srv.IpPort{Ip: ip, Port: port, CertFile: certFile, KeyFile: keyFile}
	resp := a.hClient.PutAccount(
		context.Background(),
		AdminAccount,
		common.Map2Headers(map[string]string{
			"Content-Length": "0",
			"Content-Type":   "text",
			"X-Timestamp":    fmt.Sprintf("%d", time.Now().Unix()),
		}),
	)
	io.Copy(ioutil.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return ipPort, nil, nil, fmt.Errorf("could not establish admin account: PUT %s gave status code %d", AdminAccount, resp.StatusCode)
	}
	return ipPort, a, a.logger, nil
}
