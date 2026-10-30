package bedrock

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/umairge66/gocode/internal/provider"
)

// This file maps Bedrock failures — an HTTP error (REST-JSON: the
// X-Amzn-ErrorType header plus a {"message": ...} body) or an event-stream
// exception frame mid-stream — onto provider.APIError, so ModelCaller's retry
// and fallback logic treats them like every other provider's errors:
//
//	ThrottlingException, ServiceQuotaExceededException  -> rate_limit (retry)
//	ModelNotReadyException, InternalServerException,
//	ServiceUnavailableException, ModelTimeoutException,
//	ModelErrorException, ModelStreamErrorException      -> server (retry)
//	AccessDeniedException, UnrecognizedClientException,
//	InvalidSignatureException, ExpiredToken...          -> auth
//	ValidationException                                 -> unknown (HTTP 400; never retried)
//	ResourceNotFoundException                           -> unknown (HTTP 404)
//
// The detail kept on the error is scrubbed of the credentials in use (an
// InvalidSignatureException echoes the canonical request, which can contain
// the session token).

const providerName = "bedrock"

// awsErrorBody is the REST-JSON error body (Bedrock uses "message"; some AWS
// front ends use "Message"; "__type" may carry the error code).
type awsErrorBody struct {
	Message  string `json:"message"`
	MessageU string `json:"Message"`
	Type     string `json:"__type"`
}

// normalizeErrorType strips the namespace/URI decorations AWS adds
// ("com.amazon.coral...#ValidationException", "ValidationException:http://...")
// and returns the bare code with its first letter upper-cased (event-stream
// exception types are camelCase: "throttlingException").
func normalizeErrorType(t string) string {
	t = strings.TrimSpace(t)
	if i := strings.IndexByte(t, ':'); i >= 0 {
		t = t[:i]
	}
	if i := strings.LastIndexByte(t, '#'); i >= 0 {
		t = t[i+1:]
	}
	if t != "" && t[0] >= 'a' && t[0] <= 'z' {
		t = string(t[0]-'a'+'A') + t[1:]
	}
	return t
}

// httpErrorFrom builds the typed error for a non-2xx Bedrock response.
func httpErrorFrom(resp *http.Response, model string, secrets []string) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var body awsErrorBody
	_ = json.Unmarshal(raw, &body)
	msg := body.Message
	if msg == "" {
		msg = body.MessageU
	}
	if msg == "" {
		msg = strings.TrimSpace(string(raw))
	}
	typ := normalizeErrorType(resp.Header.Get("X-Amzn-ErrorType"))
	if typ == "" {
		typ = normalizeErrorType(body.Type)
	}
	e := classify(resp.StatusCode, typ, msg, model, secrets)
	e.RetryAfter = strings.TrimSpace(resp.Header.Get("Retry-After"))
	return e
}

// streamErrorFrom builds the typed error for an event-stream exception/error
// frame. Status 0 marks it as a mid-stream failure; the category comes from
// the exception type.
func streamErrorFrom(typ, payload, model string, secrets []string) error {
	var body awsErrorBody
	msg := payload
	if json.Unmarshal([]byte(payload), &body) == nil {
		if body.Message != "" {
			msg = body.Message
		} else if body.MessageU != "" {
			msg = body.MessageU
		}
	}
	return classify(0, normalizeErrorType(typ), msg, model, secrets)
}

// classify maps (status, error type, message) to an *provider.APIError.
func classify(status int, typ, msg, model string, secrets []string) *provider.APIError {
	msg = scrub(msg, secrets)
	lmsg := strings.ToLower(msg)
	if typ == "" {
		typ = typeForStatus(status)
	}
	detail := typ
	if msg != "" {
		detail = typ + ": " + msg
	}
	e := &provider.APIError{Provider: providerName, Status: status, Detail: detail}

	switch typ {
	case "ThrottlingException", "TooManyRequestsException", "ServiceQuotaExceededException":
		e.Category = provider.CategoryRateLimit
		e.Retryable = true
		e.Message = "Rate limited by Amazon Bedrock (" + typ + "). Wait a moment and try again, or request a quota increase."
	case "ModelNotReadyException", "InternalServerException", "InternalFailure", "ServiceUnavailableException",
		"ServiceUnavailable", "ModelTimeoutException", "ModelErrorException", "ModelStreamErrorException":
		e.Category = provider.CategoryServer
		e.Retryable = true
		e.Message = "Amazon Bedrock is temporarily unavailable (" + typ + "). Try again in a moment."
	case "AccessDeniedException", "UnrecognizedClientException", "InvalidSignatureException",
		"IncompleteSignature", "IncompleteSignatureException", "ExpiredToken", "ExpiredTokenException",
		"InvalidClientTokenId", "MissingAuthenticationToken", "MissingAuthenticationTokenException",
		"UnauthorizedException", "SignatureDoesNotMatch":
		e.Category = provider.CategoryAuth
		if typ == "AccessDeniedException" && (strings.Contains(lmsg, "model") || strings.Contains(lmsg, "access to")) {
			// Model-level access: let the fallback chain try another model
			// (provider fallback recognises "permission_error" + "model:").
			e.Detail += " (permission_error model: " + model + ")"
			e.Message = "Amazon Bedrock denied access to model " + model + ". Enable model access in the Bedrock console " +
				"(and check the IAM policy allows bedrock:InvokeModel / bedrock:InvokeModelWithResponseStream). (" + msg + ")"
		} else {
			e.Message = "Authentication failed for Amazon Bedrock (" + typ + ") — check your AWS credentials (" +
				EnvBearerToken + ", " + EnvAccessKeyID + "/" + EnvSecretAccessKey + ", or " + EnvProfile + ") and region."
		}
	case "ValidationException":
		e.Category = provider.CategoryUnknown
		switch {
		case strings.Contains(lmsg, "model identifier is invalid") || strings.Contains(lmsg, "invalid model identifier"):
			e.Detail += " (not a valid model id)"
			e.Message = "Amazon Bedrock does not recognise model " + model + " (" + msg + ")"
		case strings.Contains(lmsg, "on-demand throughput") || strings.Contains(lmsg, "inference profile"):
			bare := baseModelID(model)
			e.Message = "Amazon Bedrock cannot invoke " + model + " directly in this region; use a cross-region " +
				"inference profile id such as us." + bare + " or global." + bare + " (" + msg + ")"
		case strings.Contains(lmsg, "prompt is too long"):
			e.Message = msg
		case strings.Contains(lmsg, "too long") || strings.Contains(lmsg, "too many tokens") ||
			strings.Contains(lmsg, "context window") || strings.Contains(lmsg, "input is too long"):
			// Normalise to the wording reactive compaction recognises.
			e.Message = "Prompt is too long: " + msg
		default:
			e.Message = "Amazon Bedrock rejected the request (" + statusText(status) + "): " + msg
		}
	case "ResourceNotFoundException":
		e.Category = provider.CategoryUnknown
		if strings.Contains(lmsg, "model") {
			e.Detail += " (model_not_found: model does not exist)"
		}
		e.Message = "Amazon Bedrock could not find " + model + " in this region (" + msg + ")"
	default:
		e.Category, e.Retryable = categoryForStatus(status, lmsg)
		switch e.Category {
		case provider.CategoryRateLimit:
			e.Message = "Rate limited by Amazon Bedrock. Wait a moment and try again."
		case provider.CategoryServer:
			e.Message = "Amazon Bedrock is temporarily unavailable (server error). Try again in a moment."
		case provider.CategoryAuth:
			e.Message = "Authentication failed for Amazon Bedrock — check your AWS credentials and region."
		default:
			e.Message = "Amazon Bedrock request failed (" + statusText(status) + "): " + detail
		}
	}
	return e
}

func typeForStatus(status int) string {
	switch {
	case status == 0:
		return "StreamError"
	case status == 429:
		return "ThrottlingException"
	case status == 401 || status == 403:
		return "AccessDeniedException"
	case status >= 500:
		return "InternalServerException"
	}
	return ""
}

func categoryForStatus(status int, lmsg string) (provider.Category, bool) {
	switch {
	case status == 401 || status == 403:
		return provider.CategoryAuth, false
	case status == 429:
		return provider.CategoryRateLimit, true
	case status == 408:
		return provider.CategoryServer, true
	case status >= 500 || status == 0:
		return provider.CategoryServer, true
	}
	if strings.Contains(lmsg, "throttl") {
		return provider.CategoryRateLimit, true
	}
	return provider.CategoryUnknown, false
}

func statusText(status int) string {
	if status == 0 {
		return "mid-stream"
	}
	return "HTTP " + strconv.Itoa(status)
}

// scrub removes every known secret from s, and cuts an
// InvalidSignatureException's echoed canonical request/string-to-sign.
func scrub(s string, secrets []string) string {
	for _, marker := range []string{"The Canonical String for this request should have been", "The String-to-Sign should have been"} {
		if i := strings.Index(s, marker); i >= 0 {
			s = strings.TrimSpace(s[:i])
		}
	}
	for _, sec := range secrets {
		if len(sec) >= 4 {
			s = strings.ReplaceAll(s, sec, "***")
		}
	}
	return s
}
