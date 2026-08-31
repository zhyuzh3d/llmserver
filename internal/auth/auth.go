package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"strings"

	"github.com/zhyuzh3d/llmserver/internal/config"
)

type Client struct {
	ID                 string
	AllowedDeployments map[string]struct{}
	tokenHash          [sha256.Size]byte
}

func (c *Client) Allows(deploymentID string) bool {
	_, ok := c.AllowedDeployments[deploymentID]
	return ok
}

type Authenticator struct {
	clients []*Client
}

func FromConfig(clients []config.ClientConfig) (*Authenticator, error) {
	result := &Authenticator{}
	for _, clientConfig := range clients {
		token := clientConfig.Token.Reveal()
		result.clients = append(result.clients, NewClient(clientConfig.ID, token, clientConfig.AllowedDeployments))
	}
	return result, nil
}

func NewClient(id, token string, allowedDeployments []string) *Client {
	allowed := make(map[string]struct{}, len(allowedDeployments))
	for _, deploymentID := range allowedDeployments {
		allowed[deploymentID] = struct{}{}
	}
	return &Client{
		ID:                 id,
		AllowedDeployments: allowed,
		tokenHash:          sha256.Sum256([]byte(token)),
	}
}

func New(clients ...*Client) *Authenticator {
	return &Authenticator{clients: append([]*Client(nil), clients...)}
}

func (a *Authenticator) AuthenticateAuthorization(value string) (*Client, error) {
	const prefix = "Bearer "
	if !strings.HasPrefix(value, prefix) {
		return nil, errors.New("missing bearer token")
	}
	token := strings.TrimSpace(strings.TrimPrefix(value, prefix))
	if token == "" {
		return nil, errors.New("missing bearer token")
	}
	candidate := sha256.Sum256([]byte(token))
	for _, client := range a.clients {
		if subtle.ConstantTimeCompare(candidate[:], client.tokenHash[:]) == 1 {
			return client, nil
		}
	}
	return nil, errors.New("invalid bearer token")
}
