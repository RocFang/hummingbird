//  Copyright (c) 2015-2017 Rackspace
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

package middleware

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/RocFang/hummingbird/client"
	"github.com/RocFang/hummingbird/common"
	"github.com/RocFang/hummingbird/common/ring"
	"github.com/RocFang/hummingbird/common/srv"
	"go.uber.org/zap"
)

var (
	serverInfo     = make(map[string]interface{})
	sil            sync.Mutex
	excludeHeaders = []string{
		"X-Account-Sysmeta-",
		"X-Container-Sysmeta-",
		"X-Object-Sysmeta-",
		"X-Object-Transient-Sysmeta-",
		"X-Backend-",
	}
)

func RegisterInfo(name string, data interface{}) {
	sil.Lock()
	defer sil.Unlock()
	serverInfo[name] = data
}

func serverInfoDump() ([]byte, error) {
	sil.Lock()
	defer sil.Unlock()
	data, err := json.Marshal(serverInfo)
	return data, err
}

// Used to capture response from a subrequest
type captureWriter struct {
	status int
	body   []byte
	header http.Header
}

func (x *captureWriter) Header() http.Header    { return x.header }
func (x *captureWriter) WriteHeader(status int) { x.status = status }
func (x *captureWriter) Write(b []byte) (int, error) {
	x.body = append(x.body, b...)
	return len(b), nil
}

func NewCaptureWriter() *captureWriter {
	return &captureWriter{header: make(http.Header)}
}

type AccountInfo struct {
	ContainerCount int64
	ObjectCount    int64
	ObjectBytes    int64
	Metadata       map[string]string
	SysMetadata    map[string]string
	StatusCode     int `json:"status"`
}

type AuthorizeFunc func(r *http.Request) (bool, int)
type subrequestCopy func(dst, src *http.Request)

type ProxyContextMiddleware struct {
	next               http.Handler
	log                srv.LowLevelLogger
	Cache              ring.MemcacheRing
	proxyClientFactory client.ProxyClient
	debugResponses     bool
}

type ProxyContext struct {
	*ProxyContextMiddleware
	C                client.RequestClient
	Authorize        AuthorizeFunc
	RemoteUsers      []string
	StorageOwner     bool
	ResellerRequest  bool
	ACL              string
	subrequestCopy   subrequestCopy
	Logger           *zap.Logger
	TxId             string
	responseSent     time.Time
	status           int
	accountInfoCache map[string]*AccountInfo
	depth            int
	Source           string
	S3Auth           *S3AuthInfo
}

func GetProxyContext(r *http.Request) *ProxyContext {
	if rv := r.Context().Value("proxycontext"); rv != nil {
		return rv.(*ProxyContext)
	}
	return nil
}

func (ctx *ProxyContext) Response() (time.Time, int) {
	return ctx.responseSent, ctx.status
}

func (ctx *ProxyContext) addSubrequestCopy(f subrequestCopy) {
	if ctx.subrequestCopy == nil {
		ctx.subrequestCopy = f
		return
	}
	ca := ctx.subrequestCopy
	ctx.subrequestCopy = func(dst, src *http.Request) {
		ca(dst, src)
		f(dst, src)
	}
}

func getPathParts(request *http.Request) (bool, string, string, string) {
	apiRequest, account, container, object := getPathSegments(request.URL.Path)
	return apiRequest == "v1", account, container, object
}

func getPathSegments(requestPath string) (string, string, string, string) {
	parts := strings.SplitN(requestPath, "/", 5)
	switch len(parts) {
	case 5:
		return parts[1], parts[2], parts[3], parts[4]
	case 4:
		return parts[1], parts[2], parts[3], ""
	case 3:
		return parts[1], parts[2], "", ""
	case 2:
		return parts[1], "", "", ""
	default:
		return "", "", "", ""
	}
}

func (pc *ProxyContext) GetAccountInfo(ctx context.Context, account string) (*AccountInfo, error) {
	key := fmt.Sprintf("account/%s", account)
	ai := pc.accountInfoCache[key]
	if ai == nil {
		if err := pc.Cache.GetStructured(ctx, key, &ai); err != nil {
			ai = nil
		}
	}
	if ai != nil && ai.StatusCode != 0 && ai.StatusCode/100 != 2 {
		return nil, fmt.Errorf("%d error retrieving info for account %s", ai.StatusCode, account)
	}
	if ai == nil {
		resp := pc.C.HeadAccount(ctx, account, nil)
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			pc.Cache.Set(ctx, key, &AccountInfo{StatusCode: resp.StatusCode}, 30)
			return nil, fmt.Errorf("%d error retrieving info for account %s", resp.StatusCode, account)
		}
		ai = &AccountInfo{
			Metadata:    make(map[string]string),
			SysMetadata: make(map[string]string),
			StatusCode:  resp.StatusCode,
		}
		var err error
		if ai.ContainerCount, err = strconv.ParseInt(resp.Header.Get("X-Account-Container-Count"), 10, 64); err != nil {
			return nil, fmt.Errorf("Error retrieving info for account %s : %s", account, err)
		}
		if ai.ObjectCount, err = strconv.ParseInt(resp.Header.Get("X-Account-Object-Count"), 10, 64); err != nil {
			return nil, fmt.Errorf("Error retrieving info for account %s : %s", account, err)
		}
		if ai.ObjectBytes, err = strconv.ParseInt(resp.Header.Get("X-Account-Bytes-Used"), 10, 64); err != nil {
			return nil, fmt.Errorf("Error retrieving info for account %s : %s", account, err)
		}
		for k := range resp.Header {
			if strings.HasPrefix(k, "X-Account-Meta-") {
				ai.Metadata[k[15:]] = resp.Header.Get(k)
			} else if strings.HasPrefix(k, "X-Account-Sysmeta-") {
				ai.SysMetadata[k[18:]] = resp.Header.Get(k)
			}
		}
		pc.Cache.Set(ctx, key, ai, 30)
	}
	return ai, nil
}

func (pc *ProxyContext) InvalidateAccountInfo(ctx context.Context, account string) {
	key := fmt.Sprintf("account/%s", account)
	delete(pc.accountInfoCache, key)
	pc.Cache.Delete(ctx, key)
}

func (pc *ProxyContext) AutoCreateAccount(ctx context.Context, account string, headers http.Header) {
	h := http.Header{"X-Timestamp": []string{common.GetTimestamp()},
		"X-Trans-Id": []string{pc.TxId}}
	for key := range headers {
		if strings.HasPrefix(key, "X-Account-Sysmeta-") {
			h[key] = []string{headers.Get(key)}
		}
	}
	resp := pc.C.PutAccount(ctx, account, h)
	if resp.StatusCode/100 == 2 {
		pc.InvalidateAccountInfo(ctx, account)
	}
}

func (pc *ProxyContext) newSubrequest(method, urlStr string, body io.Reader, req *http.Request, source string) (*http.Request, error) {
	if source == "" {
		panic("Programmer error: You must supply the source with newSubrequest. If you want the subrequest to be treated a user request (billing, quotas, etc.) you can set the source to \"-\"")
	}
	if source == "-" {
		source = ""
	}
	subreq, err := http.NewRequest(method, urlStr, body)
	if err != nil {
		return nil, err
	}
	subctx := &ProxyContext{
		ProxyContextMiddleware: pc.ProxyContextMiddleware,
		Authorize:              pc.Authorize,
		RemoteUsers:            pc.RemoteUsers,
		subrequestCopy:         pc.subrequestCopy,
		Logger:                 pc.Logger.With(zap.String("src", source)),
		C:                      pc.C,
		TxId:                   pc.TxId,
		accountInfoCache:       pc.accountInfoCache,
		status:                 500,
		depth:                  pc.depth + 1,
		Source:                 source,
		S3Auth:                 pc.S3Auth,
	}
	subreq = subreq.WithContext(context.WithValue(req.Context(), "proxycontext", subctx))
	if subctx.subrequestCopy != nil {
		subctx.subrequestCopy(subreq, req)
	}
	if v := req.Header.Get("Referer"); v != "" {
		subreq.Header.Set("Referer", v)
	}
	subreq.Header.Set("X-Trans-Id", subctx.TxId)
	subreq.Header.Set("X-Timestamp", common.GetTimestamp())
	return subreq, nil
}

func (pc *ProxyContext) serveHTTPSubrequest(writer http.ResponseWriter, subreq *http.Request) {
	subctx := GetProxyContext(subreq)
	// TODO: check subctx.depth
	subwriter := srv.NewCustomWriter(writer, func(w http.ResponseWriter, status int) int {
		subctx.responseSent = time.Now()
		subctx.status = status
		return status
	})
	subwriter.Header().Set("X-Trans-Id", subctx.TxId)
	pc.next.ServeHTTP(subwriter, subreq)
}

func (m *ProxyContextMiddleware) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if !srv.ValidateRequest(writer, request) {
		return
	}

	if request.URL.Path == "/info" {
		if request.URL.Query().Get("swiftinfo_sig") != "" || request.URL.Query().Get("swiftinfo_expires") != "" {
			writer.WriteHeader(403)
			return
		}
		if request.Method == "GET" {
			if data, err := serverInfoDump(); err != nil {
				srv.StandardResponse(writer, 500)
			} else {
				writer.Header().Set("Content-Type", "application/json; charset=UTF-8")
				writer.WriteHeader(200)
				writer.Write(data)
			}
			return
		} else if request.Method == "OPTIONS" {
			writer.Header().Set("Allow", "HEAD, GET, OPTIONS")
			writer.WriteHeader(200)
			return
		} else if request.Method == "HEAD" {
			if _, err := serverInfoDump(); err != nil {
				srv.StandardResponse(writer, 500)
			} else {
				writer.WriteHeader(200)
			}
			return
		}
	}

	for k := range request.Header {
		for _, ex := range excludeHeaders {
			if strings.HasPrefix(k, ex) || k == "X-Timestamp" {
				delete(request.Header, k)
			}
		}
	}

	transId := common.GetTransactionId()
	request.Header.Set("X-Trans-Id", transId)
	writer.Header().Set("X-Trans-Id", transId)
	writer.Header().Set("X-Openstack-Request-Id", transId)
	request.Header.Set("X-Timestamp", common.GetTimestamp())
	logr := m.log.With(zap.String("txn", transId))
	pc := &ProxyContext{
		ProxyContextMiddleware: m,
		Authorize:              nil,
		Logger:                 logr,
		TxId:                   transId,
		status:                 500,
		accountInfoCache:       make(map[string]*AccountInfo),
		C:                      m.proxyClientFactory.NewRequestClient(m.Cache, make(map[string]*client.ContainerInfo), logr),
	}
	// we'll almost certainly need the AccountInfo and ContainerInfo for the current path, so pre-fetch them in parallel.
	apiRequest, account, container, _ := getPathParts(request)
	if apiRequest && account != "" {
		wg := &sync.WaitGroup{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			pc.GetAccountInfo(request.Context(), account)
		}()
		if container != "" {
			wg.Add(1)
			go func() {
				defer wg.Done()
				pc.C.GetContainerInfo(request.Context(), account, container)
			}()
		}
		wg.Wait()
	}
	newWriter := srv.NewCustomWriter(writer, func(w http.ResponseWriter, status int) int {
		// strip out any bad headers before calling real WriteHeader
		for k := range w.Header() {
			if k == "X-Account-Sysmeta-Project-Domain-Id" {
				w.Header().Set("X-Account-Project-Domain-Id", w.Header().Get(k))
			}
			for _, ex := range excludeHeaders {
				if strings.HasPrefix(k, ex) {
					delete(w.Header(), k)
				}
			}
		}
		if status == http.StatusUnauthorized && w.Header().Get("Www-Authenticate") == "" {
			if account != "" {
				w.Header().Set("Www-Authenticate", fmt.Sprintf("Swift realm=\"%s\"", common.Urlencode(account)))
			} else {
				w.Header().Set("Www-Authenticate", "Swift realm=\"unknown\"")
			}
		}

		if m.debugResponses && status/100 != 2 {
			buf := debug.Stack()
			w.Header().Set("X-Source-Code", string(buf))
		}

		pc.responseSent = time.Now()
		pc.status = status
		return status
	})
	request = request.WithContext(context.WithValue(request.Context(), "proxycontext", pc))
	m.next.ServeHTTP(newWriter, request)
}

func NewContext(debugResponses bool, mc ring.MemcacheRing, log srv.LowLevelLogger, proxyClientFactory client.ProxyClient) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return &ProxyContextMiddleware{
			Cache:              mc,
			log:                log,
			next:               next,
			proxyClientFactory: proxyClientFactory,
			debugResponses:     debugResponses,
		}
	}
}
