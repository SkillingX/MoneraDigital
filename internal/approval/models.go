package approval

import (
	"encoding/json"
	"time"
)

type ApprovalRecord struct {
	ID                     int64           `db:"id"`
	ApprovalID             string          `db:"approval_id"`
	CallbackType           string          `db:"callback_type"`
	TxType                 string          `db:"tx_type"`
	Action                 string          `db:"action"`
	Reason                 string          `db:"reason"`
	TxKey                  string          `db:"tx_key"`
	ChainSymbol            string          `db:"chain_symbol"`
	CoinKey                string          `db:"coin_key"`
	TxAmount               string          `db:"tx_amount"`
	SourceAccountKey       string          `db:"source_account_key"`
	DestinationAccountKey  string          `db:"destination_account_key"`
	DestinationAccountType string          `db:"destination_account_type"`
	DestinationAddress     string          `db:"destination_address"`
	CustomerRefID          string          `db:"customer_ref_id"`
	RawRequest             json.RawMessage `db:"raw_request"`
	AmlRiskLevel           string          `db:"aml_risk_level"` // v1.1 Phase 1: AML 等级快照，非 AUTO_SWEEP/UTXO_COLLECTION 时为空（落 NULL）
	CreatedAt              time.Time       `db:"created_at"`
}

type SweepTransaction struct {
	ID                    int64      `db:"id"`
	TxKey                 string     `db:"tx_key"`
	TxHash                string     `db:"tx_hash"`
	CustomerRefID         string     `db:"customer_ref_id"`
	TxType                string     `db:"tx_type"`
	ChainSymbol           string     `db:"chain_symbol"`
	CoinKey               string     `db:"coin_key"`
	FeeCoinKey            string     `db:"fee_coin_key"`
	TxAmount              string     `db:"tx_amount"`
	EstimateFee           string     `db:"estimate_fee"`
	SourceAccountKey      string     `db:"source_account_key"`
	SourceAddress         string     `db:"source_address"`
	DestinationAccountKey string     `db:"destination_account_key"`
	DestinationAddress    string     `db:"destination_address"`
	TxStatus              string     `db:"tx_status"`
	TxSubStatus           string     `db:"tx_sub_status"`
	ApprovalID            string     `db:"approval_id"`
	ApprovalAction        string     `db:"approval_action"`
	CreatedAt             time.Time  `db:"created_at"`
	UpdatedAt             time.Time  `db:"updated_at"`
	CompletedAt           *time.Time `db:"completed_at"`
}

type ApprovalConfig struct {
	SweepTargetAccounts []string
	AllowedTxTypes      []string
}
