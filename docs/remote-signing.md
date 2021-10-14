# Remote signing

Remote signing refers to an operating mode of `lnd` in which the wallet is
segregated into two parts, each running within its own instance of `lnd`. One
instance is running in watch-only mode which means it only has **public**
keys in its wallet. The second instance (in this document referred to as
"signer" or "remote signer" instance) has the same keys in its wallet, including
the **private** keys.

The advantage of such a setup is that the `lnd` instance containing the private
keys (the "signer") can be completely offline except for a single inbound gRPC
connection and the outbound connection to `bitcoind` (or `btcd` or `neutrino`).
The signer instance can run on a different machine with more tightly locked down
network security, optimally only allowing the single gRPC connection from the
outside.

An example setup could look like:

```text
         xxxxxxxxx
  xxxxxxxxx      xxxx
xxx                 xx
x   LN p2p network  xx
x                   x
xxx               xx
   xxxxx   xxxxxxxx
        xxx
          ^                       +----------------------------------+
          | p2p traffic           | firewalled/offline network zone  |
          |                       |                                  |
          v                       |                                  |
  +----------------+     gRPC     |   +----------------+             |
  | watch-only lnd +--------------+-->| full seed lnd  |             |
  +-------+--------+              |   +------+---------+             |
          |                       |          |                       |
  +-------v--------+              |   +------v---------+             |
  | bitcoind/btcd  |              |   | bitcoind/btcd  |             |
  +----------------+              |   +----------------+             |
                                  |                                  |
                                  +----------------------------------+
```

NOTE: Offline in this context means that `lnd` itself does not need to be
reachable from the internet and itself doesn't connect out. If the `bitcoind`
or other chain backend is indeed running within this same firewall/offline zone
then that component will need at least _outbound_ internet access. But it is
also possible to use a chain backend that is running outside this zone.

## Restrictions / limitations

The current implementation of the remote signing mode comes with a few
restrictions and limitations:

- Both `lnd` instances need a connection to a chain backend:
    - The type of chain backend (`bitcoind`, `btcd` or `neutrino`) **must**
      match.
    - Both instances can point to the same chain backend node, though limits
      apply with the number of `lnd` instances that can use the same `bitcoind`
      node over ZMQ.
      See ["running multiple lnd nodes" in the safety guide](safety.md#running-multiple-lnd-nodes)
      .
    - Using a pruned chain backend is not recommended as that increases the
      chance of the two wallets falling out of sync with each other.
- The wallet of the "signer" instance **must not be used** for anything.
  Especially generating new addresses manually on the "signer" will lead to the
  two wallets falling out of sync!

## Example setup

In this example we are going to set up two nodes, the "signer" that has the full
seed and private keys and the "watch-only" node that only has public keys.

### The "signer" node

The node "signer" is the hardened node that contains the private key material
and is not connected to the internet or LN P2P network at all. Ideally only a
single RPC based connection (that can be firewalled off specifically) can be
opened to this node from the host on which the node "watch-only" is running.

Recommended entries in `lnd.conf`:

```text
# No special configuration required other than basic "hardening" parameters to
# make sure no connections to the internet are opened.

[Application Options]
# Don't listen on the p2p port.
nolisten=true

# Don't reach out to the bootstrap nodes, we don't need a synced graph.
nobootstrap=true

# Just an example, this is the port that needs to be opened in the firewall and
# reachable from the node "watch-only".
rpclisten=10019
```

After successfully starting up "signer", the following command can be run to
export the `xpub`s of the wallet:

```shell
signer>  ⛰  lncli wallet accounts list > accounts-signer.json
```

That `accounts-signer.json` file has to be copied to the machine on which
"watch-only" will be running. It contains the extended public keys for all of
`lnd`'s accounts.

A custom macaroon can be baked for the watch-only node so it only gets the
minimum required permissions on the signer instance:

```shell
signer>  ⛰ lncli bakemacaroon --save_to signer.custom.macaroon \
                message:write signer:generate address:read onchain:write
```

Copy this file (`signer.custom.macaroon`) along with the `tls.cert` of the
signer node to the machine where the watch-only node will be running.

### The "watch-only" node

The node "watch-only" is the public, internet facing node that does not contain
any private keys in its wallet but delegates all signing operations to the node
"signer" over a single RPC connection.

Required entries in `lnd.conf`:

```text
[remotesigner]
remotesigner.enable=true
remotesigner.rpchost=zane.example.internal:10019
remotesigner.tlscertpath=/home/watch-only/example/signer.tls.cert
remotesigner.macaroonpath=/home/watch-only/example/signer.custom.macaroon
```

After starting "watch-only", the wallet can be created in watch-only mode by
running:

```shell
watch-only>  ⛰  lncli createwatchonly accounts-signer.json

Input wallet password: 
Confirm password: 

Input an optional wallet birthday unix timestamp of first block to start scanning from (default 0): 


Input an optional address look-ahead used to scan for used keys (default 2500):
```

Alternatively a script can be used for initializing the watch-only wallet
through the RPC interface as is described in the next section.

## Example initialization script

This section shows an example script that initializes the watch-only wallet of
the public node using NodeJS.

To use this example, first initialize the "signer" wallet with the root key
`tprv8ZgxMBicQKsPe6jS4vDm2n7s42Q6MpvghUQqMmSKG7bTZvGKtjrcU3PGzMNG37yzxywrcdvgkwrr8eYXJmbwdvUNVT4Ucv7ris4jvA7BUmg`
using the command line. This can be done by using the new `x` option during the
interactive `lncli create` command:

```bash
signer>  ⛰ lncli create
Input wallet password: 
Confirm password:

Do you have an existing cipher seed mnemonic or extended master root key you want to use?
Enter 'y' to use an existing cipher seed mnemonic, 'x' to use an extended master root key 
or 'n' to create a new seed (Enter y/x/n):
```

Then run this script against the "watch-only" node (after editing the
constants):

```javascript

// EDIT ME:
const WATCH_ONLY_LND_DIR = '/home/watch-only/.lnd';
const WATCH_ONLY_RPC_HOSTPORT = 'localhost:10018';
const WATCH_ONLY_WALLET_PASSWORD = 'testnet3';
const LND_SOURCE_DIR = '.';

const fs = require('fs');
const grpc = require('@grpc/grpc-js');
const protoLoader = require('@grpc/proto-loader');
const loaderOptions = {
    keepCase: true,
    longs: String,
    enums: String,
    defaults: true,
    oneofs: true
};
const packageDefinition = protoLoader.loadSync([
    LND_SOURCE_DIR + '/lnrpc/walletunlocker.proto',
], loaderOptions);

process.env.GRPC_SSL_CIPHER_SUITES = 'HIGH+ECDSA'

// build ssl credentials using the cert the same as before
let lndCert = fs.readFileSync(WATCH_ONLY_LND_DIR + '/tls.cert');
let sslCreds = grpc.credentials.createSsl(lndCert);

let lnrpcDescriptor = grpc.loadPackageDefinition(packageDefinition);
let lnrpc = lnrpcDescriptor.lnrpc;
var client = new lnrpc.WalletUnlocker(WATCH_ONLY_RPC_HOSTPORT, sslCreds);

client.initWallet({
    wallet_password: Buffer.from(WATCH_ONLY_WALLET_PASSWORD, 'utf-8'),
    recovery_window: 2500,
    watch_only: {
        accounts: [{
            purpose: 49,
            coin_type: 0,
            account: 0,
            xpub: 'tpubDDXEYWvGCTytEF6hBog9p4qr2QBUvJhh4P2wM4qHHv9N489khkQoGkBXDVoquuiyBf8SKBwrYseYdtq9j2v2nttPpE8qbuW3sE2MCkFPhTq',
        }, {
            purpose: 84,
            coin_type: 0,
            account: 0,
            xpub: 'tpubDDWAWrSLRSFrG1KdqXMQQyTKYGSKLKaY7gxpvK7RdV3e3DkhvuW2GgsFvsPN4RGmuoYtUgZ1LHZE8oftz7T4mzc1BxGt5rt8zJcVQiKTPPV',
        }, {
            purpose: 1017,
            coin_type: 1,
            account: 0,
            xpub: 'tpubDDXFHr67Ro2tHKVWG2gNjjijKUH1Lyv5NKFYdJnuaLGVNBVwyV5AbykhR43iy8wYozEMbw2QfmAqZhb8gnuL5mm9sZh8YsR6FjGAbew1xoT',
        }, {
            purpose: 1017,
            coin_type: 1,
            account: 1,
            xpub: 'tpubDDXFHr67Ro2tKkccDqNfDqZpd5wCs2n6XRV2Uh185DzCTbkDaEd9v7P837zZTYBNVfaRriuxgGVgxbGjDui4CKxyzBzwz4aAZxjn2PhNcQy',
        }, {
            purpose: 1017,
            coin_type: 1,
            account: 2,
            xpub: 'tpubDDXFHr67Ro2tNH4KH41i4oTsWfRjFigoH1Ee7urvHow51opH9xJ7mu1qSPMPVtkVqQZ5tE4NTuFJPrbDqno7TQietyUDmPTwyVviJbGCwXk',
        }, {
            purpose: 1017,
            coin_type: 1,
            account: 3,
            xpub: 'tpubDDXFHr67Ro2tQj5Zvav2ALhkU6dRQAhEtNPnYJVBC8hs2U1A9ecqxRY3XTiJKBDD7e8tudhmTRs8aGWJAiAXJN5kXy3Hi6cmiwGWjXK5Cv5',
        }, {
            purpose: 1017,
            coin_type: 1,
            account: 4,
            xpub: 'tpubDDXFHr67Ro2tSSR2LLBJtotxx2U45cuESLWKA72YT9td3SzVKHAptzDEx5chsUNZ4WRMY5h6HJxRSebjRatxQKX1uUsux1LvKS1wsfNJ2PH',
        }, {
            purpose: 1017,
            coin_type: 1,
            account: 5,
            xpub: 'tpubDDXFHr67Ro2tTwzfWvNoMoPpZbxdMEfe1WhbXJxvXikGixPa4ggSRZeGx6T5yxVHTVT3rjVh35Veqsowj7emX8SZfXKDKDKcLduXCeWPUU3',
        }, {
            purpose: 1017,
            coin_type: 1,
            account: 6,
            xpub: 'tpubDDXFHr67Ro2tYEDS2EByRedfsUoEwBtrzVbS1qdPrX6sAkUYGLrZWvMmQv8KZDZ4zd9r8WzM9bJ2nGp7XuNVC4w2EBtWg7i76gbrmuEWjQh',
        }, {
            purpose: 1017,
            coin_type: 1,
            account: 7,
            xpub: 'tpubDDXFHr67Ro2tYpwnFJEQaM8eAPM2UV5uY6gFgXeSzS5aC5T9TfzXuawYKBbQMZJn8qHXLafY4tAutoda1aKP5h6Nbgy3swPbnhWbFjS5wnX',
        }, {
            purpose: 1017,
            coin_type: 1,
            account: 8,
            xpub: 'tpubDDXFHr67Ro2tddKpAjUegXqt7EGxRXnHkeLbUkfuFMGbLJYgRpG4ew5pMmGg2nmcGmHFQ29w3juNhd8N5ZZ8HwJdymC4f5ukQLJ4yg9rEr3',
        }, {
            purpose: 1017,
            coin_type: 1,
            account: 9,
            xpub: 'tpubDDXFHr67Ro2tgE89V8ZdgMytC2Jq1iT9ttGhdzR1X7haQJNBmXt8kau6taC6DGASYzbrjmo9z9w6JQFcaLNqbhS2h2PVSzKf79j265Zi8hF',
        }]
    }
}, (err, res) => {
    if (err != null) {
        console.log(err);
    }
    console.log(res);
});
```
