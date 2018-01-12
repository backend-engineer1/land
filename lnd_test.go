// +build rpctest

package main

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sync/atomic"

	"encoding/hex"
	"reflect"

	"crypto/rand"
	prand "math/rand"

	"github.com/btcsuite/btclog"
	"github.com/davecgh/go-spew/spew"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/integration/rpctest"
	"github.com/roasbeef/btcd/rpcclient"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

var (
	harnessNetParams = &chaincfg.SimNetParams
)

// harnessTest wraps a regular testing.T providing enhanced error detection
// and propagation. All error will be augmented with a full stack-trace in
// order to aid in debugging. Additionally, any panics caused by active
// test cases will also be handled and represented as fatals.
type harnessTest struct {
	t *testing.T

	// testCase is populated during test execution and represents the
	// current test case.
	testCase *testCase
}

// newHarnessTest creates a new instance of a harnessTest from a regular
// testing.T instance.
func newHarnessTest(t *testing.T) *harnessTest {
	return &harnessTest{t, nil}
}

// Fatalf causes the current active test case to fail with a fatal error. All
// integration tests should mark test failures solely with this method due to
// the error stack traces it produces.
func (h *harnessTest) Fatalf(format string, a ...interface{}) {
	stacktrace := errors.Wrap(fmt.Sprintf(format, a...), 1).ErrorStack()

	if h.testCase != nil {
		h.t.Fatalf("Failed: (%v): exited with error: \n"+
			"%v", h.testCase.name, stacktrace)
	} else {
		h.t.Fatalf("Error outside of test: %v", stacktrace)
	}
}

// RunTestCase executes a harness test case. Any errors or panics will be
// represented as fatal.
func (h *harnessTest) RunTestCase(testCase *testCase,
	net *lntest.NetworkHarness) {

	h.testCase = testCase
	defer func() {
		h.testCase = nil
	}()

	defer func() {
		if err := recover(); err != nil {
			description := errors.Wrap(err, 2).ErrorStack()
			h.t.Fatalf("Failed: (%v) paniced with: \n%v",
				h.testCase.name, description)
		}
	}()

	testCase.test(net, h)

	return
}

func (h *harnessTest) Logf(format string, args ...interface{}) {
	h.t.Logf(format, args...)
}

func (h *harnessTest) Log(args ...interface{}) {
	h.t.Log(args...)
}

func assertTxInBlock(t *harnessTest, block *wire.MsgBlock, txid *chainhash.Hash) {
	for _, tx := range block.Transactions {
		sha := tx.TxHash()
		if bytes.Equal(txid[:], sha[:]) {
			return
		}
	}

	t.Fatalf("funding tx was not included in block")
}

// mineBlocks mine 'num' of blocks and check that blocks are present in
// node blockchain.
func mineBlocks(t *harnessTest, net *lntest.NetworkHarness, num uint32,
) []*wire.MsgBlock {

	blocks := make([]*wire.MsgBlock, num)

	blockHashes, err := net.Miner.Node.Generate(num)
	if err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}

	for i, blockHash := range blockHashes {
		block, err := net.Miner.Node.GetBlock(blockHash)
		if err != nil {
			t.Fatalf("unable to get block: %v", err)
		}

		blocks[i] = block
	}

	return blocks
}

// openChannelAndAssert attempts to open a channel with the specified
// parameters extended from Alice to Bob. Additionally, two items are asserted
// after the channel is considered open: the funding transaction should be
// found within a block, and that Alice can report the status of the new
// channel.
func openChannelAndAssert(ctx context.Context, t *harnessTest,
	net *lntest.NetworkHarness, alice, bob *lntest.HarnessNode,
	fundingAmt btcutil.Amount, pushAmt btcutil.Amount) *lnrpc.ChannelPoint {

	chanOpenUpdate, err := net.OpenChannel(ctx, alice, bob, fundingAmt,
		pushAmt, false)
	if err != nil {
		t.Fatalf("unable to open channel: %v", err)
	}

	// Mine 6 blocks, then wait for Alice's node to notify us that the
	// channel has been opened. The funding transaction should be found
	// within the first newly mined block. We mine 6 blocks to make sure
	// the channel is public, as it will not be announced to the network
	// before the funding transaction is 6 blocks deep.
	block := mineBlocks(t, net, 6)[0]

	fundingChanPoint, err := net.WaitForChannelOpen(ctx, chanOpenUpdate)
	if err != nil {
		t.Fatalf("error while waiting for channel open: %v", err)
	}
	fundingTxID, err := chainhash.NewHash(fundingChanPoint.FundingTxid)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
	}
	assertTxInBlock(t, block, fundingTxID)

	// The channel should be listed in the peer information returned by
	// both peers.
	chanPoint := wire.OutPoint{
		Hash:  *fundingTxID,
		Index: fundingChanPoint.OutputIndex,
	}
	if err := net.AssertChannelExists(ctx, alice, &chanPoint); err != nil {
		t.Fatalf("unable to assert channel existence: %v", err)
	}
	if err := net.AssertChannelExists(ctx, bob, &chanPoint); err != nil {
		t.Fatalf("unable to assert channel existence: %v", err)
	}

	return fundingChanPoint
}

// closeChannelAndAssert attempts to close a channel identified by the passed
// channel point owned by the passed lighting node. A fully blocking channel
// closure is attempted, therefore the passed context should be a child derived
// via timeout from a base parent. Additionally, once the channel has been
// detected as closed, an assertion checks that the transaction is found within
// a block.
func closeChannelAndAssert(ctx context.Context, t *harnessTest,
	net *lntest.NetworkHarness, node *lntest.HarnessNode,
	fundingChanPoint *lnrpc.ChannelPoint, force bool) *chainhash.Hash {

	closeUpdates, _, err := net.CloseChannel(ctx, node, fundingChanPoint, force)
	if err != nil {
		t.Fatalf("unable to close channel: %v", err)
	}

	txid, err := chainhash.NewHash(fundingChanPoint.FundingTxid)
	if err != nil {
		t.Fatalf("unable to convert to chainhash: %v", err)
	}
	chanPointStr := fmt.Sprintf("%v:%v", txid, fundingChanPoint.OutputIndex)

	// If we didn't force close the transaction, at this point, the channel
	// should now be marked as being in the state of "pending close".
	if !force {
		pendingChansRequest := &lnrpc.PendingChannelsRequest{}
		pendingChanResp, err := node.PendingChannels(ctx, pendingChansRequest)
		if err != nil {
			t.Fatalf("unable to query for pending channels: %v", err)
		}
		var found bool
		for _, pendingClose := range pendingChanResp.PendingClosingChannels {
			if pendingClose.Channel.ChannelPoint == chanPointStr {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("channel not marked as pending close")
		}
	}

	// Finally, generate a single block, wait for the final close status
	// update, then ensure that the closing transaction was included in the
	// block.
	block := mineBlocks(t, net, 1)[0]

	closingTxid, err := net.WaitForChannelClose(ctx, closeUpdates)
	if err != nil {
		t.Fatalf("error while waiting for channel close: %v", err)
	}

	assertTxInBlock(t, block, closingTxid)

	return closingTxid
}

// numOpenChannelsPending sends an RPC request to a node to get a count of the
// node's channels that are currently in a pending state (with a broadcast, but
// not confirmed funding transaction).
func numOpenChannelsPending(ctxt context.Context, node *lntest.HarnessNode) (int, error) {
	pendingChansRequest := &lnrpc.PendingChannelsRequest{}
	resp, err := node.PendingChannels(ctxt, pendingChansRequest)
	if err != nil {
		return 0, err
	}
	return len(resp.PendingOpenChannels), nil
}

// assertNumOpenChannelsPending asserts that a pair of nodes have the expected
// number of pending channels between them.
func assertNumOpenChannelsPending(ctxt context.Context, t *harnessTest,
	alice, bob *lntest.HarnessNode, expected int) {

	const nPolls = 10

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for i := 0; i < nPolls; i++ {
		aliceNumChans, err := numOpenChannelsPending(ctxt, alice)
		if err != nil {
			t.Fatalf("error fetching alice's node (%v) pending channels %v",
				alice.NodeID, err)
		}
		bobNumChans, err := numOpenChannelsPending(ctxt, bob)
		if err != nil {
			t.Fatalf("error fetching bob's node (%v) pending channels %v",
				bob.NodeID, err)
		}

		isLastIteration := i == nPolls-1

		aliceStateCorrect := aliceNumChans == expected
		if !aliceStateCorrect && isLastIteration {
			t.Fatalf("number of pending channels for alice incorrect. "+
				"expected %v, got %v", expected, aliceNumChans)
		}

		bobStateCorrect := bobNumChans == expected
		if !bobStateCorrect && isLastIteration {
			t.Fatalf("number of pending channels for bob incorrect. "+
				"expected %v, got %v",
				expected, bobNumChans)
		}

		if aliceStateCorrect && bobStateCorrect {
			return
		}

		<-ticker.C
	}
}

// assertNumConnections asserts number current connections between two peers.
func assertNumConnections(ctxt context.Context, t *harnessTest,
	alice, bob *lntest.HarnessNode, expected int) {

	const nPolls = 10

	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()

	for i := nPolls - 1; i >= 0; i-- {
		select {
		case <-tick.C:
			aNumPeers, err := alice.ListPeers(ctxt, &lnrpc.ListPeersRequest{})
			if err != nil {
				t.Fatalf("unable to fetch alice's node (%v) list peers %v",
					alice.NodeID, err)
			}
			bNumPeers, err := bob.ListPeers(ctxt, &lnrpc.ListPeersRequest{})
			if err != nil {
				t.Fatalf("unable to fetch bob's node (%v) list peers %v",
					bob.NodeID, err)
			}
			if len(aNumPeers.Peers) != expected {
				// Continue polling if this is not the final
				// loop.
				if i > 0 {
					continue
				}
				t.Fatalf("number of peers connected to alice is incorrect: "+
					"expected %v, got %v", expected, len(aNumPeers.Peers))
			}
			if len(bNumPeers.Peers) != expected {
				// Continue polling if this is not the final
				// loop.
				if i > 0 {
					continue
				}
				t.Fatalf("number of peers connected to bob is incorrect: "+
					"expected %v, got %v", expected, len(bNumPeers.Peers))
			}

			// Alice and Bob both have the required number of
			// peers, stop polling and return to caller.
			return
		}
	}
}

// calcStaticFee calculates appropriate fees for commitment transactions.  This
// function provides a simple way to allow test balance assertions to take fee
// calculations into account.
//
// TODO(bvu): Refactor when dynamic fee estimation is added.
//
// TODO(roasbeef): can remove as fee info now exposed in listchannels?
func calcStaticFee(numHTLCs int) btcutil.Amount {
	const (
		commitWeight = btcutil.Amount(724)
		htlcWeight   = 172
		feePerKw     = btcutil.Amount(50/4) * 1000
	)
	return feePerKw * (commitWeight +
		btcutil.Amount(htlcWeight*numHTLCs)) / 1000
}

// completePaymentRequests sends payments from a lightning node to complete all
// payment requests. If the awaitResponse parameter is true, this function
// does not return until all payments successfully complete without errors.
func completePaymentRequests(ctx context.Context, client lnrpc.LightningClient,
	paymentRequests []string, awaitResponse bool) error {

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	payStream, err := client.SendPayment(ctx)
	if err != nil {
		return err
	}

	for _, payReq := range paymentRequests {
		sendReq := &lnrpc.SendRequest{PaymentRequest: payReq}
		err := payStream.Send(sendReq)
		if err != nil {
			return err
		}
	}

	if awaitResponse {
		for range paymentRequests {
			resp, err := payStream.Recv()
			if err != nil {
				return err
			}
			if resp.PaymentError != "" {
				return fmt.Errorf("received payment error: %v",
					resp.PaymentError)
			}
		}
	} else {
		// We are not waiting for feedback in the form of a response, but we
		// should still wait long enough for the server to receive and handle
		// the send before cancelling the request.
		time.Sleep(200 * time.Millisecond)
	}

	return nil
}

// testBasicChannelFunding performs a test exercising expected behavior from a
// basic funding workflow. The test creates a new channel between Alice and
// Bob, then immediately closes the channel after asserting some expected post
// conditions. Finally, the chain itself is checked to ensure the closing
// transaction was mined.
func testBasicChannelFunding(net *lntest.NetworkHarness, t *harnessTest) {
	timeout := time.Duration(time.Second * 5)
	ctxb := context.Background()

	chanAmt := maxFundingAmount
	pushAmt := btcutil.Amount(100000)

	// First establish a channel with a capacity of 0.5 BTC between Alice
	// and Bob with Alice pushing 100k satoshis to Bob's side during
	// funding. This function will block until the channel itself is fully
	// open or an error occurs in the funding process. A series of
	// assertions will be executed to ensure the funding process completed
	// successfully.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPoint := openChannelAndAssert(ctxt, t, net, net.Alice, net.Bob,
		chanAmt, pushAmt)

	ctxt, _ = context.WithTimeout(ctxb, time.Second*15)
	err := net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("alice didn't report channel: %v", err)
	}
	err = net.Bob.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("bob didn't report channel: %v", err)
	}

	// With then channel open, ensure that the amount specified above has
	// properly been pushed to Bob.
	balReq := &lnrpc.ChannelBalanceRequest{}
	aliceBal, err := net.Alice.ChannelBalance(ctxb, balReq)
	if err != nil {
		t.Fatalf("unable to get alice's balance: %v", err)
	}
	bobBal, err := net.Bob.ChannelBalance(ctxb, balReq)
	if err != nil {
		t.Fatalf("unable to get bobs's balance: %v", err)
	}
	if aliceBal.Balance != int64(chanAmt-pushAmt-calcStaticFee(0)) {
		t.Fatalf("alice's balance is incorrect: expected %v got %v",
			chanAmt-pushAmt-calcStaticFee(0), aliceBal)
	}
	if bobBal.Balance != int64(pushAmt) {
		t.Fatalf("bob's balance is incorrect: expected %v got %v",
			pushAmt, bobBal.Balance)
	}

	// Finally, immediately close the channel. This function will also
	// block until the channel is closed and will additionally assert the
	// relevant channel closing post conditions.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint, false)
}

// testUpdateChannelPolicy tests that policy updates made to a channel
// gets propagated to other nodes in the network.
func testUpdateChannelPolicy(net *lntest.NetworkHarness, t *harnessTest) {
	timeout := time.Duration(time.Second * 5)
	ctxb := context.Background()

	// Launch notification clients for all nodes, such that we can
	// get notified when they discover new channels and updates
	// in the graph.
	aliceUpdates, aQuit := subscribeGraphNotifications(t, ctxb, net.Alice)
	defer close(aQuit)
	bobUpdates, bQuit := subscribeGraphNotifications(t, ctxb, net.Bob)
	defer close(bQuit)

	chanAmt := maxFundingAmount
	pushAmt := btcutil.Amount(100000)

	// Create a channel Alice->Bob.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPoint := openChannelAndAssert(ctxt, t, net, net.Alice, net.Bob,
		chanAmt, pushAmt)

	ctxt, _ = context.WithTimeout(ctxb, time.Second*15)
	err := net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("alice didn't report channel: %v", err)
	}
	err = net.Bob.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("bob didn't report channel: %v", err)
	}

	// Create Carol and a new channel Bob->Carol.
	carol, err := net.NewNode(nil)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	carolUpdates, cQuit := subscribeGraphNotifications(t, ctxb, carol)
	defer close(cQuit)

	if err := net.ConnectNodes(ctxb, carol, net.Bob); err != nil {
		t.Fatalf("unable to connect dave to alice: %v", err)
	}

	ctxt, _ = context.WithTimeout(ctxb, timeout)
	chanPoint2 := openChannelAndAssert(ctxt, t, net, net.Bob, carol,
		chanAmt, pushAmt)

	ctxt, _ = context.WithTimeout(ctxb, time.Second*15)
	err = net.Bob.WaitForNetworkChannelOpen(ctxt, chanPoint2)
	if err != nil {
		t.Fatalf("bob didn't report channel: %v", err)
	}
	err = carol.WaitForNetworkChannelOpen(ctxt, chanPoint2)
	if err != nil {
		t.Fatalf("carol didn't report channel: %v", err)
	}

	// Update the fees for the channel Alice->Bob, and make sure
	// all nodes learn about it.
	const feeBase = 1000000
	baseFee := int64(1500)
	feeRate := int64(12)
	timeLockDelta := uint32(66)

	req := &lnrpc.PolicyUpdateRequest{
		BaseFeeMsat:   baseFee,
		FeeRate:       float64(feeRate),
		TimeLockDelta: timeLockDelta,
	}
	req.Scope = &lnrpc.PolicyUpdateRequest_ChanPoint{
		ChanPoint: chanPoint,
	}

	_, err = net.Alice.UpdateChannelPolicy(ctxb, req)
	if err != nil {
		t.Fatalf("unable to get alice's balance: %v", err)
	}

	// txStr returns the string representation of the channel's
	// funding tx.
	txStr := func(chanPoint *lnrpc.ChannelPoint) string {
		fundingTxID, err := chainhash.NewHash(chanPoint.FundingTxid)
		if err != nil {
			return ""
		}
		cp := wire.OutPoint{
			Hash:  *fundingTxID,
			Index: chanPoint.OutputIndex,
		}
		return cp.String()
	}

	// A closure that is used to wait for a channel updates that matches
	// the channel policy update done by Alice.
	waitForChannelUpdate := func(graphUpdates chan *lnrpc.GraphTopologyUpdate,
		chanPoints ...*lnrpc.ChannelPoint) {
		// Create a map containing all the channel points we are
		// waiting for updates for.
		cps := make(map[string]bool)
		for _, chanPoint := range chanPoints {
			cps[txStr(chanPoint)] = true
		}
	Loop:
		for {
			select {
			case graphUpdate := <-graphUpdates:
				if len(graphUpdate.ChannelUpdates) == 0 {
					continue
				}
				chanUpdate := graphUpdate.ChannelUpdates[0]
				fundingTxStr := txStr(chanUpdate.ChanPoint)
				if _, ok := cps[fundingTxStr]; !ok {
					continue
				}

				if chanUpdate.AdvertisingNode != net.Alice.PubKeyStr {
					continue
				}

				policy := chanUpdate.RoutingPolicy
				if policy.FeeBaseMsat != baseFee {
					continue
				}
				if policy.FeeRateMilliMsat != feeRate*feeBase {
					continue
				}
				if policy.TimeLockDelta != timeLockDelta {
					continue
				}

				// We got a policy update that matched the
				// values and channel point of what we
				// expected, delete it from the map.
				delete(cps, fundingTxStr)

				// If we have no more channel points we are
				// waiting for, break out of the loop.
				if len(cps) == 0 {
					break Loop
				}
			case <-time.After(20 * time.Second):
				t.Fatalf("did not receive channel update")
			}
		}
	}

	// Wait for all nodes to have seen the policy update done by Alice.
	waitForChannelUpdate(aliceUpdates, chanPoint)
	waitForChannelUpdate(bobUpdates, chanPoint)
	waitForChannelUpdate(carolUpdates, chanPoint)

	// assertChannelPolicy asserts that the passed node's known channel
	// policy for the passed chanPoint is consistent with Alice's current
	// expected policy values.
	assertChannelPolicy := func(node *lntest.HarnessNode,
		chanPoint *lnrpc.ChannelPoint) {
		// Get a DescribeGraph from the node.
		descReq := &lnrpc.ChannelGraphRequest{}
		chanGraph, err := node.DescribeGraph(ctxb, descReq)
		if err != nil {
			t.Fatalf("unable to query for alice's routing table: %v",
				err)
		}

		edgeFound := false
		for _, e := range chanGraph.Edges {
			if e.ChanPoint == txStr(chanPoint) {
				edgeFound = true
				if e.Node1Pub == net.Alice.PubKeyStr {
					if e.Node1Policy.FeeBaseMsat != baseFee {
						t.Fatalf("expected base fee "+
							"%v, got %v", baseFee,
							e.Node1Policy.FeeBaseMsat)
					}
					if e.Node1Policy.FeeRateMilliMsat != feeRate*feeBase {
						t.Fatalf("expected fee rate "+
							"%v, got %v", feeRate*feeBase,
							e.Node1Policy.FeeRateMilliMsat)
					}
					if e.Node1Policy.TimeLockDelta != timeLockDelta {
						t.Fatalf("expected time lock "+
							"delta %v, got %v",
							timeLockDelta,
							e.Node1Policy.TimeLockDelta)
					}
				} else {
					if e.Node2Policy.FeeBaseMsat != baseFee {
						t.Fatalf("expected base fee "+
							"%v, got %v", baseFee,
							e.Node2Policy.FeeBaseMsat)
					}
					if e.Node2Policy.FeeRateMilliMsat != feeRate*feeBase {
						t.Fatalf("expected fee rate "+
							"%v, got %v", feeRate*feeBase,
							e.Node2Policy.FeeRateMilliMsat)
					}
					if e.Node2Policy.TimeLockDelta != timeLockDelta {
						t.Fatalf("expected time lock "+
							"delta %v, got %v",
							timeLockDelta,
							e.Node2Policy.TimeLockDelta)
					}
				}
			}
		}

		if !edgeFound {
			t.Fatalf("did not find edge")
		}

	}

	// Check that all nodes now know about Alice's updated policy.
	assertChannelPolicy(net.Alice, chanPoint)
	assertChannelPolicy(net.Bob, chanPoint)
	assertChannelPolicy(carol, chanPoint)

	// Open channel to Carol.
	if err := net.ConnectNodes(ctxb, net.Alice, carol); err != nil {
		t.Fatalf("unable to connect dave to alice: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	chanPoint3 := openChannelAndAssert(ctxt, t, net, net.Alice, carol,
		chanAmt, pushAmt)

	ctxt, _ = context.WithTimeout(ctxb, time.Second*15)
	err = net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint3)
	if err != nil {
		t.Fatalf("alice didn't report channel: %v", err)
	}
	err = carol.WaitForNetworkChannelOpen(ctxt, chanPoint3)
	if err != nil {
		t.Fatalf("bob didn't report channel: %v", err)
	}

	// Make a global update, and check that both channels'
	// new policies get propagated.
	baseFee = int64(800)
	feeRate = int64(123)
	timeLockDelta = uint32(22)

	req = &lnrpc.PolicyUpdateRequest{
		BaseFeeMsat:   baseFee,
		FeeRate:       float64(feeRate),
		TimeLockDelta: timeLockDelta,
	}
	req.Scope = &lnrpc.PolicyUpdateRequest_Global{}

	_, err = net.Alice.UpdateChannelPolicy(ctxb, req)
	if err != nil {
		t.Fatalf("unable to get alice's balance: %v", err)
	}

	// Wait for all nodes to have seen the policy updates
	// for both of Alice's channels.
	waitForChannelUpdate(aliceUpdates, chanPoint, chanPoint3)
	waitForChannelUpdate(bobUpdates, chanPoint, chanPoint3)
	waitForChannelUpdate(carolUpdates, chanPoint, chanPoint3)

	// And finally check that all nodes remembers the policy
	// update they received.
	assertChannelPolicy(net.Alice, chanPoint)
	assertChannelPolicy(net.Bob, chanPoint)
	assertChannelPolicy(carol, chanPoint)

	assertChannelPolicy(net.Alice, chanPoint3)
	assertChannelPolicy(net.Bob, chanPoint3)
	assertChannelPolicy(carol, chanPoint3)

	// Close the channels.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint, false)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Bob, chanPoint2, false)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint3, false)
	ctxt, _ = context.WithTimeout(ctxb, timeout)

	// Clean up carol's node.
	if err := net.ShutdownNode(carol); err != nil {
		t.Fatalf("unable to shutdown carol: %v", err)
	}
}

// testOpenChannelAfterReorg tests that in the case where we have an open
// channel where the funding tx gets reorged out, the channel will no
// longer be present in the node's routing table.
func testOpenChannelAfterReorg(net *lntest.NetworkHarness, t *harnessTest) {
	timeout := time.Duration(time.Second * 5)
	ctxb := context.Background()

	// Set up a new miner that we can use to cause a reorg.
	args := []string{"--rejectnonstd"}
	miner, err := rpctest.New(harnessNetParams,
		&rpcclient.NotificationHandlers{}, args)
	if err != nil {
		t.Fatalf("unable to create mining node: %v", err)
	}
	if err := miner.SetUp(true, 50); err != nil {
		t.Fatalf("unable to set up mining node: %v", err)
	}
	defer miner.TearDown()

	if err := miner.Node.NotifyNewTransactions(false); err != nil {
		t.Fatalf("unable to request transaction notifications: %v", err)
	}

	// We start by connecting the new miner to our original miner,
	// such that it will sync to our original chain.
	if err := rpctest.ConnectNode(net.Miner, miner); err != nil {
		t.Fatalf("unable to connect harnesses: %v", err)
	}
	nodeSlice := []*rpctest.Harness{net.Miner, miner}
	if err := rpctest.JoinNodes(nodeSlice, rpctest.Blocks); err != nil {
		t.Fatalf("unable to join node on blocks: %v", err)
	}

	// The two should be on the same blockheight.
	_, newNodeHeight, err := miner.Node.GetBestBlock()
	if err != nil {
		t.Fatalf("unable to get current blockheight %v", err)
	}

	_, orgNodeHeight, err := net.Miner.Node.GetBestBlock()
	if err != nil {
		t.Fatalf("unable to get current blockheight %v", err)
	}

	if newNodeHeight != orgNodeHeight {
		t.Fatalf("expected new miner(%d) and original miner(%d) to "+
			"be on the same height", newNodeHeight, orgNodeHeight)
	}

	// We disconnect the two nodes, such that we can start mining on them
	// individually without the other one learning about the new blocks.
	err = net.Miner.Node.AddNode(miner.P2PAddress(), rpcclient.ANRemove)
	if err != nil {
		t.Fatalf("unable to remove node: %v", err)
	}

	// Create a new channel that requires 1 confs before it's considered
	// open, then broadcast the funding transaction
	chanAmt := maxFundingAmount
	pushAmt := btcutil.Amount(0)
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	pendingUpdate, err := net.OpenPendingChannel(ctxt, net.Alice, net.Bob,
		chanAmt, pushAmt)
	if err != nil {
		t.Fatalf("unable to open channel: %v", err)
	}

	// At this point, the channel's funding transaction will have been
	// broadcast, but not confirmed, and the channel should be pending.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	assertNumOpenChannelsPending(ctxt, t, net.Alice, net.Bob, 1)

	fundingTxID, err := chainhash.NewHash(pendingUpdate.Txid)
	if err != nil {
		t.Fatalf("unable to convert funding txid into chainhash.Hash:"+
			" %v", err)
	}

	// We now cause a fork, by letting our original miner mine 10 blocks,
	// and our new miner mine 15. This will also confirm our pending
	// channel, which should be considered open.
	block := mineBlocks(t, net, 10)[0]
	assertTxInBlock(t, block, fundingTxID)
	miner.Node.Generate(15)

	// Ensure the chain lengths are what we expect.
	_, newNodeHeight, err = miner.Node.GetBestBlock()
	if err != nil {
		t.Fatalf("unable to get current blockheight %v", err)
	}

	_, orgNodeHeight, err = net.Miner.Node.GetBestBlock()
	if err != nil {
		t.Fatalf("unable to get current blockheight %v", err)
	}

	if newNodeHeight != orgNodeHeight+5 {
		t.Fatalf("expected new miner(%d) to be 5 blocks ahead of "+
			"original miner(%d)", newNodeHeight, orgNodeHeight)
	}

	chanPoint := &lnrpc.ChannelPoint{
		FundingTxid: pendingUpdate.Txid,
		OutputIndex: pendingUpdate.OutputIndex,
	}

	// Ensure channel is no longer pending.
	assertNumOpenChannelsPending(ctxt, t, net.Alice, net.Bob, 0)

	// Wait for Alice and Bob to recognize and advertise the new channel
	// generated above.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("alice didn't advertise channel before "+
			"timeout: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = net.Bob.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("bob didn't advertise channel before "+
			"timeout: %v", err)
	}

	// Alice should now have 1 edge in her graph.
	req := &lnrpc.ChannelGraphRequest{}
	chanGraph, err := net.Alice.DescribeGraph(ctxb, req)
	if err != nil {
		t.Fatalf("unable to query for alice's routing table: %v", err)
	}

	numEdges := len(chanGraph.Edges)
	if numEdges != 1 {
		t.Fatalf("expected to find one edge in the graph, found %d",
			numEdges)
	}

	// Connecting the two miners should now cause our original one to sync
	// to the new, and longer chain.
	if err := rpctest.ConnectNode(net.Miner, miner); err != nil {
		t.Fatalf("unable to connect harnesses: %v", err)
	}

	if err := rpctest.JoinNodes(nodeSlice, rpctest.Blocks); err != nil {
		t.Fatalf("unable to join node on blocks: %v", err)
	}

	// Once again they should be on the same chain.
	_, newNodeHeight, err = miner.Node.GetBestBlock()
	if err != nil {
		t.Fatalf("unable to get current blockheight %v", err)
	}

	_, orgNodeHeight, err = net.Miner.Node.GetBestBlock()
	if err != nil {
		t.Fatalf("unable to get current blockheight %v", err)
	}

	if newNodeHeight != orgNodeHeight {
		t.Fatalf("expected new miner(%d) and original miner(%d) to "+
			"be on the same height", newNodeHeight, orgNodeHeight)
	}

	time.Sleep(time.Second * 2)

	// Since the fundingtx was reorged out, Alice should now have no edges
	// in her graph.
	req = &lnrpc.ChannelGraphRequest{}
	chanGraph, err = net.Alice.DescribeGraph(ctxb, req)
	if err != nil {
		t.Fatalf("unable to query for alice's routing table: %v", err)
	}

	numEdges = len(chanGraph.Edges)
	if numEdges != 0 {
		t.Fatalf("expected to find no edge in the graph, found %d",
			numEdges)
	}

	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint, false)
}

// testDisconnectingTargetPeer performs a test which
// disconnects Alice-peer from Bob-peer and then re-connects them again
func testDisconnectingTargetPeer(net *lntest.NetworkHarness, t *harnessTest) {

	ctxb := context.Background()

	// Check existing connection.
	assertNumConnections(ctxb, t, net.Alice, net.Bob, 1)

	chanAmt := maxFundingAmount
	pushAmt := btcutil.Amount(0)

	timeout := time.Duration(time.Second * 10)
	ctxt, _ := context.WithTimeout(ctxb, timeout)

	// Create a new channel that requires 1 confs before it's considered
	// open, then broadcast the funding transaction
	const numConfs = 1
	pendingUpdate, err := net.OpenPendingChannel(ctxt, net.Alice, net.Bob,
		chanAmt, pushAmt)
	if err != nil {
		t.Fatalf("unable to open channel: %v", err)
	}

	// At this point, the channel's funding transaction will have
	// been broadcast, but not confirmed. Alice and Bob's nodes
	// should reflect this when queried via RPC.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	assertNumOpenChannelsPending(ctxt, t, net.Alice, net.Bob, 1)

	// Disconnect Alice-peer from Bob-peer and get error
	// causes by one pending channel with detach node is existing.
	if err := net.DisconnectNodes(ctxt, net.Alice, net.Bob); err == nil {
		t.Fatalf("Bob's peer was disconnected from Alice's"+
			" while one pending channel is existing: err %v", err)
	}

	time.Sleep(time.Millisecond * 300)

	// Check existing connection.
	assertNumConnections(ctxb, t, net.Alice, net.Bob, 1)

	fundingTxID, err := chainhash.NewHash(pendingUpdate.Txid)
	if err != nil {
		t.Fatalf("unable to convert funding txid into chainhash.Hash:"+
			" %v", err)
	}

	// Mine a block, then wait for Alice's node to notify us that the
	// channel has been opened. The funding transaction should be found
	// within the newly mined block.
	block := mineBlocks(t, net, numConfs)[0]
	assertTxInBlock(t, block, fundingTxID)

	// At this point, the channel should be fully opened and there should
	// be no pending channels remaining for either node.
	time.Sleep(time.Millisecond * 300)
	ctxt, _ = context.WithTimeout(ctxb, timeout)

	assertNumOpenChannelsPending(ctxt, t, net.Alice, net.Bob, 0)

	// The channel should be listed in the peer information returned by
	// both peers.
	outPoint := wire.OutPoint{
		Hash:  *fundingTxID,
		Index: pendingUpdate.OutputIndex,
	}

	// Check both nodes to ensure that the channel is ready for operation.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	if err := net.AssertChannelExists(ctxt, net.Alice, &outPoint); err != nil {
		t.Fatalf("unable to assert channel existence: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	if err := net.AssertChannelExists(ctxt, net.Bob, &outPoint); err != nil {
		t.Fatalf("unable to assert channel existence: %v", err)
	}

	// Finally, immediately close the channel. This function will also
	// block until the channel is closed and will additionally assert the
	// relevant channel closing post conditions.
	chanPoint := &lnrpc.ChannelPoint{
		FundingTxid: pendingUpdate.Txid,
		OutputIndex: pendingUpdate.OutputIndex,
	}

	// Disconnect Alice-peer from Bob-peer and get error
	// causes by one active channel with detach node is existing.
	if err := net.DisconnectNodes(ctxt, net.Alice, net.Bob); err == nil {
		t.Fatalf("Bob's peer was disconnected from Alice's"+
			" while one active channel is existing: err %v", err)
	}

	// Check existing connection.
	assertNumConnections(ctxb, t, net.Alice, net.Bob, 1)

	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint, true)

	// Disconnect Alice-peer from Bob-peer without getting error
	// about existing channels.
	if err := net.DisconnectNodes(ctxt, net.Alice, net.Bob); err != nil {
		t.Fatalf("unable to disconnect Bob's peer from Alice's: err %v", err)
	}

	// Check zero peer connections.
	assertNumConnections(ctxb, t, net.Alice, net.Bob, 0)

	// Finally, re-connect both nodes.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	if err := net.ConnectNodes(ctxt, net.Alice, net.Bob); err != nil {
		t.Fatalf("unable to connect Alice's peer to Bob's: err %v", err)
	}

	// Check existing connection.
	assertNumConnections(ctxb, t, net.Alice, net.Bob, 1)

	// Mine enough blocks to clear the force closed outputs from the UTXO
	// nursery.
	if _, err := net.Miner.Node.Generate(4); err != nil {
		t.Fatalf("unable to mine blocks: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
}

// testFundingPersistence is intended to ensure that the Funding Manager
// persists the state of new channels prior to broadcasting the channel's
// funding transaction. This ensures that the daemon maintains an up-to-date
// representation of channels if the system is restarted or disconnected.
// testFundingPersistence mirrors testBasicChannelFunding, but adds restarts
// and checks for the state of channels with unconfirmed funding transactions.
func testChannelFundingPersistence(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	chanAmt := maxFundingAmount
	pushAmt := btcutil.Amount(0)

	timeout := time.Duration(time.Second * 10)

	// As we need to create a channel that requires more than 1
	// confirmation before it's open, with the current set of defaults,
	// we'll need to create a new node instance.
	const numConfs = 5
	carolArgs := []string{fmt.Sprintf("--bitcoin.defaultchanconfs=%v", numConfs)}
	carol, err := net.NewNode(carolArgs)
	if err != nil {
		t.Fatalf("unable to create new node: %v", err)
	}
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	if err := net.ConnectNodes(ctxt, net.Alice, carol); err != nil {
		t.Fatalf("unable to connect alice to carol: %v", err)
	}

	// Create a new channel that requires 5 confs before it's considered
	// open, then broadcast the funding transaction
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	pendingUpdate, err := net.OpenPendingChannel(ctxt, net.Alice, carol,
		chanAmt, pushAmt)
	if err != nil {
		t.Fatalf("unable to open channel: %v", err)
	}

	// At this point, the channel's funding transaction will have been
	// broadcast, but not confirmed. Alice and Bob's nodes should reflect
	// this when queried via RPC.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	assertNumOpenChannelsPending(ctxt, t, net.Alice, carol, 1)

	// Restart both nodes to test that the appropriate state has been
	// persisted and that both nodes recover gracefully.
	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}
	if err := net.RestartNode(carol, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	fundingTxID, err := chainhash.NewHash(pendingUpdate.Txid)
	if err != nil {
		t.Fatalf("unable to convert funding txid into chainhash.Hash:"+
			" %v", err)
	}

	// Mine a block, then wait for Alice's node to notify us that the
	// channel has been opened. The funding transaction should be found
	// within the newly mined block.
	block := mineBlocks(t, net, 1)[0]
	assertTxInBlock(t, block, fundingTxID)

	// Restart both nodes to test that the appropriate state has been
	// persisted and that both nodes recover gracefully.
	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}
	if err := net.RestartNode(carol, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	// The following block ensures that after both nodes have restarted,
	// they have reconnected before the execution of the next test.
	peersTimeout := time.After(15 * time.Second)
	checkPeersTick := time.NewTicker(100 * time.Millisecond)
	defer checkPeersTick.Stop()
peersPoll:
	for {
		select {
		case <-peersTimeout:
			t.Fatalf("peers unable to reconnect after restart")
		case <-checkPeersTick.C:
			peers, err := carol.ListPeers(ctxb,
				&lnrpc.ListPeersRequest{})
			if err != nil {
				t.Fatalf("ListPeers error: %v\n", err)
			}
			if len(peers.Peers) > 0 {
				break peersPoll
			}
		}
	}

	// Next, mine enough blocks s.t the channel will open with a single
	// additional block mined.
	if _, err := net.Miner.Node.Generate(3); err != nil {
		t.Fatalf("unable to mine blocks: %v", err)
	}

	// Both nodes should still show a single channel as pending.
	time.Sleep(time.Second * 1)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	assertNumOpenChannelsPending(ctxt, t, net.Alice, carol, 1)

	// Finally, mine the last block which should mark the channel as open.
	if _, err := net.Miner.Node.Generate(1); err != nil {
		t.Fatalf("unable to mine blocks: %v", err)
	}

	// At this point, the channel should be fully opened and there should
	// be no pending channels remaining for either node.
	time.Sleep(time.Second * 1)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	assertNumOpenChannelsPending(ctxt, t, net.Alice, carol, 0)

	// The channel should be listed in the peer information returned by
	// both peers.
	outPoint := wire.OutPoint{
		Hash:  *fundingTxID,
		Index: pendingUpdate.OutputIndex,
	}

	// Check both nodes to ensure that the channel is ready for operation.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	if err := net.AssertChannelExists(ctxt, net.Alice, &outPoint); err != nil {
		t.Fatalf("unable to assert channel existence: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	if err := net.AssertChannelExists(ctxt, carol, &outPoint); err != nil {
		t.Fatalf("unable to assert channel existence: %v", err)
	}

	// Finally, immediately close the channel. This function will also
	// block until the channel is closed and will additionally assert the
	// relevant channel closing post conditions.
	chanPoint := &lnrpc.ChannelPoint{
		FundingTxid: pendingUpdate.Txid,
		OutputIndex: pendingUpdate.OutputIndex,
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint, false)

	// Clean up carol's node.
	if err := net.ShutdownNode(carol); err != nil {
		t.Fatalf("unable to shutdown carol: %v", err)
	}
}

// testChannelBalance creates a new channel between Alice and  Bob, then
// checks channel balance to be equal amount specified while creation of channel.
func testChannelBalance(net *lntest.NetworkHarness, t *harnessTest) {
	timeout := time.Duration(time.Second * 5)

	// Open a channel with 0.16 BTC between Alice and Bob, ensuring the
	// channel has been opened properly.
	amount := maxFundingAmount
	ctx, _ := context.WithTimeout(context.Background(), timeout)

	// Creates a helper closure to be used below which asserts the proper
	// response to a channel balance RPC.
	checkChannelBalance := func(node lnrpc.LightningClient,
		amount btcutil.Amount) {

		response, err := node.ChannelBalance(ctx, &lnrpc.ChannelBalanceRequest{})
		if err != nil {
			t.Fatalf("unable to get channel balance: %v", err)
		}

		balance := btcutil.Amount(response.Balance)
		if balance != amount {
			t.Fatalf("channel balance wrong: %v != %v", balance,
				amount)
		}
	}

	chanPoint := openChannelAndAssert(ctx, t, net, net.Alice, net.Bob,
		amount, 0)

	// Wait for both Alice and Bob to recognize this new channel.
	ctxt, _ := context.WithTimeout(context.Background(), timeout)
	err := net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("alice didn't advertise channel before "+
			"timeout: %v", err)
	}
	ctxt, _ = context.WithTimeout(context.Background(), timeout)
	err = net.Bob.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("bob didn't advertise channel before "+
			"timeout: %v", err)
	}

	// As this is a single funder channel, Alice's balance should be
	// exactly 0.5 BTC since now state transitions have taken place yet.
	checkChannelBalance(net.Alice, amount-calcStaticFee(0))

	// Ensure Bob currently has no available balance within the channel.
	checkChannelBalance(net.Bob, 0)

	// Finally close the channel between Alice and Bob, asserting that the
	// channel has been properly closed on-chain.
	ctx, _ = context.WithTimeout(context.Background(), timeout)
	closeChannelAndAssert(ctx, t, net, net.Alice, chanPoint, false)
}

// findForceClosedChannel searches a pending channel response for a particular
// channel, returning the force closed channel upon success.
func findForceClosedChannel(t *harnessTest,
	pendingChanResp *lnrpc.PendingChannelsResponse,
	op *wire.OutPoint) *lnrpc.PendingChannelsResponse_ForceClosedChannel {

	var found bool
	var forceClose *lnrpc.PendingChannelsResponse_ForceClosedChannel
	for _, forceClose = range pendingChanResp.PendingForceClosingChannels {
		if forceClose.Channel.ChannelPoint == op.String() {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("channel not marked as force closed")
	}

	return forceClose
}

func assertCommitmentMaturity(t *harnessTest,
	forceClose *lnrpc.PendingChannelsResponse_ForceClosedChannel,
	maturityHeight uint32, blocksTilMaturity int32) {

	if forceClose.MaturityHeight != maturityHeight {
		t.Fatalf("expected commitment maturity height to be %d, "+
			"found %d instead", maturityHeight,
			forceClose.MaturityHeight)
	}
	if forceClose.BlocksTilMaturity != blocksTilMaturity {
		t.Fatalf("expected commitment blocks til maturity to be %d, "+
			"found %d instead", blocksTilMaturity,
			forceClose.BlocksTilMaturity)
	}
}

// assertForceClosedChannelNumHtlcs verifies that a force closed channel has the
// proper number of htlcs.
func assertPendingChannelNumHtlcs(t *harnessTest,
	forceClose *lnrpc.PendingChannelsResponse_ForceClosedChannel,
	expectedNumHtlcs int) {

	if len(forceClose.PendingHtlcs) != expectedNumHtlcs {
		t.Fatalf("expected force closed channel to have %d pending "+
			"htlcs, found %d instead", expectedNumHtlcs,
			len(forceClose.PendingHtlcs))
	}
}

// assertNumForceClosedChannels checks that a pending channel response has the
// expected number of force closed channels.
func assertNumForceClosedChannels(t *harnessTest,
	pendingChanResp *lnrpc.PendingChannelsResponse, expectedNumChans int) {

	if len(pendingChanResp.PendingForceClosingChannels) != expectedNumChans {
		t.Fatalf("expected to find %d force closed channels, got %d",
			expectedNumChans,
			len(pendingChanResp.PendingForceClosingChannels))
	}
}

// assertPendingHtlcStageAndMaturity uniformly tests all pending htlc's
// belonging to a force closed channel, testing for the expeced stage number,
// blocks till maturity, and the maturity height.
func assertPendingHtlcStageAndMaturity(t *harnessTest,
	forceClose *lnrpc.PendingChannelsResponse_ForceClosedChannel,
	stage, maturityHeight uint32, blocksTillMaturity int32) {

	for _, pendingHtlc := range forceClose.PendingHtlcs {
		if pendingHtlc.Stage != stage {
			t.Fatalf("expected pending htlc to be stage %d, "+
				"found %d", stage, pendingHtlc.Stage)
		}
		if pendingHtlc.MaturityHeight != maturityHeight {
			t.Fatalf("expected pending htlc maturity height to be "+
				"%d, instead has %d", maturityHeight,
				pendingHtlc.MaturityHeight)
		}
		if pendingHtlc.BlocksTilMaturity != blocksTillMaturity {
			t.Fatalf("expected pending htlc blocks til maturity "+
				"to be %d, instead has %d", blocksTillMaturity,
				pendingHtlc.BlocksTilMaturity)
		}
	}
}

// testChannelForceClosure performs a test to exercise the behavior of "force"
// closing a channel or unilaterally broadcasting the latest local commitment
// state on-chain. The test creates a new channel between Alice and Carol, then
// force closes the channel after some cursory assertions. Within the test, a
// total of 3 + n transactions will be broadcast, representing the commitment
// transaction, a transaction sweeping the local CSV delayed output, a
// transaction sweeping the CSV delayed 2nd-layer htlcs outputs, and n
// htlc success transactions, where n is the number of payments Alice attempted
// to send to Carol.  This test includes several restarts to ensure that the
// transaction output states are persisted throughout the forced closure
// process.
//
// TODO(roasbeef): also add an unsettled HTLC before force closing.
func testChannelForceClosure(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()
	const (
		timeout     = time.Duration(time.Second * 10)
		chanAmt     = btcutil.Amount(10e6)
		pushAmt     = btcutil.Amount(5e6)
		paymentAmt  = 100000
		numInvoices = 6
	)

	// TODO(roasbeef): should check default value in config here
	// instead, or make delay a param
	defaultCSV := uint32(4)
	defaultCLTV := uint32(defaultBitcoinTimeLockDelta)

	// Since we'd like to test failure scenarios with outstanding htlcs,
	// we'll introduce another node into our test network: Carol.
	carol, err := net.NewNode([]string{"--debughtlc", "--hodlhtlc"})
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}

	// We must let Alice have an open channel before she can send a node
	// announcement, so we open a channel with Carol,
	if err := net.ConnectNodes(ctxb, net.Alice, carol); err != nil {
		t.Fatalf("unable to connect alice to carol: %v", err)
	}

	// Before we start, obtain Carol's current wallet balance, we'll check
	// to ensure that at the end of the force closure by Alice, Carol
	// recognizes his new on-chain output.
	carolBalReq := &lnrpc.WalletBalanceRequest{}
	carolBalResp, err := carol.WalletBalance(ctxb, carolBalReq)
	if err != nil {
		t.Fatalf("unable to get carol's balance: %v", err)
	}

	carolStartingBalance := btcutil.Amount(carolBalResp.ConfirmedBalance * 1e8)

	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPoint := openChannelAndAssert(ctxt, t, net, net.Alice, carol,
		chanAmt, pushAmt)

	// Wait for Alice to receive the channel edge from the funding manager.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("alice didn't see the alice->carol channel before "+
			"timeout: %v", err)
	}

	// With the channel open, we'll create a few invoices for Carol that
	// Alice will pay to in order to advance the state of the channel.
	carolPaymentReqs := make([]string, numInvoices)
	for i := 0; i < numInvoices; i++ {
		preimage := bytes.Repeat([]byte{byte(128 - i)}, 32)
		invoice := &lnrpc.Invoice{
			Memo:      "testing",
			RPreimage: preimage,
			Value:     paymentAmt,
		}
		resp, err := carol.AddInvoice(ctxb, invoice)
		if err != nil {
			t.Fatalf("unable to add invoice: %v", err)
		}

		carolPaymentReqs[i] = resp.PaymentRequest
	}

	// As we'll be querying the state of Carols's channels frequently we'll
	// create a closure helper function for the purpose.
	getAliceChanInfo := func() (*lnrpc.ActiveChannel, error) {
		req := &lnrpc.ListChannelsRequest{}
		aliceChannelInfo, err := net.Alice.ListChannels(ctxb, req)
		if err != nil {
			return nil, err
		}
		if len(aliceChannelInfo.Channels) != 1 {
			t.Fatalf("alice should only have a single channel, "+
				"instead he has %v",
				len(aliceChannelInfo.Channels))
		}

		return aliceChannelInfo.Channels[0], nil
	}

	// Fetch starting height of this test so we can compute the block
	// heights we expect certain events to take place.
	_, curHeight, err := net.Miner.Node.GetBestBlock()
	if err != nil {
		t.Fatalf("unable to get best block height")
	}

	// Using the current height of the chain, derive the relevant heights
	// for incubating two-stage htlcs.
	var (
		startHeight           = uint32(curHeight)
		commCsvMaturityHeight = startHeight + 1 + defaultCSV
		htlcExpiryHeight      = startHeight + defaultCLTV
		htlcCsvMaturityHeight = startHeight + defaultCLTV + 1 + defaultCSV
	)

	// Send payments from Alice to Carol, since Carol is htlchodl mode,
	// the htlc outputs should be left unsettled, and should be swept by the
	// utxo nursery.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = completePaymentRequests(ctxt, net.Alice, carolPaymentReqs, false)
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	aliceChan, err := getAliceChanInfo()
	if err != nil {
		t.Fatalf("unable to get alice's channel info: %v", err)
	}
	if aliceChan.NumUpdates == 0 {
		t.Fatalf("alice should see at least one update to her channel")
	}

	// Now that the channel is open and we have unsettled htlcs, immediately
	// execute a force closure of the channel. This will also assert that
	// the commitment transaction was immediately broadcast in order to
	// fulfill the force closure request.
	_, closingTxID, err := net.CloseChannel(ctxb, net.Alice, chanPoint, true)
	if err != nil {
		t.Fatalf("unable to execute force channel closure: %v", err)
	}

	// Now that the channel has been force closed, it should show up in the
	// PendingChannels RPC under the force close section.
	pendingChansRequest := &lnrpc.PendingChannelsRequest{}
	pendingChanResp, err := net.Alice.PendingChannels(ctxb, pendingChansRequest)
	if err != nil {
		t.Fatalf("unable to query for pending channels: %v", err)
	}
	assertNumForceClosedChannels(t, pendingChanResp, 1)

	// Compute the outpoint of the channel, which we will use repeatedly to
	// locate the pending channel information in the rpc responses.
	txid, _ := chainhash.NewHash(chanPoint.FundingTxid[:])
	op := wire.OutPoint{
		Hash:  *txid,
		Index: chanPoint.OutputIndex,
	}

	forceClose := findForceClosedChannel(t, pendingChanResp, &op)

	// Immediately after force closing, all of the funds should be in limbo,
	// and the pending channels response should not indicate that any funds
	// have been recovered.
	if forceClose.LimboBalance == 0 {
		t.Fatalf("all funds should still be in limbo")
	}
	if forceClose.RecoveredBalance != 0 {
		t.Fatalf("no funds should yet be shown as recovered")
	}

	// The commitment transaction has not been confirmed, so we expect to
	// see a maturity height and blocks til maturity of 0.
	assertCommitmentMaturity(t, forceClose, 0, 0)

	// Since all of our payments were sent with Carol in hodl mode, all of
	// them should be unsettled and attached to the commitment transaction.
	// They also should have been configured such that they are not filtered
	// as dust. At this point, all pending htlcs should be in stage 1, with
	// a timeout set to the default CLTV expiry (144) blocks above the
	// starting height.
	assertPendingChannelNumHtlcs(t, forceClose, numInvoices)
	assertPendingHtlcStageAndMaturity(t, forceClose, 1, htlcExpiryHeight,
		int32(defaultCLTV))

	// The several restarts in this test are intended to ensure that when a
	// channel is force-closed, the UTXO nursery has persisted the state of
	// the channel in the closure process and will recover the correct state
	// when the system comes back on line. This restart tests state
	// persistence at the beginning of the process, when the commitment
	// transaction has been broadcast but not yet confirmed in a block.
	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	// Mine a block which should confirm the commitment transaction
	// broadcast as a result of the force closure.
	if _, err := net.Miner.Node.Generate(1); err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}

	// The following sleep provides time for the UTXO nursery to move the
	// output from the preschool to the kindergarten database buckets
	// prior to RestartNode() being triggered. Without this sleep, the
	// database update may fail, causing the UTXO nursery to retry the move
	// operation upon restart. This will change the blockheights from what
	// is expected by the test.
	// TODO(bvu): refactor out this sleep.
	duration := time.Millisecond * 300
	time.Sleep(duration)

	pendingChanResp, err = net.Alice.PendingChannels(ctxb, pendingChansRequest)
	if err != nil {
		t.Fatalf("unable to query for pending channels: %v", err)
	}
	assertNumForceClosedChannels(t, pendingChanResp, 1)

	forceClose = findForceClosedChannel(t, pendingChanResp, &op)

	// Now that the channel has been force closed, it should now have the
	// height and number of blocks to confirm populated.
	assertCommitmentMaturity(t, forceClose, commCsvMaturityHeight,
		int32(defaultCSV))

	// Check that our pending htlcs have deducted the block confirming the
	// commitment transactionfrom their blocks til maturity value.
	assertPendingChannelNumHtlcs(t, forceClose, numInvoices)
	assertPendingHtlcStageAndMaturity(t, forceClose, 1, htlcExpiryHeight,
		int32(defaultCLTV)-1)

	// None of our outputs have been swept, so they should all be limbo.
	if forceClose.LimboBalance == 0 {
		t.Fatalf("all funds should still be in limbo")
	}
	if forceClose.RecoveredBalance != 0 {
		t.Fatalf("no funds should yet be shown as recovered")
	}

	// The following restart is intended to ensure that outputs from the
	// force close commitment transaction have been persisted once the
	// transaction has been confirmed, but before the outputs are spendable
	// (the "kindergarten" bucket.)
	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	// Currently within the codebase, the default CSV is 4 relative blocks.
	// For the persistence test, we generate three blocks, then trigger
	// a restart and then generate the final block that should trigger
	// the creation of the sweep transaction.
	if _, err := net.Miner.Node.Generate(defaultCSV - 1); err != nil {
		t.Fatalf("unable to mine blocks: %v", err)
	}

	// The following restart checks to ensure that outputs in the
	// kindergarten bucket are persisted while waiting for the required
	// number of confirmations to be reported.
	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	pendingChanResp, err = net.Alice.PendingChannels(ctxb, pendingChansRequest)
	if err != nil {
		t.Fatalf("unable to query for pending channels: %v", err)
	}
	assertNumForceClosedChannels(t, pendingChanResp, 1)

	forceClose = findForceClosedChannel(t, pendingChanResp, &op)

	// At this point, the nursery should show that the commitment output has
	// 1 block left before its CSV delay expires. In total, we have mined
	// exactly defaultCSV blocks, so the htlc outputs should also reflect
	// that this many blocks have passed.
	assertCommitmentMaturity(t, forceClose, commCsvMaturityHeight, 1)
	assertPendingChannelNumHtlcs(t, forceClose, numInvoices)
	assertPendingHtlcStageAndMaturity(t, forceClose, 1, htlcExpiryHeight,
		int32(defaultCLTV)-int32(defaultCSV))

	// All funds should still be shown in limbo.
	if forceClose.LimboBalance == 0 {
		t.Fatalf("all funds should still be in limbo")
	}
	if forceClose.RecoveredBalance != 0 {
		t.Fatalf("no funds should yet be shown as recovered")
	}

	// Generate an additional block, which should cause the CSV delayed
	// output from the commitment txn to expire.
	if _, err := net.Miner.Node.Generate(1); err != nil {
		t.Fatalf("unable to mine blocks: %v", err)
	}

	// At this point, the sweeping transaction should now be broadcast. So
	// we fetch the node's mempool to ensure it has been properly
	// broadcast.
	sweepingTXID, err := waitForTxInMempool(net.Miner.Node, 3*time.Second)
	if err != nil {
		t.Fatalf("failed to get sweep tx from mempool: %v", err)
	}

	// Fetch the sweep transaction, all input it's spending should be from
	// the commitment transaction which was broadcast on-chain.
	sweepTx, err := net.Miner.Node.GetRawTransaction(sweepingTXID)
	if err != nil {
		t.Fatalf("unable to fetch sweep tx: %v", err)
	}
	for _, txIn := range sweepTx.MsgTx().TxIn {
		if !closingTxID.IsEqual(&txIn.PreviousOutPoint.Hash) {
			t.Fatalf("sweep transaction not spending from commit "+
				"tx %v, instead spending %v",
				closingTxID, txIn.PreviousOutPoint)
		}
	}

	// Restart Alice to ensure that she resumes watching the finalized
	// commitment sweep txid.
	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	// Next, we mine an additional block which should include the sweep
	// transaction as the input scripts and the sequence locks on the
	// inputs should be properly met.
	blockHash, err := net.Miner.Node.Generate(1)
	if err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}
	block, err := net.Miner.Node.GetBlock(blockHash[0])
	if err != nil {
		t.Fatalf("unable to get block: %v", err)
	}

	assertTxInBlock(t, block, sweepTx.Hash())

	// We sleep here to ensure that Alice has enough time to receive a
	// confirmation for the commitment transaction, which we already
	// asserted was in the last block.
	time.Sleep(300 * time.Millisecond)

	// Now that the commit output has been fully swept, check to see that
	// the channel remains open for the pending htlc outputs.
	pendingChanResp, err = net.Alice.PendingChannels(ctxb, pendingChansRequest)
	if err != nil {
		t.Fatalf("unable to query for pending channels: %v", err)
	}
	assertNumForceClosedChannels(t, pendingChanResp, 1)

	// Check that the commitment transactions shows that we are still past
	// the maturity of the commitment output.
	forceClose = findForceClosedChannel(t, pendingChanResp, &op)
	assertCommitmentMaturity(t, forceClose, commCsvMaturityHeight, -1)

	// Our pending htlcs should still be shown in the first stage, having
	// deducted an additional two blocks from the relative maturity time..
	assertPendingChannelNumHtlcs(t, forceClose, numInvoices)
	assertPendingHtlcStageAndMaturity(t, forceClose, 1, htlcExpiryHeight,
		int32(defaultCLTV)-int32(defaultCSV)-2)

	// The htlc funds will still be shown as limbo, since they are still in
	// their first stage. The commitment funds will have been recovered
	// after the commit txn was included in the last block.
	if forceClose.LimboBalance == 0 {
		t.Fatalf("htlc funds should still be in limbo")
	}
	if forceClose.RecoveredBalance == 0 {
		t.Fatalf("commitment funds should be shown as recovered")
	}

	// Compute the height preceding that which will cause the htlc CLTV
	// timeouts will expire. The outputs entered at the same height as the
	// output spending from the commitment txn, so we must deduct the number
	// of blocks we have generated since adding it to the nursery, and take
	// an additional block off so that we end up one block shy of the expiry
	// height.
	cltvHeightDelta := defaultCLTV - defaultCSV - 2 - 1

	// Check that our htlcs are still expected to expire the computed expiry
	// height, and that the remaining number of blocks is equal to the delta
	// we just computed, including an additional block to actually trigger
	// the broadcast.
	assertPendingChannelNumHtlcs(t, forceClose, numInvoices)
	assertPendingHtlcStageAndMaturity(t, forceClose, 1, htlcExpiryHeight,
		int32(cltvHeightDelta+1))

	// Advance the blockchain until just before the CLTV expires, nothing
	// exciting should have happened during this time.
	blockHash, err = net.Miner.Node.Generate(cltvHeightDelta)
	if err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}
	time.Sleep(duration)

	// We now restart Alice, to ensure that she will broadcast the presigned
	// htlc timeout txns after the delay expires after experiencing an while
	// waiting for the htlc outputs to incubate.
	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}
	time.Sleep(duration)

	pendingChanResp, err = net.Alice.PendingChannels(ctxb, pendingChansRequest)
	if err != nil {
		t.Fatalf("unable to query for pending channels: %v", err)
	}
	assertNumForceClosedChannels(t, pendingChanResp, 1)

	forceClose = findForceClosedChannel(t, pendingChanResp, &op)

	// Verify that commitment output was confirmed many moons ago.
	assertCommitmentMaturity(t, forceClose, commCsvMaturityHeight,
		-int32(cltvHeightDelta)-1)

	// We should now be at the block just before the utxo nursery will
	// attempt to broadcast the htlc timeout transactions.
	assertPendingChannelNumHtlcs(t, forceClose, numInvoices)
	assertPendingHtlcStageAndMaturity(t, forceClose, 1, htlcExpiryHeight, 1)

	// Now that our commitment confirmation depth has been surpassed, we
	// should now see a non-zero recovered balance. All htlc outputs are
	// still left in limbo, so it should be non-zero as well.
	if forceClose.LimboBalance == 0 {
		t.Fatalf("htlc funds should still be in limbo")
	}
	if forceClose.RecoveredBalance == 0 {
		t.Fatalf("commitment funds should not be in limbo")
	}

	// Now, generate the block which will cause Alice to broadcast the
	// presigned htlc timeout txns.
	blockHash, err = net.Miner.Node.Generate(1)
	if err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}

	// Since Alice had numInvoices (6) htlcs extended to Carol before force
	// closing, we expect Alice to broadcast an htlc timeout txn for each
	// one. Wait for them all to show up in the mempool.
	htlcTxIDs, err := waitForNTxsInMempool(net.Miner.Node, numInvoices,
		3*time.Second)
	if err != nil {
		t.Fatalf("unable to find htlc timeout txns in mempool: %v", err)
	}

	// Retrieve each htlc timeout txn from the mempool, and ensure it is
	// well-formed. This entails verifying that each only spends from
	// output, and that that output is from the commitment txn.
	for _, htlcTxID := range htlcTxIDs {
		// Fetch the sweep transaction, all input it's spending should
		// be from the commitment transaction which was broadcast
		// on-chain.
		htlcTx, err := net.Miner.Node.GetRawTransaction(htlcTxID)
		if err != nil {
			t.Fatalf("unable to fetch sweep tx: %v", err)
		}
		// Ensure the htlc transaction only has one input.
		if len(htlcTx.MsgTx().TxIn) != 1 {
			t.Fatalf("htlc transaction should only have one txin, "+
				"has %d", len(htlcTx.MsgTx().TxIn))
		}
		// Ensure the htlc transaction is spending from the commitment
		// transaction.
		txIn := htlcTx.MsgTx().TxIn[0]
		if !closingTxID.IsEqual(&txIn.PreviousOutPoint.Hash) {
			t.Fatalf("htlc transaction not spending from commit "+
				"tx %v, instead spending %v",
				closingTxID, txIn.PreviousOutPoint)
		}
	}

	// With the htlc timeout txns still in the mempool, we restart Alice to
	// verify that she can resume watching the htlc txns she broadcasted
	// before crashing.
	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}
	time.Sleep(duration)

	// Generate a block that mines the htlc timeout txns. Doing so now
	// activates the 2nd-stage CSV delayed outputs.
	blockHash, err = net.Miner.Node.Generate(1)
	if err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}
	// This sleep gives Alice enough to time move the crib outputs into the
	// kindergarten bucket.
	time.Sleep(duration)

	// Alice is restarted here to ensure that she promptly moved the crib
	// outputs to the kindergarten bucket after the htlc timeout txns were
	// confirmed.
	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	// Advance the chain until just before the 2nd-layer CSV delays expire.
	blockHash, err = net.Miner.Node.Generate(defaultCSV - 1)
	if err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}

	// Restart Alice to ensure that she can recover from a failure before
	// having graduated the htlc outputs in the kindergarten bucket.
	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	// Now that the channel has been fully swept, it should no longer show
	// incubated, check to see that Alice's node still reports the channel
	// as pending force closed.
	pendingChanResp, err = net.Alice.PendingChannels(ctxb, pendingChansRequest)
	if err != nil {
		t.Fatalf("unable to query for pending channels: %v", err)
	}
	assertNumForceClosedChannels(t, pendingChanResp, 1)

	forceClose = findForceClosedChannel(t, pendingChanResp, &op)
	assertCommitmentMaturity(t, forceClose, commCsvMaturityHeight,
		-int32(cltvHeightDelta)-int32(defaultCSV)-2)

	if forceClose.LimboBalance == 0 {
		t.Fatalf("htlc funds should still be in limbo")
	}
	if forceClose.RecoveredBalance == 0 {
		t.Fatalf("commitment funds should not be in limbo")
	}

	assertPendingChannelNumHtlcs(t, forceClose, numInvoices)

	// Generate a block that causes Alice to sweep the htlc outputs in the
	// kindergarten bucket.
	blockHash, err = net.Miner.Node.Generate(1)
	if err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}

	// Wait for the single sweep txn to appear in the mempool.
	htlcSweepTxID, err := waitForTxInMempool(net.Miner.Node, 15*time.Second)
	if err != nil {
		t.Fatalf("failed to get sweep tx from mempool: %v", err)
	}

	// Construct a map of the already confirmed htlc timeout txids, that
	// will count the number of times each is spent by the sweep txn. We
	// prepopulate it in this way so that we can later detect if we are
	// spending from an output that was not a confirmed htlc timeout txn.
	var htlcTxIDSet = make(map[chainhash.Hash]int)
	for _, htlcTxID := range htlcTxIDs {
		htlcTxIDSet[*htlcTxID] = 0
	}

	// Fetch the htlc sweep transaction from the mempool.
	htlcSweepTx, err := net.Miner.Node.GetRawTransaction(htlcSweepTxID)
	if err != nil {
		t.Fatalf("unable to fetch sweep tx: %v", err)
	}
	// Ensure the htlc sweep transaction only has one input for each htlc
	// Alice extended before force closing.
	if len(htlcSweepTx.MsgTx().TxIn) != numInvoices {
		t.Fatalf("htlc transaction should have %d txin, "+
			"has %d", numInvoices, len(htlcSweepTx.MsgTx().TxIn))
	}
	// Ensure that each output spends from exactly one htlc timeout txn.
	for _, txIn := range htlcSweepTx.MsgTx().TxIn {
		outpoint := txIn.PreviousOutPoint.Hash
		// Check that the input is a confirmed htlc timeout txn.
		if _, ok := htlcTxIDSet[outpoint]; !ok {
			t.Fatalf("htlc sweep output not spending from htlc "+
				"tx, instead spending output %v", outpoint)
		}
		// Increment our count for how many times this output was spent.
		htlcTxIDSet[outpoint]++

		// Check that each is only spent once.
		if htlcTxIDSet[outpoint] > 1 {
			t.Fatalf("htlc sweep tx has multiple spends from "+
				"outpoint %v", outpoint)
		}
	}

	// The following restart checks to ensure that the nursery store is
	// storing the txid of the previously broadcast htlc sweep txn, and that
	// it begins watching that txid after restarting.
	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}
	time.Sleep(duration)

	// Now that the channel has been fully swept, it should no longer show
	// incubated, check to see that Alice's node still reports the channel
	// as pending force closed.
	pendingChanResp, err = net.Alice.PendingChannels(ctxb, pendingChansRequest)
	if err != nil {
		t.Fatalf("unable to query for pending channels: %v", err)
	}
	assertNumForceClosedChannels(t, pendingChanResp, 1)

	// All htlcs should show zero blocks until maturity, as evidenced by
	// having checked the sweep transaction in the mempool.
	forceClose = findForceClosedChannel(t, pendingChanResp, &op)
	assertPendingChannelNumHtlcs(t, forceClose, numInvoices)
	assertPendingHtlcStageAndMaturity(t, forceClose, 2,
		htlcCsvMaturityHeight, 0)

	// Generate the final block that sweeps all htlc funds into the user's
	// wallet.
	blockHash, err = net.Miner.Node.Generate(1)
	if err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}
	time.Sleep(3 * duration)

	// Now that the channel has been fully swept, it should no longer show
	// up within the pending channels RPC.
	pendingChanResp, err = net.Alice.PendingChannels(ctxb, pendingChansRequest)
	if err != nil {
		t.Fatalf("unable to query for pending channels: %v", err)
	}
	assertNumForceClosedChannels(t, pendingChanResp, 0)

	// In addition to there being no pending channels, we verify that
	// pending channels does not report any money still in limbo.
	if pendingChanResp.TotalLimboBalance != 0 {
		t.Fatalf("no user funds should be left in limbo after incubation")
	}

	// At this point, Carol should now be aware of his new immediately
	// spendable on-chain balance, as it was Alice who broadcast the
	// commitment transaction.
	carolBalResp, err = net.Bob.WalletBalance(ctxb, carolBalReq)
	if err != nil {
		t.Fatalf("unable to get carol's balance: %v", err)
	}
	carolExpectedBalance := carolStartingBalance + pushAmt
	if btcutil.Amount(carolBalResp.ConfirmedBalance*1e8) < carolExpectedBalance {
		t.Fatalf("carol's balance is incorrect: expected %v got %v",
			carolExpectedBalance,
			btcutil.Amount(carolBalResp.ConfirmedBalance*1e8))
	}
}

func testSingleHopInvoice(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()
	timeout := time.Duration(time.Second * 5)

	// Open a channel with 100k satoshis between Alice and Bob with Alice being
	// the sole funder of the channel.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanAmt := btcutil.Amount(100000)
	chanPoint := openChannelAndAssert(ctxt, t, net, net.Alice, net.Bob,
		chanAmt, 0)

	assertAmountSent := func(amt btcutil.Amount) {
		// Both channels should also have properly accounted from the
		// amount that has been sent/received over the channel.
		listReq := &lnrpc.ListChannelsRequest{}
		aliceListChannels, err := net.Alice.ListChannels(ctxb, listReq)
		if err != nil {
			t.Fatalf("unable to query for alice's channel list: %v", err)
		}
		aliceSatoshisSent := aliceListChannels.Channels[0].TotalSatoshisSent
		if aliceSatoshisSent != int64(amt) {
			t.Fatalf("Alice's satoshis sent is incorrect got %v, expected %v",
				aliceSatoshisSent, amt)
		}

		bobListChannels, err := net.Bob.ListChannels(ctxb, listReq)
		if err != nil {
			t.Fatalf("unable to query for bob's channel list: %v", err)
		}
		bobSatoshisReceived := bobListChannels.Channels[0].TotalSatoshisReceived
		if bobSatoshisReceived != int64(amt) {
			t.Fatalf("Bob's satoshis received is incorrect got %v, expected %v",
				bobSatoshisReceived, amt)
		}
	}

	// Now that the channel is open, create an invoice for Bob which
	// expects a payment of 1000 satoshis from Alice paid via a particular
	// preimage.
	const paymentAmt = 1000
	preimage := bytes.Repeat([]byte("A"), 32)
	invoice := &lnrpc.Invoice{
		Memo:      "testing",
		RPreimage: preimage,
		Value:     paymentAmt,
	}
	invoiceResp, err := net.Bob.AddInvoice(ctxb, invoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	// Wait for Alice to recognize and advertise the new channel generated
	// above.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("alice didn't advertise channel before "+
			"timeout: %v", err)
	}
	err = net.Bob.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("bob didn't advertise channel before "+
			"timeout: %v", err)
	}

	// With the invoice for Bob added, send a payment towards Alice paying
	// to the above generated invoice.
	sendReq := &lnrpc.SendRequest{
		PaymentRequest: invoiceResp.PaymentRequest,
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	resp, err := net.Alice.SendPaymentSync(ctxt, sendReq)
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	// Ensure we obtain the proper preimage in the response.
	if resp.PaymentError != "" {
		t.Fatalf("error when attempting recv: %v", resp.PaymentError)
	} else if !bytes.Equal(preimage, resp.PaymentPreimage) {
		t.Fatalf("preimage mismatch: expected %v, got %v", preimage,
			resp.GetPaymentPreimage())
	}

	// Bob's invoice should now be found and marked as settled.
	payHash := &lnrpc.PaymentHash{
		RHash: invoiceResp.RHash,
	}
	dbInvoice, err := net.Bob.LookupInvoice(ctxb, payHash)
	if err != nil {
		t.Fatalf("unable to lookup invoice: %v", err)
	}
	if !dbInvoice.Settled {
		t.Fatalf("bob's invoice should be marked as settled: %v",
			spew.Sdump(dbInvoice))
	}

	// With the payment completed all balance related stats should be
	// properly updated.
	time.Sleep(time.Millisecond * 200)
	assertAmountSent(paymentAmt)

	// Create another invoice for Bob, this time leaving off the preimage
	// to one will be randomly generated. We'll test the proper
	// encoding/decoding of the zpay32 payment requests.
	invoice = &lnrpc.Invoice{
		Memo:  "test3",
		Value: paymentAmt,
	}
	invoiceResp, err = net.Bob.AddInvoice(ctxb, invoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	// Next send another payment, but this time using a zpay32 encoded
	// invoice rather than manually specifying the payment details.
	sendReq = &lnrpc.SendRequest{
		PaymentRequest: invoiceResp.PaymentRequest,
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	resp, err = net.Alice.SendPaymentSync(ctxt, sendReq)
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}
	if resp.PaymentError != "" {
		t.Fatalf("error when attempting recv: %v", resp.PaymentError)
	}

	// The second payment should also have succeeded, with the balances
	// being update accordingly.
	time.Sleep(time.Millisecond * 200)
	assertAmountSent(paymentAmt * 2)

	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint, false)
}

func testListPayments(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()
	timeout := time.Duration(time.Second * 5)

	// First start by deleting all payments that Alice knows of. This will
	// allow us to execute the test with a clean state for Alice.
	delPaymentsReq := &lnrpc.DeleteAllPaymentsRequest{}
	if _, err := net.Alice.DeleteAllPayments(ctxb, delPaymentsReq); err != nil {
		t.Fatalf("unable to delete payments: %v", err)
	}

	// Check that there are no payments before test.
	reqInit := &lnrpc.ListPaymentsRequest{}
	paymentsRespInit, err := net.Alice.ListPayments(ctxb, reqInit)
	if err != nil {
		t.Fatalf("error when obtaining Alice payments: %v", err)
	}
	if len(paymentsRespInit.Payments) != 0 {
		t.Fatalf("incorrect number of payments, got %v, want %v",
			len(paymentsRespInit.Payments), 0)
	}

	// Open a channel with 100k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	chanAmt := btcutil.Amount(100000)
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPoint := openChannelAndAssert(ctxt, t, net, net.Alice, net.Bob,
		chanAmt, 0)

	// Now that the channel is open, create an invoice for Bob which
	// expects a payment of 1000 satoshis from Alice paid via a particular
	// preimage.
	const paymentAmt = 1000
	preimage := bytes.Repeat([]byte("B"), 32)
	invoice := &lnrpc.Invoice{
		Memo:      "testing",
		RPreimage: preimage,
		Value:     paymentAmt,
	}
	addInvoiceCtxt, _ := context.WithTimeout(ctxb, timeout)
	invoiceResp, err := net.Bob.AddInvoice(addInvoiceCtxt, invoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	// Wait for Alice to recognize and advertise the new channel generated
	// above.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	if err = net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint); err != nil {
		t.Fatalf("alice didn't advertise channel before "+
			"timeout: %v", err)
	}
	if err = net.Bob.WaitForNetworkChannelOpen(ctxt, chanPoint); err != nil {
		t.Fatalf("bob didn't advertise channel before "+
			"timeout: %v", err)
	}

	// With the invoice for Bob added, send a payment towards Alice paying
	// to the above generated invoice.
	sendReq := &lnrpc.SendRequest{
		PaymentRequest: invoiceResp.PaymentRequest,
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	resp, err := net.Alice.SendPaymentSync(ctxt, sendReq)
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}
	if resp.PaymentError != "" {
		t.Fatalf("error when attempting recv: %v", resp.PaymentError)
	}

	// Grab Alice's list of payments, she should show the existence of
	// exactly one payment.
	req := &lnrpc.ListPaymentsRequest{}
	paymentsResp, err := net.Alice.ListPayments(ctxb, req)
	if err != nil {
		t.Fatalf("error when obtaining Alice payments: %v", err)
	}
	if len(paymentsResp.Payments) != 1 {
		t.Fatalf("incorrect number of payments, got %v, want %v",
			len(paymentsResp.Payments), 1)
	}
	p := paymentsResp.Payments[0]

	// Ensure that the stored path shows a direct payment to Bob with no
	// other nodes in-between.
	expectedPath := []string{
		net.Bob.PubKeyStr,
	}
	if !reflect.DeepEqual(p.Path, expectedPath) {
		t.Fatalf("incorrect path, got %v, want %v",
			p.Path, expectedPath)
	}

	// The payment amount should also match our previous payment directly.
	if p.Value != paymentAmt {
		t.Fatalf("incorrect amount, got %v, want %v",
			p.Value, paymentAmt)
	}

	// The payment hash (or r-hash) should have been stored correctly.
	correctRHash := hex.EncodeToString(invoiceResp.RHash)
	if !reflect.DeepEqual(p.PaymentHash, correctRHash) {
		t.Fatalf("incorrect RHash, got %v, want %v",
			p.PaymentHash, correctRHash)
	}

	// Finally, as we made a single-hop direct payment, there should have
	// been no fee applied.
	if p.Fee != 0 {
		t.Fatalf("incorrect Fee, got %v, want %v", p.Fee, 0)
	}

	// Delete all payments from Alice. DB should have no payments.
	delReq := &lnrpc.DeleteAllPaymentsRequest{}
	_, err = net.Alice.DeleteAllPayments(ctxb, delReq)
	if err != nil {
		t.Fatalf("Can't delete payments at the end: %v", err)
	}

	// Check that there are no payments before test.
	listReq := &lnrpc.ListPaymentsRequest{}
	paymentsResp, err = net.Alice.ListPayments(ctxb, listReq)
	if err != nil {
		t.Fatalf("error when obtaining Alice payments: %v", err)
	}
	if len(paymentsResp.Payments) != 0 {
		t.Fatalf("incorrect number of payments, got %v, want %v",
			len(paymentsRespInit.Payments), 0)
	}

	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint, false)
}

// assertAmountPaid checks that the ListChannels command of the provided
// node list the total amount sent and received as expected for the
// provided channel.
func assertAmountPaid(t *harnessTest, ctxb context.Context, channelName string,
	node *lntest.HarnessNode, chanPoint wire.OutPoint, amountSent,
	amountReceived int64) {

	checkAmountPaid := func() error {
		listReq := &lnrpc.ListChannelsRequest{}
		resp, err := node.ListChannels(ctxb, listReq)
		if err != nil {
			return fmt.Errorf("unable to for node's "+
				"channels: %v", err)
		}
		for _, channel := range resp.Channels {
			if channel.ChannelPoint != chanPoint.String() {
				continue
			}

			if channel.TotalSatoshisSent != amountSent {
				return fmt.Errorf("%v: incorrect amount"+
					" sent: %v != %v", channelName,
					channel.TotalSatoshisSent,
					amountSent)
			}
			if channel.TotalSatoshisReceived !=
				amountReceived {
				return fmt.Errorf("%v: incorrect amount"+
					" received: %v != %v",
					channelName,
					channel.TotalSatoshisReceived,
					amountReceived)
			}

			return nil
		}
		return fmt.Errorf("channel not found")
	}

	// As far as HTLC inclusion in commitment transaction might be
	// postponed we will try to check the balance couple of times,
	// and then if after some period of time we receive wrong
	// balance return the error.
	// TODO(roasbeef): remove sleep after invoice notification hooks
	// are in place
	var timeover uint32
	go func() {
		<-time.After(time.Second * 20)
		atomic.StoreUint32(&timeover, 1)
	}()

	for {
		isTimeover := atomic.LoadUint32(&timeover) == 1
		if err := checkAmountPaid(); err != nil {
			if isTimeover {
				t.Fatalf("Check amount Paid failed: %v", err)
			}
		} else {
			break
		}
	}
}

func testMultiHopPayments(net *lntest.NetworkHarness, t *harnessTest) {
	const chanAmt = btcutil.Amount(100000)
	ctxb := context.Background()
	timeout := time.Duration(time.Second * 15)
	var networkChans []*lnrpc.ChannelPoint

	// Open a channel with 100k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPointAlice := openChannelAndAssert(ctxt, t, net, net.Alice,
		net.Bob, chanAmt, 0)
	networkChans = append(networkChans, chanPointAlice)

	aliceChanTXID, err := chainhash.NewHash(chanPointAlice.FundingTxid)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
	}
	aliceFundPoint := wire.OutPoint{
		Hash:  *aliceChanTXID,
		Index: chanPointAlice.OutputIndex,
	}

	// As preliminary setup, we'll create two new nodes: Carol and Dave,
	// such that we now have a 4 ndoe, 3 channel topology. Dave will make
	// a channel with Alice, and Carol with Dave. After this setup, the
	// network topology should now look like:
	//     Carol -> Dave -> Alice -> Bob
	//
	// First, we'll create Dave and establish a channel to Alice.
	dave, err := net.NewNode(nil)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	if err := net.ConnectNodes(ctxb, dave, net.Alice); err != nil {
		t.Fatalf("unable to connect dave to alice: %v", err)
	}
	err = net.SendCoins(ctxb, btcutil.SatoshiPerBitcoin, dave)
	if err != nil {
		t.Fatalf("unable to send coins to dave: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	chanPointDave := openChannelAndAssert(ctxt, t, net, dave,
		net.Alice, chanAmt, 0)
	networkChans = append(networkChans, chanPointDave)
	daveChanTXID, err := chainhash.NewHash(chanPointDave.FundingTxid)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
	}
	daveFundPoint := wire.OutPoint{
		Hash:  *daveChanTXID,
		Index: chanPointDave.OutputIndex,
	}

	// Next, we'll create Carol and establish a channel to from her to
	// Dave.
	carol, err := net.NewNode(nil)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	if err := net.ConnectNodes(ctxb, carol, dave); err != nil {
		t.Fatalf("unable to connect carol to dave: %v", err)
	}
	err = net.SendCoins(ctxb, btcutil.SatoshiPerBitcoin, carol)
	if err != nil {
		t.Fatalf("unable to send coins to carol: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	chanPointCarol := openChannelAndAssert(ctxt, t, net, carol,
		dave, chanAmt, 0)
	networkChans = append(networkChans, chanPointCarol)

	carolChanTXID, err := chainhash.NewHash(chanPointCarol.FundingTxid)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
	}
	carolFundPoint := wire.OutPoint{
		Hash:  *carolChanTXID,
		Index: chanPointCarol.OutputIndex,
	}

	// Wait for all nodes to have seen all channels.
	for _, chanPoint := range networkChans {
		for _, node := range []*lntest.HarnessNode{net.Alice, net.Bob, carol, dave} {
			ctxt, _ = context.WithTimeout(ctxb, timeout)
			err = node.WaitForNetworkChannelOpen(ctxt, chanPoint)
			if err != nil {
				t.Fatalf("timeout waiting for channel open: %v", err)
			}
		}
	}

	// Create 5 invoices for Bob, which expect a payment from Carol for 1k
	// satoshis with a different preimage each time.
	const numPayments = 5
	const paymentAmt = 1000
	payReqs := make([]string, numPayments)
	for i := 0; i < numPayments; i++ {
		invoice := &lnrpc.Invoice{
			Memo:  "testing",
			Value: paymentAmt,
		}
		resp, err := net.Bob.AddInvoice(ctxb, invoice)
		if err != nil {
			t.Fatalf("unable to add invoice: %v", err)
		}

		payReqs[i] = resp.PaymentRequest
	}

	// We'll wait for all parties to recognize the new channels within the
	// network.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = dave.WaitForNetworkChannelOpen(ctxt, chanPointDave)
	if err != nil {
		t.Fatalf("dave didn't advertise his channel: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = carol.WaitForNetworkChannelOpen(ctxt, chanPointCarol)
	if err != nil {
		t.Fatalf("carol didn't advertise her channel in time: %v",
			err)
	}

	time.Sleep(time.Millisecond * 50)

	// Using Carol as the source, pay to the 5 invoices from Bob created
	// above.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = completePaymentRequests(ctxt, carol, payReqs, true)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	// When asserting the amount of satoshis moved, we'll factor in the
	// default base fee, as we didn't modify the fee structure when
	// creating the seed nodes in the network.
	const baseFee = 1

	// At this point all the channels within our proto network should be
	// shifted by 5k satoshis in the direction of Bob, the sink within the
	// payment flow generated above. The order of asserts corresponds to
	// increasing of time is needed to embed the HTLC in commitment
	// transaction, in channel Carol->David->Alice->Bob, order is Bob,
	// Alice, David, Carol.
	const amountPaid = int64(5000)
	assertAmountPaid(t, ctxb, "Alice(local) => Bob(remote)", net.Bob,
		aliceFundPoint, int64(0), amountPaid)
	assertAmountPaid(t, ctxb, "Alice(local) => Bob(remote)", net.Alice,
		aliceFundPoint, amountPaid, int64(0))
	assertAmountPaid(t, ctxb, "Dave(local) => Alice(remote)", net.Alice,
		daveFundPoint, int64(0), amountPaid+(baseFee*numPayments))
	assertAmountPaid(t, ctxb, "Dave(local) => Alice(remote)", dave,
		daveFundPoint, amountPaid+(baseFee*numPayments), int64(0))
	assertAmountPaid(t, ctxb, "Carol(local) => Dave(remote)", dave,
		carolFundPoint, int64(0), amountPaid+((baseFee*numPayments)*2))
	assertAmountPaid(t, ctxb, "Carol(local) => Dave(remote)", carol,
		carolFundPoint, amountPaid+(baseFee*numPayments)*2, int64(0))

	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPointAlice, false)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, dave, chanPointDave, false)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, carol, chanPointCarol, false)

	// Finally, shutdown the nodes we created for the duration of the tests,
	// only leaving the two seed nodes (Alice and Bob) within our test
	// network.
	if err := net.ShutdownNode(carol); err != nil {
		t.Fatalf("unable to shutdown carol: %v", err)
	}
	if err := net.ShutdownNode(dave); err != nil {
		t.Fatalf("unable to shutdown dave: %v", err)
	}
}

// testPrivateChannels tests that a private channel can be used for
// routing by the two endpoints of the channel, but is not known by
// the rest of the nodes in the graph.
func testPrivateChannels(net *lntest.NetworkHarness, t *harnessTest) {
	const chanAmt = btcutil.Amount(100000)
	ctxb := context.Background()
	timeout := time.Duration(time.Second * 5)
	var networkChans []*lnrpc.ChannelPoint

	// We create the the following topology:
	//
	// Dave --100k--> Alice --200k--> Bob
	//  ^		    ^
	//  |		    |
	// 100k		   100k
	//  |		    |
	//  +---- Carol ----+
	//
	// where the 100k channel between Carol and Alice is private.

	// Open a channel with 200k satoshis between Alice and Bob.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPointAlice := openChannelAndAssert(ctxt, t, net, net.Alice,
		net.Bob, chanAmt*2, 0)
	networkChans = append(networkChans, chanPointAlice)

	aliceChanTXID, err := chainhash.NewHash(chanPointAlice.FundingTxid)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
	}
	aliceFundPoint := wire.OutPoint{
		Hash:  *aliceChanTXID,
		Index: chanPointAlice.OutputIndex,
	}

	// Create Dave, and a channel to Alice of 100k.
	dave, err := net.NewNode(nil)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	if err := net.ConnectNodes(ctxb, dave, net.Alice); err != nil {
		t.Fatalf("unable to connect dave to alice: %v", err)
	}
	err = net.SendCoins(ctxb, btcutil.SatoshiPerBitcoin, dave)
	if err != nil {
		t.Fatalf("unable to send coins to dave: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	chanPointDave := openChannelAndAssert(ctxt, t, net, dave,
		net.Alice, chanAmt, 0)
	networkChans = append(networkChans, chanPointDave)
	daveChanTXID, err := chainhash.NewHash(chanPointDave.FundingTxid)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
	}
	daveFundPoint := wire.OutPoint{
		Hash:  *daveChanTXID,
		Index: chanPointDave.OutputIndex,
	}

	// Next, we'll create Carol and establish a channel from her to
	// Dave of 100k.
	carol, err := net.NewNode(nil)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	if err := net.ConnectNodes(ctxb, carol, dave); err != nil {
		t.Fatalf("unable to connect carol to dave: %v", err)
	}
	err = net.SendCoins(ctxb, btcutil.SatoshiPerBitcoin, carol)
	if err != nil {
		t.Fatalf("unable to send coins to carol: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	chanPointCarol := openChannelAndAssert(ctxt, t, net, carol,
		dave, chanAmt, 0)
	networkChans = append(networkChans, chanPointCarol)

	carolChanTXID, err := chainhash.NewHash(chanPointCarol.FundingTxid)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
	}
	carolFundPoint := wire.OutPoint{
		Hash:  *carolChanTXID,
		Index: chanPointCarol.OutputIndex,
	}

	// Wait for all nodes to have seen all these channels, as they
	// are all public.
	nodes := []*lntest.HarnessNode{net.Alice, net.Bob, carol, dave}
	for _, chanPoint := range networkChans {
		for _, node := range nodes {
			ctxt, _ = context.WithTimeout(ctxb, timeout)
			err = node.WaitForNetworkChannelOpen(ctxt, chanPoint)
			if err != nil {
				t.Fatalf("timeout waiting for channel open: %v",
					err)
			}
		}
	}

	// Now create a _private_ channel directly between Carol and
	// Alice of 100k.
	if err := net.ConnectNodes(ctxb, carol, net.Alice); err != nil {
		t.Fatalf("unable to connect dave to alice: %v", err)
	}
	chanOpenUpdate, err := net.OpenChannel(ctxb, carol, net.Alice, chanAmt,
		0, true)
	if err != nil {
		t.Fatalf("unable to open channel: %v", err)
	}

	// One block is enough to make the channel ready for use, since the
	// nodes have defaultNumConfs=1 set.
	block := mineBlocks(t, net, 1)[0]
	chanPointPrivate, err := net.WaitForChannelOpen(ctxb, chanOpenUpdate)
	if err != nil {
		t.Fatalf("error while waiting for channel open: %v", err)
	}
	fundingTxID, err := chainhash.NewHash(chanPointPrivate.FundingTxid)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
	}
	assertTxInBlock(t, block, fundingTxID)

	// The channel should be listed in the peer information returned by
	// both peers.
	privateFundPoint := wire.OutPoint{
		Hash:  *fundingTxID,
		Index: chanPointPrivate.OutputIndex,
	}
	err = net.AssertChannelExists(ctxb, carol, &privateFundPoint)
	if err != nil {
		t.Fatalf("unable to assert channel existence: %v", err)
	}
	err = net.AssertChannelExists(ctxb, net.Alice, &privateFundPoint)
	if err != nil {
		t.Fatalf("unable to assert channel existence: %v", err)
	}

	// The channel should be available for payments between Carol and Alice.
	// We check this by sending payments from Carol to Bob, that
	// collectively would deplete at least one of Carol's channels.

	// Create 2 invoices for Bob, each of 70k satoshis. Since each of
	// Carol's channels is of size 100k, these payments cannot succeed
	// by only using one of the channels.
	const numPayments = 2
	const paymentAmt = 70000
	payReqs := make([]string, numPayments)
	for i := 0; i < numPayments; i++ {
		invoice := &lnrpc.Invoice{
			Memo:  "testing",
			Value: paymentAmt,
		}
		resp, err := net.Bob.AddInvoice(ctxb, invoice)
		if err != nil {
			t.Fatalf("unable to add invoice: %v", err)
		}

		payReqs[i] = resp.PaymentRequest
	}

	time.Sleep(time.Millisecond * 50)

	// Let Carol pay the invoices.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = completePaymentRequests(ctxt, carol, payReqs, true)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	// When asserting the amount of satoshis moved, we'll factor in the
	// default base fee, as we didn't modify the fee structure when
	// creating the seed nodes in the network.
	const baseFee = 1

	// Bob should have received 140k satoshis from Alice.
	assertAmountPaid(t, ctxb, "Alice(local) => Bob(remote)", net.Bob,
		aliceFundPoint, int64(0), 2*paymentAmt)

	// Alice sent 140k to Bob.
	assertAmountPaid(t, ctxb, "Alice(local) => Bob(remote)", net.Alice,
		aliceFundPoint, 2*paymentAmt, int64(0))

	// Alice received 70k + fee from Dave.
	assertAmountPaid(t, ctxb, "Dave(local) => Alice(remote)", net.Alice,
		daveFundPoint, int64(0), paymentAmt+baseFee)

	// Dave sent 70k+fee to Alice.
	assertAmountPaid(t, ctxb, "Dave(local) => Alice(remote)", dave,
		daveFundPoint, paymentAmt+baseFee, int64(0))

	// Dave received 70k+fee of two hops from Carol.
	assertAmountPaid(t, ctxb, "Carol(local) => Dave(remote)", dave,
		carolFundPoint, int64(0), paymentAmt+baseFee*2)

	// Carol sent 70k+fee of two hops to Dave.
	assertAmountPaid(t, ctxb, "Carol(local) => Dave(remote)", carol,
		carolFundPoint, paymentAmt+baseFee*2, int64(0))

	// Alice received 70k+fee from Carol.
	assertAmountPaid(t, ctxb, "Carol(local) [private=>] Alice(remote)",
		net.Alice, privateFundPoint, int64(0), paymentAmt+baseFee)

	// Carol sent 70k+fee to Alice.
	assertAmountPaid(t, ctxb, "Carol(local) [private=>] Alice(remote)",
		carol, privateFundPoint, paymentAmt+baseFee, int64(0))

	// Alice should also be able to route payments using this channel,
	// so send two payments of 60k back to Carol.
	const paymentAmt60k = 60000
	payReqs = make([]string, numPayments)
	for i := 0; i < numPayments; i++ {
		invoice := &lnrpc.Invoice{
			Memo:  "testing",
			Value: paymentAmt60k,
		}
		resp, err := carol.AddInvoice(ctxb, invoice)
		if err != nil {
			t.Fatalf("unable to add invoice: %v", err)
		}

		payReqs[i] = resp.PaymentRequest
	}

	time.Sleep(time.Millisecond * 50)

	// Let Bob pay the invoices.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = completePaymentRequests(ctxt, net.Alice, payReqs, true)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	// Finally, we make sure Dave and Bob does not know about the
	// private channel between Carol and Alice. We first mine
	// plenty of blocks, such that the channel would have been
	// announceed in case it was public.
	mineBlocks(t, net, 10)

	// We create a helper method to check how many edges each of the
	// nodes know about. Carol and Alice should know about 4, while
	// Bob and Dave should only know about 3, since one channel is
	// private.
	numChannels := func(node *lntest.HarnessNode) int {
		req := &lnrpc.ChannelGraphRequest{}
		ctxt, _ := context.WithTimeout(ctxb, timeout)
		chanGraph, err := node.DescribeGraph(ctxt, req)
		if err != nil {
			t.Fatalf("unable go describegraph: %v", err)
		}
		return len(chanGraph.Edges)
	}

	aliceChans := numChannels(net.Alice)
	if aliceChans != 4 {
		t.Fatalf("expected Alice to know 4 edges, had %v", aliceChans)
	}
	bobChans := numChannels(net.Bob)
	if bobChans != 3 {
		t.Fatalf("expected Bob to know 3 edges, had %v", bobChans)
	}
	carolChans := numChannels(carol)
	if carolChans != 4 {
		t.Fatalf("expected Carol to know 4 edges, had %v", carolChans)
	}
	daveChans := numChannels(dave)
	if daveChans != 3 {
		t.Fatalf("expected Dave to know 3 edges, had %v", daveChans)
	}

	// Close all channels.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPointAlice, false)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, dave, chanPointDave, false)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, carol, chanPointCarol, false)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, carol, chanPointPrivate, false)

	// Finally, shutdown the nodes we created for the duration of the tests,
	// only leaving the two seed nodes (Alice and Bob) within our test
	// network.
	if err := net.ShutdownNode(carol); err != nil {
		t.Fatalf("unable to shutdown carol: %v", err)
	}
	if err := net.ShutdownNode(dave); err != nil {
		t.Fatalf("unable to shutdown dave: %v", err)
	}
}

func testInvoiceSubscriptions(net *lntest.NetworkHarness, t *harnessTest) {
	const chanAmt = btcutil.Amount(500000)
	ctxb := context.Background()
	timeout := time.Duration(time.Second * 5)

	// Open a channel with 500k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPoint := openChannelAndAssert(ctxt, t, net, net.Alice, net.Bob,
		chanAmt, 0)

	// Next create a new invoice for Bob requesting 1k satoshis.
	// TODO(roasbeef): make global list of invoices for each node to re-use
	// and avoid collisions
	const paymentAmt = 1000
	preimage := bytes.Repeat([]byte{byte(90)}, 32)
	invoice := &lnrpc.Invoice{
		Memo:      "testing",
		RPreimage: preimage,
		Value:     paymentAmt,
	}
	invoiceResp, err := net.Bob.AddInvoice(ctxb, invoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	// Create a new invoice subscription client for Bob, the notification
	// should be dispatched shortly below.
	req := &lnrpc.InvoiceSubscription{}
	bobInvoiceSubscription, err := net.Bob.SubscribeInvoices(ctxb, req)
	if err != nil {
		t.Fatalf("unable to subscribe to bob's invoice updates: %v", err)
	}

	quit := make(chan struct{})
	updateSent := make(chan struct{})
	go func() {
		invoiceUpdate, err := bobInvoiceSubscription.Recv()
		select {
		case <-quit:
			// Received cancellation
			return
		default:
		}

		if err != nil {
			t.Fatalf("unable to recv invoice update: %v", err)
		}

		// The invoice update should exactly match the invoice created
		// above, but should now be settled and have SettleDate
		if !invoiceUpdate.Settled {
			t.Fatalf("invoice not settled but shoudl be")
		}
		if invoiceUpdate.SettleDate == 0 {
			t.Fatalf("invoice should have non zero settle date, but doesn't")
		}

		if !bytes.Equal(invoiceUpdate.RPreimage, invoice.RPreimage) {
			t.Fatalf("payment preimages don't match: expected %v, got %v",
				invoice.RPreimage, invoiceUpdate.RPreimage)
		}

		close(updateSent)
	}()

	// Wait for the channel to be recognized by both Alice and Bob before
	// continuing the rest of the test.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		// TODO(roasbeef): will need to make num blocks to advertise a
		// node param
		close(quit)
		t.Fatalf("channel not seen by alice before timeout: %v", err)
	}

	// With the assertion above set up, send a payment from Alice to Bob
	// which should finalize and settle the invoice.
	sendReq := &lnrpc.SendRequest{
		PaymentRequest: invoiceResp.PaymentRequest,
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	resp, err := net.Alice.SendPaymentSync(ctxt, sendReq)
	if err != nil {
		close(quit)
		t.Fatalf("unable to send payment: %v", err)
	}
	if resp.PaymentError != "" {
		close(quit)
		t.Fatalf("error when attempting recv: %v", resp.PaymentError)
	}

	select {
	case <-time.After(time.Second * 10):
		close(quit)
		t.Fatalf("update not sent after 10 seconds")
	case <-updateSent: // Fall through on success
	}

	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint, false)
}

// testBasicChannelCreation test multiple channel opening and closing.
func testBasicChannelCreation(net *lntest.NetworkHarness, t *harnessTest) {
	const (
		numChannels = 2
		timeout     = time.Duration(time.Second * 5)
		amount      = maxFundingAmount
	)

	// Open the channel between Alice and Bob, asserting that the
	// channel has been properly open on-chain.
	chanPoints := make([]*lnrpc.ChannelPoint, numChannels)
	for i := 0; i < numChannels; i++ {
		ctx, _ := context.WithTimeout(context.Background(), timeout)
		chanPoints[i] = openChannelAndAssert(ctx, t, net, net.Alice,
			net.Bob, amount, 0)
	}

	// Close the channel between Alice and Bob, asserting that the
	// channel has been properly closed on-chain.
	for _, chanPoint := range chanPoints {
		ctx, _ := context.WithTimeout(context.Background(), timeout)
		closeChannelAndAssert(ctx, t, net, net.Alice, chanPoint, false)
	}
}

// testMaxPendingChannels checks that error is returned from remote peer if
// max pending channel number was exceeded and that '--maxpendingchannels' flag
// exists and works properly.
func testMaxPendingChannels(net *lntest.NetworkHarness, t *harnessTest) {
	maxPendingChannels := defaultMaxPendingChannels + 1
	amount := maxFundingAmount

	timeout := time.Duration(time.Second * 10)
	ctx, _ := context.WithTimeout(context.Background(), timeout)

	// Create a new node (Carol) with greater number of max pending
	// channels.
	args := []string{
		fmt.Sprintf("--maxpendingchannels=%v", maxPendingChannels),
	}
	carol, err := net.NewNode(args)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}

	ctx, _ = context.WithTimeout(context.Background(), timeout)
	if err := net.ConnectNodes(ctx, net.Alice, carol); err != nil {
		t.Fatalf("unable to connect carol to alice: %v", err)
	}

	ctx, _ = context.WithTimeout(context.Background(), timeout)
	carolBalance := btcutil.Amount(maxPendingChannels) * amount
	if err := net.SendCoins(ctx, carolBalance, carol); err != nil {
		t.Fatalf("unable to send coins to carol: %v", err)
	}

	// Send open channel requests without generating new blocks thereby
	// increasing pool of pending channels. Then check that we can't open
	// the channel if the number of pending channels exceed max value.
	openStreams := make([]lnrpc.Lightning_OpenChannelClient, maxPendingChannels)
	for i := 0; i < maxPendingChannels; i++ {
		ctx, _ = context.WithTimeout(context.Background(), timeout)
		stream, err := net.OpenChannel(ctx, net.Alice, carol, amount,
			0, false)
		if err != nil {
			t.Fatalf("unable to open channel: %v", err)
		}
		openStreams[i] = stream
	}

	// Carol exhausted available amount of pending channels, next open
	// channel request should cause ErrorGeneric to be sent back to Alice.
	ctx, _ = context.WithTimeout(context.Background(), timeout)
	_, err = net.OpenChannel(ctx, net.Alice, carol, amount, 0, false)
	if err == nil {
		t.Fatalf("error wasn't received")
	} else if grpc.Code(err) != lnwire.ErrMaxPendingChannels.ToGrpcCode() {
		t.Fatalf("not expected error was received: %v", err)
	}

	// For now our channels are in pending state, in order to not interfere
	// with other tests we should clean up - complete opening of the
	// channel and then close it.

	// Mine 6 blocks, then wait for node's to notify us that the channel has
	// been opened. The funding transactions should be found within the
	// first newly mined block. 6 blocks make sure the funding transaction
	// has enouught confirmations to be announced publicly.
	block := mineBlocks(t, net, 6)[0]

	chanPoints := make([]*lnrpc.ChannelPoint, maxPendingChannels)
	for i, stream := range openStreams {
		ctxt, _ := context.WithTimeout(context.Background(), timeout)
		fundingChanPoint, err := net.WaitForChannelOpen(ctxt, stream)
		if err != nil {
			t.Fatalf("error while waiting for channel open: %v", err)
		}

		fundingTxID, err := chainhash.NewHash(fundingChanPoint.FundingTxid)
		if err != nil {
			t.Fatalf("unable to create sha hash: %v", err)
		}

		// Ensure that the funding transaction enters a block, and is
		// properly advertised by Alice.
		assertTxInBlock(t, block, fundingTxID)
		ctxt, _ = context.WithTimeout(context.Background(), timeout)
		err = net.Alice.WaitForNetworkChannelOpen(ctxt, fundingChanPoint)
		if err != nil {
			t.Fatalf("channel not seen on network before "+
				"timeout: %v", err)
		}

		// The channel should be listed in the peer information
		// returned by both peers.
		chanPoint := wire.OutPoint{
			Hash:  *fundingTxID,
			Index: fundingChanPoint.OutputIndex,
		}
		if err := net.AssertChannelExists(ctx, net.Alice, &chanPoint); err != nil {
			t.Fatalf("unable to assert channel existence: %v", err)
		}

		chanPoints[i] = fundingChanPoint
	}

	// Next, close the channel between Alice and Carol, asserting that the
	// channel has been properly closed on-chain.
	for _, chanPoint := range chanPoints {
		ctxt, _ := context.WithTimeout(context.Background(), timeout)
		closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint, false)
	}

	// Finally, shutdown the node we created for the duration of the tests,
	// only leaving the two seed nodes (Alice and Bob) within our test
	// network.
	if err := net.ShutdownNode(carol); err != nil {
		t.Fatalf("unable to shutdown carol: %v", err)
	}
}

func copyFile(dest, src string) error {
	s, err := os.Open(src)
	if err != nil {
		return err
	}
	defer s.Close()

	d, err := os.Create(dest)
	if err != nil {
		return err
	}

	if _, err := io.Copy(d, s); err != nil {
		d.Close()
		return err
	}

	return d.Close()
}

func waitForTxInMempool(miner *rpcclient.Client,
	timeout time.Duration) (*chainhash.Hash, error) {

	var txid *chainhash.Hash
	breakTimeout := time.After(timeout)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
poll:
	for {
		select {
		case <-breakTimeout:
			return nil, errors.New("no tx found in mempool")
		case <-ticker.C:
			mempool, err := miner.GetRawMempool()
			if err != nil {
				return nil, err
			}

			if len(mempool) == 0 {
				continue
			}

			txid = mempool[0]
			break poll
		}
	}
	return txid, nil
}

// waitForNTxsInMempool polls until finding the desired number of transactions
// in the provided miner's mempool. An error is returned if the this number is
// not met after the given timeout.
func waitForNTxsInMempool(miner *rpcclient.Client, n int,
	timeout time.Duration) ([]*chainhash.Hash, error) {

	breakTimeout := time.After(timeout)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	var err error
	var mempool []*chainhash.Hash
	for {
		select {
		case <-breakTimeout:
			return nil, fmt.Errorf("wanted %v, only found %v txs "+
				"in mempool", n, len(mempool))
		case <-ticker.C:
			mempool, err = miner.GetRawMempool()
			if err != nil {
				return nil, err
			}

			if len(mempool) == n {
				return mempool, nil
			}
		}
	}
}

// testRevokedCloseRetributinPostBreachConf tests that Alice is able carry out
// retribution in the event that she fails immediately after detecting Bob's
// breach txn in the mempool.
func testRevokedCloseRetribution(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()
	const (
		timeout     = time.Duration(time.Second * 10)
		chanAmt     = maxFundingAmount
		paymentAmt  = 10000
		numInvoices = 6
	)

	// In order to test Alice's response to an uncooperative channel
	// closure by Bob, we'll first open up a channel between them with a
	// 0.5 BTC value.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPoint := openChannelAndAssert(ctxt, t, net, net.Alice, net.Bob,
		chanAmt, 0)

	// With the channel open, we'll create a few invoices for Bob that
	// Alice will pay to in order to advance the state of the channel.
	bobPayReqs := make([]string, numInvoices)
	for i := 0; i < numInvoices; i++ {
		preimage := bytes.Repeat([]byte{byte(255 - i)}, 32)
		invoice := &lnrpc.Invoice{
			Memo:      "testing",
			RPreimage: preimage,
			Value:     paymentAmt,
		}
		resp, err := net.Bob.AddInvoice(ctxb, invoice)
		if err != nil {
			t.Fatalf("unable to add invoice: %v", err)
		}

		bobPayReqs[i] = resp.PaymentRequest
	}

	// As we'll be querying the state of bob's channels frequently we'll
	// create a closure helper function for the purpose.
	getBobChanInfo := func() (*lnrpc.ActiveChannel, error) {
		req := &lnrpc.ListChannelsRequest{}
		bobChannelInfo, err := net.Bob.ListChannels(ctxb, req)
		if err != nil {
			return nil, err
		}
		if len(bobChannelInfo.Channels) != 1 {
			t.Fatalf("bob should only have a single channel, instead he has %v",
				len(bobChannelInfo.Channels))
		}

		return bobChannelInfo.Channels[0], nil
	}

	// Wait for Alice to receive the channel edge from the funding manager.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err := net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("alice didn't see the alice->bob channel before "+
			"timeout: %v", err)
	}

	// Send payments from Alice to Bob using 3 of Bob's payment hashes
	// generated above.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = completePaymentRequests(ctxt, net.Alice, bobPayReqs[:numInvoices/2],
		true)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	// Next query for Bob's channel state, as we sent 3 payments of 10k
	// satoshis each, Bob should now see his balance as being 30k satoshis.
	time.Sleep(time.Millisecond * 200)
	bobChan, err := getBobChanInfo()
	if err != nil {
		t.Fatalf("unable to get bob's channel info: %v", err)
	}
	if bobChan.LocalBalance != 30000 {
		t.Fatalf("bob's balance is incorrect, got %v, expected %v",
			bobChan.LocalBalance, 30000)
	}

	// Grab Bob's current commitment height (update number), we'll later
	// revert him to this state after additional updates to force him to
	// broadcast this soon to be revoked state.
	bobStateNumPreCopy := bobChan.NumUpdates

	// Create a temporary file to house Bob's database state at this
	// particular point in history.
	bobTempDbPath, err := ioutil.TempDir("", "bob-past-state")
	if err != nil {
		t.Fatalf("unable to create temp db folder: %v", err)
	}
	bobTempDbFile := filepath.Join(bobTempDbPath, "channel.db")
	defer os.Remove(bobTempDbPath)

	// With the temporary file created, copy Bob's current state into the
	// temporary file we created above. Later after more updates, we'll
	// restore this state.
	if err := copyFile(bobTempDbFile, net.Bob.DBPath()); err != nil {
		t.Fatalf("unable to copy database files: %v", err)
	}

	// Finally, send payments from Alice to Bob, consuming Bob's remaining
	// payment hashes.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = completePaymentRequests(ctxt, net.Alice, bobPayReqs[numInvoices/2:],
		true)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	bobChan, err = getBobChanInfo()
	if err != nil {
		t.Fatalf("unable to get bob chan info: %v", err)
	}

	// Now we shutdown Bob, copying over the his temporary database state
	// which has the *prior* channel state over his current most up to date
	// state. With this, we essentially force Bob to travel back in time
	// within the channel's history.
	if err = net.RestartNode(net.Bob, func() error {
		return os.Rename(bobTempDbFile, net.Bob.DBPath())
	}); err != nil {
		t.Fatalf("unable to restart node: %v", err)
	}

	// Now query for Bob's channel state, it should show that he's at a
	// state number in the past, not the *latest* state.
	bobChan, err = getBobChanInfo()
	if err != nil {
		t.Fatalf("unable to get bob chan info: %v", err)
	}
	if bobChan.NumUpdates != bobStateNumPreCopy {
		t.Fatalf("db copy failed: %v", bobChan.NumUpdates)
	}

	// Now force Bob to execute a *force* channel closure by unilaterally
	// broadcasting his current channel state. This is actually the
	// commitment transaction of a prior *revoked* state, so he'll soon
	// feel the wrath of Alice's retribution.
	force := true
	closeUpdates, _, err := net.CloseChannel(ctxb, net.Bob, chanPoint, force)
	if err != nil {
		t.Fatalf("unable to close channel: %v", err)
	}

	// Wait for Bob's breach transaction to show up in the mempool to ensure
	// that Alice's node has started waiting for confirmations.
	_, err = waitForTxInMempool(net.Miner.Node, 5*time.Second)
	if err != nil {
		t.Fatalf("unable to find Bob's breach tx in mempool: %v", err)
	}

	// Here, Alice sees Bob's breach transaction in the mempool, but is waiting
	// for it to confirm before continuing her retribution. We restart Alice to
	// ensure that she is persisting her retribution state and continues
	// watching for the breach transaction to confirm even after her node
	// restarts.
	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("unable to restart Alice's node: %v", err)
	}

	// Finally, generate a single block, wait for the final close status
	// update, then ensure that the closing transaction was included in the
	// block.
	block := mineBlocks(t, net, 1)[0]

	breachTXID, err := net.WaitForChannelClose(ctxb, closeUpdates)
	if err != nil {
		t.Fatalf("error while waiting for channel close: %v", err)
	}
	assertTxInBlock(t, block, breachTXID)

	// Query the mempool for Alice's justice transaction, this should be
	// broadcast as Bob's contract breaching transaction gets confirmed
	// above.
	justiceTXID, err := waitForTxInMempool(net.Miner.Node, 5*time.Second)
	if err != nil {
		t.Fatalf("unable to find Alice's justice tx in mempool: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	// Query for the mempool transaction found above. Then assert that all
	// the inputs of this transaction are spending outputs generated by
	// Bob's breach transaction above.
	justiceTx, err := net.Miner.Node.GetRawTransaction(justiceTXID)
	if err != nil {
		t.Fatalf("unable to query for justice tx: %v", err)
	}
	for _, txIn := range justiceTx.MsgTx().TxIn {
		if !bytes.Equal(txIn.PreviousOutPoint.Hash[:], breachTXID[:]) {
			t.Fatalf("justice tx not spending commitment utxo "+
				"instead is: %v", txIn.PreviousOutPoint)
		}
	}

	// We restart Alice here to ensure that she persists her retribution state
	// and successfully continues exacting retribution after restarting. At
	// this point, Alice has broadcast the justice transaction, but it hasn't
	// been confirmed yet; when Alice restarts, she should start waiting for
	// the justice transaction to confirm again.
	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("unable to restart Alice's node: %v", err)
	}

	// Now mine a block, this transaction should include Alice's justice
	// transaction which was just accepted into the mempool.
	block = mineBlocks(t, net, 1)[0]

	// The block should have exactly *two* transactions, one of which is
	// the justice transaction.
	if len(block.Transactions) != 2 {
		t.Fatalf("transaction wasn't mined")
	}
	justiceSha := block.Transactions[1].TxHash()
	if !bytes.Equal(justiceTx.Hash()[:], justiceSha[:]) {
		t.Fatalf("justice tx wasn't mined")
	}

	assertNumChannels(t, ctxb, net.Alice, 0)
}

// testRevokedCloseRetributionZeroValueRemoteOutput tests that Alice is able
// carry out retribution in the event that she fails in state where the remote
// commitment output has zero-value.
func testRevokedCloseRetributionZeroValueRemoteOutput(net *lntest.NetworkHarness,
	t *harnessTest) {

	ctxb := context.Background()
	const (
		timeout     = time.Duration(time.Second * 10)
		chanAmt     = maxFundingAmount
		paymentAmt  = 10000
		numInvoices = 6
	)

	// Since we'd like to test some multi-hop failure scenarios, we'll
	// introduce another node into our test network: Carol.
	carol, err := net.NewNode([]string{"--debughtlc", "--hodlhtlc"})
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}

	// We must let Alice have an open channel before she can send a node
	// announcement, so we open a channel with Carol,
	if err := net.ConnectNodes(ctxb, net.Alice, carol); err != nil {
		t.Fatalf("unable to connect alice to carol: %v", err)
	}

	// In order to test Alice's response to an uncooperative channel
	// closure by Carol, we'll first open up a channel between them with a
	// 0.5 BTC value.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPoint := openChannelAndAssert(ctxt, t, net, net.Alice, carol,
		chanAmt, 0)

	// With the channel open, we'll create a few invoices for Carol that
	// Alice will pay to in order to advance the state of the channel.
	carolPayReqs := make([]string, numInvoices)
	for i := 0; i < numInvoices; i++ {
		preimage := bytes.Repeat([]byte{byte(192 - i)}, 32)
		invoice := &lnrpc.Invoice{
			Memo:      "testing",
			RPreimage: preimage,
			Value:     paymentAmt,
		}
		resp, err := carol.AddInvoice(ctxb, invoice)
		if err != nil {
			t.Fatalf("unable to add invoice: %v", err)
		}

		carolPayReqs[i] = resp.PaymentRequest
	}

	// As we'll be querying the state of Carols's channels frequently we'll
	// create a closure helper function for the purpose.
	getCarolChanInfo := func() (*lnrpc.ActiveChannel, error) {
		req := &lnrpc.ListChannelsRequest{}
		carolChannelInfo, err := carol.ListChannels(ctxb, req)
		if err != nil {
			return nil, err
		}
		if len(carolChannelInfo.Channels) != 1 {
			t.Fatalf("carol should only have a single channel, "+
				"instead he has %v", len(carolChannelInfo.Channels))
		}

		return carolChannelInfo.Channels[0], nil
	}

	// Wait for Alice to receive the channel edge from the funding manager.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("alice didn't see the alice->carol channel before "+
			"timeout: %v", err)
	}

	// Next query for Carol's channel state, as we sent 0 payments, Carol
	// should now see her balance as being 0 satoshis.
	carolChan, err := getCarolChanInfo()
	if err != nil {
		t.Fatalf("unable to get carol's channel info: %v", err)
	}
	if carolChan.LocalBalance != 0 {
		t.Fatalf("carol's balance is incorrect, got %v, expected %v",
			carolChan.LocalBalance, 0)
	}

	// Grab Carol's current commitment height (update number), we'll later
	// revert her to this state after additional updates to force him to
	// broadcast this soon to be revoked state.
	carolStateNumPreCopy := carolChan.NumUpdates

	// Create a temporary file to house Carol's database state at this
	// particular point in history.
	carolTempDbPath, err := ioutil.TempDir("", "carol-past-state")
	if err != nil {
		t.Fatalf("unable to create temp db folder: %v", err)
	}
	carolTempDbFile := filepath.Join(carolTempDbPath, "channel.db")
	defer os.Remove(carolTempDbPath)

	// With the temporary file created, copy Carol's current state into the
	// temporary file we created above. Later after more updates, we'll
	// restore this state.
	if err := copyFile(carolTempDbFile, carol.DBPath()); err != nil {
		t.Fatalf("unable to copy database files: %v", err)
	}

	// Finally, send payments from Alice to Carol, consuming Carol's remaining
	// payment hashes.
	err = completePaymentRequests(ctxb, net.Alice, carolPayReqs, false)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	carolChan, err = getCarolChanInfo()
	if err != nil {
		t.Fatalf("unable to get carol chan info: %v", err)
	}

	// Now we shutdown Carol, copying over the his temporary database state
	// which has the *prior* channel state over his current most up to date
	// state. With this, we essentially force Carol to travel back in time
	// within the channel's history.
	if err = net.RestartNode(carol, func() error {
		return os.Rename(carolTempDbFile, carol.DBPath())
	}); err != nil {
		t.Fatalf("unable to restart node: %v", err)
	}

	// Now query for Carol's channel state, it should show that he's at a
	// state number in the past, not the *latest* state.
	carolChan, err = getCarolChanInfo()
	if err != nil {
		t.Fatalf("unable to get carol chan info: %v", err)
	}
	if carolChan.NumUpdates != carolStateNumPreCopy {
		t.Fatalf("db copy failed: %v", carolChan.NumUpdates)
	}

	// Now force Carol to execute a *force* channel closure by unilaterally
	// broadcasting his current channel state. This is actually the
	// commitment transaction of a prior *revoked* state, so he'll soon
	// feel the wrath of Alice's retribution.
	force := true
	closeUpdates, _, err := net.CloseChannel(ctxb, carol, chanPoint, force)
	if err != nil {
		t.Fatalf("unable to close channel: %v", err)
	}

	// Finally, generate a single block, wait for the final close status
	// update, then ensure that the closing transaction was included in the
	// block.
	block := mineBlocks(t, net, 1)[0]

	// Here, Alice receives a confirmation of Carol's breach transaction.
	// We restart Alice to ensure that she is persisting her retribution
	// state and continues exacting justice after her node restarts.
	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("unable to stop Alice's node: %v", err)
	}

	breachTXID, err := net.WaitForChannelClose(ctxb, closeUpdates)
	if err != nil {
		t.Fatalf("error while waiting for channel close: %v", err)
	}
	assertTxInBlock(t, block, breachTXID)

	// Query the mempool for Alice's justice transaction, this should be
	// broadcast as Carol's contract breaching transaction gets confirmed
	// above.
	justiceTXID, err := waitForTxInMempool(net.Miner.Node, 15*time.Second)
	if err != nil {
		t.Fatalf("unable to find Alice's justice tx in mempool: %v",
			err)
	}
	time.Sleep(100 * time.Millisecond)

	// Query for the mempool transaction found above. Then assert that all
	// the inputs of this transaction are spending outputs generated by
	// Carol's breach transaction above.
	justiceTx, err := net.Miner.Node.GetRawTransaction(justiceTXID)
	if err != nil {
		t.Fatalf("unable to query for justice tx: %v", err)
	}
	for _, txIn := range justiceTx.MsgTx().TxIn {
		if !bytes.Equal(txIn.PreviousOutPoint.Hash[:], breachTXID[:]) {
			t.Fatalf("justice tx not spending commitment utxo "+
				"instead is: %v", txIn.PreviousOutPoint)
		}
	}

	// We restart Alice here to ensure that she persists her retribution state
	// and successfully continues exacting retribution after restarting. At
	// this point, Alice has broadcast the justice transaction, but it hasn't
	// been confirmed yet; when Alice restarts, she should start waiting for
	// the justice transaction to confirm again.
	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("unable to restart Alice's node: %v", err)
	}

	// Now mine a block, this transaction should include Alice's justice
	// transaction which was just accepted into the mempool.
	block = mineBlocks(t, net, 1)[0]

	// The block should have exactly *two* transactions, one of which is
	// the justice transaction.
	if len(block.Transactions) != 2 {
		t.Fatalf("transaction wasn't mined")
	}
	justiceSha := block.Transactions[1].TxHash()
	if !bytes.Equal(justiceTx.Hash()[:], justiceSha[:]) {
		t.Fatalf("justice tx wasn't mined")
	}

	assertNumChannels(t, ctxb, net.Alice, 0)
}

// testRevokedCloseRetributionRemoteHodl tests that Alice properly responds to a
// channel breach made by the remote party, specifically in the case that the
// remote party breaches before settling extended HTLCs.
func testRevokedCloseRetributionRemoteHodl(net *lntest.NetworkHarness,
	t *harnessTest) {

	ctxb := context.Background()
	const (
		timeout     = time.Duration(time.Second * 10)
		chanAmt     = maxFundingAmount
		pushAmt     = 20000
		paymentAmt  = 10000
		numInvoices = 6
	)

	// Since this test will result in the counterparty being left in a weird
	// state, we will introduce another node into our test network: Carol.
	carol, err := net.NewNode([]string{"--debughtlc", "--hodlhtlc"})
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}

	// We must let Alice communicate with Carol before they are able to
	// open channel, so we connect Alice and Carol,
	if err := net.ConnectNodes(ctxb, net.Alice, carol); err != nil {
		t.Fatalf("unable to connect alice to carol: %v", err)
	}

	// In order to test Alice's response to an uncooperative channel
	// closure by Carol, we'll first open up a channel between them with a
	// maxFundingAmount (2^24) satoshis value.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPoint := openChannelAndAssert(ctxt, t, net, net.Alice, carol,
		chanAmt, pushAmt)

	// With the channel open, we'll create a few invoices for Carol that
	// Alice will pay to in order to advance the state of the channel.
	carolPayReqs := make([]string, numInvoices)
	for i := 0; i < numInvoices; i++ {
		preimage := bytes.Repeat([]byte{byte(192 - i)}, 32)
		invoice := &lnrpc.Invoice{
			Memo:      "testing",
			RPreimage: preimage,
			Value:     paymentAmt,
		}
		resp, err := carol.AddInvoice(ctxb, invoice)
		if err != nil {
			t.Fatalf("unable to add invoice: %v", err)
		}

		carolPayReqs[i] = resp.PaymentRequest
	}

	// As we'll be querying the state of Carol's channels frequently we'll
	// create a closure helper function for the purpose.
	getCarolChanInfo := func() (*lnrpc.ActiveChannel, error) {
		req := &lnrpc.ListChannelsRequest{}
		carolChannelInfo, err := carol.ListChannels(ctxb, req)
		if err != nil {
			return nil, err
		}
		if len(carolChannelInfo.Channels) != 1 {
			t.Fatalf("carol should only have a single channel, instead he has %v",
				len(carolChannelInfo.Channels))
		}

		return carolChannelInfo.Channels[0], nil
	}
	// We'll introduce a closure to validate that Carol's current balance
	// matches the given expected amount.
	checkCarolBalance := func(expectedAmt int64) {
		carolChan, err := getCarolChanInfo()
		if err != nil {
			t.Fatalf("unable to get carol's channel info: %v", err)
		}
		if carolChan.LocalBalance != expectedAmt {
			t.Fatalf("carol's balance is incorrect, "+
				"got %v, expected %v", carolChan.LocalBalance,
				expectedAmt)
		}
	}
	// We'll introduce another closure to validate that Carol's current
	// number of updates is at least as large as the provided minimum
	// number.
	checkCarolNumUpdatesAtleast := func(minimum uint64) {
		carolChan, err := getCarolChanInfo()
		if err != nil {
			t.Fatalf("unable to get carol's channel info: %v", err)
		}
		if carolChan.NumUpdates < minimum {
			t.Fatalf("carol's numupdates is incorrect, want %v "+
				"to be at least %v", carolChan.NumUpdates,
				minimum)
		}
	}

	// Wait for Alice to receive the channel edge from the funding manager.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("alice didn't see the alice->carol channel before "+
			"timeout: %v", err)
	}

	// Ensure that carol's balance starts with the amount we pushed to her.
	checkCarolBalance(pushAmt)

	// Send payments from Alice to Carol using 3 of Carol's payment hashes
	// generated above.
	err = completePaymentRequests(ctxb, net.Alice, carolPayReqs[:numInvoices/2],
		false)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	// Next query for Carol's channel state, as we sent 3 payments of 10k
	// satoshis each, however Carol should now see her balance as being
	// equal to the push amount in satoshis since she has not settled.
	carolChan, err := getCarolChanInfo()
	if err != nil {
		t.Fatalf("unable to get carol's channel info: %v", err)
	}
	// Grab Carol's current commitment height (update number), we'll later
	// revert her to this state after additional updates to force her to
	// broadcast this soon to be revoked state.
	carolStateNumPreCopy := carolChan.NumUpdates

	// Ensure that carol's balance still reflects the original amount we
	// pushed to her.
	checkCarolBalance(pushAmt)
	// Since Carol has not settled, she should only see at least one update
	// to her channel.
	checkCarolNumUpdatesAtleast(1)

	// Create a temporary file to house Carol's database state at this
	// particular point in history.
	carolTempDbPath, err := ioutil.TempDir("", "carol-past-state")
	if err != nil {
		t.Fatalf("unable to create temp db folder: %v", err)
	}
	carolTempDbFile := filepath.Join(carolTempDbPath, "channel.db")
	defer os.Remove(carolTempDbPath)

	// With the temporary file created, copy Carol's current state into the
	// temporary file we created above. Later after more updates, we'll
	// restore this state.
	if err := copyFile(carolTempDbFile, carol.DBPath()); err != nil {
		t.Fatalf("unable to copy database files: %v", err)
	}

	// Finally, send payments from Alice to Carol, consuming Carol's remaining
	// payment hashes.
	err = completePaymentRequests(ctxb, net.Alice, carolPayReqs[numInvoices/2:],
		false)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	// Ensure that carol's balance still shows the amount we originally
	// pushed to her, and that at least one more update has occurred.
	time.Sleep(500 * time.Millisecond)
	checkCarolBalance(pushAmt)
	checkCarolNumUpdatesAtleast(carolStateNumPreCopy + 1)

	// Now we shutdown Carol, copying over the her temporary database state
	// which has the *prior* channel state over her current most up to date
	// state. With this, we essentially force Carol to travel back in time
	// within the channel's history.
	if err = net.RestartNode(carol, func() error {
		return os.Rename(carolTempDbFile, carol.DBPath())
	}); err != nil {
		t.Fatalf("unable to restart node: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	// Ensure that Carol's view of the channel is consistent with the
	// state of the channel just before it was snapshotted.
	checkCarolBalance(pushAmt)
	checkCarolNumUpdatesAtleast(1)

	// Now query for Carol's channel state, it should show that she's at a
	// state number in the past, *not* the latest state.
	carolChan, err = getCarolChanInfo()
	if err != nil {
		t.Fatalf("unable to get carol chan info: %v", err)
	}
	if carolChan.NumUpdates != carolStateNumPreCopy {
		t.Fatalf("db copy failed: %v", carolChan.NumUpdates)
	}

	// Now force Carol to execute a *force* channel closure by unilaterally
	// broadcasting her current channel state. This is actually the
	// commitment transaction of a prior *revoked* state, so she'll soon
	// feel the wrath of Alice's retribution.
	force := true
	closeUpdates, _, err := net.CloseChannel(ctxb, carol, chanPoint, force)
	if err != nil {
		t.Fatalf("unable to close channel: %v", err)
	}

	// Query the mempool for Alice's justice transaction, this should be
	// broadcast as Carol's contract breaching transaction gets confirmed
	// above.
	_, err = waitForTxInMempool(net.Miner.Node, 5*time.Second)
	if err != nil {
		t.Fatalf("unable to find Alice's justice tx in mempool: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	// Generate a single block to mine the breach transaction.
	block := mineBlocks(t, net, 1)[0]

	// Wait so Alice receives a confirmation of Carol's breach transaction.
	time.Sleep(200 * time.Millisecond)

	// We restart Alice to ensure that she is persisting her retribution
	// state and continues exacting justice after her node restarts.
	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("unable to stop Alice's node: %v", err)
	}

	// Finally, Wait for the final close status update, then ensure that the
	// closing transaction was included in the block.
	breachTXID, err := net.WaitForChannelClose(ctxb, closeUpdates)
	if err != nil {
		t.Fatalf("error while waiting for channel close: %v", err)
	}
	assertTxInBlock(t, block, breachTXID)

	// Query the mempool for Alice's justice transaction, this should be
	// broadcast as Carol's contract breaching transaction gets confirmed
	// above.
	justiceTXID, err := waitForTxInMempool(net.Miner.Node, 5*time.Second)
	if err != nil {
		t.Fatalf("unable to find Alice's justice tx in mempool: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	// We restart Alice here to ensure that she persists her retribution state
	// and successfully continues exacting retribution after restarting. At
	// this point, Alice has broadcast the justice transaction, but it hasn't
	// been confirmed yet; when Alice restarts, she should start waiting for
	// the justice transaction to confirm again.
	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("unable to restart Alice's node: %v", err)
	}

	// Query for the mempool transaction found above. Then assert that (1)
	// the justice tx has the appropriate number of inputs, and (2) all
	// the inputs of this transaction are spending outputs generated by
	// Carol's breach transaction above.
	justiceTx, err := net.Miner.Node.GetRawTransaction(justiceTXID)
	if err != nil {
		t.Fatalf("unable to query for justice tx: %v", err)
	}
	exNumInputs := 2 + numInvoices/2
	if len(justiceTx.MsgTx().TxIn) != exNumInputs {
		t.Fatalf("justice tx should have exactly 2 commitment inputs"+
			"and %v htlc inputs, expected %v in total, got %v",
			numInvoices/2, exNumInputs,
			len(justiceTx.MsgTx().TxIn))
	}
	for _, txIn := range justiceTx.MsgTx().TxIn {
		if !bytes.Equal(txIn.PreviousOutPoint.Hash[:], breachTXID[:]) {
			t.Fatalf("justice tx not spending commitment utxo "+
				"instead is: %v", txIn.PreviousOutPoint)
		}
	}

	// Now mine a block, this transaction should include Alice's justice
	// transaction which was just accepted into the mempool.
	block = mineBlocks(t, net, 1)[0]

	// The block should have exactly *two* transactions, one of which is
	// the justice transaction.
	if len(block.Transactions) != 2 {
		t.Fatalf("transaction wasn't mined")
	}
	justiceSha := block.Transactions[1].TxHash()
	if !bytes.Equal(justiceTx.Hash()[:], justiceSha[:]) {
		t.Fatalf("justice tx wasn't mined")
	}

	assertNumChannels(t, ctxb, net.Alice, 0)
}

// assertNumChannels polls the provided node's list channels rpc until it
// reaches the desired number of total channels.
func assertNumChannels(t *harnessTest, ctxb context.Context,
	node *lntest.HarnessNode, numChannels int) {

	// Poll alice for her list of channels.
	req := &lnrpc.ListChannelsRequest{}

	var predErr error
	pred := func() bool {
		chanInfo, err := node.ListChannels(ctxb, req)
		if err != nil {
			predErr = fmt.Errorf("unable to query for alice's "+
				"channels: %v", err)
			return false
		}

		// Return true if the query returned the expected number of
		// channels.
		return len(chanInfo.Channels) == numChannels
	}

	if err := lntest.WaitPredicate(pred, time.Second*15); err != nil {
		t.Fatalf("node has incorrect number of channels: %v", predErr)
	}
}

func testHtlcErrorPropagation(net *lntest.NetworkHarness, t *harnessTest) {
	// In this test we wish to exercise the daemon's correct parsing,
	// handling, and propagation of errors that occur while processing a
	// multi-hop payment.
	timeout := time.Duration(time.Second * 15)
	ctxb := context.Background()

	const chanAmt = maxFundingAmount

	// First establish a channel with a capacity of 0.5 BTC between Alice
	// and Bob.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPointAlice := openChannelAndAssert(ctxt, t, net, net.Alice, net.Bob,
		chanAmt, 0)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	if err := net.Alice.WaitForNetworkChannelOpen(ctxt, chanPointAlice); err != nil {
		t.Fatalf("channel not seen by alice before timeout: %v", err)
	}

	commitFee := calcStaticFee(0)
	assertBaseBalance := func() {
		balReq := &lnrpc.ChannelBalanceRequest{}
		aliceBal, err := net.Alice.ChannelBalance(ctxb, balReq)
		if err != nil {
			t.Fatalf("unable to get channel balance: %v", err)
		}
		bobBal, err := net.Bob.ChannelBalance(ctxb, balReq)
		if err != nil {
			t.Fatalf("unable to get channel balance: %v", err)
		}
		if aliceBal.Balance != int64(chanAmt-commitFee) {
			t.Fatalf("alice has an incorrect balance: expected %v got %v",
				int64(chanAmt-commitFee), aliceBal)
		}
		if bobBal.Balance != int64(chanAmt-commitFee) {
			t.Fatalf("bob has an incorrect balance: expected %v got %v",
				int64(chanAmt-commitFee), bobBal)
		}
	}

	// Since we'd like to test some multi-hop failure scenarios, we'll
	// introduce another node into our test network: Carol.
	carol, err := net.NewNode(nil)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}

	// Next, we'll create a connection from Bob to Carol, and open a
	// channel between them so we have the topology: Alice -> Bob -> Carol.
	// The channel created will be of lower capacity that the one created
	// above.
	if err := net.ConnectNodes(ctxb, net.Bob, carol); err != nil {
		t.Fatalf("unable to connect bob to carol: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	const bobChanAmt = maxFundingAmount
	chanPointBob := openChannelAndAssert(ctxt, t, net, net.Bob, carol,
		chanAmt, 0)

	// Ensure that Alice has Carol in her routing table before proceeding.
	nodeInfoReq := &lnrpc.NodeInfoRequest{
		PubKey: carol.PubKeyStr,
	}
	checkTableTimeout := time.After(time.Second * 10)
	checkTableTicker := time.NewTicker(100 * time.Millisecond)
	defer checkTableTicker.Stop()

out:
	// TODO(roasbeef): make into async hook for node announcements
	for {
		select {
		case <-checkTableTicker.C:
			_, err := net.Alice.GetNodeInfo(ctxb, nodeInfoReq)
			if err != nil && strings.Contains(err.Error(),
				"unable to find") {

				continue
			}

			break out
		case <-checkTableTimeout:
			t.Fatalf("carol's node announcement didn't propagate within " +
				"the timeout period")
		}
	}

	// With the channels, open we can now start to test our multi-hop error
	// scenarios. First, we'll generate an invoice from carol that we'll
	// use to test some error cases.
	const payAmt = 10000
	invoiceReq := &lnrpc.Invoice{
		Memo:  "kek99",
		Value: payAmt,
	}
	carolInvoice, err := carol.AddInvoice(ctxb, invoiceReq)
	if err != nil {
		t.Fatalf("unable to generate carol invoice: %v", err)
	}

	// Before we send the payment, ensure that the announcement of the new
	// channel has been processed by Alice.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	if err := net.Alice.WaitForNetworkChannelOpen(ctxt, chanPointBob); err != nil {
		t.Fatalf("channel not seen by alice before timeout: %v", err)
	}

	// For the first scenario, we'll test the cancellation of an HTLC with
	// an unknown payment hash.
	// TODO(roasbeef): return failure response rather than failing entire
	// stream on payment error.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	sendReq := &lnrpc.SendRequest{
		PaymentHashString: hex.EncodeToString(bytes.Repeat([]byte("Z"), 32)),
		DestString:        hex.EncodeToString(carol.PubKey[:]),
		Amt:               payAmt,
	}
	resp, err := net.Alice.SendPaymentSync(ctxt, sendReq)
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	// The payment should've resulted in an error since we sent it with the
	// wrong payment hash.
	if resp.PaymentError == "" {
		t.Fatalf("payment should have been rejected due to invalid " +
			"payment hash")
	}
	expectedErrorCode := lnwire.CodeUnknownPaymentHash.String()
	if !strings.Contains(resp.PaymentError, expectedErrorCode) {
		// TODO(roasbeef): make into proper gRPC error code
		t.Fatalf("payment should have failed due to unknown payment hash, "+
			"instead failed due to: %v", resp.PaymentError)
	}

	// The balances of all parties should be the same as initially since
	// the HTLC was cancelled.
	assertBaseBalance()

	// Next, we'll test the case of a recognized payHash but, an incorrect
	// value on the extended HTLC.
	sendReq = &lnrpc.SendRequest{
		PaymentHashString: hex.EncodeToString(carolInvoice.RHash),
		DestString:        hex.EncodeToString(carol.PubKey[:]),
		Amt:               1000, // 10k satoshis are expected.
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	resp, err = net.Alice.SendPaymentSync(ctxt, sendReq)
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	// The payment should fail with an error since we sent 1k satoshis isn't of
	// 10k as was requested.
	if resp.PaymentError == "" {
		t.Fatalf("payment should have been rejected due to wrong " +
			"HTLC amount")
	}
	expectedErrorCode = lnwire.CodeIncorrectPaymentAmount.String()
	if !strings.Contains(resp.PaymentError, expectedErrorCode) {
		t.Fatalf("payment should have failed due to wrong amount, "+
			"instead failed due to: %v", resp.PaymentError)
	}

	// The balances of all parties should be the same as initially since
	// the HTLC was cancelled.
	assertBaseBalance()

	// Next we'll test an error that occurs mid-route due to an outgoing
	// link having insufficient capacity. In order to do so, we'll first
	// need to unbalance the link connecting Bob<->Carol.
	bobPayStream, err := net.Bob.SendPayment(ctxb)
	if err != nil {
		t.Fatalf("unable to create payment stream: %v", err)
	}

	// To do so, we'll push most of the funds in the channel over to
	// Alice's side, leaving on 10k satoshis of available balance for bob.
	// There's a max payment amount, so we'll have to do this
	// incrementally.
	amtToSend := int64(chanAmt) - 20000
	amtSent := int64(0)
	for amtSent != amtToSend {
		// We'll send in chunks of the max payment amount. If we're
		// about to send too much, then we'll only send the amount
		// remaining.
		toSend := int64(maxPaymentMSat.ToSatoshis())
		if toSend+amtSent > amtToSend {
			toSend = amtToSend - amtSent
		}

		invoiceReq = &lnrpc.Invoice{
			Value: toSend,
		}
		carolInvoice2, err := carol.AddInvoice(ctxb, invoiceReq)
		if err != nil {
			t.Fatalf("unable to generate carol invoice: %v", err)
		}
		if err := bobPayStream.Send(&lnrpc.SendRequest{
			PaymentRequest: carolInvoice2.PaymentRequest,
		}); err != nil {
			t.Fatalf("unable to send payment: %v", err)
		}

		if resp, err := bobPayStream.Recv(); err != nil {
			t.Fatalf("payment stream has been closed: %v", err)
		} else if resp.PaymentError != "" {
			t.Fatalf("bob's payment failed: %v", resp.PaymentError)
		}

		amtSent += toSend
	}

	// At this point, Alice has 50mil satoshis on her side of the channel,
	// but Bob only has 10k available on his side of the channel. So a
	// payment from Alice to Carol worth 100k satoshis should fail.
	invoiceReq = &lnrpc.Invoice{
		Value: 100000,
	}
	carolInvoice3, err := carol.AddInvoice(ctxb, invoiceReq)
	if err != nil {
		t.Fatalf("unable to generate carol invoice: %v", err)
	}

	sendReq = &lnrpc.SendRequest{
		PaymentRequest: carolInvoice3.PaymentRequest,
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	resp, err = net.Alice.SendPaymentSync(ctxt, sendReq)
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}
	if resp.PaymentError == "" {
		t.Fatalf("payment should fail due to insufficient "+
			"capacity: %v", err)
	} else if !strings.Contains(resp.PaymentError,
		lnwire.CodeTemporaryChannelFailure.String()) {
		t.Fatalf("payment should fail due to insufficient capacity, "+
			"instead: %v", resp.PaymentError)
	}

	// For our final test, we'll ensure that if a target link isn't
	// available for what ever reason then the payment fails accordingly.
	//
	// We'll attempt to complete the original invoice we created with Carol
	// above, but before we do so, Carol will go offline, resulting in a
	// failed payment.
	if err := net.ShutdownNode(carol); err != nil {
		t.Fatalf("unable to shutdown carol: %v", err)
	}
	// TODO(roasbeef): mission control
	time.Sleep(time.Second * 5)

	sendReq = &lnrpc.SendRequest{
		PaymentRequest: carolInvoice.PaymentRequest,
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	resp, err = net.Alice.SendPaymentSync(ctxt, sendReq)
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	if resp.PaymentError == "" {
		t.Fatalf("payment should have failed")
	}
	expectedErrorCode = lnwire.CodeUnknownNextPeer.String()
	if !strings.Contains(resp.PaymentError, expectedErrorCode) {
		t.Fatalf("payment should fail due to unknown hop, instead: %v",
			resp.PaymentError)
	}

	// Finally, immediately close the channel. This function will also
	// block until the channel is closed and will additionally assert the
	// relevant channel closing post conditions.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPointAlice, false)

	// Force close Bob's final channel, also mining enough blocks to
	// trigger a sweep of the funds by the utxoNursery.
	// TODO(roasbeef): use config value for default CSV here.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Bob, chanPointBob, true)
	if _, err := net.Miner.Node.Generate(5); err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}
}

// subscribeGraphNotifications subscribes to channel graph updates and launches
// a goroutine that forwards these to the returned channel.
func subscribeGraphNotifications(t *harnessTest, ctxb context.Context,
	node *lntest.HarnessNode) (chan *lnrpc.GraphTopologyUpdate, chan struct{}) {
	// We'll first start by establishing a notification client which will
	// send us notifications upon detected changes in the channel graph.
	req := &lnrpc.GraphTopologySubscription{}
	topologyClient, err := node.SubscribeChannelGraph(ctxb, req)
	if err != nil {
		t.Fatalf("unable to create topology client: %v", err)
	}

	// We'll launch a goroutine that'll be responsible for proxying all
	// notifications recv'd from the client into the channel below.
	quit := make(chan struct{})
	graphUpdates := make(chan *lnrpc.GraphTopologyUpdate, 20)
	go func() {
		for {
			select {
			case <-quit:
				return
			default:
				graphUpdate, err := topologyClient.Recv()
				select {
				case <-quit:
					return
				default:
				}

				if err == io.EOF {
					return
				} else if err != nil {
					t.Fatalf("unable to recv graph update: %v",
						err)
				}

				select {
				case graphUpdates <- graphUpdate:
				case <-quit:
					return
				}
			}
		}
	}()
	return graphUpdates, quit
}

func testGraphTopologyNotifications(net *lntest.NetworkHarness, t *harnessTest) {
	const chanAmt = maxFundingAmount
	timeout := time.Duration(time.Second * 5)
	ctxb := context.Background()

	// Let Alice subscribe to graph notifications.
	graphUpdates, quit := subscribeGraphNotifications(t, ctxb, net.Alice)

	// Open a new channel between Alice and Bob.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPoint := openChannelAndAssert(ctxt, t, net, net.Alice, net.Bob,
		chanAmt, 0)

	// The channel opening above should've triggered a few notifications
	// sent to the notification client. We'll expect two channel updates,
	// and two node announcements.
	const numExpectedUpdates = 4
	for i := 0; i < numExpectedUpdates; i++ {
		select {
		// Ensure that a new update for both created edges is properly
		// dispatched to our registered client.
		case graphUpdate := <-graphUpdates:

			if len(graphUpdate.ChannelUpdates) > 0 {
				chanUpdate := graphUpdate.ChannelUpdates[0]
				if chanUpdate.Capacity != int64(chanAmt) {
					t.Fatalf("channel capacities mismatch:"+
						" expected %v, got %v", chanAmt,
						btcutil.Amount(chanUpdate.Capacity))
				}
				switch chanUpdate.AdvertisingNode {
				case net.Alice.PubKeyStr:
				case net.Bob.PubKeyStr:
				default:
					t.Fatalf("unknown advertising node: %v",
						chanUpdate.AdvertisingNode)
				}
				switch chanUpdate.ConnectingNode {
				case net.Alice.PubKeyStr:
				case net.Bob.PubKeyStr:
				default:
					t.Fatalf("unknown connecting node: %v",
						chanUpdate.ConnectingNode)
				}
			}

			if len(graphUpdate.NodeUpdates) > 0 {
				nodeUpdate := graphUpdate.NodeUpdates[0]
				switch nodeUpdate.IdentityKey {
				case net.Alice.PubKeyStr:
				case net.Bob.PubKeyStr:
				default:
					t.Fatalf("unknown node: %v",
						nodeUpdate.IdentityKey)
				}
			}
		case <-time.After(time.Second * 10):
			t.Fatalf("timeout waiting for graph notification %v", i)
		}
	}

	_, blockHeight, err := net.Miner.Node.GetBestBlock()
	if err != nil {
		t.Fatalf("unable to get current blockheight %v", err)
	}

	// Now we'll test that updates are properly sent after channels are closed
	// within the network.
	ctxt, _ = context.WithTimeout(context.Background(), timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint, false)

	// Similar to the case above, we should receive another notification
	// detailing the channel closure.
	select {
	case graphUpdate := <-graphUpdates:
		if len(graphUpdate.ClosedChans) != 1 {
			t.Fatalf("expected a single update, instead "+
				"have %v", len(graphUpdate.ClosedChans))
		}

		closedChan := graphUpdate.ClosedChans[0]
		if closedChan.ClosedHeight != uint32(blockHeight+1) {
			t.Fatalf("close heights of channel mismatch: expected "+
				"%v, got %v", blockHeight+1, closedChan.ClosedHeight)
		}
		if !bytes.Equal(closedChan.ChanPoint.FundingTxid,
			chanPoint.FundingTxid) {
			t.Fatalf("channel point hash mismatch: expected %v, "+
				"got %v", chanPoint.FundingTxid,
				closedChan.ChanPoint.FundingTxid)
		}
		if closedChan.ChanPoint.OutputIndex != chanPoint.OutputIndex {
			t.Fatalf("output index mismatch: expected %v, got %v",
				chanPoint.OutputIndex, closedChan.ChanPoint)
		}
	case <-time.After(time.Second * 10):
		t.Fatalf("notification for channel closure not " +
			"sent")
	}

	// For the final portion of the test, we'll ensure that once a new node
	// appears in the network, the proper notification is dispatched. Note
	// that a node that does not have any channels open is ignored, so first
	// we disconnect Alice and Bob, open a channel between Bob and Carol,
	// and finally connect Alice to Bob again.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	if err := net.DisconnectNodes(ctxt, net.Alice, net.Bob); err != nil {
		t.Fatalf("unable to disconnect alice and bob: %v", err)
	}
	carol, err := net.NewNode(nil)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}

	if err := net.ConnectNodes(ctxb, net.Bob, carol); err != nil {
		t.Fatalf("unable to connect bob to carol: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	chanPoint = openChannelAndAssert(ctxt, t, net, net.Bob, carol,
		chanAmt, 0)

	// Reconnect Alice and Bob. This should result in the nodes syncing up
	// their respective graph state, with the new addition being the
	// existence of Carol in the graph, and also the channel between Bob
	// and Carol. Note that we will also receive a node announcement from
	// Bob, since a node will update its node announcement after a new
	// channel is opened.
	if err := net.ConnectNodes(ctxb, net.Alice, net.Bob); err != nil {
		t.Fatalf("unable to connect alice to bob: %v", err)
	}

	// We should receive an update advertising the newly connected node,
	// Bob's new node announcement, and the channel between Bob and Carol.
	for i := 0; i < 3; i++ {
		select {
		case graphUpdate := <-graphUpdates:
			if len(graphUpdate.NodeUpdates) > 0 {
				nodeUpdate := graphUpdate.NodeUpdates[0]
				switch nodeUpdate.IdentityKey {
				case carol.PubKeyStr:
				case net.Bob.PubKeyStr:
				default:
					t.Fatalf("unknown node update pubey: %v",
						nodeUpdate.IdentityKey)
				}
			}

			if len(graphUpdate.ChannelUpdates) > 0 {
				chanUpdate := graphUpdate.ChannelUpdates[0]
				if chanUpdate.Capacity != int64(chanAmt) {
					t.Fatalf("channel capacities mismatch:"+
						" expected %v, got %v", chanAmt,
						btcutil.Amount(chanUpdate.Capacity))
				}
				switch chanUpdate.AdvertisingNode {
				case carol.PubKeyStr:
				case net.Bob.PubKeyStr:
				default:
					t.Fatalf("unknown advertising node: %v",
						chanUpdate.AdvertisingNode)
				}
				switch chanUpdate.ConnectingNode {
				case carol.PubKeyStr:
				case net.Bob.PubKeyStr:
				default:
					t.Fatalf("unknown connecting node: %v",
						chanUpdate.ConnectingNode)
				}
			}
		case <-time.After(time.Second * 10):
			t.Fatalf("timeout waiting for graph notification %v", i)
		}
	}

	// Close the channel between Bob and Carol.
	ctxt, _ = context.WithTimeout(context.Background(), timeout)
	closeChannelAndAssert(ctxt, t, net, net.Bob, chanPoint, false)

	close(quit)

	// Finally, shutdown carol as our test has concluded successfully.
	if err := net.ShutdownNode(carol); err != nil {
		t.Fatalf("unable to shutdown carol: %v", err)
	}
}

// testNodeAnnouncement ensures that when a node is started with one or more
// external IP addresses specified on the command line, that those addresses
// announced to the network and reported in the network graph.
func testNodeAnnouncement(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	ipAddresses := map[string]bool{
		"192.168.1.1:8333":                            true,
		"[2001:db8:85a3:8d3:1319:8a2e:370:7348]:8337": true,
	}

	var lndArgs []string
	for address := range ipAddresses {
		lndArgs = append(lndArgs, "--externalip="+address)
	}

	dave, err := net.NewNode(lndArgs)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}

	// We must let Dave have an open channel before he can send a node
	// announcement, so we open a channel with Bob,
	if err := net.ConnectNodes(ctxb, net.Bob, dave); err != nil {
		t.Fatalf("unable to connect bob to carol: %v", err)
	}

	timeout := time.Duration(time.Second * 5)
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPoint := openChannelAndAssert(ctxt, t, net, net.Bob, dave,
		1000000, 0)

	// When Alice now connects with Dave, Alice will get his node announcement.
	if err := net.ConnectNodes(ctxb, net.Alice, dave); err != nil {
		t.Fatalf("unable to connect bob to carol: %v", err)
	}

	time.Sleep(time.Second * 1)
	req := &lnrpc.ChannelGraphRequest{}
	chanGraph, err := net.Alice.DescribeGraph(ctxb, req)
	if err != nil {
		t.Fatalf("unable to query for alice's routing table: %v", err)
	}

	for _, node := range chanGraph.Nodes {
		if node.PubKey == dave.PubKeyStr {
			for _, address := range node.Addresses {
				addrStr := address.String()

				// parse the IP address from the string
				// representation of the TCPAddr
				parts := strings.Split(addrStr, "\"")
				if ipAddresses[parts[3]] {
					delete(ipAddresses, parts[3])
				} else {
					if !strings.HasPrefix(parts[3],
						"127.0.0.1:") {
						t.Fatalf("unexpected IP: %v",
							parts[3])
					}
				}
			}
		}
	}
	if len(ipAddresses) != 0 {
		t.Fatalf("expected IP addresses not in channel "+
			"graph: %v", ipAddresses)
	}

	// Close the channel between Bob and Dave.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Bob, chanPoint, false)

	if err := net.ShutdownNode(dave); err != nil {
		t.Fatalf("unable to shutdown dave: %v", err)
	}
}

func testNodeSignVerify(net *lntest.NetworkHarness, t *harnessTest) {
	timeout := time.Duration(time.Second * 15)
	ctxb := context.Background()

	chanAmt := maxFundingAmount
	pushAmt := btcutil.Amount(100000)

	// Create a channel between alice and bob.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	aliceBobCh := openChannelAndAssert(ctxt, t, net, net.Alice, net.Bob,
		chanAmt, pushAmt)

	aliceMsg := []byte("alice msg")

	// alice signs "alice msg" and sends her signature to bob.
	sigReq := &lnrpc.SignMessageRequest{Msg: aliceMsg}
	sigResp, err := net.Alice.SignMessage(ctxb, sigReq)
	if err != nil {
		t.Fatalf("SignMessage rpc call failed: %v", err)
	}
	aliceSig := sigResp.Signature

	// bob verifying alice's signature should succeed since alice and bob are
	// connected.
	verifyReq := &lnrpc.VerifyMessageRequest{Msg: aliceMsg, Signature: aliceSig}
	verifyResp, err := net.Bob.VerifyMessage(ctxb, verifyReq)
	if err != nil {
		t.Fatalf("VerifyMessage failed: %v", err)
	}
	if !verifyResp.Valid {
		t.Fatalf("alice's signature didn't validate")
	}
	if verifyResp.Pubkey != net.Alice.PubKeyStr {
		t.Fatalf("alice's signature doesn't contain alice's pubkey.")
	}

	// carol is a new node that is unconnected to alice or bob.
	carol, err := net.NewNode(nil)
	if err != nil {
		t.Fatalf("unable to create new node: %v", err)
	}

	carolMsg := []byte("carol msg")

	// carol signs "carol msg" and sends her signature to bob.
	sigReq = &lnrpc.SignMessageRequest{Msg: carolMsg}
	sigResp, err = carol.SignMessage(ctxb, sigReq)
	if err != nil {
		t.Fatalf("SignMessage rpc call failed: %v", err)
	}
	carolSig := sigResp.Signature

	// bob verifying carol's signature should fail since they are not connected.
	verifyReq = &lnrpc.VerifyMessageRequest{Msg: carolMsg, Signature: carolSig}
	verifyResp, err = net.Bob.VerifyMessage(ctxb, verifyReq)
	if err != nil {
		t.Fatalf("VerifyMessage failed: %v", err)
	}
	if verifyResp.Valid {
		t.Fatalf("carol's signature should not be valid")
	}
	if verifyResp.Pubkey != carol.PubKeyStr {
		t.Fatalf("carol's signature doesn't contain her pubkey")
	}

	// Clean up carol's node.
	if err := net.ShutdownNode(carol); err != nil {
		t.Fatalf("unable to shutdown carol: %v", err)
	}

	// Close the channel between alice and bob.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, aliceBobCh, false)
}

// testAsyncPayments tests the performance of the async payments, and also
// checks that balances of both sides can't be become negative under stress
// payment strikes.
func testAsyncPayments(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	// As we'll be querying the channels  state frequently we'll
	// create a closure helper function for the purpose.
	getChanInfo := func(node *lntest.HarnessNode) (*lnrpc.ActiveChannel, error) {
		req := &lnrpc.ListChannelsRequest{}
		channelInfo, err := node.ListChannels(ctxb, req)
		if err != nil {
			return nil, err
		}
		if len(channelInfo.Channels) != 1 {
			t.Fatalf("node should only have a single channel, "+
				"instead he has %v",
				len(channelInfo.Channels))
		}

		return channelInfo.Channels[0], nil
	}

	const (
		timeout    = time.Duration(time.Second * 5)
		paymentAmt = 100
	)

	// First establish a channel with a capacity equals to the overall
	// amount of payments, between Alice and Bob, at the end of the test
	// Alice should send all money from her side to Bob.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPoint := openChannelAndAssert(ctxt, t, net, net.Alice, net.Bob,
		paymentAmt*2000, 0)

	info, err := getChanInfo(net.Alice)
	if err != nil {
		t.Fatalf("unable to get alice channel info: %v", err)
	}

	// Calculate the number of invoices.
	numInvoices := int(info.LocalBalance / paymentAmt)
	bobAmt := int64(numInvoices * paymentAmt)
	aliceAmt := info.LocalBalance - bobAmt

	// Send one more payment in order to cause insufficient capacity error.
	numInvoices++

	// Initialize seed random in order to generate invoices.
	prand.Seed(time.Now().UnixNano())

	// With the channel open, we'll create a invoices for Bob that Alice
	// will pay to in order to advance the state of the channel.
	bobPayReqs := make([]string, numInvoices)
	for i := 0; i < numInvoices; i++ {
		preimage := make([]byte, 32)
		_, err := rand.Read(preimage)
		if err != nil {
			t.Fatalf("unable to generate preimage: %v", err)
		}

		invoice := &lnrpc.Invoice{
			Memo:      "testing",
			RPreimage: preimage,
			Value:     paymentAmt,
		}
		resp, err := net.Bob.AddInvoice(ctxb, invoice)
		if err != nil {
			t.Fatalf("unable to add invoice: %v", err)
		}

		bobPayReqs[i] = resp.PaymentRequest
	}

	// Wait for Alice to receive the channel edge from the funding manager.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("alice didn't see the alice->bob channel before "+
			"timeout: %v", err)
	}

	// Open up a payment stream to Alice that we'll use to send payment to
	// Bob. We also create a small helper function to send payments to Bob,
	// consuming the payment hashes we generated above.
	ctxt, _ = context.WithTimeout(ctxb, time.Minute)
	alicePayStream, err := net.Alice.SendPayment(ctxt)
	if err != nil {
		t.Fatalf("unable to create payment stream for alice: %v", err)
	}

	// Send payments from Alice to Bob using of Bob's payment hashes
	// generated above.
	now := time.Now()
	for i := 0; i < numInvoices; i++ {
		sendReq := &lnrpc.SendRequest{
			PaymentRequest: bobPayReqs[i],
		}

		if err := alicePayStream.Send(sendReq); err != nil {
			t.Fatalf("unable to send payment: "+
				"stream has been closed: %v", err)
		}
	}

	// We should receive one insufficient capacity error, because we sent
	// one more payment than we can actually handle with the current
	// channel capacity.
	errorReceived := false
	for i := 0; i < numInvoices; i++ {
		if resp, err := alicePayStream.Recv(); err != nil {
			t.Fatalf("payment stream have been closed: %v", err)
		} else if resp.PaymentError != "" {
			if errorReceived {
				t.Fatalf("redundant payment error: %v",
					resp.PaymentError)
			}

			errorReceived = true
			continue
		}
	}

	if !errorReceived {
		t.Fatalf("insufficient capacity error haven't been received")
	}

	// All payments have been sent, mark the finish time.
	timeTaken := time.Since(now)

	// Next query for Bob's and Alice's channel states, in order to confirm
	// that all payment have been successful transmitted.
	aliceChan, err := getChanInfo(net.Alice)
	if len(aliceChan.PendingHtlcs) != 0 {
		t.Fatalf("alice's pending htlcs is incorrect, got %v, "+
			"expected %v", len(aliceChan.PendingHtlcs), 0)
	}
	if err != nil {
		t.Fatalf("unable to get bob's channel info: %v", err)
	}
	if aliceChan.RemoteBalance != bobAmt {
		t.Fatalf("alice's remote balance is incorrect, got %v, "+
			"expected %v", aliceChan.RemoteBalance, bobAmt)
	}
	if aliceChan.LocalBalance != aliceAmt {
		t.Fatalf("alice's local balance is incorrect, got %v, "+
			"expected %v", aliceChan.LocalBalance, aliceAmt)
	}

	// Wait for Bob to receive revocation from Alice.
	time.Sleep(2 * time.Second)

	bobChan, err := getChanInfo(net.Bob)
	if err != nil {
		t.Fatalf("unable to get bob's channel info: %v", err)
	}
	if len(bobChan.PendingHtlcs) != 0 {
		t.Fatalf("bob's pending htlcs is incorrect, got %v, "+
			"expected %v", len(bobChan.PendingHtlcs), 0)
	}
	if bobChan.LocalBalance != bobAmt {
		t.Fatalf("bob's local balance is incorrect, got %v, expected"+
			" %v", bobChan.LocalBalance, bobAmt)
	}
	if bobChan.RemoteBalance != aliceAmt {
		t.Fatalf("bob's remote balance is incorrect, got %v, "+
			"expected %v", bobChan.RemoteBalance, aliceAmt)
	}

	t.Log("\tBenchmark info: Elapsed time: ", timeTaken)
	t.Log("\tBenchmark info: TPS: ", float64(numInvoices)/float64(timeTaken.Seconds()))

	// Finally, immediately close the channel. This function will also
	// block until the channel is closed and will additionally assert the
	// relevant channel closing post conditions.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint, false)
}

// testBidirectionalAsyncPayments tests that nodes are able to send the
// payments to each other in async manner without blocking.
func testBidirectionalAsyncPayments(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	// As we'll be querying the channels  state frequently we'll
	// create a closure helper function for the purpose.
	getChanInfo := func(node *lntest.HarnessNode) (*lnrpc.ActiveChannel, error) {
		req := &lnrpc.ListChannelsRequest{}
		channelInfo, err := node.ListChannels(ctxb, req)
		if err != nil {
			return nil, err
		}
		if len(channelInfo.Channels) != 1 {
			t.Fatalf("node should only have a single channel, "+
				"instead he has %v",
				len(channelInfo.Channels))
		}

		return channelInfo.Channels[0], nil
	}

	const (
		timeout    = time.Duration(time.Second * 5)
		paymentAmt = 1000
	)

	// First establish a channel with a capacity equals to the overall
	// amount of payments, between Alice and Bob, at the end of the test
	// Alice should send all money from her side to Bob.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPoint := openChannelAndAssert(ctxt, t, net, net.Alice, net.Bob,
		paymentAmt*2000, paymentAmt*1000)

	info, err := getChanInfo(net.Alice)
	if err != nil {
		t.Fatalf("unable to get alice channel info: %v", err)
	}

	// Calculate the number of invoices.
	numInvoices := int(info.LocalBalance / paymentAmt)

	// Nodes should exchange the same amount of money and because of this
	// at the end balances should remain the same.
	aliceAmt := info.LocalBalance
	bobAmt := info.RemoteBalance

	// Initialize seed random in order to generate invoices.
	prand.Seed(time.Now().UnixNano())

	// With the channel open, we'll create a invoices for Bob that Alice
	// will pay to in order to advance the state of the channel.
	bobPayReqs := make([]string, numInvoices)
	for i := 0; i < numInvoices; i++ {
		preimage := make([]byte, 32)
		_, err := rand.Read(preimage)
		if err != nil {
			t.Fatalf("unable to generate preimage: %v", err)
		}

		invoice := &lnrpc.Invoice{
			Memo:      "testing",
			RPreimage: preimage,
			Value:     paymentAmt,
		}
		resp, err := net.Bob.AddInvoice(ctxb, invoice)
		if err != nil {
			t.Fatalf("unable to add invoice: %v", err)
		}

		bobPayReqs[i] = resp.PaymentRequest
	}

	// With the channel open, we'll create a invoices for Alice that Bob
	// will pay to in order to advance the state of the channel.
	alicePayReqs := make([]string, numInvoices)
	for i := 0; i < numInvoices; i++ {
		preimage := make([]byte, 32)
		_, err := rand.Read(preimage)
		if err != nil {
			t.Fatalf("unable to generate preimage: %v", err)
		}

		invoice := &lnrpc.Invoice{
			Memo:      "testing",
			RPreimage: preimage,
			Value:     paymentAmt,
		}
		resp, err := net.Alice.AddInvoice(ctxb, invoice)
		if err != nil {
			t.Fatalf("unable to add invoice: %v", err)
		}

		alicePayReqs[i] = resp.PaymentRequest
	}

	// Wait for Alice to receive the channel edge from the funding manager.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	if err = net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint); err != nil {
		t.Fatalf("alice didn't see the alice->bob channel before "+
			"timeout: %v", err)
	}
	if err = net.Bob.WaitForNetworkChannelOpen(ctxt, chanPoint); err != nil {
		t.Fatalf("bob didn't see the bob->alice channel before "+
			"timeout: %v", err)
	}

	// Open up a payment streams to Alice and to Bob, that we'll use to
	// send payment between nodes.
	alicePayStream, err := net.Alice.SendPayment(ctxb)
	if err != nil {
		t.Fatalf("unable to create payment stream for alice: %v", err)
	}

	bobPayStream, err := net.Bob.SendPayment(ctxb)
	if err != nil {
		t.Fatalf("unable to create payment stream for bob: %v", err)
	}

	// Send payments from Alice to Bob and from Bob to Alice in async
	// manner.
	for i := 0; i < numInvoices; i++ {
		aliceSendReq := &lnrpc.SendRequest{
			PaymentRequest: bobPayReqs[i],
		}

		bobSendReq := &lnrpc.SendRequest{
			PaymentRequest: alicePayReqs[i],
		}

		if err := alicePayStream.Send(aliceSendReq); err != nil {
			t.Fatalf("unable to send payment: "+
				"%v", err)
		}

		if err := bobPayStream.Send(bobSendReq); err != nil {
			t.Fatalf("unable to send payment: "+
				"%v", err)
		}
	}

	errChan := make(chan error)
	go func() {
		for i := 0; i < numInvoices; i++ {
			if resp, err := alicePayStream.Recv(); err != nil {
				errChan <- errors.Errorf("payment stream has"+
					" been closed: %v", err)
				return
			} else if resp.PaymentError != "" {
				errChan <- errors.Errorf("unable to send "+
					"payment from alice to bob: %v",
					resp.PaymentError)
				return
			}
		}
		errChan <- nil
	}()

	go func() {
		for i := 0; i < numInvoices; i++ {
			if resp, err := bobPayStream.Recv(); err != nil {
				errChan <- errors.Errorf("payment stream has"+
					" been closed: %v", err)
				return
			} else if resp.PaymentError != "" {
				errChan <- errors.Errorf("unable to send "+
					"payment from bob to alice: %v",
					resp.PaymentError)
				return
			}
		}
		errChan <- nil
	}()

	// Wait for Alice and Bob receive their payments, and throw and error
	// if something goes wrong.
	maxTime := 20 * time.Second
	for i := 0; i < 2; i++ {
		select {
		case err := <-errChan:
			if err != nil {
				t.Fatalf(err.Error())
			}
		case <-time.After(maxTime):
			t.Fatalf("waiting for payments to finish too long "+
				"(%v)", maxTime)
		}
	}

	// Wait for Alice and Bob to receive revocations messages, and update
	// states, i.e. balance info.
	time.Sleep(1 * time.Second)

	aliceInfo, err := getChanInfo(net.Alice)
	if err != nil {
		t.Fatalf("unable to get bob's channel info: %v", err)
	}
	if aliceInfo.RemoteBalance != bobAmt {
		t.Fatalf("alice's remote balance is incorrect, got %v, "+
			"expected %v", aliceInfo.RemoteBalance, bobAmt)
	}
	if aliceInfo.LocalBalance != aliceAmt {
		t.Fatalf("alice's local balance is incorrect, got %v, "+
			"expected %v", aliceInfo.LocalBalance, aliceAmt)
	}
	if len(aliceInfo.PendingHtlcs) != 0 {
		t.Fatalf("alice's pending htlcs is incorrect, got %v, "+
			"expected %v", len(aliceInfo.PendingHtlcs), 0)
	}

	// Next query for Bob's and Alice's channel states, in order to confirm
	// that all payment have been successful transmitted.
	bobInfo, err := getChanInfo(net.Bob)
	if err != nil {
		t.Fatalf("unable to get bob's channel info: %v", err)
	}

	if bobInfo.LocalBalance != bobAmt {
		t.Fatalf("bob's local balance is incorrect, got %v, expected"+
			" %v", bobInfo.LocalBalance, bobAmt)
	}
	if bobInfo.RemoteBalance != aliceAmt {
		t.Fatalf("bob's remote balance is incorrect, got %v, "+
			"expected %v", bobInfo.RemoteBalance, aliceAmt)
	}
	if len(bobInfo.PendingHtlcs) != 0 {
		t.Fatalf("bob's pending htlcs is incorrect, got %v, "+
			"expected %v", len(bobInfo.PendingHtlcs), 0)
	}

	// Finally, immediately close the channel. This function will also
	// block until the channel is closed and will additionally assert the
	// relevant channel closing post conditions.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint, false)
}

type testCase struct {
	name string
	test func(net *lntest.NetworkHarness, t *harnessTest)
}

var testsCases = []*testCase{
	{
		name: "basic funding flow",
		test: testBasicChannelFunding,
	},
	{
		name: "update channel policy",
		test: testUpdateChannelPolicy,
	},
	{
		name: "open channel reorg test",
		test: testOpenChannelAfterReorg,
	},
	{
		name: "disconnecting target peer",
		test: testDisconnectingTargetPeer,
	},
	{
		name: "graph topology notifications",
		test: testGraphTopologyNotifications,
	},
	{
		name: "funding flow persistence",
		test: testChannelFundingPersistence,
	},
	{
		name: "channel force closure",
		test: testChannelForceClosure,
	},
	{
		name: "channel balance",
		test: testChannelBalance,
	},
	{
		name: "single hop invoice",
		test: testSingleHopInvoice,
	},
	{
		name: "list outgoing payments",
		test: testListPayments,
	},
	{
		name: "max pending channel",
		test: testMaxPendingChannels,
	},
	{
		name: "multi-hop payments",
		test: testMultiHopPayments,
	},
	{
		name: "private channels",
		test: testPrivateChannels,
	},
	{
		name: "multiple channel creation",
		test: testBasicChannelCreation,
	},
	{
		name: "invoice update subscription",
		test: testInvoiceSubscriptions,
	},
	{
		name: "multi-hop htlc error propagation",
		test: testHtlcErrorPropagation,
	},
	// TODO(roasbeef): multi-path integration test
	{
		name: "node announcement",
		test: testNodeAnnouncement,
	},
	{
		name: "node sign verify",
		test: testNodeSignVerify,
	},
	{
		name: "async payments benchmark",
		test: testAsyncPayments,
	},
	{
		name: "async bidirectional payments",
		test: testBidirectionalAsyncPayments,
	},
	{
		// TODO(roasbeef): test always needs to be last as Bob's state
		// is borked since we trick him into attempting to cheat Alice?
		name: "revoked uncooperative close retribution",
		test: testRevokedCloseRetribution,
	},
	{
		name: "revoked uncooperative close retribution zero value remote output",
		test: testRevokedCloseRetributionZeroValueRemoteOutput,
	},
	{
		name: "revoked uncooperative close retribution remote hodl",
		test: testRevokedCloseRetributionRemoteHodl,
	},
}

// TestLightningNetworkDaemon performs a series of integration tests amongst a
// programmatically driven network of lnd nodes.
func TestLightningNetworkDaemon(t *testing.T) {
	ht := newHarnessTest(t)

	var lndHarness *lntest.NetworkHarness

	// First create an instance of the btcd's rpctest.Harness. This will be
	// used to fund the wallets of the nodes within the test network and to
	// drive blockchain related events within the network. Revert the default
	// setting of accepting non-standard transactions on simnet to reject them.
	// Transactions on the lightning network should always be standard to get
	// better guarantees of getting included in to blocks.
	args := []string{"--rejectnonstd"}
	handlers := &rpcclient.NotificationHandlers{
		OnTxAccepted: func(hash *chainhash.Hash, amt btcutil.Amount) {
			lndHarness.OnTxAccepted(hash)
		},
	}
	btcdHarness, err := rpctest.New(harnessNetParams, handlers, args)
	if err != nil {
		ht.Fatalf("unable to create mining node: %v", err)
	}
	defer btcdHarness.TearDown()

	// First create the network harness to gain access to its
	// 'OnTxAccepted' call back.
	lndHarness, err = lntest.NewNetworkHarness(btcdHarness)
	if err != nil {
		ht.Fatalf("unable to create lightning network harness: %v", err)
	}
	defer lndHarness.TearDownAll()

	// Spawn a new goroutine to watch for any fatal errors that any of the
	// running lnd processes encounter. If an error occurs, then the test
	// case should naturally as a result and we log the server error here to
	// help debug.
	go func() {
		for {
			select {
			case err, more := <-lndHarness.ProcessErrors():
				if !more {
					return
				}
				ht.Logf("lnd finished with error (stderr):\n%v", err)
			}
		}
	}()

	// Turn off the btcd rpc logging, otherwise it will lead to panic.
	// TODO(andrew.shvv|roasbeef) Remove the hack after re-work the way the log
	// rotator os work.
	rpcclient.UseLogger(btclog.Disabled)

	if err := btcdHarness.SetUp(true, 50); err != nil {
		ht.Fatalf("unable to set up mining node: %v", err)
	}
	if err := btcdHarness.Node.NotifyNewTransactions(false); err != nil {
		ht.Fatalf("unable to request transaction notifications: %v", err)
	}

	// Next mine enough blocks in order for segwit and the CSV package
	// soft-fork to activate on SimNet.
	numBlocks := chaincfg.SimNetParams.MinerConfirmationWindow * 2
	if _, err := btcdHarness.Node.Generate(numBlocks); err != nil {
		ht.Fatalf("unable to generate blocks: %v", err)
	}

	// With the btcd harness created, we can now complete the
	// initialization of the network. args - list of lnd arguments,
	// example: "--debuglevel=debug"
	// TODO(roasbeef): create master balanced channel with all the monies?
	if err = lndHarness.SetUp(nil); err != nil {
		ht.Fatalf("unable to set up test lightning network: %v", err)
	}

	t.Logf("Running %v integration tests", len(testsCases))
	for _, testCase := range testsCases {
		logLine := fmt.Sprintf("STARTING ============ %v ============\n",
			testCase.name)
		if err := lndHarness.Alice.AddToLog(logLine); err != nil {
			t.Fatalf("unable to add to log: %v", err)
		}
		if err := lndHarness.Bob.AddToLog(logLine); err != nil {
			t.Fatalf("unable to add to log: %v", err)
		}

		success := t.Run(testCase.name, func(t1 *testing.T) {
			ht := newHarnessTest(t1)
			ht.RunTestCase(testCase, lndHarness)
		})

		// Stop at the first failure. Mimic behavior of original test
		// framework.
		if !success {
			break
		}
	}
}
