package lnwire

// code derived from https://github .com/btcsuite/btcd/blob/master/wire/message.go

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

// MaxMessagePayload is the maximum bytes a message can be regardless of other
// individual limits imposed by messages themselves.
const MaxMessagePayload = 65535 // 65KB

// MessageType is the unique 2 byte big-endian integer that indicates the type
// of message on the wire. All messages have a very simple header which
// consists simply of 2-byte message type. We omit a length field, and checksum
// as the Lighting Protocol is intended to be encapsulated within a
// confidential+authenticated cryptographic messaging protocol.
type MessageType uint16

// The currently defined message types within this current version of the
// Lightning protocol.
const (
	MsgInit                      MessageType = 16
	MsgError                                 = 17
	MsgPing                                  = 18
	MsgPong                                  = 19
	MsgSingleFundingRequest                  = 32
	MsgSingleFundingResponse                 = 33
	MsgSingleFundingComplete                 = 34
	MsgSingleFundingSignComplete             = 35
	MsgFundingLocked                         = 36
	MsgShutdown                              = 39
	MsgClosingSigned                         = 40
	MsgUpdateAddHTLC                         = 128
	MsgUpdateFufillHTLC                      = 130
	MsgUpdateFailHTLC                        = 131
	MsgCommitSig                             = 132
	MsgRevokeAndAck                          = 133
	MsgChannelAnnouncement                   = 256
	MsgNodeAnnouncement                      = 257
	MsgChannelUpdate                         = 258
	MsgAnnounceSignatures                    = 259
)

// UnknownMessage is an implementation of the error interface that allows the
// creation of an error in response to an unknown message.
type UnknownMessage struct {
	messageType MessageType
}

// Error returns a human readable string describing the error.
//
// This is part of the error interface.
func (u *UnknownMessage) Error() string {
	return fmt.Sprintf("unable to parse message of unknown type: %v",
		u.messageType)
}

// Message is an interface that defines a lightning wire protocol message. The
// interface is general in order to allow implementing types full control over
// the representation of its data.
type Message interface {
	Decode(io.Reader, uint32) error
	Encode(io.Writer, uint32) error
	MsgType() MessageType
	MaxPayloadLength(uint32) uint32
}

// makeEmptyMessage creates a new empty message of the proper concrete type
// based on the passed message type.
func makeEmptyMessage(msgType MessageType) (Message, error) {
	var msg Message

	switch msgType {
	case MsgInit:
		msg = &Init{}
	case MsgSingleFundingRequest:
		msg = &SingleFundingRequest{}
	case MsgSingleFundingResponse:
		msg = &SingleFundingResponse{}
	case MsgSingleFundingComplete:
		msg = &SingleFundingComplete{}
	case MsgSingleFundingSignComplete:
		msg = &SingleFundingSignComplete{}
	case MsgFundingLocked:
		msg = &FundingLocked{}
	case MsgShutdown:
		msg = &Shutdown{}
	case MsgClosingSigned:
		msg = &ClosingSigned{}
	case MsgUpdateAddHTLC:
		msg = &UpdateAddHTLC{}
	case MsgUpdateFailHTLC:
		msg = &UpdateFailHTLC{}
	case MsgUpdateFufillHTLC:
		msg = &UpdateFufillHTLC{}
	case MsgCommitSig:
		msg = &CommitSig{}
	case MsgRevokeAndAck:
		msg = &RevokeAndAck{}
	case MsgError:
		msg = &Error{}
	case MsgChannelAnnouncement:
		msg = &ChannelAnnouncement{}
	case MsgChannelUpdate:
		msg = &ChannelUpdate{}
	case MsgNodeAnnouncement:
		msg = &NodeAnnouncement{}
	case MsgPing:
		msg = &Ping{}
	case MsgAnnounceSignatures:
		msg = &AnnounceSignatures{}
	case MsgPong:
		msg = &Pong{}
	default:
		return nil, fmt.Errorf("unknown message type [%d]", msgType)
	}

	return msg, nil
}

// WriteMessage writes a lightning Message to w including the necessary header
// information and returns the number of bytes written.
func WriteMessage(w io.Writer, msg Message, pver uint32) (int, error) {
	totalBytes := 0

	// Encode the message payload itself into a temporary buffer.
	// TODO(roasbeef): create buffer pool
	var bw bytes.Buffer
	if err := msg.Encode(&bw, pver); err != nil {
		return totalBytes, err
	}
	payload := bw.Bytes()
	lenp := len(payload)

	// Enforce maximum overall message payload.
	if lenp > MaxMessagePayload {
		return totalBytes, fmt.Errorf("message payload is too large - "+
			"encoded %d bytes, but maximum message payload is %d bytes",
			lenp, MaxMessagePayload)
	}

	// Enforce maximum message payload on the message type.
	mpl := msg.MaxPayloadLength(pver)
	if uint32(lenp) > mpl {
		return totalBytes, fmt.Errorf("message payload is too large - "+
			"encoded %d bytes, but maximum message payload of "+
			"type %x is %d bytes", lenp, msg.MsgType(), mpl)
	}

	// With the initial sanity checks complete, we'll now write out the
	// message type itself.
	var mType [2]byte
	binary.BigEndian.PutUint16(mType[:], uint16(msg.MsgType()))
	n, err := w.Write(mType[:])
	totalBytes += n
	if err != nil {
		return totalBytes, err
	}

	// With the message type written, we'll now write out the raw payload
	// itself.
	n, err = w.Write(payload)
	totalBytes += n

	return totalBytes, err
}

// ReadMessage reads, validates, and parses the next Lightning message from r
// for the provided protocol version.
func ReadMessage(r io.Reader, pver uint32) (Message, error) {
	// First, we'll read out the first two bytes of the message so we can
	// create the proper empty message.
	var mType [2]byte
	if _, err := io.ReadFull(r, mType[:]); err != nil {
		return nil, err
	}

	msgType := MessageType(binary.BigEndian.Uint16(mType[:]))

	// Now that we know the target message type, we can create the proper
	// empty message type and decode the message into it.
	msg, err := makeEmptyMessage(msgType)
	if err != nil {
		return nil, err
	}
	if err := msg.Decode(r, pver); err != nil {
		return nil, err
	}

	return msg, nil
}
