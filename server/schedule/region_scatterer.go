// Copyright 2017 TiKV Project Authors.
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

package schedule

import (
	"math"
	"math/rand"
	"sync"

	"github.com/pingcap/errors"
	"git.sankuai.com/inf/blade-kv-proto.git/pkg/metapb"
	"github.com/pingcap/log"
	"github.com/tikv/pd/pkg/errs"
	"github.com/tikv/pd/server/core"
	"github.com/tikv/pd/server/schedule/filter"
	"github.com/tikv/pd/server/schedule/operator"
	"github.com/tikv/pd/server/schedule/opt"
	"go.uber.org/zap"
)

const regionScatterName = "region-scatter"

type selectedStores struct {
	mu sync.Mutex
	// If checkExist is true, after each putting operation, an entry with the key constructed by group and storeID would be put
	// into "stores" map. And the entry with the same key (storeID, group) couldn't be put before "stores" being reset
	checkExist bool
	// TODO: support auto-gc for the stores
	stores map[string]map[uint64]struct{} // group -> StoreID -> struct{}
	// TODO: support auto-gc for the groupDistribution
	groupDistribution map[string]map[uint64]uint64 // group -> StoreID -> count
}

func newSelectedStores(checkExist bool) *selectedStores {
	return &selectedStores{
		checkExist:        checkExist,
		stores:            make(map[string]map[uint64]struct{}),
		groupDistribution: make(map[string]map[uint64]uint64),
	}
}

func (s *selectedStores) put(id uint64, group string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.checkExist {
		placed, ok := s.stores[group]
		if !ok {
			placed = map[uint64]struct{}{}
		}
		if _, ok := placed[id]; ok {
			return false
		}
		placed[id] = struct{}{}
		s.stores[group] = placed
	}
	distribution, ok := s.groupDistribution[group]
	if !ok {
		distribution = make(map[uint64]uint64)
	}
	distribution[id] = distribution[id] + 1
	s.groupDistribution[group] = distribution
	return true
}

func (s *selectedStores) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.checkExist {
		return
	}
	s.stores = make(map[string]map[uint64]struct{})
}

func (s *selectedStores) get(id uint64, group string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	distribution, ok := s.groupDistribution[group]
	if !ok {
		return 0
	}
	count, ok := distribution[id]
	if !ok {
		return 0
	}
	return count
}

func (s *selectedStores) newFilters(scope, group string) []filter.Filter {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.checkExist {
		return nil
	}
	cloned := make(map[uint64]struct{})
	if groupPlaced, ok := s.stores[group]; ok {
		for id := range groupPlaced {
			cloned[id] = struct{}{}
		}
	}
	return []filter.Filter{filter.NewExcludedFilter(scope, nil, cloned)}
}

// RegionScatterer scatters regions.
type RegionScatterer struct {
	name           string
	cluster        opt.Cluster
	ordinaryEngine engineContext
	specialEngines map[string]engineContext
}

// NewRegionScatterer creates a region scatterer.
// RegionScatter is used for the `Lightning`, it will scatter the specified regions before import data.
func NewRegionScatterer(cluster opt.Cluster) *RegionScatterer {
	return &RegionScatterer{
		name:           regionScatterName,
		cluster:        cluster,
		ordinaryEngine: newEngineContext(filter.NewOrdinaryEngineFilter(regionScatterName)),
		specialEngines: make(map[string]engineContext),
	}
}

type engineContext struct {
	filters        []filter.Filter
	selectedPeer   *selectedStores
	selectedLeader *selectedStores
}

func newEngineContext(filters ...filter.Filter) engineContext {
	filters = append(filters, filter.StoreStateFilter{ActionScope: regionScatterName})
	return engineContext{
		filters:        filters,
		selectedPeer:   newSelectedStores(true),
		selectedLeader: newSelectedStores(false),
	}
}

// Scatter relocates the region. If the group is defined, the regions's leader with the same group would be scattered
// in a group level instead of cluster level.
func (r *RegionScatterer) Scatter(region *core.RegionInfo, group string) (*operator.Operator, error) {
	if !opt.IsRegionReplicated(r.cluster, region) {
		return nil, errors.Errorf("region %d is not fully replicated", region.GetID())
	}

	if region.GetLeader() == nil {
		return nil, errors.Errorf("region %d has no leader", region.GetID())
	}

	return r.scatterRegion(region, group), nil
}

func (r *RegionScatterer) scatterRegion(region *core.RegionInfo, group string) *operator.Operator {
	ordinaryFilter := filter.NewOrdinaryEngineFilter(r.name)
	var ordinaryPeers []*metapb.Peer
	specialPeers := make(map[string][]*metapb.Peer)
	// Group peers by the engine of their stores
	for _, peer := range region.GetPeers() {
		store := r.cluster.GetStore(peer.GetStoreId())
		if ordinaryFilter.Target(r.cluster, store) {
			ordinaryPeers = append(ordinaryPeers, peer)
		} else {
			engine := store.GetLabelValue(filter.EngineKey)
			specialPeers[engine] = append(specialPeers[engine], peer)
		}
	}

	targetPeers := make(map[uint64]*metapb.Peer)

	scatterWithSameEngine := func(peers []*metapb.Peer, context engineContext) {
		stores := r.collectAvailableStores(group, region, context)
		for _, peer := range peers {
			if len(stores) == 0 {
				context.selectedPeer.reset()
				stores = r.collectAvailableStores(group, region, context)
			}
			if context.selectedPeer.put(peer.GetStoreId(), group) {
				delete(stores, peer.GetStoreId())
				targetPeers[peer.GetStoreId()] = peer
				continue
			}
			newPeer := r.selectPeerToReplace(group, stores, region, peer, context)
			if newPeer == nil {
				targetPeers[peer.GetStoreId()] = peer
				continue
			}
			// Remove it from stores and mark it as selected.
			delete(stores, newPeer.GetStoreId())
			context.selectedPeer.put(newPeer.GetStoreId(), group)
			targetPeers[newPeer.GetStoreId()] = newPeer
		}
	}

	scatterWithSameEngine(ordinaryPeers, r.ordinaryEngine)
	// FIXME: target leader only considers the ordinary stores，maybe we need to consider the
	// special engine stores if the engine supports to become a leader. But now there is only
	// one engine, tiflash, which does not support the leader, so don't consider it for now.
	targetLeader := r.selectAvailableLeaderStores(group, targetPeers, r.ordinaryEngine)

	for engine, peers := range specialPeers {
		context, ok := r.specialEngines[engine]
		if !ok {
			context = newEngineContext(filter.NewEngineFilter(r.name, engine))
			r.specialEngines[engine] = context
		}
		scatterWithSameEngine(peers, context)
	}

	op, err := operator.CreateScatterRegionOperator("scatter-region", r.cluster, region, targetPeers, targetLeader)
	if err != nil {
		log.Debug("fail to create scatter region operator", errs.ZapError(err))
		return nil
	}
	op.SetPriorityLevel(core.HighPriority)
	return op
}

func (r *RegionScatterer) selectPeerToReplace(group string, stores map[uint64]*core.StoreInfo, region *core.RegionInfo, oldPeer *metapb.Peer, context engineContext) *metapb.Peer {
	// scoreGuard guarantees that the distinct score will not decrease.
	regionStores := r.cluster.GetRegionStores(region)
	storeID := oldPeer.GetStoreId()
	sourceStore := r.cluster.GetStore(storeID)
	if sourceStore == nil {
		log.Error("failed to get the store", zap.Uint64("store-id", storeID), errs.ZapError(errs.ErrGetSourceStore))
		return nil
	}
	var scoreGuard filter.Filter
	if r.cluster.IsPlacementRulesEnabled() {
		scoreGuard = filter.NewRuleFitFilter(r.name, r.cluster, region, oldPeer.GetStoreId())
	} else {
		scoreGuard = filter.NewDistinctScoreFilter(r.name, r.cluster.GetLocationLabels(), regionStores, sourceStore)
	}

	candidates := make([]*core.StoreInfo, 0, len(stores))
	for _, store := range stores {
		if !scoreGuard.Target(r.cluster, store) {
			continue
		}
		candidates = append(candidates, store)
	}

	if len(candidates) == 0 {
		return nil
	}

	minPeer := uint64(math.MaxUint64)
	var selectedCandidateID uint64
	for _, candidate := range candidates {
		count := context.selectedPeer.get(candidate.GetID(), group)
		if count < minPeer {
			minPeer = count
			selectedCandidateID = candidate.GetID()
		}
	}
	if selectedCandidateID < 1 {
		target := candidates[rand.Intn(len(candidates))]
		return &metapb.Peer{
			StoreId:   target.GetID(),
			IsLearner: oldPeer.GetIsLearner(),
		}
	}

	return &metapb.Peer{
		StoreId:   selectedCandidateID,
		IsLearner: oldPeer.GetIsLearner(),
	}
}

func (r *RegionScatterer) collectAvailableStores(group string, region *core.RegionInfo, context engineContext) map[uint64]*core.StoreInfo {
	filters := []filter.Filter{
		filter.NewExcludedFilter(r.name, nil, region.GetStoreIds()),
		filter.StoreStateFilter{ActionScope: r.name, MoveRegion: true},
	}
	filters = append(filters, context.filters...)
	filters = append(filters, context.selectedPeer.newFilters(r.name, group)...)

	stores := r.cluster.GetStores()
	targets := make(map[uint64]*core.StoreInfo, len(stores))
	for _, store := range stores {
		if filter.Target(r.cluster, store, filters) && !store.IsBusy() {
			targets[store.GetID()] = store
		}
	}
	return targets
}

// selectAvailableLeaderStores select the target leader store from the candidates. The candidates would be collected by
// the existed peers store depended on the leader counts in the group level.
func (r *RegionScatterer) selectAvailableLeaderStores(group string, peers map[uint64]*metapb.Peer, context engineContext) uint64 {
	minStoreGroupLeader := uint64(math.MaxUint64)
	id := uint64(0)
	for storeID := range peers {
		storeGroupLeaderCount := context.selectedLeader.get(storeID, group)
		if minStoreGroupLeader > storeGroupLeaderCount {
			minStoreGroupLeader = storeGroupLeaderCount
			id = storeID
		}
	}
	if id != 0 {
		context.selectedLeader.put(id, group)
	}
	return id
}
