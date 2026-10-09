package crypto

import "errors"

// EncryptRequired reuses the configured storage cipher, but never falls back to
// plaintext. SMTP credentials have no legacy plaintext storage format.
func EncryptRequired(plaintext string) (string, error) {
	if globalCryptoService == nil || plaintext == "" || isEncryptedStorageValue(plaintext) {
		return "", errors.New("credential encryption unavailable or invalid input")
	}
	result, err := globalCryptoService.EncryptForStorage(plaintext, "smtp")
	if err != nil {
		return "", errors.New("credential encryption failed")
	}
	return result, nil
}

func DecryptRequired(ciphertext string) (string, error) {
	if globalCryptoService == nil || !isEncryptedStorageValue(ciphertext) {
		return "", errors.New("credential decryption unavailable or invalid storage")
	}
	result, err := globalCryptoService.DecryptFromStorage(ciphertext, "smtp")
	if err != nil || result == "" {
		return "", errors.New("credential decryption failed")
	}
	return result, nil
}
