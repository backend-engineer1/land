package lnwallet

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"math/big"

	"golang.org/x/crypto/hkdf"

	"github.com/btcsuite/fastsha256"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

var (
	// TODO(roasbeef): remove these and use the one's defined in txscript
	// within testnet-L.
	SequenceLockTimeSeconds      = uint32(1 << 22)
	SequenceLockTimeMask         = uint32(0x0000ffff)
	OP_CHECKSEQUENCEVERIFY  byte = txscript.OP_NOP3
)

// witnessScriptHash generates a pay-to-witness-script-hash public key script
// paying to a version 0 witness program paying to the passed redeem script.
func witnessScriptHash(redeemScript []byte) ([]byte, error) {
	bldr := txscript.NewScriptBuilder()

	bldr.AddOp(txscript.OP_0)
	scriptHash := fastsha256.Sum256(redeemScript)
	bldr.AddData(scriptHash[:])
	return bldr.Script()
}

// genMultiSigScript generates the non-p2sh'd multisig script for 2 of 2
// pubkeys.
func genMultiSigScript(aPub, bPub []byte) ([]byte, error) {
	if len(aPub) != 33 || len(bPub) != 33 {
		return nil, fmt.Errorf("Pubkey size error. Compressed pubkeys only")
	}

	// Swap to sort pubkeys if needed. Keys are sorted in lexicographical
	// order. The signatures within the scriptSig must also adhere to the
	// order, ensuring that the signatures for each public key appears
	// in the proper order on the stack.
	if bytes.Compare(aPub, bPub) == -1 {
		aPub, bPub = bPub, aPub
	}

	bldr := txscript.NewScriptBuilder()
	bldr.AddOp(txscript.OP_2)
	bldr.AddData(aPub) // Add both pubkeys (sorted).
	bldr.AddData(bPub)
	bldr.AddOp(txscript.OP_2)
	bldr.AddOp(txscript.OP_CHECKMULTISIG)
	return bldr.Script()
}

// genFundingPkScript creates a redeem script, and its matching p2wsh
// output for the funding transaction.
func genFundingPkScript(aPub, bPub []byte, amt int64) ([]byte, *wire.TxOut, error) {
	// As a sanity check, ensure that the passed amount is above zero.
	if amt <= 0 {
		return nil, nil, fmt.Errorf("can't create FundTx script with " +
			"zero, or negative coins")
	}

	// First, create the 2-of-2 multi-sig script itself.
	redeemScript, err := genMultiSigScript(aPub, bPub)
	if err != nil {
		return nil, nil, err
	}

	// With the 2-of-2 script in had, generate a p2wsh script which pays
	// to the funding script.
	pkScript, err := witnessScriptHash(redeemScript)
	if err != nil {
		return nil, nil, err
	}

	return redeemScript, wire.NewTxOut(amt, pkScript), nil
}

// spendMultiSig generates the witness stack required to redeem the 2-of-2 p2wsh
// multi-sig output.
func spendMultiSig(redeemScript, pubA, sigA, pubB, sigB []byte) [][]byte {
	witness := make([][]byte, 4)

	// When spending a p2wsh multi-sig script, rather than an OP_0, we add
	// a nil stack element to eat the extra pop.
	witness[0] = nil

	// When initially generating the redeemScript, we sorted the serialized
	// public keys in descending order. So we do a quick comparison in order
	// ensure the signatures appear on the Script Virual Machine stack in
	// the correct order.
	if bytes.Compare(pubA, pubB) == -1 {
		witness[1] = sigB
		witness[2] = sigA
	} else {
		witness[1] = sigA
		witness[2] = sigB
	}

	// Finally, add the pre-image as the last witness element.
	witness[3] = redeemScript

	return witness
}

// findScriptOutputIndex finds the index of the public key script output
// matching 'script'. Additionally, a boolean is returned indicating if
// a matching output was found at all.
// NOTE: The search stops after the first matching script is found.
func findScriptOutputIndex(tx *wire.MsgTx, script []byte) (bool, uint32) {
	found := false
	index := uint32(0)
	for i, txOut := range tx.TxOut {
		if bytes.Equal(txOut.PkScript, script) {
			found = true
			index = uint32(i)
			break
		}
	}

	return found, index
}

// senderHTLCScript constructs the public key script for an outgoing HTLC
// output payment for the sender's version of the commitment transaction:
//
// Possible Input Scripts:
//    SENDR: <sig> 0
//    RECVR: <sig> <preimage> 0 1
//    REVOK: <sig  <preimage> 1 1
//     * receiver revoke
//
// OP_IF
//     //Receiver
//     OP_IF
// 	//Revoke
// 	<revocation hash>
//     OP_ELSE
// 	//Receive
// 	OP_SIZE 32 OP_EQUALVERIFY
// 	<payment hash>
//     OP_ENDIF
//     OP_SWAP
//     OP_SHA256 OP_EQUALVERIFY
//     <recv key> OP_CHECKSIG
// OP_ELSE
//     //Sender
//     <absolute blockheight> OP_CHECKLOCKTIMEVERIFY
//     <relative blockheight> OP_CHECKSEQUENCEVERIFY
//     OP_2DROP
//     <sendr key> OP_CHECKSIG
// OP_ENDIF
func senderHTLCScript(absoluteTimeout, relativeTimeout uint32, senderKey,
	receiverKey *btcec.PublicKey, revokeHash, paymentHash []byte) ([]byte, error) {

	builder := txscript.NewScriptBuilder()

	// The receiver of the HTLC places a 1 as the first item in the witness
	// stack, forcing Script execution to enter the "if" clause within the
	// main body of the script.
	builder.AddOp(txscript.OP_IF)

	// The receiver will place a 1 as the second item of the witness stack
	// in the case the sender broadcasts a revoked commitment transaction.
	// Executing this branch allows the receiver to claim the sender's
	// funds as a result of their contract violation.
	builder.AddOp(txscript.OP_IF)
	builder.AddData(revokeHash)

	// Alternatively, the receiver can place a 0 as the second item of the
	// witness stack if they wish to claim the HTLC with the proper
	// pre-image as normal. In order to prevent an over-sized pre-image
	// attack (which can create undesirable redemption asymmerties, we
	// strongly require that all HTLC pre-images are exactly 32 bytes.
	builder.AddOp(txscript.OP_ELSE)
	builder.AddOp(txscript.OP_SIZE)
	builder.AddInt64(32)
	builder.AddOp(txscript.OP_EQUALVERIFY)
	builder.AddData(paymentHash)
	builder.AddOp(txscript.OP_ENDIF)

	builder.AddOp(txscript.OP_SWAP)

	builder.AddOp(txscript.OP_SHA256)
	builder.AddOp(txscript.OP_EQUALVERIFY)

	// In either case, we require a valid signature by the receiver.
	builder.AddData(receiverKey.SerializeCompressed())
	builder.AddOp(txscript.OP_CHECKSIG)

	// Otherwise, the sender of the HTLC will place a 0 as the first item
	// of the witness stack in order to sweep the funds back after the HTLC
	// times out.
	builder.AddOp(txscript.OP_ELSE)

	// In this case, the sender will need to wait for an absolute HTLC
	// timeout, then afterwards a relative timeout before we claim re-claim
	// the unsettled funds. This delay gives the other party a chance to
	// present the pre-image to the revocation hash in the event that the
	// sender (at this time) broadcasts this commitment transaction after
	// it has been revoked.
	builder.AddInt64(int64(absoluteTimeout))
	builder.AddOp(txscript.OP_CHECKLOCKTIMEVERIFY)
	builder.AddInt64(int64(relativeTimeout))
	builder.AddOp(OP_CHECKSEQUENCEVERIFY)
	builder.AddOp(txscript.OP_2DROP)
	builder.AddData(senderKey.SerializeCompressed())
	builder.AddOp(txscript.OP_CHECKSIG)

	builder.AddOp(txscript.OP_ENDIF)

	return builder.Script()
}

// senderHtlcSpendRevoke constructs a valid witness allowing the reciever of an
// HTLC to claim the output with knowledge of the revocation preimage in the
// scenario that the sender of the HTLC broadcasts a previously revoked
// commitment transaction. A valid spend requires knowledge of the pre-image to
// the commitment transaction's revocation hash, and a valid signature under
// the receiver's public key.
func senderHtlcSpendRevoke(commitScript []byte, outputAmt btcutil.Amount,
	reciverKey *btcec.PrivateKey, sweepTx *wire.MsgTx,
	revokePreimage []byte) (wire.TxWitness, error) {

	hashCache := txscript.NewTxSigHashes(sweepTx)
	sweepSig, err := txscript.RawTxInWitnessSignature(
		sweepTx, hashCache, 0, int64(outputAmt), commitScript,
		txscript.SigHashAll, reciverKey)
	if err != nil {
		return nil, err
	}

	// In order to force script execution to enter the revocation clause,
	// we place two one's as the first items in the final evalulated
	// witness stack.
	witnessStack := wire.TxWitness(make([][]byte, 5))
	witnessStack[0] = sweepSig
	witnessStack[1] = revokePreimage
	witnessStack[2] = []byte{1}
	witnessStack[3] = []byte{1}
	witnessStack[4] = commitScript

	return witnessStack, nil
}

// senderHtlcSpendRedeem constructs a valid witness allowing the receiver of an
// HTLC to redeem the pending output in the scenario that the sender broadcasts
// their version of the commitment transaction. A valid spend requires
// knowledge of the payment pre-image, and a valid signature under the
// receivers public key.
func senderHtlcSpendRedeem(commitScript []byte, outputAmt btcutil.Amount,
	reciverKey *btcec.PrivateKey, sweepTx *wire.MsgTx,
	paymentPreimage []byte) (wire.TxWitness, error) {

	hashCache := txscript.NewTxSigHashes(sweepTx)
	sweepSig, err := txscript.RawTxInWitnessSignature(
		sweepTx, hashCache, 0, int64(outputAmt), commitScript,
		txscript.SigHashAll, reciverKey)
	if err != nil {
		return nil, err
	}

	// We force script execution into the HTLC redemption clause by placing
	// a one, then a zero as the first items in the final evalulated
	// witness stack.
	witnessStack := wire.TxWitness(make([][]byte, 5))
	witnessStack[0] = sweepSig
	witnessStack[1] = paymentPreimage
	witnessStack[2] = []byte{0}
	witnessStack[3] = []byte{1}
	witnessStack[4] = commitScript

	return witnessStack, nil
}

// htlcSpendTimeout constructs a valid witness allowing the sender of an HTLC
// to recover the pending funds after an absolute, then relative locktime
// period.
func senderHtlcSpendTimeout(commitScript []byte, outputAmt btcutil.Amount,
	senderKey *btcec.PrivateKey, sweepTx *wire.MsgTx,
	absoluteTimeout, relativeTimeout uint32) (wire.TxWitness, error) {

	// Since the HTLC output has an absolute timeout before we're permitted
	// to sweep the output, we need to set the locktime of this sweepign
	// transaction to that aboslute value in order to pass Script
	// verification.
	sweepTx.LockTime = absoluteTimeout

	// Additionally, we're required to wait a relative period of time
	// before we can sweep the output in order to allow the other party to
	// contest our claim of validity to this version of the commitment
	// transaction.
	sweepTx.TxIn[0].Sequence = lockTimeToSequence(false, relativeTimeout)

	// Finally, OP_CSV requires that the version of the transaction
	// spending a pkscript with OP_CSV within it *must* be >= 2.
	sweepTx.Version = 2

	hashCache := txscript.NewTxSigHashes(sweepTx)
	sweepSig, err := txscript.RawTxInWitnessSignature(
		sweepTx, hashCache, 0, int64(outputAmt), commitScript,
		txscript.SigHashAll, senderKey)
	if err != nil {
		return nil, err
	}

	// We place a zero as the first item of the evaluated witness stack in
	// order to force Script execution to the HTLC timeout clause.
	witnessStack := wire.TxWitness(make([][]byte, 3))
	witnessStack[0] = sweepSig
	witnessStack[1] = []byte{0}
	witnessStack[2] = commitScript

	return witnessStack, nil
}

// receiverHTLCScript constructs the public key script for an incoming HTLC
// output payment for the receiver's version of the commitment transaction:
//
// Possible Input Scripts:
//    RECVR: <sig> <preimage> 1
//    REVOK: <sig> <preimage> 1 0
//    SENDR: <sig> 0 0
//
// OP_IF
//     //Receiver
//     OP_SIZE 32 OP_EQUALVERIFY
//     OP_SHA256
//     <payment hash> OP_EQUALVERIFY
//     <relative blockheight> OP_CHECKSEQUENCEVERIFY OP_DROP
//     <receiver key> OP_CHECKSIG
// OP_ELSE
//     //Sender
//     OP_IF
// 	//Revocation
//      OP_SHA256
// 	<revoke hash> OP_EQUALVERIFY
//     OP_ELSE
// 	//Refund
// 	<absolute blockehight> OP_CHECKLOCKTIMEVERIFY OP_DROP
//     OP_ENDIF
//     <sender key> OP_CHECKSIG
// OP_ENDIF
// TODO(roasbeef): go back to revocation keys in the HTLC outputs?
//  * also could combine pre-image with their key?
func receiverHTLCScript(absoluteTimeout, relativeTimeout uint32, senderKey,
	receiverKey *btcec.PublicKey, revokeHash, paymentHash []byte) ([]byte, error) {

	builder := txscript.NewScriptBuilder()

	// The receiver of the script will place a 1 as the first item of the
	// witness stack forcing Script execution to enter the "if" clause of
	// the main body of the script.
	builder.AddOp(txscript.OP_IF)

	// In this clause, the receiver can redeem the HTLC after a relative timeout.
	// This added delay gives the sender (at this time) an opportunity to
	// re-claim the pending HTLC in the event that the receiver
	// (at this time) broadcasts this old commitment transaction after it
	// has been revoked. Additionally, we require that the pre-image is
	// exactly 32-bytes in order to avoid undesirable redemption
	// asymmerties in the multi-hop scenario.
	builder.AddOp(txscript.OP_SIZE)
	builder.AddInt64(32)
	builder.AddOp(txscript.OP_EQUALVERIFY)
	builder.AddOp(txscript.OP_SHA256)
	builder.AddData(paymentHash)
	builder.AddOp(txscript.OP_EQUALVERIFY)
	builder.AddInt64(int64(relativeTimeout))
	builder.AddOp(OP_CHECKSEQUENCEVERIFY)
	builder.AddOp(txscript.OP_DROP)
	builder.AddData(receiverKey.SerializeCompressed())
	builder.AddOp(txscript.OP_CHECKSIG)

	// Otherwise, the sender will place a 0 as the first item of the
	// witness stack forcing exeuction to enter the "else" clause of the
	// main body of the script.
	builder.AddOp(txscript.OP_ELSE)

	// The sender will place a 1 as the second item of the witness stack
	// in the scenario that the receiver broadcasts an invalidated
	// commitment transaction, allowing the sender to sweep all the
	// receiver's funds.
	builder.AddOp(txscript.OP_IF)
	builder.AddOp(txscript.OP_SHA256)
	builder.AddData(revokeHash)
	builder.AddOp(txscript.OP_EQUALVERIFY)

	// If not, then the sender needs to wait for the HTLC timeout. This
	// clause may be executed if the receiver fails to present the r-value
	// in time. This prevents the pending funds from being locked up
	// indefinately.

	// The sender will place a 0 as the second item of the witness stack if
	// they wish to sweep the HTLC after an absolute refund timeout. This
	// time out clause prevents the pending funds from being locked up
	// indefinately.
	builder.AddOp(txscript.OP_ELSE)
	builder.AddInt64(int64(absoluteTimeout))
	builder.AddOp(txscript.OP_CHECKLOCKTIMEVERIFY)
	builder.AddOp(txscript.OP_DROP)
	builder.AddOp(txscript.OP_ENDIF)

	// In either case, we also require a valid signature with the sender's
	// commitment private key.
	builder.AddData(senderKey.SerializeCompressed())
	builder.AddOp(txscript.OP_CHECKSIG)

	builder.AddOp(txscript.OP_ENDIF)

	return builder.Script()
}

// receiverHtlcSpendRedeem constructs a valid witness allowing the receiver of
// an HTLC to redeem the conditional payment in the event that their commitment
// transaction is broadcast. Since this is a pay out to the receiving party as
// an output on their commitment transaction, a relative time delay is required
// before the output can be spent.
func receiverHtlcSpendRedeem(commitScript []byte, outputAmt btcutil.Amount,
	reciverKey *btcec.PrivateKey, sweepTx *wire.MsgTx,
	paymentPreimage []byte, relativeTimeout uint32) (wire.TxWitness, error) {

	// In order to properly spend the transaction, we need to set the
	// sequence number. We do this by convering the relative block delay
	// into a sequence number value able to be interpeted by
	// OP_CHECKSEQUENCEVERIFY.
	sweepTx.TxIn[0].Sequence = lockTimeToSequence(false, relativeTimeout)

	// Additionally, OP_CSV requires that the version of the transaction
	// spending a pkscript with OP_CSV within it *must* be >= 2.
	sweepTx.Version = 2

	hashCache := txscript.NewTxSigHashes(sweepTx)
	sweepSig, err := txscript.RawTxInWitnessSignature(
		sweepTx, hashCache, 0, int64(outputAmt), commitScript,
		txscript.SigHashAll, reciverKey)
	if err != nil {
		return nil, err
	}

	// Place a one as the first item in the evaluated witness stack to
	// force script execution to the HTLC redemption clause.
	witnessStack := wire.TxWitness(make([][]byte, 4))
	witnessStack[0] = sweepSig
	witnessStack[1] = paymentPreimage
	witnessStack[2] = []byte{1}
	witnessStack[3] = commitScript

	return witnessStack, nil
}

// receiverHtlcSpendRevoke constructs a valid witness allowing the sender of an
// HTLC within a previously revoked commitment transaction to re-claim the
// pending funds in the case that the receiver broadcasts this revoked
// commitment transaction.
func receiverHtlcSpendRevoke(commitScript []byte, outputAmt btcutil.Amount,
	senderKey *btcec.PrivateKey, sweepTx *wire.MsgTx,
	revokePreimage []byte) (wire.TxWitness, error) {

	// TODO(roasbeef): move sig generate outside func, or just factor out?
	hashCache := txscript.NewTxSigHashes(sweepTx)
	sweepSig, err := txscript.RawTxInWitnessSignature(
		sweepTx, hashCache, 0, int64(outputAmt), commitScript,
		txscript.SigHashAll, senderKey)
	if err != nil {
		return nil, err
	}

	// We place a zero, then one as the first items in the evaluated
	// witness stack in order to force script execution to the HTLC
	// revocation clause.
	witnessStack := wire.TxWitness(make([][]byte, 5))
	witnessStack[0] = sweepSig
	witnessStack[1] = revokePreimage
	witnessStack[2] = []byte{1}
	witnessStack[3] = []byte{0}
	witnessStack[4] = commitScript

	return witnessStack, nil
}

// receiverHtlcSpendTimeout constructs a valid witness allowing the sender of
// an HTLC to recover the pending funds after an absolute timeout in the
// scenario that the receiver of the HTLC broadcasts their version of the
// commitment transaction.
func receiverHtlcSpendTimeout(commitScript []byte, outputAmt btcutil.Amount,
	senderKey *btcec.PrivateKey, sweepTx *wire.MsgTx,
	absoluteTimeout uint32) (wire.TxWitness, error) {

	// The HTLC output has an absolute time period before we are permitted
	// to recover the pending funds. Therefore we need to set the locktime
	// on this sweeping transaction in order to pass Script verification.
	sweepTx.LockTime = absoluteTimeout

	hashCache := txscript.NewTxSigHashes(sweepTx)
	sweepSig, err := txscript.RawTxInWitnessSignature(
		sweepTx, hashCache, 0, int64(outputAmt), commitScript,
		txscript.SigHashAll, senderKey)
	if err != nil {
		return nil, err
	}

	witnessStack := wire.TxWitness(make([][]byte, 4))
	witnessStack[0] = sweepSig
	witnessStack[1] = []byte{0}
	witnessStack[2] = []byte{0}
	witnessStack[3] = commitScript

	return witnessStack, nil
}

// lockTimeToSequence converts the passed relative locktime to a sequence
// number in accordance to BIP-68.
// See: https://github.com/bitcoin/bips/blob/master/bip-0068.mediawiki
//  * (Compatibility)
func lockTimeToSequence(isSeconds bool, locktime uint32) uint32 {
	if !isSeconds {
		// The locktime is to be expressed in confirmations. Apply the
		// mask to restrict the number of confirmations to 65,535 or
		// 1.25 years.
		return SequenceLockTimeMask & locktime
	}

	// Set the 22nd bit which indicates the lock time is in seconds, then
	// shift the locktime over by 9 since the time granularity is in
	// 512-second intervals (2^9). This results in a max lock-time of
	// 33,554,431 seconds, or 1.06 years.
	return SequenceLockTimeSeconds | (locktime >> 9)
}

// commitScriptToSelf constructs the public key script for the output on the
// commitment transaction paying to the "owner" of said commitment transaction.
// If the other party learns of the pre-image to the revocation hash, then they
// can claim all the settled funds in the channel, plus the unsettled funds.
//
// Possible Input Scripts:
//     REVOKE:     <sig> 1
//     SENDRSWEEP: <sig> 0
//
// Output Script:
//     OP_IF
//         <revokeKey> OP_CHECKSIG
//     OP_ELSE
//         <timeKey> OP_CHECKSIGVERIFY
//         <numRelativeBlocks> OP_CHECKSEQUENCEVERIFY
//     OP_ENDIF
func commitScriptToSelf(csvTimeout uint32, selfKey, revokeKey *btcec.PublicKey) ([]byte, error) {
	// This script is spendable under two conditions: either the 'csvTimeout'
	// has passed and we can redeem our funds, or they can produce a valid
	// signature with the revocation public key. The revocation public key
	// will *only* be known to the other party if we have divulged the
	// revocation hash, allowing them to homomorphically derive the proper
	// private key which coresponds to the revoke public key.
	builder := txscript.NewScriptBuilder()

	builder.AddOp(txscript.OP_IF)

	// If a valid signature using the revocation key is presented, then
	// allow an immediate spend provided the proper signature.
	builder.AddData(revokeKey.SerializeCompressed())
	builder.AddOp(txscript.OP_CHECKSIG)

	builder.AddOp(txscript.OP_ELSE)

	// Otherwise, we can re-claim our funds after a CSV delay of
	// 'csvTimeout' timeout blocks, and a valid signature.
	builder.AddData(selfKey.SerializeCompressed())
	builder.AddOp(txscript.OP_CHECKSIGVERIFY)
	builder.AddInt64(int64(csvTimeout))
	builder.AddOp(OP_CHECKSEQUENCEVERIFY)

	builder.AddOp(txscript.OP_ENDIF)

	return builder.Script()
}

// commitScriptUnencumbered constructs the public key script on the commitment
// transaction paying to the "other" party. The constructed output is a normal
// p2wkh output spendable immediately, requiring no contestation period.
func commitScriptUnencumbered(key *btcec.PublicKey) ([]byte, error) {
	// This script goes to the "other" party, and it spendable immediately.
	builder := txscript.NewScriptBuilder()
	builder.AddOp(txscript.OP_0)
	builder.AddData(btcutil.Hash160(key.SerializeCompressed()))

	return builder.Script()
}

// commitSpendTimeout constructs a valid witness allowing the owner of a
// particular commitment transaction to spend the output returning settled
// funds back to themselves after an absolute block timeout.
func commitSpendTimeout(commitScript []byte, outputAmt btcutil.Amount,
	blockTimeout uint32, selfKey *btcec.PrivateKey,
	sweepTx *wire.MsgTx) (wire.TxWitness, error) {

	// In order to properly spend the transaction, we need to set the
	// sequence number. We do this by convering the relative block delay
	// into a sequence number value able to be interpeted by
	// OP_CHECKSEQUENCEVERIFY.
	sweepTx.TxIn[0].Sequence = lockTimeToSequence(false, blockTimeout)

	// Additionally, OP_CSV requires that the version of the transaction
	// spending a pkscript with OP_CSV within it *must* be >= 2.
	sweepTx.Version = 2

	// With the sequence number in place, we're now able to properly sign
	// off on the sweep transaction.
	hashCache := txscript.NewTxSigHashes(sweepTx)
	sweepSig, err := txscript.RawTxInWitnessSignature(
		sweepTx, hashCache, 0, int64(outputAmt), commitScript,
		txscript.SigHashAll, selfKey)
	if err != nil {
		return nil, err
	}

	// Place a zero as the first item in the evaluated witness stack to
	// force script execution to the timeout spend clause.
	witnessStack := wire.TxWitness(make([][]byte, 3))
	witnessStack[0] = sweepSig
	witnessStack[1] = []byte{0}
	witnessStack[2] = commitScript

	return witnessStack, nil
}

// commitSpendRevoke constructs a valid witness allowing a node to sweep the
// settled output of a malicious counter-party who broadcasts a revoked
// commitment trransaction.
func commitSpendRevoke(commitScript []byte, outputAmt btcutil.Amount,
	revocationPriv *btcec.PrivateKey, sweepTx *wire.MsgTx) (wire.TxWitness, error) {

	hashCache := txscript.NewTxSigHashes(sweepTx)
	sweepSig, err := txscript.RawTxInWitnessSignature(
		sweepTx, hashCache, 0, int64(outputAmt), commitScript,
		txscript.SigHashAll, revocationPriv)
	if err != nil {
		return nil, err
	}

	// Place a 1 as the first item in the evaluated witness stack to
	// force script execution to the revocation clause.
	witnessStack := wire.TxWitness(make([][]byte, 3))
	witnessStack[0] = sweepSig
	witnessStack[1] = []byte{1}
	witnessStack[2] = commitScript

	return witnessStack, nil
}

// commitSpendNoDelay constructs a valid witness allowing a node to spend their
// settled no-delay output on the counter-party's commitment transaction.
func commitSpendNoDelay(commitScript []byte, outputAmt btcutil.Amount,
	commitPriv *btcec.PrivateKey, sweepTx *wire.MsgTx) (wire.TxWitness, error) {

	// This is just a regular p2wkh spend which looks something like:
	//  * witness: <sig> <pubkey>
	hashCache := txscript.NewTxSigHashes(sweepTx)
	witness, err := txscript.WitnessScript(sweepTx, hashCache, 0,
		int64(outputAmt), commitScript, txscript.SigHashAll,
		commitPriv, true)
	if err != nil {
		return nil, err
	}

	return wire.TxWitness(witness), nil
}

// deriveRevocationPubkey derives the revocation public key given the
// counter-party's commitment key, and revocation pre-image derived via a
// pseudo-random-function. In the event that we (for some reason) broadcast a
// revoked commitment transaction, then if the other party knows the revocation
// pre-image, then they'll be able to derive the corresponding private key to
// this private key by exploting the homomorphism in the elliptic curve group:
//    * https://en.wikipedia.org/wiki/Group_homomorphism#Homomorphisms_of_abelian_groups
//
// The derivation is performed as follows:
//
//   revokeKey := commitKey + revokePoint
//             := G*k + G*h
//             := G * (k+h)
//
// Therefore, once we divulge the revocation pre-image, the remote peer is able to
// compute the proper private key for the revokeKey by computing:
//   revokePriv := commitPriv + revokePreimge mod N
//
// Where N is the order of the sub-group.
func deriveRevocationPubkey(commitPubKey *btcec.PublicKey,
	revokePreimage []byte) *btcec.PublicKey {

	// First we need to convert the revocation hash into a point on the
	// elliptic curve.
	revokePointX, revokePointY := btcec.S256().ScalarBaseMult(revokePreimage)

	// Now that we have the revocation point, we add this to their commitment
	// public key in order to obtain the revocation public key.
	revokeX, revokeY := btcec.S256().Add(commitPubKey.X, commitPubKey.Y,
		revokePointX, revokePointY)
	return &btcec.PublicKey{X: revokeX, Y: revokeY}
}

// deriveRevocationPrivKey derives the revocation private key given a node's
// commitment private key, and the pre-image to a previously seen revocation
// hash. Using this derived private key, a node is able to claim the output
// within the commitment transaction of a node in the case that they broadcast
// a previously revoked commitment transaction.
//
// The private key is derived as follwos:
//   revokePriv := commitPriv + revokePreimage mod N
//
// Where N is the order of the sub-group.
func deriveRevocationPrivKey(commitPrivKey *btcec.PrivateKey,
	revokePreimage []byte) *btcec.PrivateKey {

	// Convert the revocation pre-image into a scalar value so we can
	// manipulate it within the curve's defined finite field.
	revokeScalar := new(big.Int).SetBytes(revokePreimage)

	// To derive the revocation private key, we simply add the revocation
	// pre-image to the commitment private key.
	//
	// This works since:
	//  P = G*a + G*b
	//    = G*(a+b)
	//    = G*p
	revokePriv := revokeScalar.Add(revokeScalar, commitPrivKey.D)
	revokePriv = revokePriv.Mod(revokePriv, btcec.S256().N)

	privRevoke, _ := btcec.PrivKeyFromBytes(btcec.S256(), revokePriv.Bytes())
	return privRevoke
}

// deriveElkremRoot derives an elkrem root unique to a channel given the
// private key for our public key in the 2-of-2 multi-sig, and the remote
// node's multi-sig public key. The root is derived using the HKDF[1][2]
// instantiated with sha-256. The secret data used is our multi-sig private
// key, with the salt being the remote node's public key.
//
// [1]: https://eprint.iacr.org/2010/264.pdf
// [2]: https://tools.ietf.org/html/rfc5869
func deriveElkremRoot(localMultiSigKey *btcec.PrivateKey,
	remoteMultiSigKey *btcec.PublicKey) wire.ShaHash {

	secret := localMultiSigKey.Serialize()
	salt := remoteMultiSigKey.SerializeCompressed()
	info := []byte("elkrem")

	rootReader := hkdf.New(sha256.New, secret, salt, info)

	// It's safe to ignore the error her as we know for sure that we won't
	// be draining the HKDF past its available entropy horizon.
	// TODO(roasbeef): revisit...
	var elkremRoot wire.ShaHash
	rootReader.Read(elkremRoot[:])

	return elkremRoot
}
