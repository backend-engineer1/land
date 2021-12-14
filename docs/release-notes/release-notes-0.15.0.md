# Release Notes

## Security

* [Misconfigured ZMQ
  setup now gets reported](https://github.com/lightningnetwork/lnd/pull/5710).

## `lncli`

* Add [auto-generated command-line completions](https://github.com/lightningnetwork/lnd/pull/4177) 
  for Fish shell.  

## Misc

* [An example systemd service file](https://github.com/lightningnetwork/lnd/pull/6033)
  for running lnd alongside a bitcoind service is now provided in
  `contrib/init/lnd.service`.

* [Allow disabling migrations if the database backend passed to `channeldb` was
  opened in read-only mode](https://github.com/lightningnetwork/lnd/pull/6084).

## RPC Server

* [Add value to the field
  `remote_balance`](https://github.com/lightningnetwork/lnd/pull/5931) in
  `pending_force_closing_channels` under `pendingchannels` whereas before was
  empty(zero).

## Code Health

### Code cleanup, refactor, typo fixes

* [Refactored itest to better manage contexts inside integration tests](https://github.com/lightningnetwork/lnd/pull/5756).

# Contributors (Alphabetical Order)

* Daniel McNally
* ErikEk
* Torkel Rogstad
