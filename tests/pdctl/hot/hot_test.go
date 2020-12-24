// Copyright 2019 TiKV Project Authors.
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

package hot_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/gogo/protobuf/proto"
	. "github.com/pingcap/check"
	"git.sankuai.com/inf/blade-kv-proto/pkg/metapb"
	"git.sankuai.com/inf/blade-kv-proto/pkg/pdpb"
	"github.com/tikv/pd/server"
	"github.com/tikv/pd/server/api"
	"github.com/tikv/pd/server/core"
	"github.com/tikv/pd/server/statistics"
	"github.com/tikv/pd/tests"
	"github.com/tikv/pd/tests/pdctl"
)

func Test(t *testing.T) {
	TestingT(t)
}

var _ = Suite(&hotTestSuite{})

type hotTestSuite struct{}

func (s *hotTestSuite) SetUpSuite(c *C) {
	server.EnableZap = true
}

func (s *hotTestSuite) TestHot(c *C) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cluster, err := tests.NewTestCluster(ctx, 1)
	c.Assert(err, IsNil)
	err = cluster.RunInitialServers()
	c.Assert(err, IsNil)
	cluster.WaitLeader()
	pdAddr := cluster.GetConfig().GetClientURL()
	cmd := pdctl.InitCommand()

	store := metapb.Store{
		Id:      1,
		Address: "tikv1",
		State:   metapb.StoreState_Up,
		Version: "2.0.0",
	}

	leaderServer := cluster.GetServer(cluster.GetLeader())
	c.Assert(leaderServer.BootstrapCluster(), IsNil)
	pdctl.MustPutStore(c, leaderServer.GetServer(), store.Id, store.State, store.Labels)
	defer cluster.Destroy()

	// test hot store
	ss := leaderServer.GetStore(1)
	now := time.Now().Second()
	newStats := proto.Clone(ss.GetStoreStats()).(*pdpb.StoreStats)
	bytesWritten := uint64(8 * 1024 * 1024)
	bytesRead := uint64(16 * 1024 * 1024)
	keysWritten := uint64(2000)
	keysRead := uint64(4000)
	newStats.BytesWritten = bytesWritten
	newStats.BytesRead = bytesRead
	newStats.KeysWritten = keysWritten
	newStats.KeysRead = keysRead
	rc := leaderServer.GetRaftCluster()
	for i := statistics.DefaultWriteMfSize; i > 0; i-- {
		newStats.Interval = &pdpb.TimeInterval{StartTimestamp: uint64(now - 10*i), EndTimestamp: uint64(now - 10*i + 10)}
		rc.GetStoresStats().Observe(ss.GetID(), newStats)
	}

	args := []string{"-u", pdAddr, "hot", "store"}
	_, output, err := pdctl.ExecuteCommandC(cmd, args...)
	c.Assert(err, IsNil)
	hotStores := api.HotStoreStats{}
	c.Assert(json.Unmarshal(output, &hotStores), IsNil)
	c.Assert(hotStores.BytesWriteStats[1], Equals, float64(bytesWritten)/10)
	c.Assert(hotStores.BytesReadStats[1], Equals, float64(bytesRead)/10)
	c.Assert(hotStores.KeysWriteStats[1], Equals, float64(keysWritten)/10)
	c.Assert(hotStores.KeysReadStats[1], Equals, float64(keysRead)/10)

	// test hot region
	statistics.Denoising = false
	reportInterval := uint64(3) // need to be minHotRegionReportInterval < reportInterval < 3*RegionHeartBeatReportInterval
	args = []string{"-u", pdAddr, "config", "set", "hot-region-cache-hits-threshold", "0"}
	_, _, err = pdctl.ExecuteCommandC(cmd, args...)
	c.Assert(err, IsNil)

	testHot := func(hotRegionID, hotStoreID uint64, hotType string) {
		args = []string{"-u", pdAddr, "hot", hotType}
		_, output, e := pdctl.ExecuteCommandC(cmd, args...)
		hotRegion := statistics.StoreHotPeersInfos{}
		c.Assert(e, IsNil)
		c.Assert(json.Unmarshal(output, &hotRegion), IsNil)
		c.Assert(hotRegion.AsLeader, HasKey, hotStoreID)
		c.Assert(hotRegion.AsLeader[hotStoreID].Count, Equals, 1)
		c.Assert(hotRegion.AsLeader[hotStoreID].Stats[0].RegionID, Equals, hotRegionID)
	}

	hotReadRegionID, hotWriteRegionID, hotStoreID := uint64(3), uint64(2), uint64(1)
	pdctl.MustPutRegion(c, cluster, hotReadRegionID, hotStoreID, []byte("b"), []byte("c"), core.SetReadBytes(1000000000), core.SetReportInterval(reportInterval))
	pdctl.MustPutRegion(c, cluster, hotWriteRegionID, hotStoreID, []byte("c"), []byte("d"), core.SetWrittenBytes(1000000000), core.SetReportInterval(reportInterval))
	time.Sleep(5000 * time.Millisecond)
	testHot(hotReadRegionID, hotStoreID, "read")
	testHot(hotWriteRegionID, hotStoreID, "write")
}
