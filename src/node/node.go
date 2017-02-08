package node

import (
	"encoding/json"
	"errors"
	"io/ioutil"
	"math/rand"
	"os"
	"strconv"
	"time"

	"github.com/Dataman-Cloud/swan/src/agent"
	"github.com/Dataman-Cloud/swan/src/apiserver"
	"github.com/Dataman-Cloud/swan/src/config"
	"github.com/Dataman-Cloud/swan/src/event"
	"github.com/Dataman-Cloud/swan/src/manager"
	"github.com/Dataman-Cloud/swan/src/swancontext"
	"github.com/Dataman-Cloud/swan/src/types"
	"github.com/Dataman-Cloud/swan/src/utils/httpclient"

	"github.com/Sirupsen/logrus"
	"github.com/boltdb/bolt"
	"github.com/coreos/etcd/pkg/fileutil"
	"github.com/twinj/uuid"
	"golang.org/x/net/context"
)

const (
	NodeIDFileName = "/ID"
)

type Node struct {
	ID                string
	agent             *agent.Agent     // hold reference to agent, take function when in agent mode
	manager           *manager.Manager // hold a instance of manager, make logic taking place
	ctx               context.Context
	joinRetryInterval time.Duration
	RaftID            uint64
}

func NewNode(config config.SwanConfig) (*Node, error) {
	nodeID, err := loadOrCreateNodeID(config)
	if err != nil {
		return nil, err
	}

	// init swanconfig instance
	config.NodeID = nodeID
	_ = swancontext.NewSwanContext(config, event.New())

	if !swancontext.IsManager() && !swancontext.IsAgent() {
		return nil, errors.New("node must be started with at least one role in [manager,agent]")
	}

	node := &Node{
		ID:                nodeID,
		joinRetryInterval: time.Second * 5,
	}

	err = os.MkdirAll(config.DataDir+"/"+nodeID, 0644)
	if err != nil {
		logrus.Errorf("os.MkdirAll got error: %s", err)
		return nil, err
	}

	db, err := bolt.Open(config.DataDir+"/"+nodeID+"/swan.db", 0644, nil)
	if err != nil {
		logrus.Errorf("Init bolt store failed:%s", err)
		return nil, err
	}

	raftID, err := loadOrCreateRaftID(db)
	if err != nil {
		logrus.Errorf("Init raft ID failed:%s", err)
		return nil, err
	}
	node.RaftID = raftID

	if swancontext.IsManager() {
		m, err := manager.New(db)
		if err != nil {
			return nil, err
		}
		node.manager = m
	}

	if swancontext.IsAgent() {
		a, err := agent.New()
		if err != nil {
			return nil, err
		}
		node.agent = a
	}

	nodeApi := &NodeApi{node}
	apiserver.Install(swancontext.Instance().ApiServer, nodeApi)

	return node, nil
}

func loadOrCreateNodeID(swanConfig config.SwanConfig) (string, error) {
	nodeIDFile := swanConfig.DataDir + NodeIDFileName
	if !fileutil.Exist(nodeIDFile) {
		err := os.MkdirAll(swanConfig.DataDir, 0755)
		if err != nil {
			return "", err
		}

		nodeID := uuid.NewV4().String()
		idFile, err := os.OpenFile(nodeIDFile, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
		if err != nil {
			return "", err
		}

		if _, err = idFile.WriteString(nodeID); err != nil {
			return "", err
		}

		logrus.Infof("starting swan node, ID file was not found started with  new ID: %s", nodeID)
		return nodeID, nil

	} else {
		idFile, err := os.Open(nodeIDFile)
		if err != nil {
			return "", err
		}

		nodeID, err := ioutil.ReadAll(idFile)
		if err != nil {
			return "", err
		}

		logrus.Infof("starting swan node, ID file was found started with ID: %s", string(nodeID))
		return string(nodeID), nil
	}
}

// node start from here
// - 1, start manager if needed
// - 2, start agent if needed
// - 3, agent join to managers if needed
// - 4, start the API server, both for agent and client
// - 5, enter loop, wait for error or ctx.Done
func (n *Node) Start(ctx context.Context) error {
	errChan := make(chan error, 1)

	swanConfig := swancontext.Instance().Config
	nodeInfo := types.Node{
		ID:                n.ID,
		AdvertiseAddr:     swanConfig.AdvertiseAddr,
		ListenAddr:        swanConfig.ListenAddr,
		RaftListenAddr:    swanConfig.RaftListenAddr,
		RaftAdvertiseAddr: swanConfig.RaftAdvertiseAddr,
		Role:              types.NodeRole(swanConfig.Mode),
		RaftID:            n.RaftID,
	}

	if swancontext.IsManager() {
		go func() {
			if swancontext.IsNewCluster() {
				errChan <- n.runManager(ctx, n.RaftID, []types.Node{nodeInfo}, true)
			} else {
				existedNodes, err := n.JoinAsManager(nodeInfo)
				if err != nil {
					errChan <- err
				}

				errChan <- n.runManager(ctx, n.RaftID, existedNodes, false)
			}
		}()
	}

	if swancontext.IsAgent() {
		go func() {
			errChan <- n.runAgent(ctx)
		}()

		go func() {
			err := n.JoinAsAgent(nodeInfo)
			if err != nil {
				errChan <- err
			}
		}()
	}

	go func() {
		errChan <- swancontext.Instance().ApiServer.Start()
	}()

	for {
		select {
		case err := <-errChan:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (n *Node) runAgent(ctx context.Context) error {
	agentCtx, cancel := context.WithCancel(ctx)
	n.agent.CancelFunc = cancel
	return n.agent.Start(agentCtx)
}

func (n *Node) runManager(ctx context.Context, raftID uint64, peers []types.Node, isNewCluster bool) error {
	managerCtx, cancel := context.WithCancel(ctx)
	n.manager.CancelFunc = cancel
	return n.manager.Start(managerCtx, raftID, peers, isNewCluster)
}

// node stop
func (n *Node) Stop() {
	n.agent.Stop()
	n.manager.Stop()
}

func (n *Node) JoinAsAgent(nodeInfo types.Node) error {
	swanConfig := swancontext.Instance().Config
	if len(swanConfig.JoinAddrs) == 0 {
		return errors.New("start agent failed. Error: joinAddrs must be no empty")
	}

	for _, managerAddr := range swanConfig.JoinAddrs {
		registerAddr := "http://" + managerAddr + config.API_PREFIX + "/nodes"
		_, err := httpclient.NewDefaultClient().POST(context.TODO(), registerAddr, nil, nodeInfo, nil)
		if err != nil {
			logrus.Errorf("register to %s got error: %s", registerAddr, err.Error())
		}

		if err == nil {
			logrus.Infof("agent register to manager success with managerAddr: %s", managerAddr)
			return nil
		}
	}

	time.Sleep(n.joinRetryInterval)
	n.JoinAsAgent(nodeInfo)
	return nil
}

func (n *Node) JoinAsManager(nodeInfo types.Node) ([]types.Node, error) {
	swanConfig := swancontext.Instance().Config
	if len(swanConfig.JoinAddrs) == 0 {
		return nil, errors.New("start agent failed. Error: joinAddrs must be no empty")
	}

	for _, managerAddr := range swanConfig.JoinAddrs {
		registerAddr := "http://" + managerAddr + config.API_PREFIX + "/nodes"
		resp, err := httpclient.NewDefaultClient().POST(context.TODO(), registerAddr, nil, nodeInfo, nil)
		if err != nil {
			logrus.Errorf("register to %s got error: %s", registerAddr, err.Error())
			continue
		}

		var nodes []types.Node
		if err := json.Unmarshal(resp, &nodes); err != nil {
			logrus.Errorf("register to %s got error: %s", registerAddr, err.Error())
			continue
		}

		var managerNodes []types.Node
		for _, existedNode := range nodes {
			if existedNode.IsManager() {
				managerNodes = append(managerNodes, existedNode)
			}
		}

		return managerNodes, nil
	}

	return nil, errors.New("add manager failed")
}

func loadOrCreateRaftID(db *bolt.DB) (uint64, error) {
	var raftID uint64
	tx, err := db.Begin(true)
	if err != nil {
		return raftID, err
	}
	defer tx.Commit()

	var (
		raftIDBukctName = []byte("raftnode")
		raftIDDataKey   = []byte("raftid")
	)
	raftIDBkt := tx.Bucket(raftIDBukctName)
	if raftIDBkt == nil {
		raftIDBkt, err = tx.CreateBucketIfNotExists(raftIDBukctName)
		if err != nil {
			return raftID, err
		}

		raftID = uint64(rand.Int63()) + 1
		if err := raftIDBkt.Put(raftIDDataKey, []byte(strconv.FormatUint(raftID, 10))); err != nil {
			return raftID, err
		}
		logrus.Infof("raft ID was not found create a new raftID %x", raftID)
		return raftID, nil
	} else {
		raftID_ := raftIDBkt.Get(raftIDDataKey)
		raftID, err = strconv.ParseUint(string(raftID_), 10, 64)
		if err != nil {
			return raftID, err
		}

		return raftID, nil
	}
}
