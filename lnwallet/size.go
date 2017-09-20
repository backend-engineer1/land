package lnwallet

import (
	"github.com/roasbeef/btcd/blockchain"
)

const (
	// The weight(weight), which is different from the !size! (see BIP-141),
	// is calculated as:
	// Weight = 4 * BaseSize + WitnessSize (weight).
	// BaseSize - size of the transaction without witness data (bytes).
	// WitnessSize - witness size (bytes).
	// Weight - the metric for determining the weight of the transaction.

	// P2WSHSize 34 bytes
	//	- OP_0: 1 byte
	//	- OP_DATA: 1 byte (WitnessScriptSHA256 length)
	//	- WitnessScriptSHA256: 32 bytes
	P2WSHSize = 1 + 1 + 32

	// P2WKHOutputSize 31 bytes
	//      - value: 8 bytes
	//      - var_int: 1 byte (pkscript_length)
	//      - pkscript (p2wpkh): 22 bytes
	P2WKHOutputSize = 8 + 1 + 22

	// P2WSHOutputSize 43 bytes
	//      - value: 8 bytes
	//      - var_int: 1 byte (pkscript_length)
	//      - pkscript (p2wsh): 34 bytes
	P2WSHOutputSize = 8 + 1 + 34

	// P2WPKHSize 22 bytes
	//	- OP_0: 1 byte
	//	- OP_DATA: 1 byte (PublicKeyHASH160 length)
	//	- PublicKeyHASH160: 20 bytes
	P2WPKHSize = 1 + 1 + 20

	// P2WKHWitnessSize 108 bytes
	//      - OP_DATA: 1 byte (signature length)
	//      - signature
	//      - OP_DATA: 1 byte (pubkey length)
	//      - pubkey
	P2WKHWitnessSize = 1 + 73 + 1 + 33

	// MultiSigSize 71 bytes
	//	- OP_2: 1 byte
	//	- OP_DATA: 1 byte (pubKeyAlice length)
	//	- pubKeyAlice: 33 bytes
	//	- OP_DATA: 1 byte (pubKeyBob length)
	//	- pubKeyBob: 33 bytes
	//	- OP_2: 1 byte
	//	- OP_CHECKMULTISIG: 1 byte
	MultiSigSize = 1 + 1 + 33 + 1 + 33 + 1 + 1

	// WitnessSize 222 bytes
	//	- NumberOfWitnessElements: 1 byte
	//	- NilLength: 1 byte
	//	- sigAliceLength: 1 byte
	//	- sigAlice: 73 bytes
	//	- sigBobLength: 1 byte
	//	- sigBob: 73 bytes
	//	- WitnessScriptLength: 1 byte
	//	- WitnessScript (MultiSig)
	WitnessSize = 1 + 1 + 1 + 73 + 1 + 73 + 1 + MultiSigSize

	// InputSize 41 bytes
	//	- PreviousOutPoint:
	//		- Hash: 32 bytes
	//		- Index: 4 bytes
	//	- OP_DATA: 1 byte (ScriptSigLength)
	//	- ScriptSig: 0 bytes
	//	- Witness <----	we use "Witness" instead of "ScriptSig" for
	// 			transaction validation, but "Witness" is stored
	// 			separately and weight for it size is smaller. So
	// 			we separate the calculation of ordinary data
	// 			from witness data.
	//	- Sequence: 4 bytes
	InputSize = 32 + 4 + 1 + 4

	// FundingInputSize represents the size of an input to a funding
	// transaction, and is equivalent to the size of a standard segwit input
	// as calculated above.
	FundingInputSize = InputSize

	// CommitmentDelayOutput 43 bytes
	//	- Value: 8 bytes
	//	- VarInt: 1 byte (PkScript length)
	//	- PkScript (P2WSH)
	CommitmentDelayOutput = 8 + 1 + P2WSHSize

	// CommitmentKeyHashOutput 31 bytes
	//	- Value: 8 bytes
	//	- VarInt: 1 byte (PkScript length)
	//	- PkScript (P2WPKH)
	CommitmentKeyHashOutput = 8 + 1 + P2WPKHSize

	// HTLCSize 43 bytes
	//	- Value: 8 bytes
	//	- VarInt: 1 byte (PkScript length)
	//	- PkScript (PW2SH)
	HTLCSize = 8 + 1 + P2WSHSize

	// WitnessHeaderSize 2 bytes
	//	- Flag: 1 byte
	//	- Marker: 1 byte
	WitnessHeaderSize = 1 + 1

	// BaseSweepTxSize 42 + 41 * num-swept-inputs bytes
	//	- Version: 4 bytes
	//	- WitnessHeader <---- part of the witness data
	//	- CountTxIn: 2 byte
	//	- TxIn: 41 * num-swept-inputs bytes
	//		....SweptInputs....
	//	- CountTxOut: 1 byte
	//	- TxOut: 31 bytes
	//		P2WPKHOutput: 31 bytes
	//	- LockTime: 4 bytes
	BaseSweepTxSize = 4 + 2 + 1 + P2WKHOutputSize + 4

	// BaseCommitmentTxSize 125 + 43 * num-htlc-outputs bytes
	//	- Version: 4 bytes
	//	- WitnessHeader <---- part of the witness data
	//	- CountTxIn: 1 byte
	//	- TxIn: 41 bytes
	//		FundingInput
	//	- CountTxOut: 1 byte
	//	- TxOut: 74 + 43 * num-htlc-outputs bytes
	//		OutputPayingToThem,
	//		OutputPayingToUs,
	//		....HTLCOutputs...
	//	- LockTime: 4 bytes
	BaseCommitmentTxSize = 4 + 1 + FundingInputSize + 1 +
		CommitmentDelayOutput + CommitmentKeyHashOutput + 4

	// BaseCommitmentTxWeight 500 weight
	BaseCommitmentTxWeight = blockchain.WitnessScaleFactor * BaseCommitmentTxSize

	// WitnessCommitmentTxWeight 224 weight
	WitnessCommitmentTxWeight = WitnessHeaderSize + WitnessSize

	// HTLCWeight 172 weight
	HTLCWeight = blockchain.WitnessScaleFactor * HTLCSize

	// HtlcTimeoutWeight is the weight of the HTLC timeout transaction
	// which will transition an outgoing HTLC to the delay-and-claim state.
	HtlcTimeoutWeight = 663

	// HtlcSuccessWeight is the weight of the HTLC success transaction
	// which will transition an incoming HTLC to the delay-and-claim state.
	HtlcSuccessWeight = 703

	// MaxHTLCNumber is the maximum number HTLCs which can be included in a
	// commitment transaction. This limit was chosen such that, in the case
	// of a contract breach, the punishment transaction is able to sweep
	// all the HTLC's yet still remain below the widely used standard
	// weight limits.
	MaxHTLCNumber = 966

	// ToLocalPenaltyScriptSize 83 bytes
	//      - OP_IF: 1 byte
	//              - OP_DATA: 1 byte (revocationkey length)
	//              - revocationkey: 33 bytes
	//              - OP_CHECKSIG: 1 byte
	//      - OP_ELSE: 1 byte
	//              - OP_DATA: 1 byte (localkey length)
	//              - localkey: 33 bytes
	//              - OP_CHECKSIG_VERIFY: 1 byte
	//              - OP_DATA: 1 byte (delay length)
	//              - delay: 8 bytes
	//              -OP_CHECKSEQUENCEVERIFY: 1 byte
	//      - OP_ENDIF: 1 byte
	ToLocalPenaltyScriptSize = 1 + 1 + 33 + 1 + 1 + 1 + 33 + 1 + 1 + 8 + 1 + 1

	// ToLocalPenaltyWitnessSize 160 bytes
	//      - number_of_witness_elements: 1 byte
	//      - revocation_sig_length: 1 byte
	//      - revocation_sig: 73 bytes
	//      - one_length: 1 byte
	//      - witness_script_length: 1 byte
	//      - witness_script (to_local_script)
	ToLocalPenaltyWitnessSize = 1 + 1 + 73 + 1 + 1 + ToLocalPenaltyScriptSize

	// AcceptedHtlcPenaltyScriptSize 139 bytes
	//      - OP_DUP: 1 byte
	//      - OP_HASH160: 1 byte
	//      - OP_DATA: 1 byte (RIPEMD160(SHA256(revocationkey)) length)
	//      - RIPEMD160(SHA256(revocationkey)): 20 bytes
	//      - OP_EQUAL: 1 byte
	//      - OP_IF: 1 byte
	//              - OP_CHECKSIG: 1 byte
	//      - OP_ELSE: 1 byte
	//              - OP_DATA: 1 byte (remotekey length)
	//              - remotekey: 33 bytes
	//              - OP_SWAP: 1 byte
	//              - OP_SIZE: 1 byte
	//              - 32: 1 byte
	//              - OP_EQUAL: 1 byte
	//              - OP_IF: 1 byte
	//                      - OP_HASH160: 1 byte
	//                      - OP_DATA: 1 byte (RIPEMD160(payment_hash) length)
	//                      - RIPEMD160(payment_hash): 20 bytes
	//                      - OP_EQUALVERIFY: 1 byte
	//                      - 2: 1 byte
	//                      - OP_SWAP: 1 byte
	//                      - OP_DATA: 1 byte (localkey length)
	//                      - localkey: 33 bytes
	//                      - 2: 1 byte
	//                      - OP_CHECKMULTISIG: 1 byte
	//              - OP_ELSE: 1 byte
	//                      - OP_DROP: 1 byte
	//                      - OP_DATA: 1 byte (cltv_expiry length)
	//                      - cltv_expiry: 4 bytes
	//                      - OP_CHECKLOCKTIMEVERIFY: 1 byte
	//                      - OP_DROP: 1 byte
	//                      - OP_CHECKSIG: 1 byte
	//              - OP_ENDIF: 1 byte
	//      - OP_ENDIF: 1 byte
	AcceptedHtlcPenaltyScriptSize = 3*1 + 20 + 5*1 + 33 + 7*1 + 20 + 4*1 +
		33 + 5*1 + 4 + 5*1

	// AcceptedHtlcPenaltyWitnessSize 249 bytes
	//    - number_of_witness_elements: 1 byte
	//    - revocation_sig_length: 1 byte
	//    - revocation_sig: 73 bytes
	//    - revocation_key_length: 1 byte
	//    - revocation_key: 33 bytes
	//    - witness_script_length: 1 byte
	//    - witness_script (accepted_htlc_script)
	AcceptedHtlcPenaltyWitnessSize = 1 + 1 + 73 + 1 + 33 + 1 +
		AcceptedHtlcPenaltyScriptSize

	// OfferedHtlcScriptSize 133 bytes
	//      - OP_DUP: 1 byte
	//      - OP_HASH160: 1 byte
	//      - OP_DATA: 1 byte (RIPEMD160(SHA256(revocationkey)) length)
	//      - RIPEMD160(SHA256(revocationkey)): 20 bytes
	//      - OP_EQUAL: 1 byte
	//      - OP_IF: 1 byte
	//              - OP_CHECKSIG: 1 byte
	//      - OP_ELSE: 1 byte
	//              - OP_DATA: 1 byte (remotekey length)
	//              - remotekey: 33 bytes
	//              - OP_SWAP: 1 byte
	//              - OP_SIZE: 1 byte
	//              - OP_DATA: 1 byte (32 length)
	//              - 32: 1 byte
	//              - OP_EQUAL: 1 byte
	//              - OP_NOTIF: 1 byte
	//                      - OP_DROP: 1 byte
	//                      - 2: 1 byte
	//                      - OP_SWAP: 1 byte
	//                      - OP_DATA: 1 byte (localkey length)
	//                      - localkey: 33 bytes
	//                      - 2: 1 byte
	//                      - OP_CHECKMULTISIG: 1 byte
	//              - OP_ELSE: 1 byte
	//                      - OP_HASH160: 1 byte
	//                      - OP_DATA: 1 byte (RIPEMD160(payment_hash) length)
	//                      - RIPEMD160(payment_hash): 20 bytes
	//                      - OP_EQUALVERIFY: 1 byte
	//                      - OP_CHECKSIG: 1 byte
	//              - OP_ENDIF: 1 byte
	//      - OP_ENDIF: 1 byte
	OfferedHtlcScriptSize = 3*1 + 20 + 5*1 + 33 + 10*1 + 33 + 5*1 + 20 + 4*1

	// OfferedHtlcWitnessSize 243 bytes
	//    - number_of_witness_elements: 1 byte
	//    - revocation_sig_length: 1 byte
	//    - revocation_sig: 73 bytes
	//    - revocation_key_length: 1 byte
	//    - revocation_key: 33 bytes
	//    - witness_script_length: 1 byte
	//    - witness_script (offered_htlc_script)
	OfferedHtlcWitnessSize = 1 + 1 + 73 + 1 + 33 + 1 + OfferedHtlcScriptSize

	// OfferedHtlcPenaltyWitnessSize 243 bytes
	//      - number_of_witness_elements: 1 byte
	//      - revocation_sig_length: 1 byte
	//      - revocation_sig: 73 bytes
	//      - revocation_key_length: 1 byte
	//      - revocation_key: 33 bytes
	//      - witness_script_length: 1 byte
	//      - witness_script (offered_htlc_script)
	OfferedHtlcPenaltyWitnessSize = 1 + 1 + 73 + 1 + 1 + OfferedHtlcScriptSize
)

// estimateCommitTxWeight estimate commitment transaction weight depending on
// the precalculated weight of base transaction, witness data, which is needed
// for paying for funding tx, and htlc weight multiplied by their count.
func estimateCommitTxWeight(count int, prediction bool) int64 {
	// Make prediction about the size of commitment transaction with
	// additional HTLC.
	if prediction {
		count++
	}

	htlcWeight := int64(count * HTLCWeight)
	baseWeight := int64(BaseCommitmentTxWeight)
	witnessWeight := int64(WitnessCommitmentTxWeight)

	return htlcWeight + baseWeight + witnessWeight
}
