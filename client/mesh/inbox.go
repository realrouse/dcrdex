// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

// Encrypted per-peer inbox over mesh pub/sub.
//
// bisoncraft/mesh master has no client-to-client send API in its client
// library, so peers talk through a per-peer topic "inbox.<peerID>". Anybody can
// subscribe to any topic, so every message is end-to-end encrypted and signed:
//
//   - Encryption follows the #3204 tankagram scheme (secp256k1 ECDH, key =
//     sha256(shared secret), AES-256-GCM with a random 12-byte nonce prefix),
//     adapted to one-way pub/sub: the sender uses a fresh ephemeral key per
//     message against the recipient's static inbox key (ECIES style). The
//     recipient's static key comes from its signed order (maker) or signed
//     proposal (taker).
//   - The plaintext is signed with the sender's ed25519 mesh identity key. The
//     sender is the mesh-authenticated publisher and the recipient is the
//     inbox owner; both, plus the ephemeral key, are GCM additional data.

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	mc "github.com/bisoncraft/mesh/client"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/libp2p/go-libp2p/core/peer"
)

const (
	// InboxTopicPrefix is the prefix of per-peer inbox topics.
	InboxTopicPrefix = "inbox."

	inboxAADDomain = "tatanka-inbox-v1"
	inboxSigDomain = "tatanka-inbox-msg-v1"
	gcmNonceSize   = 12
	// MaxInboxMsgAge bounds replay of old inbox messages.
	MaxInboxMsgAge = 5 * time.Minute
)

var (
	errRateLimited = errors.New("inbox rate limit exceeded")
	errReplayed    = errors.New("replayed inbox message")
)

// InboxTopic is the inbox topic for a peer.
func InboxTopic(pid peer.ID) string { return InboxTopicPrefix + pid.String() }

// Wire format (bisoncraft/mesh limits a whole publish to codec.MaxMessageSize,
// 1 KiB, so this is compact binary):
//
//	data      = version(1) | ephemeral pubkey(33) | AES-GCM(nonce(12) | ct | tag)
//	plaintext = sig(64) | stamp(8) | len(type)(1) | type | payload(JSON)
//
// The recipient is implied by the topic and the sender is the mesh-authenticated
// publisher; both are bound in as GCM additional data and in the signature. The
// sender's ed25519 public key is extracted from its peer ID.
const inboxVersion = 1

type inboxMsg struct {
	Type    string
	From    peer.ID
	To      peer.ID
	Stamp   int64
	Payload json.RawMessage
	Sig     []byte
}

func inboxAAD(to, from peer.ID, ephPub []byte) []byte {
	b := new(bytes.Buffer)
	b.WriteString(inboxAADDomain)
	writeStr(b, to.String())
	writeStr(b, from.String())
	b.Write(ephPub)
	return b.Bytes()
}

func (m *inboxMsg) serialize() []byte {
	b := new(bytes.Buffer)
	b.WriteString(inboxSigDomain)
	writeStr(b, m.Type)
	writeStr(b, m.From.String())
	writeStr(b, m.To.String())
	_ = binary.Write(b, binary.BigEndian, m.Stamp)
	_ = binary.Write(b, binary.BigEndian, uint32(len(m.Payload)))
	b.Write(m.Payload)
	return b.Bytes()
}

func writeStr(b *bytes.Buffer, s string) {
	_ = binary.Write(b, binary.BigEndian, uint16(len(s)))
	b.WriteString(s)
}

func inboxKey(priv *secp256k1.PrivateKey, pub *secp256k1.PublicKey) []byte {
	k := sha256.Sum256(secp256k1.GenerateSharedSecret(priv, pub))
	return k[:]
}

func sealAES(key, plaintext, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcmNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, aad), nil
}

func openAES(key, ct, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(ct) < gcmNonceSize {
		return nil, errors.New("ciphertext too short")
	}
	return gcm.Open(nil, ct[:gcmNonceSize], ct[gcmNonceSize:], aad)
}

// sealInbox signs and encrypts a message of type typ to peer "to" whose static
// inbox key is toEnc.
func (ob *OrderBooks) sealInbox(to peer.ID, toEnc *secp256k1.PublicKey, typ string, payload any) ([]byte, error) {
	if len(typ) > 255 {
		return nil, errors.New("type too long")
	}
	pl, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	msg := &inboxMsg{Type: typ, From: ob.self, To: to, Stamp: ob.now().UnixMilli(), Payload: pl}
	if msg.Sig, err = ob.priv.Sign(msg.serialize()); err != nil {
		return nil, err
	}
	if len(msg.Sig) != 64 {
		return nil, fmt.Errorf("unexpected signature length %d", len(msg.Sig))
	}
	plain := make([]byte, 0, 64+8+1+len(typ)+len(pl))
	plain = append(plain, msg.Sig...)
	plain = binary.BigEndian.AppendUint64(plain, uint64(msg.Stamp))
	plain = append(plain, byte(len(typ)))
	plain = append(plain, typ...)
	plain = append(plain, pl...)

	eph, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		return nil, err
	}
	ephPub := eph.PubKey().SerializeCompressed()
	ct, err := sealAES(inboxKey(eph, toEnc), plain, inboxAAD(to, ob.self, ephPub))
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 1+len(ephPub)+len(ct))
	out = append(out, inboxVersion)
	out = append(out, ephPub...)
	return append(out, ct...), nil
}

// openInbox decrypts and authenticates an inbox message published to our
// inbox by sender.
func (ob *OrderBooks) openInbox(data []byte, sender peer.ID) (*inboxMsg, error) {
	if sender == "" {
		return nil, errors.New("unknown sender")
	}
	if len(data) < 1+33+gcmNonceSize+16 || data[0] != inboxVersion {
		return nil, errors.New("bad inbox envelope")
	}
	ephB := data[1:34]
	ephPub, err := secp256k1.ParsePubKey(ephB)
	if err != nil {
		return nil, fmt.Errorf("bad ephemeral key: %w", err)
	}
	plain, err := openAES(inboxKey(ob.encPriv, ephPub), data[34:], inboxAAD(ob.self, sender, ephB))
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	if len(plain) < 64+8+1 {
		return nil, errors.New("short plaintext")
	}
	typeLen := int(plain[72])
	if len(plain) < 73+typeLen {
		return nil, errors.New("short plaintext")
	}
	msg := &inboxMsg{
		Sig:     plain[:64],
		Stamp:   int64(binary.BigEndian.Uint64(plain[64:72])),
		Type:    string(plain[73 : 73+typeLen]),
		Payload: plain[73+typeLen:],
		From:    sender,
		To:      ob.self,
	}
	pub, err := sender.ExtractPublicKey()
	if err != nil {
		return nil, fmt.Errorf("sender key: %w", err)
	}
	if ok, err := pub.Verify(msg.serialize(), msg.Sig); err != nil || !ok {
		return nil, errors.New("bad signature")
	}
	now := ob.now()
	if msg.Stamp > now.Add(MaxClockSkew).UnixMilli() || msg.Stamp < now.Add(-MaxInboxMsgAge).UnixMilli() {
		return nil, errors.New("inbox message stamp out of range")
	}
	return msg, nil
}

func (ob *OrderBooks) sendInbox(ctx context.Context, to peer.ID, toEnc *secp256k1.PublicKey, typ string, payload any) {
	b, err := ob.sealInbox(to, toEnc, typ, payload)
	if err != nil {
		ob.log.Errorf("Error sealing %s for %s: %v", typ, to, err)
		return
	}
	ob.log.Tracef("Sending %s (%d bytes) to %s", typ, len(b), to)
	if err := ob.ps.Broadcast(ctx, InboxTopic(to), b); err != nil {
		ob.log.Warnf("Error sending %s to %s: %v", typ, to, err)
	}
}

// SubscribeInbox subscribes to our own inbox topic.
func (ob *OrderBooks) SubscribeInbox(ctx context.Context) error {
	err := ob.ps.Subscribe(ctx, InboxTopic(ob.self), func(ev mc.TopicEvent) {
		if ev.Type != mc.TopicEventData {
			return
		}
		msg, err := ob.receiveInbox(ev.Data, ev.Peer)
		if err != nil {
			ob.log.Debugf("Dropped inbox message from %s: %v", ev.Peer, err)
			return
		}
		ob.handleInboxMsg(ctx, msg)
	})
	if err != nil && !errors.Is(err, mc.ErrRedundantSubscription) {
		return err
	}
	ob.log.Infof("Subscribed to inbox %s", InboxTopic(ob.self))
	return nil
}
