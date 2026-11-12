package types

// preservedCiphertext records the exact at-rest ciphertext of secret fields
// whose load-path decryption failed (SYSTEM_AES_KEY missing or rotated).
//
// The lenient Scan hooks blank such fields so a row stays listable, but a
// later full-row save would then overwrite the still-recoverable ciphertext
// with an empty value. Value() restores the recorded ciphertext while the
// exported field is still empty, so unrelated updates cannot destroy it. A
// field the user re-entered is non-empty and always wins.
type preservedCiphertext map[string]string

func newPreservedCiphertext() preservedCiphertext {
	return preservedCiphertext{}
}

func (p preservedCiphertext) remember(key, ciphertext string) {
	if p == nil || ciphertext == "" {
		return
	}
	p[key] = ciphertext
}

func (p preservedCiphertext) restore(key string, value *string) {
	if p == nil || value == nil || *value != "" {
		return
	}
	if ciphertext := p[key]; ciphertext != "" {
		*value = ciphertext
	}
}
