package channeldb

import (
	"bytes"
	"encoding/binary"
	"image/color"
	"io"
	"net"
	"time"

	"github.com/boltdb/bolt"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

var (
	// nodeBucket is a bucket which houses all the vertices or nodes within
	// the channel graph. This bucket has a single-sub bucket which adds an
	// additional index from pubkey -> alias. Within the top-level of this
	// bucket, the key space maps a node's compressed public key to the
	// serialized information for that node. Additionally, there's a
	// special key "source" which stores the pubkey of the source node. The
	// source node is used as the starting point for all graph/queries and
	// traversals. The graph is formed as a star-graph with the source node
	// at the center.
	//
	// maps: pubKey -> nofInfo
	// maps: source -> selfPubKey
	nodeBucket = []byte("graph-node")

	// sourceKey is a special key that resides within the nodeBucket. The
	// sourceKey maps a key to the public key of the "self node".
	sourceKey = []byte("source")

	// aliasIndexBucket is a sub-bucket that's nested within the main
	// nodeBucket. This bucket maps the public key of a node to it's
	// current alias. This bucket is provided as it can be used within a
	// future UI layer to add an additional degree of confirmation.
	aliasIndexBucket = []byte("alias")

	// edgeBucket is a bucket which houses all of the edge or channel
	// information within the channel graph. This bucket essentially acts
	// as an adjacency list, which in conjunction with a range scan, can be
	// used to iterate over all the _outgoing_ edges for a particular node.
	// Key in the bucket use a prefix scheme which leads with the node's
	// public key and sends with the compact edge ID. For each edgeID,
	// there will be two entries within the bucket, as the graph is
	// directed: nodes may have different policies w.r.t to fees for their
	// respective directions.
	//
	// maps: pubKey || edgeID -> edge policy for node
	edgeBucket = []byte("graph-edge")

	// chanStart is an array of all zero bytes which is used to perform
	// range scans within the edgeBucket to obtain all of the outgoing
	// edges for a particular node.
	chanStart [8]byte

	// edgeIndexBucket is an index which can be used to iterate all edges
	// in the bucket, grouping them according to their in/out nodes.
	// Additionally, the items in this bucket also contain the complete
	// edge information for a channel. The edge information includes the
	// capacity of the channel, the nodes that made the channel, etc.  This
	// bucket resides within the edgeBucket above.  Creation of a edge
	// proceeds in two phases: first the edge is added to the edge index,
	// afterwards the edgeBucket can be updated with the latest details of
	// the edge as they are announced on the network.
	//
	// maps: chanID -> pubKey1 || pubKey2 || restofEdgeInfo
	edgeIndexBucket = []byte("edge-index")

	// channelPointBucket maps a channel's full outpoint (txid:index) to
	// its short 8-byte channel ID. This bucket resides within the
	// edgeBucket above, and can be used to quickly remove an edge due to
	// the outpoint being spent, or to query for existence of a channel.
	//
	// maps: outPoint -> chanID
	channelPointBucket = []byte("chan-index")

	// graphMetaBucket is a top-level bucket which stores various meta-deta
	// related to the on-disk channel graph. Data stored in this bucket
	// includes the block to which the graph has been synced to, the total
	// number of channels, etc.
	graphMetaBucket = []byte("graph-meta")

	// pruneTipKey is a key within the above graphMetaBucket that stores
	// the best known blockhash+height that the channel graph has been
	// known to be pruned to. Once a new block is discovered, any channels
	// that have been closed (by spending the outpoint) can safely be
	// removed from the graph.
	pruneTipKey = []byte("prune-tip")

	edgeBloomKey = []byte("edge-bloom")
	nodeBloomKey = []byte("node-bloom")
)

// ChannelGraph is a persistent, on-disk graph representation of the Lightning
// Network. This struct can be used to implement path finding algorithms on top
// of, and also to update a node's view based on information received from the
// p2p network. Internally, the graph is stored using a modified adjacency list
// representation with some added object interaction possible with each
// serialized edge/node. The graph is stored is directed, meaning that are two
// edges stored for each channel: an inbound/outbound edge for each node pair.
// Nodes, edges, and edge information can all be added to the graph
// independently. Edge removal results in the deletion of all edge information
// for that edge.
type ChannelGraph struct {
	db *DB

	// TODO(roasbeef): store and update bloom filter to reduce disk access
	// due to current gossip model
	//  * LRU cache for edges?
}

// addressType specifies the network protocol and version that should be used
// when connecting to a node at a particular address.
type addressType uint8

const (
	tcp4Addr  addressType = 0
	tcp6Addr  addressType = 1
	onionAddr addressType = 2
)

// ForEachChannel iterates through all the channel edges stored within the
// graph and invokes the passed callback for each edge. The callback takes two
// edges as since this is a directed graph, both the in/out edges are visited.
// If the callback returns an error, then the transaction is aborted and the
// iteration stops early.
//
// NOTE: If an edge can't be found, or wasn't advertised, then a nil pointer
// for that particular channel edge routing policy will be passed into the
// callback.
func (c *ChannelGraph) ForEachChannel(cb func(*ChannelEdgeInfo, *ChannelEdgePolicy, *ChannelEdgePolicy) error) error {
	// TODO(roasbeef): ptr map to reduce # of allocs? no duplicates

	return c.db.View(func(tx *bolt.Tx) error {
		// First, grab the node bucket. This will be used to populate
		// the Node pointers in each edge read from disk.
		nodes := tx.Bucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNotFound
		}

		// Next, grab the edge bucket which stores the edges, and also
		// the index itself so we can group the directed edges together
		// logically.
		edges := tx.Bucket(edgeBucket)
		if edges == nil {
			return ErrGraphNoEdgesFound
		}
		edgeIndex := edges.Bucket(edgeIndexBucket)
		if edgeIndex == nil {
			return ErrGraphNoEdgesFound
		}

		// For each edge pair within the edge index, we fetch each edge
		// itself and also the node information in order to fully
		// populated the object.
		return edgeIndex.ForEach(func(chanID, edgeInfoBytes []byte) error {
			infoReader := bytes.NewReader(edgeInfoBytes)
			edgeInfo, err := deserializeChanEdgeInfo(infoReader)
			if err != nil {
				return err
			}

			// The first node is contained within the first half of
			// the edge information.
			node1Pub := edgeInfoBytes[:33]
			edge1, err := fetchChanEdgePolicy(edges, chanID, node1Pub, nodes)
			if err != nil && err != ErrEdgeNotFound &&
				err != ErrGraphNodeNotFound {
				return err
			}

			// The targeted edge may have not been advertised
			// within the network, so we ensure it's non-nil before
			// deferencing its attributes.
			if edge1 != nil {
				edge1.db = c.db
				if edge1.Node != nil {
					edge1.Node.db = c.db
				}
			}

			// Similarly, the second node is contained within the
			// latter half of the edge information.
			node2Pub := edgeInfoBytes[33:]
			edge2, err := fetchChanEdgePolicy(edges, chanID, node2Pub, nodes)
			if err != nil && err != ErrEdgeNotFound &&
				err != ErrGraphNodeNotFound {
				return err
			}

			// The targeted edge may have not been advertised
			// within the network, so we ensure it's non-nil before
			// deferencing its attributes.
			if edge2 != nil {
				edge2.db = c.db
				if edge2.Node != nil {
					edge2.Node.db = c.db
				}
			}

			// With both edges read, execute the call back. IF this
			// function returns an error then the transaction will
			// be aborted.
			return cb(edgeInfo, edge1, edge2)
		})
	})
}

// ForEachNode iterates through all the stored vertices/nodes in the graph,
// executing the passed callback with each node encountered. If the callback
// returns an error, then the transaction is aborted and the iteration stops
// early.
//
// If the caller wishes to re-use an existing boltdb transaction, then it
// should be passed as the first argument.  Otherwise the first argument should
// be nil and a fresh transaction will be created to execute the graph
// traversal
//
// TODO(roasbeef): add iterator interface to allow for memory efficient graph
// traversal when graph gets mega
func (c *ChannelGraph) ForEachNode(tx *bolt.Tx, cb func(*bolt.Tx, *LightningNode) error) error {
	traversal := func(tx *bolt.Tx) error {
		// First grab the nodes bucket which stores the mapping from
		// pubKey to node information.
		nodes := tx.Bucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNotFound
		}

		return nodes.ForEach(func(pubKey, nodeBytes []byte) error {
			// If this is the source key, then we skip this
			// iteration as the value for this key is a pubKey
			// rather than raw node information.
			if bytes.Equal(pubKey, sourceKey) || len(pubKey) != 33 {
				return nil
			}

			nodeReader := bytes.NewReader(nodeBytes)
			node, err := deserializeLightningNode(nodeReader)
			if err != nil {
				return err
			}
			node.db = c.db

			// Execute the callback, the transaction will abort if
			// this returns an error.
			return cb(tx, node)
		})
	}

	// If no transaction was provided, then we'll create a new transaction
	// to execute the transaction within.
	if tx == nil {
		return c.db.View(traversal)
	}

	// Otherwise, we re-use the existing transaction to execute the graph
	// traversal.
	return traversal(tx)
}

// SourceNode returns the source node of the graph. The source node is treated
// as the center node within a star-graph. This method may be used to kick off
// a path finding algorithm in order to explore the reachability of another
// node based off the source node.
func (c *ChannelGraph) SourceNode() (*LightningNode, error) {
	var source *LightningNode
	err := c.db.View(func(tx *bolt.Tx) error {
		// First grab the nodes bucket which stores the mapping from
		// pubKey to node information.
		nodes := tx.Bucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNotFound
		}

		selfPub := nodes.Get(sourceKey)
		if selfPub == nil {
			return ErrSourceNodeNotSet
		}

		// With the pubKey of the source node retrieved, we're able to
		// fetch the full node information.
		node, err := fetchLightningNode(nodes, selfPub)
		if err != nil {
			return err
		}

		source = node
		source.db = c.db
		return nil
	})
	if err != nil {
		return nil, err
	}

	return source, nil
}

// SetSourceNode sets the source node within the graph database. The source
// node is to be used as the center of a star-graph within path finding
// algorithms.
func (c *ChannelGraph) SetSourceNode(node *LightningNode) error {
	nodePub := node.PubKey.SerializeCompressed()
	return c.db.Update(func(tx *bolt.Tx) error {
		// First grab the nodes bucket which stores the mapping from
		// pubKey to node information.
		nodes, err := tx.CreateBucketIfNotExists(nodeBucket)
		if err != nil {
			return err
		}

		// Next we create the mapping from source to the targeted
		// public key.
		if err := nodes.Put(sourceKey, nodePub); err != nil {
			return err
		}

		// Finally, we commit the information of the lightning node
		// itself.
		return addLightningNode(tx, node)
	})
}

// AddLightningNode adds a vertex/node to the graph database. If the node is not
// in the database from before, this will add a new, unconnected one to the
// graph. If it is present from before, this will update that node's
// information. Note that this method is expected to only be called to update
// an already present node from a node annoucement, or to insert a node found
// in a channel update.
//
// TODO(roasbeef): also need sig of announcement
func (c *ChannelGraph) AddLightningNode(node *LightningNode) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		return addLightningNode(tx, node)
	})
}

func addLightningNode(tx *bolt.Tx, node *LightningNode) error {
	nodes, err := tx.CreateBucketIfNotExists(nodeBucket)
	if err != nil {
		return err
	}

	aliases, err := nodes.CreateBucketIfNotExists(aliasIndexBucket)
	if err != nil {
		return err
	}

	return putLightningNode(nodes, aliases, node)
}

// LookupAlias attempts to return the alias as advertised by the target node.
// TODO(roasbeef): currently assumes that aliases are unique...
func (c *ChannelGraph) LookupAlias(pub *btcec.PublicKey) (string, error) {
	var alias string

	err := c.db.View(func(tx *bolt.Tx) error {
		nodes := tx.Bucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNodesNotFound
		}

		aliases := nodes.Bucket(aliasIndexBucket)
		if aliases == nil {
			return ErrGraphNodesNotFound
		}

		nodePub := pub.SerializeCompressed()
		a := aliases.Get(nodePub)
		if a == nil {
			return ErrNodeAliasNotFound
		}

		// TODO(roasbeef): should actually be using the utf-8
		// package...
		alias = string(a)
		return nil
	})
	if err != nil {
		return "", err
	}

	return alias, nil
}

// DeleteLightningNode removes a vertex/node from the database according to the
// node's public key.
func (c *ChannelGraph) DeleteLightningNode(nodePub *btcec.PublicKey) error {
	pub := nodePub.SerializeCompressed()

	// TODO(roasbeef): ensure dangling edges are removed...
	return c.db.Update(func(tx *bolt.Tx) error {
		nodes, err := tx.CreateBucketIfNotExists(nodeBucket)
		if err != nil {
			return err
		}

		aliases, err := tx.CreateBucketIfNotExists(aliasIndexBucket)
		if err != nil {
			return err
		}

		if err := aliases.Delete(pub); err != nil {
			return err
		}
		return nodes.Delete(pub)
	})
}

// AddChannelEdge adds a new (undirected, blank) edge to the graph database. An
// undirected edge from the two target nodes are created. The information
// stored denotes the static attributes of the channel, such as the channelID,
// the keys involved in creation of the channel, and the set of features that
// the channel supports. The chanPoint and chanID are used to uniquely identify
// the edge globally within the database.
func (c *ChannelGraph) AddChannelEdge(edge *ChannelEdgeInfo) error {
	// Construct the channel's primary key which is the 8-byte channel ID.
	var chanKey [8]byte
	binary.BigEndian.PutUint64(chanKey[:], edge.ChannelID)

	return c.db.Update(func(tx *bolt.Tx) error {
		edges, err := tx.CreateBucketIfNotExists(edgeBucket)
		if err != nil {
			return err
		}
		edgeIndex, err := edges.CreateBucketIfNotExists(edgeIndexBucket)
		if err != nil {
			return err
		}
		chanIndex, err := edges.CreateBucketIfNotExists(channelPointBucket)
		if err != nil {
			return err
		}

		// First, attempt to check if this edge has already been
		// created. If so, then we can exit early as this method is
		// meant to be idempotent.
		if edgeInfo := edgeIndex.Get(chanKey[:]); edgeInfo != nil {
			return ErrEdgeAlreadyExist
		}

		// If the edge hasn't been created yet, then we'll first add it
		// to the edge index in order to associate the edge between two
		// nodes and also store the static components of the channel.
		if err := putChanEdgeInfo(edgeIndex, edge, chanKey); err != nil {
			return err
		}

		// Finally we add it to the channel index which maps channel
		// points (outpoints) to the shorter channel ID's.
		var b bytes.Buffer
		if err := writeOutpoint(&b, &edge.ChannelPoint); err != nil {
			return err
		}
		return chanIndex.Put(b.Bytes(), chanKey[:])
	})
}

// HasChannelEdge returns true if the database knows of a channel edge with the
// passed channel ID, and false otherwise. If the an edge with that ID is found
// within the graph, then two time stamps representing the last time the edge
// was updated for both directed edges are returned along with the boolean.
func (c *ChannelGraph) HasChannelEdge(chanID uint64) (time.Time, time.Time, bool, error) {
	// TODO(roasbeef): check internal bloom filter first

	var (
		node1UpdateTime time.Time
		node2UpdateTime time.Time
		exists          bool
	)

	if err := c.db.View(func(tx *bolt.Tx) error {
		edges := tx.Bucket(edgeBucket)
		if edges == nil {
			return ErrGraphNoEdgesFound
		}
		edgeIndex := edges.Bucket(edgeIndexBucket)
		if edgeIndex == nil {
			return ErrGraphNoEdgesFound
		}

		var channelID [8]byte
		byteOrder.PutUint64(channelID[:], chanID)
		if edgeIndex.Get(channelID[:]) == nil {
			exists = false
			return nil
		}

		exists = true

		// If the channel has been found in the graph, then retrieve
		// the edges itself so we can return the last updated
		// timestmaps.
		nodes := tx.Bucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNodeNotFound
		}

		e1, e2, err := fetchChanEdgePolicies(edgeIndex, edges, nodes,
			channelID[:], c.db)
		if err != nil {
			return err
		}

		// As we may have only one of the edges populated, only set the
		// update time if the edge was found in the database.
		if e1 != nil {
			node1UpdateTime = e1.LastUpdate
		}
		if e2 != nil {
			node2UpdateTime = e2.LastUpdate
		}

		return nil
	}); err != nil {
		return time.Time{}, time.Time{}, exists, err
	}

	return node1UpdateTime, node2UpdateTime, exists, nil
}

// UpdateChannelEdge retrieves and update edge of the graph database. Method
// only reserved for updating an edge info after it's already been created.
// In order to maintain this constraints, we return an error in the scenario
// that an edge info hasn't yet been created yet, but someone attempts to update
// it.
func (c *ChannelGraph) UpdateChannelEdge(edge *ChannelEdgeInfo) error {
	// Construct the channel's primary key which is the 8-byte channel ID.
	var chanKey [8]byte
	binary.BigEndian.PutUint64(chanKey[:], edge.ChannelID)

	return c.db.Update(func(tx *bolt.Tx) error {
		edges, err := tx.CreateBucketIfNotExists(edgeBucket)
		if err != nil {
			return err
		}
		edgeIndex, err := edges.CreateBucketIfNotExists(edgeIndexBucket)
		if err != nil {
			return err
		}

		if edgeInfo := edgeIndex.Get(chanKey[:]); edgeInfo == nil {
			return ErrEdgeNotFound
		}

		return putChanEdgeInfo(edgeIndex, edge, chanKey)
	})
}

const (
	// pruneTipBytes is the total size of the value which stores the
	// current prune tip of the graph. The prune tip indicates if the
	// channel graph is in sync with the current UTXO state. The structure
	// is: blockHash || blockHeight, taking 36 bytes total.
	pruneTipBytes = 32 + 4
)

// PruneGraph prunes newly closed channels from the channel graph in response
// to a new block being solved on the network. Any transactions which spend the
// funding output of any known channels within he graph will be deleted.
// Additionally, the "prune tip", or the last block which has been used to
// prune the graph is stored so callers can ensure the graph is fully in sync
// with the current UTXO state. A slice of channels that have been closed by
// the target block are returned if the function succeeds without error.
func (c *ChannelGraph) PruneGraph(spentOutputs []*wire.OutPoint,
	blockHash *chainhash.Hash, blockHeight uint32) ([]*ChannelEdgeInfo, error) {

	var chansClosed []*ChannelEdgeInfo

	err := c.db.Update(func(tx *bolt.Tx) error {
		// First grab the edges bucket which houses the information
		// we'd like to delete
		edges, err := tx.CreateBucketIfNotExists(edgeBucket)
		if err != nil {
			return err
		}

		// Next grab the two edge indexes which will also need to be updated.
		edgeIndex, err := edges.CreateBucketIfNotExists(edgeIndexBucket)
		if err != nil {
			return err
		}
		chanIndex, err := edges.CreateBucketIfNotExists(channelPointBucket)
		if err != nil {
			return err
		}

		// For each of the outpoints that've been spent within the
		// block, we attempt to delete them from the graph as if that
		// outpoint was a channel, then it has now been closed.
		for _, chanPoint := range spentOutputs {
			// TODO(roasbeef): load channel bloom filter, continue
			// if NOT if filter

			var opBytes bytes.Buffer
			if err := writeOutpoint(&opBytes, chanPoint); err != nil {
				return nil
			}

			// First attempt to see if the channel exists within
			// the database, if not, then we can exit early.
			chanID := chanIndex.Get(opBytes.Bytes())
			if chanID == nil {
				continue
			}

			// However, if it does, then we'll read out the full
			// version so we can add it to the set of deleted
			// channels.
			edgeInfo, err := fetchChanEdgeInfo(edgeIndex, chanID)
			if err != nil {
				return err
			}
			chansClosed = append(chansClosed, edgeInfo)

			// Attempt to delete the channel, an ErrEdgeNotFound
			// will be returned if that outpoint isn't known to be
			// a channel. If no error is returned, then a channel
			// was successfully pruned.
			err = delChannelByEdge(edges, edgeIndex, chanIndex,
				chanPoint)
			if err != nil && err != ErrEdgeNotFound {
				return err
			}
		}

		metaBucket, err := tx.CreateBucketIfNotExists(graphMetaBucket)
		if err != nil {
			return err
		}

		// With the graph pruned, update the current "prune tip" which
		// can be used to check if the graph is fully synced with the
		// current UTXO state.
		var newTip [pruneTipBytes]byte
		copy(newTip[:], blockHash[:])
		byteOrder.PutUint32(newTip[32:], blockHeight)

		return metaBucket.Put(pruneTipKey, newTip[:])
	})
	if err != nil {
		return nil, err
	}

	return chansClosed, nil
}

// PruneTip returns the block height and hash of the latest block that has been
// used to prune channels in the graph. Knowing the "prune tip" allows callers
// to tell if the graph is currently in sync with the current best known UTXO
// state.
func (c *ChannelGraph) PruneTip() (*chainhash.Hash, uint32, error) {
	var (
		currentTip [pruneTipBytes]byte
		tipHash    chainhash.Hash
		tipHeight  uint32
	)

	err := c.db.View(func(tx *bolt.Tx) error {
		graphMeta := tx.Bucket(graphMetaBucket)
		if graphMeta == nil {
			return ErrGraphNotFound
		}

		tipBytes := graphMeta.Get(pruneTipKey)
		if tipBytes == nil {
			return ErrGraphNeverPruned
		}
		copy(currentTip[:], tipBytes)

		return nil
	})
	if err != nil {
		return nil, 0, err
	}

	// Once we have the prune tip, the first 32 bytes are the block hash,
	// with the latter 4 bytes being the block height.
	copy(tipHash[:], currentTip[:32])
	tipHeight = byteOrder.Uint32(currentTip[32:])

	return &tipHash, tipHeight, nil
}

// DeleteChannelEdge removes an edge from the database as identified by it's
// funding outpoint. If the edge does not exist within the database, then this
func (c *ChannelGraph) DeleteChannelEdge(chanPoint *wire.OutPoint) error {
	// TODO(roasbeef): possibly delete from node bucket if node has no more
	// channels
	// TODO(roasbeef): don't delete both edges?

	return c.db.Update(func(tx *bolt.Tx) error {
		// First grab the edges bucket which houses the information
		// we'd like to delete
		edges, err := tx.CreateBucketIfNotExists(edgeBucket)
		if err != nil {
			return err
		}
		// Next grab the two edge indexes which will also need to be updated.
		edgeIndex, err := edges.CreateBucketIfNotExists(edgeIndexBucket)
		if err != nil {
			return err
		}
		chanIndex, err := edges.CreateBucketIfNotExists(channelPointBucket)
		if err != nil {
			return err
		}

		return delChannelByEdge(edges, edgeIndex, chanIndex, chanPoint)
	})
}

// ChannelID attempt to lookup the 8-byte compact channel ID which maps to the
// passed channel point (outpoint). If the passed channel doesn't exist within
// the database, then ErrEdgeNotFound is returned.
func (c *ChannelGraph) ChannelID(chanPoint *wire.OutPoint) (uint64, error) {
	var chanID uint64

	var b bytes.Buffer
	if err := writeOutpoint(&b, chanPoint); err != nil {
		return 0, nil
	}

	if err := c.db.View(func(tx *bolt.Tx) error {
		edges := tx.Bucket(edgeBucket)
		if edges == nil {
			return ErrGraphNoEdgesFound
		}
		chanIndex := edges.Bucket(channelPointBucket)
		if edges == nil {
			return ErrGraphNoEdgesFound
		}

		chanIDBytes := chanIndex.Get(b.Bytes())
		if chanIDBytes == nil {
			return ErrEdgeNotFound
		}

		chanID = byteOrder.Uint64(chanIDBytes)

		return nil
	}); err != nil {
		return 0, err
	}

	return chanID, nil
}

func delChannelByEdge(edges *bolt.Bucket, edgeIndex *bolt.Bucket,
	chanIndex *bolt.Bucket, chanPoint *wire.OutPoint) error {
	var b bytes.Buffer
	if err := writeOutpoint(&b, chanPoint); err != nil {
		return err
	}

	// If the channel's outpoint doesn't exist within the outpoint
	// index, then the edge does not exist.
	chanID := chanIndex.Get(b.Bytes())
	if chanID == nil {
		return ErrEdgeNotFound
	}

	// Otherwise we obtain the two public keys from the mapping:
	// chanID -> pubKey1 || pubKey2. With this, we can construct
	// the keys which house both of the directed edges for this
	// channel.
	nodeKeys := edgeIndex.Get(chanID)

	// The edge key is of the format pubKey || chanID. First we
	// construct the latter half, populating the channel ID.
	var edgeKey [33 + 8]byte
	copy(edgeKey[33:], chanID)

	// With the latter half constructed, copy over the first public
	// key to delete the edge in this direction, then the second to
	// delete the edge in the opposite direction.
	copy(edgeKey[:33], nodeKeys[:33])
	if edges.Get(edgeKey[:]) != nil {
		if err := edges.Delete(edgeKey[:]); err != nil {
			return err
		}
	}
	copy(edgeKey[:33], nodeKeys[33:])
	if edges.Get(edgeKey[:]) != nil {
		if err := edges.Delete(edgeKey[:]); err != nil {
			return err
		}
	}

	// Finally, with the edge data deleted, we can purge the
	// information from the two edge indexes.
	if err := edgeIndex.Delete(chanID); err != nil {
		return err
	}
	return chanIndex.Delete(b.Bytes())
}

// UpdateEdgePolicy updates the edge routing policy for a single directed edge
// within the database for the referenced channel. The `flags` attribute within
// the ChannelEdgePolicy determines which of the directed edges are being
// updated. If the flag is 1, then the first node's information is being
// updated, otherwise it's the second node's information. The node ordering is
// determined tby the lexicographical ordering of the identity public keys of
// the nodes on either side of the channel.
func (c *ChannelGraph) UpdateEdgePolicy(edge *ChannelEdgePolicy) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		edges, err := tx.CreateBucketIfNotExists(edgeBucket)
		if err != nil {
			return err
		}
		edgeIndex, err := edges.CreateBucketIfNotExists(edgeIndexBucket)
		if err != nil {
			return err
		}

		// Create the channelID key be converting the channel ID
		// integer into a byte slice.
		var chanID [8]byte
		byteOrder.PutUint64(chanID[:], edge.ChannelID)

		// With the channel ID, we then fetch the value storing the two
		// nodes which connect this channel edge.
		nodeInfo := edgeIndex.Get(chanID[:])
		if nodeInfo == nil {
			return ErrEdgeNotFound
		}

		// Depending on the flags value passed above, either the first
		// or second edge policy is being updated.
		var fromNode, toNode []byte
		if edge.Flags == 0 {
			fromNode = nodeInfo[:33]
			toNode = nodeInfo[33:67]
		} else {
			fromNode = nodeInfo[33:67]
			toNode = nodeInfo[:33]
		}

		// Finally, with the direction of the edge being updated
		// identified, we update the on-disk edge representation.
		return putChanEdgePolicy(edges, edge, fromNode, toNode)
	})
}

// LightningNode represents an individual vertex/node within the channel graph.
// A node is connected to other nodes by one or more channel edges emanating
// from it. As the graph is directed, a node will also have an incoming edge
// attached to it for each outgoing edge.
type LightningNode struct {
	// PubKey is the node's long-term identity public key. This key will be
	// used to authenticated any advertisements/updates sent by the node.
	PubKey *btcec.PublicKey

	// HaveNodeAnnouncement indicates whether we received a node annoucement
	// for this particular node. If true, the remaining fields will be set,
	// if false only the PubKey is known for this node.
	HaveNodeAnnouncement bool

	// LastUpdate is the last time the vertex information for this node has
	// been updated.
	LastUpdate time.Time

	// Address is the TCP address this node is reachable over.
	Addresses []net.Addr

	// Color is the selected color for the node.
	Color color.RGBA

	// Alias is a nick-name for the node. The alias can be used to confirm
	// a node's identity or to serve as a short ID for an address book.
	Alias string

	// AuthSig is a signature under the advertised public key which serves
	// to authenticate the attributes announced by this node.
	//
	// TODO(roasbeef): hook into serialization once full verification is in
	AuthSig *btcec.Signature

	// Features is the list of protocol features supported by this node.
	Features *lnwire.FeatureVector

	db *DB

	// TODO(roasbeef): discovery will need storage to keep it's last IP
	// address and re-announce if interface changes?

	// TODO(roasbeef): add update method and fetch?
}

// FetchLightningNode attempts to look up a target node by its identity public
// key. If the node isn't found in the database, then ErrGraphNodeNotFound is
// returned.
func (c *ChannelGraph) FetchLightningNode(pub *btcec.PublicKey) (*LightningNode, error) {
	var node *LightningNode
	nodePub := pub.SerializeCompressed()
	err := c.db.View(func(tx *bolt.Tx) error {
		// First grab the nodes bucket which stores the mapping from
		// pubKey to node information.
		nodes := tx.Bucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNotFound
		}

		// If a key for this serialized public key isn't found, then
		// the target node doesn't exist within the database.
		nodeBytes := nodes.Get(nodePub)
		if nodeBytes == nil {
			return ErrGraphNodeNotFound
		}

		// If the node is found, then we can de deserialize the node
		// information to return to the user.
		nodeReader := bytes.NewReader(nodeBytes)
		n, err := deserializeLightningNode(nodeReader)
		if err != nil {
			return err
		}
		n.db = c.db

		node = n

		return nil
	})
	if err != nil {
		return nil, err
	}

	return node, nil
}

// HasLightningNode determines if the graph has a vertex identified by the
// target node identity public key. If the node exists in the database, a
// timestamp of when the data for the node was lasted updated is returned along
// with a true boolean. Otherwise, an empty time.Time is returned with a false
// boolean.
func (c *ChannelGraph) HasLightningNode(pub *btcec.PublicKey) (time.Time, bool, error) {
	var (
		updateTime time.Time
		exists     bool
	)

	nodePub := pub.SerializeCompressed()
	err := c.db.View(func(tx *bolt.Tx) error {
		// First grab the nodes bucket which stores the mapping from
		// pubKey to node information.
		nodes := tx.Bucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNotFound
		}

		// If a key for this serialized public key isn't found, we can
		// exit early.
		nodeBytes := nodes.Get(nodePub)
		if nodeBytes == nil {
			exists = false
			return nil
		}

		// Otherwise we continue on to obtain the time stamp
		// representing the last time the data for this node was
		// updated.
		nodeReader := bytes.NewReader(nodeBytes)
		node, err := deserializeLightningNode(nodeReader)
		if err != nil {
			return err
		}

		exists = true
		updateTime = node.LastUpdate
		return nil
	})
	if err != nil {
		return time.Time{}, exists, nil
	}

	return updateTime, exists, nil
}

// ForEachChannel iterates through all the outgoing channel edges from this
// node, executing the passed callback with each edge as its sole argument. The
// first edge policy is the outgoing edge *to* the connecting node, while the
// second is the incoming edge *from* the connecting node. If the callback
// returns an error, then the iteration is halted with the error propagated
// back up to the caller.
//
// If the caller wishes to re-use an existing boltdb transaction, then it
// should be passed as the first argument.  Otherwise the first argument should
// be nil and a fresh transaction will be created to execute the graph
// traversal.
func (l *LightningNode) ForEachChannel(tx *bolt.Tx,
	cb func(*bolt.Tx, *ChannelEdgeInfo, *ChannelEdgePolicy, *ChannelEdgePolicy) error) error {

	nodePub := l.PubKey.SerializeCompressed()

	traversal := func(tx *bolt.Tx) error {
		nodes := tx.Bucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNotFound
		}
		edges := tx.Bucket(edgeBucket)
		if edges == nil {
			return ErrGraphNotFound
		}
		edgeIndex := edges.Bucket(edgeIndexBucket)
		if edgeIndex == nil {
			return ErrGraphNoEdgesFound
		}

		// In order to reach all the edges for this node, we take
		// advantage of the construction of the key-space within the
		// edge bucket. The keys are stored in the form: pubKey ||
		// chanID. Therefore, starting from a chanID of zero, we can
		// scan forward in the bucket, grabbing all the edges for the
		// node. Once the prefix no longer matches, then we know we're
		// done.
		var nodeStart [33 + 8]byte
		copy(nodeStart[:], nodePub)
		copy(nodeStart[33:], chanStart[:])

		// Starting from the key pubKey || 0, we seek forward in the
		// bucket until the retrieved key no longer has the public key
		// as its prefix. This indicates that we've stepped over into
		// another node's edges, so we can terminate our scan.
		edgeCursor := edges.Cursor()
		for nodeEdge, edgeInfo := edgeCursor.Seek(nodeStart[:]); bytes.HasPrefix(nodeEdge, nodePub); nodeEdge, edgeInfo = edgeCursor.Next() {
			// If the prefix still matches, then the value is the
			// raw edge information. So we can now serialize the
			// edge info and fetch the outgoing node in order to
			// retrieve the full channel edge.
			edgeReader := bytes.NewReader(edgeInfo)
			toEdgePolicy, err := deserializeChanEdgePolicy(edgeReader, nodes)
			if err != nil {
				return err
			}
			toEdgePolicy.db = l.db
			toEdgePolicy.Node.db = l.db

			chanID := nodeEdge[33:]
			edgeInfo, err := fetchChanEdgeInfo(edgeIndex, chanID)
			if err != nil {
				return err
			}

			// We'll also fetch the incoming edge so this
			// information can be available to the caller.
			incomingNode := toEdgePolicy.Node.PubKey.SerializeCompressed()
			fromEdgePolicy, err := fetchChanEdgePolicy(
				edges, chanID, incomingNode, nodes,
			)
			if err != nil && err != ErrEdgeNotFound &&
				err != ErrGraphNodeNotFound {
				return err
			}
			if fromEdgePolicy != nil {
				fromEdgePolicy.db = l.db
				if fromEdgePolicy.Node != nil {
					fromEdgePolicy.Node.db = l.db
				}
			}

			// Finally, we execute the callback.
			err = cb(tx, edgeInfo, toEdgePolicy, fromEdgePolicy)
			if err != nil {
				return err
			}
		}

		return nil
	}

	// If no transaction was provided, then we'll create a new transaction
	// to execute the transaction within.
	if tx == nil {
		return l.db.View(traversal)
	}

	// Otherwise, we re-use the existing transaction to execute the graph
	// traversal.
	return traversal(tx)
}

// ChannelEdgeInfo represents a fully authenticated channel along with all its
// unique attributes. Once an authenticated channel announcement has been
// processed on the network, then a instance of ChannelEdgeInfo encapsulating
// the channels attributes is stored. The other portions relevant to routing
// policy of a channel are stored within a ChannelEdgePolicy for each direction
// of the channel.
type ChannelEdgeInfo struct {
	// ChannelID is the unique channel ID for the channel. The first 3
	// bytes are the block height, the next 3 the index within the block,
	// and the last 2 bytes are the output index for the channel.
	ChannelID uint64

	// ChainHash is the hash that uniquely identifies the chain that this
	// channel was opened within.
	//
	// TODO(roasbeef): need to modify db keying for multi-chain
	//  * must add chain hash to prefix as well
	ChainHash chainhash.Hash

	// NodeKey1 is the identity public key of the "first" node that was
	// involved in the creation of this channel. A node is considered
	// "first" if the lexicographical ordering the its serialized public
	// key is "smaller" than that of the other node involved in channel
	// creation.
	NodeKey1 *btcec.PublicKey

	// NodeKey2 is the identity public key of the "second" node that was
	// involved in the creation of this channel. A node is considered
	// "second" if the lexicographical ordering the its serialized public
	// key is "larger" than that of the other node involved in channel
	// creation.
	NodeKey2 *btcec.PublicKey

	// BitcoinKey1 is the Bitcoin multi-sig key belonging to the first
	// node, that was involved in the funding transaction that originally
	// created the channel that this struct represents.
	BitcoinKey1 *btcec.PublicKey

	// BitcoinKey2 is the Bitcoin multi-sig key belonging to the second
	// node, that was involved in the funding transaction that originally
	// created the channel that this struct represents.
	BitcoinKey2 *btcec.PublicKey

	// Features is an opaque byte slice that encodes the set of channel
	// specific features that this channel edge supports.
	Features []byte

	// AuthProof is the authentication proof for this channel. This proof
	// contains a set of signatures binding four identities, which attests
	// to the legitimacy of the advertised channel.
	AuthProof *ChannelAuthProof

	// ChannelPoint is the funding outpoint of the channel. This can be
	// used to uniquely identify the channel within the channel graph.
	ChannelPoint wire.OutPoint

	// Capacity is the total capacity of the channel, this is determined by
	// the value output in the outpoint that created this channel.
	Capacity btcutil.Amount
}

// ChannelAuthProof is the authentication proof (the signature portion) for a
// channel. Using the four signatures contained in the struct, and some
// axillary knowledge (the funding script, node identities, and outpoint) nodes
// on the network are able to validate the authenticity and existence of a
// channel. Each of these signatures signs the following digest: chanID ||
// nodeID1 || nodeID2 || bitcoinKey1|| bitcoinKey2 || 2-byte-feature-len ||
// features.
type ChannelAuthProof struct {
	// NodeSig1 is the signature using the identity key of the node that is
	// first in a lexicographical ordering of the serialized public keys of
	// the two nodes that created the channel.
	NodeSig1 *btcec.Signature

	// NodeSig2 is the signature using the identity key of the node that is
	// second in a lexicographical ordering of the serialized public keys
	// of the two nodes that created the channel.
	NodeSig2 *btcec.Signature

	// BitcoinSig1 is the signature using the public key of the first node
	// that was used in the channel's multi-sig output.
	BitcoinSig1 *btcec.Signature

	// BitcoinSig2 is the signature using the public key of the second node
	// that was used in the channel's multi-sig output.
	BitcoinSig2 *btcec.Signature
}

// IsEmpty check is the authentication proof is empty Proof is empty if at
// least one of the signatures are equal to nil.
func (p *ChannelAuthProof) IsEmpty() bool {
	return p.NodeSig1 == nil ||
		p.NodeSig2 == nil ||
		p.BitcoinSig1 == nil ||
		p.BitcoinSig2 == nil
}

// ChannelEdgePolicy represents a *directed* edge within the channel graph. For
// each channel in the database, there are two distinct edges: one for each
// possible direction of travel along the channel. The edges themselves hold
// information concerning fees, and minimum time-lock information which is
// utilized during path finding.
type ChannelEdgePolicy struct {
	// Signature is a channel announcement signature, which is needed for
	// proper edge policy announcement.
	Signature *btcec.Signature

	// ChannelID is the unique channel ID for the channel. The first 3
	// bytes are the block height, the next 3 the index within the block,
	// and the last 2 bytes are the output index for the channel.
	ChannelID uint64

	// LastUpdate is the last time an authenticated edge for this channel
	// was received.
	LastUpdate time.Time

	// Flags is a bitfield which signals the capabilities of the channel as
	// well as the directed edge this update applies to.
	// TODO(roasbeef):  make into wire struct
	Flags uint16

	// TimeLockDelta is the number of blocks this node will subtract from
	// the expiry of an incoming HTLC. This value expresses the time buffer
	// the node would like to HTLC exchanges.
	TimeLockDelta uint16

	// MinHTLC is the smallest value HTLC this node will accept, expressed
	// in millisatoshi.
	MinHTLC lnwire.MilliSatoshi

	// FeeBaseMSat is the base HTLC fee that will be charged for forwarding
	// ANY HTLC, expressed in mSAT's.
	FeeBaseMSat lnwire.MilliSatoshi

	// FeeProportionalMillionths is the rate that the node will charge for
	// HTLCs for each millionth of a satoshi forwarded.
	FeeProportionalMillionths lnwire.MilliSatoshi

	// Node is the LightningNode that this directed edge leads to. Using
	// this pointer the channel graph can further be traversed.
	Node *LightningNode

	db *DB
}

// FetchChannelEdgesByOutpoint attempts to lookup the two directed edges for
// the channel identified by the funding outpoint. If the channel can't be
// found, then ErrEdgeNotFound is returned. A struct which houses the general
// information for the channel itself is returned as well as two structs that
// contain the routing policies for the channel in either direction.
func (c *ChannelGraph) FetchChannelEdgesByOutpoint(op *wire.OutPoint) (*ChannelEdgeInfo, *ChannelEdgePolicy, *ChannelEdgePolicy, error) {

	var (
		edgeInfo *ChannelEdgeInfo
		policy1  *ChannelEdgePolicy
		policy2  *ChannelEdgePolicy
	)

	err := c.db.Update(func(tx *bolt.Tx) error {
		// First, grab the node bucket. This will be used to populate
		// the Node pointers in each edge read from disk.
		nodes, err := tx.CreateBucketIfNotExists(nodeBucket)
		if err != nil {
			return err
		}

		// Next, grab the edge bucket which stores the edges, and also
		// the index itself so we can group the directed edges together
		// logically.
		edges, err := tx.CreateBucketIfNotExists(edgeBucket)
		if err != nil {
			return err
		}
		edgeIndex, err := edges.CreateBucketIfNotExists(edgeIndexBucket)
		if err != nil {
			return err
		}

		// If the channel's outpoint doesn't exist within the outpoint
		// index, then the edge does not exist.
		chanIndex, err := edges.CreateBucketIfNotExists(channelPointBucket)
		if err != nil {
			return err
		}
		var b bytes.Buffer
		if err := writeOutpoint(&b, op); err != nil {
			return err
		}
		chanID := chanIndex.Get(b.Bytes())
		if chanID == nil {
			return ErrEdgeNotFound
		}

		// If the channel is found to exists, then we'll first retrieve
		// the general information for the channel.
		edge, err := fetchChanEdgeInfo(edgeIndex, chanID)
		if err != nil {
			return err
		}
		edgeInfo = edge

		// Once we have the information about the channels' parameters,
		// we'll fetch the routing policies for each for the directed
		// edges.
		e1, e2, err := fetchChanEdgePolicies(edgeIndex, edges, nodes,
			chanID, c.db)
		if err != nil {
			return err
		}

		policy1 = e1
		policy2 = e2
		return nil
	})
	if err != nil {
		return nil, nil, nil, err
	}

	return edgeInfo, policy1, policy2, nil
}

// FetchChannelEdgesByID attempts to lookup the two directed edges for the
// channel identified by the channel ID. If the channel can't be found, then
// ErrEdgeNotFound is returned. A struct which houses the general information
// for the channel itself is returned as well as two structs that contain the
// routing policies for the channel in either direction.
func (c *ChannelGraph) FetchChannelEdgesByID(chanID uint64) (*ChannelEdgeInfo, *ChannelEdgePolicy, *ChannelEdgePolicy, error) {

	var (
		edgeInfo  *ChannelEdgeInfo
		policy1   *ChannelEdgePolicy
		policy2   *ChannelEdgePolicy
		channelID [8]byte
	)

	err := c.db.View(func(tx *bolt.Tx) error {
		// First, grab the node bucket. This will be used to populate
		// the Node pointers in each edge read from disk.
		nodes := tx.Bucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNotFound
		}

		// Next, grab the edge bucket which stores the edges, and also
		// the index itself so we can group the directed edges together
		// logically.
		edges := tx.Bucket(edgeBucket)
		if edges == nil {
			return ErrGraphNoEdgesFound
		}
		edgeIndex := edges.Bucket(edgeIndexBucket)
		if edgeIndex == nil {
			return ErrGraphNoEdgesFound
		}

		byteOrder.PutUint64(channelID[:], chanID)

		edge, err := fetchChanEdgeInfo(edgeIndex, channelID[:])
		if err != nil {
			return err
		}
		edgeInfo = edge

		e1, e2, err := fetchChanEdgePolicies(edgeIndex, edges, nodes,
			channelID[:], c.db)
		if err != nil {
			return err
		}

		policy1 = e1
		policy2 = e2
		return nil
	})
	if err != nil {
		return nil, nil, nil, err
	}

	return edgeInfo, policy1, policy2, nil
}

// ChannelView returns the verifiable edge information for each active channel
// within the known channel graph. The set of UTXO's returned are the ones that
// need to be watched on chain to detect channel closes on the resident
// blockchain.
func (c *ChannelGraph) ChannelView() ([]wire.OutPoint, error) {
	var chanPoints []wire.OutPoint
	if err := c.db.View(func(tx *bolt.Tx) error {
		// We're going to iterate over the entire channel index, so
		// we'll need to fetch the edgeBucket to get to the index as
		// it's a sub-bucket.
		edges := tx.Bucket(edgeBucket)
		if edges == nil {
			return ErrGraphNoEdgesFound
		}
		chanIndex := edges.Bucket(channelPointBucket)
		if chanIndex == nil {
			return ErrGraphNoEdgesFound
		}

		// Once we have the proper bucket, we'll range over each key
		// (which is the channel point for the channel) and decode it,
		// accumulating each entry.
		return chanIndex.ForEach(func(chanPointBytes, _ []byte) error {
			chanPointReader := bytes.NewReader(chanPointBytes)

			var chanPoint wire.OutPoint
			err := readOutpoint(chanPointReader, &chanPoint)
			if err != nil {
				return err
			}

			chanPoints = append(chanPoints, chanPoint)
			return nil
		})
	}); err != nil {
		return nil, err
	}

	return chanPoints, nil
}

// NewChannelEdgePolicy returns a new blank ChannelEdgePolicy.
func (c *ChannelGraph) NewChannelEdgePolicy() *ChannelEdgePolicy {
	return &ChannelEdgePolicy{db: c.db}
}

func putLightningNode(nodeBucket *bolt.Bucket, aliasBucket *bolt.Bucket, node *LightningNode) error {
	var (
		scratch [16]byte
		b       bytes.Buffer
	)

	nodePub := node.PubKey.SerializeCompressed()

	// If the node has the update time set, write it, else write 0.
	updateUnix := uint64(0)
	if node.LastUpdate.Unix() > 0 {
		updateUnix = uint64(node.LastUpdate.Unix())
	}

	byteOrder.PutUint64(scratch[:8], updateUnix)
	if _, err := b.Write(scratch[:8]); err != nil {
		return err
	}

	if _, err := b.Write(nodePub); err != nil {
		return err
	}

	// If we got a node announcement for this node, we will have the rest of
	// the data available. If not we don't have more data to write.
	if !node.HaveNodeAnnouncement {
		// Write HaveNodeAnnouncement=0.
		byteOrder.PutUint16(scratch[:2], 0)
		if _, err := b.Write(scratch[:2]); err != nil {
			return err
		}

		return nodeBucket.Put(nodePub, b.Bytes())
	}

	// Write HaveNodeAnnouncement=1.
	byteOrder.PutUint16(scratch[:2], 1)
	if _, err := b.Write(scratch[:2]); err != nil {
		return err
	}

	if err := binary.Write(&b, byteOrder, node.Color.R); err != nil {
		return err
	}
	if err := binary.Write(&b, byteOrder, node.Color.G); err != nil {
		return err
	}
	if err := binary.Write(&b, byteOrder, node.Color.B); err != nil {
		return err
	}

	if err := wire.WriteVarString(&b, 0, node.Alias); err != nil {
		return err
	}

	if err := node.Features.Encode(&b); err != nil {
		return err
	}

	numAddresses := uint16(len(node.Addresses))
	byteOrder.PutUint16(scratch[:2], numAddresses)
	if _, err := b.Write(scratch[:2]); err != nil {
		return err
	}

	for _, address := range node.Addresses {
		if address.Network() == "tcp" {
			if address.(*net.TCPAddr).IP.To4() != nil {
				scratch[0] = uint8(tcp4Addr)
				if _, err := b.Write(scratch[:1]); err != nil {
					return err
				}
				copy(scratch[:4], address.(*net.TCPAddr).IP.To4())
				if _, err := b.Write(scratch[:4]); err != nil {
					return err
				}
			} else {
				scratch[0] = uint8(tcp6Addr)
				if _, err := b.Write(scratch[:1]); err != nil {
					return err
				}
				copy(scratch[:], address.(*net.TCPAddr).IP.To16())
				if _, err := b.Write(scratch[:]); err != nil {
					return err
				}
			}
			byteOrder.PutUint16(scratch[:2],
				uint16(address.(*net.TCPAddr).Port))
			if _, err := b.Write(scratch[:2]); err != nil {
				return err
			}
		}
	}

	err := wire.WriteVarBytes(&b, 0, node.AuthSig.Serialize())
	if err != nil {
		return err
	}

	if err := aliasBucket.Put(nodePub, []byte(node.Alias)); err != nil {
		return err
	}

	return nodeBucket.Put(nodePub, b.Bytes())

}

func fetchLightningNode(nodeBucket *bolt.Bucket,
	nodePub []byte) (*LightningNode, error) {

	nodeBytes := nodeBucket.Get(nodePub)
	if nodeBytes == nil {
		return nil, ErrGraphNodeNotFound
	}

	nodeReader := bytes.NewReader(nodeBytes)
	return deserializeLightningNode(nodeReader)
}

func deserializeLightningNode(r io.Reader) (*LightningNode, error) {
	node := &LightningNode{}
	var scratch [8]byte

	if _, err := r.Read(scratch[:]); err != nil {
		return nil, err
	}

	unix := int64(byteOrder.Uint64(scratch[:]))
	node.LastUpdate = time.Unix(unix, 0)

	var pub [33]byte
	if _, err := r.Read(pub[:]); err != nil {
		return nil, err
	}
	var err error
	node.PubKey, err = btcec.ParsePubKey(pub[:], btcec.S256())
	if err != nil {
		return nil, err
	}

	if _, err := r.Read(scratch[:2]); err != nil {
		return nil, err
	}

	hasNodeAnn := byteOrder.Uint16(scratch[:2])
	if hasNodeAnn == 1 {
		node.HaveNodeAnnouncement = true
	} else {
		node.HaveNodeAnnouncement = false
	}

	// The rest of the data is optional, and will only be there if we got a node
	// announcement for this node.
	if !node.HaveNodeAnnouncement {
		return node, nil
	}

	// We did get a node announcement for this node, so we'll have the rest
	// of the data available.
	if err := binary.Read(r, byteOrder, &node.Color.R); err != nil {
		return nil, err
	}
	if err := binary.Read(r, byteOrder, &node.Color.G); err != nil {
		return nil, err
	}
	if err := binary.Read(r, byteOrder, &node.Color.B); err != nil {
		return nil, err
	}

	node.Alias, err = wire.ReadVarString(r, 0)
	if err != nil {
		return nil, err
	}

	node.Features, err = lnwire.NewFeatureVectorFromReader(r)
	if err != nil {
		return nil, err
	}

	if _, err := r.Read(scratch[:2]); err != nil {
		return nil, err
	}
	numAddresses := int(byteOrder.Uint16(scratch[:2]))

	var addresses []net.Addr
	for i := 0; i < numAddresses; i++ {
		var address net.Addr
		if _, err := r.Read(scratch[:1]); err != nil {
			return nil, err
		}

		// TODO(roasbeef): also add onion addrs
		switch addressType(scratch[0]) {
		case tcp4Addr:
			addr := &net.TCPAddr{}
			var ip [4]byte
			if _, err := r.Read(ip[:]); err != nil {
				return nil, err
			}
			addr.IP = (net.IP)(ip[:])
			if _, err := r.Read(scratch[:2]); err != nil {
				return nil, err
			}
			addr.Port = int(byteOrder.Uint16(scratch[:2]))
			address = addr
		case tcp6Addr:
			addr := &net.TCPAddr{}
			var ip [16]byte
			if _, err := r.Read(ip[:]); err != nil {
				return nil, err
			}
			addr.IP = (net.IP)(ip[:])
			if _, err := r.Read(scratch[:2]); err != nil {
				return nil, err
			}
			addr.Port = int(byteOrder.Uint16(scratch[:2]))
			address = addr
		default:
			return nil, ErrUnknownAddressType
		}

		addresses = append(addresses, address)
	}
	node.Addresses = addresses

	sigBytes, err := wire.ReadVarBytes(r, 0, 80, "sig")
	if err != nil {
		return nil, err
	}

	node.AuthSig, err = btcec.ParseSignature(sigBytes, btcec.S256())
	if err != nil {
		return nil, err
	}

	return node, nil
}

func putChanEdgeInfo(edgeIndex *bolt.Bucket, edgeInfo *ChannelEdgeInfo, chanID [8]byte) error {
	var b bytes.Buffer

	if _, err := b.Write(edgeInfo.NodeKey1.SerializeCompressed()); err != nil {
		return err
	}
	if _, err := b.Write(edgeInfo.NodeKey2.SerializeCompressed()); err != nil {
		return err
	}
	if _, err := b.Write(edgeInfo.BitcoinKey1.SerializeCompressed()); err != nil {
		return err
	}
	if _, err := b.Write(edgeInfo.BitcoinKey2.SerializeCompressed()); err != nil {
		return err
	}

	if err := wire.WriteVarBytes(&b, 0, edgeInfo.Features); err != nil {
		return err
	}

	authProof := edgeInfo.AuthProof
	var nodeSig1, nodeSig2, bitcoinSig1, bitcoinSig2 []byte
	if authProof != nil {
		nodeSig1 = authProof.NodeSig1.Serialize()
		nodeSig2 = authProof.NodeSig2.Serialize()
		bitcoinSig1 = authProof.BitcoinSig1.Serialize()
		bitcoinSig2 = authProof.BitcoinSig2.Serialize()
	}

	if err := wire.WriteVarBytes(&b, 0, nodeSig1); err != nil {
		return err
	}
	if err := wire.WriteVarBytes(&b, 0, nodeSig2); err != nil {
		return err
	}
	if err := wire.WriteVarBytes(&b, 0, bitcoinSig1); err != nil {
		return err
	}
	if err := wire.WriteVarBytes(&b, 0, bitcoinSig2); err != nil {
		return err
	}

	if err := writeOutpoint(&b, &edgeInfo.ChannelPoint); err != nil {
		return err
	}
	if err := binary.Write(&b, byteOrder, uint64(edgeInfo.Capacity)); err != nil {
		return err
	}
	if _, err := b.Write(chanID[:]); err != nil {
		return err
	}
	if _, err := b.Write(edgeInfo.ChainHash[:]); err != nil {
		return err
	}

	return edgeIndex.Put(chanID[:], b.Bytes())
}

func fetchChanEdgeInfo(edgeIndex *bolt.Bucket,
	chanID []byte) (*ChannelEdgeInfo, error) {

	edgeInfoBytes := edgeIndex.Get(chanID)
	if edgeInfoBytes == nil {
		return nil, ErrEdgeNotFound
	}

	edgeInfoReader := bytes.NewReader(edgeInfoBytes)
	return deserializeChanEdgeInfo(edgeInfoReader)
}

func deserializeChanEdgeInfo(r io.Reader) (*ChannelEdgeInfo, error) {
	var (
		err         error
		pubKeyBytes [33]byte
		edgeInfo    = &ChannelEdgeInfo{}
	)

	readKey := func() (*btcec.PublicKey, error) {
		if _, err := io.ReadFull(r, pubKeyBytes[:]); err != nil {
			return nil, err
		}

		return btcec.ParsePubKey(pubKeyBytes[:], btcec.S256())
	}

	edgeInfo.NodeKey1, err = readKey()
	if err != nil {
		return nil, err
	}
	edgeInfo.NodeKey2, err = readKey()
	if err != nil {
		return nil, err
	}
	edgeInfo.BitcoinKey1, err = readKey()
	if err != nil {
		return nil, err
	}
	edgeInfo.BitcoinKey2, err = readKey()
	if err != nil {
		return nil, err
	}

	edgeInfo.Features, err = wire.ReadVarBytes(r, 0, 900, "features")
	if err != nil {
		return nil, err
	}

	proof := &ChannelAuthProof{}

	readSig := func() (*btcec.Signature, error) {
		sigBytes, err := wire.ReadVarBytes(r, 0, 80, "sigs")
		if err != nil {
			return nil, err
		}

		if len(sigBytes) != 0 {
			return btcec.ParseSignature(sigBytes, btcec.S256())
		}

		return nil, nil
	}

	proof.NodeSig1, err = readSig()
	if err != nil {
		return nil, err
	}
	proof.NodeSig2, err = readSig()
	if err != nil {
		return nil, err
	}
	proof.BitcoinSig1, err = readSig()
	if err != nil {
		return nil, err
	}
	proof.BitcoinSig2, err = readSig()
	if err != nil {
		return nil, err
	}

	if !proof.IsEmpty() {
		edgeInfo.AuthProof = proof
	}

	edgeInfo.ChannelPoint = wire.OutPoint{}
	if err := readOutpoint(r, &edgeInfo.ChannelPoint); err != nil {
		return nil, err
	}
	if err := binary.Read(r, byteOrder, &edgeInfo.Capacity); err != nil {
		return nil, err
	}
	if err := binary.Read(r, byteOrder, &edgeInfo.ChannelID); err != nil {
		return nil, err
	}

	if _, err := io.ReadFull(r, edgeInfo.ChainHash[:]); err != nil {
		return nil, err
	}

	return edgeInfo, nil
}

func putChanEdgePolicy(edges *bolt.Bucket, edge *ChannelEdgePolicy, from, to []byte) error {
	var edgeKey [33 + 8]byte
	copy(edgeKey[:], from)
	byteOrder.PutUint64(edgeKey[33:], edge.ChannelID)

	var b bytes.Buffer

	err := wire.WriteVarBytes(&b, 0, edge.Signature.Serialize())
	if err != nil {
		return err
	}

	if err := binary.Write(&b, byteOrder, edge.ChannelID); err != nil {
		return err
	}

	var scratch [8]byte
	updateUnix := uint64(edge.LastUpdate.Unix())
	byteOrder.PutUint64(scratch[:], updateUnix)
	if _, err := b.Write(scratch[:]); err != nil {
		return err
	}

	if err := binary.Write(&b, byteOrder, edge.Flags); err != nil {
		return err
	}
	if err := binary.Write(&b, byteOrder, edge.TimeLockDelta); err != nil {
		return err
	}
	if err := binary.Write(&b, byteOrder, uint64(edge.MinHTLC)); err != nil {
		return err
	}
	if err := binary.Write(&b, byteOrder, uint64(edge.FeeBaseMSat)); err != nil {
		return err
	}
	if err := binary.Write(&b, byteOrder, uint64(edge.FeeProportionalMillionths)); err != nil {
		return err
	}

	if _, err := b.Write(to); err != nil {
		return err
	}

	return edges.Put(edgeKey[:], b.Bytes()[:])
}

func fetchChanEdgePolicy(edges *bolt.Bucket, chanID []byte,
	nodePub []byte, nodes *bolt.Bucket) (*ChannelEdgePolicy, error) {

	var edgeKey [33 + 8]byte
	copy(edgeKey[:], nodePub)
	copy(edgeKey[33:], chanID[:])

	edgeBytes := edges.Get(edgeKey[:])
	if edgeBytes == nil {
		return nil, ErrEdgeNotFound
	}

	edgeReader := bytes.NewReader(edgeBytes)

	return deserializeChanEdgePolicy(edgeReader, nodes)
}

func fetchChanEdgePolicies(edgeIndex *bolt.Bucket, edges *bolt.Bucket,
	nodes *bolt.Bucket, chanID []byte,
	db *DB) (*ChannelEdgePolicy, *ChannelEdgePolicy, error) {

	edgeInfo := edgeIndex.Get(chanID)
	if edgeInfo == nil {
		return nil, nil, ErrEdgeNotFound
	}

	// The first node is contained within the first half of the edge
	// information. We only propagate the error here and below if it's
	// something other than edge non-existence.
	node1Pub := edgeInfo[:33]
	edge1, err := fetchChanEdgePolicy(edges, chanID, node1Pub, nodes)
	if err != nil && err != ErrEdgeNotFound {
		return nil, nil, err
	}

	// As we may have a single direction of the edge but not the other,
	// only fill in the database pointers if the edge is found.
	if edge1 != nil {
		edge1.db = db
		edge1.Node.db = db
	}

	// Similarly, the second node is contained within the latter
	// half of the edge information.
	node2Pub := edgeInfo[33:67]
	edge2, err := fetchChanEdgePolicy(edges, chanID, node2Pub, nodes)
	if err != nil && err != ErrEdgeNotFound {
		return nil, nil, err
	}

	if edge2 != nil {
		edge2.db = db
		edge2.Node.db = db
	}

	return edge1, edge2, nil
}

func deserializeChanEdgePolicy(r io.Reader,
	nodes *bolt.Bucket) (*ChannelEdgePolicy, error) {

	edge := &ChannelEdgePolicy{}

	sigBytes, err := wire.ReadVarBytes(r, 0, 80, "sig")
	if err != nil {
		return nil, err
	}

	edge.Signature, err = btcec.ParseSignature(sigBytes, btcec.S256())
	if err != nil {
		return nil, err
	}

	if err := binary.Read(r, byteOrder, &edge.ChannelID); err != nil {
		return nil, err
	}

	var scratch [8]byte
	if _, err := r.Read(scratch[:]); err != nil {
		return nil, err
	}
	unix := int64(byteOrder.Uint64(scratch[:]))
	edge.LastUpdate = time.Unix(unix, 0)

	if err := binary.Read(r, byteOrder, &edge.Flags); err != nil {
		return nil, err
	}
	if err := binary.Read(r, byteOrder, &edge.TimeLockDelta); err != nil {
		return nil, err
	}

	var n uint64
	if err := binary.Read(r, byteOrder, &n); err != nil {
		return nil, err
	}
	edge.MinHTLC = lnwire.MilliSatoshi(n)

	if err := binary.Read(r, byteOrder, &n); err != nil {
		return nil, err
	}
	edge.FeeBaseMSat = lnwire.MilliSatoshi(n)

	if err := binary.Read(r, byteOrder, &n); err != nil {
		return nil, err
	}
	edge.FeeProportionalMillionths = lnwire.MilliSatoshi(n)

	var pub [33]byte
	if _, err := r.Read(pub[:]); err != nil {
		return nil, err
	}

	node, err := fetchLightningNode(nodes, pub[:])
	if err != nil {
		return nil, err
	}

	edge.Node = node
	return edge, nil
}
