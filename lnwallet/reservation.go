package lnwallet

import (
	"net"
	"sync"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

// ChannelContribution is the primary constituent of the funding workflow within
// lnwallet. Each side first exchanges their respective contributions along with
// channel specific parameters like the min fee/KB. Once contributions have been
// exchanged, each side will then produce signatures for all their inputs to the
// funding transactions, and finally a signature for the other party's version
// of the commitment transaction.
type ChannelContribution struct {
	// FundingOutpoint is the amount of funds contributed to the funding
	// transaction.
	FundingAmount btcutil.Amount

	// Inputs to the funding transaction.
	Inputs []*wire.TxIn

	// ChangeOutputs are the Outputs to be used in the case that the total
	// value of the funding inputs is greater than the total potential
	// channel capacity.
	ChangeOutputs []*wire.TxOut

	// MultiSigKey is the the key to be used for the funding transaction's
	// P2SH multi-sig 2-of-2 output.
	// TODO(roasbeef): replace with CDP
	MultiSigKey *btcec.PublicKey

	// CommitKey is the key to be used for this party's version of the
	// commitment transaction.
	CommitKey *btcec.PublicKey

	// DeliveryAddress is the address to be used for delivery of cleared
	// channel funds in the scenario of a cooperative channel closure.
	DeliveryAddress btcutil.Address

	// RevocationKey is the key to be used in the revocation clause for the
	// initial version of this party's commitment transaction.
	RevocationKey *btcec.PublicKey

	// DustLimit is the threshold below which no HTLC output should be
	// generated for this party. HTLCs below this amount are not
	// enforceable on-chain from this party's point of view.
	DustLimit btcutil.Amount

	// CsvDelay The delay (in blocks) to be used for the pay-to-self output
	// in this party's version of the commitment transaction.
	CsvDelay uint32
}

// InputScript represents any script inputs required to redeem a previous
// output. This struct is used rather than just a witness, or scripSig in
// order to accommodate nested p2sh which utilizes both types of input scripts.
type InputScript struct {
	Witness   [][]byte
	ScriptSig []byte
}

// ChannelReservation represents an intent to open a lightning payment channel
// a counterparty. The funding processes from reservation to channel opening
// is a 3-step process. In order to allow for full concurrency during the
// reservation workflow, resources consumed by a contribution are "locked"
// themselves. This prevents a number of race conditions such as two funding
// transactions double-spending the same input. A reservation can also be
// cancelled, which removes the resources from limbo, allowing another
// reservation to claim them.
//
// The reservation workflow consists of the following three steps:
//  1. lnwallet.InitChannelReservation
//     * One requests the wallet to allocate the necessary resources for a
//      channel reservation. These resources a put in limbo for the lifetime
//      of a reservation.
//    * Once completed the reservation will have the wallet's contribution
//      accessible via the .OurContribution() method. This contribution
//      contains the necessary items to allow the remote party to build both
//      the funding, and commitment transactions.
//  2. ChannelReservation.ProcessContribution/ChannelReservation.ProcessSingleContribution
//     * The counterparty presents their contribution to the payment channel.
//       This allows us to build the funding, and commitment transactions
//       ourselves.
//     * We're now able to sign our inputs to the funding transactions, and
//       the counterparty's version of the commitment transaction.
//     * All signatures crafted by us, are now available via .OurSignatures().
//  3. ChannelReservation.CompleteReservation/ChannelReservation.CompleteReservationSingle
//     * The final step in the workflow. The counterparty presents the
//       signatures for all their inputs to the funding transaction, as well
//       as a signature to our version of the commitment transaction.
//     * We then verify the validity of all signatures before considering the
//       channel "open".
type ChannelReservation struct {
	// This mutex MUST be held when either reading or modifying any of the
	// fields below.
	sync.RWMutex

	// fundingTx is the funding transaction for this pending channel.
	fundingTx *wire.MsgTx

	// In order of sorted inputs. Sorting is done in accordance
	// to BIP-69: https://github.com/bitcoin/bips/blob/master/bip-0069.mediawiki.
	ourFundingInputScripts   []*InputScript
	theirFundingInputScripts []*InputScript

	// Our signature for their version of the commitment transaction.
	ourCommitmentSig   []byte
	theirCommitmentSig []byte

	ourContribution   *ChannelContribution
	theirContribution *ChannelContribution

	partialState *channeldb.OpenChannel
	nodeAddr     *net.TCPAddr

	// The ID of this reservation, used to uniquely track the reservation
	// throughout its lifetime.
	reservationID uint64

	// numConfsToOpen is the number of confirmations required before the
	// channel should be considered open.
	numConfsToOpen uint16

	// pushSat the amount of satoshis that should be pushed to the
	// responder of a single funding channel as part of the initial
	// commitment state.
	pushSat btcutil.Amount

	// chanOpen houses a struct containing the channel and additional
	// confirmation details will be sent on once the channel is considered
	// 'open'. A channel is open once the funding transaction has reached a
	// sufficient number of confirmations.
	chanOpen    chan *openChanDetails
	chanOpenErr chan error

	wallet *LightningWallet
}

// NewChannelReservation creates a new channel reservation. This function is
// used only internally by lnwallet. In order to concurrent safety, the
// creation of all channel reservations should be carried out via the
// lnwallet.InitChannelReservation interface.
func NewChannelReservation(capacity, fundingAmt btcutil.Amount, minFeeRate btcutil.Amount,
	wallet *LightningWallet, id uint64, numConfs uint16,
	pushSat btcutil.Amount) *ChannelReservation {

	var (
		ourBalance   btcutil.Amount
		theirBalance btcutil.Amount
		initiator    bool
	)

	// If we're the responder to a single-funder reservation, then we have
	// no initial balance in the channel unless the remote party is pushing
	// some funds to us within the first commitment state.
	if fundingAmt == 0 {
		ourBalance = pushSat
		theirBalance = capacity - commitFee - pushSat
		initiator = false
	} else {
		// TODO(roasbeef): need to rework fee structure in general and
		// also when we "unlock" dual funder within the daemon

		if capacity == fundingAmt+commitFee {
			// If we're initiating a single funder workflow, then
			// we pay all the initial fees within the commitment
			// transaction. We also deduct our balance by the
			// amount pushed as part of the initial state.
			ourBalance = capacity - commitFee - pushSat
		} else {
			// Otherwise, this is a dual funder workflow where both
			// slides split the amount funded and the commitment
			// fee.
			ourBalance = fundingAmt - commitFee
		}

		theirBalance = capacity - fundingAmt - commitFee + pushSat
		initiator = true
	}

	// Next we'll set the channel type based on what we can ascertain about
	// the balances/push amount within the channel.
	var chanType channeldb.ChannelType

	// If either of the balances are zero at this point, or we have a
	// non-zero push amt (there's no pushing for dual funder), then this is
	// a single-funder channel.
	if ourBalance == 0 || theirBalance == 0 || pushSat != 0 {
		chanType = channeldb.SingleFunder
	} else {
		// Otherwise, this is a dual funder channel, and no side is
		// technically the "initiator"
		initiator = false
		chanType = channeldb.DualFunder
	}

	return &ChannelReservation{
		ourContribution: &ChannelContribution{
			FundingAmount: ourBalance,
		},
		theirContribution: &ChannelContribution{
			FundingAmount: theirBalance,
		},
		partialState: &channeldb.OpenChannel{
			Capacity:     capacity,
			IsInitiator:  initiator,
			IsPending:    true,
			ChanType:     chanType,
			OurBalance:   ourBalance,
			TheirBalance: theirBalance,
			MinFeePerKb:  minFeeRate,
			Db:           wallet.ChannelDB,
		},
		numConfsToOpen: numConfs,
		pushSat:        pushSat,
		reservationID:  id,
		chanOpen:       make(chan *openChanDetails, 1),
		chanOpenErr:    make(chan error, 1),
		wallet:         wallet,
	}
}

// OurContribution returns the wallet's fully populated contribution to the
// pending payment channel. See 'ChannelContribution' for further details
// regarding the contents of a contribution.
// NOTE: This SHOULD NOT be modified.
// TODO(roasbeef): make copy?
func (r *ChannelReservation) OurContribution() *ChannelContribution {
	r.RLock()
	defer r.RUnlock()
	return r.ourContribution
}

// ProcessContribution verifies the counterparty's contribution to the pending
// payment channel. As a result of this incoming message, lnwallet is able to
// build the funding transaction, and both commitment transactions. Once this
// message has been processed, all signatures to inputs to the funding
// transaction belonging to the wallet are available. Additionally, the wallet
// will generate a signature to the counterparty's version of the commitment
// transaction.
func (r *ChannelReservation) ProcessContribution(theirContribution *ChannelContribution) error {
	errChan := make(chan error, 1)

	r.wallet.msgChan <- &addContributionMsg{
		pendingFundingID: r.reservationID,
		contribution:     theirContribution,
		err:              errChan,
	}

	return <-errChan
}

// ProcessSingleContribution verifies, and records the initiator's contribution
// to this pending single funder channel. Internally, no further action is
// taken other than recording the initiator's contribution to the single funder
// channel.
func (r *ChannelReservation) ProcessSingleContribution(theirContribution *ChannelContribution) error {
	errChan := make(chan error, 1)

	r.wallet.msgChan <- &addSingleContributionMsg{
		pendingFundingID: r.reservationID,
		contribution:     theirContribution,
		err:              errChan,
	}

	return <-errChan
}

// TheirContribution returns the counterparty's pending contribution to the
// payment channel. See 'ChannelContribution' for further details regarding
// the contents of a contribution. This attribute will ONLY be available
// after a call to .ProcesContribution().
// NOTE: This SHOULD NOT be modified.
func (r *ChannelReservation) TheirContribution() *ChannelContribution {
	r.RLock()
	defer r.RUnlock()
	return r.theirContribution
}

// OurSignatures retrieves the wallet's signatures to all inputs to the funding
// transaction belonging to itself, and also a signature for the counterparty's
// version of the commitment transaction. The signatures for the wallet's
// inputs to the funding transaction are returned in sorted order according to
// BIP-69: https://github.com/bitcoin/bips/blob/master/bip-0069.mediawiki.
// NOTE: These signatures will only be populated after a call to
// .ProcesContribution()
func (r *ChannelReservation) OurSignatures() ([]*InputScript, []byte) {
	r.RLock()
	defer r.RUnlock()
	return r.ourFundingInputScripts, r.ourCommitmentSig
}

// CompleteReservation finalizes the pending channel reservation,
// transitioning from a pending payment channel, to an open payment
// channel. All passed signatures to the counterparty's inputs to the funding
// transaction will be fully verified. Signatures are expected to be passed in
// sorted order according to BIP-69:
// https://github.com/bitcoin/bips/blob/master/bip-0069.mediawiki. Additionally,
// verification is performed in order to ensure that the counterparty supplied
// a valid signature to our version of the commitment transaction.
// Once this method returns, caller's should then call .WaitForChannelOpen()
// which will block until the funding transaction obtains the configured number
// of confirmations. Once the method unblocks, a LightningChannel instance is
// returned, marking the channel available for updates.
func (r *ChannelReservation) CompleteReservation(fundingInputScripts []*InputScript,
	commitmentSig []byte) (*channeldb.OpenChannel, error) {

	// TODO(roasbeef): add flag for watch or not?
	errChan := make(chan error, 1)
	completeChan := make(chan *channeldb.OpenChannel, 1)

	r.wallet.msgChan <- &addCounterPartySigsMsg{
		pendingFundingID:         r.reservationID,
		theirFundingInputScripts: fundingInputScripts,
		theirCommitmentSig:       commitmentSig,
		completeChan:             completeChan,
		err:                      errChan,
	}

	return <-completeChan, <-errChan
}

// CompleteReservationSingle finalizes the pending single funder channel
// reservation. Using the funding outpoint of the constructed funding transaction,
// and the initiator's signature for our version of the commitment transaction,
// we are able to verify the correctness of our committment transaction as
// crafted by the initiator. Once this method returns, our signature for the
// initiator's version of the commitment transaction is available via
// the .OurSignatures() method. As this method should only be called as a
// response to a single funder channel, only a commitment signature will be
// populated.
func (r *ChannelReservation) CompleteReservationSingle(
	revocationKey *btcec.PublicKey, fundingPoint *wire.OutPoint,
	commitSig []byte, obsfucator [StateHintSize]byte) (*channeldb.OpenChannel, error) {

	errChan := make(chan error, 1)
	completeChan := make(chan *channeldb.OpenChannel, 1)

	r.wallet.msgChan <- &addSingleFunderSigsMsg{
		pendingFundingID:   r.reservationID,
		revokeKey:          revocationKey,
		fundingOutpoint:    fundingPoint,
		theirCommitmentSig: commitSig,
		obsfucator:         obsfucator,
		completeChan:       completeChan,
		err:                errChan,
	}

	return <-completeChan, <-errChan
}

// TheirSignatures returns the counterparty's signatures to all inputs to the
// funding transaction belonging to them, as well as their signature for the
// wallet's version of the commitment transaction. This methods is provided for
// additional verification, such as needed by tests.
// NOTE: These attributes will be unpopulated before a call to
// .CompleteReservation().
func (r *ChannelReservation) TheirSignatures() ([]*InputScript, []byte) {
	r.RLock()
	defer r.RUnlock()
	return r.theirFundingInputScripts, r.theirCommitmentSig
}

// FinalFundingTx returns the finalized, fully signed funding transaction for
// this reservation.
//
// NOTE: If this reservation was created as the non-initiator to a single
// funding workflow, then the full funding transaction will not be available.
// Instead we will only have the final outpoint of the funding transaction.
func (r *ChannelReservation) FinalFundingTx() *wire.MsgTx {
	r.RLock()
	defer r.RUnlock()
	return r.fundingTx
}

// FundingRedeemScript returns the fully populated funding redeem script.
//
// NOTE: This method will only return a non-nil value after either
// ProcesContribution or ProcessSingleContribution have been executed and
// returned without error.
func (r *ChannelReservation) FundingRedeemScript() []byte {
	r.RLock()
	defer r.RUnlock()
	return r.partialState.FundingWitnessScript
}

// LocalCommitTx returns the commitment transaction for the local node involved
// in this funding reservation.
func (r *ChannelReservation) LocalCommitTx() *wire.MsgTx {
	r.RLock()
	defer r.RUnlock()

	return r.partialState.OurCommitTx
}

// SetTheirDustLimit set dust limit of the remote party.
func (r *ChannelReservation) SetTheirDustLimit(dustLimit btcutil.Amount) {
	r.Lock()
	defer r.Unlock()

	r.partialState.TheirDustLimit = dustLimit
}

// FundingOutpoint returns the outpoint of the funding transaction.
//
// NOTE: The pointer returned will only be set once the .ProcesContribution()
// method is called in the case of the initiator of a single funder workflow,
// and after the .CompleteReservationSingle() method is called in the case of
// a responder to a single funder workflow.
func (r *ChannelReservation) FundingOutpoint() *wire.OutPoint {
	r.RLock()
	defer r.RUnlock()
	return r.partialState.FundingOutpoint
}

// StateNumObfuscator returns the bytes to be used to obsfucate the state
// number hints for all future states of the commitment transaction for this
// workflow.
//
// NOTE: This value will only be available for a single funder workflow after
// the CompleteReservation or CompleteReservationSingle methods have been
// successfully executed.
func (r *ChannelReservation) StateNumObfuscator() [StateHintSize]byte {
	r.RLock()
	defer r.RUnlock()
	return r.partialState.StateHintObsfucator
}

// Cancel abandons this channel reservation. This method should be called in
// the scenario that communications with the counterparty break down. Upon
// cancellation, all resources previously reserved for this pending payment
// channel are returned to the free pool, allowing subsequent reservations to
// utilize the now freed resources.
func (r *ChannelReservation) Cancel() error {
	errChan := make(chan error, 1)
	r.wallet.msgChan <- &fundingReserveCancelMsg{
		pendingFundingID: r.reservationID,
		err:              errChan,
	}

	return <-errChan
}

// OpenChannelDetails wraps the finalized fully confirmed channel which
// resulted from a ChannelReservation instance with details concerning exactly
// _where_ in the chain the channel was ultimately opened.
type OpenChannelDetails struct {
	// Channel is the active channel created by an instance of a
	// ChannelReservation and the required funding workflow.
	Channel *LightningChannel

	// ConfirmationHeight is the block height within the chain that included
	// the channel.
	ConfirmationHeight uint32

	// TransactionIndex is the index within the confirming block that the
	// transaction resides.
	TransactionIndex uint32
}
