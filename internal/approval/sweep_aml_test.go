package approval

import (
	"encoding/json"
	"testing"
)

func TestDecideSweepAML(t *testing.T) {
	tests := []struct {
		name          string
		state         string
		amlListRaw    string // 原始 JSON；空字符串表示 nil RawMessage
		wantApprove   bool
		wantRiskLevel string
		wantReason    string
	}{
		// TRIGGERED + risk 等级
		{
			name:          "triggered_low_approves",
			state:         "TRIGGERED",
			amlListRaw:    `[{"provider":"MistTrack","status":"COMPLETED","riskLevel":"LOW"}]`,
			wantApprove:   true,
			wantRiskLevel: "LOW",
			wantReason:    "SWEEP_AML_OK",
		},
		{
			name:          "triggered_medium_rejects",
			state:         "TRIGGERED",
			amlListRaw:    `[{"provider":"MistTrack","status":"COMPLETED","riskLevel":"MEDIUM"}]`,
			wantApprove:   false,
			wantRiskLevel: "MEDIUM",
			wantReason:    "SWEEP_AML_RISK_MEDIUM",
		},
		{
			name:          "triggered_high_rejects",
			state:         "TRIGGERED",
			amlListRaw:    `[{"provider":"MistTrack","status":"COMPLETED","riskLevel":"HIGH"}]`,
			wantApprove:   false,
			wantRiskLevel: "HIGH",
			wantReason:    "SWEEP_AML_RISK_HIGH",
		},
		{
			name:          "triggered_severe_rejects",
			state:         "TRIGGERED",
			amlListRaw:    `[{"provider":"MistTrack","status":"COMPLETED","riskLevel":"SEVERE"}]`,
			wantApprove:   false,
			wantRiskLevel: "SEVERE",
			wantReason:    "SWEEP_AML_RISK_SEVERE",
		},
		{
			name:          "triggered_unknown_rejects",
			state:         "TRIGGERED",
			amlListRaw:    `[{"provider":"MistTrack","status":"COMPLETED","riskLevel":"UNKNOWN"}]`,
			wantApprove:   false,
			wantRiskLevel: "UNKNOWN",
			wantReason:    "SWEEP_AML_RISK_UNKNOWN",
		},
		{
			name:          "triggered_pending_rejects",
			state:         "TRIGGERED",
			amlListRaw:    `[{"provider":"MistTrack","status":"PENDING","riskLevel":""}]`,
			wantApprove:   false,
			wantRiskLevel: "PENDING",
			wantReason:    "SWEEP_AML_RISK_PENDING",
		},
		{
			name:          "triggered_failed_rejects",
			state:         "TRIGGERED",
			amlListRaw:    `[{"provider":"MistTrack","status":"FAILED","riskLevel":""}]`,
			wantApprove:   false,
			wantRiskLevel: "FAILED",
			wantReason:    "SWEEP_AML_RISK_FAILED",
		},
		{
			name:          "triggered_skipped_rejects",
			state:         "TRIGGERED",
			amlListRaw:    `[{"provider":"MistTrack","status":"SKIPPED","riskLevel":""}]`,
			wantApprove:   false,
			wantRiskLevel: "SKIPPED",
			wantReason:    "SWEEP_AML_RISK_SKIPPED",
		},
		{
			name:          "triggered_empty_array_rejects",
			state:         "TRIGGERED",
			amlListRaw:    `[]`,
			wantApprove:   false,
			wantRiskLevel: "EMPTY",
			wantReason:    "SWEEP_AML_RISK_EMPTY",
		},
		{
			name:          "triggered_null_rejects",
			state:         "TRIGGERED",
			amlListRaw:    `null`,
			wantApprove:   false,
			wantRiskLevel: "EMPTY",
			wantReason:    "SWEEP_AML_RISK_EMPTY",
		},
		{
			name:          "triggered_no_amllist_rejects",
			state:         "TRIGGERED",
			amlListRaw:    "", // nil RawMessage
			wantApprove:   false,
			wantRiskLevel: "EMPTY",
			wantReason:    "SWEEP_AML_RISK_EMPTY",
		},
		{
			name:          "triggered_mixed_high_wins",
			state:         "TRIGGERED",
			amlListRaw:    `[{"provider":"MistTrack","status":"COMPLETED","riskLevel":"LOW"},{"provider":"Chainalysis","status":"COMPLETED","riskLevel":"HIGH"}]`,
			wantApprove:   false,
			wantRiskLevel: "HIGH",
			wantReason:    "SWEEP_AML_RISK_HIGH",
		},
		{
			name:          "triggered_invalid_json_rejects",
			state:         "TRIGGERED",
			amlListRaw:    `{not json`,
			wantApprove:   false,
			wantRiskLevel: "PARSE_FAILED",
			wantReason:    "SWEEP_AML_PARSE_FAILED",
		},
		// state 非 TRIGGERED
		{
			name:          "untriggered_rejects",
			state:         "UNTRIGGERED",
			amlListRaw:    "",
			wantApprove:   false,
			wantRiskLevel: "STATE_UNTRIGGERED",
			wantReason:    "SWEEP_AML_STATE_UNTRIGGERED",
		},
		{
			name:          "in_progress_rejects",
			state:         "IN_PROGRESS",
			amlListRaw:    "",
			wantApprove:   false,
			wantRiskLevel: "STATE_IN_PROGRESS",
			wantReason:    "SWEEP_AML_STATE_IN_PROGRESS",
		},
		{
			name:          "empty_state_rejects",
			state:         "",
			amlListRaw:    "",
			wantApprove:   false,
			wantRiskLevel: "STATE_MISSING",
			wantReason:    "SWEEP_AML_STATE_MISSING",
		},
		{
			name:          "unknown_state_rejects",
			state:         "WEIRD_VALUE",
			amlListRaw:    "",
			wantApprove:   false,
			wantRiskLevel: "STATE_WEIRD_VALUE",
			wantReason:    "SWEEP_AML_STATE_WEIRD_VALUE",
		},
		{
			name:          "triggered_unrecognized_aml_status_rejects",
			state:         "TRIGGERED",
			amlListRaw:    `[{"provider":"X","status":"PROCESSING","riskLevel":""}]`,
			wantApprove:   false,
			wantRiskLevel: "UNKNOWN",
			wantReason:    "SWEEP_AML_RISK_UNKNOWN",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var raw json.RawMessage
			if tt.amlListRaw != "" {
				raw = json.RawMessage(tt.amlListRaw)
			}
			got := DecideSweepAML(tt.state, raw)
			if got.Approve != tt.wantApprove {
				t.Errorf("Approve = %v, want %v", got.Approve, tt.wantApprove)
			}
			if got.RiskLevel != tt.wantRiskLevel {
				t.Errorf("RiskLevel = %q, want %q", got.RiskLevel, tt.wantRiskLevel)
			}
			if got.Reason != tt.wantReason {
				t.Errorf("Reason = %q, want %q", got.Reason, tt.wantReason)
			}
		})
	}
}
