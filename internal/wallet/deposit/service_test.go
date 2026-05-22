package deposit

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"monera-digital/internal/safeheron"
	walletconfig "monera-digital/internal/wallet/config"
)

// mockRepo records every write so each test can assert on the resulting
// state without touching a real DB. ProcessOne expects BeginTx → Lock → write
// → MarkEventDone/Error → Commit, so we capture each call ordered.
type mockRepo struct {
	mu sync.Mutex

	beginTxErr error
	pending    []*Event
	owners     map[string]int
	ownerErr   error

	deposits      map[string]*DepositRow // keyed by safeheron_tx_key
	depositErr    error
	upsertCalls   int
	failedUpdates map[int64]string
	manualUpdates map[int64]string
	creditedIDs   map[int64]bool

	lastLookupNetworkFamily string // captures networkFamily arg from LookupAddressOwner

	accountID     int64
	accountErr    error
	creditErr     error
	newBalance    string
	journalCalls  []*JournalEntry
	journalErr    error
	creditAccount string

	doneIDs  []int64
	errorIDs []struct {
		id  int64
		msg string
	}
	markErrorErr       error // forces MarkEventError to fail (T7-I-5)
	findDepositByTxErr error // forces FindDepositByTxKey to fail

	commitCalls           int
	rollbackCalls         int
	beginTxCalls          int     // tracks how many tx were begun (T7-I-5 hot-loop check)
	noTxIncrements        []int64 // recorded eventIDs of IncrementEventAttemptsNoTx calls (T10 C-1/I-2)
	rollbackBeforeNoTxInc bool    // true means each noTx increment is observed AFTER at least one rollback

	commitErr   error // injected commit failure for fakeTx
	markDoneErr error // forces MarkEventDone to fail
	markMRErr   error // forces MarkDepositManualReview to fail
}

func newMockRepo() *mockRepo {
	return &mockRepo{
		owners:        map[string]int{},
		deposits:      map[string]*DepositRow{},
		failedUpdates: map[int64]string{},
		manualUpdates: map[int64]string{},
		creditedIDs:   map[int64]bool{},
	}
}

func (m *mockRepo) BeginTx(_ context.Context) (Tx, error) {
	m.mu.Lock()
	m.beginTxCalls++
	m.mu.Unlock()
	if m.beginTxErr != nil {
		return nil, m.beginTxErr
	}
	return &fakeTx{mu: &m.mu, commits: &m.commitCalls, rollbacks: &m.rollbackCalls, commitErr: m.commitErr}, nil
}

type fakeTx struct {
	mu        *sync.Mutex
	commits   *int
	rollbacks *int
	commitErr error // injected commit failure
}

func (f *fakeTx) Commit() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	*f.commits++
	if f.commitErr != nil {
		return f.commitErr
	}
	return nil
}
func (f *fakeTx) Rollback() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	*f.rollbacks++
	return nil
}

func (m *mockRepo) InsertEventOrSkip(ctx context.Context, evt *Event) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.pending {
		if p.EventID == evt.EventID {
			return false, nil
		}
	}
	clone := *evt
	clone.ID = int64(len(m.pending) + 1)
	m.pending = append(m.pending, &clone)
	return true, nil
}

func (m *mockRepo) LockNextPendingEvent(_ context.Context, _ Tx) (*Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, p := range m.pending {
		if p.ProcessStatus == ProcessPending || p.ProcessStatus == "" {
			p.ProcessStatus = "LOCKED"
			return m.pending[i], nil
		}
	}
	return nil, ErrNoPending
}

func (m *mockRepo) UpsertDeposit(_ context.Context, _ Tx, d *DepositRow) (*DepositRow, error) {
	if m.depositErr != nil {
		return nil, m.depositErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.upsertCalls++
	if existing, ok := m.deposits[d.SafeheronTxKey]; ok {
		// Apply status_rank guard: only update when new rank is >= existing.
		if d.StatusRank >= existing.StatusRank {
			merged := *existing
			merged.SafeheronStatus = d.SafeheronStatus
			merged.SafeheronSubStatus = d.SafeheronSubStatus
			merged.StatusRank = d.StatusRank
			merged.BlockHeight = d.BlockHeight
			merged.BlockHash = d.BlockHash
			merged.SafeheronCoinKey = d.SafeheronCoinKey // R2-I-1: mirror SQL DO UPDATE SET
			m.deposits[d.SafeheronTxKey] = &merged
			return &merged, nil
		}
		out := *existing
		return &out, nil
	}
	d.ID = int64(len(m.deposits) + 1)
	clone := *d
	m.deposits[d.SafeheronTxKey] = &clone
	return &clone, nil
}

func (m *mockRepo) FindOrCreateAccountForUpdate(_ context.Context, _ Tx, userID int, currency string) (int64, string, error) {
	if m.accountErr != nil {
		return 0, "", m.accountErr
	}
	if m.accountID == 0 {
		m.accountID = 7777
	}
	return m.accountID, "0", nil
}

func (m *mockRepo) CreditAccount(_ context.Context, _ Tx, _ int64, amount string) (string, error) {
	if m.creditErr != nil {
		return "", m.creditErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.creditAccount += "+" + amount
	if m.newBalance == "" {
		m.newBalance = amount
	}
	return m.newBalance, nil
}

func (m *mockRepo) WriteJournal(_ context.Context, _ Tx, j *JournalEntry) error {
	if m.journalErr != nil {
		return m.journalErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	clone := *j
	m.journalCalls = append(m.journalCalls, &clone)
	return nil
}

func (m *mockRepo) MarkDepositCredited(_ context.Context, _ Tx, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.creditedIDs[id] = true
	for _, d := range m.deposits {
		if d.ID == id {
			d.Status = DepositStatusCredited
		}
	}
	return nil
}

func (m *mockRepo) MarkDepositFailed(_ context.Context, _ Tx, id int64, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range m.deposits {
		if d.ID == id && (d.Status == DepositStatusCredited || d.Status == DepositStatusManualReview) {
			return ErrDepositTerminalState
		}
	}
	m.failedUpdates[id] = reason
	for _, d := range m.deposits {
		if d.ID == id {
			d.Status = DepositStatusFailed
		}
	}
	return nil
}

func (m *mockRepo) MarkDepositManualReview(_ context.Context, _ Tx, id int64, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.markMRErr != nil {
		return m.markMRErr
	}
	for _, d := range m.deposits {
		if d.ID == id && (d.Status == DepositStatusCredited || d.Status == DepositStatusFailed) {
			return ErrDepositTerminalState
		}
	}
	m.manualUpdates[id] = reason
	for _, d := range m.deposits {
		if d.ID == id {
			d.Status = DepositStatusManualReview
		}
	}
	return nil
}

func (m *mockRepo) MarkEventDone(_ context.Context, _ Tx, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.markDoneErr != nil {
		return m.markDoneErr
	}
	m.doneIDs = append(m.doneIDs, id)
	return nil
}

func (m *mockRepo) MarkEventError(_ context.Context, _ Tx, id int64, msg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.markErrorErr != nil {
		return m.markErrorErr
	}
	m.errorIDs = append(m.errorIDs, struct {
		id  int64
		msg string
	}{id, msg})
	return nil
}

// === AML/KYT mock methods ===

func (m *mockRepo) UpdateAMLFields(_ context.Context, _ Tx, _ int64, _, _ string, _ time.Time, _ []byte) error {
	return nil
}

func (m *mockRepo) MoveToKYTPending(_ context.Context, _ Tx, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range m.deposits {
		if d.ID == id {
			if d.Status != DepositStatusPending {
				return ErrDepositNotPending
			}
			d.Status = DepositStatusKYTPending
			return nil
		}
	}
	return ErrDepositNotPending
}

func (m *mockRepo) LockOneKYTPendingTimeout(_ context.Context, _ Tx, _ time.Duration) (*DepositRow, error) {
	return nil, ErrNoPending
}

func (m *mockRepo) LockOneAmlPending(_ context.Context, _ Tx, _ time.Duration) (*DepositRow, error) {
	return nil, ErrNoPending
}

func (m *mockRepo) FindDepositByTxKey(_ context.Context, _ Tx, txKey string) (*DepositRow, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.findDepositByTxErr != nil {
		return nil, false, m.findDepositByTxErr
	}
	if d, ok := m.deposits[txKey]; ok {
		return d, true, nil
	}
	return nil, false, nil
}

func (m *mockRepo) IncrementEventAttemptsNoTx(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.noTxIncrements = append(m.noTxIncrements, id)
	// Record whether at least one rollback was observed before this call.
	// Used to assert C-1: NoTx update must come after the tx lock is released.
	if m.rollbackCalls > 0 {
		m.rollbackBeforeNoTxInc = true
	}
	return nil
}

func (m *mockRepo) LookupAddressOwner(_ context.Context, addr, networkFamily string) (int, bool, error) {
	m.mu.Lock()
	m.lastLookupNetworkFamily = networkFamily
	m.mu.Unlock()
	if m.ownerErr != nil {
		return 0, false, m.ownerErr
	}
	uid, ok := m.owners[addr]
	return uid, ok, nil
}

func newTestRegistry(symbol, chainCode, safeheronKey, minAmount string, coinID int) *stubRegistry {
	return newTestRegistryWithNetwork(symbol, chainCode, "EVM", safeheronKey, minAmount, coinID)
}

func newTestRegistryWithNetwork(symbol, chainCode, networkFamily, safeheronKey, minAmount string, coinID int) *stubRegistry {
	return &stubRegistry{
		byKey: map[string]*walletconfig.CoinChain{
			safeheronKey: {
				ID:        coinID,
				ChainCode: chainCode,
				Chain: &walletconfig.Chain{
					Code:          chainCode,
					NetworkFamily: networkFamily,
				},
				Coin:             &walletconfig.Coin{ID: coinID, Symbol: symbol},
				SafeheronCoinKey: safeheronKey,
				MinDepositAmount: minAmount,
			},
		},
	}
}

type stubRegistry struct {
	byKey map[string]*walletconfig.CoinChain
}

func (s *stubRegistry) GetCoinChainBySafeheronKey(key string) (*walletconfig.CoinChain, bool) {
	cc, ok := s.byKey[key]
	return cc, ok
}

type capturedAlert struct {
	level  string
	title  string
	fields map[string]string
}

func newAlertCollector() (AlertFunc, *[]capturedAlert) {
	var alerts []capturedAlert
	mu := &sync.Mutex{}
	fn := func(level, title string, fields map[string]string) {
		mu.Lock()
		defer mu.Unlock()
		alerts = append(alerts, capturedAlert{level, title, fields})
	}
	return fn, &alerts
}

func enqueueRaw(t *testing.T, repo *mockRepo, env PayloadEnvelope) *Event {
	t.Helper()
	raw, err := MarshalRawPayload(env)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	evt := &Event{
		EventID:        env.EventDetail.TxKey + ":" + env.EventDetail.TransactionStatus,
		EventType:      env.EventType,
		SafeheronTxKey: env.EventDetail.TxKey,
		RawPayload:     raw,
	}
	if _, err := repo.InsertEventOrSkip(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
	return evt
}

func newSvc(t *testing.T, repo *mockRepo, reg ChainsRegistry, alertFn AlertFunc) *Service {
	t.Helper()
	s := NewService(repo, reg, alertFn)
	counter := 0
	s.SetSerialFunc(func() string {
		counter++
		return "DPS-test-" + intToStr(counter)
	})
	return s
}

func intToStr(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	var out []byte
	for i > 0 {
		out = append([]byte{digits[i%10]}, out...)
		i /= 10
	}
	return string(out)
}

// ------------------- happy path -------------------

func TestProcessOne_CompletedConfirmedCredits(t *testing.T) {
	repo := newMockRepo()
	repo.owners["0xdest"] = 42
	reg := newTestRegistry("ETH", "ETHEREUM", "ETH(SEPOLIA)_ETHEREUM_SEPOLIA", "0.0001", 11)
	alert, alerts := newAlertCollector()
	svc := newSvc(t, repo, reg, alert)

	enqueueRaw(t, repo, PayloadEnvelope{
		EventType: "TRANSACTION_STATUS_CHANGED",
		EventDetail: PayloadEventDetail{
			TxKey:                "tx-1",
			CoinKey:              "ETH(SEPOLIA)_ETHEREUM_SEPOLIA",
			TxAmount:             "0.5",
			TransactionStatus:    "COMPLETED",
			TransactionSubStatus: "CONFIRMED",
			TransactionDirection: "INFLOW",
			DestinationAddress:   "0xdest",
		},
	})

	processed, err := svc.ProcessOne(context.Background())
	if err != nil || !processed {
		t.Fatalf("expected processed=true err=nil, got %v / %v", processed, err)
	}
	if !repo.creditedIDs[1] {
		t.Fatalf("expected deposit to be CREDITED, state=%+v", repo.deposits)
	}
	if len(repo.journalCalls) != 1 || repo.journalCalls[0].BizType != JournalBizTypeDeposit {
		t.Fatalf("expected 1 journal entry (biz_type=10), got %+v", repo.journalCalls)
	}
	if len(repo.doneIDs) != 1 {
		t.Fatalf("expected event marked DONE")
	}
	if len(*alerts) != 0 {
		t.Fatalf("happy path should not alert, got %+v", *alerts)
	}
}

func TestProcessOne_PassesNetworkFamilyToLookupAddressOwner(t *testing.T) {
	repo := newMockRepo()
	repo.owners["0xdest"] = 42
	reg := newTestRegistryWithNetwork("ETH", "ETHEREUM", "EVM", "ETH(SEPOLIA)_ETHEREUM_SEPOLIA", "0.0001", 11)
	svc := newSvc(t, repo, reg, nil)

	enqueueRaw(t, repo, PayloadEnvelope{
		EventType: "TRANSACTION_STATUS_CHANGED",
		EventDetail: PayloadEventDetail{
			TxKey:                "tx-nf",
			CoinKey:              "ETH(SEPOLIA)_ETHEREUM_SEPOLIA",
			TxAmount:             "0.5",
			TransactionStatus:    "COMPLETED",
			TransactionSubStatus: "CONFIRMED",
			TransactionDirection: "INFLOW",
			DestinationAddress:   "0xdest",
		},
	})

	if _, err := svc.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if repo.lastLookupNetworkFamily != "EVM" {
		t.Errorf("expected networkFamily=EVM passed to LookupAddressOwner, got %q", repo.lastLookupNetworkFamily)
	}
}

func TestProcessOne_TRONNetworkFamily(t *testing.T) {
	repo := newMockRepo()
	repo.owners["TAddr1"] = 42
	reg := newTestRegistryWithNetwork("TRX", "TRON", "TRON", "TRX(SHASTA)_TRON_TESTNET", "0.1", 20)
	svc := newSvc(t, repo, reg, nil)

	enqueueRaw(t, repo, PayloadEnvelope{
		EventType: "TRANSACTION_STATUS_CHANGED",
		EventDetail: PayloadEventDetail{
			TxKey:                "tx-tron",
			CoinKey:              "TRX(SHASTA)_TRON_TESTNET",
			TxAmount:             "10",
			TransactionStatus:    "COMPLETED",
			TransactionSubStatus: "CONFIRMED",
			TransactionDirection: "INFLOW",
			DestinationAddress:   "TAddr1",
		},
	})

	if _, err := svc.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if repo.lastLookupNetworkFamily != "TRON" {
		t.Errorf("expected networkFamily=TRON for TRON chain, got %q", repo.lastLookupNetworkFamily)
	}
}

// TestProcessOne_PopulatesSafeheronCoinKey verifies that deposits rows carry
// the registry's safeheron_coin_key so the deposits table can be reconciled
// against coin_chains.safeheron_coin_key for dashboard / ops reporting.
// Regression: R2-I-1 (continuation of T7-I-2) — repository previously stored
// an empty string for this column unconditionally.
func TestProcessOne_PopulatesSafeheronCoinKey(t *testing.T) {
	repo := newMockRepo()
	repo.owners["0xdest"] = 42
	reg := newTestRegistry("USDT", "ETHEREUM", "USDT_ERC20", "1", 4)
	svc := newSvc(t, repo, reg, nil)

	enqueueRaw(t, repo, PayloadEnvelope{
		EventType: "TRANSACTION_STATUS_CHANGED",
		EventDetail: PayloadEventDetail{
			TxKey:                "tx-coin-key",
			CoinKey:              "USDT_ERC20",
			TxAmount:             "10",
			TransactionStatus:    "COMPLETED",
			TransactionSubStatus: "CONFIRMED",
			TransactionDirection: "INFLOW",
			DestinationAddress:   "0xdest",
		},
	})

	if _, err := svc.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	row := repo.deposits["tx-coin-key"]
	if row == nil {
		t.Fatal("expected deposit row to be written")
	}
	if row.SafeheronCoinKey != "USDT_ERC20" {
		t.Errorf("deposit.SafeheronCoinKey must equal registry coin key for reconciliation; got %q want %q",
			row.SafeheronCoinKey, "USDT_ERC20")
	}
}

// ------------------- idempotency -------------------

func TestProcessOne_RepeatedCompletedDoesNotDoubleCredit(t *testing.T) {
	repo := newMockRepo()
	repo.owners["0xdest"] = 42
	reg := newTestRegistry("ETH", "ETHEREUM", "K", "0.0001", 11)
	svc := newSvc(t, repo, reg, nil)

	enqueueRaw(t, repo, PayloadEnvelope{
		EventType: "TRANSACTION_STATUS_CHANGED",
		EventDetail: PayloadEventDetail{
			TxKey:                "tx-1",
			CoinKey:              "K",
			TxAmount:             "0.5",
			TransactionStatus:    "COMPLETED",
			TransactionSubStatus: "CONFIRMED",
			TransactionDirection: "INFLOW",
			DestinationAddress:   "0xdest",
		},
	})
	if _, err := svc.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Repeat the same event a second time — should be no-op because
	// deposits.status is now CREDITED.
	enqueueRaw(t, repo, PayloadEnvelope{
		EventType: "TRANSACTION_STATUS_CHANGED",
		EventDetail: PayloadEventDetail{
			TxKey:                "tx-1",
			CoinKey:              "K",
			TxAmount:             "0.5",
			TransactionStatus:    "COMPLETED",
			TransactionSubStatus: "CONFIRMED",
			TransactionDirection: "INFLOW",
			DestinationAddress:   "0xdest",
		},
	})
	if _, err := svc.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(repo.journalCalls) != 1 {
		t.Errorf("expected 1 journal call (no double credit), got %d", len(repo.journalCalls))
	}
}

// ------------------- status-rank monotonicity -------------------

func TestProcessOne_OutOfOrderCompletedThenConfirmingNoRegress(t *testing.T) {
	repo := newMockRepo()
	repo.owners["0xdest"] = 42
	reg := newTestRegistry("ETH", "ETHEREUM", "K", "0.0001", 11)
	svc := newSvc(t, repo, reg, nil)

	enqueueRaw(t, repo, PayloadEnvelope{
		EventType: "TRANSACTION_STATUS_CHANGED",
		EventDetail: PayloadEventDetail{
			TxKey:                "tx-1",
			CoinKey:              "K",
			TxAmount:             "1",
			TransactionStatus:    "COMPLETED",
			TransactionSubStatus: "CONFIRMED",
			TransactionDirection: "INFLOW",
			DestinationAddress:   "0xdest",
		},
	})
	if _, err := svc.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Now an out-of-order CONFIRMING arrives.
	enqueueRaw(t, repo, PayloadEnvelope{
		EventType: "TRANSACTION_STATUS_CHANGED",
		EventDetail: PayloadEventDetail{
			TxKey:                "tx-1",
			CoinKey:              "K",
			TxAmount:             "1",
			TransactionStatus:    "CONFIRMING",
			TransactionSubStatus: "",
			TransactionDirection: "INFLOW",
			DestinationAddress:   "0xdest",
		},
	})
	if _, err := svc.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	dep := repo.deposits["tx-1"]
	if dep.StatusRank != StatusRank("COMPLETED") {
		t.Errorf("expected status_rank to remain COMPLETED=100, got %d", dep.StatusRank)
	}
	if dep.Status != DepositStatusCredited {
		t.Errorf("expected deposit status to remain CREDITED, got %s", dep.Status)
	}
	if len(repo.journalCalls) != 1 {
		t.Errorf("expected exactly 1 journal entry, got %d", len(repo.journalCalls))
	}
}

// ------------------- routing failures → MANUAL_REVIEW -------------------

func TestProcessOne_AddressUnassigned_FlagsManualReview(t *testing.T) {
	repo := newMockRepo()
	reg := newTestRegistry("ETH", "ETHEREUM", "K", "0.0001", 11)
	alertFn, alerts := newAlertCollector()
	svc := newSvc(t, repo, reg, alertFn)

	enqueueRaw(t, repo, PayloadEnvelope{
		EventType: "TRANSACTION_CREATED",
		EventDetail: PayloadEventDetail{
			TxKey:                "tx-x",
			CoinKey:              "K",
			TxAmount:             "1",
			TransactionStatus:    "COMPLETED",
			TransactionSubStatus: "CONFIRMED",
			TransactionDirection: "INFLOW",
			DestinationAddress:   "0xstranger",
		},
	})
	if _, err := svc.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(repo.manualUpdates) != 1 {
		t.Fatalf("expected 1 manual_review update, got %v", repo.manualUpdates)
	}
	for _, reason := range repo.manualUpdates {
		if reason != ReasonAddressUnassigned {
			t.Errorf("expected ADDRESS_UNASSIGNED, got %s", reason)
		}
	}
	if len(*alerts) != 1 || (*alerts)[0].fields["reason"] != ReasonAddressUnassigned {
		t.Errorf("expected alert with reason ADDRESS_UNASSIGNED, got %+v", *alerts)
	}
	if len(repo.journalCalls) != 0 {
		t.Errorf("expected no journal entry on manual review")
	}
}

func TestProcessOne_CoinUnsupported_FlagsManualReview(t *testing.T) {
	repo := newMockRepo()
	repo.owners["0xdest"] = 42
	reg := &stubRegistry{byKey: map[string]*walletconfig.CoinChain{}}
	alertFn, alerts := newAlertCollector()
	svc := newSvc(t, repo, reg, alertFn)

	enqueueRaw(t, repo, PayloadEnvelope{
		EventType: "TRANSACTION_CREATED",
		EventDetail: PayloadEventDetail{
			TxKey:                "tx-1",
			CoinKey:              "UNKNOWN",
			TxAmount:             "1",
			TransactionStatus:    "COMPLETED",
			TransactionSubStatus: "CONFIRMED",
			TransactionDirection: "INFLOW",
			DestinationAddress:   "0xdest",
		},
	})
	if _, err := svc.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if (*alerts)[0].fields["reason"] != ReasonCoinUnsupported {
		t.Errorf("expected COIN_UNSUPPORTED alert, got %+v", *alerts)
	}
}

func TestProcessOne_BelowMinAmount_FlagsManualReview(t *testing.T) {
	repo := newMockRepo()
	repo.owners["0xdest"] = 42
	reg := newTestRegistry("ETH", "ETHEREUM", "K", "1", 11)
	alertFn, alerts := newAlertCollector()
	svc := newSvc(t, repo, reg, alertFn)

	enqueueRaw(t, repo, PayloadEnvelope{
		EventType: "TRANSACTION_CREATED",
		EventDetail: PayloadEventDetail{
			TxKey:                "tx-1",
			CoinKey:              "K",
			TxAmount:             "0.0001",
			TransactionStatus:    "COMPLETED",
			TransactionSubStatus: "CONFIRMED",
			TransactionDirection: "INFLOW",
			DestinationAddress:   "0xdest",
		},
	})
	if _, err := svc.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if (*alerts)[0].fields["reason"] != ReasonBelowMinAmount {
		t.Errorf("expected BELOW_MIN_AMOUNT alert, got %+v", *alerts)
	}
}

// TestProcessOne_InvalidMinAmountConfig_FlagsManualReview verifies that a
// malformed coin_chains.min_deposit_amount (e.g. operator typo) routes the
// event to MANUAL_REVIEW instead of silently defaulting to 0 (which would
// credit accounts despite a broken config). Regression: T7-S-4.
func TestProcessOne_InvalidMinAmountConfig_FlagsManualReview(t *testing.T) {
	repo := newMockRepo()
	repo.owners["0xdest"] = 42
	// MinDepositAmount is non-numeric — decimal.NewFromString returns an error.
	reg := newTestRegistry("ETH", "ETHEREUM", "K", "not-a-number", 11)
	alertFn, alerts := newAlertCollector()
	svc := newSvc(t, repo, reg, alertFn)

	enqueueRaw(t, repo, PayloadEnvelope{
		EventType: "TRANSACTION_CREATED",
		EventDetail: PayloadEventDetail{
			TxKey:                "tx-1",
			CoinKey:              "K",
			TxAmount:             "0.5",
			TransactionStatus:    "COMPLETED",
			TransactionSubStatus: "CONFIRMED",
			TransactionDirection: "INFLOW",
			DestinationAddress:   "0xdest",
		},
	})
	if _, err := svc.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*alerts) != 1 {
		t.Fatalf("expected 1 alert, got %d", len(*alerts))
	}
	if (*alerts)[0].fields["reason"] != ReasonInvalidCoinConfig {
		t.Errorf("expected INVALID_COIN_CONFIG alert, got %+v", *alerts)
	}
	if len(repo.journalCalls) != 0 {
		t.Errorf("invalid min_amount must NOT credit account; got %d journal entries", len(repo.journalCalls))
	}
	if len(repo.manualUpdates) != 1 {
		t.Errorf("expected deposit marked MANUAL_REVIEW, got %v", repo.manualUpdates)
	}
}

// ------------------- failed-terminal branch -------------------

func TestProcessOne_FailedStatusTransitionsAndAlerts(t *testing.T) {
	repo := newMockRepo()
	repo.owners["0xdest"] = 42
	reg := newTestRegistry("ETH", "ETHEREUM", "K", "0.0001", 11)
	alertFn, alerts := newAlertCollector()
	svc := newSvc(t, repo, reg, alertFn)

	enqueueRaw(t, repo, PayloadEnvelope{
		EventType: "TRANSACTION_STATUS_CHANGED",
		EventDetail: PayloadEventDetail{
			TxKey:                "tx-1",
			CoinKey:              "K",
			TxAmount:             "0.5",
			TransactionStatus:    "FAILED",
			TransactionSubStatus: "INSUFFICIENT_FEE",
			TransactionDirection: "INFLOW",
			DestinationAddress:   "0xdest",
		},
	})
	if _, err := svc.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(repo.failedUpdates) != 1 {
		t.Fatalf("expected deposit marked FAILED, got %v", repo.failedUpdates)
	}
	if len(repo.journalCalls) != 0 {
		t.Errorf("FAILED branch must not write journal")
	}
	if len(*alerts) != 1 || (*alerts)[0].title != "Deposit failed" {
		t.Errorf("expected failed alert, got %+v", *alerts)
	}
}

// ------------------- early-exit filters -------------------

func TestProcessOne_NonAllowedEventTypeSkipped(t *testing.T) {
	repo := newMockRepo()
	svc := newSvc(t, repo, &stubRegistry{}, nil)
	enqueueRaw(t, repo, PayloadEnvelope{
		EventType:   "ACCOUNT_CREATED",
		EventDetail: PayloadEventDetail{TxKey: "tx-x", TransactionStatus: "X"},
	})
	if _, err := svc.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(repo.deposits) != 0 || len(repo.journalCalls) != 0 {
		t.Errorf("non-allowed event must not write any rows")
	}
	if len(repo.doneIDs) != 1 {
		t.Errorf("event still must be marked DONE")
	}
}

func TestProcessOne_OutflowDirectionSkipped(t *testing.T) {
	repo := newMockRepo()
	svc := newSvc(t, repo, &stubRegistry{}, nil)
	enqueueRaw(t, repo, PayloadEnvelope{
		EventType: "TRANSACTION_STATUS_CHANGED",
		EventDetail: PayloadEventDetail{
			TxKey:                "tx-out",
			TransactionDirection: "OUTFLOW",
			TransactionStatus:    "COMPLETED",
			TransactionSubStatus: "CONFIRMED",
		},
	})
	if _, err := svc.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(repo.deposits) != 0 {
		t.Errorf("OUTFLOW must skip")
	}
	if len(repo.doneIDs) != 1 {
		t.Errorf("event must be marked DONE even when skipped")
	}
}

// ------------------- queue empty -------------------

func TestProcessOne_EmptyQueueReturnsFalse(t *testing.T) {
	repo := newMockRepo()
	svc := newSvc(t, repo, &stubRegistry{}, nil)
	processed, err := svc.ProcessOne(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if processed {
		t.Errorf("expected processed=false on empty queue")
	}
}

// ------------------- error path -------------------

func TestProcessOne_DepositErrorMarksEventErrorAndReturnsErr(t *testing.T) {
	repo := newMockRepo()
	repo.owners["0xdest"] = 1
	repo.depositErr = errors.New("db boom")
	reg := newTestRegistry("ETH", "ETHEREUM", "K", "0.0001", 11)
	svc := newSvc(t, repo, reg, nil)
	enqueueRaw(t, repo, PayloadEnvelope{
		EventType: "TRANSACTION_STATUS_CHANGED",
		EventDetail: PayloadEventDetail{
			TxKey:                "tx-err",
			CoinKey:              "K",
			TxAmount:             "1",
			TransactionStatus:    "COMPLETED",
			TransactionSubStatus: "CONFIRMED",
			TransactionDirection: "INFLOW",
			DestinationAddress:   "0xdest",
		},
	})
	processed, err := svc.ProcessOne(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !processed {
		t.Error("expected processed=true even on error")
	}
	if len(repo.errorIDs) != 1 {
		t.Errorf("expected event marked ERROR, got %+v", repo.errorIDs)
	}
}

// TestProcessOne_MarkEventErrorFailWrapsSentinel verifies that when MarkEventError
// itself fails (the most pathological branch: DB outage during error-handling),
// ProcessOne returns an error wrapping ErrMarkErrorFailed so the worker can back
// off to its ticker interval instead of hot-looping on the same PENDING row.
// Regression: T7-I-5.
func TestProcessOne_MarkEventErrorFailWrapsSentinel(t *testing.T) {
	repo := newMockRepo()
	repo.pending = []*Event{{
		ID:         1,
		EventType:  "TRANSACTION_STATUS_CHANGED",
		RawPayload: []byte(`not-json`), // forces unmarshal failure inside processEvent
	}}
	repo.markErrorErr = errors.New("db connection dropped")

	svc := NewService(repo, nil, nil)
	processed, err := svc.ProcessOne(context.Background())

	if !processed {
		t.Errorf("expected processed=true so worker accounts the attempt")
	}
	if err == nil {
		t.Fatalf("expected error when MarkEventError fails")
	}
	if !errors.Is(err, ErrMarkErrorFailed) {
		t.Errorf("err must wrap ErrMarkErrorFailed for worker back-off detection, got: %v", err)
	}
}

// ------------------- creditDepositFromRow error paths -------------------

func TestProcessOne_CreditAccountError_ReturnsErr(t *testing.T) {
	repo := newMockRepo()
	repo.owners["0xdest"] = 42
	repo.creditErr = errors.New("credit boom")
	reg := newTestRegistry("ETH", "ETHEREUM", "K", "0.0001", 11)
	svc := newSvc(t, repo, reg, nil)

	enqueueRaw(t, repo, PayloadEnvelope{
		EventType: "TRANSACTION_STATUS_CHANGED",
		EventDetail: PayloadEventDetail{
			TxKey:                "tx-credit-err",
			CoinKey:              "K",
			TxAmount:             "0.5",
			TransactionStatus:    "COMPLETED",
			TransactionSubStatus: "CONFIRMED",
			TransactionDirection: "INFLOW",
			DestinationAddress:   "0xdest",
		},
	})
	_, err := svc.ProcessOne(context.Background())
	if err == nil {
		t.Fatal("expected error from credit failure")
	}
	if len(repo.creditedIDs) != 0 {
		t.Errorf("should not mark credited on credit error")
	}
}

func TestProcessOne_JournalError_ReturnsErr(t *testing.T) {
	repo := newMockRepo()
	repo.owners["0xdest"] = 42
	repo.journalErr = errors.New("journal boom")
	reg := newTestRegistry("ETH", "ETHEREUM", "K", "0.0001", 11)
	svc := newSvc(t, repo, reg, nil)

	enqueueRaw(t, repo, PayloadEnvelope{
		EventType: "TRANSACTION_STATUS_CHANGED",
		EventDetail: PayloadEventDetail{
			TxKey:                "tx-journal-err",
			CoinKey:              "K",
			TxAmount:             "0.5",
			TransactionStatus:    "COMPLETED",
			TransactionSubStatus: "CONFIRMED",
			TransactionDirection: "INFLOW",
			DestinationAddress:   "0xdest",
		},
	})
	_, err := svc.ProcessOne(context.Background())
	if err == nil {
		t.Fatal("expected error from journal failure")
	}
}

func TestProcessOne_AccountLockError_ReturnsErr(t *testing.T) {
	repo := newMockRepo()
	repo.owners["0xdest"] = 42
	repo.accountErr = errors.New("account lock boom")
	reg := newTestRegistry("ETH", "ETHEREUM", "K", "0.0001", 11)
	svc := newSvc(t, repo, reg, nil)

	enqueueRaw(t, repo, PayloadEnvelope{
		EventType: "TRANSACTION_STATUS_CHANGED",
		EventDetail: PayloadEventDetail{
			TxKey:                "tx-acct-err",
			CoinKey:              "K",
			TxAmount:             "0.5",
			TransactionStatus:    "COMPLETED",
			TransactionSubStatus: "CONFIRMED",
			TransactionDirection: "INFLOW",
			DestinationAddress:   "0xdest",
		},
	})
	_, err := svc.ProcessOne(context.Background())
	if err == nil {
		t.Fatal("expected error from account lock failure")
	}
}

// ------------------- flagAndFinalize when flagManualReview fails -------------------

func TestProcessOne_FlagManualReviewUpsertFails_MarksEventError(t *testing.T) {
	repo := newMockRepo()
	// No address owner → ADDRESS_UNASSIGNED path triggers flagManualReview
	// But we make UpsertDeposit fail inside flagManualReview
	repo.depositErr = errors.New("upsert boom in MR")
	reg := newTestRegistry("ETH", "ETHEREUM", "K", "0.0001", 11)
	svc := newSvc(t, repo, reg, nil)

	enqueueRaw(t, repo, PayloadEnvelope{
		EventType: "TRANSACTION_CREATED",
		EventDetail: PayloadEventDetail{
			TxKey:                "tx-mr-fail",
			CoinKey:              "K",
			TxAmount:             "1",
			TransactionStatus:    "COMPLETED",
			TransactionSubStatus: "CONFIRMED",
			TransactionDirection: "INFLOW",
			DestinationAddress:   "0xstranger",
		},
	})
	_, err := svc.ProcessOne(context.Background())
	if err == nil {
		t.Fatal("expected error when flagManualReview fails")
	}
	// MarkEventError should have been called
	if len(repo.errorIDs) != 1 {
		t.Errorf("expected 1 error event (from flagAndFinalize error path), got %d", len(repo.errorIDs))
	}
}

// ------------------- LookupAddressOwner error -------------------

func TestProcessOne_LookupOwnerError_ReturnsErr(t *testing.T) {
	repo := newMockRepo()
	repo.ownerErr = errors.New("owner lookup boom")
	reg := newTestRegistry("ETH", "ETHEREUM", "K", "0.0001", 11)
	svc := newSvc(t, repo, reg, nil)

	enqueueRaw(t, repo, PayloadEnvelope{
		EventType: "TRANSACTION_STATUS_CHANGED",
		EventDetail: PayloadEventDetail{
			TxKey:                "tx-owner-err",
			CoinKey:              "K",
			TxAmount:             "0.5",
			TransactionStatus:    "COMPLETED",
			TransactionSubStatus: "CONFIRMED",
			TransactionDirection: "INFLOW",
			DestinationAddress:   "0xdest",
		},
	})
	_, err := svc.ProcessOne(context.Background())
	if err == nil {
		t.Fatal("expected error from LookupAddressOwner failure")
	}
}

// ------------------- SetKYTDeps defaults -------------------

func TestSetKYTDeps_DefaultValues(t *testing.T) {
	svc := NewService(newMockRepo(), nil, nil)
	svc.SetKYTDeps(nil, true, 0, 0)

	if svc.kytOrphanMaxRetry != 100 {
		t.Errorf("expected default orphanMaxRetry=100, got %d", svc.kytOrphanMaxRetry)
	}
	if svc.kytTimeout != 20*time.Minute {
		t.Errorf("expected default timeout=20m, got %s", svc.kytTimeout)
	}
}

func TestSetKYTDeps_CustomValues(t *testing.T) {
	svc := NewService(newMockRepo(), nil, nil)
	svc.SetKYTDeps(nil, true, 50, 10*time.Minute)

	if svc.kytOrphanMaxRetry != 50 {
		t.Errorf("expected orphanMaxRetry=50, got %d", svc.kytOrphanMaxRetry)
	}
	if svc.kytTimeout != 10*time.Minute {
		t.Errorf("expected timeout=10m, got %s", svc.kytTimeout)
	}
}

// ------------------- BeginTx error -------------------

func TestProcessOne_BeginTxError_ReturnsErr(t *testing.T) {
	repo := newMockRepo()
	repo.beginTxErr = errors.New("db down")
	svc := NewService(repo, nil, nil)

	_, err := svc.ProcessOne(context.Background())
	if err == nil {
		t.Fatal("expected error from BeginTx failure")
	}
}

// TestStatusRank verifies the monotonic ordering of Safeheron transactionStatus
// values. Locked by plan §3.1 / SPEC §4.6:
//
//	SUBMITTED=10, SIGNING=20, BROADCASTING=30, CONFIRMING=50,
//	FAILED/CANCELLED/REJECTED=90, COMPLETED=100, unknown→0.
//
// "CREATED" is NOT a Safeheron transactionStatus and must fall through to 0
// so it never poisons a partially-credited row.
func TestStatusRank(t *testing.T) {
	cases := []struct {
		in  string
		out int
	}{
		{"SUBMITTED", 10},
		{"SIGNING", 20},
		{"BROADCASTING", 30},
		{"CONFIRMING", 50},
		{"FAILED", 90}, {"CANCELLED", 90}, {"REJECTED", 90},
		{"COMPLETED", 100},
		{"CREATED", 0}, // not part of plan §3.1, treated as unknown
		{"UNKNOWN", 0},
		{"", 0},
	}
	for _, c := range cases {
		if got := StatusRank(c.in); got != c.out {
			t.Errorf("StatusRank(%q) = %d, want %d", c.in, got, c.out)
		}
	}
}

// === T11.1 D-41: Status overwrite protection ===

func TestMarkDepositFailed_CreditedDeposit_ReturnsError(t *testing.T) {
	repo := newMockRepo()
	repo.deposits["tx-credited"] = &DepositRow{ID: 1, SafeheronTxKey: "tx-credited", Status: DepositStatusCredited}

	err := repo.MarkDepositFailed(context.Background(), &fakeTx{mu: &repo.mu}, 1, "TIMEOUT")
	if !errors.Is(err, ErrDepositTerminalState) {
		t.Fatalf("expected ErrDepositTerminalState, got %v", err)
	}
	if repo.deposits["tx-credited"].Status != DepositStatusCredited {
		t.Fatalf("expected status to remain CREDITED, got %s", repo.deposits["tx-credited"].Status)
	}
}

func TestMarkDepositFailed_ManualReviewDeposit_ReturnsError(t *testing.T) {
	repo := newMockRepo()
	repo.deposits["tx-mr"] = &DepositRow{ID: 1, SafeheronTxKey: "tx-mr", Status: DepositStatusManualReview}

	err := repo.MarkDepositFailed(context.Background(), &fakeTx{mu: &repo.mu}, 1, "CANCELLED")
	if !errors.Is(err, ErrDepositTerminalState) {
		t.Fatalf("expected ErrDepositTerminalState, got %v", err)
	}
	if repo.deposits["tx-mr"].Status != DepositStatusManualReview {
		t.Fatalf("expected status to remain MANUAL_REVIEW, got %s", repo.deposits["tx-mr"].Status)
	}
}

func TestMarkDepositManualReview_CreditedDeposit_ReturnsError(t *testing.T) {
	repo := newMockRepo()
	repo.deposits["tx-credited"] = &DepositRow{ID: 1, SafeheronTxKey: "tx-credited", Status: DepositStatusCredited}

	err := repo.MarkDepositManualReview(context.Background(), &fakeTx{mu: &repo.mu}, 1, "KYT_RISK")
	if !errors.Is(err, ErrDepositTerminalState) {
		t.Fatalf("expected ErrDepositTerminalState, got %v", err)
	}
	if repo.deposits["tx-credited"].Status != DepositStatusCredited {
		t.Fatalf("expected status to remain CREDITED, got %s", repo.deposits["tx-credited"].Status)
	}
}

func TestMarkDepositManualReview_FailedDeposit_ReturnsError(t *testing.T) {
	repo := newMockRepo()
	repo.deposits["tx-failed"] = &DepositRow{ID: 1, SafeheronTxKey: "tx-failed", Status: DepositStatusFailed}

	err := repo.MarkDepositManualReview(context.Background(), &fakeTx{mu: &repo.mu}, 1, "KYT_RISK")
	if !errors.Is(err, ErrDepositTerminalState) {
		t.Fatalf("expected ErrDepositTerminalState, got %v", err)
	}
	if repo.deposits["tx-failed"].Status != DepositStatusFailed {
		t.Fatalf("expected status to remain FAILED, got %s", repo.deposits["tx-failed"].Status)
	}
}

func TestMarkDepositFailed_PendingDeposit_Succeeds(t *testing.T) {
	repo := newMockRepo()
	repo.deposits["tx-pending"] = &DepositRow{ID: 1, SafeheronTxKey: "tx-pending", Status: DepositStatusPending}

	err := repo.MarkDepositFailed(context.Background(), &fakeTx{mu: &repo.mu}, 1, "TIMEOUT")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if repo.deposits["tx-pending"].Status != DepositStatusFailed {
		t.Fatalf("expected status FAILED, got %s", repo.deposits["tx-pending"].Status)
	}
}

// === T11.5 D-45: KYT environment double-check ===

func TestSetKYTDeps_ProductionDisabled_Panics(t *testing.T) {
	t.Setenv("APP_ENV", "production")
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic when KYT disabled in production")
		}
	}()
	svc := NewService(newMockRepo(), newTestRegistry("ETH", "ETHEREUM", "K", "0.0001", 1), nil)
	svc.SetKYTDeps(nil, false, 0, 0)
}

func TestSetKYTDeps_NonProductionDisabled_NoPanic(t *testing.T) {
	t.Setenv("APP_ENV", "local")
	svc := NewService(newMockRepo(), newTestRegistry("ETH", "ETHEREUM", "K", "0.0001", 1), nil)
	svc.SetKYTDeps(nil, false, 0, 0) // should not panic
}

// === T11.7 D-47: MoveToKYTPending ErrDepositNotPending skips KYT ===

func TestProcessOne_MoveToKYTPending_AlreadyAdvanced_SkipsKYT(t *testing.T) {
	repo := newMockRepo()
	repo.owners["0xdest"] = 42
	reg := newTestRegistry("ETH", "ETHEREUM", "K", "0.0001", 11)

	kytCalled := false
	mockClient := &mockKYTClient{reportFn: func(_ context.Context, _ string) (*safeheron.KytReportResponse, error) {
		kytCalled = true
		return nil, errors.New("should not be called")
	}}

	// Wrap repo to force MoveToKYTPending → ErrDepositNotPending
	wrappedRepo := &alwaysNotPendingRepo{mockRepo: repo}
	svc := NewService(wrappedRepo, reg, nil)
	svc.SetKYTDeps(mockClient, true, 3, time.Minute)
	svc.SetSerialFunc(func() string { return "TEST-SERIAL" })

	enqueueRaw(t, repo, PayloadEnvelope{
		EventType: "TRANSACTION_STATUS_CHANGED",
		EventDetail: PayloadEventDetail{
			TxKey:                "tx-already-advanced",
			CoinKey:              "K",
			TxAmount:             "1.0",
			TransactionStatus:    "COMPLETED",
			TransactionSubStatus: "CONFIRMED",
			TransactionDirection: "INFLOW",
			DestinationAddress:   "0xdest",
		},
	})

	_, err := svc.ProcessOne(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if kytCalled {
		t.Error("KYT API should NOT be called when deposit already advanced past PENDING")
	}
}

type alwaysNotPendingRepo struct {
	*mockRepo
}

func (r *alwaysNotPendingRepo) MoveToKYTPending(_ context.Context, _ Tx, _ int64) error {
	return ErrDepositNotPending
}

// === T11.1 D-41: service-level ErrDepositTerminalState handling ===

func TestProcessOne_ManualReview_CreditedDeposit_NoError(t *testing.T) {
	repo := newMockRepo()
	repo.owners["0xdest"] = 42
	reg := newTestRegistry("ETH", "ETHEREUM", "K", "0.0001", 11)

	mockClient := &mockKYTClient{reportFn: func(_ context.Context, _ string) (*safeheron.KytReportResponse, error) {
		return &safeheron.KytReportResponse{
			TxKey:                      "tx-credited-then-mr",
			AmlScreeningTriggeredState: "TRIGGERED",
			AmlList: []safeheron.AmlReport{{
				Provider:  "MistTrack",
				Status:    "COMPLETED",
				RiskLevel: "HIGH",
			}},
		}, nil
	}}

	svc := NewService(repo, reg, nil)
	svc.SetKYTDeps(mockClient, true, 3, time.Minute)
	svc.SetSerialFunc(func() string { return "TEST-SERIAL" })

	enqueueRaw(t, repo, PayloadEnvelope{
		EventType: "TRANSACTION_STATUS_CHANGED",
		EventDetail: PayloadEventDetail{
			TxKey:                "tx-credited-then-mr",
			CoinKey:              "K",
			TxAmount:             "1.0",
			TransactionStatus:    "COMPLETED",
			TransactionSubStatus: "CONFIRMED",
			TransactionDirection: "INFLOW",
			DestinationAddress:   "0xdest",
		},
	})

	// Simulate race: after UpsertDeposit creates the deposit as PENDING and
	// MoveToKYTPending moves it to KYT_PENDING, then creditDepositFromRow
	// credits it before T-γ's MarkDepositManualReview runs.
	// We achieve this by wrapping MarkDepositManualReview to pre-credit.
	wrappedRepo := &creditBeforeMRRepo{mockRepo: repo}
	svc2 := NewService(wrappedRepo, reg, nil)
	svc2.SetKYTDeps(mockClient, true, 3, time.Minute)
	svc2.SetSerialFunc(func() string { return "TEST-SERIAL" })

	enqueueRaw(t, repo, PayloadEnvelope{
		EventType: "TRANSACTION_STATUS_CHANGED",
		EventDetail: PayloadEventDetail{
			TxKey:                "tx-credited-then-mr-2",
			CoinKey:              "K",
			TxAmount:             "1.0",
			TransactionStatus:    "COMPLETED",
			TransactionSubStatus: "CONFIRMED",
			TransactionDirection: "INFLOW",
			DestinationAddress:   "0xdest",
		},
	})

	_, err := svc2.ProcessOne(context.Background())
	if err != nil {
		t.Fatalf("expected no error when MarkDepositManualReview hits CREDITED deposit, got: %v", err)
	}
}

type creditBeforeMRRepo struct {
	*mockRepo
}

func (r *creditBeforeMRRepo) MarkDepositManualReview(_ context.Context, _ Tx, id int64, reason string) error {
	r.mockRepo.mu.Lock()
	for _, d := range r.mockRepo.deposits {
		if d.ID == id {
			d.Status = DepositStatusCredited
		}
	}
	r.mockRepo.mu.Unlock()
	return r.mockRepo.MarkDepositManualReview(context.Background(), &fakeTx{mu: &r.mockRepo.mu}, id, reason)
}

// TestProcessOne_BelowMinAmount_TwoEvents_SingleAlert verifies that when two
// events arrive for the same below-min deposit (e.g. CONFIRMING then
// COMPLETED), only one MANUAL_REVIEW alert is fired — not two.
// Regression guard for the duplicate-alert bug fixed in flagManualReview.
func TestProcessOne_BelowMinAmount_TwoEvents_SingleAlert(t *testing.T) {
	repo := newMockRepo()
	repo.owners["0xdest"] = 42
	// min_deposit_amount = 1, incoming amount = 0.001 → always below min
	reg := newTestRegistry("USDT", "BSC", "USDT_BEP20", "1", 18)
	alertFn, alerts := newAlertCollector()
	svc := newSvc(t, repo, reg, alertFn)

	detail := PayloadEventDetail{
		TxKey:                "tx-dust",
		CoinKey:              "USDT_BEP20",
		TxAmount:             "0.001",
		TransactionDirection: "INFLOW",
		DestinationAddress:   "0xdest",
	}

	// Event 1: CONFIRMING
	detail.TransactionStatus = "CONFIRMING"
	detail.TransactionSubStatus = ""
	enqueueRaw(t, repo, PayloadEnvelope{EventType: "TRANSACTION_CREATED", EventDetail: detail})
	if _, err := svc.ProcessOne(context.Background()); err != nil {
		t.Fatalf("event 1: %v", err)
	}

	// Event 2: COMPLETED (same tx, deposit already MANUAL_REVIEW)
	detail.TransactionStatus = "COMPLETED"
	detail.TransactionSubStatus = "CONFIRMED"
	enqueueRaw(t, repo, PayloadEnvelope{EventType: "TRANSACTION_STATUS_CHANGED", EventDetail: detail})
	if _, err := svc.ProcessOne(context.Background()); err != nil {
		t.Fatalf("event 2: %v", err)
	}

	if len(*alerts) != 1 {
		t.Errorf("expected exactly 1 alert, got %d: %+v", len(*alerts), *alerts)
	}
	if len(*alerts) > 0 && (*alerts)[0].fields["reason"] != ReasonBelowMinAmount {
		t.Errorf("expected BELOW_MIN_AMOUNT, got %q", (*alerts)[0].fields["reason"])
	}
}

// TestProcessOne_BelowMinAmount_FindDepositError verifies that a DB error from
// FindDepositByTxKey inside flagManualReview is surfaced to the caller.
func TestProcessOne_BelowMinAmount_FindDepositError(t *testing.T) {
	repo := newMockRepo()
	repo.owners["0xdest"] = 42
	repo.findDepositByTxErr = errors.New("db unavailable")
	reg := newTestRegistry("USDT", "BSC", "USDT_BEP20", "1", 18)
	svc := newSvc(t, repo, reg, nil)

	enqueueRaw(t, repo, PayloadEnvelope{
		EventType: "TRANSACTION_CREATED",
		EventDetail: PayloadEventDetail{
			TxKey:                "tx-dust-err",
			CoinKey:              "USDT_BEP20",
			TxAmount:             "0.001",
			TransactionStatus:    "CONFIRMING",
			TransactionDirection: "INFLOW",
			DestinationAddress:   "0xdest",
		},
	})

	_, err := svc.ProcessOne(context.Background())
	if err == nil {
		t.Fatal("expected error from FindDepositByTxKey, got nil")
	}
	if !strings.Contains(err.Error(), "check existing deposit") {
		t.Errorf("expected error to contain 'check existing deposit', got: %v", err)
	}
}

// === 本次变更补充测试 ===

// TestProcessOne_AddressUnassigned_AssetFieldPopulated 验证地址未分配时 deposits.asset
// 字段仍正确写入 coin symbol（修复前该字段为空字符串）。
func TestProcessOne_AddressUnassigned_AssetFieldPopulated(t *testing.T) {
	repo := newMockRepo()
	// 不设置 owner → LookupAddressOwner 返回 not found
	reg := newTestRegistry("ETH", "ETHEREUM", "ETH", "0.0001", 7)
	svc := newSvc(t, repo, reg, nil)

	enqueueRaw(t, repo, PayloadEnvelope{
		EventType: "TRANSACTION_CREATED",
		EventDetail: PayloadEventDetail{
			TxKey:                "tx-unassigned",
			CoinKey:              "ETH",
			TxAmount:             "0.05",
			TransactionStatus:    "COMPLETED",
			TransactionSubStatus: "CONFIRMED",
			TransactionDirection: "INFLOW",
			DestinationAddress:   "0xunknown",
		},
	})
	if _, err := svc.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}

	dep, ok := repo.deposits["tx-unassigned"]
	if !ok {
		t.Fatal("expected deposit row to be written")
	}
	if dep.Asset != "ETH" {
		t.Errorf("expected Asset=ETH, got %q (symbol must be populated even when address unassigned)", dep.Asset)
	}
	if dep.Status != DepositStatusManualReview {
		t.Errorf("expected status MANUAL_REVIEW, got %s", dep.Status)
	}
}

// TestProcessOne_AddressUnassigned_TwoEvents_SingleAlert 验证同一未分配地址充值收到两
// 条事件时，第二条不重复告警（alreadyFlagged 路径）。
func TestProcessOne_AddressUnassigned_TwoEvents_SingleAlert(t *testing.T) {
	repo := newMockRepo()
	reg := newTestRegistry("ETH", "ETHEREUM", "ETH", "0.0001", 7)
	alertFn, alerts := newAlertCollector()
	svc := newSvc(t, repo, reg, alertFn)

	detail := PayloadEventDetail{
		TxKey:                "tx-dup-unassigned",
		CoinKey:              "ETH",
		TxAmount:             "0.05",
		TransactionDirection: "INFLOW",
		DestinationAddress:   "0xunknown",
	}

	// 第一条：CONFIRMING
	detail.TransactionStatus = "CONFIRMING"
	enqueueRaw(t, repo, PayloadEnvelope{EventType: "TRANSACTION_CREATED", EventDetail: detail})
	if _, err := svc.ProcessOne(context.Background()); err != nil {
		t.Fatalf("event 1: %v", err)
	}

	// 第二条：COMPLETED（同笔，deposit 已是 MANUAL_REVIEW）
	detail.TransactionStatus = "COMPLETED"
	detail.TransactionSubStatus = "CONFIRMED"
	enqueueRaw(t, repo, PayloadEnvelope{EventType: "TRANSACTION_STATUS_CHANGED", EventDetail: detail})
	if _, err := svc.ProcessOne(context.Background()); err != nil {
		t.Fatalf("event 2: %v", err)
	}

	if len(*alerts) != 1 {
		t.Errorf("expected exactly 1 alert for 2 events on same unassigned deposit, got %d", len(*alerts))
	}
	// 两条事件操作同一笔 deposit（同 ID），manualUpdates map 只有一个键
	if len(repo.manualUpdates) != 1 {
		t.Errorf("expected 1 deposit in manualUpdates (same deposit, two events), got %d", len(repo.manualUpdates))
	}
}

// TestProcessOne_MarkMRNonTerminalError_Propagated 验证 MarkDepositManualReview 返回
// 非 ErrDepositTerminalState 错误时，flagManualReview 将其包装后上浮（不静默吞掉）。
func TestProcessOne_MarkMRNonTerminalError_Propagated(t *testing.T) {
	repo := newMockRepo()
	repo.markMRErr = errors.New("constraint violation")
	reg := newTestRegistry("ETH", "ETHEREUM", "ETH", "0.0001", 7)
	svc := newSvc(t, repo, reg, nil)

	enqueueRaw(t, repo, PayloadEnvelope{
		EventType: "TRANSACTION_CREATED",
		EventDetail: PayloadEventDetail{
			TxKey:                "tx-mr-err",
			CoinKey:              "ETH",
			TxAmount:             "0.05",
			TransactionDirection: "INFLOW",
			TransactionStatus:    "COMPLETED",
			DestinationAddress:   "0xunknown",
		},
	})
	_, err := svc.ProcessOne(context.Background())
	if err == nil {
		t.Fatal("expected error when MarkDepositManualReview fails with non-terminal error")
	}
	if !strings.Contains(err.Error(), "mark manual_review") {
		t.Errorf("expected 'mark manual_review' in error, got: %v", err)
	}
	// flagAndFinalize calls MarkEventError when procErr != nil
	if len(repo.errorIDs) != 1 {
		t.Errorf("expected MarkEventError called once, got %d", len(repo.errorIDs))
	}
}

// TestProcessOne_AddressUnassigned_AlertUserIDIsNA 验证地址未分配时告警字段
// userId 为 "N/A" 而非 "0"，避免运维误读为系统账户（S-3）。
func TestProcessOne_AddressUnassigned_AlertUserIDIsNA(t *testing.T) {
	repo := newMockRepo()
	reg := newTestRegistry("ETH", "ETHEREUM", "ETH", "0.0001", 7)
	alertFn, alerts := newAlertCollector()
	svc := newSvc(t, repo, reg, alertFn)

	enqueueRaw(t, repo, PayloadEnvelope{
		EventType: "TRANSACTION_CREATED",
		EventDetail: PayloadEventDetail{
			TxKey:                "tx-na-userid",
			CoinKey:              "ETH",
			TxAmount:             "0.05",
			TransactionDirection: "INFLOW",
			TransactionStatus:    "COMPLETED",
			DestinationAddress:   "0xunknown",
		},
	})
	if _, err := svc.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*alerts) != 1 {
		t.Fatalf("expected 1 alert, got %d", len(*alerts))
	}
	got := (*alerts)[0].fields["userId"]
	if got != "N/A" {
		t.Errorf("expected userId=N/A for unassigned address, got %q", got)
	}
}

// TestWarnIfTerminalState_TerminalError_Absorbed 验证传入 ErrDepositTerminalState 时
// 函数返回 nil（告警日志但不冒泡）。
func TestWarnIfTerminalState_TerminalError_Absorbed(t *testing.T) {
	err := warnIfTerminalState(ErrDepositTerminalState, 42, "MANUAL_REVIEW")
	if err != nil {
		t.Errorf("expected nil when ErrDepositTerminalState is passed, got %v", err)
	}
}

// TestWarnIfTerminalState_OtherError_Propagated 验证非终态错误原样返回。
func TestWarnIfTerminalState_OtherError_Propagated(t *testing.T) {
	sentinel := errors.New("db connection lost")
	err := warnIfTerminalState(sentinel, 42, "MANUAL_REVIEW")
	if !errors.Is(err, sentinel) {
		t.Errorf("expected sentinel error to propagate, got %v", err)
	}
}

// TestScanAmlPending_BeginTxError_Logged 验证 scanOneAmlPending 内部 BeginTx 失败时
// ScanAmlPending 记录错误日志（不 panic）。
func TestScanAmlPending_BeginTxError_Logged(t *testing.T) {
	repo := newMockRepo()
	repo.beginTxErr = errors.New("db unavailable")
	svc := newSvc(t, repo, nil, nil)
	svc.ScanAmlPending(context.Background()) // must not panic
}

// TestSetAMLFirstPollDelay_Negative_DefaultsToFiveMinutes 验证传入负值时使用默认 5m。
func TestSetAMLFirstPollDelay_Negative_DefaultsToFiveMinutes(t *testing.T) {
	repo := newMockRepo()
	svc := NewService(repo, nil, nil)
	svc.SetAMLFirstPollDelay(-1)
	if svc.amlFirstPollDelay != 5*time.Minute {
		t.Errorf("expected 5m, got %v", svc.amlFirstPollDelay)
	}
}
