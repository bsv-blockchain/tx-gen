package txbuilder

import (
	"strings"

	"github.com/bsv-blockchain/go-sdk/script"
)

// lockScriptTagBytes is the length of the instance tag pushed by the
// tagged-drop lock script.
const lockScriptTagBytes = 6

// TaggedDropLockScript returns OP_PUSHBYTES_6 <tag> OP_DROP, where tag is
// instanceID normalized to exactly six UTF-8 bytes (see lockScriptTag).
//
// Every instance therefore gets its own script hash, which keeps the UTXOs
// of separate deployments apart on WhatsOnChain.
func TaggedDropLockScript(instanceID string) (*script.Script, error) {
	tag, err := lockScriptTag(instanceID)
	if err != nil {
		return nil, err
	}
	s := make(script.Script, 0, lockScriptTagBytes+2)
	s = append(s, byte(lockScriptTagBytes))
	s = append(s, tag[:]...)
	s = append(s, script.OpDROP)
	return &s, nil
}

// lockScriptTag normalizes instanceID to exactly six UTF-8 bytes.
//
// Surrounding whitespace is trimmed. Whole runes are copied while they fit,
// so a multi-byte character is never split, and any remaining bytes are padded
// with spaces.
func lockScriptTag(instanceID string) ([lockScriptTagBytes]byte, error) {
	var tag [lockScriptTagBytes]byte
	for i := range tag {
		tag[i] = ' '
	}

	value := strings.TrimSpace(instanceID)
	if value == "" {
		return tag, ErrInstanceIDRequired
	}

	offset := 0
	for _, r := range value {
		encoded := []byte(string(r))
		if offset+len(encoded) > len(tag) {
			break
		}
		copy(tag[offset:], encoded)
		offset += len(encoded)
		if offset == len(tag) {
			break
		}
	}
	return tag, nil
}

// opTrueScript returns the tagged-drop unlocking script: a single OP_TRUE (0x51).
func opTrueScript() *script.Script {
	s := script.Script{script.OpTRUE}
	return &s
}
