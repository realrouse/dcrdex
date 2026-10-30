// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package core

import "decred.org/dcrdex/dex"

// dex.revivel.app TLS cert (dcrdex rpc.cert). Public pin only; never commit rpc.key.
// Official dex.decred.org is not pinned here: this client speaks PerMatchAddr
// protocol and cannot trade on the older public DEX.
var dexRevivelCert = []byte(`-----BEGIN CERTIFICATE-----
MIIDEzCCAnSgAwIBAgIRAJciP+UrshMjU8A7sP+7fNgwCgYIKoZIzj0EAwQwRDEi
MCAGA1UEChMZZGNyZGV4IGF1dG9nZW5lcmF0ZWQgY2VydDEeMBwGA1UEAxMVNGds
MS5sLnRpbWU0dnBzLmNsb3VkMB4XDTI2MDgyMzE3MjYzMVoXDTM2MDgyMTE3MjYz
MVowRDEiMCAGA1UEChMZZGNyZGV4IGF1dG9nZW5lcmF0ZWQgY2VydDEeMBwGA1UE
AxMVNGdsMS5sLnRpbWU0dnBzLmNsb3VkMIGbMBAGByqGSM49AgEGBSuBBAAjA4GG
AAQAg65Nzqe3mfjt3r36SPmg0F40pMvPI01J1u/OZJFhJntAmgqN4bT82DGoZXez
UWqP78J1CWiehuOdqVhdEurxGHEBHQOYfTFavRm1sR75IEgzrmFD8b54n/2eF8Tp
JxAh3kcYkRICsp7jrpuVnDWUeou521QLJkbqEb8RtvL1zBOOL/KjggECMIH/MA4G
A1UdDwEB/wQEAwICpDAPBgNVHRMBAf8EBTADAQH/MB0GA1UdDgQWBBR/0UfKsOkH
ovMBf92iw0A5mTFSxTCBvAYDVR0RBIG0MIGxghU0Z2wxLmwudGltZTR2cHMuY2xv
dWSCCWxvY2FsaG9zdIIPZGV4LnJldml2ZWwuYXBwhwR/AAABhxAAAAAAAAAAAAAA
AAAAAAABhwRZKACThwQKKACThwSsEQABhwSsEgABhwQKCAABhxAqAntAWSgAkwAA
AAAAAAABhxD+gAAAAAAAAAIAWf/+KACThxD+gAAAAAAAACQBLv/+K+LRhxD+gAAA
AAAAAGQDFv/+MSfmMAoGCCqGSM49BAMEA4GMADCBiAJCAfXwOOhayBxYw1Ia4YKt
vB6mVrI3StPcQ0s9OIWL2jK3nTrLzGQgBcH1N+Itcpw2DGW2AIUaa5z0kjgVRLqr
hiRRAkIBpnSNvysXU5LkwSMB/aODtbkit8pnUkG4BrWGRslVxnMGyYuyxu2jzWwB
ZDjG5XLjndXy93xm48IiOClxxmnNFmk=
-----END CERTIFICATE-----
`)

var simnetHarnessCert = []byte(`-----BEGIN CERTIFICATE-----
MIICpTCCAgagAwIBAgIQZMfxMkSi24xMr4CClCODrzAKBggqhkjOPQQDBDBJMSIw
IAYDVQQKExlkY3JkZXggYXV0b2dlbmVyYXRlZCBjZXJ0MSMwIQYDVQQDExp1YnVu
dHUtcy0xdmNwdS0yZ2ItbG9uMS0wMTAeFw0yMDA2MDgxMjM4MjNaFw0zMDA2MDcx
MjM4MjNaMEkxIjAgBgNVBAoTGWRjcmRleCBhdXRvZ2VuZXJhdGVkIGNlcnQxIzAh
BgNVBAMTGnVidW50dS1zLTF2Y3B1LTJnYi1sb24xLTAxMIGbMBAGByqGSM49AgEG
BSuBBAAjA4GGAAQApXJpVD7si8yxoITESq+xaXWtEpsCWU7X+8isRDj1cFfH53K6
/XNvn3G+Yq0L22Q8pMozGukA7KuCQAAL0xnuo10AecWBN0Zo2BLHvpwKkmAs71C+
5BITJksqFxvjwyMKbo3L/5x8S/JmAWrZoepBLfQ7HcoPqLAcg0XoIgJjOyFZgc+j
gYwwgYkwDgYDVR0PAQH/BAQDAgKkMA8GA1UdEwEB/wQFMAMBAf8wZgYDVR0RBF8w
XYIadWJ1bnR1LXMtMXZjcHUtMmdiLWxvbjEtMDGCCWxvY2FsaG9zdIcEfwAAAYcQ
AAAAAAAAAAAAAAAAAAAAAYcEsj5QQYcEChAABYcQ/oAAAAAAAAAYPqf//vUPXDAK
BggqhkjOPQQDBAOBjAAwgYgCQgFMEhyTXnT8phDJAnzLbYRktg7rTAbTuQRDp1PE
jf6b2Df4DkSX7JPXvVi3NeBru+mnrOkHBUMqZd0m036aC4q/ZAJCASa+olu4Isx7
8JE3XB6kGr+s48eIFPtmq1D0gOvRr3yMHrhJe3XDNqvppcHihG0qNb0gyaiX18Cv
vF8Ti1x2vTkD
-----END CERTIFICATE-----
`)

var CertStore = map[dex.Network]map[string][]byte{
	dex.Mainnet: {
		"dex.revivel.app:7232": dexRevivelCert,
	},
	dex.Testnet: {
		"bison.exchange:17232": nil, // Uses certificate authority
	},
	dex.Simnet: {
		"127.0.0.1:17273": simnetHarnessCert,
	},
}
