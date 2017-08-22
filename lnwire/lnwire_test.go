package lnwire

import (
	"bytes"
	"encoding/hex"
	"math"
	"math/big"
	"math/rand"
	"net"
	"reflect"
	"testing"
	"testing/quick"

	"github.com/davecgh/go-spew/spew"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

var (
	revHash = [32]byte{
		0xb7, 0x94, 0x38, 0x5f, 0x2d, 0x1e, 0xf7, 0xab,
		0x4d, 0x92, 0x73, 0xd1, 0x90, 0x63, 0x81, 0xb4,
		0x4f, 0x2f, 0x6f, 0x25, 0x88, 0xa3, 0xef, 0xb9,
		0x6a, 0x49, 0x18, 0x83, 0x31, 0x98, 0x47, 0x53,
	}

	shaHash1Bytes, _ = hex.DecodeString("e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")
	shaHash1, _      = chainhash.NewHash(shaHash1Bytes)
	outpoint1        = wire.NewOutPoint(shaHash1, 0)
	testSig          = &btcec.Signature{
		R: new(big.Int),
		S: new(big.Int),
	}
	_, _ = testSig.R.SetString("63724406601629180062774974542967536251589935445068131219452686511677818569431", 10)
	_, _ = testSig.S.SetString("18801056069249825825291287104931333862866033135609736119018462340006816851118", 10)

	// TODO(roasbeef): randomly generate from three types of addrs
	a1        = &net.TCPAddr{IP: (net.IP)([]byte{0x7f, 0x0, 0x0, 0x1}), Port: 8333}
	a2, _     = net.ResolveTCPAddr("tcp", "[2001:db8:85a3:0:0:8a2e:370:7334]:80")
	testAddrs = []net.Addr{a1, a2}
)

func randPubKey() (*btcec.PublicKey, error) {
	priv, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		return nil, err
	}

	return priv.PubKey(), nil
}

func randFeatureVector(r *rand.Rand) *FeatureVector {
	numFeatures := r.Int31n(10000)
	features := make([]Feature, numFeatures)
	for i := int32(0); i < numFeatures; i++ {
		features[i] = Feature{
			Flag: featureFlag(rand.Int31n(2) + 1),
		}
	}

	return NewFeatureVector(features)
}

func TestMaxOutPointIndex(t *testing.T) {
	t.Parallel()

	op := wire.OutPoint{
		Index: math.MaxUint32,
	}

	var b bytes.Buffer
	if err := writeElement(&b, op); err == nil {
		t.Fatalf("write of outPoint should fail, index exceeds 16-bits")
	}
}

func TestEmptyMessageUnknownType(t *testing.T) {
	t.Parallel()

	fakeType := MessageType(math.MaxUint16)
	if _, err := makeEmptyMessage(fakeType); err == nil {
		t.Fatalf("should not be able to make an empty message of an " +
			"unknown type")
	}
}

// TestLightningWireProtocol uses the testing/quick package to create a series
// of fuzz tests to attempt to break a primary scenario which is implemented as
// property based testing scenario.
func TestLightningWireProtocol(t *testing.T) {
	t.Parallel()

	// mainScenario is the primary test that will programmatically be
	// executed for all registered wire messages. The quick-checker within
	// testing/quick will attempt to find an input to this function, s.t
	// the function returns false, if so then we've found an input that
	// violates our model of the system.
	mainScenario := func(msg Message) bool {
		// Give a new message, we'll serialize the message into a new
		// bytes buffer.
		var b bytes.Buffer
		if _, err := WriteMessage(&b, msg, 0); err != nil {
			t.Fatalf("unable to write msg: %v", err)
			return false
		}

		// Next, we'll ensure that the serialized payload (subtracting
		// the 2 bytes for the message type) is _below_ the specified
		// max payload size for this message.
		payloadLen := uint32(b.Len()) - 2
		if payloadLen > msg.MaxPayloadLength(0) {
			t.Fatalf("msg payload constraint violated: %v > %v",
				payloadLen, msg.MaxPayloadLength(0))
			return false
		}

		// Finally, we'll deserialize the message from the written
		// buffer, and finally assert that the messages are equal.
		newMsg, err := ReadMessage(&b, 0)
		if err != nil {
			t.Fatalf("unable to read msg: %v", err)
			return false
		}
		if !reflect.DeepEqual(msg, newMsg) {
			t.Fatalf("messages don't match after re-encoding: %v "+
				"vs %v", spew.Sdump(msg), spew.Sdump(newMsg))
			return false
		}

		return true
	}

	// customTypeGen is a map of functions that are able to randomly
	// generate a given type. These functions are needed for types which
	// are too complex for the testing/quick package to automatically
	// generate.
	customTypeGen := map[MessageType]func([]reflect.Value, *rand.Rand){
		MsgInit: func(v []reflect.Value, r *rand.Rand) {
			req := NewInitMessage(
				randFeatureVector(r),
				randFeatureVector(r),
			)
			req.GlobalFeatures.featuresMap = nil
			req.LocalFeatures.featuresMap = nil

			v[0] = reflect.ValueOf(*req)
		},
		MsgOpenChannel: func(v []reflect.Value, r *rand.Rand) {
			req := OpenChannel{
				FundingAmount:    btcutil.Amount(r.Int63()),
				PushAmount:       MilliSatoshi(r.Int63()),
				DustLimit:        btcutil.Amount(r.Int63()),
				MaxValueInFlight: MilliSatoshi(r.Int63()),
				ChannelReserve:   btcutil.Amount(r.Int63()),
				HtlcMinimum:      MilliSatoshi(r.Int31()),
				FeePerKiloWeight: uint32(r.Int63()),
				CsvDelay:         uint16(r.Int31()),
				MaxAcceptedHTLCs: uint16(r.Int31()),
				ChannelFlags:     byte(r.Int31()),
			}

			if _, err := r.Read(req.ChainHash[:]); err != nil {
				t.Fatalf("unable to generate chain hash: %v", err)
				return
			}

			if _, err := r.Read(req.PendingChannelID[:]); err != nil {
				t.Fatalf("unable to generate pending chan id: %v", err)
				return
			}

			var err error
			req.FundingKey, err = randPubKey()
			if err != nil {
				t.Fatalf("unable to generate key: %v", err)
				return
			}
			req.RevocationPoint, err = randPubKey()
			if err != nil {
				t.Fatalf("unable to generate key: %v", err)
				return
			}
			req.PaymentPoint, err = randPubKey()
			if err != nil {
				t.Fatalf("unable to generate key: %v", err)
				return
			}
			req.DelayedPaymentPoint, err = randPubKey()
			if err != nil {
				t.Fatalf("unable to generate key: %v", err)
				return
			}
			req.FirstCommitmentPoint, err = randPubKey()
			if err != nil {
				t.Fatalf("unable to generate key: %v", err)
				return
			}

			v[0] = reflect.ValueOf(req)
		},
		MsgAcceptChannel: func(v []reflect.Value, r *rand.Rand) {
			req := AcceptChannel{
				DustLimit:        btcutil.Amount(r.Int63()),
				MaxValueInFlight: MilliSatoshi(r.Int63()),
				ChannelReserve:   btcutil.Amount(r.Int63()),
				MinAcceptDepth:   uint32(r.Int31()),
				HtlcMinimum:      MilliSatoshi(r.Int31()),
				CsvDelay:         uint16(r.Int31()),
				MaxAcceptedHTLCs: uint16(r.Int31()),
			}

			if _, err := r.Read(req.PendingChannelID[:]); err != nil {
				t.Fatalf("unable to generate pending chan id: %v", err)
				return
			}

			var err error
			req.FundingKey, err = randPubKey()
			if err != nil {
				t.Fatalf("unable to generate key: %v", err)
				return
			}
			req.RevocationPoint, err = randPubKey()
			if err != nil {
				t.Fatalf("unable to generate key: %v", err)
				return
			}
			req.PaymentPoint, err = randPubKey()
			if err != nil {
				t.Fatalf("unable to generate key: %v", err)
				return
			}
			req.DelayedPaymentPoint, err = randPubKey()
			if err != nil {
				t.Fatalf("unable to generate key: %v", err)
				return
			}
			req.FirstCommitmentPoint, err = randPubKey()
			if err != nil {
				t.Fatalf("unable to generate key: %v", err)
				return
			}

			v[0] = reflect.ValueOf(req)
		},
		MsgFundingCreated: func(v []reflect.Value, r *rand.Rand) {
			req := FundingCreated{}

			if _, err := r.Read(req.PendingChannelID[:]); err != nil {
				t.Fatalf("unable to generate pending chan id: %v", err)
				return
			}

			if _, err := r.Read(req.FundingPoint.Hash[:]); err != nil {
				t.Fatalf("unable to generate hash: %v", err)
				return
			}
			req.FundingPoint.Index = uint32(r.Int31()) % math.MaxUint16

			req.CommitSig = testSig

			v[0] = reflect.ValueOf(req)
		},
		MsgFundingSigned: func(v []reflect.Value, r *rand.Rand) {
			var c [32]byte
			if _, err := r.Read(c[:]); err != nil {
				t.Fatalf("unable to generate chan id: %v", err)
				return
			}

			req := FundingSigned{
				ChanID:    ChannelID(c),
				CommitSig: testSig,
			}

			v[0] = reflect.ValueOf(req)
		},
		MsgFundingLocked: func(v []reflect.Value, r *rand.Rand) {

			var c [32]byte
			if _, err := r.Read(c[:]); err != nil {
				t.Fatalf("unable to generate chan id: %v", err)
				return
			}

			pubKey, err := randPubKey()
			if err != nil {
				t.Fatalf("unable to generate key: %v", err)
				return
			}

			req := NewFundingLocked(ChannelID(c), pubKey)

			v[0] = reflect.ValueOf(*req)
		},
		MsgClosingSigned: func(v []reflect.Value, r *rand.Rand) {
			req := ClosingSigned{
				FeeSatoshis: uint64(r.Int63()),
				Signature:   testSig,
			}

			if _, err := r.Read(req.ChannelID[:]); err != nil {
				t.Fatalf("unable to generate chan id: %v", err)
				return
			}

			v[0] = reflect.ValueOf(req)
		},
		MsgCommitSig: func(v []reflect.Value, r *rand.Rand) {
			req := NewCommitSig()
			if _, err := r.Read(req.ChanID[:]); err != nil {
				t.Fatalf("unable to generate chan id: %v", err)
				return
			}
			req.CommitSig = testSig

			numSigs := uint16(r.Int31n(1020))
			req.HtlcSigs = make([]*btcec.Signature, numSigs)
			for i := 0; i < int(numSigs); i++ {
				req.HtlcSigs[i] = testSig
			}

			v[0] = reflect.ValueOf(*req)
		},
		MsgRevokeAndAck: func(v []reflect.Value, r *rand.Rand) {
			req := NewRevokeAndAck()
			if _, err := r.Read(req.ChanID[:]); err != nil {
				t.Fatalf("unable to generate chan id: %v", err)
				return
			}
			if _, err := r.Read(req.Revocation[:]); err != nil {
				t.Fatalf("unable to generate bytes: %v", err)
				return
			}
			var err error
			req.NextRevocationKey, err = randPubKey()
			if err != nil {
				t.Fatalf("unable to generate key: %v", err)
				return
			}

			v[0] = reflect.ValueOf(*req)
		},
		MsgChannelAnnouncement: func(v []reflect.Value, r *rand.Rand) {
			req := ChannelAnnouncement{
				ShortChannelID: NewShortChanIDFromInt(uint64(r.Int63())),
				Features:       randFeatureVector(r),
			}
			req.Features.featuresMap = nil
			req.NodeSig1 = testSig
			req.NodeSig2 = testSig
			req.BitcoinSig1 = testSig
			req.BitcoinSig2 = testSig

			var err error
			req.NodeID1, err = randPubKey()
			if err != nil {
				t.Fatalf("unable to generate key: %v", err)
				return
			}
			req.NodeID2, err = randPubKey()
			if err != nil {
				t.Fatalf("unable to generate key: %v", err)
				return
			}
			req.BitcoinKey1, err = randPubKey()
			if err != nil {
				t.Fatalf("unable to generate key: %v", err)
				return
			}
			req.BitcoinKey2, err = randPubKey()
			if err != nil {
				t.Fatalf("unable to generate key: %v", err)
				return
			}
			if _, err := r.Read(req.ChainHash[:]); err != nil {
				t.Fatalf("unable to generate chain hash: %v", err)
				return
			}

			v[0] = reflect.ValueOf(req)
		},
		MsgNodeAnnouncement: func(v []reflect.Value, r *rand.Rand) {
			var a [32]byte
			if _, err := r.Read(a[:]); err != nil {
				t.Fatalf("unable to generate alias: %v", err)
				return
			}

			req := NodeAnnouncement{
				Signature: testSig,
				Features:  randFeatureVector(r),
				Timestamp: uint32(r.Int31()),
				Alias:     a,
				RGBColor: RGB{
					red:   uint8(r.Int31()),
					green: uint8(r.Int31()),
					blue:  uint8(r.Int31()),
				},
				Addresses: testAddrs,
			}
			req.Features.featuresMap = nil

			var err error
			req.NodeID, err = randPubKey()
			if err != nil {
				t.Fatalf("unable to generate key: %v", err)
				return
			}

			v[0] = reflect.ValueOf(req)
		},
		MsgChannelUpdate: func(v []reflect.Value, r *rand.Rand) {
			req := ChannelUpdate{
				Signature:       testSig,
				ShortChannelID:  NewShortChanIDFromInt(uint64(r.Int63())),
				Timestamp:       uint32(r.Int31()),
				Flags:           uint16(r.Int31()),
				TimeLockDelta:   uint16(r.Int31()),
				HtlcMinimumMsat: MilliSatoshi(r.Int63()),
				BaseFee:         uint32(r.Int31()),
				FeeRate:         uint32(r.Int31()),
			}
			if _, err := r.Read(req.ChainHash[:]); err != nil {
				t.Fatalf("unable to generate chain hash: %v", err)
				return
			}

			v[0] = reflect.ValueOf(req)
		},
		MsgAnnounceSignatures: func(v []reflect.Value, r *rand.Rand) {
			req := AnnounceSignatures{
				ShortChannelID:   NewShortChanIDFromInt(uint64(r.Int63())),
				NodeSignature:    testSig,
				BitcoinSignature: testSig,
			}
			if _, err := r.Read(req.ChannelID[:]); err != nil {
				t.Fatalf("unable to generate chan id: %v", err)
				return
			}

			v[0] = reflect.ValueOf(req)
		},
	}

	// With the above types defined, we'll now generate a slice of
	// scenarios to feed into quick.Check. The function scans in input
	// space of the target function under test, so we'll need to create a
	// series of wrapper functions to force it to iterate over the target
	// types, but re-use the mainScenario defined above.
	tests := []struct {
		msgType  MessageType
		scenario interface{}
	}{
		{
			msgType: MsgInit,
			scenario: func(m Init) bool {
				return mainScenario(&m)
			},
		},
		{
			msgType: MsgError,
			scenario: func(m Error) bool {
				return mainScenario(&m)
			},
		},
		{
			msgType: MsgPing,
			scenario: func(m Ping) bool {
				return mainScenario(&m)
			},
		},
		{
			msgType: MsgPong,
			scenario: func(m Pong) bool {
				return mainScenario(&m)
			},
		},
		{
			msgType: MsgOpenChannel,
			scenario: func(m OpenChannel) bool {
				return mainScenario(&m)
			},
		},
		{
			msgType: MsgAcceptChannel,
			scenario: func(m AcceptChannel) bool {
				return mainScenario(&m)
			},
		},
		{
			msgType: MsgFundingCreated,
			scenario: func(m FundingCreated) bool {
				return mainScenario(&m)
			},
		},
		{
			msgType: MsgFundingSigned,
			scenario: func(m FundingSigned) bool {
				return mainScenario(&m)
			},
		},
		{
			msgType: MsgFundingLocked,
			scenario: func(m FundingLocked) bool {
				return mainScenario(&m)
			},
		},
		{
			msgType: MsgShutdown,
			scenario: func(m Shutdown) bool {
				return mainScenario(&m)
			},
		},
		{
			msgType: MsgClosingSigned,
			scenario: func(m ClosingSigned) bool {
				return mainScenario(&m)
			},
		},
		{
			msgType: MsgUpdateAddHTLC,
			scenario: func(m UpdateAddHTLC) bool {
				return mainScenario(&m)
			},
		},
		{
			msgType: MsgUpdateFufillHTLC,
			scenario: func(m UpdateFufillHTLC) bool {
				return mainScenario(&m)
			},
		},
		{
			msgType: MsgUpdateFailHTLC,
			scenario: func(m UpdateFailHTLC) bool {
				return mainScenario(&m)
			},
		},
		{
			msgType: MsgCommitSig,
			scenario: func(m CommitSig) bool {
				return mainScenario(&m)
			},
		},
		{
			msgType: MsgRevokeAndAck,
			scenario: func(m RevokeAndAck) bool {
				return mainScenario(&m)
			},
		},
		{
			msgType: MsgUpdateFee,
			scenario: func(m UpdateFee) bool {
				return mainScenario(&m)
			},
		},
		{

			msgType: MsgUpdateFailMalformedHTLC,
			scenario: func(m UpdateFailMalformedHTLC) bool {
				return mainScenario(&m)
			},
		},
		{
			msgType: MsgChannelAnnouncement,
			scenario: func(m ChannelAnnouncement) bool {
				return mainScenario(&m)
			},
		},
		{
			msgType: MsgNodeAnnouncement,
			scenario: func(m NodeAnnouncement) bool {
				return mainScenario(&m)
			},
		},
		{
			msgType: MsgChannelUpdate,
			scenario: func(m ChannelUpdate) bool {
				return mainScenario(&m)
			},
		},
		{
			msgType: MsgAnnounceSignatures,
			scenario: func(m AnnounceSignatures) bool {
				return mainScenario(&m)
			},
		},
	}
	for _, test := range tests {
		var config *quick.Config

		// If the type defined is within the custom type gen map above,
		// then we'll modify the default config to use this Value
		// function that knows how to generate the proper types.
		if valueGen, ok := customTypeGen[test.msgType]; ok {
			config = &quick.Config{
				Values: valueGen,
			}
		}

		t.Logf("Running fuzz tests for msgType=%v", test.msgType)
		if err := quick.Check(test.scenario, config); err != nil {
			t.Fatalf("fuzz checks for msg=%v failed: %v",
				test.msgType, err)
		}
	}

}
