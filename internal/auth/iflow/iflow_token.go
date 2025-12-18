package iflow

import (
	"encoding/json"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/securefile"
)

// IFlowTokenStorage persists iFlow OAuth credentials alongside the derived API key.
type IFlowTokenStorage struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	LastRefresh  string `json:"last_refresh"`
	Expire       string `json:"expired"`
	APIKey       string `json:"api_key"`
	Email        string `json:"email"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	Cookie       string `json:"cookie"`
	Type         string `json:"type"`
}

// SaveTokenToFile serialises the token storage to disk.
func (ts *IFlowTokenStorage) SaveTokenToFile(authFilePath string) error {
	misc.LogSavingCredentials(authFilePath)
	ts.Type = "iflow"
	data, err := json.Marshal(ts)
	if err != nil {
		return fmt.Errorf("iflow token: marshal token failed: %w", err)
	}
	if err := securefile.WriteAuthJSONFile(authFilePath, data); err != nil {
		return fmt.Errorf("iflow token: write token failed: %w", err)
	}
	return nil
}
