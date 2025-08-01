// FIXME(thaJeztah): remove once we are a module; the go:build directive prevents go from downgrading language version to go1.16:
//go:build go1.23

package networkdb

//go:generate protoc -I=. -I=../../../vendor/ --gogofaster_out=import_path=github.com/docker/docker/daemon/libnetwork/networkdb:. networkdb.proto

import (
	"context"
	cryptorand "crypto/rand"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/containerd/log"
	"github.com/docker/docker/daemon/internal/stringid"
	"github.com/docker/docker/daemon/libnetwork/types"
	"github.com/docker/go-events"
	iradix "github.com/hashicorp/go-immutable-radix/v2"
	"github.com/hashicorp/memberlist"
	"github.com/hashicorp/serf/serf"
)

const (
	byTable int = 1 + iota
	byNetwork
)

// NetworkDB instance drives the networkdb cluster and acts the broker
// for cluster-scoped and network-scoped gossip and watches.
type NetworkDB struct {
	// The clocks MUST be the first things
	// in this struct due to Golang issue #599.

	// Global lamport clock for node network attach events.
	networkClock serf.LamportClock

	// Global lamport clock for table events.
	tableClock serf.LamportClock

	sync.RWMutex

	// NetworkDB configuration.
	config *Config

	// All the tree index (byTable, byNetwork) that we maintain
	// the db.
	indexes map[int]*iradix.Tree[*entry]

	// Memberlist we use to drive the cluster.
	memberlist *memberlist.Memberlist

	// List of all peer nodes in the cluster not-limited to any
	// network.
	nodes map[string]*node

	// An approximation of len(nodes) that can be accessed without
	// synchronization.
	estNodes atomic.Int32

	// List of all peer nodes which have failed
	failedNodes map[string]*node

	// List of all peer nodes which have left
	leftNodes map[string]*node

	// A multi-dimensional map of network/node attachments for peer nodes.
	// The first key is a node name and the second key is a network ID for
	// the network that node is participating in.
	networks map[string]map[string]*network

	// A map of this node's network attachments.
	thisNodeNetworks map[string]*thisNodeNetwork

	// A map of nodes which are participating in a given
	// network. The key is a network ID.
	networkNodes map[string][]string

	// A table of ack channels for every node from which we are
	// waiting for an ack.
	bulkSyncAckTbl map[string]chan struct{}

	// Broadcast queue for network event gossip.
	networkBroadcasts *memberlist.TransmitLimitedQueue

	// Broadcast queue for node event gossip.
	nodeBroadcasts *memberlist.TransmitLimitedQueue

	// A central context to stop all go routines running on
	// behalf of the NetworkDB instance.
	ctx       context.Context
	cancelCtx context.CancelFunc

	// A central broadcaster for all local watchers watching table
	// events.
	broadcaster *events.Broadcaster

	// List of all tickers which needed to be stopped when
	// cleaning up.
	tickers []*time.Ticker

	// Reference to the memberlist's keyring to add & remove keys
	keyring *memberlist.Keyring

	// bootStrapIP is the list of IPs that can be used to bootstrap
	// the gossip.
	bootStrapIP []string

	// lastStatsTimestamp is the last timestamp when the stats got printed
	lastStatsTimestamp time.Time

	// lastHealthTimestamp is the last timestamp when the health score got printed
	lastHealthTimestamp time.Time

	rngMu sync.Mutex
	rng   *rand.Rand
}

// PeerInfo represents the peer (gossip cluster) nodes of a network
type PeerInfo struct {
	Name string
	IP   string
}

// PeerClusterInfo represents the peer (gossip cluster) nodes
type PeerClusterInfo struct {
	PeerInfo
}

type node struct {
	memberlist.Node
	ltime serf.LamportTime
	// Number of hours left before the reaper removes the node
	reapTime time.Duration
}

// network describes the node/network attachment.
type network struct {
	// Lamport time for the latest state of the entry.
	ltime serf.LamportTime

	// Node leave is in progress.
	leaving bool

	// Number of seconds still left before a deleted network entry gets
	// removed from networkDB
	reapTime time.Duration
}

// thisNodeNetwork describes a network attachment on the local node.
type thisNodeNetwork struct {
	network

	// Gets set to true after the first bulk sync happens
	inSync bool

	// The broadcast queue for this network's table event gossip
	// for entries owned by this node.
	tableBroadcasts *memberlist.TransmitLimitedQueue

	// The broadcast queue for this network's table event gossip
	// relayed from other nodes.
	//
	// Messages in this queue are broadcasted when there is space available
	// in the gossip packet after filling it with tableBroadcast messages.
	// Relayed messages are broadcasted at a lower priority than messages
	// originating from this node to ensure that local messages are always
	// broadcasted in a timely manner, irrespective of how many messages
	// from other nodes are queued for rebroadcasting.
	tableRebroadcasts *memberlist.TransmitLimitedQueue

	// Number of gossip messages sent related to this network during the last stats collection period
	qMessagesSent atomic.Int64

	// Number of entries on the network. This value is the sum of all the entries of all the tables of a specific network.
	// Its use is for statistics purposes. It keep tracks of database size and is printed per network every StatsPrintPeriod
	// interval
	entriesNumber atomic.Int64

	// An approximation of len(nDB.networkNodes[nid]) that can be accessed
	// without synchronization.
	networkNodes atomic.Int32
}

// Config represents the configuration of the networkdb instance and
// can be passed by the caller.
type Config struct {
	// NodeID is the node unique identifier of the node when is part of the cluster
	NodeID string

	// Hostname is the node hostname.
	Hostname string

	// BindAddr is the IP on which networkdb listens. It can be
	// 0.0.0.0 to listen on all addresses on the host.
	BindAddr string

	// AdvertiseAddr is the node's IP address that we advertise for
	// cluster communication.
	AdvertiseAddr string

	// BindPort is the local node's port to which we bind to for
	// cluster communication.
	BindPort int

	// Keys to be added to the Keyring of the memberlist. Key at index
	// 0 is the primary key
	Keys [][]byte

	// PacketBufferSize is the maximum number of bytes that memberlist will
	// put in a packet (this will be for UDP packets by default with a NetTransport).
	// A safe value for this is typically 1400 bytes (which is the default). However,
	// depending on your network's MTU (Maximum Transmission Unit) you may
	// be able to increase this to get more content into each gossip packet.
	PacketBufferSize int

	// reapEntryInterval duration of a deleted entry before being garbage collected
	reapEntryInterval time.Duration

	// reapNetworkInterval duration of a deleted network before being garbage collected
	// NOTE this MUST always be higher than reapEntryInterval
	reapNetworkInterval time.Duration

	// rejoinClusterDuration represents retryJoin timeout used by rejoinClusterBootStrap.
	// Default is 10sec.
	rejoinClusterDuration time.Duration

	// rejoinClusterInterval represents interval on which rejoinClusterBootStrap runs.
	// Default is 60sec.
	rejoinClusterInterval time.Duration

	// StatsPrintPeriod the period to use to print queue stats
	// Default is 5min
	StatsPrintPeriod time.Duration

	// HealthPrintPeriod the period to use to print the health score
	// Default is 1min
	HealthPrintPeriod time.Duration
}

// entry defines a table entry
type entry struct {
	// node from which this entry was learned.
	node string

	// Lamport time for the most recent update to the entry
	ltime serf.LamportTime

	// Opaque value store in the entry
	value []byte

	// Deleting the entry is in progress. All entries linger in
	// the cluster for certain amount of time after deletion.
	deleting bool

	// Number of seconds still left before a deleted table entry gets
	// removed from networkDB
	reapTime time.Duration
}

// DefaultConfig returns a NetworkDB config with default values
func DefaultConfig() *Config {
	hostname, _ := os.Hostname()
	return &Config{
		NodeID:                stringid.TruncateID(stringid.GenerateRandomID()),
		Hostname:              hostname,
		BindAddr:              "0.0.0.0",
		PacketBufferSize:      1400,
		StatsPrintPeriod:      5 * time.Minute,
		HealthPrintPeriod:     1 * time.Minute,
		reapEntryInterval:     30 * time.Minute,
		rejoinClusterDuration: 10 * time.Second,
		rejoinClusterInterval: 60 * time.Second,
	}
}

// New creates a new instance of NetworkDB using the Config passed by
// the caller.
func New(c *Config) (*NetworkDB, error) {
	nDB := newNetworkDB(c)
	log.G(context.TODO()).Infof("New memberlist node - Node:%v will use memberlist nodeID:%v with config:%+v", c.Hostname, c.NodeID, c)
	if err := nDB.clusterInit(); err != nil {
		return nil, err
	}

	return nDB, nil
}

func newNetworkDB(c *Config) *NetworkDB {
	// The garbage collection logic for entries leverage the presence of the network.
	// For this reason the expiration time of the network is put slightly higher than the entry expiration so that
	// there is at least 5 extra cycle to make sure that all the entries are properly deleted before deleting the network.
	c.reapNetworkInterval = c.reapEntryInterval + 5*reapPeriod

	var rngSeed [32]byte
	_, _ = cryptorand.Read(rngSeed[:]) // Documented never to return an error

	return &NetworkDB{
		config: c,
		indexes: map[int]*iradix.Tree[*entry]{
			byTable:   iradix.New[*entry](),
			byNetwork: iradix.New[*entry](),
		},
		networks:         make(map[string]map[string]*network),
		thisNodeNetworks: make(map[string]*thisNodeNetwork),
		nodes:            make(map[string]*node),
		failedNodes:      make(map[string]*node),
		leftNodes:        make(map[string]*node),
		networkNodes:     make(map[string][]string),
		bulkSyncAckTbl:   make(map[string]chan struct{}),
		broadcaster:      events.NewBroadcaster(),
		rng:              rand.New(rand.NewChaCha8(rngSeed)), //gosec:disable G404 -- not used in a security sensitive context
	}
}

// Join joins this NetworkDB instance with a list of peer NetworkDB
// instances passed by the caller in the form of addr:port
func (nDB *NetworkDB) Join(members []string) error {
	nDB.Lock()
	nDB.bootStrapIP = append([]string(nil), members...)
	log.G(context.TODO()).Infof("The new bootstrap node list is:%v", nDB.bootStrapIP)
	nDB.Unlock()
	return nDB.clusterJoin(members)
}

// Close destroys this NetworkDB instance by leave the cluster,
// stopping timers, canceling goroutines etc.
func (nDB *NetworkDB) Close() {
	if err := nDB.clusterLeave(); err != nil {
		log.G(context.TODO()).Errorf("%v(%v) Could not close DB: %v", nDB.config.Hostname, nDB.config.NodeID, err)
	}

	// Avoid (*Broadcaster).run goroutine leak
	nDB.broadcaster.Close()
}

// ClusterPeers returns all the gossip cluster peers.
func (nDB *NetworkDB) ClusterPeers() []PeerInfo {
	nDB.RLock()
	defer nDB.RUnlock()
	peers := make([]PeerInfo, 0, len(nDB.nodes))
	for _, node := range nDB.nodes {
		peers = append(peers, PeerInfo{
			Name: node.Name,
			IP:   node.Node.Addr.String(),
		})
	}
	return peers
}

// Peers returns the gossip peers for a given network.
func (nDB *NetworkDB) Peers(nid string) []PeerInfo {
	nDB.RLock()
	defer nDB.RUnlock()
	peers := make([]PeerInfo, 0, len(nDB.networkNodes[nid]))
	for _, nodeName := range nDB.networkNodes[nid] {
		if node, ok := nDB.nodes[nodeName]; ok {
			peers = append(peers, PeerInfo{
				Name: node.Name,
				IP:   node.Addr.String(),
			})
		} else {
			// Added for testing purposes, this condition should never happen else mean that the network list
			// is out of sync with the node list
			peers = append(peers, PeerInfo{Name: nodeName, IP: "unknown"})
		}
	}
	return peers
}

// GetEntry retrieves the value of a table entry in a given (network,
// table, key) tuple
func (nDB *NetworkDB) GetEntry(tname, nid, key string) ([]byte, error) {
	nDB.RLock()
	defer nDB.RUnlock()
	v, err := nDB.getEntry(tname, nid, key)
	if err != nil {
		return nil, err
	}
	if v != nil && v.deleting {
		return nil, types.NotFoundErrorf("entry in table %s network id %s and key %s deleted and pending garbage collection", tname, nid, key)
	}

	// note: this panics if a nil entry was stored in the table; after
	// discussion, we decided to not gracefully handle this situation as
	// this would be an unexpected situation;
	// see https://github.com/moby/moby/pull/48157#discussion_r1674428635
	return v.value, nil
}

func (nDB *NetworkDB) getEntry(tname, nid, key string) (*entry, error) {
	e, ok := nDB.indexes[byTable].Get([]byte(fmt.Sprintf("/%s/%s/%s", tname, nid, key)))
	if !ok {
		return nil, types.NotFoundErrorf("could not get entry in table %s with network id %s and key %s", tname, nid, key)
	}

	return e, nil
}

// CreateEntry creates a table entry in NetworkDB for given (network,
// table, key) tuple and if the NetworkDB is part of the cluster
// propagates this event to the cluster. It is an error to create an
// entry for the same tuple for which there is already an existing
// entry unless the current entry is deleting state.
func (nDB *NetworkDB) CreateEntry(tname, nid, key string, value []byte) error {
	nDB.Lock()
	oldEntry, err := nDB.getEntry(tname, nid, key)
	if err == nil || (oldEntry != nil && !oldEntry.deleting) {
		nDB.Unlock()
		return fmt.Errorf("cannot create entry in table %s with network id %s and key %s, already exists", tname, nid, key)
	}

	entry := &entry{
		ltime: nDB.tableClock.Increment(),
		node:  nDB.config.NodeID,
		value: value,
	}

	nDB.createOrUpdateEntry(nid, tname, key, entry)
	nDB.Unlock()

	if err := nDB.sendTableEvent(TableEventTypeCreate, nid, tname, key, entry); err != nil {
		return fmt.Errorf("cannot send create event for table %s, %v", tname, err)
	}

	return nil
}

// UpdateEntry updates a table entry in NetworkDB for given (network,
// table, key) tuple and if the NetworkDB is part of the cluster
// propagates this event to the cluster. It is an error to update a
// non-existent entry.
func (nDB *NetworkDB) UpdateEntry(tname, nid, key string, value []byte) error {
	nDB.Lock()
	if _, err := nDB.getEntry(tname, nid, key); err != nil {
		nDB.Unlock()
		return fmt.Errorf("cannot update entry as the entry in table %s with network id %s and key %s does not exist", tname, nid, key)
	}

	entry := &entry{
		ltime: nDB.tableClock.Increment(),
		node:  nDB.config.NodeID,
		value: value,
	}

	nDB.createOrUpdateEntry(nid, tname, key, entry)
	nDB.Unlock()

	if err := nDB.sendTableEvent(TableEventTypeUpdate, nid, tname, key, entry); err != nil {
		return fmt.Errorf("cannot send table update event: %v", err)
	}

	return nil
}

// TableElem elem
type TableElem struct {
	Value []byte
	owner string
}

// GetTableByNetwork walks the networkdb by the give table and network id and
// returns a map of keys and values
func (nDB *NetworkDB) GetTableByNetwork(tname, nid string) map[string]*TableElem {
	nDB.RLock()
	root := nDB.indexes[byTable].Root()
	nDB.RUnlock()
	entries := make(map[string]*TableElem)
	root.WalkPrefix([]byte(fmt.Sprintf("/%s/%s", tname, nid)), func(k []byte, v *entry) bool {
		if v.deleting {
			return false
		}
		key := string(k)
		key = key[strings.LastIndex(key, "/")+1:]
		entries[key] = &TableElem{Value: v.value, owner: v.node}
		return false
	})
	return entries
}

// DeleteEntry deletes a table entry in NetworkDB for given (network,
// table, key) tuple and if the NetworkDB is part of the cluster
// propagates this event to the cluster.
func (nDB *NetworkDB) DeleteEntry(tname, nid, key string) error {
	nDB.Lock()
	oldEntry, err := nDB.getEntry(tname, nid, key)
	if err != nil || oldEntry == nil || oldEntry.deleting {
		nDB.Unlock()
		return fmt.Errorf("cannot delete entry %s with network id %s and key %s "+
			"does not exist or is already being deleted", tname, nid, key)
	}

	entry := &entry{
		ltime:    nDB.tableClock.Increment(),
		node:     nDB.config.NodeID,
		value:    oldEntry.value,
		deleting: true,
		reapTime: nDB.config.reapEntryInterval,
	}

	nDB.createOrUpdateEntry(nid, tname, key, entry)
	nDB.Unlock()

	if err := nDB.sendTableEvent(TableEventTypeDelete, nid, tname, key, entry); err != nil {
		return fmt.Errorf("cannot send table delete event: %v", err)
	}

	return nil
}

func (nDB *NetworkDB) deleteNodeFromNetworks(deletedNode string) {
	for nid, nodes := range nDB.networkNodes {
		updatedNodes := make([]string, 0, len(nodes))
		for _, node := range nodes {
			if node == deletedNode {
				continue
			}

			updatedNodes = append(updatedNodes, node)
		}

		nDB.networkNodes[nid] = updatedNodes
	}

	delete(nDB.networks, deletedNode)
}

// deleteNodeNetworkEntries deletes all table entries for a network owned by
// node from the local store.
func (nDB *NetworkDB) deleteNodeNetworkEntries(nid, node string) {
	nDB.indexes[byNetwork].Root().WalkPrefix([]byte("/"+nid),
		func(path []byte, oldEntry *entry) bool {
			// Do nothing if the entry is owned by a remote node that is not leaving the network
			// because the event is triggered for a node that does not own this entry.
			if oldEntry.node != node {
				return false
			}
			params := strings.Split(string(path[1:]), "/")
			nwID, tName, key := params[0], params[1], params[2]

			nDB.deleteEntry(nwID, tName, key)

			// Notify to the upper layer only entries not already marked for deletion
			if !oldEntry.deleting {
				nDB.broadcaster.Write(WatchEvent{
					Table:     tName,
					NetworkID: nwID,
					Key:       key,
					Prev:      oldEntry.value,
				})
			}
			return false
		})
}

// deleteNodeTableEntries deletes all table entries owned by node from the local
// store, across all networks.
func (nDB *NetworkDB) deleteNodeTableEntries(node string) {
	nDB.indexes[byTable].Root().Walk(func(path []byte, oldEntry *entry) bool {
		if oldEntry.node != node {
			return false
		}

		params := strings.Split(string(path[1:]), "/")
		tName, nwID, key := params[0], params[1], params[2]

		nDB.deleteEntry(nwID, tName, key)

		if !oldEntry.deleting {
			nDB.broadcaster.Write(WatchEvent{
				Table:     tName,
				NetworkID: nwID,
				Key:       key,
				Prev:      oldEntry.value,
			})
		}
		return false
	})
}

// WalkTable walks a single table in NetworkDB and invokes the passed
// function for each entry in the table passing the network, key,
// value. The walk stops if the passed function returns a true.
func (nDB *NetworkDB) WalkTable(tname string, fn func(string, string, []byte, bool) bool) error {
	nDB.RLock()
	root := nDB.indexes[byTable].Root()
	nDB.RUnlock()
	root.WalkPrefix([]byte("/"+tname), func(path []byte, v *entry) bool {
		params := strings.Split(string(path[1:]), "/")
		nid := params[1]
		key := params[2]
		return fn(nid, key, v.value, v.deleting)
	})

	return nil
}

// JoinNetwork joins this node to a given network and propagates this
// event across the cluster. This triggers this node joining the
// sub-cluster of this network and participates in the network-scoped
// gossip and bulk sync for this network.
func (nDB *NetworkDB) JoinNetwork(nid string) error {
	ltime := nDB.networkClock.Increment()

	nDB.Lock()
	n, ok := nDB.thisNodeNetworks[nid]
	if ok {
		if !n.leaving {
			nDB.Unlock()
			return fmt.Errorf("networkdb: network %s is already joined", nid)
		}
		n.network = network{ltime: ltime}
		n.inSync = false
	} else {
		n = &thisNodeNetwork{
			network: network{ltime: ltime},
			tableBroadcasts: &memberlist.TransmitLimitedQueue{
				RetransmitMult: 4,
			},
			tableRebroadcasts: &memberlist.TransmitLimitedQueue{
				RetransmitMult: 4,
			},
		}
		numNodes := func() int { return int(n.networkNodes.Load()) }
		n.tableBroadcasts.NumNodes = numNodes
		n.tableRebroadcasts.NumNodes = numNodes
	}
	nDB.addNetworkNode(nid, nDB.config.NodeID)

	if err := nDB.sendNetworkEvent(nid, NetworkEventTypeJoin, ltime); err != nil {
		nDB.Unlock()
		return fmt.Errorf("failed to send join network event for %s: %v", nid, err)
	}

	networkNodes := nDB.networkNodes[nid]
	n.networkNodes.Store(int32(len(networkNodes)))
	nDB.thisNodeNetworks[nid] = n
	nDB.Unlock()

	log.G(context.TODO()).Debugf("%v(%v): joined network %s", nDB.config.Hostname, nDB.config.NodeID, nid)
	if _, err := nDB.bulkSync(networkNodes, true); err != nil {
		log.G(context.TODO()).Errorf("Error bulk syncing while joining network %s: %v", nid, err)
	}

	// Mark the network as being synced
	// note this is a best effort, we are not checking the result of the bulk sync
	nDB.Lock()
	n.inSync = true
	nDB.Unlock()

	return nil
}

// LeaveNetwork leaves this node from a given network and propagates
// this event across the cluster. This triggers this node leaving the
// sub-cluster of this network and as a result will no longer
// participate in the network-scoped gossip and bulk sync for this
// network. Also remove all the table entries for this network from
// networkdb
func (nDB *NetworkDB) LeaveNetwork(nid string) error {
	ltime := nDB.networkClock.Increment()
	if err := nDB.sendNetworkEvent(nid, NetworkEventTypeLeave, ltime); err != nil {
		return fmt.Errorf("failed to send leave network event for %s: %v", nid, err)
	}

	nDB.Lock()
	defer nDB.Unlock()

	// Remove myself from the list of the nodes participating to the network
	nDB.deleteNetworkNode(nid, nDB.config.NodeID)

	// Mark all the local entries for deletion
	// so that if we rejoin the network
	// before another node has received the network-leave notification,
	// the old entries owned by us will still be purged as expected.
	// Delete all the remote entries from our local store
	// without leaving any tombstone.
	// This ensures that we will accept the CREATE events
	// for entries owned by remote nodes
	// if we later rejoin the network.
	nDB.indexes[byNetwork].Root().WalkPrefix([]byte("/"+nid), func(path []byte, oldEntry *entry) bool {
		owned := oldEntry.node == nDB.config.NodeID
		if owned && oldEntry.deleting {
			return false
		}

		params := strings.Split(string(path[1:]), "/")
		nwID, tName, key := params[0], params[1], params[2]
		if owned {
			newEntry := &entry{
				ltime:    nDB.tableClock.Increment(),
				node:     oldEntry.node,
				value:    oldEntry.value,
				deleting: true,
				reapTime: nDB.config.reapEntryInterval,
			}
			nDB.createOrUpdateEntry(nwID, tName, key, newEntry)
		} else {
			nDB.deleteEntry(nwID, tName, key)
		}
		if !oldEntry.deleting {
			nDB.broadcaster.Write(WatchEvent{
				Table:     tName,
				NetworkID: nwID,
				Key:       key,
				Prev:      oldEntry.value,
			})
		}
		return false
	})

	n, ok := nDB.thisNodeNetworks[nid]
	if !ok {
		return fmt.Errorf("could not find network %s while trying to leave", nid)
	}

	log.G(context.TODO()).Debugf("%v(%v): leaving network %s", nDB.config.Hostname, nDB.config.NodeID, nid)
	n.ltime = ltime
	n.reapTime = nDB.config.reapNetworkInterval
	n.leaving = true
	return nil
}

// addNetworkNode adds the node to the list of nodes which participate
// in the passed network only if it is not already present. Caller
// should hold the NetworkDB lock while calling this
func (nDB *NetworkDB) addNetworkNode(nid string, nodeName string) {
	nodes := nDB.networkNodes[nid]
	for _, node := range nodes {
		if node == nodeName {
			return
		}
	}

	nDB.networkNodes[nid] = append(nDB.networkNodes[nid], nodeName)
	if n, ok := nDB.thisNodeNetworks[nid]; ok {
		n.networkNodes.Store(int32(len(nDB.networkNodes[nid])))
	}
}

// Deletes the node from the list of nodes which participate in the
// passed network. Caller should hold the NetworkDB lock while calling
// this
func (nDB *NetworkDB) deleteNetworkNode(nid string, nodeName string) {
	nodes, ok := nDB.networkNodes[nid]
	if !ok || len(nodes) == 0 {
		return
	}
	newNodes := make([]string, 0, len(nodes)-1)
	for _, name := range nodes {
		if name == nodeName {
			continue
		}
		newNodes = append(newNodes, name)
	}
	nDB.networkNodes[nid] = newNodes
	if n, ok := nDB.thisNodeNetworks[nid]; ok {
		n.networkNodes.Store(int32(len(newNodes)))
	}
}

// findCommonNetworks find the networks that both this node and the
// passed node have joined.
func (nDB *NetworkDB) findCommonNetworks(nodeName string) []string {
	nDB.RLock()
	defer nDB.RUnlock()

	var networks []string
	for nid := range nDB.thisNodeNetworks {
		if n, ok := nDB.networks[nodeName][nid]; ok {
			if !n.leaving {
				networks = append(networks, nid)
			}
		}
	}

	return networks
}

func (nDB *NetworkDB) updateLocalNetworkTime() {
	nDB.Lock()
	defer nDB.Unlock()

	ltime := nDB.networkClock.Increment()
	for _, n := range nDB.thisNodeNetworks {
		n.ltime = ltime
	}
}

// createOrUpdateEntry this function handles the creation or update of entries into the local
// tree store. It is also used to keep in sync the entries number of the network (all tables are aggregated)
func (nDB *NetworkDB) createOrUpdateEntry(nid, tname, key string, v *entry) (okTable bool, okNetwork bool) {
	nDB.indexes[byTable], _, okTable = nDB.indexes[byTable].Insert([]byte(fmt.Sprintf("/%s/%s/%s", tname, nid, key)), v)
	nDB.indexes[byNetwork], _, okNetwork = nDB.indexes[byNetwork].Insert([]byte(fmt.Sprintf("/%s/%s/%s", nid, tname, key)), v)
	if !okNetwork {
		// Add only if it is an insert not an update
		n, ok := nDB.thisNodeNetworks[nid]
		if ok {
			n.entriesNumber.Add(1)
		}
	}
	return okTable, okNetwork
}

// deleteEntry this function handles the deletion of entries into the local tree store.
// It is also used to keep in sync the entries number of the network (all tables are aggregated)
func (nDB *NetworkDB) deleteEntry(nid, tname, key string) (okTable bool, okNetwork bool) {
	nDB.indexes[byTable], _, okTable = nDB.indexes[byTable].Delete([]byte(fmt.Sprintf("/%s/%s/%s", tname, nid, key)))
	nDB.indexes[byNetwork], _, okNetwork = nDB.indexes[byNetwork].Delete([]byte(fmt.Sprintf("/%s/%s/%s", nid, tname, key)))
	if okNetwork {
		// Remove only if the delete is successful
		n, ok := nDB.thisNodeNetworks[nid]
		if ok {
			n.entriesNumber.Add(-1)
		}
	}
	return okTable, okNetwork
}
