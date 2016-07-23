package lnwallet

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/boltdb/bolt"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcutil/txsort"

	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/rpctest"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
	"github.com/roasbeef/btcutil/coinset"
	"github.com/roasbeef/btcwallet/waddrmgr"
)

var (
	privPass = []byte("private-test")

	// For simplicity a single priv key controls all of our test outputs.
	testWalletPrivKey = []byte{
		0x2b, 0xd8, 0x06, 0xc9, 0x7f, 0x0e, 0x00, 0xaf,
		0x1a, 0x1f, 0xc3, 0x32, 0x8f, 0xa7, 0x63, 0xa9,
		0x26, 0x97, 0x23, 0xc8, 0xdb, 0x8f, 0xac, 0x4f,
		0x93, 0xaf, 0x71, 0xdb, 0x18, 0x6d, 0x6e, 0x90,
	}

	// We're alice :)
	bobsPrivKey = []byte{
		0x81, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
		0x63, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
		0xd, 0xe7, 0x95, 0xe4, 0xb7, 0x25, 0xb8, 0x4d,
		0x1e, 0xb, 0x4c, 0xfd, 0x9e, 0xc5, 0x8c, 0xe9,
	}

	// Use a hard-coded HD seed.
	testHdSeed = [32]byte{
		0xb7, 0x94, 0x38, 0x5f, 0x2d, 0x1e, 0xf7, 0xab,
		0x4d, 0x92, 0x73, 0xd1, 0x90, 0x63, 0x81, 0xb4,
		0x4f, 0x2f, 0x6f, 0x25, 0x88, 0xa3, 0xef, 0xb9,
		0x6a, 0x49, 0x18, 0x83, 0x31, 0x98, 0x47, 0x53,
	}

	// The number of confirmations required to consider any created channel
	// open.
	numReqConfs = uint16(1)
)

// assertProperBalance asserts than the total value of the unspent outputs
// within the wallet are *exactly* amount. If unable to retrieve the current
// balance, or the assertion fails, the test will halt with a fatal error.
func assertProperBalance(t *testing.T, lw *LightningWallet, numConfirms int32, amount int64) {
	balance, err := lw.CalculateBalance(numConfirms)
	if err != nil {
		t.Fatalf("unable to query for balance: %v", err)
	}
	if balance != btcutil.Amount(amount*1e8) {
		t.Fatalf("wallet credits not properly loaded, should have 40BTC, "+
			"instead have %v", balance)
	}
}

func assertChannelOpen(t *testing.T, miner *rpctest.Harness, numConfs uint32,
	c <-chan *LightningChannel) *LightningChannel {
	// Mine a single block. After this block is mined, the channel should
	// be considered fully open.
	if _, err := miner.Node.Generate(1); err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}
	select {
	case lnc := <-c:
		return lnc
	case <-time.After(time.Second * 5):
		t.Fatalf("channel never opened")
		return nil
	}

	return nil
}

// bobNode represents the other party involved as a node within LN. Bob is our
// only "default-route", we have a direct connection with him.
type bobNode struct {
	privKey *btcec.PrivateKey

	// For simplicity, used for both the commit tx and the multi-sig output.
	channelKey      *btcec.PublicKey
	deliveryAddress btcutil.Address
	revocation      [32]byte
	delay           uint32
	id              [wire.HashSize]byte

	availableOutputs []*wire.TxIn
	changeOutputs    []*wire.TxOut
	fundingAmt       btcutil.Amount
}

// Contribution returns bobNode's contribution necessary to open a payment
// channel with Alice.
func (b *bobNode) Contribution(aliceCommitKey *btcec.PublicKey) *ChannelContribution {
	revokeKey := deriveRevocationPubkey(aliceCommitKey, b.revocation[:])
	return &ChannelContribution{
		FundingAmount:   b.fundingAmt,
		Inputs:          b.availableOutputs,
		ChangeOutputs:   b.changeOutputs,
		MultiSigKey:     b.channelKey,
		CommitKey:       b.channelKey,
		DeliveryAddress: b.deliveryAddress,
		RevocationKey:   revokeKey,
		CsvDelay:        b.delay,
	}
}

// SingleContribution returns bobNode's contribution to a single funded
// channel. This contribution contains no inputs nor change outputs.
func (b *bobNode) SingleContribution(aliceCommitKey *btcec.PublicKey) *ChannelContribution {
	revokeKey := deriveRevocationPubkey(aliceCommitKey, b.revocation[:])
	return &ChannelContribution{
		FundingAmount:   b.fundingAmt,
		MultiSigKey:     b.channelKey,
		CommitKey:       b.channelKey,
		DeliveryAddress: b.deliveryAddress,
		RevocationKey:   revokeKey,
		CsvDelay:        b.delay,
	}
}

// signFundingTx generates signatures for all the inputs in the funding tx
// belonging to Bob.
// NOTE: This generates the full witness stack.
func (b *bobNode) signFundingTx(fundingTx *wire.MsgTx) ([]*InputScript, error) {
	bobInputScripts := make([]*InputScript, 0, len(b.availableOutputs))
	bobPkScript := b.changeOutputs[0].PkScript

	inputValue := int64(7e8)
	hashCache := txscript.NewTxSigHashes(fundingTx)
	for i, _ := range fundingTx.TxIn {
		// Alice has already signed this input.
		if fundingTx.TxIn[i].Witness != nil {
			continue
		}

		witness, err := txscript.WitnessScript(fundingTx, hashCache, i,
			inputValue, bobPkScript, txscript.SigHashAll, b.privKey,
			true)
		if err != nil {
			return nil, err
		}

		inputScript := &InputScript{Witness: witness}
		bobInputScripts = append(bobInputScripts, inputScript)
	}

	return bobInputScripts, nil
}

// signCommitTx generates a raw signature required for generating a spend from
// the funding transaction.
func (b *bobNode) signCommitTx(commitTx *wire.MsgTx, fundingScript []byte,
	channelValue int64) ([]byte, error) {

	hashCache := txscript.NewTxSigHashes(commitTx)

	return txscript.RawTxInWitnessSignature(commitTx, hashCache, 0,
		channelValue, fundingScript, txscript.SigHashAll, b.privKey)
}

// newBobNode generates a test "ln node" to interact with Alice (us). For the
// funding transaction, bob has a single output totaling 7BTC. For our basic
// test, he'll fund the channel with 5BTC, leaving 2BTC to the change output.
// TODO(roasbeef): proper handling of change etc.
func newBobNode(miner *rpctest.Harness, amt btcutil.Amount) (*bobNode, error) {
	// First, parse Bob's priv key in order to obtain a key he'll use for the
	// multi-sig funding transaction.
	privKey, pubKey := btcec.PrivKeyFromBytes(btcec.S256(), bobsPrivKey)

	// Next, generate an output redeemable by bob.
	pkHash := btcutil.Hash160(pubKey.SerializeCompressed())
	bobAddr, err := btcutil.NewAddressWitnessPubKeyHash(
		pkHash,
		miner.ActiveNet)
	if err != nil {
		return nil, err
	}
	bobAddrScript, err := txscript.PayToAddrScript(bobAddr)
	if err != nil {
		return nil, err
	}

	// Give bobNode one 7 BTC output for use in creating channels.
	output := &wire.TxOut{7e8, bobAddrScript}
	mainTxid, err := miner.CoinbaseSpend([]*wire.TxOut{output})
	if err != nil {
		return nil, err
	}

	// Mine a block in order to include the above output in a block. During
	// the reservation workflow, we currently test to ensure that the funding
	// output we're given actually exists.
	if _, err := miner.Node.Generate(1); err != nil {
		return nil, err
	}

	// Grab the transaction in order to locate the output index to Bob.
	tx, err := miner.Node.GetRawTransaction(mainTxid)
	if err != nil {
		return nil, err
	}
	found, index := findScriptOutputIndex(tx.MsgTx(), bobAddrScript)
	if !found {
		return nil, fmt.Errorf("output to bob never created")
	}

	prevOut := wire.NewOutPoint(mainTxid, index)
	bobTxIn := wire.NewTxIn(prevOut, nil, nil)

	// Using bobs priv key above, create a change output he can spend.
	bobChangeOutput := wire.NewTxOut(2*1e8, bobAddrScript)

	// Bob's initial revocation hash is just his private key with the first
	// byte changed...
	var revocation [32]byte
	copy(revocation[:], bobsPrivKey)
	revocation[0] = 0xff

	// His ID is just as creative...
	var id [wire.HashSize]byte
	id[0] = 0xff

	return &bobNode{
		id:               id,
		privKey:          privKey,
		channelKey:       pubKey,
		deliveryAddress:  bobAddr,
		revocation:       revocation,
		fundingAmt:       amt,
		delay:            5,
		availableOutputs: []*wire.TxIn{bobTxIn},
		changeOutputs:    []*wire.TxOut{bobChangeOutput},
	}, nil
}

func loadTestCredits(miner *rpctest.Harness, w *LightningWallet, numOutputs, btcPerOutput int) error {
	// Using the mining node, spend from a coinbase output numOutputs to
	// give us btcPerOutput with each output.
	satoshiPerOutput := int64(btcPerOutput * 1e8)
	addrs := make([]btcutil.Address, 0, numOutputs)
	for i := 0; i < numOutputs; i++ {
		// Grab a fresh address from the wallet to house this output.
		walletAddr, err := w.NewAddress(waddrmgr.DefaultAccountNum, waddrmgr.WitnessPubKey)
		if err != nil {
			return err
		}

		script, err := txscript.PayToAddrScript(walletAddr)
		if err != nil {
			return err
		}

		addrs = append(addrs, walletAddr)

		output := &wire.TxOut{satoshiPerOutput, script}
		if _, err := miner.CoinbaseSpend([]*wire.TxOut{output}); err != nil {
			return err
		}
	}

	// TODO(roasbeef): shouldn't hardcode 10, use config param that dictates
	// how many confs we wait before opening a channel.
	// Generate 10 blocks with the mining node, this should mine all
	// numOutputs transactions created above. We generate 10 blocks here
	// in order to give all the outputs a "sufficient" number of confirmations.
	if _, err := miner.Node.Generate(10); err != nil {
		return err
	}

	_, bestHeight, err := miner.Node.GetBestBlock()
	if err != nil {
		return err
	}

	// Wait until the wallet has finished syncing up to the main chain.
	ticker := time.NewTicker(100 * time.Millisecond)
out:
	for {
		select {
		case <-ticker.C:
			if w.Manager.SyncedTo().Height == bestHeight {
				break out
			}
		}
	}
	ticker.Stop()

	// Trigger a re-scan to ensure the wallet knows of the newly created
	// outputs it can spend.
	if err := w.Rescan(addrs, nil); err != nil {
		return err
	}

	return nil
}

// createTestWallet creates a test LightningWallet will a total of 20BTC
// available for funding channels.
func createTestWallet(miningNode *rpctest.Harness, netParams *chaincfg.Params) (string, *LightningWallet, error) {
	privPass := []byte("private-test")
	tempTestDir, err := ioutil.TempDir("", "lnwallet")
	if err != nil {
		return "", nil, nil
	}

	rpcConfig := miningNode.RPCConfig()
	config := &Config{
		PrivatePass: privPass,
		HdSeed:      testHdSeed[:],
		DataDir:     tempTestDir,
		NetParams:   netParams,
		RpcHost:     rpcConfig.Host,
		RpcUser:     rpcConfig.User,
		RpcPass:     rpcConfig.Pass,
		CACert:      rpcConfig.Certificates,
	}

	dbDir := filepath.Join(tempTestDir, "cdb")
	cdb, err := channeldb.Open(dbDir, &chaincfg.SegNet4Params)
	if err != nil {
		return "", nil, err
	}

	wallet, err := NewLightningWallet(config, cdb)
	if err != nil {
		return "", nil, err
	}
	if err := wallet.Startup(); err != nil {
		return "", nil, err
	}

	cdb.RegisterCryptoSystem(&WaddrmgrEncryptorDecryptor{wallet.Manager})

	// Load our test wallet with 10 outputs each holding 4BTC.
	if err := loadTestCredits(miningNode, wallet, 10, 4); err != nil {
		return "", nil, err
	}

	return tempTestDir, wallet, nil
}

func testDualFundingReservationWorkflow(miner *rpctest.Harness, lnwallet *LightningWallet, t *testing.T) {
	// Create the bob-test wallet which will be the other side of our funding
	// channel.
	fundingAmount := btcutil.Amount(5 * 1e8)
	bobNode, err := newBobNode(miner, fundingAmount)
	if err != nil {
		t.Fatalf("unable to create bob node: %v", err)
	}

	// Bob initiates a channel funded with 5 BTC for each side, so 10
	// BTC total. He also generates 2 BTC in change.
	chanReservation, err := lnwallet.InitChannelReservation(fundingAmount*2,
		fundingAmount, bobNode.id, numReqConfs, 4)
	if err != nil {
		t.Fatalf("unable to initialize funding reservation: %v", err)
	}

	// The channel reservation should now be populated with a multi-sig key
	// from our HD chain, a change output with 3 BTC, and 2 outputs selected
	// of 4 BTC each. Additionally, the rest of the items needed to fufill a
	// funding contribution should also have been filled in.
	ourContribution := chanReservation.OurContribution()
	if len(ourContribution.Inputs) != 2 {
		t.Fatalf("outputs for funding tx not properly selected, have %v "+
			"outputs should have 2", len(ourContribution.Inputs))
	}
	if ourContribution.MultiSigKey == nil {
		t.Fatalf("alice's key for multi-sig not found")
	}
	if ourContribution.CommitKey == nil {
		t.Fatalf("alice's key for commit not found")
	}
	if ourContribution.DeliveryAddress == nil {
		t.Fatalf("alice's final delivery address not found")
	}
	if ourContribution.CsvDelay == 0 {
		t.Fatalf("csv delay not set")
	}

	// Bob sends over his output, change addr, pub keys, initial revocation,
	// final delivery address, and his accepted csv delay for the
	// commitment transactions.
	bobContribution := bobNode.Contribution(ourContribution.CommitKey)
	if err := chanReservation.ProcessContribution(bobContribution); err != nil {
		t.Fatalf("unable to add bob's funds to the funding tx: %v", err)
	}

	// At this point, the reservation should have our signatures, and a
	// partial funding transaction (missing bob's sigs).
	theirContribution := chanReservation.TheirContribution()
	ourFundingSigs, ourCommitSig := chanReservation.OurSignatures()
	if len(ourFundingSigs) != 2 {
		t.Fatalf("only %v of our sigs present, should have 2",
			len(ourFundingSigs))
	}
	if ourCommitSig == nil {
		t.Fatalf("commitment sig not found")
	}
	if ourContribution.RevocationKey == nil {
		t.Fatalf("alice's revocation key not found")
	}
	// Additionally, the funding tx should have been populated.
	if chanReservation.fundingTx == nil {
		t.Fatalf("funding transaction never created!")
	}
	// Their funds should also be filled in.
	if len(theirContribution.Inputs) != 1 {
		t.Fatalf("bob's outputs for funding tx not properly selected, have %v "+
			"outputs should have 2", len(theirContribution.Inputs))
	}
	if theirContribution.ChangeOutputs[0].Value != 2e8 {
		t.Fatalf("bob should have one change output with value 2e8"+
			"satoshis, is instead %v",
			theirContribution.ChangeOutputs[0].Value)
	}
	if theirContribution.MultiSigKey == nil {
		t.Fatalf("bob's key for multi-sig not found")
	}
	if theirContribution.CommitKey == nil {
		t.Fatalf("bob's key for commit tx not found")
	}
	if theirContribution.DeliveryAddress == nil {
		t.Fatalf("bob's final delivery address not found")
	}
	if theirContribution.RevocationKey == nil {
		t.Fatalf("bob's revocaiton key not found")
	}

	// Alice responds with her output, change addr, multi-sig key and signatures.
	// Bob then responds with his signatures.
	bobsSigs, err := bobNode.signFundingTx(chanReservation.fundingTx)
	if err != nil {
		t.Fatalf("unable to sign inputs for bob: %v", err)
	}
	commitSig, err := bobNode.signCommitTx(
		chanReservation.partialState.OurCommitTx,
		chanReservation.partialState.FundingRedeemScript,
		10e8)
	if err != nil {
		t.Fatalf("bob is unable to sign alice's commit tx: %v", err)
	}
	if err := chanReservation.CompleteReservation(bobsSigs, commitSig); err != nil {
		t.Fatalf("unable to complete funding tx: %v", err)
	}

	// At this point, the channel can be considered "open" when the funding
	// txn hits a "comfortable" depth.

	// The resulting active channel state should have been persisted to the DB.
	fundingTx := chanReservation.FinalFundingTx()
	fundingSha := fundingTx.TxSha()
	nodeID := wire.ShaHash(bobNode.id)
	channels, err := lnwallet.channelDB.FetchOpenChannels(&nodeID)
	if err != nil {
		t.Fatalf("unable to retrieve channel from DB: %v", err)
	}
	if !bytes.Equal(channels[0].FundingOutpoint.Hash[:], fundingSha[:]) {
		t.Fatalf("channel state not properly saved")
	}

	// Assert that tha channel opens after a single block.
	lnc := assertChannelOpen(t, miner, uint32(numReqConfs),
		chanReservation.DispatchChan())

	// Now that the channel is open, execute a cooperative closure of the
	// now open channel.
	aliceCloseSig, _, err := lnc.InitCooperativeClose()
	if err != nil {
		t.Fatalf("unable to init cooperative closure: %v", err)
	}

	// Obtain bob's signature for the closure transaction.
	redeemScript := lnc.channelState.FundingRedeemScript
	fundingOut := lnc.ChannelPoint()
	fundingTxIn := wire.NewTxIn(fundingOut, nil, nil)
	bobCloseTx := createCooperativeCloseTx(fundingTxIn,
		lnc.channelState.TheirBalance, lnc.channelState.OurBalance,
		lnc.channelState.TheirDeliveryScript, lnc.channelState.OurDeliveryScript,
		false)
	bobSig, err := bobNode.signCommitTx(bobCloseTx,
		redeemScript,
		int64(lnc.channelState.Capacity))
	if err != nil {
		t.Fatalf("unable to generate bob's signature for closing tx: %v", err)
	}

	// Broadcast the transaction to the network. This transaction should
	// be accepted, and found in the next mined block.
	ourKey := lnc.channelState.OurMultiSigKey.PubKey().SerializeCompressed()
	theirKey := lnc.channelState.TheirMultiSigKey.SerializeCompressed()
	witness := spendMultiSig(redeemScript, ourKey, aliceCloseSig,
		theirKey, bobSig)
	bobCloseTx.TxIn[0].Witness = witness
	if err := lnwallet.PublishTransaction(bobCloseTx); err != nil {
		t.Fatalf("broadcast of close tx rejected: %v", err)
	}
}

func testFundingTransactionLockedOutputs(miner *rpctest.Harness, lnwallet *LightningWallet, t *testing.T) {
	// Create two channels, both asking for 8 BTC each, totalling 16
	// BTC.
	// TODO(roasbeef): tests for concurrent funding.
	//  * also func for below
	fundingAmount := btcutil.Amount(8 * 1e8)
	chanReservation1, err := lnwallet.InitChannelReservation(fundingAmount,
		fundingAmount, testHdSeed, numReqConfs, 4)
	if err != nil {
		t.Fatalf("unable to initialize funding reservation 1: %v", err)
	}
	chanReservation2, err := lnwallet.InitChannelReservation(fundingAmount,
		fundingAmount, testHdSeed, numReqConfs, 4)
	if err != nil {
		t.Fatalf("unable to initialize funding reservation 2: %v", err)
	}

	ourContribution1 := chanReservation1.OurContribution()
	if len(ourContribution1.Inputs) != 3 {
		t.Fatalf("outputs for funding tx not properly selected, has %v "+
			"outputs should have 3", len(ourContribution1.Inputs))
	}
	if len(ourContribution1.ChangeOutputs) != 1 {
		t.Fatalf("funding transaction should have one change output, instead has %v",
			len(ourContribution1.ChangeOutputs))
	}
	ourContribution2 := chanReservation2.OurContribution()
	if len(ourContribution2.Inputs) != 3 {
		t.Fatalf("outputs for funding tx not properly selected, have %v "+
			"outputs should have 3", len(ourContribution2.Inputs))
	}
	if len(ourContribution2.ChangeOutputs) != 1 {
		t.Fatalf("funding transaction should have one change, instead has %v",
			len(ourContribution2.ChangeOutputs))
	}

	// Now attempt to reserve funds for another channel, this time requesting
	// 90 BTC. We only have around 24BTC worth of outpoints that aren't locked, so
	// this should fail.
	amt := btcutil.Amount(90 * 1e8)
	failedReservation, err := lnwallet.InitChannelReservation(amt, amt,
		testHdSeed, numReqConfs, 4)
	if err == nil {
		t.Fatalf("not error returned, should fail on coin selection")
	}
	if err != coinset.ErrCoinsNoSelectionAvailable {
		t.Fatalf("error not coinselect error: %v", err)
	}
	if failedReservation != nil {
		t.Fatalf("reservation should be nil")
	}
}

func testFundingCancellationNotEnoughFunds(miner *rpctest.Harness, lnwallet *LightningWallet, t *testing.T) {
	// Create a reservation for 22 BTC.
	fundingAmount := btcutil.Amount(22 * 1e8)
	chanReservation, err := lnwallet.InitChannelReservation(fundingAmount,
		fundingAmount, testHdSeed, numReqConfs, 4)
	if err != nil {
		t.Fatalf("unable to initialize funding reservation: %v", err)
	}

	// There should be three locked outpoints.
	lockedOutPoints := lnwallet.LockedOutpoints()
	if len(lockedOutPoints) != 6 {
		t.Fatalf("two outpoints should now be locked, instead %v are",
			len(lockedOutPoints))
	}

	// Attempt to create another channel with 22 BTC, this should fail.
	failedReservation, err := lnwallet.InitChannelReservation(fundingAmount,
		fundingAmount, testHdSeed, numReqConfs, 4)
	if err != coinset.ErrCoinsNoSelectionAvailable {
		t.Fatalf("coin selection succeded should have insufficient funds: %+v",
			failedReservation)
	}

	// Now cancel that old reservation.
	if err := chanReservation.Cancel(); err != nil {
		t.Fatalf("unable to cancel reservation: %v", err)
	}

	// Those outpoints should no longer be locked.
	lockedOutPoints = lnwallet.LockedOutpoints()
	if len(lockedOutPoints) != 0 {
		t.Fatalf("outpoints still locked")
	}

	// Reservation ID should now longer be tracked.
	_, ok := lnwallet.fundingLimbo[chanReservation.reservationID]
	if ok {
		t.Fatalf("funding reservation still in map")
	}

	// TODO(roasbeef): create method like Balance that ignores locked
	// outpoints, will let us fail early/fast instead of querying and
	// attempting coin selection.

	// Request to fund a new channel should now succeeed.
	_, err = lnwallet.InitChannelReservation(fundingAmount, fundingAmount,
		testHdSeed, numReqConfs, 4)
	if err != nil {
		t.Fatalf("unable to initialize funding reservation: %v", err)
	}
}

func testCancelNonExistantReservation(miner *rpctest.Harness, lnwallet *LightningWallet, t *testing.T) {
	// Create our own reservation, give it some ID.
	res := newChannelReservation(1000, 1000, 5000, lnwallet, 22, numReqConfs)

	// Attempt to cancel this reservation. This should fail, we know
	// nothing of it.
	if err := res.Cancel(); err == nil {
		t.Fatalf("cancelled non-existant reservation")
	}
}

func testSingleFunderReservationWorkflowInitiator(miner *rpctest.Harness, lnwallet *LightningWallet, t *testing.T) {
	// For this scenario, we (lnwallet) will be the channel initiator while bob
	// will be the recipient.

	// Create the bob-test wallet which will be the other side of our funding
	// channel.
	bobNode, err := newBobNode(miner, 0)
	if err != nil {
		t.Fatalf("unable to create bob node: %v", err)
	}

	// Initialize a reservation for a channel with 4 BTC funded solely by us.
	fundingAmt := btcutil.Amount(4 * 1e8)
	chanReservation, err := lnwallet.InitChannelReservation(fundingAmt,
		fundingAmt, bobNode.id, numReqConfs, 4)
	if err != nil {
		t.Fatalf("unable to init channel reservation: %v", err)
	}

	// Verify all contribution fields have been set properly.
	ourContribution := chanReservation.OurContribution()
	if len(ourContribution.Inputs) < 1 {
		t.Fatalf("outputs for funding tx not properly selected, have %v "+
			"outputs should at least 1", len(ourContribution.Inputs))
	}
	if len(ourContribution.ChangeOutputs) != 1 {
		t.Fatalf("coin selection failed, should have one change outputs, "+
			"instead have: %v", len(ourContribution.ChangeOutputs))
	}
	if ourContribution.MultiSigKey == nil {
		t.Fatalf("alice's key for multi-sig not found")
	}
	if ourContribution.CommitKey == nil {
		t.Fatalf("alice's key for commit not found")
	}
	if ourContribution.DeliveryAddress == nil {
		t.Fatalf("alice's final delivery address not found")
	}
	if ourContribution.CsvDelay == 0 {
		t.Fatalf("csv delay not set")
	}

	// At this point bob now responds to our request with a response
	// containing his channel contribution. The contribution will have no
	// inputs, only a multi-sig key, csv delay, etc.
	bobContribution := bobNode.SingleContribution(ourContribution.CommitKey)
	if err := chanReservation.ProcessContribution(bobContribution); err != nil {
		t.Fatalf("unable to add bob's contribution: %v", err)
	}

	// At this point, the reservation should have our signatures, and a
	// partial funding transaction (missing bob's sigs).
	theirContribution := chanReservation.TheirContribution()
	ourFundingSigs, ourCommitSig := chanReservation.OurSignatures()
	if len(ourFundingSigs) != 2 {
		t.Fatalf("only %v of our sigs present, should have 2",
			len(ourFundingSigs))
	}
	if ourCommitSig == nil {
		t.Fatalf("commitment sig not found")
	}
	// Additionally, the funding tx should have been populated.
	if chanReservation.fundingTx == nil {
		t.Fatalf("funding transaction never created!")
	}
	// Their funds should also be filled in.
	if len(theirContribution.Inputs) != 0 {
		t.Fatalf("bob shouldn't have any inputs, instead has %v",
			len(theirContribution.Inputs))
	}
	if len(theirContribution.ChangeOutputs) != 0 {
		t.Fatalf("bob shouldn't have any change outputs, instead "+
			"has %v", theirContribution.ChangeOutputs[0].Value)
	}
	if ourContribution.RevocationKey == nil {
		t.Fatalf("alice's revocation hash not found")
	}
	if theirContribution.MultiSigKey == nil {
		t.Fatalf("bob's key for multi-sig not found")
	}
	if theirContribution.CommitKey == nil {
		t.Fatalf("bob's key for commit tx not found")
	}
	if theirContribution.DeliveryAddress == nil {
		t.Fatalf("bob's final delivery address not found")
	}
	if theirContribution.RevocationKey == nil {
		t.Fatalf("bob's revocaiton hash not found")
	}

	// With this contribution processed, we're able to create the
	// funding+commitment transactions, as well as generate a signature
	// for bob's version of the commitment transaction.
	//
	// Now Bob can generate a signature for our version of the commitment
	// transaction, allowing us to complete the reservation.
	bobCommitSig, err := bobNode.signCommitTx(
		chanReservation.partialState.OurCommitTx,
		chanReservation.partialState.FundingRedeemScript,
		int64(fundingAmt))
	if err != nil {
		t.Fatalf("bob is unable to sign alice's commit tx: %v", err)
	}
	if err := chanReservation.CompleteReservation(nil, bobCommitSig); err != nil {
		t.Fatalf("unable to complete funding tx: %v", err)
	}

	// TODO(roasbeef): verify our sig for bob's once sighash change is
	// merged.

	// The resulting active channel state should have been persisted to the DB.
	// TODO(roasbeef): de-duplicate
	fundingTx := chanReservation.FinalFundingTx()
	fundingSha := fundingTx.TxSha()
	nodeID := wire.ShaHash(bobNode.id)
	channels, err := lnwallet.channelDB.FetchOpenChannels(&nodeID)
	if err != nil {
		t.Fatalf("unable to retrieve channel from DB: %v", err)
	}
	if !bytes.Equal(channels[0].FundingOutpoint.Hash[:], fundingSha[:]) {
		t.Fatalf("channel state not properly saved: %v vs %v",
			hex.EncodeToString(channels[0].FundingOutpoint.Hash[:]),
			hex.EncodeToString(fundingSha[:]))
	}

	assertChannelOpen(t, miner, uint32(numReqConfs), chanReservation.DispatchChan())
}

func testSingleFunderReservationWorkflowResponder(miner *rpctest.Harness, lnwallet *LightningWallet, t *testing.T) {
	// For this scenario, bob will initiate the channel, while we simply act as
	// the responder.
	capacity := btcutil.Amount(4 * 1e8)

	// Create the bob-test wallet which will be initiator of a single
	// funder channel shortly.
	bobNode, err := newBobNode(miner, capacity)
	if err != nil {
		t.Fatalf("unable to create bob node: %v", err)
	}

	// Bob sends over a single funding request, so we allocate our
	// contribution and the necessary resources.
	fundingAmt := btcutil.Amount(0)
	chanReservation, err := lnwallet.InitChannelReservation(capacity,
		fundingAmt, bobNode.id, numReqConfs, 4)
	if err != nil {
		t.Fatalf("unable to init channel reservation: %v", err)
	}

	// Verify all contribution fields have been set properly. Since we are
	// the recipient of a single-funder channel, we shouldn't have selected
	// any coins or generated any change outputs.
	ourContribution := chanReservation.OurContribution()
	if len(ourContribution.Inputs) != 0 {
		t.Fatalf("outputs for funding tx not properly selected, have %v "+
			"outputs should have 0", len(ourContribution.Inputs))
	}
	if len(ourContribution.ChangeOutputs) != 0 {
		t.Fatalf("coin selection failed, should have no change outputs, "+
			"instead have: %v", ourContribution.ChangeOutputs[0].Value)
	}
	if ourContribution.MultiSigKey == nil {
		t.Fatalf("alice's key for multi-sig not found")
	}
	if ourContribution.CommitKey == nil {
		t.Fatalf("alice's key for commit not found")
	}
	if ourContribution.DeliveryAddress == nil {
		t.Fatalf("alice's final delivery address not found")
	}
	if ourContribution.CsvDelay == 0 {
		t.Fatalf("csv delay not set")
	}

	// Next we process Bob's single funder contribution which doesn't
	// include any inputs or change addresses, as only Bob will construct
	// the funding transaction.
	bobContribution := bobNode.Contribution(ourContribution.CommitKey)
	if err := chanReservation.ProcessSingleContribution(bobContribution); err != nil {
		t.Fatalf("unable to process bob's contribution: %v", err)
	}
	if chanReservation.fundingTx != nil {
		t.Fatalf("funding transaction populated!")
	}
	if len(bobContribution.Inputs) != 1 {
		t.Fatalf("bob shouldn't have one inputs, instead has %v",
			len(bobContribution.Inputs))
	}
	if ourContribution.RevocationKey == nil {
		t.Fatalf("alice's revocation key not found")
	}
	if len(bobContribution.ChangeOutputs) != 1 {
		t.Fatalf("bob shouldn't have one change output, instead "+
			"has %v", len(bobContribution.ChangeOutputs))
	}
	if bobContribution.MultiSigKey == nil {
		t.Fatalf("bob's key for multi-sig not found")
	}
	if bobContribution.CommitKey == nil {
		t.Fatalf("bob's key for commit tx not found")
	}
	if bobContribution.DeliveryAddress == nil {
		t.Fatalf("bob's final delivery address not found")
	}
	if bobContribution.RevocationKey == nil {
		t.Fatalf("bob's revocaiton key not found")
	}

	fundingRedeemScript, multiOut, err := genFundingPkScript(
		ourContribution.MultiSigKey.SerializeCompressed(),
		bobContribution.MultiSigKey.SerializeCompressed(),
		int64(capacity))
	if err != nil {
		t.Fatalf("unable to generate multi-sig output: %v", err)
	}

	// At this point, we send Bob our contribution, allowing him to
	// construct the funding transaction, and sign our version of the
	// commitment transaction.
	fundingTx := wire.NewMsgTx()
	fundingTx.AddTxIn(bobNode.availableOutputs[0])
	fundingTx.AddTxOut(bobNode.changeOutputs[0])
	fundingTx.AddTxOut(multiOut)
	txsort.InPlaceSort(fundingTx)
	if _, err := bobNode.signFundingTx(fundingTx); err != nil {
		t.Fatalf("unable to generate bob's funding sigs: %v", err)
	}

	// Locate the output index of the 2-of-2 in order to send back to the
	// wallet so it can finalize the transaction by signing bob's commitment
	// transaction.
	fundingTxID := fundingTx.TxSha()
	_, multiSigIndex := findScriptOutputIndex(fundingTx, multiOut.PkScript)
	fundingOutpoint := wire.NewOutPoint(&fundingTxID, multiSigIndex)

	fundingTxIn := wire.NewTxIn(fundingOutpoint, nil, nil)
	aliceCommitTx, err := createCommitTx(fundingTxIn, ourContribution.CommitKey,
		bobContribution.CommitKey, ourContribution.RevocationKey,
		ourContribution.CsvDelay, 0, capacity)
	if err != nil {
		t.Fatalf("unable to create alice's commit tx: %v", err)
	}
	txsort.InPlaceSort(aliceCommitTx)
	bobCommitSig, err := bobNode.signCommitTx(aliceCommitTx,
		fundingRedeemScript, int64(capacity))
	if err != nil {
		t.Fatalf("unable to sign alice's commit tx: %v", err)
	}

	// With this stage complete, Alice can now complete the reservation.
	bobRevokeKey := bobContribution.RevocationKey
	if err := chanReservation.CompleteReservationSingle(bobRevokeKey,
		fundingOutpoint, bobCommitSig); err != nil {
		t.Fatalf("unable to complete reservation: %v", err)
	}

	// Alice should have saved the funding output.
	if chanReservation.partialState.FundingOutpoint != fundingOutpoint {
		t.Fatalf("funding outputs don't match: %#v vs %#v",
			chanReservation.partialState.FundingOutpoint,
			fundingOutpoint)
	}

	// Some period of time later, Bob presents us with an SPV proof
	// attesting to an open channel. At this point Alice recognizes the
	// channel, saves the state to disk, and creates the channel itself.
	if _, err := chanReservation.FinalizeReservation(); err != nil {
		t.Fatalf("unable to finalize reservation: %v", err)
	}

	// TODO(roasbeef): bob verify alice's sig
}

func testFundingReservationInvalidCounterpartySigs(miner *rpctest.Harness, lnwallet *LightningWallet, t *testing.T) {
}

func testFundingTransactionTxFees(miner *rpctest.Harness, lnwallet *LightningWallet, t *testing.T) {
}

var walletTests = []func(miner *rpctest.Harness, w *LightningWallet, test *testing.T){
	testDualFundingReservationWorkflow,
	testSingleFunderReservationWorkflowInitiator,
	testSingleFunderReservationWorkflowResponder,
	testFundingTransactionLockedOutputs,
	testFundingCancellationNotEnoughFunds,
	testFundingReservationInvalidCounterpartySigs,
	testFundingTransactionLockedOutputs,
	// TODO(roasbeef):
	// * test for non-existant output given in funding tx
	// * channel open after confirmations
	// * channel update stuff
}

type testLnWallet struct {
	lnwallet    *LightningWallet
	cleanUpFunc func()
}

func clearWalletState(w *LightningWallet) error {
	w.nextFundingID = 0
	w.fundingLimbo = make(map[uint64]*ChannelReservation)
	w.ResetLockedOutpoints()

	// TODO(roasbeef): should also restore outputs to original state.

	return w.channelDB.Wipe()
}

// TODO(roasbeef): purge bobNode in favor of dual lnwallet's
func TestLightningWallet(t *testing.T) {
	netParams := &chaincfg.SimNetParams

	// Initialize the harness around a btcd node which will serve as our
	// dedicated miner to generate blocks, cause re-orgs, etc. We'll set
	// up this node with a chain length of 125, so we have plentyyy of BTC
	// to play around with.
	miningNode, err := rpctest.New(netParams, nil, nil)
	defer miningNode.TearDown()
	if err != nil {
		t.Fatalf("unable to create mining node: %v", err)
	}
	if err := miningNode.SetUp(true, 25); err != nil {
		t.Fatalf("unable to set up mining node: %v", err)
	}

	// Funding via 10 outputs with 4BTC each.
	testDir, lnwallet, err := createTestWallet(miningNode, netParams)
	if err != nil {
		t.Fatalf("unable to create test ln wallet: %v", err)
	}
	defer os.RemoveAll(testDir)
	defer lnwallet.Shutdown()

	// The wallet should now have 40BTC available for spending.
	assertProperBalance(t, lnwallet, 1, 40)

	// Execute every test, clearing possibly mutated wallet state after
	// each step.
	for _, walletTest := range walletTests {
		walletTest(miningNode, lnwallet, t)

		// TODO(roasbeef): possible reset mining node's chainstate to
		// initial level, cleanly wipe buckets
		if err := clearWalletState(lnwallet); err != nil &&
			err != bolt.ErrBucketNotFound {
			t.Fatalf("unable to wipe wallet state: %v", err)
		}
	}
}
