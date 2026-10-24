package utils

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// HTTPClient is the shared, bounded HTTP client used for ALL outbound HTTP
// calls (LiteLLM, embeddings, images, audio, maps, news, search, version
// checks). It keeps a bound on connect/response time so no single call can
// hang forever and wedge a goroutine.
//
// The 90s ResponseHeaderTimeout still allows long SSE chat streams; only the
// time to first response byte is bounded. Dial/TLS are bounded so no call can
// hang on connect either.
var HTTPClient = &http.Client{
	Timeout: 0, // no overall deadline: SSE streams can run for minutes
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 90 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   8,
	},
}

// ---------------------------------------------------------------------------
// Outbound LLM concurrency + retry.
//
// Root cause of the recurring "workflows stop and need to be restarted"
// issue (from the server error log):
//
//   OpenRouter 402:
//     "... would exceed your available credits given your current in-flight
//      requests. Retry after in-flight requests settle"
//     Retry-After: "120"
//
//   -> LiteLLM surfaces it as 500 Internal Server Error
//   -> [LLMLoop] Error calling LLM ... -> the workflow goroutine exits
//
// The server fires several LLM/embedding calls at once (chat stream, async
// message embeddings, title generation, eco checkpoint summary, a second
// chat). Free / low-tier OpenRouter budgets allow only a couple of in-flight
// requests; exceeding that returns 402, LiteLLM wraps it as 500, and the LLM
// loop treats it as fatal — killing the workflow. That also explains why a
// turn that ends with a 60-minute wait never crashes: there are zero
// in-flight requests during the wait.
//
// Two-part fix:
//   1. LLMSem — a global semaphore capping concurrent outbound LLM/embedding
//      HTTP requests (capacity 2: safely inside free-tier in-flight budgets,
//      still parallel enough for streaming). Every LLM/embed caller acquires
//      it around the request (released once response headers arrive, so long
//      SSE streams do not hold the slot).
//   2. DoLLMHTTPWithRetry — issues the request under the semaphore and, on a
//      transient provider rejection (402/429 or 5xx), backs off for the
//      Retry-After header (OpenRouter sends 120s) and retries instead of
//      returning a fatal error. Bounded so it cannot retry forever.
// ---------------------------------------------------------------------------

// LLMSem caps the number of in-flight outbound LLM/embedding requests.
// Tuned for OpenRouter free-tier budgets (commonly 1-2 concurrent).
var LLMSem = make(chan struct{}, 1)

// AcquireLLMSlot blocks until an outbound LLM slot is free.
func AcquireLLMSlot() { LLMSem <- struct{}{} }

// ReleaseLLMSlot frees an outbound LLM slot.
func ReleaseLLMSlot() { <-LLMSem }

// LLMRetryBudget is how many times we'll retry a rate/budget-limited call.
// OpenRouter's in-flight-budget 402 resolves as other in-flight streams
// settle (Retry-After ~120s). 15 attempts x up to 120s backoff = up to
// ~30 minutes of retrying a budget stall — long enough for even a busy key
// to clear — so a transient 402 no longer kills the workflow. Bounded so a
// genuinely dead provider still eventually errors out.
const LLMRetryBudget = 15

// ParseRetryAfter extracts the Retry-After header (seconds). Returns 120s if
// unparseable (OpenRouter's default for in-flight budget 402s).
func ParseRetryAfter(resp *http.Response) time.Duration {
	if resp == nil {
		return 120 * time.Second
	}
	ra := resp.Header.Get("Retry-After")
	if ra == "" {
		return 120 * time.Second
	}
	if sec, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil {
		if sec < 1 {
			sec = 1
		}
		if sec > 300 {
			sec = 300 // never back off more than 5 min per attempt
		}
		return time.Duration(sec) * time.Second
	}
	return 120 * time.Second
}

// RetryableStatus returns true for statuses that indicate a transient
// provider-side rejection where a retry after backoff is appropriate.
func RetryableStatus(code int) bool {
	// 402 = in-flight budget / credit exhaustion (OpenRouter)
	// 429 = rate limited
	// 5xx = upstream overloaded / temporarily failing
	return code == 402 || code == 429 || (code >= 500 && code < 600)
}

// DoLLMHTTPWithRetry issues an LLM/embedding HTTP request under the global
// concurrency semaphore, retrying on 402/429/5xx with Retry-After backoff.
// Returns the 2xx response (caller owns closing Body) or nil + error after
// the retry budget is exhausted. The semaphore slot is released as soon as
// response headers arrive, so long SSE streams do not count against the
// in-flight budget.
func DoLLMHTTPWithRetry(req *http.Request) (*http.Response, error) {
	retries := 0
	for {
		AcquireLLMSlot()
		resp, err := HTTPClient.Do(req)
		ReleaseLLMSlot()

		if err != nil {
			if retries < LLMRetryBudget {
				retries++
				time.Sleep(time.Duration(20*retries) * time.Second)
				continue
			}
			return nil, err
		}

		if RetryableStatus(resp.StatusCode) {
			wait := ParseRetryAfter(resp)
			// Drain + close the body so the connection can be reused.
			io.ReadAll(resp.Body)
			resp.Body.Close()
			if retries < LLMRetryBudget {
				retries++
				Log("[LLM] provider rejected attempt %d with status %d; backing off %v and retrying",
					retries, resp.StatusCode, wait)
				time.Sleep(wait)
				continue
			}
			return nil, fmt.Errorf("LLM provider rejected request with status %d after %d retries", resp.StatusCode, LLMRetryBudget)
		}

		return resp, nil
	}
}
