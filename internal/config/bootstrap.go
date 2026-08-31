package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// BootstrapSecretFile creates the single secret xconfig when it is absent and
// generates durable client tokens missing from it. It never reads another
// credential source.
func BootstrapSecretFile(publicPath, secretPath string) (bool, error) {
	publicConfig, err := LoadPublicFile(publicPath)
	if err != nil {
		return false, err
	}
	secrets := SecretConfig{
		Version:         1,
		ClientTokens:    make(map[string]string, len(publicConfig.Clients)),
		ProviderAPIKeys: make(map[string]string),
	}
	changed := false
	if raw, readErr := os.ReadFile(secretPath); readErr == nil {
		decoder := yaml.NewDecoder(strings.NewReader(string(raw)))
		decoder.KnownFields(true)
		if err := decoder.Decode(&secrets); err != nil {
			return false, fmt.Errorf("decode existing secret xconfig: %w", err)
		}
		if secrets.ClientTokens == nil {
			secrets.ClientTokens = map[string]string{}
		}
		if secrets.ProviderAPIKeys == nil {
			secrets.ProviderAPIKeys = map[string]string{}
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return false, readErr
	} else {
		changed = true
	}
	for _, client := range publicConfig.Clients {
		if secrets.ClientTokens[client.ID] != "" {
			continue
		}
		token, tokenErr := randomToken()
		if tokenErr != nil {
			return false, tokenErr
		}
		secrets.ClientTokens[client.ID] = token
		changed = true
	}
	if !changed {
		return false, nil
	}
	secretRaw, err := marshalSecrets(secrets)
	if err != nil {
		return false, err
	}
	if err := writeAtomic(secretPath, secretRaw, 0o600, 0o700); err != nil {
		return false, fmt.Errorf("bootstrap secret xconfig: %w", err)
	}
	return true, nil
}

func randomToken() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}
