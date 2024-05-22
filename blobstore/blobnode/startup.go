// Copyright 2022 The CubeFS Authors.
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

package blobnode

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	bnapi "github.com/cubefs/cubefs/blobstore/api/blobnode"
	cmapi "github.com/cubefs/cubefs/blobstore/api/clustermgr"
	"github.com/cubefs/cubefs/blobstore/blobnode/base/flow"
	"github.com/cubefs/cubefs/blobstore/blobnode/core"
	"github.com/cubefs/cubefs/blobstore/blobnode/core/disk"
	myos "github.com/cubefs/cubefs/blobstore/blobnode/sys"
	"github.com/cubefs/cubefs/blobstore/common/config"
	"github.com/cubefs/cubefs/blobstore/common/diskutil"
	bloberr "github.com/cubefs/cubefs/blobstore/common/errors"
	"github.com/cubefs/cubefs/blobstore/common/proto"
	"github.com/cubefs/cubefs/blobstore/common/rpc"
	"github.com/cubefs/cubefs/blobstore/common/taskswitch"
	"github.com/cubefs/cubefs/blobstore/common/trace"
	"github.com/cubefs/cubefs/blobstore/util/errors"
	"github.com/cubefs/cubefs/blobstore/util/limit/keycount"
	"github.com/cubefs/cubefs/blobstore/util/log"
)

const (
	TickInterval   = 1
	HeartbeatTicks = 30
	ExpiresTicks   = 60
	LostDiskCount  = 3
)

var diskHealthMetric = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Namespace: "blobstore",
		Subsystem: "blobnode",
		Name:      "lost_disk",
		Help:      "blobnode lost disk",
	},
	[]string{"cluster_id", "idc", "rack", "host", "disk"},
)

func init() {
	prometheus.MustRegister(diskHealthMetric)
}

func readFormatInfo(ctx context.Context, diskRootPath string) (
	formatInfo *core.FormatInfo, err error,
) {
	span := trace.SpanFromContextSafe(ctx)
	_, err = os.ReadDir(diskRootPath)
	if err != nil {
		span.Errorf("read disk root path error:%s", diskRootPath)
		return nil, err
	}
	formatInfo, err = core.ReadFormatInfo(ctx, diskRootPath)
	if err != nil {
		if os.IsNotExist(err) {
			span.Warnf("format file not exist. must be first register")
			return new(core.FormatInfo), nil
		}
		return nil, err
	}

	return formatInfo, err
}

func findDisk(disks []*bnapi.DiskInfo, clusterID proto.ClusterID, diskID proto.DiskID) (
	*bnapi.DiskInfo, bool) {
	for _, d := range disks {
		if d.ClusterID == clusterID && d.DiskID == diskID {
			return d, true
		}
	}
	return nil, false
}

func isAllInConfig(ctx context.Context, registeredDisks []*bnapi.DiskInfo, conf *Config) bool {
	span := trace.SpanFromContextSafe(ctx)
	configDiskMap := make(map[string]struct{})
	for i := range conf.Disks {
		configDiskMap[conf.Disks[i].Path] = struct{}{}
	}
	// check all registered normal disks are in config
	for _, registeredDisk := range registeredDisks {
		if registeredDisk.Status != proto.DiskStatusNormal {
			continue
		}
		if _, ok := configDiskMap[registeredDisk.Path]; !ok {
			span.Errorf("disk registered to clustermgr, but is not in config: %v", registeredDisk.Path)
			return false
		}
	}
	return true
}

// call by heartbeat single, or datafile read/write concurrence
func (s *Service) handleDiskIOError(ctx context.Context, diskID proto.DiskID, diskErr error) {
	span := trace.SpanFromContextSafe(ctx)

	span.Debugf("diskID:%d diskErr: %v", diskID, diskErr)

	// first: set disk broken in memory
	s.lock.RLock()
	ds, exist := s.Disks[diskID]
	s.lock.RUnlock()
	if !exist {
		span.Errorf("such diskID(%d) does exist", diskID)
		return
	}

	if diskutil.IsLostDisk(ds.DiskInfo().Path) {
		lostCnt := 1
		diskStorages := s.copyDiskStorages(ctx)
		for _, dsAPI := range diskStorages {
			if dsAPI.ID() == diskID {
				continue
			}
			if diskutil.IsLostDisk(dsAPI.DiskInfo().Path) {
				lostCnt++
				span.Errorf("open diskId: %v, path: %v, disk lost", dsAPI.ID(), dsAPI.DiskInfo().Path)
				s.reportLostDisk(&s.Conf.HostInfo, dsAPI.DiskInfo().Path) // runtime check
			}
		}
		if lostCnt >= LostDiskCount {
			log.Fatalf("lost disk count:%d over threshold:%d", lostCnt, LostDiskCount)
		}
	}

	ds.SetStatus(proto.DiskStatusBroken)

	// May be used by callback func, when concurrently read/write shard in datafile.go. so limit do once
	_, _, shared := s.groupRun.Do(fmt.Sprintf("diskID:%d", diskID), func() (interface{}, error) {
		// second: notify cluster mgr
		for {
			err := s.ClusterMgrClient.SetDisk(ctx, diskID, proto.DiskStatusBroken)
			// error is nil or already broken status
			if err == nil || rpc.DetectStatusCode(err) == bloberr.CodeChangeDiskStatusNotAllow {
				span.Infof("set disk(%d) broken success, err:%v", diskID, err)
				break
			}
			span.Errorf("set disk(%d) broken failed: %v", diskID, err)
			time.Sleep(3 * time.Second)
		}

		// After the repair is triggered, the handle can be safely removed
		go s.waitRepairAndClose(ctx, ds)

		// we already tell cm this disk is bad
		dsInfo := ds.DiskInfo()
		s.reportOnlineDisk(&core.HostInfo{
			ClusterID: dsInfo.ClusterID,
			IDC:       dsInfo.Idc,
			Rack:      dsInfo.Rack,
			Host:      dsInfo.Host,
		}, dsInfo.Path)

		return nil, nil
	})

	span.Debugf("diskID:%d diskErr: %v, shared:%v", diskID, diskErr, shared)
}

func (s *Service) waitRepairAndClose(ctx context.Context, disk core.DiskAPI) {
	span := trace.SpanFromContextSafe(ctx)

	ticker := time.NewTicker(time.Duration(s.Conf.DiskStatusCheckIntervalSec) * time.Second)
	defer ticker.Stop()

	diskID := disk.ID()
	for {
		select {
		case <-s.closeCh:
			span.Warnf("service is closed. return")
			return
		case <-ticker.C:
		}

		info, err := s.ClusterMgrClient.DiskInfo(ctx, diskID)
		if err != nil {
			span.Errorf("Failed get clustermgr diskinfo %d, err:%v", diskID, err)
			continue
		}

		if info.Status >= proto.DiskStatusRepairing {
			span.Infof("disk:%d path:%s status:%v", diskID, info.Path, info.Status)
			break
		}
	}

	// after the repair is triggered, the handle can be safely removed
	span.Infof("Delete %d from the map table of the service", diskID)

	s.lock.Lock()
	delete(s.Disks, disk.ID())
	s.lock.Unlock()

	disk.ResetChunks(ctx)

	span.Infof("disk %d will gc close", diskID)
}

func (s *Service) handleDiskDrop(ctx context.Context, ds core.DiskAPI) {
	diskID := ds.ID()
	span := trace.SpanFromContextSafe(ctx)
	span.Debugf("diskID:%d dropped, start check clean", diskID)

	// 1. set disk dropped in memory
	ds.SetStatus(proto.DiskStatusDropped)

	// 2. check all chunks is clean: chunk handler in memory, physics chunk files
	go func() {
		ticker := time.NewTicker(time.Duration(s.Conf.DiskStatusCheckIntervalSec) * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-s.closeCh:
				span.Warn("service is closed. skip check disk drop")
				return
			case <-ticker.C:
			}

			if ds.IsCleanUp(ctx) {
				break // is clean, need to delete disk handler
			}
			// not clean, wait it, next check
		}

		// 3. physics chunks is already empty, destroy disk/chunks handlers
		span.Infof("diskID:%d dropped, will gc destroy resource", diskID)
		s.lock.Lock()
		delete(s.Disks, diskID)
		s.lock.Unlock()

		span.Debugf("diskID:%d dropped, end check clean", diskID)
	}()
}

func setDefaultIOStat(dryRun bool) error {
	ios, err := flow.NewIOFlowStat("default", dryRun)
	if err != nil {
		return errors.New("init stat failed")
	}
	flow.SetupDefaultIOStat(ios)
	return nil
}

func (s *Service) fixDiskConf(config *core.Config) {
	config.AllocDiskID = s.ClusterMgrClient.AllocDiskID
	config.NotifyCompacting = s.ClusterMgrClient.SetCompactChunk
	config.HandleIOError = s.handleDiskIOError

	// init configs
	config.RuntimeConfig = s.Conf.DiskConfig
	// init hostInfo
	config.HostInfo = s.Conf.HostInfo
	// init metaInfo
	config.MetaConfig = s.Conf.MetaConfig
}

func NewService(conf Config) (svr *Service, err error) {
	span, ctx := trace.StartSpanFromContext(context.Background(), "NewBlobNodeService")

	configInit(&conf)

	clusterMgrCli := cmapi.New(conf.Clustermgr)
	node := cmapi.ServiceNode{
		ClusterID: uint64(conf.ClusterID),
		Name:      proto.ServiceNameBlobNode,
		Host:      conf.Host,
		Idc:       conf.IDC,
	}
	err = clusterMgrCli.RegisterService(ctx, node, TickInterval, HeartbeatTicks, ExpiresTicks)
	if err != nil {
		span.Fatalf("blobnode register to clusterMgr error:%v", err)
	}

	if err = registerNode(ctx, clusterMgrCli, &conf); err != nil {
		span.Fatalf("fail to register node to clusterMgr, err:%+v", err)
		return nil, err
	}

	registeredDisks, err := clusterMgrCli.ListHostDisk(ctx, conf.Host)
	if err != nil {
		span.Errorf("Failed ListDisk from clusterMgr. err:%v", err)
		return nil, err
	}
	span.Infof("registered disks: %v", registeredDisks)

	check := isAllInConfig(ctx, registeredDisks, &conf)
	if !check {
		span.Errorf("no all registered normal disk in config")
		return nil, errors.New("registered disk not in config")
	}
	span.Infof("registered disks are all in config")

	svr = &Service{
		ClusterMgrClient: clusterMgrCli,
		Disks:            make(map[proto.DiskID]core.DiskAPI),
		Conf:             &conf,

		DeleteQpsLimitPerDisk: keycount.New(conf.DeleteQpsLimitPerDisk),
		DeleteQpsLimitPerKey:  keycount.NewBlockingKeyCountLimit(1),
		ChunkLimitPerVuid:     keycount.New(1),
		DiskLimitPerKey:       keycount.New(1),
		InspectLimiterPerKey:  keycount.New(1),

		closeCh: make(chan struct{}),
	}

	switchMgr := taskswitch.NewSwitchMgr(clusterMgrCli)
	svr.inspectMgr, err = NewDataInspectMgr(svr, conf.InspectConf, switchMgr)
	if err != nil {
		return nil, err
	}

	svr.ctx, svr.cancel = context.WithCancel(context.Background())

	wg := sync.WaitGroup{}
	errCh := make(chan error, len(conf.Disks))

	lostCnt := int32(0)
	for _, diskConf := range conf.Disks {
		wg.Add(1)

		go func(diskConf core.Config) {
			var err error
			defer func() {
				errCh <- err
				wg.Done()
			}()

			svr.fixDiskConf(&diskConf)

			if diskConf.MustMountPoint && !myos.IsMountPoint(diskConf.Path) {
				lost := atomic.AddInt32(&lostCnt, 1)
				svr.reportLostDisk(&diskConf.HostInfo, diskConf.Path) // startup check lost disk
				// skip
				span.Errorf("Path is not mount point:%s, err:%v. skip init", diskConf.Path, err)
				if lost >= LostDiskCount {
					log.Fatalf("lost disk count:%d over threshold:%d", lost, LostDiskCount)
				}
				return
			}
			// read disk meta. get DiskID
			format, err := readFormatInfo(ctx, diskConf.Path)
			if err != nil {
				// todo: report to ums
				span.Errorf("Failed read diskMeta:%s, err:%v. skip init", diskConf.Path, err)
				err = nil // skip
				return
			}

			span.Debugf("local disk meta: %v", format)

			// found diskInfo store in cluster mgr
			diskInfo, foundInCluster := findDisk(registeredDisks, conf.ClusterID, format.DiskID)
			span.Debugf("diskInfo: %v, foundInCluster:%v", diskInfo, foundInCluster)

			nonNormal := foundInCluster && diskInfo.Status != proto.DiskStatusNormal
			if nonNormal {
				// todo: report to ums
				span.Warnf("disk(%v):path(%v) is not normal, skip init", format.DiskID, diskConf.Path)
				return
			}

			ds, err := disk.NewDiskStorage(svr.ctx, diskConf)
			if err != nil {
				span.Errorf("Failed Open DiskStorage. conf:%v, err:%v", diskConf, err)
				return
			}

			if !foundInCluster {
				span.Warnf("diskInfo:%v not found in clusterMgr, will register to cluster", diskInfo)
				diskInfo := ds.DiskInfo() // get nodeID to add disk
				err := clusterMgrCli.AddDisk(ctx, &diskInfo)
				if err != nil {
					span.Errorf("Failed register disk: %v, err:%v", diskInfo, err)
					return
				}
			}

			svr.lock.Lock()
			svr.Disks[ds.DiskID] = ds
			svr.lock.Unlock()

			svr.reportOnlineDisk(&diskConf.HostInfo, diskConf.Path) // restart, normal disk
			span.Infof("Init disk storage, cluster:%v, diskID:%v", conf.ClusterID, format.DiskID)
		}(diskConf)
	}
	wg.Wait()

	close(errCh)
	for err := range errCh {
		if err != nil {
			return nil, err
		}
	}

	if err = setDefaultIOStat(conf.DiskConfig.IOStatFileDryRun); err != nil {
		span.Errorf("Failed set default iostat file, err:%v", err)
		return nil, err
	}

	callBackFn := func(conf []byte) error {
		_, ctx := trace.StartSpanFromContext(ctx, "")
		c := Config{}
		if err = config.LoadData(&c, conf); err != nil {
			log.Errorf("reload fail to load config, err: %v", err)
			return err
		}
		// limit
		svr.changeLimit(ctx, c)
		// qos
		err := svr.changeQos(ctx, c)
		return err
	}
	config.Register(callBackFn)

	svr.WorkerService, err = NewWorkerService(&conf.WorkerConfig, clusterMgrCli, conf.ClusterID, conf.IDC)
	if err != nil {
		span.Errorf("Failed to new worker service, err: %v", err)
		return
	}

	// background loop goroutines
	go svr.loopHeartbeatToClusterMgr()
	go svr.loopReportChunkInfoToClusterMgr()
	go svr.loopGcRubbishChunkFile()
	go svr.loopCleanExpiredStatFile()
	go svr.inspectMgr.loopDataInspect()

	return
}

// when find the lost disk, set value 1
func (s *Service) reportLostDisk(hostInfo *core.HostInfo, diskPath string) {
	diskHealthMetric.WithLabelValues(hostInfo.ClusterID.ToString(),
		hostInfo.IDC,
		hostInfo.Rack,
		hostInfo.Host,
		diskPath,
	).Set(1)
}

// 1. when startup ok, we set value 0(normal disk)
// 2. when the lost disk is report to cm(set broken), we know it is bad disk, set 0
func (s *Service) reportOnlineDisk(hostInfo *core.HostInfo, diskPath string) {
	diskHealthMetric.WithLabelValues(hostInfo.ClusterID.ToString(),
		hostInfo.IDC,
		hostInfo.Rack,
		hostInfo.Host,
		diskPath,
	).Set(0)
}

func registerNode(ctx context.Context, clusterMgrCli *cmapi.Client, conf *Config) error {
	if err := core.CheckNodeConf(&conf.HostInfo); err != nil {
		return err
	}

	nodeToCm := bnapi.NodeInfo{
		ClusterID: conf.ClusterID,
		DiskType:  conf.DiskType,
		Idc:       conf.IDC,
		Rack:      conf.Rack,
		Host:      conf.Host,
		Role:      proto.NodeRoleBlobNode,
	}

	nodeID, err := clusterMgrCli.AddNode(ctx, &nodeToCm)
	if err != nil && rpc.DetectStatusCode(err) != http.StatusCreated {
		return err
	}

	conf.NodeID = nodeID
	return nil
}
