package lnwallet

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

// TestCommitmentSpendValidation test the spendability of both outputs within
// the commitment transaction.
//
// The following spending cases are covered by this test:
//   * Alice's spend from the delayed output on her commitment transaction.
//   * Bob's spend from Alice's delayed output when she broadcasts a revoked
//     commitment transaction.
//   * Bob's spend from his unencumbered output within Alice's commitment
//     transaction.
func TestCommitmentSpendValidation(t *testing.T) {
	t.Parallel()

	// We generate a fake output, and the corresponding txin. This output
	// doesn't need to exist, as we'll only be validating spending from the
	// transaction that references this.
	txid, err := chainhash.NewHash(testHdSeed.CloneBytes())
	if err != nil {
		t.Fatalf("unable to create txid: %v", err)
	}
	fundingOut := &wire.OutPoint{
		Hash:  *txid,
		Index: 50,
	}
	fakeFundingTxIn := wire.NewTxIn(fundingOut, nil, nil)

	const channelBalance = btcutil.Amount(1 * 10e8)
	const csvTimeout = uint32(5)

	// We also set up set some resources for the commitment transaction.
	// Each side currently has 1 BTC within the channel, with a total
	// channel capacity of 2BTC.
	aliceKeyPriv, aliceKeyPub := btcec.PrivKeyFromBytes(btcec.S256(),
		testWalletPrivKey)
	bobKeyPriv, bobKeyPub := btcec.PrivKeyFromBytes(btcec.S256(),
		bobsPrivKey)

	revocationPreimage := testHdSeed.CloneBytes()
	commitSecret, commitPoint := btcec.PrivKeyFromBytes(btcec.S256(),
		revocationPreimage)
	revokePubKey := DeriveRevocationPubkey(bobKeyPub, commitPoint)

	aliceDelayKey := TweakPubKey(aliceKeyPub, commitPoint)
	bobPayKey := TweakPubKey(bobKeyPub, commitPoint)

	aliceCommitTweak := SingleTweakBytes(commitPoint, aliceKeyPub)
	bobCommitTweak := SingleTweakBytes(commitPoint, bobKeyPub)

	aliceSelfOutputSigner := &mockSigner{aliceKeyPriv}

	// With all the test data set up, we create the commitment transaction.
	// We only focus on a single party's transactions, as the scripts are
	// identical with the roles reversed.
	//
	// This is Alice's commitment transaction, so she must wait a CSV delay
	// of 5 blocks before sweeping the output, while bob can spend
	// immediately with either the revocation key, or his regular key.
	commitmentTx, err := CreateCommitTx(fakeFundingTxIn, aliceDelayKey,
		bobPayKey, revokePubKey, csvTimeout, channelBalance,
		channelBalance, DefaultDustLimit())
	if err != nil {
		t.Fatalf("unable to create commitment transaction: %v", nil)
	}

	delayOutput := commitmentTx.TxOut[0]
	regularOutput := commitmentTx.TxOut[1]

	// We're testing an uncooperative close, output sweep, so construct a
	// transaction which sweeps the funds to a random address.
	targetOutput, err := commitScriptUnencumbered(aliceKeyPub)
	if err != nil {
		t.Fatalf("unable to create target output: %v", err)
	}
	sweepTx := wire.NewMsgTx(2)
	sweepTx.AddTxIn(wire.NewTxIn(&wire.OutPoint{
		Hash:  commitmentTx.TxHash(),
		Index: 0,
	}, nil, nil))
	sweepTx.AddTxOut(&wire.TxOut{
		PkScript: targetOutput,
		Value:    0.5 * 10e8,
	})

	// First, we'll test spending with Alice's key after the timeout.
	delayScript, err := commitScriptToSelf(csvTimeout, aliceDelayKey,
		revokePubKey)
	if err != nil {
		t.Fatalf("unable to generate alice delay script: %v", err)
	}
	sweepTx.TxIn[0].Sequence = lockTimeToSequence(false, csvTimeout)
	signDesc := &SignDescriptor{
		WitnessScript: delayScript,
		PubKey:        aliceKeyPub,
		SingleTweak:   aliceCommitTweak,
		SigHashes:     txscript.NewTxSigHashes(sweepTx),
		Output: &wire.TxOut{
			Value: int64(channelBalance),
		},
		HashType:   txscript.SigHashAll,
		InputIndex: 0,
	}
	aliceWitnessSpend, err := CommitSpendTimeout(aliceSelfOutputSigner,
		signDesc, sweepTx)
	if err != nil {
		t.Fatalf("unable to generate delay commit spend witness: %v", err)
	}
	sweepTx.TxIn[0].Witness = aliceWitnessSpend
	vm, err := txscript.NewEngine(delayOutput.PkScript,
		sweepTx, 0, txscript.StandardVerifyFlags, nil,
		nil, int64(channelBalance))
	if err != nil {
		t.Fatalf("unable to create engine: %v", err)
	}
	if err := vm.Execute(); err != nil {
		t.Fatalf("spend from delay output is invalid: %v", err)
	}

	bobSigner := &mockSigner{bobKeyPriv}

	// Next, we'll test bob spending with the derived revocation key to
	// simulate the scenario when Alice broadcasts this commitment
	// transaction after it's been revoked.
	signDesc = &SignDescriptor{
		PubKey:        bobKeyPub,
		DoubleTweak:   commitSecret,
		WitnessScript: delayScript,
		SigHashes:     txscript.NewTxSigHashes(sweepTx),
		Output: &wire.TxOut{
			Value: int64(channelBalance),
		},
		HashType:   txscript.SigHashAll,
		InputIndex: 0,
	}
	bobWitnessSpend, err := CommitSpendRevoke(bobSigner, signDesc,
		sweepTx)
	if err != nil {
		t.Fatalf("unable to generate revocation witness: %v", err)
	}
	sweepTx.TxIn[0].Witness = bobWitnessSpend
	vm, err = txscript.NewEngine(delayOutput.PkScript,
		sweepTx, 0, txscript.StandardVerifyFlags, nil,
		nil, int64(channelBalance))
	if err != nil {
		t.Fatalf("unable to create engine: %v", err)
	}
	if err := vm.Execute(); err != nil {
		t.Fatalf("revocation spend is invalid: %v", err)
	}

	// In order to test the final scenario, we modify the TxIn of the sweep
	// transaction to instead point to to the regular output (non delay)
	// within the commitment transaction.
	sweepTx.TxIn[0] = &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  commitmentTx.TxHash(),
			Index: 1,
		},
	}

	// Finally, we test bob sweeping his output as normal in the case that
	// Alice broadcasts this commitment transaction.
	bobScriptp2wkh, err := commitScriptUnencumbered(bobPayKey)
	if err != nil {
		t.Fatalf("unable to create bob p2wkh script: %v", err)
	}
	signDesc = &SignDescriptor{
		PubKey:        bobKeyPub,
		SingleTweak:   bobCommitTweak,
		WitnessScript: bobScriptp2wkh,
		SigHashes:     txscript.NewTxSigHashes(sweepTx),
		Output: &wire.TxOut{
			Value:    int64(channelBalance),
			PkScript: bobScriptp2wkh,
		},
		HashType:   txscript.SigHashAll,
		InputIndex: 0,
	}
	bobRegularSpend, err := CommitSpendNoDelay(bobSigner, signDesc,
		sweepTx)
	if err != nil {
		t.Fatalf("unable to create bob regular spend: %v", err)
	}
	sweepTx.TxIn[0].Witness = bobRegularSpend
	vm, err = txscript.NewEngine(regularOutput.PkScript,
		sweepTx, 0, txscript.StandardVerifyFlags, nil,
		nil, int64(channelBalance))
	if err != nil {
		t.Fatalf("unable to create engine: %v", err)
	}
	if err := vm.Execute(); err != nil {
		t.Fatalf("bob p2wkh spend is invalid: %v", err)
	}
}

// TestRevocationKeyDerivation tests that given a public key, and a revocation
// hash, the homomorphic revocation public and private key derivation work
// properly.
func TestRevocationKeyDerivation(t *testing.T) {
	t.Parallel()

	// First, we'll generate a commitment point, and a commitment secret.
	// These will be used to derive the ultimate revocation keys.
	revocationPreimage := testHdSeed.CloneBytes()
	commitSecret, commitPoint := btcec.PrivKeyFromBytes(btcec.S256(),
		revocationPreimage)

	// With the commitment secrets generated, we'll now create the base
	// keys we'll use to derive the revocation key from.
	basePriv, basePub := btcec.PrivKeyFromBytes(btcec.S256(),
		testWalletPrivKey)

	// With the point and key obtained, we can now derive the revocation
	// key itself.
	revocationPub := DeriveRevocationPubkey(basePub, commitPoint)

	// The revocation public key derived from the original public key, and
	// the one derived from the private key should be identical.
	revocationPriv := DeriveRevocationPrivKey(basePriv, commitSecret)
	if !revocationPub.IsEqual(revocationPriv.PubKey()) {
		t.Fatalf("derived public keys don't match!")
	}
}

// TestTweakKeyDerivation tests that given a public key, and commitment tweak,
// then we're able to properly derive a tweaked private key that corresponds to
// the computed tweak public key. This scenario ensure that our key derivation
// for any of the non revocation keys on the commitment transaction is correct.
func TestTweakKeyDerivation(t *testing.T) {
	t.Parallel()

	// First, we'll generate a base public key that we'll be "tweaking".
	baseSecret := testHdSeed.CloneBytes()
	basePriv, basePub := btcec.PrivKeyFromBytes(btcec.S256(), baseSecret)

	// With the base key create, we'll now create a commitment point, and
	// from that derive the bytes we'll used to tweak the base public key.
	commitPoint := ComputeCommitmentPoint(bobsPrivKey)
	commitTweak := SingleTweakBytes(commitPoint, basePub)

	// Next, we'll modify the public key. When we apply the same operation
	// to the private key we should get a key that matches.
	tweakedPub := TweakPubKey(basePub, commitPoint)

	// Finally, attempt to re-generate the private key that matches the
	// tweaked public key. The derived key should match exactly.
	derivedPriv := TweakPrivKey(basePriv, commitTweak)
	if !derivedPriv.PubKey().IsEqual(tweakedPub) {
		t.Fatalf("pub keys don't match")
	}
}

// makeWitnessTestCase is a helper function used within test cases involving
// the validity of a crafted witness. This function is a wrapper function which
// allows constructing table-driven tests. In the case of an error while
// constructing the witness, the test fails fatally.
func makeWitnessTestCase(t *testing.T,
	f func() (wire.TxWitness, error)) func() wire.TxWitness {

	return func() wire.TxWitness {
		witness, err := f()
		if err != nil {
			t.Fatalf("unable to create witness test case: %v", err)
		}

		return witness
	}
}

// TestHTLCSenderSpendValidation tests all possible valid+invalid redemption
// paths in the script used within the sender's commitment transaction for an
// outgoing HTLC.
//
// The following cases are exercised by this test:
// sender script:
//  * receiver spends
//    * revoke w/ sig
//    * HTLC with invalid preimage size
//    * HTLC with valid preimage size + sig
//  * sender spends
//    * invalid lock-time for CLTV
//    * invalid sequence for CSV
//    * valid lock-time+sequence, valid sig
func TestHTLCSenderSpendValidation(t *testing.T) {
	t.Parallel()

	// We generate a fake output, and the corresponding txin. This output
	// doesn't need to exist, as we'll only be validating spending from the
	// transaction that references this.
	txid, err := chainhash.NewHash(testHdSeed.CloneBytes())
	if err != nil {
		t.Fatalf("unable to create txid: %v", err)
	}
	fundingOut := &wire.OutPoint{
		Hash:  *txid,
		Index: 50,
	}
	fakeFundingTxIn := wire.NewTxIn(fundingOut, nil, nil)

	// Next we'll the commitment secret for our commitment tx and also the
	// revocation key that we'll use as well.
	revokePreimage := testHdSeed.CloneBytes()
	commitSecret, commitPoint := btcec.PrivKeyFromBytes(btcec.S256(),
		revokePreimage)

	// Generate a payment preimage to be used below.
	paymentPreimage := revokePreimage
	paymentPreimage[0] ^= 1
	paymentHash := sha256.Sum256(paymentPreimage[:])

	// We'll also need some tests keys for alice and bob, and metadata of
	// the HTLC output.
	aliceKeyPriv, aliceKeyPub := btcec.PrivKeyFromBytes(btcec.S256(),
		testWalletPrivKey)
	bobKeyPriv, bobKeyPub := btcec.PrivKeyFromBytes(btcec.S256(),
		bobsPrivKey)
	paymentAmt := btcutil.Amount(1 * 10e8)

	aliceLocalKey := TweakPubKey(aliceKeyPub, commitPoint)
	bobLocalKey := TweakPubKey(bobKeyPub, commitPoint)

	// As we'll be modeling spends from Alice's commitment transaction,
	// we'll be using Bob's base point for the revocation key.
	revocationKey := DeriveRevocationPubkey(bobKeyPub, commitPoint)

	// Generate the raw HTLC redemption scripts, and its p2wsh counterpart.
	htlcWitnessScript, err := senderHTLCScript(aliceLocalKey, bobLocalKey,
		revocationKey, paymentHash[:])
	if err != nil {
		t.Fatalf("unable to create htlc sender script: %v", err)
	}
	htlcPkScript, err := witnessScriptHash(htlcWitnessScript)
	if err != nil {
		t.Fatalf("unable to create p2wsh htlc script: %v", err)
	}

	// This will be Alice's commitment transaction. In this scenario Alice
	// is sending an HTLC to a node she has a path to (could be Bob, could
	// be multiple hops down, it doesn't really matter).
	htlcOutput := &wire.TxOut{
		Value:    int64(paymentAmt),
		PkScript: htlcPkScript,
	}
	senderCommitTx := wire.NewMsgTx(2)
	senderCommitTx.AddTxIn(fakeFundingTxIn)
	senderCommitTx.AddTxOut(htlcOutput)

	prevOut := &wire.OutPoint{
		Hash:  senderCommitTx.TxHash(),
		Index: 0,
	}

	sweepTx := wire.NewMsgTx(2)
	sweepTx.AddTxIn(wire.NewTxIn(prevOut, nil, nil))
	sweepTx.AddTxOut(
		&wire.TxOut{
			PkScript: []byte("doesn't matter"),
			Value:    1 * 10e8,
		},
	)
	sweepTxSigHashes := txscript.NewTxSigHashes(sweepTx)

	bobCommitTweak := SingleTweakBytes(commitPoint, bobKeyPub)
	aliceCommitTweak := SingleTweakBytes(commitPoint, aliceKeyPub)

	// Finally, we'll create mock signers for both of them based on their
	// private keys. This test simplifies a bit and uses the same key as
	// the base point for all scripts and derivations.
	bobSigner := &mockSigner{bobKeyPriv}
	aliceSigner := &mockSigner{aliceKeyPriv}

	// We'll also generate a signature on the sweep transaction above
	// that'll act as Bob's signature to Alice for the second level HTLC
	// transaction.
	bobSignDesc := SignDescriptor{
		PubKey:        bobKeyPub,
		SingleTweak:   bobCommitTweak,
		WitnessScript: htlcWitnessScript,
		Output:        htlcOutput,
		HashType:      txscript.SigHashAll,
		SigHashes:     sweepTxSigHashes,
		InputIndex:    0,
	}
	bobRecvrSig, err := bobSigner.SignOutputRaw(sweepTx, &bobSignDesc)
	if err != nil {
		t.Fatalf("unable to generate alice signature: %v", err)
	}

	testCases := []struct {
		witness func() wire.TxWitness
		valid   bool
	}{
		{
			// revoke w/ sig
			// TODO(roasbeef): test invalid revoke
			makeWitnessTestCase(t, func() (wire.TxWitness, error) {
				signDesc := &SignDescriptor{
					PubKey:        bobKeyPub,
					DoubleTweak:   commitSecret,
					WitnessScript: htlcWitnessScript,
					Output:        htlcOutput,
					HashType:      txscript.SigHashAll,
					SigHashes:     sweepTxSigHashes,
					InputIndex:    0,
				}

				return senderHtlcSpendRevoke(bobSigner, signDesc,
					revocationKey, sweepTx)
			}),
			true,
		},
		{
			// HTLC with invalid preimage size
			makeWitnessTestCase(t, func() (wire.TxWitness, error) {
				signDesc := &SignDescriptor{
					PubKey:        bobKeyPub,
					SingleTweak:   bobCommitTweak,
					WitnessScript: htlcWitnessScript,
					Output:        htlcOutput,
					HashType:      txscript.SigHashAll,
					SigHashes:     sweepTxSigHashes,
					InputIndex:    0,
				}

				return senderHtlcSpendRedeem(bobSigner, signDesc,
					sweepTx,
					// Invalid preimage length
					bytes.Repeat([]byte{1}, 45))
			}),
			false,
		},
		{
			// HTLC with valid preimage size + sig
			// TODO(roabeef): invalid preimage
			makeWitnessTestCase(t, func() (wire.TxWitness, error) {
				signDesc := &SignDescriptor{
					PubKey:        bobKeyPub,
					SingleTweak:   bobCommitTweak,
					WitnessScript: htlcWitnessScript,
					Output:        htlcOutput,
					HashType:      txscript.SigHashAll,
					SigHashes:     sweepTxSigHashes,
					InputIndex:    0,
				}

				return senderHtlcSpendRedeem(bobSigner, signDesc,
					sweepTx, paymentPreimage)
			}),
			true,
		},
		{
			// valid spend to the transition the state of the HTLC
			// output with the second level HTLC timeout
			// transaction.
			makeWitnessTestCase(t, func() (wire.TxWitness, error) {
				signDesc := &SignDescriptor{
					PubKey:        aliceKeyPub,
					SingleTweak:   aliceCommitTweak,
					WitnessScript: htlcWitnessScript,
					Output:        htlcOutput,
					HashType:      txscript.SigHashAll,
					SigHashes:     sweepTxSigHashes,
					InputIndex:    0,
				}

				return senderHtlcSpendTimeout(bobRecvrSig, aliceSigner,
					signDesc, sweepTx)
			}),
			true,
		},
	}

	for i, testCase := range testCases {
		sweepTx.TxIn[0].Witness = testCase.witness()

		vm, err := txscript.NewEngine(htlcPkScript,
			sweepTx, 0, txscript.StandardVerifyFlags, nil,
			nil, int64(paymentAmt))
		if err != nil {
			t.Fatalf("unable to create engine: %v", err)
		}

		// This buffer will trace execution of the Script, only dumping
		// out to stdout in the case that a test fails.
		var debugBuf bytes.Buffer

		done := false
		for !done {
			dis, err := vm.DisasmPC()
			if err != nil {
				t.Fatalf("stepping (%v)\n", err)
			}
			debugBuf.WriteString(fmt.Sprintf("stepping %v\n", dis))

			done, err = vm.Step()
			if err != nil && testCase.valid {
				fmt.Println(debugBuf.String())
				t.Fatalf("spend test case #%v failed, spend "+
					"should be valid: %v", i, err)
			} else if err == nil && !testCase.valid && done {
				fmt.Println(debugBuf.String())
				t.Fatalf("spend test case #%v succeed, spend "+
					"should be invalid: %v", i, err)
			}

			debugBuf.WriteString(fmt.Sprintf("Stack: %v", vm.GetStack()))
			debugBuf.WriteString(fmt.Sprintf("AltStack: %v", vm.GetAltStack()))
		}
	}
}

// TestHTLCReceiverSpendValidation tests all possible valid+invalid redemption
// paths in the script used within the receiver's commitment transaction for an
// incoming HTLC.
//
// The following cases are exercised by this test:
//  * receiver spends
//     * HTLC redemption w/ invalid preimage size
//     * HTLC redemption w/ invalid sequence
//     * HTLC redemption w/ valid preimage size
//  * sender spends
//     * revoke w/ sig
//     * refund w/ invalid lock time
//     * refund w/ valid lock time
func TestHTLCReceiverSpendValidation(t *testing.T) {
	t.Parallel()

	// We generate a fake output, and the corresponding txin. This output
	// doesn't need to exist, as we'll only be validating spending from the
	// transaction that references this.
	txid, err := chainhash.NewHash(testHdSeed.CloneBytes())
	if err != nil {
		t.Fatalf("unable to create txid: %v", err)
	}
	fundingOut := &wire.OutPoint{
		Hash:  *txid,
		Index: 50,
	}
	fakeFundingTxIn := wire.NewTxIn(fundingOut, nil, nil)

	// Next we'll the commitment secret for our commitment tx and also the
	// revocation key that we'll use as well.
	revokePreimage := testHdSeed.CloneBytes()
	commitSecret, commitPoint := btcec.PrivKeyFromBytes(btcec.S256(),
		revokePreimage)

	// Generate a payment preimage to be used below.
	paymentPreimage := revokePreimage
	paymentPreimage[0] ^= 1
	paymentHash := sha256.Sum256(paymentPreimage[:])

	// We'll also need some tests keys for alice and bob, and metadata of
	// the HTLC output.
	aliceKeyPriv, aliceKeyPub := btcec.PrivKeyFromBytes(btcec.S256(),
		testWalletPrivKey)
	bobKeyPriv, bobKeyPub := btcec.PrivKeyFromBytes(btcec.S256(),
		bobsPrivKey)
	paymentAmt := btcutil.Amount(1 * 10e8)
	cltvTimeout := uint32(8)

	aliceLocalKey := TweakPubKey(aliceKeyPub, commitPoint)
	bobLocalKey := TweakPubKey(bobKeyPub, commitPoint)

	// As we'll be modeling spends from Bob's commitment transaction, we'll
	// be using Alice's base point for the revocation key.
	revocationKey := DeriveRevocationPubkey(aliceKeyPub, commitPoint)

	// Generate the raw HTLC redemption scripts, and its p2wsh counterpart.
	htlcWitnessScript, err := receiverHTLCScript(cltvTimeout, aliceLocalKey,
		bobLocalKey, revocationKey, paymentHash[:])
	if err != nil {
		t.Fatalf("unable to create htlc sender script: %v", err)
	}
	htlcPkScript, err := witnessScriptHash(htlcWitnessScript)
	if err != nil {
		t.Fatalf("unable to create p2wsh htlc script: %v", err)
	}

	// This will be Bob's commitment transaction. In this scenario Alice is
	// sending an HTLC to a node she has a path to (could be Bob, could be
	// multiple hops down, it doesn't really matter).
	htlcOutput := &wire.TxOut{
		Value:    int64(paymentAmt),
		PkScript: htlcWitnessScript,
	}

	receiverCommitTx := wire.NewMsgTx(2)
	receiverCommitTx.AddTxIn(fakeFundingTxIn)
	receiverCommitTx.AddTxOut(htlcOutput)

	prevOut := &wire.OutPoint{
		Hash:  receiverCommitTx.TxHash(),
		Index: 0,
	}

	sweepTx := wire.NewMsgTx(2)
	sweepTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: *prevOut,
	})
	sweepTx.AddTxOut(
		&wire.TxOut{
			PkScript: []byte("doesn't matter"),
			Value:    1 * 10e8,
		},
	)
	sweepTxSigHashes := txscript.NewTxSigHashes(sweepTx)

	bobCommitTweak := SingleTweakBytes(commitPoint, bobKeyPub)
	aliceCommitTweak := SingleTweakBytes(commitPoint, aliceKeyPub)

	// Finally, we'll create mock signers for both of them based on their
	// private keys. This test simplifies a bit and uses the same key as
	// the base point for all scripts and derivations.
	bobSigner := &mockSigner{bobKeyPriv}
	aliceSigner := &mockSigner{aliceKeyPriv}

	// We'll also generate a signature on the sweep transaction above
	// that'll act as Alice's signature to Bob for the second level HTLC
	// transaction.
	aliceSignDesc := SignDescriptor{
		PubKey:        aliceKeyPub,
		SingleTweak:   aliceCommitTweak,
		WitnessScript: htlcWitnessScript,
		Output:        htlcOutput,
		HashType:      txscript.SigHashAll,
		SigHashes:     sweepTxSigHashes,
		InputIndex:    0,
	}
	aliceSenderSig, err := aliceSigner.SignOutputRaw(sweepTx, &aliceSignDesc)
	if err != nil {
		t.Fatalf("unable to generate alice signature: %v", err)
	}

	// TODO(roasbeef): modify valid to check precise script errors?
	testCases := []struct {
		witness func() wire.TxWitness
		valid   bool
	}{
		{
			// HTLC redemption w/ invalid preimage size
			makeWitnessTestCase(t, func() (wire.TxWitness, error) {
				signDesc := &SignDescriptor{
					PubKey:        bobKeyPub,
					SingleTweak:   bobCommitTweak,
					WitnessScript: htlcWitnessScript,
					Output:        htlcOutput,
					HashType:      txscript.SigHashAll,
					SigHashes:     sweepTxSigHashes,
					InputIndex:    0,
				}

				return receiverHtlcSpendRedeem(aliceSenderSig,
					bytes.Repeat([]byte{1}, 45), bobSigner,
					signDesc, sweepTx)

			}),
			false,
		},
		{
			// HTLC redemption w/ valid preimage size
			makeWitnessTestCase(t, func() (wire.TxWitness, error) {
				signDesc := &SignDescriptor{
					PubKey:        bobKeyPub,
					SingleTweak:   bobCommitTweak,
					WitnessScript: htlcWitnessScript,
					Output:        htlcOutput,
					HashType:      txscript.SigHashAll,
					SigHashes:     sweepTxSigHashes,
					InputIndex:    0,
				}

				return receiverHtlcSpendRedeem(aliceSenderSig,
					paymentPreimage[:], bobSigner,
					signDesc, sweepTx)
			}),
			true,
		},
		{
			// revoke w/ sig
			makeWitnessTestCase(t, func() (wire.TxWitness, error) {
				signDesc := &SignDescriptor{
					PubKey:        aliceKeyPub,
					DoubleTweak:   commitSecret,
					WitnessScript: htlcWitnessScript,
					Output:        htlcOutput,
					HashType:      txscript.SigHashAll,
					SigHashes:     sweepTxSigHashes,
					InputIndex:    0,
				}

				return receiverHtlcSpendRevoke(aliceSigner,
					signDesc, revocationKey, sweepTx)
			}),
			true,
		},
		{
			// refund w/ invalid lock time
			makeWitnessTestCase(t, func() (wire.TxWitness, error) {
				signDesc := &SignDescriptor{
					PubKey:        aliceKeyPub,
					SingleTweak:   aliceCommitTweak,
					WitnessScript: htlcWitnessScript,
					Output:        htlcOutput,
					HashType:      txscript.SigHashAll,
					SigHashes:     sweepTxSigHashes,
					InputIndex:    0,
				}

				return receiverHtlcSpendTimeout(aliceSigner, signDesc,
					sweepTx, cltvTimeout-2)
			}),
			false,
		},
		{
			// refund w/ valid lock time
			makeWitnessTestCase(t, func() (wire.TxWitness, error) {
				signDesc := &SignDescriptor{
					PubKey:        aliceKeyPub,
					SingleTweak:   aliceCommitTweak,
					WitnessScript: htlcWitnessScript,
					Output:        htlcOutput,
					HashType:      txscript.SigHashAll,
					SigHashes:     sweepTxSigHashes,
					InputIndex:    0,
				}

				return receiverHtlcSpendTimeout(aliceSigner, signDesc,
					sweepTx, cltvTimeout)
			}),
			true,
		},
	}

	for i, testCase := range testCases {
		sweepTx.TxIn[0].Witness = testCase.witness()

		vm, err := txscript.NewEngine(htlcPkScript,
			sweepTx, 0, txscript.StandardVerifyFlags, nil,
			nil, int64(paymentAmt))
		if err != nil {
			t.Fatalf("unable to create engine: %v", err)
		}

		// This buffer will trace execution of the Script, only dumping
		// out to stdout in the case that a test fails.
		var debugBuf bytes.Buffer

		done := false
		for !done {
			dis, err := vm.DisasmPC()
			if err != nil {
				t.Fatalf("stepping (%v)\n", err)
			}
			debugBuf.WriteString(fmt.Sprintf("stepping %v\n", dis))

			done, err = vm.Step()
			if err != nil && testCase.valid {
				fmt.Println(debugBuf.String())
				t.Fatalf("spend test case #%v failed, spend should be valid: %v", i, err)
			} else if err == nil && !testCase.valid && done {
				fmt.Println(debugBuf.String())
				t.Fatalf("spend test case #%v succeed, spend should be invalid: %v", i, err)
			}

			debugBuf.WriteString(fmt.Sprintf("Stack: %v", vm.GetStack()))
			debugBuf.WriteString(fmt.Sprintf("AltStack: %v", vm.GetAltStack()))
		}
	}
}

// TestSecondLevelHtlcSpends tests all the possible redemption clauses from the
// HTLC success and timeout covenant transactions.
func TestSecondLevelHtlcSpends(t *testing.T) {
	t.Parallel()

	// We'll start be creating a creating a 2BTC HTLC.
	const htlcAmt = btcutil.Amount(2 * 10e8)

	// In all of our scenarios, the CSV timeout to claim a self output will
	// be 5 blocks.
	const claimDelay = 5

	// First we'll set up some initial key state for Alice and Bob that
	// will be used in the scripts we created below.
	aliceKeyPriv, aliceKeyPub := btcec.PrivKeyFromBytes(btcec.S256(),
		testWalletPrivKey)
	bobKeyPriv, bobKeyPub := btcec.PrivKeyFromBytes(btcec.S256(),
		bobsPrivKey)

	revokePreimage := testHdSeed.CloneBytes()
	commitSecret, commitPoint := btcec.PrivKeyFromBytes(
		btcec.S256(), revokePreimage)

	// As we're modeling this as Bob sweeping the HTLC on-chain from his
	// commitment transaction after a period of time, we'll be using a
	// revocation key derived from Alice's base point and his secret.
	revocationKey := DeriveRevocationPubkey(aliceKeyPub, commitPoint)

	// Next, craft a fake HTLC outpoint that we'll use to generate the
	// sweeping transaction using.
	txid, err := chainhash.NewHash(testHdSeed.CloneBytes())
	if err != nil {
		t.Fatalf("unable to create txid: %v", err)
	}
	htlcOutPoint := &wire.OutPoint{
		Hash:  *txid,
		Index: 0,
	}
	sweepTx := wire.NewMsgTx(2)
	sweepTx.AddTxIn(wire.NewTxIn(htlcOutPoint, nil, nil))
	sweepTx.AddTxOut(
		&wire.TxOut{
			PkScript: []byte("doesn't matter"),
			Value:    1 * 10e8,
		},
	)
	sweepTxSigHashes := txscript.NewTxSigHashes(sweepTx)

	// The delay key will be crafted using Bob's public key as the output
	// we created will be spending from Alice's commitment transaction.
	delayKey := TweakPubKey(bobKeyPub, commitPoint)

	// The commit tweak will be required in order for Bob to derive the
	// proper key need to spend the output.
	commitTweak := SingleTweakBytes(commitPoint, bobKeyPub)

	// Finally we'll generate the HTLC script itself that we'll be spending
	// from. The revocation clause can be claimed by Alice, while Bob can
	// sweep the output after a particular delay.
	htlcWitnessScript, err := secondLevelHtlcScript(revocationKey,
		delayKey, claimDelay)
	if err != nil {
		t.Fatalf("unable to create htlc script: %v", err)
	}
	htlcPkScript, err := witnessScriptHash(htlcWitnessScript)
	if err != nil {
		t.Fatalf("unable to create htlc output: %v", err)
	}

	htlcOutput := &wire.TxOut{
		PkScript: htlcPkScript,
		Value:    int64(htlcAmt),
	}

	// TODO(roasbeef): make actually use timeout/sucess txns?

	// Finally, we'll create mock signers for both of them based on their
	// private keys. This test simplifies a bit and uses the same key as
	// the base point for all scripts and derivations.
	bobSigner := &mockSigner{bobKeyPriv}
	aliceSigner := &mockSigner{aliceKeyPriv}

	testCases := []struct {
		witness func() wire.TxWitness
		valid   bool
	}{
		{
			// Sender of the HTLC attempts to activate the
			// revocation clause, but uses the wrong key (fails to
			// use the double tweak in this case).
			makeWitnessTestCase(t, func() (wire.TxWitness, error) {
				signDesc := &SignDescriptor{
					PubKey:        aliceKeyPub,
					WitnessScript: htlcWitnessScript,
					Output:        htlcOutput,
					HashType:      txscript.SigHashAll,
					SigHashes:     sweepTxSigHashes,
					InputIndex:    0,
				}

				return htlcSpendRevoke(aliceSigner, signDesc,
					sweepTx)
			}),
			false,
		},
		{
			// Sender of HTLC activates the revocation clause.
			makeWitnessTestCase(t, func() (wire.TxWitness, error) {
				signDesc := &SignDescriptor{
					PubKey:        aliceKeyPub,
					DoubleTweak:   commitSecret,
					WitnessScript: htlcWitnessScript,
					Output:        htlcOutput,
					HashType:      txscript.SigHashAll,
					SigHashes:     sweepTxSigHashes,
					InputIndex:    0,
				}

				return htlcSpendRevoke(aliceSigner, signDesc,
					sweepTx)
			}),
			true,
		},
		{
			// Receiver of the HTLC attempts to sweep, but tries to
			// do so pre-maturely with a smaller CSV delay (2
			// blocks instead of 5 blocks).
			makeWitnessTestCase(t, func() (wire.TxWitness, error) {
				signDesc := &SignDescriptor{
					PubKey:        bobKeyPub,
					SingleTweak:   commitTweak,
					WitnessScript: htlcWitnessScript,
					Output:        htlcOutput,
					HashType:      txscript.SigHashAll,
					SigHashes:     sweepTxSigHashes,
					InputIndex:    0,
				}

				return htlcSpendSuccess(bobSigner, signDesc,
					sweepTx, claimDelay-3)
			}),
			false,
		},
		{
			// Receiver of the HTLC sweeps with the proper CSV
			// delay, but uses the wrong key (leaves off the single
			// tweak).
			makeWitnessTestCase(t, func() (wire.TxWitness, error) {
				signDesc := &SignDescriptor{
					PubKey:        bobKeyPub,
					WitnessScript: htlcWitnessScript,
					Output:        htlcOutput,
					HashType:      txscript.SigHashAll,
					SigHashes:     sweepTxSigHashes,
					InputIndex:    0,
				}

				return htlcSpendSuccess(bobSigner, signDesc,
					sweepTx, claimDelay)
			}),
			false,
		},
		{
			// Receiver of the HTLC sweeps with the proper CSV
			// delay, and the correct key.
			makeWitnessTestCase(t, func() (wire.TxWitness, error) {
				signDesc := &SignDescriptor{
					PubKey:        bobKeyPub,
					SingleTweak:   commitTweak,
					WitnessScript: htlcWitnessScript,
					Output:        htlcOutput,
					HashType:      txscript.SigHashAll,
					SigHashes:     sweepTxSigHashes,
					InputIndex:    0,
				}

				return htlcSpendSuccess(bobSigner, signDesc,
					sweepTx, claimDelay)
			}),
			true,
		},
	}

	for i, testCase := range testCases {
		sweepTx.TxIn[0].Witness = testCase.witness()

		vm, err := txscript.NewEngine(htlcPkScript,
			sweepTx, 0, txscript.StandardVerifyFlags, nil,
			nil, int64(htlcAmt))
		if err != nil {
			t.Fatalf("unable to create engine: %v", err)
		}

		// This buffer will trace execution of the Script, only dumping
		// out to stdout in the case that a test fails.
		var debugBuf bytes.Buffer

		done := false
		for !done {
			dis, err := vm.DisasmPC()
			if err != nil {
				t.Fatalf("stepping (%v)\n", err)
			}
			debugBuf.WriteString(fmt.Sprintf("stepping %v\n", dis))

			done, err = vm.Step()
			if err != nil && testCase.valid {
				fmt.Println(debugBuf.String())
				t.Fatalf("spend test case #%v failed, spend "+
					"should be valid: %v", i, err)
			} else if err == nil && !testCase.valid && done {
				fmt.Println(debugBuf.String())
				t.Fatalf("spend test case #%v succeed, spend "+
					"should be invalid: %v", i, err)
			}

			debugBuf.WriteString(fmt.Sprintf("Stack: %v", vm.GetStack()))
			debugBuf.WriteString(fmt.Sprintf("AltStack: %v", vm.GetAltStack()))
		}
	}
}

func TestCommitTxStateHint(t *testing.T) {
	t.Parallel()

	stateHintTests := []struct {
		name       string
		from       uint64
		to         uint64
		inputs     int
		shouldFail bool
	}{
		{
			name:       "states 0 to 1000",
			from:       0,
			to:         1000,
			inputs:     1,
			shouldFail: false,
		},
		{
			name:       "states 'maxStateHint-1000' to 'maxStateHint'",
			from:       maxStateHint - 1000,
			to:         maxStateHint,
			inputs:     1,
			shouldFail: false,
		},
		{
			name:       "state 'maxStateHint+1'",
			from:       maxStateHint + 1,
			to:         maxStateHint + 10,
			inputs:     1,
			shouldFail: true,
		},
		{
			name:       "commit transaction with two inputs",
			inputs:     2,
			shouldFail: true,
		},
	}

	var obsfucator [StateHintSize]byte
	copy(obsfucator[:], testHdSeed[:StateHintSize])
	timeYesterday := uint32(time.Now().Unix() - 24*60*60)

	for _, test := range stateHintTests {
		commitTx := wire.NewMsgTx(2)

		// Add supplied number of inputs to the commitment transaction.
		for i := 0; i < test.inputs; i++ {
			commitTx.AddTxIn(&wire.TxIn{})
		}

		for i := test.from; i <= test.to; i++ {
			stateNum := uint64(i)

			err := SetStateNumHint(commitTx, stateNum, obsfucator)
			if err != nil && !test.shouldFail {
				t.Fatalf("unable to set state num %v: %v", i, err)
			} else if err == nil && test.shouldFail {
				t.Fatalf("Failed(%v): test should fail but did not", test.name)
			}

			locktime := commitTx.LockTime
			sequence := commitTx.TxIn[0].Sequence

			// Locktime should not be less than 500,000,000 and not larger
			// than the time 24 hours ago. One day should provide a good
			// enough buffer for the tests.
			if locktime < 5e8 || locktime > timeYesterday {
				if !test.shouldFail {
					t.Fatalf("The value of locktime (%v) may cause the commitment "+
						"transaction to be unspendable", locktime)
				}
			}

			if sequence&wire.SequenceLockTimeDisabled == 0 {
				if !test.shouldFail {
					t.Fatalf("Sequence locktime is NOT disabled when it should be")
				}
			}

			extractedStateNum := GetStateNumHint(commitTx, obsfucator)
			if extractedStateNum != stateNum && !test.shouldFail {
				t.Fatalf("state number mismatched, expected %v, got %v",
					stateNum, extractedStateNum)
			} else if extractedStateNum == stateNum && test.shouldFail {
				t.Fatalf("Failed(%v): test should fail but did not", test.name)
			}
		}
		t.Logf("Passed: %v", test.name)
	}
}
