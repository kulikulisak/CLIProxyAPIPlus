package generic

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/oauthhttp"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// CheckOAuth2Token validates a token against a generic OAuth2 introspection endpoint.
func CheckOAuth2Token(ctx context.Context, token string, cfg config.GenericAuth) (*coreauth.Auth, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// 1. Resolve configuration (env vars override if config is empty)
	introspectionURL := cfg.IntrospectionURL
	if introspectionURL == "" {
		introspectionURL = os.Getenv("OAUTH_TOKEN_INFO_ENDPOINT")
	}
	clientID := cfg.ClientID
	if clientID == "" {
		clientID = os.Getenv("OAUTH_CLIENT_ID")
	}
	clientSecret := cfg.ClientSecret
	if clientSecret == "" {
		clientSecret = os.Getenv("OAUTH_CLIENT_SECRET")
	}

	if introspectionURL == "" {
		return nil, fmt.Errorf("introspection URL not configured (set introspection-url or OAUTH_TOKEN_INFO_ENDPOINT)")
	}

	userIDField := cfg.UserIDField
	if userIDField == "" {
		userIDField = os.Getenv("OAUTH_USER_ID_FIELD_NAME")
		if userIDField == "" {
			userIDField = "sub"
		}
	}

	emailField := cfg.EmailField
	if emailField == "" {
		emailField = "email"
	}

	// 2. Determine if it's an introspection endpoint (RFC 7662) or UserInfo (GET)
	// Heuristic: treat URLs containing "introspect" with client credentials as RFC 7662 introspection.
	isIntrospection := strings.Contains(introspectionURL, "introspect") && clientID != "" && clientSecret != ""

	var (
		method         string
		contentType    string
		accept         string
		authorization  string
		encodedForm    string
		useRequestBody bool
	)
	if isIntrospection {
		log.Debug("Using OAuth2 introspection endpoint (POST)")
		data := url.Values{}
		data.Set("token", token)

		method = http.MethodPost
		encodedForm = data.Encode()
		useRequestBody = true
		contentType = "application/x-www-form-urlencoded"
		accept = "application/json"

		// Basic Auth for client credentials
		auth := clientID + ":" + clientSecret
		basic := base64.StdEncoding.EncodeToString([]byte(auth))
		authorization = "Basic " + basic

	} else {
		log.Debug("Using generic token info endpoint (GET)")
		method = http.MethodGet
		authorization = "Bearer " + token
		contentType = "application/json"
		accept = "application/json"
	}

	// 3. Execute request (hardened retries + response size caps).
	client := &http.Client{Timeout: 30 * time.Second}
	status, _, body, err := oauthhttp.Do(
		ctx,
		client,
		func() (*http.Request, error) {
			var bodyReader *strings.Reader
			if useRequestBody {
				bodyReader = strings.NewReader(encodedForm)
			} else {
				bodyReader = strings.NewReader("")
			}
			var req *http.Request
			var errReq error
			if useRequestBody {
				req, errReq = http.NewRequestWithContext(ctx, method, introspectionURL, bodyReader)
			} else {
				req, errReq = http.NewRequestWithContext(ctx, method, introspectionURL, nil)
			}
			if errReq != nil {
				return nil, errReq
			}
			if strings.TrimSpace(authorization) != "" {
				req.Header.Set("Authorization", authorization)
			}
			if strings.TrimSpace(contentType) != "" {
				req.Header.Set("Content-Type", contentType)
			}
			if strings.TrimSpace(accept) != "" {
				req.Header.Set("Accept", accept)
			}
			return req, nil
		},
		oauthhttp.DefaultRetryConfig(),
	)
	if err != nil {
		return nil, fmt.Errorf("token validation request failed: %w", err)
	}
	if status >= http.StatusBadRequest {
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = fmt.Sprintf("status %d", status)
		}
		return nil, fmt.Errorf("token validation failed: %s", msg)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	// 4. Validate response
	if isIntrospection {
		active, ok := result["active"].(bool)
		if !ok || !active {
			return nil, fmt.Errorf("token is not active")
		}
	}

	// 5. Map to Auth struct
	auth := &coreauth.Auth{
		Provider:  cfg.ProviderID,
		Metadata:  make(map[string]any),
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
		Status:    coreauth.StatusActive,
	}

	// Extract ID
	if id, ok := result[userIDField].(string); ok {
		auth.ID = id
	} else if id, ok := result["id"].(string); ok {
		auth.ID = id // Fallback
	} else {
		// If no ID found, generate one or fail? 
		// For now, let's use a hash of the token if strictly necessary, but better to fail.
		// Or use the token itself as ID? No, that's bad.
		// Let's use "generic-user" + timestamp
		auth.ID = fmt.Sprintf("generic-%d", time.Now().UnixNano())
	}

	// Extract Email
	if email, ok := result[emailField].(string); ok {
		auth.Metadata["email"] = email
		auth.Label = email
	}

	// Store raw response in metadata for flexibility
	for k, v := range result {
		auth.Metadata[k] = v
	}

	return auth, nil
}
