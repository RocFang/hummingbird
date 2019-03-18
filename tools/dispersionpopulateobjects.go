package tools

// In /etc/hummingbird/andrewd-server.conf:
// [dispersion-populate-objects]
// retry_time = 3600     # seconds before retrying a failed populate pass
// report_interval = 600 # seconds between progress reports
// concurrency = 0       # how many cpu cores to use while populating

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"sync/atomic"
	"time"

	"github.com/RocFang/hummingbird/common"
	"github.com/RocFang/hummingbird/common/conf"
	"github.com/uber-go/tally"
	"go.uber.org/zap"
)

type dispersionPopulateObjects struct {
	aa               *AutoAdmin
	retryTime        time.Duration
	reportInterval   time.Duration
	concurrency      uint64
	passesMetric     tally.Timer
	passesMetrics    map[int]tally.Timer
	successesMetrics map[int]tally.Counter
	errorsMetrics    map[int]tally.Counter
}

func newDispersionPopulateObjects(aa *AutoAdmin) *dispersionPopulateObjects {
	dpo := &dispersionPopulateObjects{
		aa:               aa,
		retryTime:        time.Duration(aa.serverconf.GetInt("dispersion-populate-objects", "retry_time", 3600)) * time.Second,
		reportInterval:   time.Duration(aa.serverconf.GetInt("dispersion-populate-objects", "report_interval", 600)) * time.Second,
		passesMetric:     aa.metricsScope.Timer("disp_pop_obj_passes"),
		passesMetrics:    map[int]tally.Timer{},
		successesMetrics: map[int]tally.Counter{},
		errorsMetrics:    map[int]tally.Counter{},
	}
	concurrency := aa.serverconf.GetInt("dispersion-populate-objects", "concurrency", 0)
	if concurrency < 1 {
		concurrency = 0
	}
	dpo.concurrency = uint64(concurrency)
	return dpo
}

func (dpo *dispersionPopulateObjects) runForever() {
	for {
		sleepFor := dpo.runOnce()
		if sleepFor < 0 {
			break
		}
		time.Sleep(sleepFor)
	}
}

func (dpo *dispersionPopulateObjects) runOnce() time.Duration {
	defer dpo.passesMetric.Start().Stop()
	start := time.Now()
	logger := dpo.aa.logger.With(zap.String("process", "dispersion populate objects"))
	logger.Debug("starting pass")
	if err := dpo.aa.db.startProcessPass("dispersion populate", "object-overall", 0); err != nil {
		logger.Error("startProcessPass", zap.Error(err))
	}
	failed := false
	for _, policy := range dpo.aa.policies {
		if !policy.Deprecated {
			if !dpo.putDispersionObjects(logger, policy) {
				failed = true
			}
		}
	}
	if err := dpo.aa.db.progressProcessPass("dispersion populate", "object-overall", 0, fmt.Sprintf("%d policies", len(dpo.aa.policies))); err != nil {
		logger.Error("progressProcessPass", zap.Error(err))
	}
	if err := dpo.aa.db.completeProcessPass("dispersion populate", "object-overall", 0); err != nil {
		logger.Error("completeProcessPass", zap.Error(err))
	}
	if !failed {
		logger.Debug("pass completed successfully")
		return -1
	}
	sleepFor := time.Until(start.Add(dpo.retryTime))
	if sleepFor < 0 {
		sleepFor = 0
	}
	logger.Debug("pass complete but with errors", zap.String("next attempt", sleepFor.String()))
	return sleepFor
}

func (dpo *dispersionPopulateObjects) putDispersionObjects(logger *zap.Logger, policy *conf.Policy) bool {
	if dpo.passesMetrics[policy.Index] == nil {
		dpo.passesMetrics[policy.Index] = dpo.aa.metricsScope.Timer(fmt.Sprintf("disp_pop_obj_%d_passes", policy.Index))
		dpo.successesMetrics[policy.Index] = dpo.aa.metricsScope.Counter(fmt.Sprintf("disp_pop_obj_%d_successes", policy.Index))
		dpo.errorsMetrics[policy.Index] = dpo.aa.metricsScope.Counter(fmt.Sprintf("disp_pop_obj_%d_errors", policy.Index))
	}
	defer dpo.passesMetrics[policy.Index].Start().Stop()
	start := time.Now()
	logger = logger.With(zap.Int("policy", policy.Index))
	container := fmt.Sprintf("disp-objs-%d", policy.Index)
	resp := dpo.aa.hClient.HeadObject(context.Background(), AdminAccount, container, "object-init", nil)
	io.Copy(ioutil.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		logger.Debug("object-init already exists; no need to populate objects")
		return true
	}
	resp = dpo.aa.hClient.PutContainer(
		context.Background(),
		AdminAccount,
		container,
		common.Map2Headers(map[string]string{
			"Content-Length":   "0",
			"Content-Type":     "text",
			"X-Timestamp":      fmt.Sprintf("%d", time.Now().Unix()),
			"X-Storage-Policy": policy.Name,
		}),
	)
	if resp.StatusCode/100 != 2 {
		logger.Error("PUT", zap.String("account", AdminAccount), zap.String("container", container), zap.Int("status", resp.StatusCode))
		return false
	}
	objectRing, resp := dpo.aa.hClient.ObjectRingFor(context.Background(), AdminAccount, container)
	if objectRing == nil || resp != nil {
		if resp == nil {
			logger.Error("no ring")
		} else {
			logger.Error("no ring", zap.Int("status", resp.StatusCode))
		}
		return false
	}
	logger.Debug("starting policy pass")
	if err := dpo.aa.db.startProcessPass("dispersion populate", "object", policy.Index); err != nil {
		logger.Error("startProcessPass", zap.Error(err))
	}
	objectNames := make(chan string, 100)
	cancel := make(chan struct{})
	var successes int64
	var errors int64
	go generateDispersionNames(container, "", objectRing, objectNames, cancel, dpo.concurrency)
	progressDone := make(chan struct{})
	go func() {
		for {
			select {
			case <-cancel:
				close(progressDone)
				return
			case <-time.After(dpo.reportInterval):
				s := atomic.LoadInt64(&successes)
				e := atomic.LoadInt64(&errors)
				var eta time.Duration
				if s+e > 0 {
					eta = time.Duration(int64(time.Since(start)) / (s + e) * (int64(objectRing.PartitionCount()) - s - e))
				}
				logger.Debug("progress", zap.Int64("successes", s), zap.Int64("errors", e), zap.String("eta", eta.String()))
				if err := dpo.aa.db.progressProcessPass("dispersion populate", "object", policy.Index, fmt.Sprintf("%d of %d partitions, %d successes, %d errors, %s eta", s+e, objectRing.PartitionCount(), s, e, eta)); err != nil {
					logger.Error("progressProcessPass", zap.Error(err))
				}
			}
		}
	}()
	for object := range objectNames {
		xtimestamp := time.Now()
		resp := dpo.aa.hClient.PutObject(
			context.Background(),
			AdminAccount,
			container,
			object,
			common.Map2Headers(map[string]string{
				"Content-Length": "0",
				"Content-Type":   "text",
				"X-Timestamp":    common.CanonicalTimestampFromTime(xtimestamp),
			}),
			bytes.NewReader([]byte{}),
		)
		io.Copy(ioutil.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode/100 == 2 {
			atomic.AddInt64(&successes, 1)
			dpo.successesMetrics[policy.Index].Inc(1)
		} else {
			dpo.errorsMetrics[policy.Index].Inc(1)
			if atomic.AddInt64(&errors, 1) > 1000 {
				// After 1000 errors we'll just assume "things" are broken
				// right now and try again next pass.
				break
			}
			logger.Error("PUT", zap.String("account", AdminAccount), zap.String("container", container), zap.String("object", object), zap.Int("status", resp.StatusCode))
		}
	}
	close(cancel)
	<-progressDone
	if errors == 0 {
		xtimestamp := time.Now()
		resp = dpo.aa.hClient.PutObject(
			context.Background(),
			AdminAccount,
			container,
			"object-init",
			common.Map2Headers(map[string]string{
				"Content-Length": "0",
				"Content-Type":   "text",
				"X-Timestamp":    common.CanonicalTimestampFromTime(xtimestamp),
			}),
			bytes.NewReader([]byte{}),
		)
		io.Copy(ioutil.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			logger.Error("PUT", zap.String("account", AdminAccount), zap.String("container", container), zap.String("object", "object-init"), zap.Int("status", resp.StatusCode))
			errors++
			dpo.errorsMetrics[policy.Index].Inc(1)
		}
	}
	if err := dpo.aa.db.progressProcessPass("dispersion populate", "object", policy.Index, fmt.Sprintf("%d successes, %d errors", successes, errors)); err != nil {
		logger.Error("progressProcessPass", zap.Error(err))
	}
	if err := dpo.aa.db.completeProcessPass("dispersion populate", "object", policy.Index); err != nil {
		logger.Error("completeProcessPass", zap.Error(err))
	}
	if errors == 0 {
		logger.Debug("policy pass completed successfully", zap.Int64("successes", successes), zap.Int64("errors", errors))
		return true
	}
	logger.Debug("policy pass completed with errors - will try again later", zap.Int64("successes", successes), zap.Int64("errors", errors))
	return false
}
