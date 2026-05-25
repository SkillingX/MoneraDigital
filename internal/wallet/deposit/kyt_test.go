package deposit

import (
	"testing"
	"time"

	"monera-digital/internal/safeheron"
)

// TestSummarizeRiskLevel 已迁移到 internal/safeheron/aml_test.go::TestSummarizeAmlRiskLevel (v1.1 §13.5.1)

func TestDecideKYT(t *testing.T) {
	low := []safeheron.AmlReport{{Status: "COMPLETED", RiskLevel: "LOW"}}
	high := []safeheron.AmlReport{{Status: "COMPLETED", RiskLevel: "HIGH"}}
	severe := []safeheron.AmlReport{{Status: "COMPLETED", RiskLevel: "SEVERE"}}
	medium := []safeheron.AmlReport{{Status: "COMPLETED", RiskLevel: "MEDIUM"}}
	unknown := []safeheron.AmlReport{{Status: "COMPLETED", RiskLevel: "UNKNOWN"}}
	pending := []safeheron.AmlReport{{Status: "PENDING"}}
	failed := []safeheron.AmlReport{{Status: "FAILED"}}
	skipped := []safeheron.AmlReport{{Status: "SKIPPED"}}

	tests := []struct {
		name           string
		state          string
		amlList        []safeheron.AmlReport
		isAfterTimeout bool
		wantAction     KytDecisionAction
		wantRisk       string
		wantReason     string
		wantAlert      string
	}{
		// 初查路径
		{"IN_PROGRESS", "IN_PROGRESS", nil, false, KytActionKeepPending, safeheron.AmlRiskPending, "", ""},
		{"UNTRIGGERED", "UNTRIGGERED", nil, false, KytActionManualReview, safeheron.AmlRiskUnknown, ReasonKytUntriggered, "WARN"},
		{"TRIGGERED LOW", "TRIGGERED", low, false, KytActionCredit, safeheron.AmlRiskLow, "", ""},
		{"TRIGGERED HIGH", "TRIGGERED", high, false, KytActionManualReview, safeheron.AmlRiskHigh, "KYT_RISK_HIGH", "ERROR"},
		{"TRIGGERED SEVERE", "TRIGGERED", severe, false, KytActionManualReview, safeheron.AmlRiskSevere, "KYT_RISK_SEVERE", "ERROR"},
		{"TRIGGERED MEDIUM", "TRIGGERED", medium, false, KytActionManualReview, safeheron.AmlRiskMedium, "KYT_RISK_MEDIUM", "WARN"},
		{"TRIGGERED UNKNOWN", "TRIGGERED", unknown, false, KytActionManualReview, safeheron.AmlRiskUnknown, "KYT_RISK_UNKNOWN", "WARN"},
		{"TRIGGERED PENDING", "TRIGGERED", pending, false, KytActionKeepPending, safeheron.AmlRiskPending, "", ""},
		{"TRIGGERED FAILED", "TRIGGERED", failed, false, KytActionManualReview, safeheron.AmlRiskFailed, ReasonKytProviderFailed, "WARN"},
		{"TRIGGERED SKIPPED", "TRIGGERED", skipped, false, KytActionManualReview, safeheron.AmlRiskSkipped, ReasonKytSkipped, "WARN"},

		// I-2: TRIGGERED + empty amlList → ManualReview (not silent credit)
		{"TRIGGERED empty list", "TRIGGERED", nil, false, KytActionManualReview, safeheron.AmlRiskEmpty, ReasonKytEmptyAmlList, "WARN"},
		{"TRIGGERED empty list timeout", "TRIGGERED", []safeheron.AmlReport{}, true, KytActionManualReview, safeheron.AmlRiskEmpty, ReasonKytEmptyAmlList, "WARN"},

		// S-3: unknown state → ManualReview
		{"unknown state", "BANANA", nil, false, KytActionManualReview, safeheron.AmlRiskUnknown, ReasonKytUnknownState, "ERROR"},
		{"empty state", "", nil, false, KytActionManualReview, safeheron.AmlRiskUnknown, ReasonKytUnknownState, "ERROR"},

		// 超时兜底路径
		{"IN_PROGRESS timeout", "IN_PROGRESS", nil, true, KytActionManualReview, safeheron.AmlRiskPending, ReasonKytTimeoutStillPending, "ERROR"},
		{"UNTRIGGERED timeout", "UNTRIGGERED", nil, true, KytActionManualReview, safeheron.AmlRiskUnknown, ReasonKytUntriggeredAfterTimeout, "WARN"},
		{"TRIGGERED LOW timeout", "TRIGGERED", low, true, KytActionCredit, safeheron.AmlRiskLow, "", ""},
		{"TRIGGERED HIGH timeout", "TRIGGERED", high, true, KytActionManualReview, safeheron.AmlRiskHigh, "KYT_RISK_HIGH_AFTER_TIMEOUT", "ERROR"},
		{"TRIGGERED SEVERE timeout", "TRIGGERED", severe, true, KytActionManualReview, safeheron.AmlRiskSevere, "KYT_RISK_SEVERE_AFTER_TIMEOUT", "ERROR"},
		{"TRIGGERED MEDIUM timeout", "TRIGGERED", medium, true, KytActionManualReview, safeheron.AmlRiskMedium, "KYT_RISK_MEDIUM_AFTER_TIMEOUT", "WARN"},
		{"TRIGGERED PENDING timeout", "TRIGGERED", pending, true, KytActionManualReview, safeheron.AmlRiskPending, ReasonKytTimeoutStillPending, "ERROR"},
		{"TRIGGERED FAILED timeout", "TRIGGERED", failed, true, KytActionManualReview, safeheron.AmlRiskFailed, ReasonKytProviderFailedAfterTimeout, "WARN"},
		{"TRIGGERED SKIPPED timeout", "TRIGGERED", skipped, true, KytActionManualReview, safeheron.AmlRiskSkipped, ReasonKytSkippedAfterTimeout, "WARN"},
		{"TRIGGERED UNKNOWN timeout", "TRIGGERED", unknown, true, KytActionManualReview, safeheron.AmlRiskUnknown, "KYT_RISK_UNKNOWN_AFTER_TIMEOUT", "WARN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DecideKYT(tt.state, tt.amlList, tt.isAfterTimeout)
			if got.Action != tt.wantAction {
				t.Errorf("Action = %d, want %d", got.Action, tt.wantAction)
			}
			if got.RiskLevel != tt.wantRisk {
				t.Errorf("RiskLevel = %q, want %q", got.RiskLevel, tt.wantRisk)
			}
			if got.Reason != tt.wantReason {
				t.Errorf("Reason = %q, want %q", got.Reason, tt.wantReason)
			}
			if tt.wantAlert != "" && got.AlertLevel != tt.wantAlert {
				t.Errorf("AlertLevel = %q, want %q", got.AlertLevel, tt.wantAlert)
			}
		})
	}
}

func TestAlertLevelForKyt(t *testing.T) {
	tests := []struct {
		risk string
		want string
	}{
		{safeheron.AmlRiskHigh, "ERROR"},
		{safeheron.AmlRiskSevere, "ERROR"},
		{safeheron.AmlRiskLow, "WARN"},
		{safeheron.AmlRiskMedium, "WARN"},
		{safeheron.AmlRiskUnknown, "WARN"},
		{safeheron.AmlRiskFailed, "WARN"},
	}
	for _, tt := range tests {
		if got := AlertLevelForKyt(tt.risk); got != tt.want {
			t.Errorf("AlertLevelForKyt(%q) = %q, want %q", tt.risk, got, tt.want)
		}
	}
}

func TestMaxLastUpdateTime(t *testing.T) {
	t.Run("single valid timestamp", func(t *testing.T) {
		aml := []safeheron.AmlReport{{LastUpdateTime: "1715500001000"}}
		got := maxLastUpdateTime(aml)
		want := time.UnixMilli(1715500001000)
		if !got.Equal(want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("empty LastUpdateTime returns zero (S-3)", func(t *testing.T) {
		aml := []safeheron.AmlReport{{LastUpdateTime: ""}}
		got := maxLastUpdateTime(aml)
		if !got.IsZero() {
			t.Errorf("expected zero time so timeout scan fires immediately, got %v", got)
		}
	})

	t.Run("multiple picks max", func(t *testing.T) {
		aml := []safeheron.AmlReport{
			{LastUpdateTime: "1715500001000"},
			{LastUpdateTime: "1715500003000"},
			{LastUpdateTime: "1715500002000"},
		}
		got := maxLastUpdateTime(aml)
		want := time.UnixMilli(1715500003000)
		if !got.Equal(want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("all parse failures return zero (S-3)", func(t *testing.T) {
		aml := []safeheron.AmlReport{
			{LastUpdateTime: "not-a-number"},
			{LastUpdateTime: ""},
		}
		got := maxLastUpdateTime(aml)
		if !got.IsZero() {
			t.Errorf("expected zero time so timeout scan fires immediately, got %v", got)
		}
	})

	t.Run("empty list returns zero (S-3)", func(t *testing.T) {
		got := maxLastUpdateTime(nil)
		if !got.IsZero() {
			t.Errorf("expected zero time for empty list, got %v", got)
		}
	})
}

func TestBuildKytRiskReason(t *testing.T) {
	if got := BuildKytRiskReason("HIGH"); got != "KYT_RISK_HIGH" {
		t.Errorf("got %q", got)
	}
}

func TestBuildKytTimeoutRiskReason(t *testing.T) {
	if got := BuildKytTimeoutRiskReason("SEVERE"); got != "KYT_RISK_SEVERE_AFTER_TIMEOUT" {
		t.Errorf("got %q", got)
	}
}
