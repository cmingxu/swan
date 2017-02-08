package config

import (
	"errors"
	"os"
	"strings"
	"time"

	"github.com/Dataman-Cloud/swan/src/utils"
	"github.com/urfave/cli"
)

type SwanConfig struct {
	NodeID            string   `json:"nodeID"`
	LogLevel          string   `json:"logLevel"`
	Mode              SwanMode `json:"mode"` // manager, agent, mixed
	DataDir           string   `json:"dataDir"`
	Domain            string   `json:"domain"`
	RaftAdvertiseAddr string   `json:"raftAdvertiseAddr"`
	RaftListenAddr    string   `json:"raftListenAddr"`
	ListenAddr        string   `json:"listenAddr"`
	AdvertiseAddr     string   `json:"advertiseAddr"`
	JoinAddrs         []string `json:"joinAddrs"`

	Scheduler Scheduler `json:"scheduler"`

	DNS     DNS     `json:"dns"`
	Janitor Janitor `json:"janitor"`
}

type Scheduler struct {
	ZkPath             string `json:"zkpath"`
	MesosFrameworkUser string `json:"mesos-framwork-user"`
	Hostname           string `json:"hostname"`
}

type DNS struct {
	Domain    string `json:"domain"`
	RecurseOn bool   `json:"recurse_on"`
	IP        string `json:"ip"`
	Port      int    `json:"port"`

	SOARname   string `json:"soarname"`
	SOAMname   string `json:"soamname"`
	SOASerial  uint32 `json:"soaserial"`
	SOARefresh uint32 `json:"soarefresh"`
	SOARetry   uint32 `json:"soaretry"`
	SOAExpire  uint32 `json:"soaexpire"`

	TTL int `json:"ttl"`

	Resolvers       []string      `json:"resolvers"`
	ExchangeTimeout time.Duration `json:"exchange_timeout"`
}

type Janitor struct {
	IP          string `json:"ip"`
	Port        int    `json:"port"`
	Domain      string `json:"domain"`
	AdvertiseIP string `json:"advertiseIp"`
}

func NewConfig(c *cli.Context) (SwanConfig, error) {
	swanConfig := SwanConfig{
		LogLevel:       "info",
		Mode:           Mixed,
		DataDir:        "./data/",
		Domain:         "swan.com",
		ListenAddr:     "0.0.0.0:9999",
		RaftListenAddr: "0.0.0.0:2111",
		JoinAddrs:      []string{"0.0.0.0:9999"},

		Scheduler: Scheduler{
			ZkPath:             "zk://0.0.0.0:2181",
			MesosFrameworkUser: "root",
			Hostname:           hostname(),
		},

		DNS: DNS{
			Domain: "swan.com",
			IP:     "0.0.0.0",
			Port:   53,

			RecurseOn:       true,
			TTL:             3,
			Resolvers:       []string{"114.114.114.114"},
			ExchangeTimeout: time.Second * 3,
		},

		Janitor: Janitor{
			IP:          "0.0.0.0",
			Port:        80,
			AdvertiseIP: "0.0.0.0",
			Domain:      "swan.com",
		},
	}

	if c.String("log-level") != "" {
		swanConfig.LogLevel = c.String("log-level")
	}

	if c.String("mode") != "" {
		if utils.SliceContains([]string{"mixed", "manager", "agent"}, c.String("mode")) {
			swanConfig.Mode = SwanMode(c.String("mode"))
		} else {
			return swanConfig, errors.New("mode should be one of mixed, manager or agent")
		}
	}

	if c.String("data-dir") != "" {
		swanConfig.DataDir = c.String("data-dir")
		if !strings.HasSuffix(swanConfig.DataDir, "/") {
			swanConfig.DataDir = swanConfig.DataDir + "/"
		}
	}

	if c.String("zk-path") != "" {
		swanConfig.Scheduler.ZkPath = c.String("zk-path")
	}

	if c.String("domain") != "" {
		swanConfig.Domain = c.String("domain")
		swanConfig.DNS.Domain = c.String("domain")
		swanConfig.Janitor.Domain = c.String("domain")
	}

	// TODO(upccup): this is not the optimal solution. Maybe we can use listen-addr replace --swan-cluster
	// if swan mode is mixed agent listen addr use the same with manager. But if the mode is agent we need
	// a listen-addr just for agent
	if swanConfig.Mode == Agent {
		swanConfig.ListenAddr = c.String("listen-addr")
	}

	if c.String("listen-addr") != "" {
		swanConfig.ListenAddr = c.String("listen-addr")
	}

	swanConfig.AdvertiseAddr = c.String("advertise-addr")
	if swanConfig.AdvertiseAddr == "" {
		swanConfig.AdvertiseAddr = swanConfig.ListenAddr
	}

	if c.String("janitor-advertise-ip") != "" {
		swanConfig.Janitor.AdvertiseIP = c.String("janitor-advertise-ip")
	}

	if c.String("raft-advertise-addr") != "" {
		swanConfig.RaftAdvertiseAddr = c.String("raft-advertise-addr")
	}

	if c.String("raft-listen-addr") != "" {
		swanConfig.RaftListenAddr = c.String("raft-listen-addr")
	}

	if swanConfig.RaftAdvertiseAddr == "" {
		swanConfig.RaftAdvertiseAddr = swanConfig.RaftListenAddr
	}

	if c.String("join-addrs") != "" {
		swanConfig.JoinAddrs = strings.Split(c.String("join-addrs"), ",")
	}

	return swanConfig, nil
}

func hostname() string {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "UNKNOWN"
	}

	return hostname
}
