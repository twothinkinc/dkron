package dcron

import (
	"crypto/sha1"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/hashicorp/memberlist"
	"github.com/hashicorp/serf/serf"
	"github.com/mitchellh/cli"
	"github.com/spf13/viper"
)

const (
	QuerySchedulerRestart = "scheduler:restart"
	QueryRunJob           = "run:job"
	QueryExecutionDone    = "execution:done"

	// gracefulTimeout controls how long we wait before forcefully terminating
	gracefulTimeout = 3 * time.Second
)

// AgentCommand run dcron server
type AgentCommand struct {
	Ui         cli.Ui
	ShutdownCh <-chan struct{}
	serf       *serf.Serf
	config     *Config
	etcd       *etcdClient
	eventCh    chan serf.Event
	sched      *Scheduler
}

func (a *AgentCommand) Help() string {
	helpText := `
Usage: dcron server [options]
	Run dcron server
Options:
`
	return strings.TrimSpace(helpText)
}

// readConfig is responsible for setup of our configuration using
// the command line and any file configs
func (a *AgentCommand) readConfig(args []string) *Config {
	hostname, err := os.Hostname()
	if err != nil {
		log.Panic(err)
	}

	cmdFlags := flag.NewFlagSet("server", flag.ContinueOnError)
	cmdFlags.Usage = func() { a.Ui.Output(a.Help()) }
	cmdFlags.String("node", hostname, "node name")
	viper.SetDefault("node_name", cmdFlags.Lookup("node").Value)
	cmdFlags.String("bind", "0.0.0.0:7946", "address to bind listeners to")
	viper.SetDefault("bind_addr", cmdFlags.Lookup("bind").Value)
	cmdFlags.String("http-addr", ":8080", "HTTP address")
	viper.SetDefault("http_addr", cmdFlags.Lookup("http-addr").Value)
	cmdFlags.String("discover", "dcron", "mDNS discovery name")
	viper.SetDefault("discover", cmdFlags.Lookup("discover").Value)
	cmdFlags.String("etcd-machines", "127.0.0.1:2379", "etcd machines addresses")
	viper.SetDefault("etcd_machines", cmdFlags.Lookup("etcd-machines").Value)
	cmdFlags.String("profile", "lan", "timing profile to use (lan, wan, local)")
	viper.SetDefault("profile", cmdFlags.Lookup("profile").Value)
	viper.SetDefault("server", cmdFlags.Bool("server", false, "start dcron server"))

	startJoin := &AppendSliceValue{}
	cmdFlags.Var(startJoin, "join", "address of agent to join on startup")
	// viper.SetDefault("join", startJoin)

	if err := cmdFlags.Parse(args); err != nil {
		log.Fatal(err)
	}

	tags := viper.GetStringMapString("tags")
	server := viper.GetBool("server")
	nodeName := viper.GetString("node_name")

	if server {
		data := []byte(nodeName + fmt.Sprintf("%s", time.Now()))
		tags["key"] = fmt.Sprintf("%x", sha1.Sum(data))
		tags["server"] = "true"
	}

	config := &Config{
		NodeName:     nodeName,
		BindAddr:     viper.GetString("bind_addr"),
		HTTPAddr:     viper.GetString("http_addr"),
		Discover:     viper.GetString("discover"),
		EtcdMachines: viper.GetStringSlice("etcd_machines"),
		Server:       server,
		Profile:      viper.GetString("profile"),
		StartJoin:    *startJoin,
		Tags:         tags,
	}

	// log.Fatal(config.EtcdMachines)
	return config
}

// setupAgent is used to create the agent we use
func (a *AgentCommand) setupSerf(config *Config) *serf.Serf {
	bindIP, bindPort, err := config.AddrParts(config.BindAddr)
	if err != nil {
		a.Ui.Error(fmt.Sprintf("Invalid bind address: %s", err))
		return nil
	}

	// Check if we have an interface
	if iface, _ := config.NetworkInterface(); iface != nil {
		addrs, err := iface.Addrs()
		if err != nil {
			a.Ui.Error(fmt.Sprintf("Failed to get interface addresses: %s", err))
			return nil
		}
		if len(addrs) == 0 {
			a.Ui.Error(fmt.Sprintf("Interface '%s' has no addresses", config.Interface))
			return nil
		}

		// If there is no bind IP, pick an address
		if bindIP == "0.0.0.0" {
			found := false
			for _, ad := range addrs {
				var addrIP net.IP
				if runtime.GOOS == "windows" {
					// Waiting for https://github.com/golang/go/issues/5395 to use IPNet only
					addr, ok := ad.(*net.IPAddr)
					if !ok {
						continue
					}
					addrIP = addr.IP
				} else {
					addr, ok := ad.(*net.IPNet)
					if !ok {
						continue
					}
					addrIP = addr.IP
				}

				// Skip self-assigned IPs
				if addrIP.IsLinkLocalUnicast() {
					continue
				}

				// Found an IP
				found = true
				bindIP = addrIP.String()
				a.Ui.Output(fmt.Sprintf("Using interface '%s' address '%s'",
					config.Interface, bindIP))

				// Update the configuration
				bindAddr := &net.TCPAddr{
					IP:   net.ParseIP(bindIP),
					Port: bindPort,
				}
				config.BindAddr = bindAddr.String()
				break
			}
			if !found {
				a.Ui.Error(fmt.Sprintf("Failed to find usable address for interface '%s'", config.Interface))
				return nil
			}

		} else {
			// If there is a bind IP, ensure it is available
			found := false
			for _, a := range addrs {
				addr, ok := a.(*net.IPNet)
				if !ok {
					continue
				}
				if addr.IP.String() == bindIP {
					found = true
					break
				}
			}
			if !found {
				a.Ui.Error(fmt.Sprintf("Interface '%s' has no '%s' address",
					config.Interface, bindIP))
				return nil
			}
		}
	}

	var advertiseIP string
	var advertisePort int
	if config.AdvertiseAddr != "" {
		advertiseIP, advertisePort, err = config.AddrParts(config.AdvertiseAddr)
		if err != nil {
			a.Ui.Error(fmt.Sprintf("Invalid advertise address: %s", err))
			return nil
		}
	}

	encryptKey, err := config.EncryptBytes()
	if err != nil {
		a.Ui.Error(fmt.Sprintf("Invalid encryption key: %s", err))
		return nil
	}

	serfConfig := serf.DefaultConfig()
	switch config.Profile {
	case "lan":
		serfConfig.MemberlistConfig = memberlist.DefaultLANConfig()
	case "wan":
		serfConfig.MemberlistConfig = memberlist.DefaultWANConfig()
	case "local":
		serfConfig.MemberlistConfig = memberlist.DefaultLocalConfig()
	default:
		a.Ui.Error(fmt.Sprintf("Unknown profile: %s", config.Profile))
		return nil
	}

	serfConfig.MemberlistConfig.BindAddr = bindIP
	serfConfig.MemberlistConfig.BindPort = bindPort
	serfConfig.MemberlistConfig.AdvertiseAddr = advertiseIP
	serfConfig.MemberlistConfig.AdvertisePort = advertisePort
	serfConfig.MemberlistConfig.SecretKey = encryptKey
	serfConfig.NodeName = config.NodeName
	serfConfig.Tags = config.Tags
	serfConfig.SnapshotPath = config.SnapshotPath
	serfConfig.CoalescePeriod = 3 * time.Second
	serfConfig.QuiescentPeriod = time.Second
	serfConfig.UserCoalescePeriod = 3 * time.Second
	serfConfig.UserQuiescentPeriod = time.Second
	if config.ReconnectInterval != 0 {
		serfConfig.ReconnectInterval = config.ReconnectInterval
	}
	if config.ReconnectTimeout != 0 {
		serfConfig.ReconnectTimeout = config.ReconnectTimeout
	}
	if config.TombstoneTimeout != 0 {
		serfConfig.TombstoneTimeout = config.TombstoneTimeout
	}
	serfConfig.EnableNameConflictResolution = !config.DisableNameResolution
	if config.KeyringFile != "" {
		serfConfig.KeyringFile = config.KeyringFile
	}
	serfConfig.RejoinAfterLeave = config.RejoinAfterLeave

	// Create a channel to listen for events from Serf
	a.eventCh = make(chan serf.Event, 64)
	serfConfig.EventCh = a.eventCh

	// Start Serf
	a.Ui.Output("Starting Serf agent...")
	log.Printf("[INFO] agent: Serf agent starting")

	// Create serf first
	serf, err := serf.Create(serfConfig)
	if err != nil {
		a.Ui.Error(err.Error())
		log.Error(err)
		return nil
	}

	return serf
}

func (a *AgentCommand) Run(args []string) int {
	a.config = a.readConfig(args)
	if a.serf = a.setupSerf(a.config); a.serf == nil {
		log.Fatal("Can not setup serf")
	}
	a.join(a.config.StartJoin, true)

	if a.config.Server {
		a.etcd = NewEtcdClient(a.config.EtcdMachines, a)
		a.sched = NewScheduler()

		go func() {
			a.ServeHTTP()
		}()

		if a.ElectLeader() {
			jobs, err := a.etcd.GetJobs()
			if err != nil {
				log.Fatal(err)
			}
			a.sched.Start(jobs)
		}
	}
	go a.eventLoop()

	return a.handleSignals()
}

// handleSignals blocks until we get an exit-causing signal
func (a *AgentCommand) handleSignals() int {
	signalCh := make(chan os.Signal, 4)
	signal.Notify(signalCh, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)

	// Wait for a signal
	var sig os.Signal
	select {
	case s := <-signalCh:
		sig = s
	case <-a.ShutdownCh:
		sig = os.Interrupt
	}
	a.Ui.Output(fmt.Sprintf("Caught signal: %v", sig))

	// Check if we should do a graceful leave
	graceful := false
	if sig == syscall.SIGTERM || sig == os.Interrupt {
		graceful = true
	}

	// Bail fast if not doing a graceful leave
	if !graceful {
		return 1
	}

	// Attempt a graceful leave
	gracefulCh := make(chan struct{})
	a.Ui.Output("Gracefully shutting down agent...")
	go func() {
		if err := a.serf.Leave(); err != nil {
			a.Ui.Error(fmt.Sprintf("Error: %s", err))
			return
		}
		close(gracefulCh)
	}()

	// Wait for leave or another signal
	select {
	case <-signalCh:
		return 1
	case <-time.After(gracefulTimeout):
		return 1
	case <-gracefulCh:
		return 0
	}
}

func (a *AgentCommand) Synopsis() string {
	return "Run dcron server"
}

// dcron leader election routine
func (a *AgentCommand) ElectLeader() bool {
	leader := a.etcd.GetLeader()

	if leader != "" {
		if !a.isLeader(leader) {
			log.Debug("Trying to set itself as leader")
			res, err := a.etcd.Client.CompareAndSwap(keyspace+"/leader", a.config.Tags["key"], 0, leader, 0)
			if err != nil {
				log.Errorln("Error trying to set itself as leader", err)
				return false
			}

			log.WithFields(logrus.Fields{
				"old_leader": res.PrevNode.Value,
				"new_leader": res.Node.Value,
			}).Debug("Leader Swap")
			return true
		} else {
			log.Printf("The current leader [%s] is active", leader)
		}
	} else {
		log.Debug("Trying to set itself as leader")
		res, err := a.etcd.Client.Create(keyspace+"/leader", a.config.NodeName, 0)
		if err != nil {
			log.Error(res, err)
		}
		log.Printf("Successfully set [%s] as dcron leader", a.config.NodeName)
		return true
	}

	return false
}

func (a *AgentCommand) isLeader(leader string) bool {
	members := a.serf.Members()
	for _, member := range members {
		if member.Tags["key"] == leader && member.Status == serf.StatusAlive {
			return true
		}
	}
	return false
}

// eventLoop listens to events from Serf and fans out to event handlers
func (a *AgentCommand) eventLoop() {
	serfShutdownCh := a.serf.ShutdownCh()
	log.Info("agent: Listen for events")
	for {
		select {
		case e := <-a.eventCh:
			log.Infof("agent: Received event: %s", e.String())
			if e.EventType() == serf.EventMemberFailed && a.config.Server {
				failed := e.(serf.MemberEvent)
				for _, member := range failed.Members {
					if member.Tags["key"] == a.etcd.GetLeader() && member.Status != serf.StatusAlive {
						if a.ElectLeader() {
							log.Debug("Restarting scheduler")
							a.schedulerRestart()
						}
					}
				}
			}

			if e.EventType() == serf.EventQuery {
				query := e.(*serf.Query)

				if query.Name == QuerySchedulerRestart && a.config.Server {
					a.schedulerRestart()
				}

				if query.Name == QueryRunJob {
					log.Infof("Running job: %s", query.Payload)
					go func() {
						a.invokeJob(query.Payload)
					}()
				}

				if query.Name == QueryExecutionDone {

				}
			}

		case <-serfShutdownCh:
			log.Warn("agent: Serf shutdown detected, quitting")
			return
		}
	}
}

func (a *AgentCommand) schedulerRestart() {
	// Restart scheduler
	jobs, err := a.etcd.GetJobs()
	if err != nil {
		log.Fatal(err)
	}
	a.sched.Restart(jobs)
}

func (a *AgentCommand) schedulerReloadQuery(leader string) {
	params := &serf.QueryParam{
		FilterNodes: []string{leader},
		RequestAck:  true,
	}

	qr, err := a.serf.Query("scheduler:reload", []byte(""), params)
	if err != nil {
		log.Fatal("Error sending the scheduler reload query", err)
	}

	ackCh := qr.AckCh()
	respCh := qr.ResponseCh()

	for !qr.Finished() {
		select {
		case ack, ok := <-ackCh:
			if ok {
				log.WithFields(logrus.Fields{
					"from": ack,
				}).Debug("Received ack")
			}
		case resp, ok := <-respCh:
			if ok {
				log.WithFields(logrus.Fields{
					"from":    resp.From,
					"payload": string(resp.Payload),
				}).Debug("Received response")
			}
		}
	}
	log.Debugf("Done receiving acks and responses from scheduler reload query")
}

// Join asks the Serf instance to join. See the Serf.Join function.
func (a *AgentCommand) join(addrs []string, replay bool) (n int, err error) {
	log.Info("agent: joining: %v replay: %v", addrs, replay)
	ignoreOld := !replay
	n, err = a.serf.Join(addrs, ignoreOld)
	if n > 0 {
		log.Infof("agent: joined: %d nodes", n)
	}
	if err != nil {
		log.Warnf("agent: error joining: %v", err)
	}
	return
}

func (a *AgentCommand) RunQuery(job *Job) {
	filterNodes, err := a.processFilteredNodes(job)
	if err != nil {
		log.Fatalf("Error processing filtered nodes for job %s, %s", job.Name, err.Error())
	}
	log.Debug("Filtered nodes to run: ", filterNodes)
	log.Debug("Filtered tags to run: ", job.Tags)

	params := &serf.QueryParam{
		FilterNodes: filterNodes,
		FilterTags:  job.Tags,
		RequestAck:  true,
	}

	jobJson, _ := json.Marshal(job)
	log.Debugf("Sending %s query for job %s", QueryRunJob, job.Name)
	qr, err := a.serf.Query(QueryRunJob, jobJson, params)
	if err != nil {
		log.Fatalf("Error sending the %s query", QueryRunJob, err)
	}

	ackCh := qr.AckCh()
	respCh := qr.ResponseCh()

	for !qr.Finished() {
		select {
		case ack, ok := <-ackCh:
			if ok {
				log.WithFields(logrus.Fields{
					"from": ack,
				}).Debug("Received ack")
			}
		case resp, ok := <-respCh:
			if ok {
				log.WithFields(logrus.Fields{
					"from":    resp.From,
					"payload": string(resp.Payload),
				}).Debug("Received response")
			}
		}
	}
	log.Debugf("Done receiving acks and responses from run query")
}

func (a *AgentCommand) processFilteredNodes(job *Job) ([]string, error) {
	var nodes []string
	for jtk, jtv := range job.Tags {
		var tc []string
		if tc = strings.Split(jtv, ":"); len(tc) == 2 {
			tv := tc[0]

			// Set original tag to clean tag
			job.Tags[jtk] = tv

			count, err := strconv.Atoi(tc[1])
			if err != nil {
				return nil, err
			}

			for _, member := range a.serf.Members() {
				for mtk, mtv := range member.Tags {
					if mtk == jtk && mtv == jtv {
						if len(nodes) < count {
							nodes = append(nodes, member.Name)
						}
					}
				}
			}
		}
	}

	return nodes, nil
}