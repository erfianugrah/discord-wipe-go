// Package discord provides a Discord HTTP API client for the wipe tool.
//
// It handles auth, rate limiting (header-driven bucket pacing), retry on
// transient network errors, and the three-layer safety guarantee (only-my-
// messages: author-filtered search, export reads only our messages, 403 is
// terminal).
package discord

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	API          = "https://discord.com/api/v10"
	UserAgent    = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	DefaultRetry = 8
	RetryBase    = 2.0
	RetryCap     = 30.0
)

// Client is a Discord HTTP client bound to a user token.
type Client struct {
	token   string
	baseURL string // defaults to API; overridable in tests via httptest
	client  *http.Client
	log     *slog.Logger
}

// NewClient creates a Discord client for the given token.
func NewClient(token string) *Client {
	return &Client{
		token:   token,
		baseURL: API,
		client: &http.Client{
			Timeout: 60 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:    10,
				IdleConnTimeout: 90 * time.Second,
			},
		},
		log: slog.Default(),
	}
}

// ---------------------------------------------------------------------------
// Auth / identity
// ---------------------------------------------------------------------------

// AuthError is returned when Discord rejects the token (401).
type AuthError struct {
	Message string
	Body    string
}

func (e *AuthError) Error() string {
	return fmt.Sprintf("Discord rejected the token (401): %s", e.Message)
}

// User represents a Discord user as returned by /users/@me.
type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

// GetMe returns the authenticated user.
func (c *Client) GetMe() (*User, error) {
	resp, err := c.do("GET", c.baseURL+"/users/@me", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 401 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return nil, &AuthError{Message: string(body), Body: string(body)}
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return nil, fmt.Errorf("get me: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var u User
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return nil, fmt.Errorf("decode user: %w", err)
	}
	return &u, nil
}

// ---------------------------------------------------------------------------
// Guilds
// ---------------------------------------------------------------------------

// Guild is a minimal Discord guild representation.
type Guild struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ListGuilds returns all guilds the user is a member of.
func (c *Client) ListGuilds() ([]Guild, error) {
	resp, err := c.do("GET", c.baseURL+"/users/@me/guilds", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return nil, &AuthError{Message: string(body)}
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return nil, fmt.Errorf("list guilds: HTTP %d: %s", resp.StatusCode, string(body))
	}
	var gs []Guild
	if err := json.NewDecoder(resp.Body).Decode(&gs); err != nil {
		return nil, fmt.Errorf("decode guilds: %w", err)
	}
	return gs, nil
}

// GetGuild fetches a single guild by ID.
func (c *Client) GetGuild(guildID string) (*Guild, error) {
	resp, err := c.do("GET", c.baseURL+"/guilds/"+guildID, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("get guild %s: HTTP %d", guildID, resp.StatusCode)
	}
	var g Guild
	if err := json.NewDecoder(resp.Body).Decode(&g); err != nil {
		return nil, fmt.Errorf("decode guild: %w", err)
	}
	return &g, nil
}

// Channel represents a Discord channel within a guild.
type Channel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type int    `json:"type"`
}

// ListGuildChannels returns all channels in a guild.
func (c *Client) ListGuildChannels(guildID string) ([]Channel, error) {
	resp, err := c.do("GET", c.baseURL+"/guilds/"+guildID+"/channels", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("list channels for %s: HTTP %d", guildID, resp.StatusCode)
	}
	var chs []Channel
	if err := json.NewDecoder(resp.Body).Decode(&chs); err != nil {
		return nil, fmt.Errorf("decode channels: %w", err)
	}
	return chs, nil
}

// ---------------------------------------------------------------------------
// DMs
// ---------------------------------------------------------------------------

// DMChannel is a DM or group DM channel.
type DMChannel struct {
	ID         string        `json:"id"`
	Type       int           `json:"type"` // 1=DM, 3=GROUP_DM
	Recipients []DMRecipient `json:"recipients"`
}

// DMRecipient is a user in a DM channel.
type DMRecipient struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

// ListDMs returns all open DM channels.
func (c *Client) ListDMs() ([]DMChannel, error) {
	resp, err := c.do("GET", c.baseURL+"/users/@me/channels", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return nil, &AuthError{Message: string(body)}
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return nil, fmt.Errorf("list DMs: HTTP %d: %s", resp.StatusCode, string(body))
	}
	var dms []DMChannel
	if err := json.NewDecoder(resp.Body).Decode(&dms); err != nil {
		return nil, fmt.Errorf("decode DMs: %w", err)
	}
	return dms, nil
}

// ---------------------------------------------------------------------------
// Message search
// ---------------------------------------------------------------------------

// SearchResult holds a single message from the search endpoint.
type SearchResult struct {
	ID        string `json:"id"`
	ChannelID string `json:"channel_id"`
	Content   string `json:"content"`
}

// SearchResponse is the parsed response from the messages/search endpoint.
type SearchResponse struct {
	TotalResults int            `json:"total_results"`
	Messages     []SearchResult `json:"messages"` // parsed by us from the nested format
}

// SearchParams holds optional filters for the search endpoint.
type SearchParams struct {
	AuthorID  string
	MaxID     int64
	MinID     int64
	Content   string
	ChannelID string // only for guild-scoped search
	Offset    int
}

// SearchMessages searches for messages in a guild or channel scope.
//
// Returns (total_results, hits, retry_after_seconds).
// On 429, retry_after is the wait time. On 403/404, total_results is -1.
func (c *Client) SearchMessages(scope string, scopeID string, params SearchParams) (int, []SearchResult, float64, error) {
	var url string
	if scope == "guild" {
		url = fmt.Sprintf("%s/guilds/%s/messages/search", c.baseURL, scopeID)
	} else {
		url = fmt.Sprintf("%s/channels/%s/messages/search", c.baseURL, scopeID)
	}

	q := make(map[string]string)
	q["author_id"] = params.AuthorID
	q["offset"] = strconv.Itoa(params.Offset)
	q["include_nsfw"] = "true"
	if params.MaxID > 0 {
		q["max_id"] = strconv.FormatInt(params.MaxID, 10)
	}
	if params.MinID > 0 {
		q["min_id"] = strconv.FormatInt(params.MinID, 10)
	}
	if params.Content != "" {
		q["content"] = params.Content
	}
	if params.ChannelID != "" && scope == "guild" {
		q["channel_id"] = params.ChannelID
	}

	resp, err := c.do("GET", url, q)
	if err != nil {
		c.log.Error("search request failed", "err", err)
		return 0, nil, 0, nil
	}
	defer resp.Body.Close()

	// 401 is terminal: the token was rejected. Return a typed error so the
	// caller can park/exit instead of crashing. (Previously this panicked
	// with no recover() anywhere — a mid-pass token rotation killed the
	// daemon outright.)
	if resp.StatusCode == 401 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return 0, nil, 0, &AuthError{Message: string(body)}
	}

	if resp.StatusCode == 429 {
		retry := parseRetryAfter(resp)
		return 0, nil, retry, nil
	}
	if resp.StatusCode == 202 {
		retry := parseRetryAfter(resp)
		if retry == 0 {
			retry = 5
		}
		return 0, nil, retry, nil
	}
	if resp.StatusCode == 403 || resp.StatusCode == 404 {
		return -1, nil, 0, nil
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		c.log.Error("search HTTP error", "status", resp.StatusCode, "body", string(body))
		return 0, nil, 0, nil
	}

	// Discord's search response nests messages in groups:
	// {"messages": [[{...}, {...}], [{...}]]}
	var raw struct {
		TotalResults int `json:"total_results"`
		Messages     [][]struct {
			Hit       bool   `json:"hit"`
			ID        string `json:"id"`
			ChannelID string `json:"channel_id"`
			Content   string `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		c.log.Error("decode search response", "err", err)
		return 0, nil, 0, nil
	}

	var hits []SearchResult
	for _, group := range raw.Messages {
		for _, m := range group {
			if m.Hit {
				hits = append(hits, SearchResult{
					ID:        m.ID,
					ChannelID: m.ChannelID,
					Content:   m.Content,
				})
			}
		}
	}
	return raw.TotalResults, hits, 0, nil
}

// ---------------------------------------------------------------------------
// Message deletion
// ---------------------------------------------------------------------------

// DeleteResult is the outcome of a delete operation.
type DeleteResult struct {
	Status     string  // "ok", "gone", "forbidden", "retry", "auth"
	RetryAfter float64 // seconds to wait before next attempt
}

// DeleteMessage deletes one message. Returns the status, a pacing hint, and a
// non-nil error ONLY when the token was rejected (401) — a terminal condition
// the caller must park/exit on rather than retry. 403/400 are non-fatal
// "forbidden" (skipped, never retried — the only-my-messages defence in depth).
func (c *Client) DeleteMessage(channelID, msgID string) (DeleteResult, error) {
	url := fmt.Sprintf("%s/channels/%s/messages/%s", c.baseURL, channelID, msgID)
	resp, err := c.do("DELETE", url, nil)
	if err != nil {
		c.log.Error("delete request failed", "err", err)
		return DeleteResult{Status: "retry", RetryAfter: 5}, nil
	}
	defer resp.Body.Close()

	bucketHint := bucketPacing(resp)

	switch resp.StatusCode {
	case 204:
		return DeleteResult{Status: "ok", RetryAfter: bucketHint}, nil
	case 404:
		return DeleteResult{Status: "gone", RetryAfter: bucketHint}, nil
	case 400:
		// Terminal errors: archived thread, missing access, system message.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		c.log.Debug("delete terminal 400", "channel", channelID, "msg", msgID, "body", string(body))
		return DeleteResult{Status: "forbidden"}, nil
	case 403:
		return DeleteResult{Status: "forbidden"}, nil
	case 429:
		retry := parseRetryAfter(resp)
		if retry == 0 {
			retry = 1
		}
		return DeleteResult{Status: "retry", RetryAfter: retry}, nil
	case 401:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return DeleteResult{Status: "auth"}, &AuthError{Message: string(body)}
	default:
		if resp.StatusCode >= 500 {
			return DeleteResult{Status: "retry", RetryAfter: 5}, nil
		}
		c.log.Warn("unexpected delete response", "status", resp.StatusCode)
		return DeleteResult{Status: "retry", RetryAfter: 2}, nil
	}
}

// ---------------------------------------------------------------------------
// Message fetch (for export/backup)
// ---------------------------------------------------------------------------

// FetchedMessage is a message from GET /channels/{id}/messages.
type FetchedMessage struct {
	ID        string `json:"id"`
	ChannelID string `json:"channel_id"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp"`
	Author    struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"author"`
	Attachments []struct {
		ID       string `json:"id"`
		Filename string `json:"filename"`
		URL      string `json:"url"`
		Size     int    `json:"size"`
	} `json:"attachments"`
}

// FetchMessages gets messages from a channel with pagination.
// beforeID=0 fetches the most recent messages. afterID=0 means no lower bound.
// Returns (messages, hasMore).
func (c *Client) FetchMessages(channelID string, beforeID, afterID int64, limit int) ([]FetchedMessage, bool, error) {
	url := fmt.Sprintf("%s/channels/%s/messages", c.baseURL, channelID)
	q := make(map[string]string)
	q["limit"] = strconv.Itoa(limit)
	if beforeID > 0 {
		q["before"] = strconv.FormatInt(beforeID, 10)
	}
	if afterID > 0 {
		q["after"] = strconv.FormatInt(afterID, 10)
	}

	resp, err := c.do("GET", url, q)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 401 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return nil, false, &AuthError{Message: string(body)}
	}
	if resp.StatusCode == 429 {
		retry := parseRetryAfter(resp)
		if retry == 0 {
			retry = 5
		}
		time.Sleep(time.Duration(retry * float64(time.Second)))
		// Retry the request
		resp2, err := c.do("GET", url, q)
		if err != nil {
			return nil, false, err
		}
		resp.Body.Close()
		resp = resp2
		goto parseBody
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return nil, false, fmt.Errorf("fetch messages: HTTP %d: %s", resp.StatusCode, string(body))
	}

parseBody:
	var msgs []FetchedMessage
	if err := json.NewDecoder(resp.Body).Decode(&msgs); err != nil {
		return nil, false, fmt.Errorf("decode messages: %w", err)
	}
	hasMore := len(msgs) == limit
	return msgs, hasMore, nil
}

// LeaveGuild leaves a guild (DELETE /users/@me/guilds/{id}).
func (c *Client) LeaveGuild(guildID string) error {
	url := fmt.Sprintf("%s/users/@me/guilds/%s", c.baseURL, guildID)
	// Retry on 429
	for attempt := 0; attempt < 3; attempt++ {
		resp, err := c.do("DELETE", url, nil)
		if err != nil {
			return err
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		resp.Body.Close()
		if resp.StatusCode == 204 {
			return nil
		}
		if resp.StatusCode == 429 {
			retry := parseRetryAfter(resp)
			if retry == 0 {
				retry = 5
			}
			time.Sleep(time.Duration(retry * float64(time.Second)))
			continue
		}
		if resp.StatusCode == 401 {
			return &AuthError{Message: string(body)}
		}
		return fmt.Errorf("leave guild %s: HTTP %d: %s", guildID, resp.StatusCode, string(body))
	}
	return fmt.Errorf("leave guild %s: exhausted retries on 429", guildID)
}

// CloseDM closes a DM channel (DELETE /channels/{id}).
func (c *Client) CloseDM(channelID string) error {
	url := fmt.Sprintf("%s/channels/%s", c.baseURL, channelID)
	for attempt := 0; attempt < 3; attempt++ {
		resp, err := c.do("DELETE", url, nil)
		if err != nil {
			return err
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		resp.Body.Close()
		if resp.StatusCode == 200 || resp.StatusCode == 204 {
			return nil
		}
		if resp.StatusCode == 429 {
			retry := parseRetryAfter(resp)
			if retry == 0 {
				retry = 5
			}
			time.Sleep(time.Duration(retry * float64(time.Second)))
			continue
		}
		if resp.StatusCode == 401 {
			return &AuthError{Message: string(body)}
		}
		return fmt.Errorf("close DM %s: HTTP %d: %s", channelID, resp.StatusCode, string(body))
	}
	return fmt.Errorf("close DM %s: exhausted retries on 429", channelID)
}

// ---------------------------------------------------------------------------
// HTTP internals
// ---------------------------------------------------------------------------

func (c *Client) do(method, url string, query map[string]string) (*http.Response, error) {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", c.token)
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "application/json")
	if query != nil {
		q := req.URL.Query()
		for k, v := range query {
			q.Set(k, v)
		}
		req.URL.RawQuery = q.Encode()
	}

	var attempt int
	for {
		attempt++
		resp, err := c.client.Do(req)
		if err != nil {
			if attempt >= DefaultRetry {
				return nil, fmt.Errorf("request failed after %d attempts: %w", attempt, err)
			}
			delay := time.Duration(math.Min(RetryCap, RetryBase*math.Pow(2, float64(attempt-1)))) * time.Second
			c.log.Info("transient network error, retrying",
				"method", method, "url", url, "err", err,
				"attempt", attempt, "delay", delay)
			time.Sleep(delay)
			continue
		}
		return resp, nil
	}
}

// ---------------------------------------------------------------------------
// Rate-limiting helpers
// ---------------------------------------------------------------------------

// bucketPacing returns the optimal per-call delay from Discord's rate-limit
// bucket headers. Returns 0 if no headers are present.
func bucketPacing(resp *http.Response) float64 {
	remS := resp.Header.Get("X-RateLimit-Remaining")
	reset := parseFloatHeader(resp, "X-RateLimit-Reset-After")
	rem, err := strconv.Atoi(remS)
	if err != nil || rem < 0 {
		return 0
	}
	if rem == 0 && reset > 0 {
		return reset
	}
	if rem > 0 && reset > 0 {
		return reset / float64(rem)
	}
	return 0
}

func parseRetryAfter(resp *http.Response) float64 {
	// Try JSON body first (Discord includes retry_after in 429 responses).
	if resp.Body != nil {
		bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 200))
		if err == nil {
			// Re-inject for the caller.
			resp.Body = io.NopCloser(strings.NewReader(string(bodyBytes)))
			var body struct {
				RetryAfter float64 `json:"retry_after"`
			}
			if json.Unmarshal(bodyBytes, &body) == nil && body.RetryAfter > 0 {
				return body.RetryAfter
			}
		}
	}
	return parseFloatHeader(resp, "Retry-After")
}

func parseFloatHeader(resp *http.Response, key string) float64 {
	s := resp.Header.Get(key)
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return f
}
