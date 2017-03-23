package manager

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Dataman-Cloud/swan/src/apiserver"
	"github.com/Dataman-Cloud/swan/src/config"
	log "github.com/Dataman-Cloud/swan/src/context_logger"
	"github.com/Dataman-Cloud/swan/src/event"
	eventbus "github.com/Dataman-Cloud/swan/src/event"
	"github.com/Dataman-Cloud/swan/src/manager/framework"
	frameworkStore "github.com/Dataman-Cloud/swan/src/manager/framework/store"
	"github.com/Dataman-Cloud/swan/src/manager/raft"
	raftstore "github.com/Dataman-Cloud/swan/src/manager/raft/store"
	"github.com/Dataman-Cloud/swan/src/types"

	"github.com/Sirupsen/logrus"
	"github.com/boltdb/bolt"
	"github.com/coreos/etcd/pkg/fileutil"
	events "github.com/docker/go-events"
	"golang.org/x/net/context"
)

type Manager struct {
	raftNode   *raft.Node
	CancelFunc context.CancelFunc

	framework *framework.Framework

	apiServer *apiserver.ApiServer

	criticalErrorChan chan error

	janitorListener  *event.JanitorListener
	resolverListener *event.DNSListener

	NodeInfo types.Node

	JoinAddrs []string
}

func New(nodeID string, managerConf config.ManagerConfig) (*Manager, error) {
	nodeDataDir := filepath.Join(managerConf.DataDir, nodeID)
	boltDbPath := filepath.Join(nodeDataDir, "swan.db")

	var nodeInfo types.Node
	var raftStore *raftstore.BoltbDb
	var err error

	if fileutil.Exist(boltDbPath) {
		nodeInfo, raftStore, err = recoverNodeData(nodeID, boltDbPath)
	} else {
		if err := os.MkdirAll(nodeDataDir, 0700); err != nil {
			return nil, err
		}

		nodeInfo, raftStore, err = initNodeData(nodeID, boltDbPath, managerConf)
	}

	if err != nil {
		return nil, err
	}

	raftNodeOpts := raft.NodeOptions{
		RaftID:        nodeInfo.RaftID,
		SwanNodeID:    nodeID,
		DataDir:       nodeDataDir,
		ListenAddr:    nodeInfo.RaftListenAddr,
		AdvertiseAddr: nodeInfo.RaftAdvertiseAddr,
		DB:            raftStore,
	}
	raftNode, err := raft.NewNode(raftNodeOpts)
	if err != nil {
		logrus.Errorf("init raft node failed. Error: %s", err.Error())
		return nil, err
	}

	managerServer := apiserver.NewApiServer(managerConf.ListenAddr, managerConf.AdvertiseAddr)

	framework, err := framework.New(frameworkStore.NewStore(raftStore.DB, raftNode), managerServer)
	if err != nil {
		logrus.Errorf("init framework failed. Error: ", err.Error())
		return nil, err
	}

	manager := &Manager{
		raftNode:          raftNode,
		framework:         framework,
		resolverListener:  event.NewDNSListener(),
		janitorListener:   event.NewJanitorListener(),
		NodeInfo:          nodeInfo,
		apiServer:         managerServer,
		JoinAddrs:         managerConf.JoinAddrs,
		criticalErrorChan: make(chan error, 1),
	}

	managerApi := &ManagerApi{manager}
	apiserver.Install(managerServer, managerApi)

	return manager, nil
}

func recoverNodeData(nodeID string, boltDbPath string) (types.Node, *raftstore.BoltbDb, error) {
	var nodeInfo types.Node
	db, err := bolt.Open(boltDbPath, 0644, nil)
	if err != nil {
		return nodeInfo, nil, err
	}

	boltDb := raftstore.NewBoltbdStore(db)

	nodeMetadata, err := boltDb.GetNode(nodeID)
	if err != nil {
		return nodeInfo, nil, err
	}

	nodeInfo = RaftTypeNodeToNode(*nodeMetadata)

	return nodeInfo, boltDb, nil
}

func initNodeData(nodeID string, boltDbPath string, managerConf config.ManagerConfig) (types.Node, *raftstore.BoltbDb, error) {
	var nodeInfo types.Node
	boltDb, err := bolt.Open(boltDbPath, 0644, nil)
	if err != nil {
		return nodeInfo, nil, err
	}

	nodeInfo = types.Node{
		ID:                nodeID,
		ListenAddr:        managerConf.ListenAddr,
		AdvertiseAddr:     managerConf.AdvertiseAddr,
		RaftListenAddr:    managerConf.RaftListenAddr,
		RaftAdvertiseAddr: managerConf.RaftAdvertiseAddr,
		Role:              types.RoleManager,
		RaftID:            uint64(rand.Int63()) + 1,
	}

	boltDbStore := raftstore.NewBoltbdStore(boltDb)

	return nodeInfo, boltDbStore, nil
}

func (manager *Manager) Stop() {
	manager.CancelFunc()

	return
}

func (manager *Manager) JoinAndStart(ctx context.Context) error {
	managers := manager.GetManagers()
	if len(managers) != 0 {
		return manager.start(ctx, managers, false)
	}

	managers, err := manager.JoinToCluster(manager.NodeInfo)
	if err != nil {
		return err
	}

	return manager.start(ctx, managers, false)
}

func (manager *Manager) InitAndStart(ctx context.Context) error {
	managers := manager.GetManagers()
	if len(managers) != 0 {
		return manager.start(ctx, managers, true)
	}

	managers = append(managers, manager.NodeInfo)
	return manager.start(ctx, managers, true)
}

func (manager *Manager) start(ctx context.Context, raftPeers []types.Node, isNewCluster bool) error {
	if err := manager.LoadNodeData(); err != nil {
		return err
	}

	// when follower => leader or leader => follower
	leadershipCh, cancel := manager.raftNode.SubscribeLeadership()
	defer cancel()

	leadershipChangeEventCtx, _ := context.WithCancel(ctx)
	go manager.handleLeadershipEvents(leadershipChangeEventCtx, leadershipCh)

	leaderChangeCh, cancel := manager.raftNode.SubcribeLeaderChange()
	defer cancel()

	// when new leader was elected within cluster
	leaderChangeEventCtx, _ := context.WithCancel(ctx)
	go manager.handleLeaderChangeEvents(leaderChangeEventCtx, leaderChangeCh)

	raftCtx, _ := context.WithCancel(ctx)
	if err := manager.raftNode.StartRaft(raftCtx, raftPeers, isNewCluster); err != nil {
		return err
	}

	// NOTICE: although WaitForLeader is returned, if call propose value as soon
	// there maybe return error: node losts leader status.
	// we should do propseValue in the handleLeadershipEvents go become leader event
	if err := manager.raftNode.WaitForLeader(ctx); err != nil {
		return err
	}

	go func() {
		manager.criticalErrorChan <- manager.apiServer.Start()
	}()

	for {
		select {
		case err := <-manager.criticalErrorChan:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (manager *Manager) handleLeadershipEvents(ctx context.Context, leadershipCh chan events.Event) {
	var eventBusStarted, frameworkStarted bool
	var once sync.Once
	for {
		select {
		case leadershipEvent := <-leadershipCh:
			// TODO lock it and if manager stop return
			newState := leadershipEvent.(raft.LeadershipState)

			ctx = log.WithLogger(ctx, logrus.WithField("raft_id", fmt.Sprintf("%x", manager.raftNode.Config.ID)))
			if newState == raft.IsLeader {
				log.G(ctx).Info("Now i become a leader !!!")

				once.Do(func() {
					// at the first time node become leader add itself to store.
					// this is use for ensure the first node of cluster can be add to store.
					if err := manager.presistNodeData(manager.NodeInfo); err != nil {
						manager.criticalErrorChan <- err
					}
				})

				eventBusCtx, _ := context.WithCancel(ctx)
				go func() {
					log.G(eventBusCtx).Info("starting eventBus in leader.")

					eventBusStarted = true
					eventbus.Start(ctx)

					eventbus.AddListener(manager.resolverListener)
					eventbus.AddListener(manager.janitorListener)
				}()

				frameworkCtx, _ := context.WithCancel(ctx)
				go func() {
					log.G(frameworkCtx).Info("starting framework in leader.")

					frameworkStarted = true
					manager.criticalErrorChan <- manager.framework.Start(frameworkCtx)
				}()

			} else if newState == raft.IsFollower {
				log.G(ctx).Info("now i become a follower !!!")

				if eventBusStarted {
					eventbus.RemoveListener(manager.resolverListener)
					eventbus.RemoveListener(manager.janitorListener)
					eventbus.Stop()
					eventBusStarted = false

					log.G(ctx).Info("eventBus has been stopped")

				}

				if frameworkStarted {
					log.G(ctx).Info("framework has been stopped")

					manager.framework.Stop()
					frameworkStarted = false
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

func (manager *Manager) handleLeaderChangeEvents(ctx context.Context, leaderChangeCh chan events.Event) {
	for {
		select {
		case leaderChangeEvent := <-leaderChangeCh:
			leaderRaftID := leaderChangeEvent.(uint64)

			manager.updateLeaderAddr(leaderRaftID)
		case <-ctx.Done():
			return
		}
	}
}

func (manager *Manager) updateLeaderAddr(leaderRaftID uint64) {
	var leaderAddr string

	// beacuse of the leader change event maybe published before all data has been sync.
	// maybe we can't find the leader node at first time, so need this loop for retry.
	// maintain a cluster membership maybe a better way like swarmkit:
	// https://github.com/docker/swarmkit/blob/master/manager/state/raft/membership/cluster.go
	for {
		// If leader was losted, this value is 0
		if int(leaderRaftID) == 0 {
			leaderAddr = ""
		} else {
			leaderNode, err := manager.findLeaderByRaftID(leaderRaftID)
			if err != nil {
				log.L.Warnf("update leaderAddr failed. Error: %s", err.Error())
				leaderAddr = ""
			} else {
				leaderAddr = leaderNode.AdvertiseAddr
			}

			manager.apiServer.UpdateLeaderAddr(leaderAddr)
			log.L.Infof("Now leader is change to %x, leader advertise-addr: %s", leaderRaftID, leaderAddr)
		}

		// if leader was losted or the leader addresss was found return
		if int(leaderRaftID) == 0 || leaderAddr != "" {
			return
		}

		time.Sleep(time.Second)
	}
}
