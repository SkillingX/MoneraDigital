package safeheron

import "testing"

func TestSummarizeAmlRiskLevel(t *testing.T) {
	tests := []struct {
		name     string
		amlList  []AmlReport
		expected string
	}{
		{"COMPLETED LOW", []AmlReport{{Status: "COMPLETED", RiskLevel: "LOW"}}, AmlRiskLow},
		{"COMPLETED MEDIUM", []AmlReport{{Status: "COMPLETED", RiskLevel: "MEDIUM"}}, AmlRiskMedium},
		{"COMPLETED HIGH", []AmlReport{{Status: "COMPLETED", RiskLevel: "HIGH"}}, AmlRiskHigh},
		{"COMPLETED SEVERE", []AmlReport{{Status: "COMPLETED", RiskLevel: "SEVERE"}}, AmlRiskSevere},
		{"COMPLETED UNKNOWN", []AmlReport{{Status: "COMPLETED", RiskLevel: "UNKNOWN"}}, AmlRiskUnknown},
		{"PENDING status", []AmlReport{{Status: "PENDING", RiskLevel: ""}}, AmlRiskPending},
		{"FAILED status", []AmlReport{{Status: "FAILED", RiskLevel: ""}}, AmlRiskFailed},
		{"SKIPPED status", []AmlReport{{Status: "SKIPPED", RiskLevel: ""}}, AmlRiskSkipped},
		{"PENDING beats FAILED", []AmlReport{
			{Status: "FAILED", RiskLevel: ""},
			{Status: "PENDING", RiskLevel: ""},
		}, AmlRiskPending},
		{"multi COMPLETED takes highest", []AmlReport{
			{Status: "COMPLETED", RiskLevel: "LOW"},
			{Status: "COMPLETED", RiskLevel: "HIGH"},
			{Status: "COMPLETED", RiskLevel: "MEDIUM"},
		}, AmlRiskHigh},
		{"empty list returns EMPTY sentinel", []AmlReport{}, AmlRiskEmpty},
		{"nil list returns EMPTY sentinel", nil, AmlRiskEmpty},
		{"unrecognized status rejects (fail-closed)", []AmlReport{
			{Status: "PROCESSING", RiskLevel: ""},
		}, AmlRiskUnknown},
		{"all unrecognized statuses reject", []AmlReport{
			{Status: "REVIEWING", RiskLevel: ""},
			{Status: "PROCESSING", RiskLevel: "LOW"},
		}, AmlRiskUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SummarizeAmlRiskLevel(tt.amlList)
			if got != tt.expected {
				t.Errorf("SummarizeAmlRiskLevel() = %q, want %q", got, tt.expected)
			}
		})
	}
}
