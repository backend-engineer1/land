package lnwallet

import (
	"fmt"

	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
)

// WitnessType determines how an output's witness will be generated. The
// default commitmentTimeLock type will generate a witness that will allow
// spending of a time-locked transaction enforced by CheckSequenceVerify.
type WitnessType uint16

const (
	// Witness that allows us to spend the output of a commitment transaction
	// after a relative lock-time lockout.
	CommitmentTimeLock WitnessType = 0

	// Witness that allows us to spend a settled no-delay output immediately on
	// a counterparty's commitment transaction.
	CommitmentNoDelay WitnessType = 1

	// Witness that allows us to sweep the settled output of a malicious
	// counterparty's who broadcasts a revoked commitment transaction.
	CommitmentRevoke WitnessType = 2
)

// WitnessGenerator represents a function which is able to generate the final
// witness for a particular public key script. This function acts as an
// abstraction layer, hiding the details of the underlying script.
type WitnessGenerator func(tx *wire.MsgTx, hc *txscript.TxSigHashes,
	inputIndex int) ([][]byte, error)

// GenWitnessFunc will return a WitnessGenerator function that an output
// uses to generate the witness for a sweep transaction.
func (wt WitnessType) GenWitnessFunc(signer *Signer,
	descriptor *SignDescriptor) WitnessGenerator {

	return func(tx *wire.MsgTx, hc *txscript.TxSigHashes,
		inputIndex int) ([][]byte, error) {

		desc := descriptor
		desc.SigHashes = hc
		desc.InputIndex = inputIndex

		switch wt {
		case CommitmentTimeLock:
			return CommitSpendTimeout(*signer, desc, tx)
		case CommitmentNoDelay:
			return CommitSpendNoDelay(*signer, desc, tx)
		case CommitmentRevoke:
			return CommitSpendRevoke(*signer, desc, tx)
		default:
			return nil, fmt.Errorf("unknown witness type: %v", wt)
		}
	}

}
