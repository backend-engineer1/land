package routing

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/ioutil"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

const (
	// basicGraphFilePath is the file path for a basic graph used within
	// the tests. The basic graph consists of 5 nodes with 5 channels
	// connecting them.
	basicGraphFilePath         = "testdata/basic_graph.json"
	excessiveHopsGraphFilePath = "testdata/excessive_hops.json"
)

// testGraph is the struct which coresponds to the JSON format used to encode
// graphs within the files in the testdata directory.
//
// TODO(roasbeef): add test graph auto-generator
type testGraph struct {
	Info  []string   `json:"info"`
	Nodes []testNode `json:"nodes"`
	Edges []testChan `json:"edges"`
}

// testNode represents a node within the test graph above. We skip certain
// information such as the node's IP address as that information isn't needed
// for our tests.
type testNode struct {
	Source bool   `json:"source"`
	PubKey string `json:"pubkey"`
	Alias  string `json:"alias"`
}

// testChan represents the JSON version of a payment channel. This struct
// matches the Json that's encoded under the "edges" key within the test graph.
type testChan struct {
	Node1        string  `json:"node_1"`
	Node2        string  `json:"node_2"`
	ChannelID    uint64  `json:"channel_id"`
	ChannelPoint string  `json:"channel_point"`
	Flags        uint16  `json:"flags"`
	Expiry       uint16  `json:"expiry"`
	MinHTLC      int64   `json:"min_htlc"`
	FeeBaseMsat  int64   `json:"fee_base_msat"`
	FeeRate      float64 `json:"fee_rate"`
	Capacity     int64   `json:"capacity"`
}

// makeTestGraph creates a new instance of a channeldb.ChannelGraph for testing
// purposes. A callback which cleans up the created temporary directories is
// also returned and intended to be executed after the test completes.
func makeTestGraph() (*channeldb.ChannelGraph, func(), error) {
	// First, create a temporary directory to be used for the duration of
	// this test.
	tempDirName, err := ioutil.TempDir("", "channeldb")
	if err != nil {
		return nil, nil, err
	}

	// Next, create channeldb for the first time.
	cdb, err := channeldb.Open(tempDirName)
	if err != nil {
		return nil, nil, err
	}

	cleanUp := func() {
		cdb.Close()
		os.RemoveAll(tempDirName)
	}

	return cdb.ChannelGraph(), cleanUp, nil
}

// aliasMap is a map from a node's alias to its public key. This type is
// provided in order to allow easily look up from the human rememberable alias
// to an exact node's public key.
type aliasMap map[string]*btcec.PublicKey

// parseTestGraph returns a fully populated ChannelGraph given a path to a JSON
// file which encodes a test graph.
func parseTestGraph(path string) (*channeldb.ChannelGraph, func(), aliasMap, error) {
	graphJson, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, nil, nil, err
	}

	// First unmarshal the JSON graph into an instance of the testGraph
	// struct. Using the struct tags created above in the struct, the JSON
	// will be properly parsed into the struct above.
	var g testGraph
	if err := json.Unmarshal(graphJson, &g); err != nil {
		return nil, nil, nil, err
	}

	// We'll use this fake address for the IP address of all the nodes in
	// our tests. This value isn't needed for path finding so it doesn't
	// need to be unique.
	testAddr, err := net.ResolveTCPAddr("tcp", "192.0.0.1:8888")
	if err != nil {
		return nil, nil, nil, err
	}

	// Next, create a temporary graph database for usage within the test.
	graph, cleanUp, err := makeTestGraph()
	if err != nil {
		return nil, nil, nil, err
	}

	aliasMap := make(map[string]*btcec.PublicKey)
	var source *channeldb.LightningNode

	// First we insert all the nodes within the graph as vertexes.
	for _, node := range g.Nodes {
		pubBytes, err := hex.DecodeString(node.PubKey)
		if err != nil {
			return nil, nil, nil, err
		}
		pub, err := btcec.ParsePubKey(pubBytes, btcec.S256())
		if err != nil {
			return nil, nil, nil, err
		}

		dbNode := &channeldb.LightningNode{
			LastUpdate: time.Now(),
			Address:    testAddr,
			PubKey:     pub,
			Alias:      node.Alias,
		}

		// We require all aliases within the graph to be unique for our
		// tests.
		if _, ok := aliasMap[node.Alias]; ok {
			return nil, nil, nil, errors.New("aliases for nodes " +
				"must be unique!")
		} else {
			// If the alias is unique, then add the node to the
			// alias map for easy lookup.
			aliasMap[node.Alias] = pub
		}

		// If the node is tagged as the source, then we create a
		// pointer to is so we can mark the source in the graph
		// properly.
		if node.Source {
			// If we come across a node that's marked as the
			// source, and we've already set the source in a prior
			// iteration, then the JSON has an error as only ONE
			// node can be the source in the graph.
			if source != nil {
				return nil, nil, nil, errors.New("JSON is invalid " +
					"multiple nodes are tagged as the source")
			}

			source = dbNode
		}

		// With the node fully parsed, add it as a vertex within the
		// graph.
		if err := graph.AddLightningNode(dbNode); err != nil {
			return nil, nil, nil, err
		}
	}

	// Set the selected source node
	if err := graph.SetSourceNode(source); err != nil {
		return nil, nil, nil, err
	}

	// With all the vertexes inserted, we can now insert the edges into the
	// test graph.
	for _, edge := range g.Edges {
		node1Bytes, err := hex.DecodeString(edge.Node1)
		if err != nil {
			return nil, nil, nil, err
		}
		node1Pub, err := btcec.ParsePubKey(node1Bytes, btcec.S256())
		if err != nil {
			return nil, nil, nil, err
		}

		node2Bytes, err := hex.DecodeString(edge.Node2)
		if err != nil {
			return nil, nil, nil, err
		}
		node2Pub, err := btcec.ParsePubKey(node2Bytes, btcec.S256())
		if err != nil {
			return nil, nil, nil, err
		}

		fundingTXID := strings.Split(edge.ChannelPoint, ":")[0]
		txidBytes, err := chainhash.NewHashFromStr(fundingTXID)
		if err != nil {
			return nil, nil, nil, err
		}
		fundingPoint := wire.OutPoint{
			Hash:  *txidBytes,
			Index: 0,
		}

		// We first insert the existence of the edge between the two
		// nodes.
		if err := graph.AddChannelEdge(node1Pub, node2Pub, &fundingPoint,
			edge.ChannelID); err != nil {
			return nil, nil, nil, err
		}

		edge := &channeldb.ChannelEdge{
			ChannelID:                 edge.ChannelID,
			ChannelPoint:              fundingPoint,
			LastUpdate:                time.Now(),
			Expiry:                    edge.Expiry,
			MinHTLC:                   btcutil.Amount(edge.MinHTLC),
			FeeBaseMSat:               btcutil.Amount(edge.FeeBaseMsat),
			FeeProportionalMillionths: btcutil.Amount(edge.FeeRate),
			Capacity:                  btcutil.Amount(edge.Capacity),
		}

		// As the graph itself is directed, we need to insert two edges
		// into the graph: one from node1->node2 and one from
		// node2->node1. A flag of 0 indicates this is the routing
		// policy for the first node, and a flag of 1 indicates its the
		// information for the second node.
		edge.Flags = 0
		if err := graph.UpdateEdgeInfo(edge); err != nil {
			return nil, nil, nil, err
		}

		edge.Flags = 1
		if err := graph.UpdateEdgeInfo(edge); err != nil {
			return nil, nil, nil, err
		}
	}

	return graph, cleanUp, aliasMap, nil
}

func TestBasicGraphPathFinding(t *testing.T) {
	graph, cleanUp, aliases, err := parseTestGraph(basicGraphFilePath)
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to create graph: %v", err)
	}

	// With the test graph loaded, we'll test some basic path finding using
	// the pre-generated graph. Consult the testdata/basic_graph.json file
	// to follow along with the assumptions we'll use to test the path
	// finding.

	const paymentAmt = btcutil.Amount(100)
	target := aliases["sophon"]
	route, err := findRoute(graph, target, paymentAmt)
	if err != nil {
		t.Fatalf("unable to find route: %v", err)
	}

	// The length of the route selected should be of exactly length two.
	if len(route.Hops) != 2 {
		t.Fatalf("route is of incorrect length, expected %v got %v", 2,
			len(route.Hops))
	}

	// As each hop only decrements a single block from the time-lock, the
	// total time lock value should be two.
	if route.TotalTimeLock != 2 {
		t.Fatalf("expected time lock of %v, instead have %v", 2,
			route.TotalTimeLock)
	}

	// The first hop in the path should be an edge from roasbeef to goku.
	if !route.Hops[0].Channel.Node.PubKey.IsEqual(aliases["songoku"]) {
		t.Fatalf("first hop should be goku, is instead: %v",
			route.Hops[0].Channel.Node.Alias)
	}

	// WE shoul

	// The second hop should be from goku to sophon.
	if !route.Hops[1].Channel.Node.PubKey.IsEqual(aliases["sophon"]) {
		t.Fatalf("second hop should be sophon, is instead: %v",
			route.Hops[0].Channel.Node.Alias)
	}

	// Next, attempt to query for a path to Luo Ji for 100 satoshis, there
	// exist two possible paths in the graph, but the shorter (1 hop) path
	// should be selected.
	target = aliases["luoji"]
	route, err = findRoute(graph, target, paymentAmt)
	if err != nil {
		t.Fatalf("unable to find route: %v", err)
	}

	// The length of the path should be exactly one hop as it's the
	// "shortest" known path in the graph.
	if len(route.Hops) != 1 {
		t.Fatalf("shortest path not selected, should be of length 1, "+
			"is instead: %v", len(route.Hops))
	}

	// As we have a direct path, the total time lock value should be
	// exactly one.
	if route.TotalTimeLock != 1 {
		t.Fatalf("expected time lock of %v, instead have %v", 1,
			route.TotalTimeLock)
	}

	// Additionally, since this is a single-hop payment, we shouldn't have
	// to pay any fees in total, so the total amount should be the payment
	// amount.
	if route.TotalAmount != paymentAmt {
		t.Fatalf("incorrect total amount, expected %v got %v",
			paymentAmt, route.TotalAmount)
	}
}

func TestNewRoutePathTooLong(t *testing.T) {
	// Ensure that potential paths which are over the maximum hop-limit are
	// rejected.
	graph, cleanUp, aliases, err := parseTestGraph(excessiveHopsGraphFilePath)
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to create graph: %v", err)
	}

	const paymentAmt = btcutil.Amount(100)

	// We start by confirminig that routing a payment 20 hops away is possible.
	// Alice should be able to find a valid route to ursula.
	target := aliases["ursula"]
	route, err := findRoute(graph, target, paymentAmt)
	if err != nil {
		t.Fatalf("path should have been found")
	}

	// Vincent is 21 hops away from Alice, and thus no valid route should be
	// presented to Alice.
	target = aliases["vincent"]
	route, err = findRoute(graph, target, paymentAmt)
	if err == nil {
		t.Fatalf("should not have been able to find path, supposed to be "+
			"greater than 20 hops, found route with %v hops", len(route.Hops))
	}

}

func TestPathNotAvailable(t *testing.T) {
	graph, cleanUp, _, err := parseTestGraph(basicGraphFilePath)
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to create graph: %v", err)
	}

	// With the test graph loaded, we'll test that queries for target that
	// are either unreachable within the graph, or unknown result in an
	// error.
	unknownNodeStr := "03dd46ff29a6941b4a2607525b043ec9b020b3f318a1bf281536fd7011ec59c882"
	unknownNodeBytes, err := hex.DecodeString(unknownNodeStr)
	if err != nil {
		t.Fatalf("unable to parse bytes: %v", err)
	}
	unknownNode, err := btcec.ParsePubKey(unknownNodeBytes, btcec.S256())
	if err != nil {
		t.Fatalf("unable to parse pubkey: %v", err)
	}

	if _, err := findRoute(graph, unknownNode, 100); err != ErrNoPathFound {
		t.Fatalf("path shouldn't have been found: %v", err)
	}
}

func TestPathInsufficientCapacity(t *testing.T) {
	graph, cleanUp, aliases, err := parseTestGraph(basicGraphFilePath)
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to create graph: %v", err)
	}

	// Next, test that attempting to find a path in which the current
	// channel graph cannot support due to insufficient capacity triggers
	// an error.

	// To test his we'll attempt to make a payment of 1 BTC, or 100 million
	// satoshis. The largest channel in the basic graph is of size 100k
	// satoshis, so we shouldn't be able to find a path to sophon even
	// though we have a 2-hop link.
	target := aliases["sophon"]

	const payAmt = btcutil.SatoshiPerBitcoin
	_, err = findRoute(graph, target, payAmt)
	if err != ErrInsufficientCapacity {
		t.Fatalf("graph shouldn't be able to support payment: %v", err)
	}
}

func TestPathInsufficientCapacityWithFee(t *testing.T) {
	// TODO(roasbeef): encode live graph to json
}
