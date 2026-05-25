package approval

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var (
	ErrDuplicateApproval  = errors.New("approval: duplicate approval_id")
	ErrApprovalNotFound   = errors.New("approval: record not found")
	ErrDuplicateSweepTx   = errors.New("approval: duplicate sweep tx_key")
	ErrSweepNotFound      = errors.New("approval: sweep tx_key not found")
	ErrSweepTerminalState = errors.New("approval: sweep tx already in terminal state")
)

type Repository interface {
	InsertApprovalRecord(ctx context.Context, rec *ApprovalRecord) error
	GetApprovalByID(ctx context.Context, approvalID string) (*ApprovalRecord, error)
	InsertSweepTransaction(ctx context.Context, st *SweepTransaction) error
	UpdateSweepStatus(ctx context.Context, txKey, status, subStatus, txHash string, completedAt *time.Time) error
}

type DBRepository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *DBRepository {
	return &DBRepository{db: db}
}

func (r *DBRepository) InsertApprovalRecord(ctx context.Context, rec *ApprovalRecord) error {
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO approval_records
		   (approval_id, callback_type, tx_type, action, reason,
		    tx_key, chain_symbol, coin_key, tx_amount,
		    source_account_key, destination_account_key,
		    destination_account_type, destination_address,
		    customer_ref_id, raw_request, aml_risk_level)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		 ON CONFLICT (approval_id) DO NOTHING
		 RETURNING id, created_at`,
		rec.ApprovalID, rec.CallbackType, nilIfEmpty(rec.TxType),
		rec.Action, nilIfEmpty(rec.Reason),
		nilIfEmpty(rec.TxKey), nilIfEmpty(rec.ChainSymbol),
		nilIfEmpty(rec.CoinKey), nilIfEmpty(rec.TxAmount),
		nilIfEmpty(rec.SourceAccountKey), nilIfEmpty(rec.DestinationAccountKey),
		nilIfEmpty(rec.DestinationAccountType), nilIfEmpty(rec.DestinationAddress),
		nilIfEmpty(rec.CustomerRefID), rec.RawRequest,
		nilIfEmpty(rec.AmlRiskLevel),
	).Scan(&rec.ID, &rec.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrDuplicateApproval
		}
		return fmt.Errorf("insert approval record: %w", err)
	}
	return nil
}

func (r *DBRepository) GetApprovalByID(ctx context.Context, approvalID string) (*ApprovalRecord, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT id, approval_id, callback_type,
		        COALESCE(tx_type, ''), action, COALESCE(reason, ''),
		        COALESCE(tx_key, ''), COALESCE(chain_symbol, 'UNKNOWN'),
		        COALESCE(coin_key, ''), COALESCE(tx_amount, ''),
		        COALESCE(source_account_key, ''), COALESCE(destination_account_key, ''),
		        COALESCE(destination_account_type, ''), COALESCE(destination_address, ''),
		        COALESCE(customer_ref_id, ''), raw_request,
		        COALESCE(aml_risk_level, ''), created_at
		 FROM approval_records WHERE approval_id = $1`,
		approvalID,
	)
	rec := &ApprovalRecord{}
	if err := row.Scan(
		&rec.ID, &rec.ApprovalID, &rec.CallbackType,
		&rec.TxType, &rec.Action, &rec.Reason,
		&rec.TxKey, &rec.ChainSymbol,
		&rec.CoinKey, &rec.TxAmount,
		&rec.SourceAccountKey, &rec.DestinationAccountKey,
		&rec.DestinationAccountType, &rec.DestinationAddress,
		&rec.CustomerRefID, &rec.RawRequest,
		&rec.AmlRiskLevel, &rec.CreatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrApprovalNotFound
		}
		return nil, fmt.Errorf("get approval by id: %w", err)
	}
	return rec, nil
}

func (r *DBRepository) InsertSweepTransaction(ctx context.Context, st *SweepTransaction) error {
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO sweep_transactions
		   (tx_key, tx_hash, customer_ref_id, tx_type,
		    chain_symbol, coin_key, fee_coin_key, tx_amount, estimate_fee,
		    source_account_key, source_address,
		    destination_account_key, destination_address,
		    tx_status, tx_sub_status,
		    approval_id, approval_action)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
		 ON CONFLICT (tx_key) DO NOTHING
		 RETURNING id, created_at, updated_at`,
		st.TxKey, nilIfEmpty(st.TxHash), nilIfEmpty(st.CustomerRefID),
		st.TxType, st.ChainSymbol, st.CoinKey,
		nilIfEmpty(st.FeeCoinKey), st.TxAmount, nilIfEmpty(st.EstimateFee),
		nilIfEmpty(st.SourceAccountKey), nilIfEmpty(st.SourceAddress),
		nilIfEmpty(st.DestinationAccountKey), nilIfEmpty(st.DestinationAddress),
		st.TxStatus, nilIfEmpty(st.TxSubStatus),
		nilIfEmpty(st.ApprovalID), nilIfEmpty(st.ApprovalAction),
	).Scan(&st.ID, &st.CreatedAt, &st.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrDuplicateSweepTx
		}
		return fmt.Errorf("insert sweep transaction: %w", err)
	}
	return nil
}

func (r *DBRepository) UpdateSweepStatus(ctx context.Context, txKey, status, subStatus, txHash string, completedAt *time.Time) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE sweep_transactions
		 SET tx_status = $2, tx_sub_status = $3, tx_hash = COALESCE($4, tx_hash),
		     completed_at = $5, updated_at = NOW()
		 WHERE tx_key = $1 AND tx_status NOT IN ('COMPLETED', 'FAILED')`,
		txKey, status, nilIfEmpty(subStatus), nilIfEmpty(txHash), completedAt,
	)
	if err != nil {
		return fmt.Errorf("update sweep status: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update sweep status rows affected: %w", err)
	}
	if n == 0 {
		var exists bool
		if qErr := r.db.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM sweep_transactions WHERE tx_key=$1)`,
			txKey,
		).Scan(&exists); qErr != nil {
			return fmt.Errorf("check sweep existence: %w", qErr)
		}
		if !exists {
			return ErrSweepNotFound
		}
		return ErrSweepTerminalState
	}
	return nil
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

var _ Repository = (*DBRepository)(nil)
