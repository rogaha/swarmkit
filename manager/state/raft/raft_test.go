package raft_test

import (
	"fmt"
	"io/ioutil"
	"log"
	"reflect"
	"testing"
	"time"

	"google.golang.org/grpc/grpclog"

	"golang.org/x/net/context"

	"github.com/Sirupsen/logrus"
	"github.com/docker/swarmkit/api"
	cautils "github.com/docker/swarmkit/ca/testutils"
	"github.com/docker/swarmkit/manager/state/raft"
	raftutils "github.com/docker/swarmkit/manager/state/raft/testutils"
	"github.com/docker/swarmkit/manager/state/store"
	"github.com/pivotal-golang/clock/fakeclock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var tc *cautils.TestCA

func init() {
	grpclog.SetLogger(log.New(ioutil.Discard, "", log.LstdFlags))
	logrus.SetOutput(ioutil.Discard)

	tc = cautils.NewTestCA(nil, cautils.AcceptancePolicy(true, true, ""))
}

func TestRaftBootstrap(t *testing.T) {
	t.Parallel()

	nodes, _ := raftutils.NewRaftCluster(t, tc)
	defer raftutils.TeardownCluster(t, nodes)

	assert.Equal(t, 3, len(nodes[1].GetMemberlist()))
	assert.Equal(t, 3, len(nodes[2].GetMemberlist()))
	assert.Equal(t, 3, len(nodes[3].GetMemberlist()))
}

func TestRaftLeader(t *testing.T) {
	t.Parallel()

	nodes, _ := raftutils.NewRaftCluster(t, tc)
	defer raftutils.TeardownCluster(t, nodes)

	assert.True(t, nodes[1].IsLeader(), "error: node 1 is not the Leader")

	// nodes should all have the same leader
	assert.Equal(t, nodes[1].Leader(), nodes[1].Config.ID)
	assert.Equal(t, nodes[2].Leader(), nodes[1].Config.ID)
	assert.Equal(t, nodes[3].Leader(), nodes[1].Config.ID)
}

func TestRaftLeaderDown(t *testing.T) {
	t.Parallel()

	nodes, clockSource := raftutils.NewRaftCluster(t, tc)
	defer raftutils.TeardownCluster(t, nodes)

	// Stop node 1
	nodes[1].Stop()

	newCluster := map[uint64]*raftutils.TestNode{
		2: nodes[2],
		3: nodes[3],
	}
	// Wait for the re-election to occur
	raftutils.WaitForCluster(t, clockSource, newCluster)

	// Leader should not be 1
	assert.NotEqual(t, nodes[2].Leader(), nodes[1].Config.ID)

	// Ensure that node 2 and node 3 have the same leader
	assert.Equal(t, nodes[3].Leader(), nodes[2].Leader())

	// Find the leader node and a follower node
	var (
		leaderNode   *raftutils.TestNode
		followerNode *raftutils.TestNode
	)
	for i, n := range newCluster {
		if n.Config.ID == n.Leader() {
			leaderNode = n
			if i == 2 {
				followerNode = newCluster[3]
			} else {
				followerNode = newCluster[2]
			}
		}
	}

	require.NotNil(t, leaderNode)
	require.NotNil(t, followerNode)

	// Propose a value
	value, err := raftutils.ProposeValue(t, leaderNode)
	assert.NoError(t, err, "failed to propose value")

	// The value should be replicated on all remaining nodes
	raftutils.CheckValue(t, clockSource, leaderNode, value)
	assert.Equal(t, len(leaderNode.GetMemberlist()), 3)

	raftutils.CheckValue(t, clockSource, followerNode, value)
	assert.Equal(t, len(followerNode.GetMemberlist()), 3)
}

func TestRaftFollowerDown(t *testing.T) {
	t.Parallel()

	nodes, clockSource := raftutils.NewRaftCluster(t, tc)
	defer raftutils.TeardownCluster(t, nodes)

	// Stop node 3
	nodes[3].Stop()

	// Leader should still be 1
	assert.True(t, nodes[1].IsLeader(), "node 1 is not a leader anymore")
	assert.Equal(t, nodes[2].Leader(), nodes[1].Config.ID)

	// Propose a value
	value, err := raftutils.ProposeValue(t, nodes[1])
	assert.NoError(t, err, "failed to propose value")

	// The value should be replicated on all remaining nodes
	raftutils.CheckValue(t, clockSource, nodes[1], value)
	assert.Equal(t, len(nodes[1].GetMemberlist()), 3)

	raftutils.CheckValue(t, clockSource, nodes[2], value)
	assert.Equal(t, len(nodes[2].GetMemberlist()), 3)
}

func TestRaftLogReplication(t *testing.T) {
	t.Parallel()

	nodes, clockSource := raftutils.NewRaftCluster(t, tc)
	defer raftutils.TeardownCluster(t, nodes)

	// Propose a value
	value, err := raftutils.ProposeValue(t, nodes[1])
	assert.NoError(t, err, "failed to propose value")

	// All nodes should have the value in the physical store
	raftutils.CheckValue(t, clockSource, nodes[1], value)
	raftutils.CheckValue(t, clockSource, nodes[2], value)
	raftutils.CheckValue(t, clockSource, nodes[3], value)
}

func TestRaftLogReplicationWithoutLeader(t *testing.T) {
	t.Parallel()
	nodes, clockSource := raftutils.NewRaftCluster(t, tc)
	defer raftutils.TeardownCluster(t, nodes)

	// Stop the leader
	nodes[1].Stop()

	// Propose a value
	_, err := raftutils.ProposeValue(t, nodes[2])
	assert.Error(t, err)

	// No value should be replicated in the store in the absence of the leader
	raftutils.CheckNoValue(t, clockSource, nodes[2])
	raftutils.CheckNoValue(t, clockSource, nodes[3])
}

func TestRaftQuorumFailure(t *testing.T) {
	t.Parallel()

	// Bring up a 5 nodes cluster
	nodes, clockSource := raftutils.NewRaftCluster(t, tc)
	raftutils.AddRaftNode(t, clockSource, nodes, tc)
	raftutils.AddRaftNode(t, clockSource, nodes, tc)
	defer raftutils.TeardownCluster(t, nodes)

	// Lose a majority
	for i := uint64(3); i <= 5; i++ {
		nodes[i].Server.Stop()
		nodes[i].Stop()
	}

	// Propose a value
	_, err := raftutils.ProposeValue(t, nodes[1])
	assert.Error(t, err)

	// The value should not be replicated, we have no majority
	raftutils.CheckNoValue(t, clockSource, nodes[2])
	raftutils.CheckNoValue(t, clockSource, nodes[1])
}

func TestRaftQuorumRecovery(t *testing.T) {
	t.Parallel()

	// Bring up a 5 nodes cluster
	nodes, clockSource := raftutils.NewRaftCluster(t, tc)
	raftutils.AddRaftNode(t, clockSource, nodes, tc)
	raftutils.AddRaftNode(t, clockSource, nodes, tc)
	defer raftutils.TeardownCluster(t, nodes)

	// Lose a majority
	for i := uint64(1); i <= 3; i++ {
		nodes[i].Server.Stop()
		nodes[i].Shutdown()
	}

	raftutils.AdvanceTicks(clockSource, 5)

	// Restore the majority by restarting node 3
	nodes[3] = raftutils.RestartNode(t, clockSource, nodes[3], false)

	delete(nodes, 1)
	delete(nodes, 2)
	raftutils.WaitForCluster(t, clockSource, nodes)

	// Propose a value
	value, err := raftutils.ProposeValue(t, raftutils.Leader(nodes))
	assert.NoError(t, err)

	for _, node := range nodes {
		raftutils.CheckValue(t, clockSource, node, value)
	}
}

func TestRaftFollowerLeave(t *testing.T) {
	t.Parallel()

	// Bring up a 5 nodes cluster
	nodes, clockSource := raftutils.NewRaftCluster(t, tc)
	raftutils.AddRaftNode(t, clockSource, nodes, tc)
	raftutils.AddRaftNode(t, clockSource, nodes, tc)
	nodes, clockSource = raftutils.NewRaftCluster(t, tc)
	raftutils.AddRaftNode(t, clockSource, nodes, tc)
	raftutils.AddRaftNode(t, clockSource, nodes, tc)
	defer raftutils.TeardownCluster(t, nodes)

	// Node 5 leaves the cluster
	// Use gRPC instead of calling handler directly because of
	// authorization check.
	client, err := nodes[1].ConnectToMember(nodes[1].Address, 10*time.Second)
	assert.NoError(t, err)
	defer client.Conn.Close()
	ctx, _ := context.WithTimeout(context.Background(), 10*time.Second)
	resp, err := client.Leave(ctx, &api.LeaveRequest{Node: &api.RaftMember{RaftID: nodes[5].Config.ID}})
	assert.NoError(t, err, "error sending message to leave the raft")
	assert.NotNil(t, resp, "leave response message is nil")

	delete(nodes, 5)

	raftutils.WaitForPeerNumber(t, clockSource, nodes, 4)

	// Propose a value
	value, err := raftutils.ProposeValue(t, nodes[1])
	assert.NoError(t, err, "failed to propose value")

	// Value should be replicated on every node
	raftutils.CheckValue(t, clockSource, nodes[1], value)
	assert.Equal(t, len(nodes[1].GetMemberlist()), 4)

	raftutils.CheckValue(t, clockSource, nodes[2], value)
	assert.Equal(t, len(nodes[2].GetMemberlist()), 4)

	raftutils.CheckValue(t, clockSource, nodes[3], value)
	assert.Equal(t, len(nodes[3].GetMemberlist()), 4)

	raftutils.CheckValue(t, clockSource, nodes[4], value)
	assert.Equal(t, len(nodes[4].GetMemberlist()), 4)
}

func TestRaftLeaderLeave(t *testing.T) {
	t.Parallel()

	nodes, clockSource := raftutils.NewRaftCluster(t, tc)

	// node 1 is the leader
	assert.Equal(t, nodes[1].Leader(), nodes[1].Config.ID)

	// Try to leave the raft
	// Use gRPC instead of calling handler directly because of
	// authorization check.
	client, err := nodes[1].ConnectToMember(nodes[1].Address, 10*time.Second)
	assert.NoError(t, err)
	defer client.Conn.Close()
	ctx, _ := context.WithTimeout(context.Background(), 10*time.Second)
	resp, err := client.Leave(ctx, &api.LeaveRequest{Node: &api.RaftMember{RaftID: nodes[1].Config.ID}})
	assert.NoError(t, err, "error sending message to leave the raft")
	assert.NotNil(t, resp, "leave response message is nil")

	newCluster := map[uint64]*raftutils.TestNode{
		2: nodes[2],
		3: nodes[3],
	}
	// Wait for election tick
	raftutils.WaitForCluster(t, clockSource, newCluster)

	// Leader should not be 1
	assert.NotEqual(t, nodes[2].Leader(), nodes[1].Config.ID)
	assert.Equal(t, nodes[2].Leader(), nodes[3].Leader())

	leader := nodes[2].Leader()

	// Find the leader node and a follower node
	var (
		leaderNode   *raftutils.TestNode
		followerNode *raftutils.TestNode
	)
	for i, n := range nodes {
		if n.Config.ID == leader {
			leaderNode = n
			if i == 2 {
				followerNode = nodes[3]
			} else {
				followerNode = nodes[2]
			}
		}
	}

	require.NotNil(t, leaderNode)
	require.NotNil(t, followerNode)

	// Propose a value
	value, err := raftutils.ProposeValue(t, leaderNode)
	assert.NoError(t, err, "failed to propose value")

	// The value should be replicated on all remaining nodes
	raftutils.CheckValue(t, clockSource, leaderNode, value)
	assert.Equal(t, len(leaderNode.GetMemberlist()), 2)

	raftutils.CheckValue(t, clockSource, followerNode, value)
	assert.Equal(t, len(followerNode.GetMemberlist()), 2)

	raftutils.TeardownCluster(t, newCluster)
}

func TestRaftNewNodeGetsData(t *testing.T) {
	t.Parallel()

	// Bring up a 3 node cluster
	nodes, clockSource := raftutils.NewRaftCluster(t, tc)
	defer raftutils.TeardownCluster(t, nodes)

	// Propose a value
	value, err := raftutils.ProposeValue(t, nodes[1])
	assert.NoError(t, err, "failed to propose value")

	// Add a new node
	raftutils.AddRaftNode(t, clockSource, nodes, tc)

	time.Sleep(500 * time.Millisecond)

	// Value should be replicated on every node
	for _, node := range nodes {
		raftutils.CheckValue(t, clockSource, node, value)
		assert.Equal(t, len(node.GetMemberlist()), 4)
	}
}

func TestRaftRejoin(t *testing.T) {
	t.Parallel()

	nodes, clockSource := raftutils.NewRaftCluster(t, tc)
	defer raftutils.TeardownCluster(t, nodes)

	ids := []string{"id1", "id2"}

	// Propose a value
	values := make([]*api.Node, 2)
	var err error
	values[0], err = raftutils.ProposeValue(t, nodes[1], ids[0])
	assert.NoError(t, err, "failed to propose value")

	// The value should be replicated on node 3
	raftutils.CheckValue(t, clockSource, nodes[3], values[0])
	assert.Equal(t, len(nodes[3].GetMemberlist()), 3)

	// Stop node 3
	nodes[3].Server.Stop()
	nodes[3].Shutdown()

	// Propose another value
	values[1], err = raftutils.ProposeValue(t, nodes[1], ids[1])
	assert.NoError(t, err, "failed to propose value")

	// Nodes 1 and 2 should have the new value
	raftutils.CheckValuesOnNodes(t, clockSource, map[uint64]*raftutils.TestNode{1: nodes[1], 2: nodes[2]}, ids, values)

	nodes[3] = raftutils.RestartNode(t, clockSource, nodes[3], false)
	raftutils.WaitForCluster(t, clockSource, nodes)

	// Node 3 should have all values, including the one proposed while
	// it was unavailable.
	raftutils.CheckValuesOnNodes(t, clockSource, nodes, ids, values)
}

func testRaftRestartCluster(t *testing.T, stagger bool) {
	nodes, clockSource := raftutils.NewRaftCluster(t, tc)
	defer raftutils.TeardownCluster(t, nodes)

	// Propose a value
	values := make([]*api.Node, 2)
	var err error
	values[0], err = raftutils.ProposeValue(t, nodes[1], "id1")
	assert.NoError(t, err, "failed to propose value")

	// Stop all nodes
	for _, node := range nodes {
		node.Server.Stop()
		node.Shutdown()
	}

	raftutils.AdvanceTicks(clockSource, 5)

	// Restart all nodes
	i := 0
	for k, node := range nodes {
		if stagger && i != 0 {
			raftutils.AdvanceTicks(clockSource, 1)
		}
		nodes[k] = raftutils.RestartNode(t, clockSource, node, false)
		i++
	}
	raftutils.WaitForCluster(t, clockSource, nodes)

	// Propose another value
	values[1], err = raftutils.ProposeValue(t, raftutils.Leader(nodes), "id2")
	assert.NoError(t, err, "failed to propose value")

	for _, node := range nodes {
		assert.NoError(t, raftutils.PollFunc(clockSource, func() error {
			var err error
			node.MemoryStore().View(func(tx store.ReadTx) {
				var allNodes []*api.Node
				allNodes, err = store.FindNodes(tx, store.All)
				if err != nil {
					return
				}
				if len(allNodes) != 2 {
					err = fmt.Errorf("expected 2 nodes, got %d", len(allNodes))
					return
				}

				for i, nodeID := range []string{"id1", "id2"} {
					n := store.GetNode(tx, nodeID)
					if !reflect.DeepEqual(n, values[i]) {
						err = fmt.Errorf("node %s did not match expected value", nodeID)
						return
					}
				}
			})
			return err
		}))
	}
}

func TestRaftRestartClusterSimultaneously(t *testing.T) {
	t.Parallel()

	// Establish a cluster, stop all nodes (simulating a total outage), and
	// restart them simultaneously.
	testRaftRestartCluster(t, false)
}

func TestRaftRestartClusterStaggered(t *testing.T) {
	t.Parallel()

	// Establish a cluster, stop all nodes (simulating a total outage), and
	// restart them one at a time.
	testRaftRestartCluster(t, true)
}

func TestRaftForceNewCluster(t *testing.T) {
	t.Parallel()

	nodes, clockSource := raftutils.NewRaftCluster(t, tc)

	// Propose a value
	values := make([]*api.Node, 2)
	var err error
	values[0], err = raftutils.ProposeValue(t, nodes[1], "id1")
	assert.NoError(t, err, "failed to propose value")

	// The memberlist should contain 3 members on each node
	for i := 1; i <= 3; i++ {
		assert.Equal(t, len(nodes[uint64(i)].GetMemberlist()), 3)
	}

	// Stop all nodes
	for _, node := range nodes {
		node.Server.Stop()
		node.Shutdown()
	}

	raftutils.AdvanceTicks(clockSource, 5)

	toClean := map[uint64]*raftutils.TestNode{
		2: nodes[2],
		3: nodes[3],
	}
	raftutils.TeardownCluster(t, toClean)
	delete(nodes, 2)
	delete(nodes, 3)

	// Only restart the first node with force-new-cluster option
	nodes[1] = raftutils.RestartNode(t, clockSource, nodes[1], true)
	raftutils.WaitForCluster(t, clockSource, nodes)

	// The memberlist should contain only one node (self)
	assert.Equal(t, len(nodes[1].GetMemberlist()), 1)

	// Add 2 more members
	nodes[2] = raftutils.NewJoinNode(t, clockSource, nodes[1].Address, tc)
	raftutils.WaitForCluster(t, clockSource, nodes)

	nodes[3] = raftutils.NewJoinNode(t, clockSource, nodes[1].Address, tc)
	raftutils.WaitForCluster(t, clockSource, nodes)

	newCluster := map[uint64]*raftutils.TestNode{
		1: nodes[1],
		2: nodes[2],
		3: nodes[3],
	}
	defer raftutils.TeardownCluster(t, newCluster)

	// The memberlist should contain 3 members on each node
	for i := 1; i <= 3; i++ {
		assert.Equal(t, len(nodes[uint64(i)].GetMemberlist()), 3)
	}

	// Propose another value
	values[1], err = raftutils.ProposeValue(t, raftutils.Leader(nodes), "id2")
	assert.NoError(t, err, "failed to propose value")

	for _, node := range nodes {
		assert.NoError(t, raftutils.PollFunc(clockSource, func() error {
			var err error
			node.MemoryStore().View(func(tx store.ReadTx) {
				var allNodes []*api.Node
				allNodes, err = store.FindNodes(tx, store.All)
				if err != nil {
					return
				}
				if len(allNodes) != 2 {
					err = fmt.Errorf("expected 2 nodes, got %d", len(allNodes))
					return
				}

				for i, nodeID := range []string{"id1", "id2"} {
					n := store.GetNode(tx, nodeID)
					if !reflect.DeepEqual(n, values[i]) {
						err = fmt.Errorf("node %s did not match expected value", nodeID)
						return
					}
				}
			})
			return err
		}))
	}
}

func TestRaftUnreachableNode(t *testing.T) {
	t.Parallel()

	nodes := make(map[uint64]*raftutils.TestNode)
	var clockSource *fakeclock.FakeClock
	nodes[1], clockSource = raftutils.NewInitNode(t, tc, nil)

	ctx := context.Background()
	// Add a new node, but don't start its server yet
	n := raftutils.NewNode(t, clockSource, tc, raft.NewNodeOptions{JoinAddr: nodes[1].Address})
	go n.Run(ctx)

	raftutils.AdvanceTicks(clockSource, 5)
	time.Sleep(100 * time.Millisecond)

	raft.Register(n.Server, n.Node)

	// Now start the new node's server
	go func() {
		// After stopping, we should receive an error from Serve
		assert.Error(t, n.Server.Serve(n.Listener))
	}()

	nodes[2] = n
	raftutils.WaitForCluster(t, clockSource, nodes)

	defer raftutils.TeardownCluster(t, nodes)

	// Propose a value
	value, err := raftutils.ProposeValue(t, nodes[1])
	assert.NoError(t, err, "failed to propose value")

	// All nodes should have the value in the physical store
	raftutils.CheckValue(t, clockSource, nodes[1], value)
	raftutils.CheckValue(t, clockSource, nodes[2], value)
}

func TestRaftJoinFollower(t *testing.T) {
	t.Parallel()

	nodes := make(map[uint64]*raftutils.TestNode)
	var clockSource *fakeclock.FakeClock
	nodes[1], clockSource = raftutils.NewInitNode(t, tc, nil)
	nodes[2] = raftutils.NewJoinNode(t, clockSource, nodes[1].Address, tc)
	raftutils.WaitForCluster(t, clockSource, nodes)

	// Point new node at a follower's address, rather than the leader
	nodes[3] = raftutils.NewJoinNode(t, clockSource, nodes[2].Address, tc)
	raftutils.WaitForCluster(t, clockSource, nodes)
	defer raftutils.TeardownCluster(t, nodes)

	// Propose a value
	value, err := raftutils.ProposeValue(t, nodes[1])
	assert.NoError(t, err, "failed to propose value")

	// All nodes should have the value in the physical store
	raftutils.CheckValue(t, clockSource, nodes[1], value)
	raftutils.CheckValue(t, clockSource, nodes[2], value)
	raftutils.CheckValue(t, clockSource, nodes[3], value)
}
