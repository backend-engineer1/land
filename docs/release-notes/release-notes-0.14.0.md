# Release Notes

# Build System

* [A new pre-submit check has been
  added](https://github.com/lightningnetwork/lnd/pull/5520) to ensure that all
  PRs ([aside from merge
  commits](https://github.com/lightningnetwork/lnd/pull/5543)) add an entry in
  the release notes folder that at leasts links to PR being added.

* [A new build target itest-race](https://github.com/lightningnetwork/lnd/pull/5542) 
  to help uncover undetected data races with our itests.

# Code Health

## Code cleanup, refactor, typo fixes

* [Unused error check 
  removed](https://github.com/lightningnetwork/lnd/pull/5537).
* [Shorten Pull Request check list by referring to the CI checks that are 
  in place](https://github.com/lightningnetwork/lnd/pull/5545).
* [Added minor fixes to contribution guidelines](https://github.com/lightningnetwork/lnd/pull/5503).

## Database

* [Ensure single writer for legacy
  code](https://github.com/lightningnetwork/lnd/pull/5547) when using etcd
  backend.

# Contributors (Alphabetical Order)
* ErikEk
* Zero-1729
