package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/awalterschulze/gographviz"
	"github.com/golang/protobuf/jsonpb"
	"github.com/golang/protobuf/proto"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcutil"
	"github.com/urfave/cli"
	"golang.org/x/net/context"
)

// TODO(roasbeef): cli logic for supporting both positional and unix style
// arguments.

func printJson(resp interface{}) {
	b, err := json.Marshal(resp)
	if err != nil {
		fatal(err)
	}

	var out bytes.Buffer
	json.Indent(&out, b, "", "\t")
	out.WriteTo(os.Stdout)
}

func printRespJson(resp proto.Message) {
	jsonMarshaler := &jsonpb.Marshaler{
		EmitDefaults: true,
		Indent:       "    ",
	}

	jsonStr, err := jsonMarshaler.MarshalToString(resp)
	if err != nil {
		fmt.Println("unable to decode response: ", err)
		return
	}

	fmt.Println(jsonStr)
}

var NewAddressCommand = cli.Command{
	Name:      "newaddress",
	Usage:     "generates a new address.",
	ArgsUsage: "address-type",
	Description: "Generate a wallet new address. Address-types has to be one of:\n" +
		"   - p2wkh:  Push to witness key hash\n" +
		"   - np2wkh: Push to nested witness key hash\n" +
		"   - p2pkh:  Push to public key hash",
	Action: newAddress,
}

func newAddress(ctx *cli.Context) error {
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	stringAddrType := ctx.Args().First()

	// Map the string encoded address type, to the concrete typed address
	// type enum. An unrecognized address type will result in an error.
	var addrType lnrpc.NewAddressRequest_AddressType
	switch stringAddrType { // TODO(roasbeef): make them ints on the cli?
	case "p2wkh":
		addrType = lnrpc.NewAddressRequest_WITNESS_PUBKEY_HASH
	case "np2wkh":
		addrType = lnrpc.NewAddressRequest_NESTED_PUBKEY_HASH
	case "p2pkh":
		addrType = lnrpc.NewAddressRequest_PUBKEY_HASH
	default:
		return fmt.Errorf("invalid address type %v, support address type "+
			"are: p2wkh, np2wkh, p2pkh", stringAddrType)
	}

	ctxb := context.Background()
	addr, err := client.NewAddress(ctxb, &lnrpc.NewAddressRequest{
		Type: addrType,
	})
	if err != nil {
		return err
	}

	printRespJson(addr)
	return nil
}

var SendCoinsCommand = cli.Command{
	Name:      "sendcoins",
	Usage:     "send bitcoin on-chain to an address",
	ArgsUsage: "addr amt",
	Description: "Send amt coins in satoshis to the BASE58 encoded bitcoin address addr.\n\n" +
		"   Positional arguments and flags can be used interchangeably but not at the same time!",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "addr",
			Usage: "the BASE58 encoded bitcoin address to send coins to on-chain",
		},
		// TODO(roasbeef): switch to BTC on command line? int may not be sufficient
		cli.Int64Flag{
			Name:  "amt",
			Usage: "the number of bitcoin denominated in satoshis to send",
		},
	},
	Action: sendCoins,
}

func sendCoins(ctx *cli.Context) error {
	var (
		addr string
		amt  int64
		err  error
	)
	args := ctx.Args()

	if ctx.NArg() == 0 && ctx.NumFlags() == 0 {
		cli.ShowCommandHelp(ctx, "sendcoins")
		return nil
	}

	switch {
	case ctx.IsSet("addr"):
		addr = ctx.String("addr")
	case args.Present():
		addr = args.First()
		args = args.Tail()
	default:
		return fmt.Errorf("Address argument missing")
	}

	switch {
	case ctx.IsSet("amt"):
		amt = ctx.Int64("amt")
	case args.Present():
		amt, err = strconv.ParseInt(args.First(), 10, 64)
	default:
		return fmt.Errorf("Amount argument missing")
	}

	if err != nil {
		return fmt.Errorf("unable to decode amount: %v", err)
	}

	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.SendCoinsRequest{
		Addr:   addr,
		Amount: amt,
	}
	txid, err := client.SendCoins(ctxb, req)
	if err != nil {
		return err
	}

	printRespJson(txid)
	return nil
}

var SendManyCommand = cli.Command{
	Name:      "sendmany",
	Usage:     "send bitcoin on-chain to multiple addresses.",
	ArgsUsage: "send-json-string",
	Description: "create and broadcast a transaction paying the specified " +
		"amount(s) to the passed address(es)\n" +
		"   'send-json-string' decodes addresses and the amount to send " +
		"respectively in the following format.\n" +
		`   '{"ExampleAddr": NumCoinsInSatoshis, "SecondAddr": NumCoins}'`,
	Action: sendMany,
}

func sendMany(ctx *cli.Context) error {
	var amountToAddr map[string]int64

	jsonMap := ctx.Args().First()
	if err := json.Unmarshal([]byte(jsonMap), &amountToAddr); err != nil {
		return err
	}

	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	txid, err := client.SendMany(ctxb, &lnrpc.SendManyRequest{amountToAddr})
	if err != nil {
		return err
	}

	printRespJson(txid)
	return nil
}

var ConnectCommand = cli.Command{
	Name:      "connect",
	Usage:     "connect to a remote lnd peer",
	ArgsUsage: "<pubkey>@host",
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name: "perm",
			Usage: "If set, the daemon will attempt to persistently " +
				"connect to the target peer.\n" +
				"           If not, the call will be synchronous.",
		},
	},
	Action: connectPeer,
}

func connectPeer(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	targetAddress := ctx.Args().First()
	splitAddr := strings.Split(targetAddress, "@")
	if len(splitAddr) != 2 {
		return fmt.Errorf("target address expected in format: " +
			"pubkey@host:port")
	}

	addr := &lnrpc.LightningAddress{
		Pubkey: splitAddr[0],
		Host:   splitAddr[1],
	}
	req := &lnrpc.ConnectPeerRequest{
		Addr: addr,
		Perm: ctx.Bool("perm"),
	}

	lnid, err := client.ConnectPeer(ctxb, req)
	if err != nil {
		return err
	}

	printRespJson(lnid)
	return nil
}

// TODO(roasbeef): change default number of confirmations
var OpenChannelCommand = cli.Command{
	Name:  "openchannel",
	Usage: "Open a channel to an existing peer.",
	Description: "Attempt to open a new channel to an existing peer with the key node-key, " +
		"optionally blocking until the channel is 'open'. " +
		"The channel will be initialized with local-amt satoshis local and push-amt " +
		"satoshis for the remote node. Once the " +
		"channel is open, a channelPoint (txid:vout) of the funding " +
		"output is returned. NOTE: peer_id and node_key are " +
		"mutually exclusive, only one should be used, not both.",
	ArgsUsage: "node-key local-amt push-amt [num-confs]",
	Flags: []cli.Flag{
		cli.IntFlag{
			Name:  "peer_id",
			Usage: "the relative id of the peer to open a channel with",
		},
		cli.StringFlag{
			Name: "node_key",
			Usage: "the identity public key of the target peer " +
				"serialized in compressed format",
		},
		cli.IntFlag{
			Name:  "local_amt",
			Usage: "the number of satoshis the wallet should commit to the channel",
		},
		cli.IntFlag{
			Name: "push_amt",
			Usage: "the number of satoshis to push to the remote " +
				"side as part of the initial commitment state",
		},
		cli.IntFlag{
			Name: "num_confs",
			Usage: "the number of confirmations required before the " +
				"channel is considered 'open'",
			Value: 1,
		},
		cli.BoolFlag{
			Name:  "block",
			Usage: "block and wait until the channel is fully open",
		},
	},
	Action: openChannel,
}

func openChannel(ctx *cli.Context) error {
	// TODO(roasbeef): add deadline to context
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()
	args := ctx.Args()
	var err error

	// Show command help if no arguments provided
	if ctx.NArg() == 0 && ctx.NumFlags() == 0 {
		cli.ShowCommandHelp(ctx, "openchannel")
		return nil
	}

	if ctx.IsSet("peer_id") && ctx.IsSet("node_key") {
		return fmt.Errorf("both peer_id and lightning_id cannot be set " +
			"at the same time, only one can be specified")
	}

	req := &lnrpc.OpenChannelRequest{
		NumConfs: uint32(ctx.Int("num_confs")),
	}

	switch {
	case ctx.IsSet("peer_id"):
		req.TargetPeerId = int32(ctx.Int("peer_id"))
	case ctx.IsSet("node_key"):
		nodePubHex, err := hex.DecodeString(ctx.String("node_key"))
		if err != nil {
			return fmt.Errorf("unable to decode node public key: %v", err)
		}
		req.NodePubkey = nodePubHex
	case args.Present():
		nodePubHex, err := hex.DecodeString(args.First())
		if err != nil {
			return fmt.Errorf("unable to decode node public key: %v", err)
		}
		args = args.Tail()
		req.NodePubkey = nodePubHex
	default:
		return fmt.Errorf("lightning id argument missing")
	}

	switch {
	case ctx.IsSet("local_amt"):
		req.LocalFundingAmount = int64(ctx.Int("local_amt"))
	case args.Present():
		req.LocalFundingAmount, err = strconv.ParseInt(args.First(), 10, 64)
		if err != nil {
			return fmt.Errorf("unable to decode local amt: %v", err)
		}
		args = args.Tail()
	default:
		return fmt.Errorf("local amt argument missing")
	}

	switch {
	case ctx.IsSet("push_amt"):
		req.PushSat = int64(ctx.Int("push_amt"))
	case args.Present():
		req.PushSat, err = strconv.ParseInt(args.First(), 10, 64)
		if err != nil {
			return fmt.Errorf("unable to decode push amt: %v", err)
		}
	default:
		return fmt.Errorf("push amt argument missing")
	}

	stream, err := client.OpenChannel(ctxb, req)
	if err != nil {
		return err
	}

	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}

		switch update := resp.Update.(type) {
		case *lnrpc.OpenStatusUpdate_ChanPending:
			txid, err := chainhash.NewHash(update.ChanPending.Txid)
			if err != nil {
				return err
			}

			printJson(struct {
				FundingTxid string `json:"funding_txid"`
			}{
				FundingTxid: txid.String(),
			},
			)

			if !ctx.Bool("block") {
				return nil
			}

		case *lnrpc.OpenStatusUpdate_ChanOpen:
			channelPoint := update.ChanOpen.ChannelPoint
			txid, err := chainhash.NewHash(channelPoint.FundingTxid)
			if err != nil {
				return err
			}

			index := channelPoint.OutputIndex
			printJson(struct {
				ChannelPoint string `json:"channel_point"`
			}{
				ChannelPoint: fmt.Sprintf("%v:%v", txid, index),
			},
			)
		}
	}

	return nil
}

// TODO(roasbeef): also allow short relative channel ID.
var CloseChannelCommand = cli.Command{
	Name:  "closechannel",
	Usage: "Close an existing channel.",
	Description: "Close an existing channel. The channel can be closed either " +
		"cooperatively, or uncooperatively (forced).",
	ArgsUsage: "funding_txid [output_index [time_limit]]",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "funding_txid",
			Usage: "the txid of the channel's funding transaction",
		},
		cli.IntFlag{
			Name: "output_index",
			Usage: "the output index for the funding output of the funding " +
				"transaction",
		},
		cli.StringFlag{
			Name: "time_limit",
			Usage: "a relative deadline afterwhich the attempt should be " +
				"abandonded",
		},
		cli.BoolFlag{
			Name: "force",
			Usage: "after the time limit has passed, attempt an " +
				"uncooperative closure",
		},
		cli.BoolFlag{
			Name:  "block",
			Usage: "block until the channel is closed",
		},
	},
	Action: closeChannel,
}

func closeChannel(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	args := ctx.Args()
	var (
		txid string
		err  error
	)

	// Show command help if no arguments provieded
	if ctx.NArg() == 0 && ctx.NumFlags() == 0 {
		cli.ShowCommandHelp(ctx, "closeChannel")
		return nil
	}

	// TODO(roasbeef): implement time deadline within server
	req := &lnrpc.CloseChannelRequest{
		ChannelPoint: &lnrpc.ChannelPoint{},
		Force:        ctx.Bool("force"),
	}

	switch {
	case ctx.IsSet("funding_txid"):
		txid = ctx.String("funding_txid")
	case args.Present():
		txid = args.First()
		args = args.Tail()
	default:
		return fmt.Errorf("funding txid argument missing")
	}

	txidhash, err := chainhash.NewHashFromStr(txid)
	if err != nil {
		return err
	}
	req.ChannelPoint.FundingTxid = txidhash[:]

	switch {
	case ctx.IsSet("output_index"):
		req.ChannelPoint.OutputIndex = uint32(ctx.Int("output_index"))
	case args.Present():
		index, err := strconv.ParseInt(args.First(), 10, 32)
		if err != nil {
			return fmt.Errorf("unable to decode output index: %v", err)
		}
		req.ChannelPoint.OutputIndex = uint32(index)
	default:
		req.ChannelPoint.OutputIndex = 0
	}

	stream, err := client.CloseChannel(ctxb, req)
	if err != nil {
		return err
	}

	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}

		switch update := resp.Update.(type) {
		case *lnrpc.CloseStatusUpdate_ClosePending:
			closingHash := update.ClosePending.Txid
			txid, err := chainhash.NewHash(closingHash)
			if err != nil {
				return err
			}

			printJson(struct {
				ClosingTXID string `json:"closing_txid"`
			}{
				ClosingTXID: txid.String(),
			})

			if !ctx.Bool("block") {
				return nil
			}

		case *lnrpc.CloseStatusUpdate_ChanClose:
			closingHash := update.ChanClose.ClosingTxid
			txid, err := chainhash.NewHash(closingHash)
			if err != nil {
				return err
			}

			printJson(struct {
				ClosingTXID string `json:"closing_txid"`
			}{
				ClosingTXID: txid.String(),
			})
		}
	}

	return nil
}

var ListPeersCommand = cli.Command{
	Name:   "listpeers",
	Usage:  "List all active, currently connected peers.",
	Action: listPeers,
}

func listPeers(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.ListPeersRequest{}
	resp, err := client.ListPeers(ctxb, req)
	if err != nil {
		return err
	}

	printRespJson(resp)
	return nil
}

var WalletBalanceCommand = cli.Command{
	Name:  "walletbalance",
	Usage: "compute and display the wallet's current balance",
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name: "witness_only",
			Usage: "if only witness outputs should be considered when " +
				"calculating the wallet's balance",
		},
	},
	Action: walletBalance,
}

func walletBalance(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.WalletBalanceRequest{
		WitnessOnly: ctx.Bool("witness_only"),
	}
	resp, err := client.WalletBalance(ctxb, req)
	if err != nil {
		return err
	}

	printRespJson(resp)
	return nil
}

var ChannelBalanceCommand = cli.Command{
	Name:   "channelbalance",
	Usage:  "returns the sum of the total available channel balance across all open channels",
	Action: channelBalance,
}

func channelBalance(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.ChannelBalanceRequest{}
	resp, err := client.ChannelBalance(ctxb, req)
	if err != nil {
		return err
	}

	printRespJson(resp)
	return nil
}

var GetInfoCommand = cli.Command{
	Name:   "getinfo",
	Usage:  "returns basic information related to the active daemon",
	Action: getInfo,
}

func getInfo(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.GetInfoRequest{}
	resp, err := client.GetInfo(ctxb, req)
	if err != nil {
		return err
	}

	printRespJson(resp)
	return nil
}

var PendingChannelsCommand = cli.Command{
	Name:  "pendingchannels",
	Usage: "display information pertaining to pending channels",
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name:  "open, o",
			Usage: "display the status of new pending channels",
		},
		cli.BoolFlag{
			Name:  "close, c",
			Usage: "display the status of channels being closed",
		},
		cli.BoolFlag{
			Name: "all, a",
			Usage: "display the status of channels in the " +
				"process of being opened or closed",
		},
	},
	Action: pendingChannels,
}

func pendingChannels(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	var channelStatus lnrpc.ChannelStatus
	switch {
	case ctx.Bool("all"):
		channelStatus = lnrpc.ChannelStatus_ALL
	case ctx.Bool("open"):
		channelStatus = lnrpc.ChannelStatus_OPENING
	case ctx.Bool("close"):
		channelStatus = lnrpc.ChannelStatus_CLOSING
	default:
		channelStatus = lnrpc.ChannelStatus_ALL
	}

	req := &lnrpc.PendingChannelRequest{channelStatus}
	resp, err := client.PendingChannels(ctxb, req)
	if err != nil {
		return err
	}

	printRespJson(resp)

	return nil
}

var ListChannelsCommand = cli.Command{
	Name:  "listchannels",
	Usage: "list all open channels",
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name:  "active_only, a",
			Usage: "only list channels which are currently active",
		},
	},
	Action: listChannels,
}

func listChannels(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.ListChannelsRequest{}
	resp, err := client.ListChannels(ctxb, req)
	if err != nil {
		return err
	}

	// TODO(roasbeef): defer close the client for the all

	printRespJson(resp)

	return nil
}

var SendPaymentCommand = cli.Command{
	Name:  "sendpayment",
	Usage: "send a payment over lightning",
	ArgsUsage: "(destination amount payment_hash " +
		"| --pay_req=[payment request])",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "dest, d",
			Usage: "the compressed identity pubkey of the " +
				"payment recipient",
		},
		cli.Int64Flag{
			Name:  "amt, a",
			Usage: "number of satoshis to send",
		},
		cli.StringFlag{
			Name:  "payment_hash, r",
			Usage: "the hash to use within the payment's HTLC",
		},
		cli.BoolFlag{
			Name:  "debug_send",
			Usage: "use the debug rHash when sending the HTLC",
		},
		cli.StringFlag{
			Name:  "pay_req",
			Usage: "a zbase32-check encoded payment request to fulfill",
		},
	},
	Action: sendPaymentCommand,
}

func sendPaymentCommand(ctx *cli.Context) error {
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	// Show command help if no arguments provieded
	if ctx.NArg() == 0 && ctx.NumFlags() == 0 {
		cli.ShowCommandHelp(ctx, "sendpayment")
		return nil
	}

	var req *lnrpc.SendRequest
	if ctx.IsSet("pay_req") {
		req = &lnrpc.SendRequest{
			PaymentRequest: ctx.String("pay_req"),
		}
	} else {
		args := ctx.Args()

		var (
			destNode []byte
			err      error
			amount   int64
		)

		switch {
		case ctx.IsSet("dest"):
			destNode, err = hex.DecodeString(ctx.String("dest"))
		case args.Present():
			destNode, err = hex.DecodeString(args.First())
			args = args.Tail()
		default:
			return fmt.Errorf("destination txid argument missing")
		}
		if err != nil {
			return err
		}

		if len(destNode) != 33 {
			return fmt.Errorf("dest node pubkey must be exactly 33 bytes, is "+
				"instead: %v", len(destNode))
		}

		if ctx.IsSet("amt") {
			amount = ctx.Int64("amt")
		} else if args.Present() {
			amount, err = strconv.ParseInt(args.First(), 10, 64)
			args = args.Tail()
			if err != nil {
				return fmt.Errorf("unable to decode payment amount: %v", err)
			}
		}

		req = &lnrpc.SendRequest{
			Dest: destNode,
			Amt:  amount,
		}

		if ctx.Bool("debug_send") && (ctx.IsSet("payment_hash") || args.Present()) {
			return fmt.Errorf("do not provide a payment hash with debug send")
		} else if !ctx.Bool("debug_send") {
			var rHash []byte

			switch {
			case ctx.IsSet("payment_hash"):
				rHash, err = hex.DecodeString(ctx.String("payment_hash"))
			case args.Present():
				rHash, err = hex.DecodeString(args.First())
			default:
				return fmt.Errorf("payment hash argument missing")
			}

			if err != nil {
				return err
			}
			if len(rHash) != 32 {
				return fmt.Errorf("payment hash must be exactly 32 "+
					"bytes, is instead %v", len(rHash))
			}
			req.PaymentHash = rHash
		}
	}

	paymentStream, err := client.SendPayment(context.Background())
	if err != nil {
		return err
	}

	if err := paymentStream.Send(req); err != nil {
		return err
	}

	resp, err := paymentStream.Recv()
	if err != nil {
		return err
	}

	paymentStream.CloseSend()

	printJson(struct {
		P string       `json:"payment_preimage"`
		R *lnrpc.Route `json:"payment_route"`
	}{
		P: hex.EncodeToString(resp.PaymentPreimage),
		R: resp.PaymentRoute,
	})

	return nil
}

var AddInvoiceCommand = cli.Command{
	Name:  "addinvoice",
	Usage: "add a new invoice.",
	Description: "Add a new invoice, expressing intent for a future payment. " +
		"The value of the invoice in satoshis and a 32 byte hash preimage are neccesary for the creation",
	ArgsUsage: "value preimage",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "memo",
			Usage: "an optional memo to attach along with the invoice",
		},
		cli.StringFlag{
			Name:  "receipt",
			Usage: "an optional cryptographic receipt of payment",
		},
		cli.StringFlag{
			Name:  "preimage",
			Usage: "the hex-encoded preimage (32 byte) which will allow settling an incoming HTLC payable to this preimage",
		},
		cli.Int64Flag{
			Name:  "value",
			Usage: "the value of this invoice in satoshis",
		},
	},
	Action: addInvoice,
}

func addInvoice(ctx *cli.Context) error {
	var (
		preimage []byte
		receipt  []byte
		value    int64
		err      error
	)

	client, cleanUp := getClient(ctx)
	defer cleanUp()

	args := ctx.Args()

	switch {
	case ctx.IsSet("value"):
		value = ctx.Int64("value")
	case args.Present():
		value, err = strconv.ParseInt(args.First(), 10, 64)
		args = args.Tail()
		if err != nil {
			return fmt.Errorf("unable to decode value argument: %v", err)
		}
	default:
		return fmt.Errorf("value argument missing")
	}

	switch {
	case ctx.IsSet("preimage"):
		preimage, err = hex.DecodeString(ctx.String("preimage"))
	case args.Present():
		preimage, err = hex.DecodeString(args.First())
	}

	if err != nil {
		return fmt.Errorf("unable to parse preimage: %v", err)
	}

	receipt, err = hex.DecodeString(ctx.String("receipt"))
	if err != nil {
		return fmt.Errorf("unable to parse receipt: %v", err)
	}

	invoice := &lnrpc.Invoice{
		Memo:      ctx.String("memo"),
		Receipt:   receipt,
		RPreimage: preimage,
		Value:     value,
	}

	resp, err := client.AddInvoice(context.Background(), invoice)
	if err != nil {
		return err
	}

	printJson(struct {
		RHash  string `json:"r_hash"`
		PayReq string `json:"pay_req"`
	}{
		RHash:  hex.EncodeToString(resp.RHash),
		PayReq: resp.PaymentRequest,
	})

	return nil
}

var LookupInvoiceCommand = cli.Command{
	Name:      "lookupinvoice",
	Usage:     "Lookup an existing invoice by its payment hash.",
	ArgsUsage: "rhash",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "rhash",
			Usage: "the 32 byte payment hash of the invoice to query for, the hash " +
				"should be a hex-encoded string",
		},
	},
	Action: lookupInvoice,
}

func lookupInvoice(ctx *cli.Context) error {
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	var (
		rHash []byte
		err   error
	)

	switch {
	case ctx.IsSet("rhash"):
		rHash, err = hex.DecodeString(ctx.String("rhash"))
	case ctx.Args().Present():
		rHash, err = hex.DecodeString(ctx.Args().First())
	default:
		return fmt.Errorf("rhash argument missing")
	}

	if err != nil {
		return fmt.Errorf("unable to decode rhash argument: %v", err)
	}

	req := &lnrpc.PaymentHash{
		RHash: rHash,
	}

	invoice, err := client.LookupInvoice(context.Background(), req)
	if err != nil {
		return err
	}

	printRespJson(invoice)

	return nil
}

var ListInvoicesCommand = cli.Command{
	Name:  "listinvoices",
	Usage: "List all invoices currently stored.",
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name: "pending_only",
			Usage: "toggles if all invoices should be returned, or only " +
				"those that are currently unsettled",
		},
	},
	Action: listInvoices,
}

func listInvoices(ctx *cli.Context) error {
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	pendingOnly := true
	if !ctx.Bool("pending_only") {
		pendingOnly = false
	}

	req := &lnrpc.ListInvoiceRequest{
		PendingOnly: pendingOnly,
	}

	invoices, err := client.ListInvoices(context.Background(), req)
	if err != nil {
		return err
	}

	printRespJson(invoices)

	return nil
}

var DescribeGraphCommand = cli.Command{
	Name: "describegraph",
	Description: "prints a human readable version of the known channel " +
		"graph from the PoV of the node",
	Usage: "describe the network graph",
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name:  "render",
			Usage: "If set, then an image of graph will be generated and displayed. The generated image is stored within the current directory with a file name of 'graph.svg'",
		},
	},
	Action: describeGraph,
}

func describeGraph(ctx *cli.Context) error {
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.ChannelGraphRequest{}

	graph, err := client.DescribeGraph(context.Background(), req)
	if err != nil {
		return err
	}

	// If the draw flag is on, then we'll use the 'dot' command to create a
	// visualization of the graph itself.
	if ctx.Bool("render") {
		return drawChannelGraph(graph)
	}

	printRespJson(graph)
	return nil
}

// normalizeFunc is a factory function which returns a function that normalizes
// the capacity of of edges within the graph. The value of the returned
// function can be used to either plot the capacities, or to use a weight in a
// rendering of the graph.
func normalizeFunc(edges []*lnrpc.ChannelEdge, scaleFactor float64) func(int64) float64 {
	var (
		min float64 = math.MaxInt64
		max float64
	)

	for _, edge := range edges {
		// In order to obtain saner values, we reduce the capacity of a
		// channel to it's base 2 logarithm.
		z := math.Log2(float64(edge.Capacity))

		if z < min {
			min = z
		}
		if z > max {
			max = z
		}
	}

	return func(x int64) float64 {
		y := math.Log2(float64(x))

		// TODO(roasbeef): results in min being zero
		return float64(y-min) / float64(max-min) * scaleFactor
	}
}

func drawChannelGraph(graph *lnrpc.ChannelGraph) error {
	// First we'll create a temporary file that we'll write the compiled
	// string that describes our graph in the dot format to.
	tempDotFile, err := ioutil.TempFile("", "")
	if err != nil {
		return err
	}
	defer os.Remove(tempDotFile.Name())

	// Next, we'll create (or re-create) the file that the final graph
	// image will be written to.
	imageFile, err := os.Create("graph.svg")
	if err != nil {
		return err
	}

	// With our temporary files set up, we'll initialize the graphviz
	// object that we'll use to draw our graph.
	graphName := "LightningNetwork"
	graphCanvas := gographviz.NewGraph()
	graphCanvas.SetName(graphName)
	graphCanvas.SetDir(false)

	const numKeyChars = 10

	truncateStr := func(k string, n uint) string {
		return k[:n]
	}

	// For each node within the graph, we'll add a new vertex to the graph.
	for _, node := range graph.Nodes {
		// Rather than using the entire hex-encoded string, we'll only
		// use the first 10 characters. We also add a prefix of "Z" as
		// graphviz is unable to parse the compressed pubkey as a
		// non-integer.
		//
		// TODO(roasbeef): should be able to get around this?
		nodeID := fmt.Sprintf(`"%v"`, truncateStr(node.PubKey, numKeyChars))

		graphCanvas.AddNode(graphName, nodeID, gographviz.Attrs{})
	}

	normalize := normalizeFunc(graph.Edges, 3)

	// Similarly, for each edge we'll add an edge between the corresponding
	// nodes added to the graph above.
	for _, edge := range graph.Edges {
		// Once again, we add a 'Z' prefix so we're compliant with the
		// dot grammar.
		src := fmt.Sprintf(`"%v"`, truncateStr(edge.Node1Pub, numKeyChars))
		dest := fmt.Sprintf(`"%v"`, truncateStr(edge.Node2Pub, numKeyChars))

		// The weight for our edge will be the total capacity of the
		// channel, in BTC.
		// TODO(roasbeef): can also factor in the edges time-lock delta
		// and fee information
		amt := btcutil.Amount(edge.Capacity).ToBTC()
		edgeWeight := strconv.FormatFloat(amt, 'f', -1, 64)

		// The label for each edge will simply be a truncated version
		// of it's channel ID.
		chanIDStr := strconv.FormatUint(edge.ChannelId, 10)
		edgeLabel := fmt.Sprintf(`"cid:%v"`, truncateStr(chanIDStr, 7))

		// We'll also use a normalized version of the channels'
		// capacity in satoshis in order to modulate the "thickness" of
		// the line that creates the edge within the graph.
		normalizedCapacity := normalize(edge.Capacity)
		edgeThickness := strconv.FormatFloat(normalizedCapacity, 'f', -1, 64)

		// TODO(roasbeef): color code based on percentile capacity
		graphCanvas.AddEdge(src, dest, false, gographviz.Attrs{
			"penwidth": edgeThickness,
			"weight":   edgeWeight,
			"label":    edgeLabel,
		})
	}

	// With the declarative generation of the graph complete, we now write
	// the dot-string description of the graph
	graphDotString := graphCanvas.String()
	if _, err := tempDotFile.WriteString(graphDotString); err != nil {
		return err
	}
	if err := tempDotFile.Sync(); err != nil {
		return err
	}

	var errBuffer bytes.Buffer

	// Once our dot file has been written to disk, we can use the dot
	// command itself to generate the drawn rendering of the graph
	// described.
	drawCmd := exec.Command("dot", "-T"+"svg", "-o"+imageFile.Name(),
		tempDotFile.Name())
	drawCmd.Stderr = &errBuffer
	if err := drawCmd.Run(); err != nil {
		fmt.Println("error rendering graph: ", errBuffer.String())
		fmt.Println("dot: ", graphDotString)

		return err
	}

	errBuffer.Reset()

	// Finally, we'll open the drawn graph to display to the user.
	openCmd := exec.Command("open", imageFile.Name())
	openCmd.Stderr = &errBuffer
	if err := openCmd.Run(); err != nil {
		fmt.Println("error opening rendered graph image: ",
			errBuffer.String())
		return err
	}

	return nil
}

var ListPaymentsCommand = cli.Command{
	Name:   "listpayments",
	Usage:  "list all outgoing payments",
	Action: listPayments,
}

func listPayments(ctx *cli.Context) error {
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.ListPaymentsRequest{}

	payments, err := client.ListPayments(context.Background(), req)
	if err != nil {
		return err
	}

	printRespJson(payments)
	return nil
}

var GetChanInfoCommand = cli.Command{
	Name:  "getchaninfo",
	Usage: "get the state of a channel",
	Description: "prints out the latest authenticated state for a " +
		"particular channel",
	ArgsUsage: "chan_id",
	Flags: []cli.Flag{
		cli.Int64Flag{
			Name:  "chan_id",
			Usage: "the 8-byte compact channel ID to query for",
		},
	},
	Action: getChanInfo,
}

func getChanInfo(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	var (
		chan_id int64
		err     error
	)

	switch {
	case ctx.IsSet("chan_id"):
		chan_id = ctx.Int64("chan_id")
	case ctx.Args().Present():
		chan_id, err = strconv.ParseInt(ctx.Args().First(), 10, 64)
	default:
		return fmt.Errorf("chan_id argument missing")
	}

	req := &lnrpc.ChanInfoRequest{
		ChanId: uint64(chan_id),
	}

	chanInfo, err := client.GetChanInfo(ctxb, req)
	if err != nil {
		return err
	}

	printRespJson(chanInfo)
	return nil
}

var GetNodeInfoCommand = cli.Command{
	Name:  "getnodeinfo",
	Usage: "Get information on a specific node.",
	Description: "prints out the latest authenticated node state for an " +
		"advertised node",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "pub_key",
			Usage: "the 33-byte hex-encoded compressed public of the target " +
				"node",
		},
	},
	Action: getNodeInfo,
}

func getNodeInfo(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	if !ctx.IsSet("pub_key") {
		return fmt.Errorf("pub_key argument missing")
	}

	req := &lnrpc.NodeInfoRequest{
		PubKey: ctx.String("pub_key"),
	}

	nodeInfo, err := client.GetNodeInfo(ctxb, req)
	if err != nil {
		return err
	}

	printRespJson(nodeInfo)
	return nil
}

var QueryRouteCommand = cli.Command{
	Name:        "queryroute",
	Usage:       "Query a route to a destination.",
	Description: "Queries the channel router for a potential path to the destination that has sufficient flow for the amount including fees",
	ArgsUsage:   "dest amt",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "dest",
			Usage: "the 33-byte hex-encoded public key for the payment " +
				"destination",
		},
		cli.Int64Flag{
			Name:  "amt",
			Usage: "the amount to send expressed in satoshis",
		},
	},
	Action: queryRoute,
}

func queryRoute(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	var (
		dest string
		amt  int64
		err  error
	)

	args := ctx.Args()

	switch {
	case ctx.IsSet("dest"):
		dest = ctx.String("dest")
	case args.Present():
		dest = args.First()
		args = args.Tail()
	default:
		return fmt.Errorf("dest argument missing")
	}

	switch {
	case ctx.IsSet("amt"):
		amt = ctx.Int64("amt")
	case args.Present():
		amt, err = strconv.ParseInt(args.First(), 10, 64)
		if err != nil {
			return fmt.Errorf("unable to decode amt argument: %v", err)
		}
	default:
		return fmt.Errorf("amt argument missing")
	}

	req := &lnrpc.RouteRequest{
		PubKey: dest,
		Amt:    amt,
	}

	route, err := client.QueryRoute(ctxb, req)
	if err != nil {
		return err
	}

	printRespJson(route)
	return nil
}

var GetNetworkInfoCommand = cli.Command{
	Name:  "getnetworkinfo",
	Usage: "getnetworkinfo",
	Description: "returns a set of statistics pertaining to the known channel " +
		"graph",
	Action: getNetworkInfo,
}

func getNetworkInfo(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.NetworkInfoRequest{}

	netInfo, err := client.GetNetworkInfo(ctxb, req)
	if err != nil {
		return err
	}

	printRespJson(netInfo)
	return nil
}

var DebugLevel = cli.Command{
	Name:        "debuglevel",
	Usage:       "Set the debug level.",
	Description: "Logging level for all subsystems {trace, debug, info, warn, error, critical} -- You may also specify <subsystem>=<level>,<subsystem2>=<level>,... to set the log level for individual subsystems -- Use show to list available subsystems",
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name:  "show",
			Usage: "if true, then the list of available sub-systems will be printed out",
		},
		cli.StringFlag{
			Name:  "level",
			Usage: "the level specification to target either a coarse logging level, or granular set of specific sub-systems with loggin levels for each",
		},
	},
	Action: debugLevel,
}

func debugLevel(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.DebugLevelRequest{
		Show:      ctx.Bool("show"),
		LevelSpec: ctx.String("level"),
	}

	resp, err := client.DebugLevel(ctxb, req)
	if err != nil {
		return err
	}

	printRespJson(resp)
	return nil
}

var DecodePayReq = cli.Command{
	Name:        "decodepayreq",
	Usage:       "Decode a payment request.",
	Description: "Decode the passed payment request revealing the destination, payment hash and value of the payment request",
	ArgsUsage:   "pay_req",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "pay_req",
			Usage: "the zpay32 encoded payment request",
		},
	},
	Action: decodePayReq,
}

func decodePayReq(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	var payreq string

	switch {
	case ctx.IsSet("pay_req"):
		payreq = ctx.String("pay_req")
	case ctx.Args().Present():
		payreq = ctx.Args().First()
	default:
		return fmt.Errorf("pay_req argument missing")
	}

	resp, err := client.DecodePayReq(ctxb, &lnrpc.PayReqString{
		PayReq: payreq,
	})
	if err != nil {
		return err
	}

	printRespJson(resp)
	return nil
}
