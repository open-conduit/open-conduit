package chatcompletions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Message is one Chat Completions message as the executor keeps it in a
// Work's conversation: system, user, assistant (text and/or tool calls)
// or tool (one call's result).
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// MarshalJSON leaves out an assistant message's empty content when it
// carries tool calls, which every provider accepts, rather than an empty
// string, which some refuse.
func (m Message) MarshalJSON() ([]byte, error) {
	type wire struct {
		Role       string     `json:"role"`
		Content    *string    `json:"content,omitempty"`
		ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
		ToolCallID string     `json:"tool_call_id,omitempty"`
	}
	w := wire{Role: m.Role, ToolCalls: m.ToolCalls, ToolCallID: m.ToolCallID}
	if m.Content != "" || len(m.ToolCalls) == 0 {
		w.Content = &m.Content
	}
	return json.Marshal(w)
}

// ToolCall is one function call an assistant message asks for.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall is a call's function name and its arguments, a JSON text.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool is one function the model may call.
type Tool struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

// FunctionDef is a tool's name, description and JSON Schema parameters.
type FunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

// reply is one answered request: the assistant message, why it stopped
// and what it cost.
type reply struct {
	Message      Message
	FinishReason string
	Model        string
	Usage        roundUsage
}

// roundUsage is one round's usage as the provider reports it.
type roundUsage struct {
	Model            string   `json:"model,omitempty"`
	PromptTokens     uint64   `json:"prompt_tokens"`
	CompletionTokens uint64   `json:"completion_tokens"`
	CachedTokens     uint64   `json:"cached_tokens,omitempty"`
	Cost             *float64 `json:"cost,omitempty"`
}

// Why a request failed, as the attempt's reason.
const (
	failRateLimit = "rate_limit"
	failProvider  = "provider_unavailable"
	failTimeout   = "timeout"
	failContext   = "context_limit"
	failAuth      = "auth"
	failNotFound  = "not_found"
	failRequest   = "request_refused"
	failMalformed = "malformed_reply"
)

// requestError is a request that got no usable reply: its reason, whether
// it is a provider's limit (a rate limit, or a refusal that says when to
// come back) with the time the provider named, and a short detail that
// never holds the request.
type requestError struct {
	Reason  string
	Limited bool
	ResetAt *time.Time
	Status  int
	Detail  string
}

func (e *requestError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("%s (HTTP %d): %s", e.Reason, e.Status, e.Detail)
	}
	return e.Reason + ": " + e.Detail
}

// endpoint is one binding's Chat Completions endpoint.
type endpoint struct {
	url     string
	key     string
	headers map[string]string
	model   string
	extra   map[string]json.RawMessage
	timeout time.Duration
	client  *http.Client
	now     func() time.Time
}

// maxReply bounds a reply body.
const maxReply = 16 << 20

// complete sends one request and decodes its reply. ctx cancelling is
// returned as ctx's error, so the caller can tell an interrupt from a
// request that ran out of time.
func (p *endpoint) complete(ctx context.Context, messages []Message, tools []Tool) (reply, error) {
	body := map[string]any{}
	for k, v := range p.extra {
		body[k] = v
	}
	body["model"], body["messages"], body["stream"] = p.model, messages, false
	if len(tools) > 0 {
		body["tools"] = tools
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return reply{}, &requestError{Reason: failRequest, Detail: "the request could not be encoded"}
	}
	rctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, p.url+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return reply{}, &requestError{Reason: failRequest, Detail: "the request could not be made"}
	}
	for k, v := range p.headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if p.key != "" {
		req.Header.Set("Authorization", "Bearer "+p.key)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return reply{}, ctx.Err()
		}
		if rctx.Err() != nil {
			return reply{}, &requestError{Reason: failTimeout, Detail: fmt.Sprintf("no reply within %s", p.timeout)}
		}
		return reply{}, &requestError{Reason: failProvider, Detail: "the endpoint could not be reached"}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxReply+1))
	if err != nil {
		if ctx.Err() != nil {
			return reply{}, ctx.Err()
		}
		if rctx.Err() != nil {
			return reply{}, &requestError{Reason: failTimeout, Detail: fmt.Sprintf("no complete reply within %s", p.timeout)}
		}
		return reply{}, &requestError{Reason: failProvider, Detail: "the reply was cut off"}
	}
	if len(data) > maxReply {
		return reply{}, &requestError{Reason: failMalformed, Detail: "the reply is larger than 16 MiB"}
	}
	if resp.StatusCode != http.StatusOK {
		return reply{}, p.statusError(resp, data)
	}
	return decodeReply(data)
}

// statusError classifies a refused request. A rate limit is a limit, with
// the time the provider says to come back (Retry-After); a server error or
// an overloaded provider may pass; a context-length refusal says the
// conversation outgrew the model; bad credentials or an unknown model
// won't cure themselves.
func (p *endpoint) statusError(resp *http.Response, data []byte) error {
	detail := providerMessage(data)
	e := &requestError{Status: resp.StatusCode, Detail: detail}
	switch code := resp.StatusCode; {
	case code == http.StatusTooManyRequests:
		e.Reason, e.Limited, e.ResetAt = failRateLimit, true, retryAfter(resp.Header.Get("Retry-After"), p.now())
	case code == http.StatusRequestTimeout || code >= 500:
		// An outage that says when to come back is a limit like a rate
		// limit's; one that doesn't is an ordinary failure, retried on
		// the runtime's retry delays.
		e.Reason, e.ResetAt = failProvider, retryAfter(resp.Header.Get("Retry-After"), p.now())
		e.Limited = e.ResetAt != nil
	case contextLimit(detail):
		e.Reason = failContext
	case code == http.StatusUnauthorized || code == http.StatusForbidden || code == http.StatusPaymentRequired:
		e.Reason = failAuth
	case code == http.StatusNotFound:
		e.Reason = failNotFound
	default:
		e.Reason = failRequest
	}
	return e
}

// contextLimit reports a refusal that names the model's context length.
func contextLimit(detail string) bool {
	d := strings.ToLower(detail)
	return strings.Contains(d, "context_length") || strings.Contains(d, "context length") || strings.Contains(d, "context window") || strings.Contains(d, "maximum context") || strings.Contains(d, "too many tokens")
}

// providerMessage is the error message a refusal carries ({"error":
// {"message"}}, or {"error": "…"}), clipped, or else the start of the body.
func providerMessage(data []byte) string {
	var body struct {
		Error json.RawMessage `json:"error"`
	}
	msg := ""
	if json.Unmarshal(data, &body) == nil && len(body.Error) > 0 {
		var obj struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(body.Error, &obj) == nil && obj.Message != "" {
			msg = obj.Message
		} else {
			_ = json.Unmarshal(body.Error, &msg)
		}
	}
	if msg == "" {
		msg = string(data)
	}
	return truncate(strings.TrimSpace(msg), 500)
}

// retryAfter is a Retry-After header's time: seconds or an HTTP date.
// Nil when absent, unreadable or past.
func retryAfter(v string, now time.Time) *time.Time {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	var at time.Time
	if s, err := strconv.ParseFloat(v, 64); err == nil && s >= 0 && s < 7*24*3600 {
		at = now.Add(time.Duration(s * float64(time.Second)))
	} else if t, err := http.ParseTime(v); err == nil {
		at = t
	} else {
		return nil
	}
	if !at.After(now) {
		return nil
	}
	at = at.UTC().Truncate(time.Second).Add(time.Second)
	return &at
}

// decodeReply reads a 200 reply, tolerating what providers differ in:
// an error object in a 200 body, content as a string, null or a list of
// text parts, and arguments as a JSON text or an object.
func decodeReply(data []byte) (reply, error) {
	var body struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content   json.RawMessage `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string          `json:"name"`
						Arguments json.RawMessage `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens        uint64   `json:"prompt_tokens"`
			CompletionTokens    uint64   `json:"completion_tokens"`
			Cost                *float64 `json:"cost"`
			PromptTokensDetails *struct {
				CachedTokens uint64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return reply{}, &requestError{Reason: failMalformed, Detail: "the reply is not a Chat Completions object"}
	}
	if len(body.Choices) == 0 {
		if len(body.Error) > 0 && string(body.Error) != "null" {
			// An error in a 200 body (OpenRouter passes a provider's
			// failure on this way) carries its HTTP code, when it has one.
			var e struct {
				Code json.RawMessage `json:"code"`
			}
			_ = json.Unmarshal(body.Error, &e)
			detail := providerMessage(data)
			switch code := strings.Trim(string(e.Code), `"`); {
			case code == "429" || strings.Contains(code, "rate"):
				return reply{}, &requestError{Reason: failRateLimit, Limited: true, Detail: detail}
			case contextLimit(detail):
				return reply{}, &requestError{Reason: failContext, Detail: detail}
			case strings.HasPrefix(code, "4") && len(code) == 3:
				return reply{}, &requestError{Reason: failRequest, Detail: detail}
			}
			return reply{}, &requestError{Reason: failProvider, Detail: detail}
		}
		return reply{}, &requestError{Reason: failMalformed, Detail: "the reply has no choices"}
	}
	c := body.Choices[0]
	r := reply{FinishReason: c.FinishReason, Model: body.Model}
	r.Message = Message{Role: "assistant", Content: textContent(c.Message.Content)}
	for _, call := range c.Message.ToolCalls {
		args := strings.TrimSpace(string(call.Function.Arguments))
		var s string
		if json.Unmarshal(call.Function.Arguments, &s) == nil {
			args = s
		}
		if args == "null" {
			args = ""
		}
		r.Message.ToolCalls = append(r.Message.ToolCalls, ToolCall{ID: call.ID, Type: "function", Function: FunctionCall{Name: call.Function.Name, Arguments: args}})
	}
	r.Usage.Model = body.Model
	if u := body.Usage; u != nil {
		r.Usage.PromptTokens, r.Usage.CompletionTokens, r.Usage.Cost = u.PromptTokens, u.CompletionTokens, u.Cost
		if u.PromptTokensDetails != nil {
			r.Usage.CachedTokens = u.PromptTokensDetails.CachedTokens
		}
	}
	return r, nil
}

// textContent is a message's content as text: a string, the text parts
// of a list, or "" for null.
func textContent(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Text != "" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}

// asRequestError unwraps a requestError.
func asRequestError(err error) (*requestError, bool) {
	var re *requestError
	ok := errors.As(err, &re)
	return re, ok
}
