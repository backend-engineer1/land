## Lightning Network Daemon

[![Build Status](http://img.shields.io/travis/lightningnetwork/lnd.svg)](https://travis-ci.org/lightningnetwork/lnd) 
[![MIT licensed](https://img.shields.io/badge/license-MIT-blue.svg)](https://github.com/lightningnetwork/lnd/blob/master/LICENSE) 
[![Irc](https://img.shields.io/badge/chat-on%20freenode-brightgreen.svg)](https://webchat.freenode.net/?channels=lnd) 
[![Godoc](https://godoc.org/github.com/lightningnetwork/lnd?status.svg)](https://godoc.org/github.com/lightningnetwork/lnd)
[![Coverage Status](https://coveralls.io/repos/github/lightningnetwork/lnd/badge.svg?branch=master)](https://coveralls.io/github/lightningnetwork/lnd?branch=master)

The Lightning Network Daemon (`lnd`) - is a complete implementation of a
[Lightning Network](https://lightning.network) node and currently deployed on
`testnet3` - the Bitcoin Test Network.  `lnd` has several pluggable back-end
chain services including [`btcd`](https://github.com/btcsuite/btcd) (a
full-node) and [`neutrino`](https://github.com/lightninglabs/neutrino) (a new
experimental light client). The project's codebase uses the
[btcsuite](https://github.com/btcsuite/) set of Bitcoin libraries, and also
exports a large set of isolated re-usable Lightning Network related libraries
within it.  In the current state `lnd` is capable of: 
* Creating channels.
* Closing channels.
* Completely managing all channel states (including the exceptional ones!).
* Maintaining a fully authenticated+validated channel graph.
* Performing path finding within the network, passively forwarding incoming payments.
* Sending outgoing [onion-encrypted payments](https://github.com/lightningnetwork/lightning-onion) 
through the network.
* Updating advertised fee schedules.
* Automatic channel management ([`autopilot`](https://github.com/lightningnetwork/lnd/tree/master/autopilot)).

## Lightning Network Specification Compliance
`lnd` doesn't yet _fully_ conform to the [Lightning Network specification
(BOLTs)](https://github.com/lightningnetwork/lightning-rfc). BOLT stands for:
Basic of Lightning Technologies. The specifications are currently being drafted
by several groups of implementers based around the world including the
developers of `lnd`. The set of specification documents as well as our
implementation of the specification are still a work-in-progress. With that
said, `lnd` the current status of `lnd`'s BOLT compliance is:

  - [X] BOLT 1: Base Protocol
  - [ ] BOLT 2: Peer Protocol for Channel Management
     * `lnd` has not yet integrated full channel re-transmission upon
       reconnection
  - [X] BOLT 3: Bitcoin Transaction and Script Formats
  - [X] BOLT 4: Onion Routing Protocol
  - [X] BOLT 5: Recommendations for On-chain Transaction Handling
  - [X] BOLT 7: P2P Node and Channel Discovery
  - [X] BOLT 8: Encrypted and Authenticated Transport

## Developer Resources

The daemon has been designed to be as developer friendly as possible in order
to facilitate application development on top of `lnd`. To primary RPC
interfaces are exported: an HTTP REST API, and a [gRPC](https://grpc.io/)
service. The exported API's are not yet stable, so be warned: they may change
drastically in the near future.

An automatically generated set of documentation for the RPC API's can be found
at [api.lightning.community](api.lightning.community). A set of developer
resources including talks, articles, and example applications can be found at:
[dev.lightning.community](dev.lightning.community).

Finally, we also have an active
[Slack](https://join.slack.com/t/lightningcommunity/shared_invite/MjI4OTg3MzQ4MjI2LTE1MDMxNzM1NTMtNjlmOGYzOTI1Ng)
where protocol developers, application developers, testers and users gather to
discuss various aspects of `lnd` and also Lightning in general.

## Installation
  In order to build from source, please see [the installation
  instructions](docs/INSTALL.md).
  
## IRC
  * irc.freenode.net
  * channel #lnd
  * [webchat](https://webchat.freenode.net/?channels=lnd)

## Further reading
* [Step-by-step send payment guide with docker](https://github.com/lightningnetwork/lnd/tree/master/docker)
* [Contribution guide](https://github.com/lightningnetwork/lnd/blob/master/docs/code_contribution_guidelines.md)
