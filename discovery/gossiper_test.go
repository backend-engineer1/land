package discovery

import (
	"encoding/hex"
	"fmt"
	"net"
	"reflect"
	"sync"

	prand "math/rand"

	"testing"

	"math/big"

	"time"

	"io/ioutil"
	"os"

	"github.com/davecgh/go-spew/spew"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/wire"
)

var (
	testAddr = &net.TCPAddr{IP: (net.IP)([]byte{0xA, 0x0, 0x0, 0x1}),
		Port: 9000}
	testAddrs    = []net.Addr{testAddr}
	testFeatures = lnwire.NewRawFeatureVector()
	testSig      = &btcec.Signature{
		R: new(big.Int),
		S: new(big.Int),
	}
	_, _ = testSig.R.SetString("63724406601629180062774974542967536251589935445068131219452686511677818569431", 10)
	_, _ = testSig.S.SetString("18801056069249825825291287104931333862866033135609736119018462340006816851118", 10)

	inputStr = "147caa76786596590baa4e98f5d9f48b86c7765e489f7a6ff3360fe5c674360b"
	sha, _   = chainhash.NewHashFromStr(inputStr)
	outpoint = wire.NewOutPoint(sha, 0)

	bitcoinKeyPriv1, _ = btcec.NewPrivateKey(btcec.S256())
	bitcoinKeyPub1     = bitcoinKeyPriv1.PubKey()

	nodeKeyPriv1, _ = btcec.NewPrivateKey(btcec.S256())
	nodeKeyPub1     = nodeKeyPriv1.PubKey()

	bitcoinKeyPriv2, _ = btcec.NewPrivateKey(btcec.S256())
	bitcoinKeyPub2     = bitcoinKeyPriv2.PubKey()

	nodeKeyPriv2, _ = btcec.NewPrivateKey(btcec.S256())
	nodeKeyPub2     = nodeKeyPriv2.PubKey()

	trickleDelay     = time.Millisecond * 100
	retransmitDelay  = time.Hour * 1
	proofMatureDelta uint32
)

// makeTestDB creates a new instance of the ChannelDB for testing purposes. A
// callback which cleans up the created temporary directories is also returned
// and intended to be executed after the test completes.
func makeTestDB() (*channeldb.DB, func(), error) {
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

	return cdb, cleanUp, nil
}

type mockSigner struct {
	privKey *btcec.PrivateKey
}

func (n *mockSigner) SignMessage(pubKey *btcec.PublicKey,
	msg []byte) (*btcec.Signature, error) {

	if !pubKey.IsEqual(n.privKey.PubKey()) {
		return nil, fmt.Errorf("unknown public key")
	}

	digest := chainhash.DoubleHashB(msg)
	sign, err := n.privKey.Sign(digest)
	if err != nil {
		return nil, fmt.Errorf("can't sign the message: %v", err)
	}

	return sign, nil
}

type mockGraphSource struct {
	nodes      []*channeldb.LightningNode
	infos      map[uint64]*channeldb.ChannelEdgeInfo
	edges      map[uint64][]*channeldb.ChannelEdgePolicy
	bestHeight uint32
}

func newMockRouter(height uint32) *mockGraphSource {
	return &mockGraphSource{
		bestHeight: height,
		infos:      make(map[uint64]*channeldb.ChannelEdgeInfo),
		edges:      make(map[uint64][]*channeldb.ChannelEdgePolicy),
	}
}

var _ routing.ChannelGraphSource = (*mockGraphSource)(nil)

func (r *mockGraphSource) AddNode(node *channeldb.LightningNode) error {
	r.nodes = append(r.nodes, node)
	return nil
}

func (r *mockGraphSource) AddEdge(info *channeldb.ChannelEdgeInfo) error {
	if _, ok := r.infos[info.ChannelID]; ok {
		return errors.New("info already exist")
	}
	r.infos[info.ChannelID] = info
	return nil
}

func (r *mockGraphSource) UpdateEdge(edge *channeldb.ChannelEdgePolicy) error {
	r.edges[edge.ChannelID] = append(
		r.edges[edge.ChannelID],
		edge,
	)
	return nil
}

func (r *mockGraphSource) SelfEdges() ([]*channeldb.ChannelEdgePolicy, error) {
	return nil, nil
}

func (r *mockGraphSource) CurrentBlockHeight() (uint32, error) {
	return r.bestHeight, nil
}

func (r *mockGraphSource) AddProof(chanID lnwire.ShortChannelID,
	proof *channeldb.ChannelAuthProof) error {
	info, ok := r.infos[chanID.ToUint64()]
	if !ok {
		return errors.New("channel does not exist")
	}
	info.AuthProof = proof
	return nil
}

func (r *mockGraphSource) ForEachNode(func(node *channeldb.LightningNode) error) error {
	return nil
}

func (r *mockGraphSource) ForAllOutgoingChannels(cb func(i *channeldb.ChannelEdgeInfo,
	c *channeldb.ChannelEdgePolicy) error) error {
	return nil
}

func (r *mockGraphSource) ForEachChannel(func(chanInfo *channeldb.ChannelEdgeInfo,
	e1, e2 *channeldb.ChannelEdgePolicy) error) error {
	return nil
}

func (r *mockGraphSource) GetChannelByID(chanID lnwire.ShortChannelID) (
	*channeldb.ChannelEdgeInfo,
	*channeldb.ChannelEdgePolicy,
	*channeldb.ChannelEdgePolicy, error) {

	chanInfo, ok := r.infos[chanID.ToUint64()]
	if !ok {
		return nil, nil, nil, channeldb.ErrEdgeNotFound
	}

	edges := r.edges[chanID.ToUint64()]
	if len(edges) == 0 {
		return chanInfo, nil, nil, nil
	}

	if len(edges) == 1 {
		return chanInfo, edges[0], nil, nil
	}

	return chanInfo, edges[0], edges[1], nil
}

type mockNotifier struct {
	clientCounter uint32
	epochClients  map[uint32]chan *chainntnfs.BlockEpoch

	sync.RWMutex
}

func newMockNotifier() *mockNotifier {
	return &mockNotifier{
		epochClients: make(map[uint32]chan *chainntnfs.BlockEpoch),
	}
}

func (m *mockNotifier) RegisterConfirmationsNtfn(txid *chainhash.Hash,
	numConfs, _ uint32) (*chainntnfs.ConfirmationEvent, error) {

	return nil, nil
}

func (m *mockNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint, _ uint32) (*chainntnfs.SpendEvent, error) {
	return nil, nil
}

func (m *mockNotifier) notifyBlock(hash chainhash.Hash, height uint32) {
	m.RLock()
	defer m.RUnlock()

	for _, client := range m.epochClients {
		client <- &chainntnfs.BlockEpoch{
			Height: int32(height),
			Hash:   &hash,
		}
	}
}

func (m *mockNotifier) RegisterBlockEpochNtfn() (*chainntnfs.BlockEpochEvent, error) {
	m.RLock()
	defer m.RUnlock()

	epochChan := make(chan *chainntnfs.BlockEpoch)
	clientID := m.clientCounter
	m.clientCounter++
	m.epochClients[clientID] = epochChan

	return &chainntnfs.BlockEpochEvent{
		Epochs: epochChan,
		Cancel: func() {},
	}, nil
}

func (m *mockNotifier) Start() error {
	return nil
}

func (m *mockNotifier) Stop() error {
	return nil
}

type annBatch struct {
	nodeAnn1 *lnwire.NodeAnnouncement
	nodeAnn2 *lnwire.NodeAnnouncement

	localChanAnn  *lnwire.ChannelAnnouncement
	remoteChanAnn *lnwire.ChannelAnnouncement

	chanUpdAnn1 *lnwire.ChannelUpdate
	chanUpdAnn2 *lnwire.ChannelUpdate

	localProofAnn  *lnwire.AnnounceSignatures
	remoteProofAnn *lnwire.AnnounceSignatures
}

func createAnnouncements(blockHeight uint32) (*annBatch, error) {
	var err error
	var batch annBatch
	timestamp := uint32(123456)

	batch.nodeAnn1, err = createNodeAnnouncement(nodeKeyPriv1, timestamp)
	if err != nil {
		return nil, err
	}

	batch.nodeAnn2, err = createNodeAnnouncement(nodeKeyPriv2, timestamp)
	if err != nil {
		return nil, err
	}

	batch.remoteChanAnn, err = createRemoteChannelAnnouncement(blockHeight)
	if err != nil {
		return nil, err
	}

	batch.localProofAnn = &lnwire.AnnounceSignatures{
		NodeSignature:    batch.remoteChanAnn.NodeSig1,
		BitcoinSignature: batch.remoteChanAnn.BitcoinSig1,
	}

	batch.remoteProofAnn = &lnwire.AnnounceSignatures{
		NodeSignature:    batch.remoteChanAnn.NodeSig2,
		BitcoinSignature: batch.remoteChanAnn.BitcoinSig2,
	}

	batch.localChanAnn, err = createRemoteChannelAnnouncement(blockHeight)
	if err != nil {
		return nil, err
	}
	batch.localChanAnn.BitcoinSig1 = nil
	batch.localChanAnn.BitcoinSig2 = nil
	batch.localChanAnn.NodeSig1 = nil
	batch.localChanAnn.NodeSig2 = nil

	batch.chanUpdAnn1, err = createUpdateAnnouncement(
		blockHeight, 0, nodeKeyPriv1, timestamp,
	)
	if err != nil {
		return nil, err
	}

	batch.chanUpdAnn2, err = createUpdateAnnouncement(
		blockHeight, 1, nodeKeyPriv2, timestamp,
	)
	if err != nil {
		return nil, err
	}

	return &batch, nil

}

func createNodeAnnouncement(priv *btcec.PrivateKey,
	timestamp uint32) (*lnwire.NodeAnnouncement,
	error) {
	var err error

	k := hex.EncodeToString(priv.Serialize())
	alias, err := lnwire.NewNodeAlias("kek" + k[:10])
	if err != nil {
		return nil, err
	}

	a := &lnwire.NodeAnnouncement{
		Timestamp: timestamp,
		Addresses: testAddrs,
		NodeID:    priv.PubKey(),
		Alias:     alias,
		Features:  testFeatures,
	}

	signer := mockSigner{priv}
	a.Signature, err = SignAnnouncement(&signer, priv.PubKey(), a)
	if err != nil {
		return nil, err
	}

	return a, nil
}

func createUpdateAnnouncement(blockHeight uint32, flags lnwire.ChanUpdateFlag,
	nodeKey *btcec.PrivateKey, timestamp uint32) (*lnwire.ChannelUpdate,
	error) {

	var err error

	a := &lnwire.ChannelUpdate{
		ShortChannelID: lnwire.ShortChannelID{
			BlockHeight: blockHeight,
		},
		Timestamp:       timestamp,
		TimeLockDelta:   uint16(prand.Int63()),
		Flags:           flags,
		HtlcMinimumMsat: lnwire.MilliSatoshi(prand.Int63()),
		FeeRate:         uint32(prand.Int31()),
		BaseFee:         uint32(prand.Int31()),
	}

	pub := nodeKey.PubKey()
	signer := mockSigner{nodeKey}
	if a.Signature, err = SignAnnouncement(&signer, pub, a); err != nil {
		return nil, err
	}

	return a, nil
}

func createRemoteChannelAnnouncement(blockHeight uint32) (*lnwire.ChannelAnnouncement, error) {

	var err error
	a := &lnwire.ChannelAnnouncement{
		ShortChannelID: lnwire.ShortChannelID{
			BlockHeight: blockHeight,
			TxIndex:     0,
			TxPosition:  0,
		},
		NodeID1:     nodeKeyPub1,
		NodeID2:     nodeKeyPub2,
		BitcoinKey1: bitcoinKeyPub1,
		BitcoinKey2: bitcoinKeyPub2,
		Features:    testFeatures,
	}

	pub := nodeKeyPriv1.PubKey()
	signer := mockSigner{nodeKeyPriv1}
	if a.NodeSig1, err = SignAnnouncement(&signer, pub, a); err != nil {
		return nil, err
	}

	pub = nodeKeyPriv2.PubKey()
	signer = mockSigner{nodeKeyPriv2}
	if a.NodeSig2, err = SignAnnouncement(&signer, pub, a); err != nil {
		return nil, err
	}

	pub = bitcoinKeyPriv1.PubKey()
	signer = mockSigner{bitcoinKeyPriv1}
	if a.BitcoinSig1, err = SignAnnouncement(&signer, pub, a); err != nil {
		return nil, err
	}

	pub = bitcoinKeyPriv2.PubKey()
	signer = mockSigner{bitcoinKeyPriv2}
	if a.BitcoinSig2, err = SignAnnouncement(&signer, pub, a); err != nil {
		return nil, err
	}

	return a, nil
}

type testCtx struct {
	gossiper           *AuthenticatedGossiper
	router             *mockGraphSource
	notifier           *mockNotifier
	broadcastedMessage chan msgWithSenders
}

func createTestCtx(startHeight uint32) (*testCtx, func(), error) {
	// Next we'll initialize an instance of the channel router with mock
	// versions of the chain and channel notifier. As we don't need to test
	// any p2p functionality, the peer send and switch send,
	// broadcast functions won't be populated.
	notifier := newMockNotifier()
	router := newMockRouter(startHeight)

	db, cleanUpDb, err := makeTestDB()
	if err != nil {
		return nil, nil, err
	}

	broadcastedMessage := make(chan msgWithSenders, 10)
	gossiper, err := New(Config{
		Notifier: notifier,
		Broadcast: func(senders map[routing.Vertex]struct{},
			msgs ...lnwire.Message) error {

			for _, msg := range msgs {
				broadcastedMessage <- msgWithSenders{
					msg:     msg,
					senders: senders,
				}
			}

			return nil
		},
		SendToPeer: func(target *btcec.PublicKey, msg ...lnwire.Message) error {
			return nil
		},
		Router:           router,
		TrickleDelay:     trickleDelay,
		RetransmitDelay:  retransmitDelay,
		ProofMatureDelta: proofMatureDelta,
		DB:               db,
	}, nodeKeyPub1)
	if err != nil {
		cleanUpDb()
		return nil, nil, fmt.Errorf("unable to create router %v", err)
	}
	if err := gossiper.Start(); err != nil {
		cleanUpDb()
		return nil, nil, fmt.Errorf("unable to start router: %v", err)
	}

	cleanUp := func() {
		gossiper.Stop()
		cleanUpDb()
	}

	return &testCtx{
		router:             router,
		notifier:           notifier,
		gossiper:           gossiper,
		broadcastedMessage: broadcastedMessage,
	}, cleanUp, nil
}

// TestProcessAnnouncement checks that mature announcements are propagated to
// the router subsystem.
func TestProcessAnnouncement(t *testing.T) {
	t.Parallel()

	timestamp := uint32(123456)

	ctx, cleanup, err := createTestCtx(0)
	if err != nil {
		t.Fatalf("can't create context: %v", err)
	}
	defer cleanup()

	assertSenderExistence := func(sender *btcec.PublicKey, msg msgWithSenders) {
		if _, ok := msg.senders[routing.NewVertex(sender)]; !ok {
			t.Fatalf("sender=%x not present in %v",
				sender.SerializeCompressed(), spew.Sdump(msg))
		}
	}

	// Create node valid, signed announcement, process it with with
	// gossiper service, check that valid announcement have been
	// propagated farther into the lightning network, and check that we
	// added new node into router.
	na, err := createNodeAnnouncement(nodeKeyPriv1, timestamp)
	if err != nil {
		t.Fatalf("can't create node announcement: %v", err)
	}

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(na, na.NodeID):
	case <-time.After(2 * time.Second):
		t.Fatal("remote announcement not processed")
	}
	if err != nil {
		t.Fatalf("can't process remote announcement: %v", err)
	}

	select {
	case msg := <-ctx.broadcastedMessage:
		assertSenderExistence(na.NodeID, msg)
	case <-time.After(2 * trickleDelay):
		t.Fatal("announcememt wasn't proceeded")
	}

	if len(ctx.router.nodes) != 1 {
		t.Fatalf("node wasn't added to router: %v", err)
	}

	// Pretending that we receive the valid channel announcement from
	// remote side, and check that we broadcasted it to the our network,
	// and added channel info in the router.
	ca, err := createRemoteChannelAnnouncement(0)
	if err != nil {
		t.Fatalf("can't create channel announcement: %v", err)
	}

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(ca, na.NodeID):
	case <-time.After(2 * time.Second):
		t.Fatal("remote announcement not processed")
	}
	if err != nil {
		t.Fatalf("can't process remote announcement: %v", err)
	}

	select {
	case msg := <-ctx.broadcastedMessage:
		assertSenderExistence(na.NodeID, msg)
	case <-time.After(2 * trickleDelay):
		t.Fatal("announcememt wasn't proceeded")
	}

	if len(ctx.router.infos) != 1 {
		t.Fatalf("edge wasn't added to router: %v", err)
	}

	// Pretending that we received valid channel policy update from remote
	// side, and check that we broadcasted it to the other network, and
	// added updates to the router.
	ua, err := createUpdateAnnouncement(0, 0, nodeKeyPriv1, timestamp)
	if err != nil {
		t.Fatalf("can't create update announcement: %v", err)
	}

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(ua, na.NodeID):
	case <-time.After(2 * time.Second):
		t.Fatal("remote announcement not processed")
	}
	if err != nil {
		t.Fatalf("can't process remote announcement: %v", err)
	}

	select {
	case msg := <-ctx.broadcastedMessage:
		assertSenderExistence(na.NodeID, msg)
	case <-time.After(2 * trickleDelay):
		t.Fatal("announcememt wasn't proceeded")
	}

	if len(ctx.router.edges) != 1 {
		t.Fatalf("edge update wasn't added to router: %v", err)
	}
}

// TestPrematureAnnouncement checks that premature announcements are
// not propagated to the router subsystem until block with according
// block height received.
func TestPrematureAnnouncement(t *testing.T) {
	t.Parallel()

	timestamp := uint32(123456)

	ctx, cleanup, err := createTestCtx(0)
	if err != nil {
		t.Fatalf("can't create context: %v", err)
	}
	defer cleanup()

	na, err := createNodeAnnouncement(nodeKeyPriv1, timestamp)
	if err != nil {
		t.Fatalf("can't create node announcement: %v", err)
	}

	// Pretending that we receive the valid channel announcement from
	// remote side, but block height of this announcement is greater than
	// highest know to us, for that reason it should be added to the
	// repeat/premature batch.
	ca, err := createRemoteChannelAnnouncement(1)
	if err != nil {
		t.Fatalf("can't create channel announcement: %v", err)
	}

	select {
	case <-ctx.gossiper.ProcessRemoteAnnouncement(ca, na.NodeID):
		t.Fatal("announcement was proceeded")
	case <-time.After(100 * time.Millisecond):
	}

	if len(ctx.router.infos) != 0 {
		t.Fatal("edge was added to router")
	}

	// Pretending that we receive the valid channel update announcement from
	// remote side, but block height of this announcement is greater than
	// highest know to us, for that reason it should be added to the
	// repeat/premature batch.
	ua, err := createUpdateAnnouncement(1, 0, nodeKeyPriv1, timestamp)
	if err != nil {
		t.Fatalf("can't create update announcement: %v", err)
	}

	select {
	case <-ctx.gossiper.ProcessRemoteAnnouncement(ua, na.NodeID):
		t.Fatal("announcement was proceeded")
	case <-time.After(100 * time.Millisecond):
	}

	if len(ctx.router.edges) != 0 {
		t.Fatal("edge update was added to router")
	}

	// Generate new block and waiting the previously added announcements
	// to be proceeded.
	newBlock := &wire.MsgBlock{}
	ctx.notifier.notifyBlock(newBlock.Header.BlockHash(), 1)

	select {
	case <-ctx.broadcastedMessage:
	case <-time.After(2 * trickleDelay):
		t.Fatal("announcememt wasn't broadcasted")
	}

	if len(ctx.router.infos) != 1 {
		t.Fatalf("edge was't added to router: %v", err)
	}

	select {
	case <-ctx.broadcastedMessage:
	case <-time.After(2 * trickleDelay):
		t.Fatal("announcememt wasn't broadcasted")
	}

	if len(ctx.router.edges) != 1 {
		t.Fatalf("edge update wasn't added to router: %v", err)
	}
}

// TestSignatureAnnouncementLocalFirst ensures that the AuthenticatedGossiper
// properly processes partial and fully announcement signatures message.
func TestSignatureAnnouncementLocalFirst(t *testing.T) {
	t.Parallel()

	ctx, cleanup, err := createTestCtx(uint32(proofMatureDelta))
	if err != nil {
		t.Fatalf("can't create context: %v", err)
	}
	defer cleanup()

	// Set up a channel that we can use to inspect the messages
	// sent directly fromn the gossiper.
	sentMsgs := make(chan lnwire.Message, 10)
	ctx.gossiper.cfg.SendToPeer = func(target *btcec.PublicKey, msg ...lnwire.Message) error {
		select {
		case sentMsgs <- msg[0]:
		case <-ctx.gossiper.quit:
			return fmt.Errorf("shutting down")
		}
		return nil
	}

	batch, err := createAnnouncements(0)
	if err != nil {
		t.Fatalf("can't generate announcements: %v", err)
	}

	localKey := batch.nodeAnn1.NodeID
	remoteKey := batch.nodeAnn2.NodeID

	// Recreate lightning network topology. Initialize router with channel
	// between two nodes.
	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.localChanAnn,
		localKey):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.chanUpdAnn1,
		localKey):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}

	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel update announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	// The local ChannelUpdate should now be sent directly to the remote peer,
	// such that the edge can be used for routing, regardless if this channel
	// is announced or not (private channel).
	select {
	case msg := <-sentMsgs:
		if msg != batch.chanUpdAnn1 {
			t.Fatalf("expected local channel update, instead got %v", msg)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("gossiper did not send channel update to peer")
	}

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(batch.chanUpdAnn2,
		remoteKey):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel update announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	// Pretending that we receive local channel announcement from funding
	// manager, thereby kick off the announcement exchange process.
	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.localProofAnn,
		localKey):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}

	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("announcements were broadcast")
	case <-time.After(2 * trickleDelay):
	}

	number := 0
	if err := ctx.gossiper.waitingProofs.ForAll(
		func(*channeldb.WaitingProof) error {
			number++
			return nil
		},
	); err != nil {
		t.Fatalf("unable to retrieve objects from store: %v", err)
	}

	if number != 1 {
		t.Fatal("wrong number of objects in storage")
	}

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(batch.remoteProofAnn,
		remoteKey):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}

	for i := 0; i < 3; i++ {
		select {
		case <-ctx.broadcastedMessage:
		case <-time.After(time.Second):
			t.Fatal("announcement wasn't broadcast")
		}
	}

	number = 0
	if err := ctx.gossiper.waitingProofs.ForAll(
		func(*channeldb.WaitingProof) error {
			number++
			return nil
		},
	); err != nil && err != channeldb.ErrWaitingProofNotFound {
		t.Fatalf("unable to retrieve objects from store: %v", err)
	}

	if number != 0 {
		t.Fatal("waiting proof should be removed from storage")
	}
}

// TestOrphanSignatureAnnouncement ensures that the gossiper properly
// processes announcement with unknown channel ids.
func TestOrphanSignatureAnnouncement(t *testing.T) {
	t.Parallel()

	ctx, cleanup, err := createTestCtx(uint32(proofMatureDelta))
	if err != nil {
		t.Fatalf("can't create context: %v", err)
	}
	defer cleanup()

	// Set up a channel that we can use to inspect the messages
	// sent directly from the gossiper.
	sentMsgs := make(chan lnwire.Message, 10)
	ctx.gossiper.cfg.SendToPeer = func(target *btcec.PublicKey, msg ...lnwire.Message) error {
		select {
		case sentMsgs <- msg[0]:
		case <-ctx.gossiper.quit:
			return fmt.Errorf("shutting down")
		}
		return nil
	}

	batch, err := createAnnouncements(0)
	if err != nil {
		t.Fatalf("can't generate announcements: %v", err)
	}

	localKey := batch.nodeAnn1.NodeID
	remoteKey := batch.nodeAnn2.NodeID

	// Pretending that we receive local channel announcement from funding
	// manager, thereby kick off the announcement exchange process, in
	// this case the announcement should be added in the orphan batch
	// because we haven't announce the channel yet.
	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(batch.remoteProofAnn,
		remoteKey):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	if err != nil {
		t.Fatalf("unable to proceed announcement: %v", err)
	}

	number := 0
	if err := ctx.gossiper.waitingProofs.ForAll(
		func(*channeldb.WaitingProof) error {
			number++
			return nil
		},
	); err != nil {
		t.Fatalf("unable to retrieve objects from store: %v", err)
	}

	if number != 1 {
		t.Fatal("wrong number of objects in storage")
	}

	// Recreate lightning network topology. Initialize router with channel
	// between two nodes.
	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.localChanAnn,
		localKey):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}

	if err != nil {
		t.Fatalf("unable to process: %v", err)
	}

	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.chanUpdAnn1,
		localKey):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	if err != nil {
		t.Fatalf("unable to process: %v", err)
	}

	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel update announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	// The local ChannelUpdate should now be sent directly to the remote peer,
	// such that the edge can be used for routing, regardless if this channel
	// is announced or not (private channel).
	select {
	case msg := <-sentMsgs:
		if msg != batch.chanUpdAnn1 {
			t.Fatalf("expected local channel update, instead got %v", msg)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("gossiper did not send channel update to peer")
	}

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(batch.chanUpdAnn2,
		remoteKey):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	if err != nil {
		t.Fatalf("unable to process: %v", err)
	}
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel update announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	// After that we process local announcement, and waiting to receive
	// the channel announcement.
	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.localProofAnn,
		localKey):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	if err != nil {
		t.Fatalf("unable to process: %v", err)
	}

	// The local proof should be sent to the remote peer.
	select {
	case msg := <-sentMsgs:
		if msg != batch.localProofAnn {
			t.Fatalf("expected local proof to be sent, got %v", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("local proof was not sent to peer")
	}

	// And since both remote and local announcements are processed, we
	// should be broadcasting the final channel announcements.
	for i := 0; i < 3; i++ {
		select {
		case <-ctx.broadcastedMessage:
		case <-time.After(time.Second):
			t.Fatal("announcement wasn't broadcast")
		}
	}

	number = 0
	if err := ctx.gossiper.waitingProofs.ForAll(
		func(p *channeldb.WaitingProof) error {
			number++
			return nil
		},
	); err != nil {
		t.Fatalf("unable to retrieve objects from store: %v", err)
	}

	if number != 0 {
		t.Fatalf("wrong number of objects in storage: %v", number)
	}
}

// Test that sending AnnounceSignatures to remote peer will continue
// to be tried until the peer comes online.
func TestSignatureAnnouncementRetry(t *testing.T) {
	t.Parallel()

	ctx, cleanup, err := createTestCtx(uint32(proofMatureDelta))
	if err != nil {
		t.Fatalf("can't create context: %v", err)
	}
	defer cleanup()

	batch, err := createAnnouncements(0)
	if err != nil {
		t.Fatalf("can't generate announcements: %v", err)
	}

	localKey := batch.nodeAnn1.NodeID
	remoteKey := batch.nodeAnn2.NodeID

	// Recreate lightning network topology. Initialize router with channel
	// between two nodes.
	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.localChanAnn,
		localKey):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.chanUpdAnn1,
		localKey):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel update announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(batch.chanUpdAnn2,
		remoteKey):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel update announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	// Make the SendToPeer fail, simulating the peer being offline.
	ctx.gossiper.cfg.SendToPeer = func(target *btcec.PublicKey,
		msg ...lnwire.Message) error {
		return fmt.Errorf("intentional error in SendToPeer")
	}

	// We expect the gossiper to register for a notification when the peer
	// comes back online, so keep track of the channel it wants to get
	// notified on.
	notifyPeers := make(chan chan<- struct{}, 1)
	ctx.gossiper.cfg.NotifyWhenOnline = func(peer *btcec.PublicKey,
		connectedChan chan<- struct{}) {
		notifyPeers <- connectedChan
	}

	// Pretending that we receive local channel announcement from funding
	// manager, thereby kick off the announcement exchange process.
	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.localProofAnn,
		localKey):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}

	// Since sending this local announcement proof to the remote will fail,
	// the gossiper should register for a notification when the remote is
	// online again.
	var conChan chan<- struct{}
	select {
	case conChan = <-notifyPeers:
	case <-time.After(2 * time.Second):
		t.Fatalf("gossiper did not ask to get notified when " +
			"peer is online")
	}

	// Since both proofs are not yet exchanged, no message should be
	// broadcasted yet.
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("announcements were broadcast")
	case <-time.After(2 * trickleDelay):
	}

	number := 0
	if err := ctx.gossiper.waitingProofs.ForAll(
		func(*channeldb.WaitingProof) error {
			number++
			return nil
		},
	); err != nil {
		t.Fatalf("unable to retrieve objects from store: %v", err)
	}

	if number != 1 {
		t.Fatal("wrong number of objects in storage")
	}

	// When the peer comes online, the gossiper gets notified, and should
	// retry sending the AnnnounceSignatures. We make the SendToPeer
	// method work again.
	sentToPeer := make(chan lnwire.Message, 1)
	ctx.gossiper.cfg.SendToPeer = func(target *btcec.PublicKey,
		msg ...lnwire.Message) error {
		sentToPeer <- msg[0]
		return nil
	}

	// Notify that peer is now online. THis should trigger a new call
	// to SendToPeer.
	close(conChan)

	select {
	case <-sentToPeer:
	case <-time.After(2 * time.Second):
		t.Fatalf("gossiper did not send message when peer came online")
	}

	// Now give the gossiper the remote proof. This should trigger a
	// broadcast of 3 messages (ChannelAnnouncement + 2 ChannelUpdate).
	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(batch.remoteProofAnn,
		remoteKey):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}

	for i := 0; i < 3; i++ {
		select {
		case <-ctx.broadcastedMessage:
		case <-time.After(time.Second):
			t.Fatal("announcement wasn't broadcast")
		}
	}

	number = 0
	if err := ctx.gossiper.waitingProofs.ForAll(
		func(*channeldb.WaitingProof) error {
			number++
			return nil
		},
	); err != nil && err != channeldb.ErrWaitingProofNotFound {
		t.Fatalf("unable to retrieve objects from store: %v", err)
	}

	if number != 0 {
		t.Fatal("waiting proof should be removed from storage")
	}
}

// Test that if we restart the gossiper, it will retry sending the
// AnnounceSignatures to the peer if it did not succeed before
// shutting down, and the full channel proof is not yet assembled.
func TestSignatureAnnouncementRetryAtStartup(t *testing.T) {
	t.Parallel()

	ctx, cleanup, err := createTestCtx(uint32(proofMatureDelta))
	if err != nil {
		t.Fatalf("can't create context: %v", err)
	}
	defer cleanup()

	batch, err := createAnnouncements(0)
	if err != nil {
		t.Fatalf("can't generate announcements: %v", err)
	}

	localKey := batch.nodeAnn1.NodeID
	remoteKey := batch.nodeAnn2.NodeID

	// Recreate lightning network topology. Initialize router with channel
	// between two nodes.
	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.localChanAnn,
		localKey):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.chanUpdAnn1,
		localKey):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel update announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(batch.chanUpdAnn2,
		remoteKey):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel update announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	// Make the SendToPeerFail, simulating the peer being offline.
	ctx.gossiper.cfg.SendToPeer = func(target *btcec.PublicKey,
		msg ...lnwire.Message) error {
		return fmt.Errorf("intentional error in SendToPeer")
	}
	notifyPeers := make(chan chan<- struct{}, 1)
	ctx.gossiper.cfg.NotifyWhenOnline = func(peer *btcec.PublicKey,
		connectedChan chan<- struct{}) {
		notifyPeers <- connectedChan
	}

	// Pretending that we receive local channel announcement from funding
	// manager, thereby kick off the announcement exchange process.
	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.localProofAnn,
		localKey):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}

	// Since sending to the remote peer will fail, the gossiper should
	// register for a notification when it comes back online.
	var conChan chan<- struct{}
	select {
	case conChan = <-notifyPeers:
	case <-time.After(2 * time.Second):
		t.Fatalf("gossiper did not ask to get notified when " +
			"peer is online")
	}

	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("announcements were broadcast")
	case <-time.After(2 * trickleDelay):
	}

	number := 0
	if err := ctx.gossiper.waitingProofs.ForAll(
		func(*channeldb.WaitingProof) error {
			number++
			return nil
		},
	); err != nil {
		t.Fatalf("unable to retrieve objects from store: %v", err)
	}

	if number != 1 {
		t.Fatal("wrong number of objects in storage")
	}

	// Shut down gossiper, and restart. This should trigger a new attempt
	// to send the message to the peer.
	ctx.gossiper.Stop()
	gossiper, err := New(Config{
		Notifier:  ctx.gossiper.cfg.Notifier,
		Broadcast: ctx.gossiper.cfg.Broadcast,
		SendToPeer: func(target *btcec.PublicKey,
			msg ...lnwire.Message) error {
			return fmt.Errorf("intentional error in SendToPeer")
		},
		NotifyWhenOnline: func(peer *btcec.PublicKey,
			connectedChan chan<- struct{}) {
			notifyPeers <- connectedChan
		},
		Router:           ctx.gossiper.cfg.Router,
		TrickleDelay:     trickleDelay,
		RetransmitDelay:  retransmitDelay,
		ProofMatureDelta: proofMatureDelta,
		DB:               ctx.gossiper.cfg.DB,
	}, ctx.gossiper.selfKey)
	if err != nil {
		t.Fatalf("unable to recreate gossiper: %v", err)
	}
	if err := gossiper.Start(); err != nil {
		t.Fatalf("unable to start recreated gossiper: %v", err)
	}
	defer gossiper.Stop()

	ctx.gossiper = gossiper

	// After starting up, the gossiper will see that it has a waitingproof
	// in the database, and will retry sending its part to the remote. Since
	// SendToPeer will fail again, it should register for a notification
	// when the peer comes online.
	select {
	case conChan = <-notifyPeers:
	case <-time.After(2 * time.Second):
		t.Fatalf("gossiper did not ask to get notified when " +
			"peer is online")
	}

	// Fix the SendToPeer method.
	sentToPeer := make(chan lnwire.Message, 1)
	ctx.gossiper.cfg.SendToPeer = func(target *btcec.PublicKey,
		msg ...lnwire.Message) error {
		select {
		case sentToPeer <- msg[0]:
		case <-ctx.gossiper.quit:
			return fmt.Errorf("shutting down")
		}

		return nil
	}
	// Notify that peer is now online. This should trigger a new call
	// to SendToPeer.
	close(conChan)

	select {
	case <-sentToPeer:
	case <-time.After(2 * time.Second):
		t.Fatalf("gossiper did not send message when peer came online")
	}

	// Now exchanging the remote channel proof, the channel annoncement
	// broadcast should continue as normal.
	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(batch.remoteProofAnn,
		remoteKey):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}

	for i := 0; i < 3; i++ {
		select {
		case <-ctx.broadcastedMessage:
		case <-time.After(time.Second):
			t.Fatal("announcement wasn't broadcast")
		}
	}

	number = 0
	if err := ctx.gossiper.waitingProofs.ForAll(
		func(*channeldb.WaitingProof) error {
			number++
			return nil
		},
	); err != nil && err != channeldb.ErrWaitingProofNotFound {
		t.Fatalf("unable to retrieve objects from store: %v", err)
	}

	if number != 0 {
		t.Fatal("waiting proof should be removed from storage")
	}
}

// TestSignatureAnnouncementFullProofWhenRemoteProof tests that if a
// remote proof is received when we already have the full proof,
// the gossiper will send the full proof (ChannelAnnouncement) to
// the remote peer.
func TestSignatureAnnouncementFullProofWhenRemoteProof(t *testing.T) {
	t.Parallel()

	ctx, cleanup, err := createTestCtx(uint32(proofMatureDelta))
	if err != nil {
		t.Fatalf("can't create context: %v", err)
	}
	defer cleanup()

	batch, err := createAnnouncements(0)
	if err != nil {
		t.Fatalf("can't generate announcements: %v", err)
	}

	localKey := batch.nodeAnn1.NodeID
	remoteKey := batch.nodeAnn2.NodeID

	// Recreate lightning network topology. Initialize router with channel
	// between two nodes.
	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.localChanAnn,
		localKey):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.chanUpdAnn1,
		localKey):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel update announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(batch.chanUpdAnn2,
		remoteKey):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel update announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}
	// Set up a channel we can use to inspect messages sent by the
	// gossiper to the remote peer.
	sentToPeer := make(chan lnwire.Message, 1)
	ctx.gossiper.cfg.SendToPeer = func(target *btcec.PublicKey,
		msg ...lnwire.Message) error {
		select {
		case <-ctx.gossiper.quit:
			return fmt.Errorf("gossiper shutting down")
		case sentToPeer <- msg[0]:
		}
		return nil
	}

	notifyPeers := make(chan chan<- struct{}, 1)
	ctx.gossiper.cfg.NotifyWhenOnline = func(peer *btcec.PublicKey,
		connectedChan chan<- struct{}) {
		notifyPeers <- connectedChan
	}

	// Pretending that we receive local channel announcement from funding
	// manager, thereby kick off the announcement exchange process.
	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.localProofAnn,
		localKey):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(batch.remoteProofAnn,
		remoteKey):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}

	// We expect the gossiper to send this message to the remote peer.
	select {
	case msg := <-sentToPeer:
		if msg != batch.localProofAnn {
			t.Fatalf("wrong message sent to peer: %v", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not send local proof to peer")
	}

	// And all channel announcements should be broadcast.
	for i := 0; i < 3; i++ {
		select {
		case <-ctx.broadcastedMessage:
		case <-time.After(time.Second):
			t.Fatal("announcement wasn't broadcast")
		}
	}

	number := 0
	if err := ctx.gossiper.waitingProofs.ForAll(
		func(*channeldb.WaitingProof) error {
			number++
			return nil
		},
	); err != nil && err != channeldb.ErrWaitingProofNotFound {
		t.Fatalf("unable to retrieve objects from store: %v", err)
	}

	if number != 0 {
		t.Fatal("waiting proof should be removed from storage")
	}

	// Now give the gossiper the remote proof yet again. This should
	// trigger a send of the full ChannelAnnouncement.
	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(batch.remoteProofAnn,
		remoteKey):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}

	// We expect the gossiper to send this message to the remote peer.
	select {
	case msg := <-sentToPeer:
		_, ok := msg.(*lnwire.ChannelAnnouncement)
		if !ok {
			t.Fatalf("expected ChannelAnnouncement, intead got %T", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not send local proof to peer")
	}

}

// TestDeDuplicatedAnnouncements ensures that the deDupedAnnouncements struct
// properly stores and delivers the set of de-duplicated announcements.
func TestDeDuplicatedAnnouncements(t *testing.T) {
	t.Parallel()

	timestamp := uint32(123456)
	announcements := deDupedAnnouncements{}
	announcements.Reset()

	// Ensure that after new deDupedAnnouncements struct is created and
	// reset that storage of each announcement type is empty.
	if len(announcements.channelAnnouncements) != 0 {
		t.Fatal("channel announcements map not empty after reset")
	}
	if len(announcements.channelUpdates) != 0 {
		t.Fatal("channel updates map not empty after reset")
	}
	if len(announcements.nodeAnnouncements) != 0 {
		t.Fatal("node announcements map not empty after reset")
	}

	// Ensure that remote channel announcements are properly stored
	// and de-duplicated.
	ca, err := createRemoteChannelAnnouncement(0)
	if err != nil {
		t.Fatalf("can't create remote channel announcement: %v", err)
	}
	announcements.AddMsgs(networkMsg{msg: ca, peer: bitcoinKeyPub2})
	if len(announcements.channelAnnouncements) != 1 {
		t.Fatal("new channel announcement not stored in batch")
	}

	// We'll create a second instance of the same announcement with the
	// same channel ID. Adding this shouldn't cause an increase in the
	// number of items as they should be de-duplicated.
	ca2, err := createRemoteChannelAnnouncement(0)

	if err != nil {
		t.Fatalf("can't create remote channel announcement: %v", err)
	}
	announcements.AddMsgs(networkMsg{msg: ca2, peer: bitcoinKeyPub2})
	if len(announcements.channelAnnouncements) != 1 {
		t.Fatal("channel announcement not replaced in batch")
	}

	// Next, we'll ensure that channel update announcements are properly
	// stored and de-duplicated. We do this by creating two updates
	// announcements with the same short ID and flag.
	ua, err := createUpdateAnnouncement(0, 0, nodeKeyPriv1, timestamp)
	if err != nil {
		t.Fatalf("can't create update announcement: %v", err)
	}
	announcements.AddMsgs(networkMsg{msg: ua, peer: bitcoinKeyPub2})
	if len(announcements.channelUpdates) != 1 {
		t.Fatal("new channel update not stored in batch")
	}

	// Adding the very same announcement shouldn't cause an increase in the
	// number of ChannelUpdate announcements stored.
	ua2, err := createUpdateAnnouncement(0, 0, nodeKeyPriv1, timestamp)
	if err != nil {
		t.Fatalf("can't create update announcement: %v", err)
	}
	announcements.AddMsgs(networkMsg{msg: ua2, peer: bitcoinKeyPub2})
	if len(announcements.channelUpdates) != 1 {
		t.Fatal("channel update not replaced in batch")
	}

	// Adding an announcment with a later timestamp should replace the
	// stored one.
	ua3, err := createUpdateAnnouncement(0, 0, nodeKeyPriv1, timestamp+1)
	if err != nil {
		t.Fatalf("can't create update announcement: %v", err)
	}
	announcements.AddMsgs(networkMsg{msg: ua3, peer: bitcoinKeyPub2})
	if len(announcements.channelUpdates) != 1 {
		t.Fatal("channel update not replaced in batch")
	}

	assertChannelUpdate := func(channelUpdate *lnwire.ChannelUpdate) {
		channelKey := channelUpdateID{
			ua3.ShortChannelID,
			ua3.Flags,
		}

		mws, ok := announcements.channelUpdates[channelKey]
		if !ok {
			t.Fatal("channel update not in batch")
		}
		if mws.msg != channelUpdate {
			t.Fatalf("expected channel update %v, got %v)",
				channelUpdate, mws.msg)
		}
	}

	// Check that ua3 is the currently stored channel update.
	assertChannelUpdate(ua3)

	// Adding a channel update with an earlier timestamp should NOT
	// replace the one stored.
	ua4, err := createUpdateAnnouncement(0, 0, nodeKeyPriv1, timestamp)
	if err != nil {
		t.Fatalf("can't create update announcement: %v", err)
	}
	announcements.AddMsgs(networkMsg{msg: ua4, peer: bitcoinKeyPub2})
	if len(announcements.channelUpdates) != 1 {
		t.Fatal("channel update not in batch")
	}
	assertChannelUpdate(ua3)

	// Next well ensure that node announcements are properly de-duplicated.
	// We'll first add a single instance with a node's private key.
	na, err := createNodeAnnouncement(nodeKeyPriv1, timestamp)
	if err != nil {
		t.Fatalf("can't create node announcement: %v", err)
	}
	announcements.AddMsgs(networkMsg{msg: na, peer: bitcoinKeyPub2})
	if len(announcements.nodeAnnouncements) != 1 {
		t.Fatal("new node announcement not stored in batch")
	}

	// We'll now add another node to the batch.
	na2, err := createNodeAnnouncement(nodeKeyPriv2, timestamp)
	if err != nil {
		t.Fatalf("can't create node announcement: %v", err)
	}
	announcements.AddMsgs(networkMsg{msg: na2, peer: bitcoinKeyPub2})
	if len(announcements.nodeAnnouncements) != 2 {
		t.Fatal("second node announcement not stored in batch")
	}

	// Adding a new instance of the _same_ node shouldn't increase the size
	// of the node ann batch.
	na3, err := createNodeAnnouncement(nodeKeyPriv2, timestamp)
	if err != nil {
		t.Fatalf("can't create node announcement: %v", err)
	}
	announcements.AddMsgs(networkMsg{msg: na3, peer: bitcoinKeyPub2})
	if len(announcements.nodeAnnouncements) != 2 {
		t.Fatal("second node announcement not replaced in batch")
	}

	// Ensure that node announcement with different pointer to same public
	// key is still de-duplicated.
	newNodeKeyPointer := nodeKeyPriv2
	na4, err := createNodeAnnouncement(newNodeKeyPointer, timestamp)
	if err != nil {
		t.Fatalf("can't create node announcement: %v", err)
	}
	announcements.AddMsgs(networkMsg{msg: na4, peer: bitcoinKeyPub2})
	if len(announcements.nodeAnnouncements) != 2 {
		t.Fatal("second node announcement not replaced again in batch")
	}

	// Ensure that node announcement with increased timestamp replaces
	// what is currently stored.
	na5, err := createNodeAnnouncement(nodeKeyPriv2, timestamp+1)
	if err != nil {
		t.Fatalf("can't create node announcement: %v", err)
	}
	announcements.AddMsgs(networkMsg{msg: na5, peer: bitcoinKeyPub2})
	if len(announcements.nodeAnnouncements) != 2 {
		t.Fatal("node announcement not replaced in batch")
	}
	nodeID := routing.NewVertex(nodeKeyPriv2.PubKey())
	stored, ok := announcements.nodeAnnouncements[nodeID]
	if !ok {
		t.Fatalf("node announcement not found in batch")
	}
	if stored.msg != na5 {
		t.Fatalf("expected de-duped node announcement to be %v, got %v",
			na5, stored.msg)
	}

	// Ensure that announcement batch delivers channel announcements,
	// channel updates, and node announcements in proper order.
	batch := announcements.Emit()
	if len(batch) != 4 {
		t.Fatal("announcement batch incorrect length")
	}

	if !reflect.DeepEqual(batch[0].msg, ca2) {
		t.Fatalf("channel announcement not first in batch: got %v, "+
			"expected %v", spew.Sdump(batch[0].msg), spew.Sdump(ca2))
	}

	if !reflect.DeepEqual(batch[1].msg, ua3) {
		t.Fatalf("channel update not next in batch: got %v, "+
			"expected %v", spew.Sdump(batch[1].msg), spew.Sdump(ua2))
	}

	// We'll ensure that both node announcements are present. We check both
	// indexes as due to the randomized order of map iteration they may be
	// in either place.
	if !reflect.DeepEqual(batch[2].msg, na) && !reflect.DeepEqual(batch[3].msg, na) {
		t.Fatal("first node announcement not in last part of batch: "+
			"got %v, expected %v", batch[2].msg,
			na)
	}
	if !reflect.DeepEqual(batch[2].msg, na5) && !reflect.DeepEqual(batch[3].msg, na5) {
		t.Fatalf("second node announcement not in last part of batch: "+
			"got %v, expected %v", batch[3].msg,
			na5)
	}

	// Ensure that after reset, storage of each announcement type
	// in deDupedAnnouncements struct is empty again.
	announcements.Reset()
	if len(announcements.channelAnnouncements) != 0 {
		t.Fatal("channel announcements map not empty after reset")
	}
	if len(announcements.channelUpdates) != 0 {
		t.Fatal("channel updates map not empty after reset")
	}
	if len(announcements.nodeAnnouncements) != 0 {
		t.Fatal("node announcements map not empty after reset")
	}
}

// TestReceiveRemoteChannelUpdateFirst tests that if we receive a
// CHannelUpdate from the remote before we have processed our
// own ChannelAnnouncement, it will be reprocessed later, after
// our ChannelAnnouncement.
func TestReceiveRemoteChannelUpdateFirst(t *testing.T) {
	t.Parallel()

	ctx, cleanup, err := createTestCtx(uint32(proofMatureDelta))
	if err != nil {
		t.Fatalf("can't create context: %v", err)
	}
	defer cleanup()

	// Set up a channel that we can use to inspect the messages
	// sent directly fromn the gossiper.
	sentMsgs := make(chan lnwire.Message, 10)
	ctx.gossiper.cfg.SendToPeer = func(target *btcec.PublicKey, msg ...lnwire.Message) error {
		select {
		case sentMsgs <- msg[0]:
		case <-ctx.gossiper.quit:
			return fmt.Errorf("shutting down")
		}
		return nil
	}

	batch, err := createAnnouncements(0)
	if err != nil {
		t.Fatalf("can't generate announcements: %v", err)
	}

	localKey := batch.nodeAnn1.NodeID
	remoteKey := batch.nodeAnn2.NodeID

	// Recreate the case where the remote node is snding us its ChannelUpdate
	// before we have been able to process our own ChannelAnnouncement and
	// ChannelUpdate.
	err = <-ctx.gossiper.ProcessRemoteAnnouncement(batch.chanUpdAnn2, remoteKey)
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel update announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	// Since the remote ChannelUpdate was added for an edge that
	// we did not already know about, it should have been added
	// to the map of premature ChannelUpdates. Check that nothing
	// was added to the graph.
	chanInfo, e1, e2, err := ctx.router.GetChannelByID(batch.chanUpdAnn1.ShortChannelID)
	if err != channeldb.ErrEdgeNotFound {
		t.Fatalf("Expected ErrEdgeNotFound, got: %v", err)
	}
	if chanInfo != nil {
		t.Fatalf("chanInfo was not nil")
	}
	if e1 != nil {
		t.Fatalf("e1 was not nil")
	}
	if e2 != nil {
		t.Fatalf("e2 was not nil")
	}

	// Recreate lightning network topology. Initialize router with channel
	// between two nodes.
	err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.localChanAnn, localKey)
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.chanUpdAnn1, localKey)
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel update announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	// The local ChannelUpdate should now be sent directly to the remote peer,
	// such that the edge can be used for routing, regardless if this channel
	// is announced or not (private channel).
	select {
	case msg := <-sentMsgs:
		if msg != batch.chanUpdAnn1 {
			t.Fatalf("expected local channel update, instead got %v", msg)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("gossiper did not send channel update to peer")
	}

	// At this point the remote ChannelUpdate we received earlier should
	// be reprocessed, as we now have the necessary edge entry in the graph.
	// Check that the ChannelEdgePolicy was added to the graph.
	chanInfo, e1, e2, err = ctx.router.GetChannelByID(batch.chanUpdAnn1.ShortChannelID)
	if err != nil {
		t.Fatalf("unable to get channel from router: %v", err)
	}
	if chanInfo == nil {
		t.Fatalf("chanInfo was nil")
	}
	if e1 == nil {
		t.Fatalf("e1 was nil")
	}
	if e2 == nil {
		t.Fatalf("e2 was nil")
	}

	// Pretending that we receive local channel announcement from funding
	// manager, thereby kick off the announcement exchange process.
	err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.localProofAnn, localKey)
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}

	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("announcements were broadcast")
	case <-time.After(2 * trickleDelay):
	}

	number := 0
	if err := ctx.gossiper.waitingProofs.ForAll(
		func(*channeldb.WaitingProof) error {
			number++
			return nil
		},
	); err != nil {
		t.Fatalf("unable to retrieve objects from store: %v", err)
	}

	if number != 1 {
		t.Fatal("wrong number of objects in storage")
	}

	err = <-ctx.gossiper.ProcessRemoteAnnouncement(batch.remoteProofAnn, remoteKey)
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}

	for i := 0; i < 3; i++ {
		select {
		case <-ctx.broadcastedMessage:
		case <-time.After(time.Second):
			t.Fatal("announcement wasn't broadcast")
		}
	}

	number = 0
	if err := ctx.gossiper.waitingProofs.ForAll(
		func(*channeldb.WaitingProof) error {
			number++
			return nil
		},
	); err != nil && err != channeldb.ErrWaitingProofNotFound {
		t.Fatalf("unable to retrieve objects from store: %v", err)
	}

	if number != 0 {
		t.Fatal("waiting proof should be removed from storage")
	}
}
