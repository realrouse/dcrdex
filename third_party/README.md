# third_party

## lbcd

`third_party/lbcd` is a copy of [lbryio/lbcd](https://github.com/lbryio/lbcd)
at commit `a0ff51b` (tag `v0.22.119`) with one small local patch applied
(`lbcd-pebble-v1-iter.patch`, +17/-5 in 3 files):

- `claimtrie/block/blockrepo/pebble.go`
- `claimtrie/node/noderepo/pebble.go`
- `claimtrie/temporal/temporalrepo/pebble.go`

Why: go-ethereum (ETH/EVM assets) requires `github.com/cockroachdb/pebble`
v1, where `DB.NewIter` returns `(*Iterator, error)`. Upstream lbcd still uses
the old one-value API, so the claimtrie packages (pulled in via neutrino ->
lbcd/blockchain) do not compile next to go-ethereum. Go modules can only pick
one pebble version, so the patched lbcd is used via

    replace github.com/lbryio/lbcd => ./third_party/lbcd

in the root `go.mod`. This lets a fresh clone build without a sibling
`../lbcd` checkout. This is not a fork of lbcd; it is only a build fix.

Removed from the copy (not needed to build): `.git`, `.github`, `docs`,
Dockerfiles, goreleaser/golangci configs, `goclean.sh`, `.gitignore`.

When lbcd upstream supports pebble v1, delete this directory and the replace.
