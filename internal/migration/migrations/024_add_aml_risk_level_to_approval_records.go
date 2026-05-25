package migrations

import (
	"database/sql"
	"fmt"
	"os"

	"monera-digital/internal/migration"
)

// AddAmlRiskLevelToApprovalRecords 给 approval_records 表加 aml_risk_level 列
// (v1.1 Phase 1 / D-AML-11)。
// 历史数据不回填，新列默认 NULL；非 AUTO_SWEEP/UTXO_COLLECTION 的审批记录也保持 NULL。
type AddAmlRiskLevelToApprovalRecords struct{}

func (m *AddAmlRiskLevelToApprovalRecords) Version() string { return "024" }
func (m *AddAmlRiskLevelToApprovalRecords) Description() string {
	return "Add aml_risk_level column to approval_records (v1.1 Phase 1 AML hard block)"
}

func (m *AddAmlRiskLevelToApprovalRecords) Up(db *sql.DB) error {
	_, err := db.Exec(`
		ALTER TABLE approval_records
			ADD COLUMN IF NOT EXISTS aml_risk_level VARCHAR(32);

		CREATE INDEX IF NOT EXISTS idx_approval_records_aml_risk
			ON approval_records(aml_risk_level);
	`)
	if err != nil {
		return fmt.Errorf("add aml_risk_level column: %w", err)
	}
	return nil
}

func (m *AddAmlRiskLevelToApprovalRecords) Down(db *sql.DB) error {
	if os.Getenv("APP_ENV") == "production" {
		return fmt.Errorf("BLOCKED: rollback of approval_records.aml_risk_level in production would lose audit trail; use a manual migration instead")
	}
	_, err := db.Exec(`
		DROP INDEX IF EXISTS idx_approval_records_aml_risk;
		ALTER TABLE approval_records DROP COLUMN IF EXISTS aml_risk_level;
	`)
	if err != nil {
		return fmt.Errorf("drop aml_risk_level column: %w", err)
	}
	return nil
}

var _ migration.Migration = (*AddAmlRiskLevelToApprovalRecords)(nil)
