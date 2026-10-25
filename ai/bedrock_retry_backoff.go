//go:build !pig_strip_bedrock_converse_stream

// 中文导读：Bedrock 重试退避：@smithy/core StandardRetryStrategy/DefaultRetryBackoffStrategy 与 parseRetryAfterHeader 的移植。
package ai

// Ports node_modules/@smithy/core/dist-es/submodules/retry (StandardRetryStrategy, DefaultRetryBackoffStrategy, parseRetryAfterHeader), the retry delay of Pi's Bedrock client.

import (
	"errors"
	"math"
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/MichaelKinsy/PiG/internal/lazyregexp"
)

// The JavaScript SDK's standard retry constants, as bundled in Pi's Bedrock client (@smithy/core 3.33.3, the lockfile's version).
const (
	// upstream: node_modules/@smithy/core/dist-cjs/submodules/retry/index.js:DEFAULT_RETRY_DELAY_BASE
	bedrockSDKRetryDelayBase = 100
	// upstream: node_modules/@smithy/core/dist-cjs/submodules/retry/index.js:THROTTLING_RETRY_DELAY_BASE
	bedrockSDKThrottlingRetryDelayBase = 500
	// upstream: node_modules/@smithy/core/dist-cjs/submodules/retry/index.js:MAXIMUM_RETRY_DELAY
	bedrockSDKMaximumRetryDelay = 20 * 1000
	// StandardRetryStrategy.refreshRetryTokenForRetry caps a retry-after hint at the computed delay plus 5000 ms.
	// upstream: node_modules/@smithy/core/dist-cjs/submodules/retry/index.js:refreshRetryTokenForRetry
	bedrockSDKRetryAfterSlackMs = 5_000
)

// upstream: node_modules/@smithy/core/dist-cjs/submodules/retry/index.js:THROTTLING_ERROR_CODES
var bedrockSDKThrottlingErrorCodes = []string{"BandwidthLimitExceeded", "EC2ThrottledException", "LimitExceededException", "PriorRequestNotComplete", "ProvisionedThroughputExceededException", "RequestLimitExceeded", "RequestThrottled", "RequestThrottledException", "SlowDown", "ThrottledException", "Throttling", "ThrottlingException", "TooManyRequestsException", "TransactionInProgressException"}

// bedrockSDKBackoff is the JavaScript SDK's standard retry delay, which Pi's Bedrock client waits between attempts:
// floor(random() * min(base * 2^retryCount, 20000)) ms, with base 500 for a throttling error and 100 otherwise, then raised to a
// retry-after hint by at most 5000 ms. The Go SDK's default waits random() * 2^attempt seconds.
type bedrockSDKBackoff struct {
	random func() float64
	now    func() time.Time
}

// bedrockRetryRandom is Math.random for the retry jitter.
var bedrockRetryRandom = rand.Float64

// newBedrockRetryer keeps the client's resolved retry mode (AWS_RETRY_MODE or the shared config's retry_mode): the JavaScript SDK's AdaptiveRetryStrategy adds its rate limiter to a StandardRetryStrategy with the same backoff.
func newBedrockRetryer(mode aws.RetryMode) aws.Retryer {
	backoff := func(options *retry.StandardOptions) {
		options.Backoff = bedrockSDKBackoff{random: bedrockRetryRandom, now: time.Now}
	}
	if mode == aws.RetryModeAdaptive {
		return retry.NewAdaptiveMode(func(options *retry.AdaptiveModeOptions) {
			options.StandardOptions = append(options.StandardOptions, backoff)
		})
	}
	return retry.NewStandard(backoff)
}

// BackoffDelay receives the 1-based number of the attempt that failed; the JavaScript retry count of the next attempt's token is one less.
// 退避公式（JS SDK StandardRetryStrategy 移植）：
// floor(random() * min(base * 2^(attempt-1), 20000ms))——节流错误
// base=500ms，其他 100ms；retry-after 服务器提示把延迟抬到提示
// 值，但最多只比算出的延迟多 5000ms slack（防服务器要求过长）。
func (b bedrockSDKBackoff) BackoffDelay(attempt int, err error) (time.Duration, error) {
	base := float64(bedrockSDKRetryDelayBase)
	if bedrockSDKThrottlingError(err) {
		base = bedrockSDKThrottlingRetryDelayBase
	}
	delayMs := math.Floor(b.random() * math.Min(base*math.Pow(2, float64(max(attempt-1, 0))), bedrockSDKMaximumRetryDelay))
	delay := time.Duration(delayMs) * time.Millisecond
	if response, ok := errors.AsType[*smithyhttp.ResponseError](err); ok && response.Response != nil && response.Response.Response != nil {
		now := b.now()
		if hint, ok := bedrockSDKRetryAfterHint(response.Response.Header, now); ok {
			delay = max(delay, min(hint.Sub(now), delay+bedrockSDKRetryAfterSlackMs*time.Millisecond))
		}
	}
	return delay, nil
}

// bedrockSDKThrottlingError is service-error-classification isThrottlingError: status 429 or a throttling error name.
func bedrockSDKThrottlingError(err error) bool {
	if response, ok := errors.AsType[*smithyhttp.ResponseError](err); ok && response.HTTPStatusCode() == http.StatusTooManyRequests {
		return true
	}
	apiError, ok := errors.AsType[smithy.APIError](err)
	return ok && slices.Contains(bedrockSDKThrottlingErrorCodes, apiError.ErrorCode())
}

var (
	bedrockRetryAfterGMTSeconds = lazyregexp.New(` GMT, ([\d.]+)$`)
	bedrockRetryAfterSeconds    = lazyregexp.New(`^((\d+)|(\d+\.\d+))$`)
)

// bedrockSDKRetryAfterHint is parseRetryAfterHeader for the retry-after and x-amz-retry-after headers. Node keeps the wire order of the headers and the first of the two wins; Go's header map has no order, so retry-after is read first.
// 头解析顺序：retry-after 优先于 x-amz-retry-after（Go 的
// header map 不保留到达顺序，故固定先查前者）。retry-after
// 值四种格式依序尝试：GMT 日期、"GMT, 秒数"、纯秒数、
// 其他可 ParseTime 的非过去日期。
func bedrockSDKRetryAfterHint(header http.Header, now time.Time) (time.Time, bool) {
	if values, ok := header[http.CanonicalHeaderKey("retry-after")]; ok && len(values) > 0 {
		value := values[0]
		seconds := math.NaN()
		switch {
		case strings.HasSuffix(value, "GMT"):
			if date, err := http.ParseTime(value); err == nil {
				seconds = date.Sub(now).Seconds()
			}
		case bedrockRetryAfterGMTSeconds.MatchString(value):
			seconds = bedrockRetryNumber(bedrockRetryAfterGMTSeconds.FindStringSubmatch(value)[1])
		case bedrockRetryAfterSeconds.MatchString(value):
			seconds = bedrockRetryNumber(value)
		default:
			if date, err := http.ParseTime(value); err == nil && !date.Before(now) {
				seconds = date.Sub(now).Seconds()
			}
		}
		if math.IsNaN(seconds) {
			return time.Time{}, false
		}
		return now.Add(time.Duration(seconds * float64(time.Second))), true
	}
	if values, ok := header[http.CanonicalHeaderKey("x-amz-retry-after")]; ok && len(values) > 0 {
		milliseconds := bedrockRetryNumber(values[0])
		if math.IsNaN(milliseconds) {
			return time.Time{}, false
		}
		return now.Add(time.Duration(milliseconds * float64(time.Millisecond))), true
	}
	return time.Time{}, false
}

// bedrockRetryNumber is Number(text) for decimal text; hexadecimal and Infinity spellings, which no retry header carries, are not modelled.
func bedrockRetryNumber(text string) float64 {
	value, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil {
		if strings.TrimSpace(text) == "" {
			return 0
		}
		return math.NaN()
	}
	return value
}
