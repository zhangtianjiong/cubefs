// Copyright 2018 The CubeFS Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package master

import (
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/cubefs/cubefs/util/log"
)

func (c *Cluster) scheduleToLoadMetaPartitions() {
	go func() {
		for {
			if c.partition != nil && c.partition.IsRaftLeader() {
				if c.vols != nil {
					c.checkLoadMetaPartitions()
				}
			}
			time.Sleep(2 * time.Second * defaultIntervalToCheckDataPartition)
		}
	}()
}

func (c *Cluster) checkLoadMetaPartitions() {
	defer func() {
		if r := recover(); r != nil {
			log.LogWarnf("checkDiskRecoveryProgress occurred panic,err[%v]", r)
			WarnBySpecialKey(fmt.Sprintf("%v_%v_scheduling_job_panic", c.Name, ModuleName),
				"checkDiskRecoveryProgress occurred panic")
		}
	}()
	vols := c.allVols()
	for _, vol := range vols {
		mps := vol.cloneMetaPartitionMap()
		for _, mp := range mps {
			c.doLoadMetaPartition(mp)
		}
	}
}

func (mp *MetaPartition) checkSnapshot(clusterID string) {
	if len(mp.LoadResponse) == 0 {
		return
	}
	if !mp.doCompare() {
		return
	}
	if !mp.isSameApplyID() {
		return
	}
	mp.checkInodeCount(clusterID)
	mp.checkDentryCount(clusterID)
}

func (mp *MetaPartition) doCompare() bool {
	for _, lr := range mp.LoadResponse {
		if !lr.DoCompare {
			return false
		}
	}
	return true
}

func (mp *MetaPartition) isSameApplyID() bool {
	rst := true
	applyID := mp.LoadResponse[0].ApplyID
	for _, loadResponse := range mp.LoadResponse {
		if applyID != loadResponse.ApplyID {
			rst = false
		}
	}
	return rst
}

func (mp *MetaPartition) checkInodeCount(clusterID string) {
	isEqual := true
	maxInode := mp.LoadResponse[0].MaxInode
	maxInodeCount := mp.LoadResponse[0].InodeCount

	if mp.IsRecover {
		return
	}
	for _, loadResponse := range mp.LoadResponse {
		diff := math.Abs(float64(loadResponse.MaxInode) - float64(maxInode))
		if diff > defaultRangeOfCountDifferencesAllowed {
			isEqual = false
			break
		}
		diff = math.Abs(float64(loadResponse.InodeCount) - float64(maxInodeCount))
		if diff > defaultRangeOfCountDifferencesAllowed {
			isEqual = false
			break
		}
	}

	if !isEqual {
		msg := fmt.Sprintf("inode count is not equal,vol[%v],mpID[%v],", mp.volName, mp.PartitionID)
		for _, lr := range mp.LoadResponse {
			inodeMaxInodeStr := strconv.FormatUint(lr.MaxInode, 10)
			inodeMaxCountStr := strconv.FormatUint(lr.InodeCount, 10)
			applyIDStr := strconv.FormatUint(uint64(lr.ApplyID), 10)
			msg = msg + lr.Addr + " applyId[" + applyIDStr + "] maxInode[" + inodeMaxInodeStr + "] maxInodeCnt[" + inodeMaxCountStr + "],"
		}
		Warn(clusterID, msg)
		mp.EqualCheckPass = false
	}
}

func (mp *MetaPartition) checkDentryCount(clusterID string) {
	if mp.IsRecover {
		return
	}
	isEqual := true
	dentryCount := mp.LoadResponse[0].DentryCount
	for _, loadResponse := range mp.LoadResponse {
		diff := math.Abs(float64(loadResponse.DentryCount) - float64(dentryCount))
		if diff > defaultRangeOfCountDifferencesAllowed {
			isEqual = false
		}
	}

	if !isEqual {
		msg := fmt.Sprintf("dentry count is not equal,vol[%v],mpID[%v],", mp.volName, mp.PartitionID)
		for _, lr := range mp.LoadResponse {
			dentryCountStr := strconv.FormatUint(lr.DentryCount, 10)
			applyIDStr := strconv.FormatUint(uint64(lr.ApplyID), 10)
			msg = msg + lr.Addr + " applyId[" + applyIDStr + "] dentryCount[" + dentryCountStr + "],"
		}
		mp.EqualCheckPass = false
		Warn(clusterID, msg)
	}
}

func (c *Cluster) scheduleToCheckMetaPartitionRecoveryProgress() {
	go func() {
		for {
			if c.partition != nil && c.partition.IsRaftLeader() {
				if c.vols != nil {
					c.checkMetaPartitionRecoveryProgress()
				}
			}
			time.Sleep(time.Second * defaultIntervalToCheckDataPartition)
		}
	}()
}

func (c *Cluster) checkMetaPartitionRecoveryProgress() {
	defer func() {
		if r := recover(); r != nil {
			log.LogWarnf("checkMetaPartitionRecoveryProgress occurred panic,err[%v]", r)
			WarnBySpecialKey(fmt.Sprintf("%v_%v_scheduling_job_panic", c.Name, ModuleName),
				"checkMetaPartitionRecoveryProgress occurred panic")
		}
	}()

	c.badPartitionMutex.Lock()
	defer c.badPartitionMutex.Unlock()

	c.BadMetaPartitionIds.Range(func(key, value interface{}) bool {
		badMetaPartitionIds := value.([]uint64)
		newBadMpIds := make([]uint64, 0)
		for _, partitionID := range badMetaPartitionIds {
			partition, err := c.getMetaPartitionByID(partitionID)
			if err != nil {
				Warn(c.Name, fmt.Sprintf("checkMetaPartitionRecoveryProgress clusterID[%v], partitionID[%v] is not exist", c.Name, partitionID))
				continue
			}

			vol, err := c.getVol(partition.volName)
			if err != nil {
				Warn(c.Name, fmt.Sprintf("checkMetaPartitionRecoveryProgress clusterID[%v],vol[%v] partitionID[%v]is not exist",
					c.Name, partition.volName, partitionID))
				continue
			}

			if len(partition.Replicas) == 0 || len(partition.Replicas) < int(vol.mpReplicaNum) {
				newBadMpIds = append(newBadMpIds, partitionID)
				continue
			}

			if partition.getMinusOfMaxInodeID() < defaultMinusOfMaxInodeID {
				partition.IsRecover = false
				partition.RLock()
				c.syncUpdateMetaPartition(partition)
				partition.RUnlock()
				Warn(c.Name, fmt.Sprintf("checkMetaPartitionRecoveryProgress clusterID[%v],vol[%v] partitionID[%v] has recovered success",
					c.Name, partition.volName, partitionID))
			} else {
				newBadMpIds = append(newBadMpIds, partitionID)
			}
		}

		if len(newBadMpIds) == 0 {
			Warn(c.Name, fmt.Sprintf("checkMetaPartitionRecoveryProgress clusterID[%v],node[%v] has recovered success", c.Name, key))
			c.BadMetaPartitionIds.Delete(key)
		} else {
			c.BadMetaPartitionIds.Store(key, newBadMpIds)
			log.LogInfof("checkMetaPartitionRecoveryProgress BadMetaPartitionIds there is still (%d) mp in recover, addr (%s)", len(newBadMpIds), key)
		}

		return true
	})
}
