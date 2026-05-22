package alert

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type captureEmailer struct {
	mu    sync.Mutex
	calls []emailCall
	fail  bool
}

type emailCall struct {
	To      string
	Subject string
	Body    string
}

func (c *captureEmailer) SendAlertEmail(_ context.Context, to, subject, body string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, emailCall{To: to, Subject: subject, Body: body})
	if c.fail {
		return errAlertEmailFailed
	}
	return nil
}

var errAlertEmailFailed = &emailFailErr{}

type emailFailErr struct{}

func (e *emailFailErr) Error() string { return "email failed" }

func feishuOKHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"code":0,"msg":"success"}`))
}

func TestClassifyAlertPrefix(t *testing.T) {
	tests := []struct {
		title string
		want  string
	}{
		{"KYT alert manual review", "【AML告警】"},
		{"KYT timeout API failure", "【AML告警】"},
		{"KYT timeout manual review", "【AML告警】"},
		{"KYT orphan alert exceeded retries", "【AML告警】"},
		{"KYT manual review", "【AML告警】"},
		{"Deposit KYT overlap", "【AML告警】"},
		{"Deposit failed", "【充值告警】"},
		{"Deposit manual review", "【充值告警】"},
		{"Withdraw timeout", "【提现告警】"},
		{"Registry refresh failed", "【系统告警】"},
		{"Unknown error", "【系统告警】"},
		{"", "【系统告警】"},
	}
	for _, tt := range tests {
		t.Run(tt.title, func(t *testing.T) {
			if got := classifyAlertPrefix(tt.title); got != tt.want {
				t.Errorf("classifyAlertPrefix(%q) = %q, want %q", tt.title, got, tt.want)
			}
		})
	}
}

func TestAlertService_Nil_Safe(t *testing.T) {
	var a *AlertService
	a.Send("INFO", "title", nil) // should be no-op, not panic
}

func TestAlertService_FormatAlert_Deterministic(t *testing.T) {
	out := formatAlert("【充值告警】", "ERROR", "Deposit manual review", map[string]string{
		"userId":             "42",
		"reason":             "ADDRESS_UNASSIGNED",
		"destinationAddress": "0xabc",
	})
	want := strings.Join([]string{
		"【充值告警】level=ERROR",
		"title=Deposit manual review",
		"destinationAddress=0xabc",
		"reason=ADDRESS_UNASSIGNED",
		"userId=42",
		"",
	}, "\n")
	if out != want {
		t.Errorf("format mismatch:\nwant=%q\n got=%q", want, out)
	}
}

func TestAlertService_FeishuPOSTsJSON(t *testing.T) {
	var captured atomic.Pointer[map[string]any]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("expected JSON content-type, got %s", r.Header.Get("Content-Type"))
		}
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		captured.Store(&m)
		feishuOKHandler(w, r)
	}))
	defer srv.Close()

	a := NewAlertService(srv.URL, "", nil, nil)
	a.Send("ERROR", "Test", map[string]string{"reason": "X"})

	got := captured.Load()
	if got == nil {
		t.Fatal("feishu endpoint not hit")
	}
	if (*got)["msg_type"] != "text" {
		t.Errorf("expected msg_type=text, got %v", (*got)["msg_type"])
	}
	content, ok := (*got)["content"].(map[string]any)
	if !ok || !strings.Contains(content["text"].(string), "reason=X") {
		t.Errorf("content missing reason: %+v", (*got)["content"])
	}
}

func TestAlertService_FeishuSigning_AddsTimestampAndSign(t *testing.T) {
	const secret = "test-secret"
	var captured atomic.Pointer[map[string]any]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		captured.Store(&m)
		feishuOKHandler(w, r)
	}))
	defer srv.Close()

	a := NewAlertService(srv.URL, secret, nil, nil)
	a.Send("ERROR", "Test", nil)

	got := captured.Load()
	if got == nil {
		t.Fatal("feishu endpoint not hit")
	}

	tsRaw, ok := (*got)["timestamp"]
	if !ok {
		t.Fatal("missing timestamp field in signed request")
	}
	signRaw, ok := (*got)["sign"]
	if !ok {
		t.Fatal("missing sign field in signed request")
	}

	ts := tsRaw.(string)
	sign := signRaw.(string)

	// Verify signature: HMAC-SHA256(key=ts+"\n"+secret, message="")
	mac := hmac.New(sha256.New, []byte(ts+"\n"+secret))
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if sign != expected {
		t.Errorf("sign mismatch: got %q, want %q", sign, expected)
	}

	// Timestamp must be within last 10 seconds
	tsInt, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		t.Fatalf("timestamp not an integer: %q", ts)
	}
	age := time.Now().Unix() - tsInt
	if age < 0 || age > 10 {
		t.Errorf("timestamp too old or in future: age=%ds", age)
	}
}

func TestAlertService_FeishuNoSigning_OmitsTimestampAndSign(t *testing.T) {
	var captured atomic.Pointer[map[string]any]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		captured.Store(&m)
		feishuOKHandler(w, r)
	}))
	defer srv.Close()

	a := NewAlertService(srv.URL, "", nil, nil)
	a.Send("ERROR", "Test", nil)

	got := captured.Load()
	if got == nil {
		t.Fatal("feishu endpoint not hit")
	}
	if _, ok := (*got)["timestamp"]; ok {
		t.Error("timestamp should not be present when no secret configured")
	}
	if _, ok := (*got)["sign"]; ok {
		t.Error("sign should not be present when no secret configured")
	}
}

func TestAlertService_FeishuAPIError_Logged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":19021,"msg":"sign match fail"}`))
	}))
	defer srv.Close()

	a := NewAlertService(srv.URL, "", nil, nil)
	a.Send("ERROR", "Test", nil) // must not panic; error is logged internally
}

// TestAlertService_FeishuNonJSONBody_ParseErrorLogged 验证服务器返回 200 但 body 不是
// JSON 时，sendFeishu 记录解析错误（不 panic）。这覆盖了签名后 json.Decode 失败路径。
func TestAlertService_FeishuNonJSONBody_ParseErrorLogged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not-json"))
	}))
	defer srv.Close()

	a := NewAlertService(srv.URL, "", nil, nil)
	a.Send("ERROR", "Test", nil) // must not panic
}

func TestAlertService_EmailFanoutNonBlocking(t *testing.T) {
	emailer := &captureEmailer{}
	a := NewAlertService("", "", []string{"a@x.com", "b@x.com"}, emailer)
	a.Send("INFO", "Test", map[string]string{"k": "v"})
	if len(emailer.calls) != 2 {
		t.Errorf("expected 2 email calls, got %d", len(emailer.calls))
	}
}

func TestAlertService_EmailFailureSwallowed(t *testing.T) {
	emailer := &captureEmailer{fail: true}
	a := NewAlertService("", "", []string{"a@x.com"}, emailer)
	a.Send("INFO", "Test", nil) // must not panic / return error
	if len(emailer.calls) != 1 {
		t.Errorf("expected 1 email call even when it fails, got %d", len(emailer.calls))
	}
}

func TestAlertService_FeishuFailureSwallowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	a := NewAlertService(srv.URL, "", nil, nil)
	a.Send("INFO", "Test", nil) // must not error
}

func TestAlertService_EmptyFeishuURL_NoCall(t *testing.T) {
	a := NewAlertService("", "", nil, nil)
	a.Send("INFO", "Test", nil) // no panic on nil-everything
}

// TestAlertService_FeishuRespectsCtxDeadline verifies sendFeishu propagates a
// context deadline to the outbound HTTP request rather than relying solely on
// httpClient.Timeout. Regression: T7-S-3.
func TestAlertService_FeishuRespectsCtxDeadline(t *testing.T) {
	released := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-released:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	defer close(released)

	a := NewAlertService(srv.URL, "", nil, nil)
	a.httpClient = &http.Client{Timeout: 5 * time.Second}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	a.sendFeishu(ctx, "test message")
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Errorf("sendFeishu did not honor ctx deadline; elapsed=%v (httpClient.Timeout would have been 5s, ctx deadline 50ms)", elapsed)
	}
}

// TestAlertService_EmailSubjectIncludesTitle verifies that the alert title
// reaches the inbox as the email subject. Previously the title was dropped
// because AlertService borrowed SendActivationEmail (no subject parameter).
// Regression: T7-I-6.
func TestAlertService_EmailSubjectIncludesTitle(t *testing.T) {
	emailer := &captureEmailer{}
	a := NewAlertService("", "", []string{"ops@x.com"}, emailer)
	a.Send("ERROR", "Deposit manual review", map[string]string{"reason": "ADDRESS_UNASSIGNED"})

	if len(emailer.calls) != 1 {
		t.Fatalf("expected 1 email, got %d", len(emailer.calls))
	}
	got := emailer.calls[0]
	if !strings.Contains(got.Subject, "Deposit manual review") {
		t.Errorf("email subject must include alert title, got %q", got.Subject)
	}
	if !strings.Contains(got.Body, "reason=ADDRESS_UNASSIGNED") {
		t.Errorf("email body must include alert fields, got %q", got.Body)
	}
}
