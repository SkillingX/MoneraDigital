package approval

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func newMock(t *testing.T) (*DBRepository, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewRepository(db), mock
}

// ---------------------------------------------------------------------------
// InsertApprovalRecord
// ---------------------------------------------------------------------------

func TestInsertApprovalRecord_Success(t *testing.T) {
	repo, mock := newMock(t)
	now := time.Now()

	mock.ExpectQuery(`INSERT INTO approval_records`).
		WithArgs(
			"ap-1", "TRANSACTION", "AUTO_SWEEP", "APPROVE", nil,
			"tx-1", "ETH", "USDT_ERC20", "100",
			nil, "acct-main", "VAULT_ACCOUNT", "0xabc",
			nil, json.RawMessage(`{}`),
			"LOW",
		).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(1, now))

	rec := &ApprovalRecord{
		ApprovalID:             "ap-1",
		CallbackType:           "TRANSACTION",
		TxType:                 "AUTO_SWEEP",
		Action:                 "APPROVE",
		TxKey:                  "tx-1",
		ChainSymbol:            "ETH",
		CoinKey:                "USDT_ERC20",
		TxAmount:               "100",
		DestinationAccountKey:  "acct-main",
		DestinationAccountType: "VAULT_ACCOUNT",
		DestinationAddress:     "0xabc",
		RawRequest:             json.RawMessage(`{}`),
		AmlRiskLevel:           "LOW",
	}

	if err := repo.InsertApprovalRecord(context.Background(), rec); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.ID != 1 {
		t.Errorf("id = %d, want 1", rec.ID)
	}
	if rec.CreatedAt.IsZero() {
		t.Error("createdAt should not be zero")
	}
}

func TestInsertApprovalRecord_Duplicate(t *testing.T) {
	repo, mock := newMock(t)

	mock.ExpectQuery(`INSERT INTO approval_records`).
		WithArgs(
			"ap-1", "TRANSACTION", "AUTO_SWEEP", "APPROVE", nil,
			"tx-1", "ETH", "USDT_ERC20", "100",
			nil, "acct-main", "VAULT_ACCOUNT", "0xabc",
			nil, json.RawMessage(`{}`),
			nil,
		).
		WillReturnError(sql.ErrNoRows)

	rec := &ApprovalRecord{
		ApprovalID:             "ap-1",
		CallbackType:           "TRANSACTION",
		TxType:                 "AUTO_SWEEP",
		Action:                 "APPROVE",
		TxKey:                  "tx-1",
		ChainSymbol:            "ETH",
		CoinKey:                "USDT_ERC20",
		TxAmount:               "100",
		DestinationAccountKey:  "acct-main",
		DestinationAccountType: "VAULT_ACCOUNT",
		DestinationAddress:     "0xabc",
		RawRequest:             json.RawMessage(`{}`),
	}

	err := repo.InsertApprovalRecord(context.Background(), rec)
	if err != ErrDuplicateApproval {
		t.Fatalf("expected ErrDuplicateApproval, got %v", err)
	}
}

func TestInsertApprovalRecord_DBError(t *testing.T) {
	repo, mock := newMock(t)

	mock.ExpectQuery(`INSERT INTO approval_records`).
		WillReturnError(sql.ErrConnDone)

	rec := &ApprovalRecord{
		ApprovalID:   "ap-1",
		CallbackType: "CALLBACK_TEST",
		Action:       "APPROVE",
		RawRequest:   json.RawMessage(`{}`),
	}

	err := repo.InsertApprovalRecord(context.Background(), rec)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestInsertApprovalRecord_CallbackTest(t *testing.T) {
	repo, mock := newMock(t)
	now := time.Now()

	mock.ExpectQuery(`INSERT INTO approval_records`).
		WithArgs(
			"test-1", "CALLBACK_TEST", nil, "APPROVE", nil,
			nil, nil, nil, nil,
			nil, nil, nil, nil,
			nil, json.RawMessage(`{}`),
			nil,
		).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(2, now))

	rec := &ApprovalRecord{
		ApprovalID:   "test-1",
		CallbackType: "CALLBACK_TEST",
		Action:       "APPROVE",
		RawRequest:   json.RawMessage(`{}`),
	}

	if err := repo.InsertApprovalRecord(context.Background(), rec); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.ID != 2 {
		t.Errorf("id = %d, want 2", rec.ID)
	}
}

// ---------------------------------------------------------------------------
// GetApprovalByID
// ---------------------------------------------------------------------------

func TestGetApprovalByID_Found(t *testing.T) {
	repo, mock := newMock(t)
	now := time.Now()

	mock.ExpectQuery(`SELECT .+ FROM approval_records WHERE approval_id`).
		WithArgs("ap-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "approval_id", "callback_type",
			"tx_type", "action", "reason",
			"tx_key", "chain_symbol",
			"coin_key", "tx_amount",
			"source_account_key", "destination_account_key",
			"destination_account_type", "destination_address",
			"customer_ref_id", "raw_request",
			"aml_risk_level", "created_at",
		}).AddRow(
			1, "ap-1", "TRANSACTION",
			"AUTO_SWEEP", "APPROVE", "",
			"tx-1", "ETH",
			"USDT_ERC20", "100",
			"", "acct-main",
			"VAULT_ACCOUNT", "0xabc",
			"", json.RawMessage(`{}`),
			"HIGH", now,
		))

	rec, err := repo.GetApprovalByID(context.Background(), "ap-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Action != "APPROVE" {
		t.Errorf("action = %q, want APPROVE", rec.Action)
	}
	if rec.ChainSymbol != "ETH" {
		t.Errorf("chainSymbol = %q, want ETH", rec.ChainSymbol)
	}
	if rec.AmlRiskLevel != "HIGH" {
		t.Errorf("amlRiskLevel = %q, want HIGH", rec.AmlRiskLevel)
	}
}

func TestGetApprovalByID_NotFound(t *testing.T) {
	repo, mock := newMock(t)

	mock.ExpectQuery(`SELECT .+ FROM approval_records WHERE approval_id`).
		WithArgs("nonexistent").
		WillReturnError(sql.ErrNoRows)

	_, err := repo.GetApprovalByID(context.Background(), "nonexistent")
	if err != ErrApprovalNotFound {
		t.Fatalf("expected ErrApprovalNotFound, got %v", err)
	}
}

func TestGetApprovalByID_DBError(t *testing.T) {
	repo, mock := newMock(t)

	mock.ExpectQuery(`SELECT .+ FROM approval_records WHERE approval_id`).
		WithArgs("ap-1").
		WillReturnError(sql.ErrConnDone)

	_, err := repo.GetApprovalByID(context.Background(), "ap-1")
	if err == nil {
		t.Fatal("expected error")
	}
}

// ---------------------------------------------------------------------------
// InsertSweepTransaction
// ---------------------------------------------------------------------------

func TestInsertSweepTransaction_Success(t *testing.T) {
	repo, mock := newMock(t)
	now := time.Now()

	mock.ExpectQuery(`INSERT INTO sweep_transactions`).
		WithArgs(
			"tx-1", nil, nil,
			"AUTO_SWEEP", "ETH", "USDT_ERC20",
			"ETH", "100", "0.001",
			"src-acct", "0xdef",
			"dst-acct", "0xabc",
			"PENDING", nil,
			"ap-1", "APPROVE",
		).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at", "updated_at"}).AddRow(1, now, now))

	st := &SweepTransaction{
		TxKey:                 "tx-1",
		TxType:                "AUTO_SWEEP",
		ChainSymbol:           "ETH",
		CoinKey:               "USDT_ERC20",
		FeeCoinKey:            "ETH",
		TxAmount:              "100",
		EstimateFee:           "0.001",
		SourceAccountKey:      "src-acct",
		SourceAddress:         "0xdef",
		DestinationAccountKey: "dst-acct",
		DestinationAddress:    "0xabc",
		TxStatus:              "PENDING",
		ApprovalID:            "ap-1",
		ApprovalAction:        "APPROVE",
	}

	if err := repo.InsertSweepTransaction(context.Background(), st); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if st.ID != 1 {
		t.Errorf("id = %d, want 1", st.ID)
	}
}

func TestInsertSweepTransaction_Duplicate(t *testing.T) {
	repo, mock := newMock(t)

	// ON CONFLICT DO NOTHING + RETURNING yields no rows on duplicate tx_key
	mock.ExpectQuery(`INSERT INTO sweep_transactions`).
		WillReturnError(sql.ErrNoRows)

	st := &SweepTransaction{
		TxKey:       "tx-dup",
		TxType:      "AUTO_SWEEP",
		CoinKey:     "ETH",
		TxAmount:    "1",
		TxStatus:    "PENDING",
		ChainSymbol: "ETH",
	}

	err := repo.InsertSweepTransaction(context.Background(), st)
	if err != ErrDuplicateSweepTx {
		t.Fatalf("expected ErrDuplicateSweepTx, got %v", err)
	}
}

func TestInsertSweepTransaction_DBError(t *testing.T) {
	repo, mock := newMock(t)

	mock.ExpectQuery(`INSERT INTO sweep_transactions`).
		WillReturnError(sql.ErrConnDone)

	st := &SweepTransaction{
		TxKey:       "tx-1",
		TxType:      "AUTO_SWEEP",
		CoinKey:     "ETH",
		TxAmount:    "1",
		TxStatus:    "PENDING",
		ChainSymbol: "ETH",
	}

	err := repo.InsertSweepTransaction(context.Background(), st)
	if err == nil {
		t.Fatal("expected error")
	}
}

// ---------------------------------------------------------------------------
// UpdateSweepStatus
// ---------------------------------------------------------------------------

func TestUpdateSweepStatus_Success(t *testing.T) {
	repo, mock := newMock(t)
	now := time.Now()

	mock.ExpectExec(`UPDATE sweep_transactions`).
		WithArgs("tx-1", "COMPLETED", nil, "0xhash", &now).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := repo.UpdateSweepStatus(context.Background(), "tx-1", "COMPLETED", "", "0xhash", &now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestUpdateSweepStatus_AlreadyTerminal(t *testing.T) {
	repo, mock := newMock(t)

	mock.ExpectExec(`UPDATE sweep_transactions`).
		WithArgs("terminal-tx", "COMPLETED", nil, "0xhash", (*time.Time)(nil)).
		WillReturnResult(sqlmock.NewResult(0, 0))

	mock.ExpectQuery(`SELECT EXISTS`).
		WithArgs("terminal-tx").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	err := repo.UpdateSweepStatus(context.Background(), "terminal-tx", "COMPLETED", "", "0xhash", nil)
	if !errors.Is(err, ErrSweepTerminalState) {
		t.Fatalf("expected ErrSweepTerminalState for already-terminal tx, got %v", err)
	}
}

func TestUpdateSweepStatus_WithSubStatus(t *testing.T) {
	repo, mock := newMock(t)

	mock.ExpectExec(`UPDATE sweep_transactions`).
		WithArgs("tx-1", "FAILED", "INSUFFICIENT_BALANCE", nil, (*time.Time)(nil)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := repo.UpdateSweepStatus(context.Background(), "tx-1", "FAILED", "INSUFFICIENT_BALANCE", "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// M-4: tx_key not in sweep_transactions at all → ErrSweepNotFound (not sql.ErrNoRows)
func TestUpdateSweepStatus_TxKeyNotFound_Distinct(t *testing.T) {
	repo, mock := newMock(t)

	mock.ExpectExec(`UPDATE sweep_transactions`).
		WithArgs("nonexistent-tx", "COMPLETED", nil, "0xhash", (*time.Time)(nil)).
		WillReturnResult(sqlmock.NewResult(0, 0))

	mock.ExpectQuery(`SELECT EXISTS`).
		WithArgs("nonexistent-tx").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	err := repo.UpdateSweepStatus(context.Background(), "nonexistent-tx", "COMPLETED", "", "0xhash", nil)
	if !errors.Is(err, ErrSweepNotFound) {
		t.Fatalf("expected ErrSweepNotFound for nonexistent tx_key, got %v", err)
	}
}

func TestUpdateSweepStatus_ExistenceCheckDBError(t *testing.T) {
	repo, mock := newMock(t)

	mock.ExpectExec(`UPDATE sweep_transactions`).
		WithArgs("tx-1", "COMPLETED", nil, nil, (*time.Time)(nil)).
		WillReturnResult(sqlmock.NewResult(0, 0))

	mock.ExpectQuery(`SELECT EXISTS`).
		WithArgs("tx-1").
		WillReturnError(sql.ErrConnDone)

	err := repo.UpdateSweepStatus(context.Background(), "tx-1", "COMPLETED", "", "", nil)
	if err == nil {
		t.Fatal("expected error when existence check fails")
	}
	if !strings.Contains(err.Error(), "check sweep existence") {
		t.Errorf("error should mention existence check, got: %v", err)
	}
}

func TestUpdateSweepStatus_DBError(t *testing.T) {
	repo, mock := newMock(t)

	mock.ExpectExec(`UPDATE sweep_transactions`).
		WillReturnError(sql.ErrConnDone)

	err := repo.UpdateSweepStatus(context.Background(), "tx-1", "COMPLETED", "", "", nil)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestUpdateSweepStatus_RowsAffectedError(t *testing.T) {
	repo, mock := newMock(t)

	mock.ExpectExec(`UPDATE sweep_transactions`).
		WithArgs("tx-1", "COMPLETED", nil, nil, (*time.Time)(nil)).
		WillReturnResult(sqlmock.NewErrorResult(errors.New("driver: rows affected not supported")))

	err := repo.UpdateSweepStatus(context.Background(), "tx-1", "COMPLETED", "", "", nil)
	if err == nil {
		t.Fatal("expected error when RowsAffected fails")
	}
	if !strings.Contains(err.Error(), "rows affected") {
		t.Errorf("error should mention rows affected, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Interface compliance
// ---------------------------------------------------------------------------

func TestDBRepository_ImplementsRepository(t *testing.T) {
	var _ Repository = (*DBRepository)(nil)
}

// ---------------------------------------------------------------------------
// nilIfEmpty helper
// ---------------------------------------------------------------------------

func TestNilIfEmpty(t *testing.T) {
	if v := nilIfEmpty(""); v != nil {
		t.Errorf("nilIfEmpty(\"\") = %v, want nil", v)
	}
	if v := nilIfEmpty("hello"); v != "hello" {
		t.Errorf("nilIfEmpty(\"hello\") = %v, want \"hello\"", v)
	}
}
