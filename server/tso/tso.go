// Copyright 2016 TiKV Project Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package tso

import (
	"path"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/pingcap/failpoint"
	"git.sankuai.com/inf/blade-kv-proto/pkg/pdpb"
	"github.com/pingcap/log"
	"github.com/tikv/pd/pkg/errs"
	"github.com/tikv/pd/pkg/etcdutil"
	"github.com/tikv/pd/pkg/tsoutil"
	"github.com/tikv/pd/pkg/typeutil"
	"github.com/tikv/pd/server/kv"
	"github.com/tikv/pd/server/member"
	"go.etcd.io/etcd/clientv3"
	"go.uber.org/zap"
)

const (
	// UpdateTimestampStep is used to update timestamp.
	UpdateTimestampStep  = 50 * time.Millisecond
	updateTimestampGuard = time.Millisecond
	maxLogical           = int64(1 << 18)
)

// TimestampOracle is used to maintain the logic of tso.
type TimestampOracle struct {
	// For tso, set after pd becomes leader.
	ts            unsafe.Pointer
	lastSavedTime atomic.Value

	mu    sync.RWMutex
	lease *member.LeaderLease

	rootPath      string
	member        string
	client        *clientv3.Client
	saveInterval  time.Duration
	maxResetTSGap func() time.Duration
}

// NewTimestampOracle creates a new TimestampOracle.
// TODO: remove saveInterval
func NewTimestampOracle(client *clientv3.Client, rootPath string, member string, saveInterval time.Duration, maxResetTSGap func() time.Duration) *TimestampOracle {
	return &TimestampOracle{
		rootPath:      rootPath,
		client:        client,
		saveInterval:  saveInterval,
		maxResetTSGap: maxResetTSGap,
		member:        member,
	}
}

type atomicObject struct {
	physical time.Time
	logical  int64
}

func (t *TimestampOracle) getTimestampPath() string {
	return path.Join(t.rootPath, "timestamp")
}

func (t *TimestampOracle) loadTimestamp() (time.Time, error) {
	data, err := etcdutil.GetValue(t.client, t.getTimestampPath())
	if err != nil {
		return typeutil.ZeroTime, err
	}
	if len(data) == 0 {
		return typeutil.ZeroTime, nil
	}
	return typeutil.ParseTimestamp(data)
}

func (t *TimestampOracle) checkLease() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.lease != nil && !t.lease.IsExpired()
}

func (t *TimestampOracle) setLease(lease *member.LeaderLease) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lease = lease
}

// save timestamp, if lastTs is 0, we think the timestamp doesn't exist, so create it,
// otherwise, update it.
func (t *TimestampOracle) saveTimestamp(ts time.Time) error {
	data := typeutil.Uint64ToBytes(uint64(ts.UnixNano()))
	key := t.getTimestampPath()

	leaderPath := path.Join(t.rootPath, "leader")
	txn := kv.NewSlowLogTxn(t.client).If(append([]clientv3.Cmp{}, clientv3.Compare(clientv3.Value(leaderPath), "=", t.member))...)
	resp, err := txn.Then(clientv3.OpPut(key, string(data))).Commit()
	if err != nil {
		return errs.ErrEtcdKVPut.Wrap(err).GenWithStackByCause()
	}
	if !resp.Succeeded {
		return errs.ErrEtcdTxn.FastGenByArgs()
	}

	t.lastSavedTime.Store(ts)

	return nil
}

// SyncTimestamp is used to synchronize the timestamp.
func (t *TimestampOracle) SyncTimestamp(lease *member.LeaderLease) error {
	tsoCounter.WithLabelValues("sync").Inc()

	t.setLease(lease)

	failpoint.Inject("delaySyncTimestamp", func() {
		time.Sleep(time.Second)
	})

	last, err := t.loadTimestamp()
	if err != nil {
		return err
	}

	next := time.Now()
	failpoint.Inject("fallBackSync", func() {
		next = next.Add(time.Hour)
	})

	// If the current system time minus the saved etcd timestamp is less than `updateTimestampGuard`,
	// the timestamp allocation will start from the saved etcd timestamp temporarily.
	if typeutil.SubTimeByWallClock(next, last) < updateTimestampGuard {
		log.Error("system time may be incorrect", zap.Time("last", last), zap.Time("next", next), errs.ZapError(errs.ErrIncorrectSystemTime))
		next = last.Add(updateTimestampGuard)
	}

	save := next.Add(t.saveInterval)
	if err = t.saveTimestamp(save); err != nil {
		tsoCounter.WithLabelValues("err_save_sync_ts").Inc()
		return err
	}

	tsoCounter.WithLabelValues("sync_ok").Inc()
	log.Info("sync and save timestamp", zap.Time("last", last), zap.Time("save", save), zap.Time("next", next))

	current := &atomicObject{
		physical: next,
	}
	atomic.StorePointer(&t.ts, unsafe.Pointer(current))

	return nil
}

// ResetUserTimestamp update the physical part with specified tso.
func (t *TimestampOracle) ResetUserTimestamp(tso uint64) error {
	if !t.checkLease() {
		tsoCounter.WithLabelValues("err_lease_reset_ts").Inc()
		return errs.ErrResetUserTimestamp.FastGenByArgs("lease expired")
	}
	physical, _ := tsoutil.ParseTS(tso)
	next := physical.Add(time.Millisecond)
	prev := (*atomicObject)(atomic.LoadPointer(&t.ts))

	// do not update
	if typeutil.SubTimeByWallClock(next, prev.physical) <= 3*updateTimestampGuard {
		tsoCounter.WithLabelValues("err_reset_small_ts").Inc()
		return errs.ErrResetUserTimestamp.FastGenByArgs("the specified ts too small than now")
	}

	if typeutil.SubTimeByWallClock(next, prev.physical) >= t.maxResetTSGap() {
		tsoCounter.WithLabelValues("err_reset_large_ts").Inc()
		return errs.ErrResetUserTimestamp.FastGenByArgs("the specified ts too large than now")
	}

	save := next.Add(t.saveInterval)
	if err := t.saveTimestamp(save); err != nil {
		tsoCounter.WithLabelValues("err_save_reset_ts").Inc()
		return err
	}
	update := &atomicObject{
		physical: next,
	}
	atomic.CompareAndSwapPointer(&t.ts, unsafe.Pointer(prev), unsafe.Pointer(update))
	tsoCounter.WithLabelValues("reset_tso_ok").Inc()
	return nil
}

// UpdateTimestamp is used to update the timestamp.
// This function will do two things:
// 1. When the logical time is going to be used up, the current physical time needs to increase.
// 2. If the time window is not enough, which means the saved etcd time minus the next physical time
//    is less than or equal to `updateTimestampGuard`, it will need to be updated and save the
//    next physical time plus `TsoSaveInterval` into etcd.
//
// Here is some constraints that this function must satisfy:
// 1. The physical time is monotonically increasing.
// 2. The saved time is monotonically increasing.
// 3. The physical time is always less than the saved timestamp.
func (t *TimestampOracle) UpdateTimestamp() error {
	prev := (*atomicObject)(atomic.LoadPointer(&t.ts))
	now := time.Now()

	failpoint.Inject("fallBackUpdate", func() {
		now = now.Add(time.Hour)
	})

	tsoCounter.WithLabelValues("save").Inc()

	jetLag := typeutil.SubTimeByWallClock(now, prev.physical)
	if jetLag > 3*UpdateTimestampStep {
		log.Warn("clock offset", zap.Duration("jet-lag", jetLag), zap.Time("prev-physical", prev.physical), zap.Time("now", now))
		tsoCounter.WithLabelValues("slow_save").Inc()
	}

	if jetLag < 0 {
		tsoCounter.WithLabelValues("system_time_slow").Inc()
	}

	var next time.Time
	prevLogical := atomic.LoadInt64(&prev.logical)
	// If the system time is greater, it will be synchronized with the system time.
	if jetLag > updateTimestampGuard {
		next = now
	} else if prevLogical > maxLogical/2 {
		// The reason choosing maxLogical/2 here is that it's big enough for common cases.
		// Because there is enough timestamp can be allocated before next update.
		log.Warn("the logical time may be not enough", zap.Int64("prev-logical", prevLogical))
		next = prev.physical.Add(time.Millisecond)
	} else {
		// It will still use the previous physical time to alloc the timestamp.
		tsoCounter.WithLabelValues("skip_save").Inc()
		return nil
	}

	// It is not safe to increase the physical time to `next`.
	// The time window needs to be updated and saved to etcd.
	if typeutil.SubTimeByWallClock(t.lastSavedTime.Load().(time.Time), next) <= updateTimestampGuard {
		save := next.Add(t.saveInterval)
		if err := t.saveTimestamp(save); err != nil {
			tsoCounter.WithLabelValues("err_save_update_ts").Inc()
			return err
		}
	}

	current := &atomicObject{
		physical: next,
		logical:  0,
	}

	atomic.StorePointer(&t.ts, unsafe.Pointer(current))
	tsoGauge.WithLabelValues("tso").Set(float64(next.Unix()))

	return nil
}

// ResetTimestamp is used to reset the timestamp.
func (t *TimestampOracle) ResetTimestamp() {
	zero := &atomicObject{
		physical: typeutil.ZeroTime,
	}
	atomic.StorePointer(&t.ts, unsafe.Pointer(zero))
	t.setLease(nil)
}

var maxRetryCount = 10

// GetRespTS is used to get a timestamp.
func (t *TimestampOracle) GetRespTS(count uint32) (pdpb.Timestamp, error) {
	var resp pdpb.Timestamp

	if count == 0 {
		return resp, errs.ErrGenerateTimestamp.FastGenByArgs("tso count should be positive")
	}

	failpoint.Inject("skipRetryGetTS", func() {
		maxRetryCount = 1
	})

	for i := 0; i < maxRetryCount; i++ {
		current := (*atomicObject)(atomic.LoadPointer(&t.ts))
		if current == nil || current.physical == typeutil.ZeroTime {
			// If it's leader, maybe SyncTimestamp hasn't completed yet
			if t.checkLease() {
				log.Info("sync hasn't completed yet, wait for a while")
				time.Sleep(200 * time.Millisecond)
				continue
			}
			log.Error("invalid timestamp", zap.Any("timestamp", current))
			return pdpb.Timestamp{}, errs.ErrGenerateTimestamp.FastGenByArgs("timestamp in memory isn't initialized")
		}

		resp.Physical = current.physical.UnixNano() / int64(time.Millisecond)
		resp.Logical = atomic.AddInt64(&current.logical, int64(count))
		if resp.Logical >= maxLogical {
			log.Error("logical part outside of max logical interval, please check ntp time",
				zap.Reflect("response", resp),
				zap.Int("retry-count", i), errs.ZapError(errs.ErrLogicOverflow))
			tsoCounter.WithLabelValues("logical_overflow").Inc()
			time.Sleep(UpdateTimestampStep)
			continue
		}
		// In case lease expired after the first check.
		if !t.checkLease() {
			return pdpb.Timestamp{}, errs.ErrGenerateTimestamp.FastGenByArgs("not the pd leader")
		}
		return resp, nil
	}
	return resp, errs.ErrGenerateTimestamp.FastGenByArgs("maximum number of retries exceeded")
}

// Now returns the current tso time.
func (t *TimestampOracle) Now() (time.Time, error) {
	resp, err := t.GetRespTS(1)
	if err != nil {
		return time.Time{}, err
	}
	tm, _ := tsoutil.ParseTimestamp(resp)
	return tm, nil
}
