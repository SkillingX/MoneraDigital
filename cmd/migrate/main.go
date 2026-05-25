//go:build ignore
// +build ignore

package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"

	"monera-digital/internal/migration"
	"monera-digital/internal/migration/migrations"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/joho/godotenv"
)

func main() {
	if os.Getenv("APP_ENV") != "production" {
		_ = godotenv.Overload(".env")
	}

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL environment variable is required")
	}

	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		log.Fatal("Failed to connect:", err)
	}
	defer db.Close()

	migrator := migration.NewMigrator(db)
	migrator.Register(&migrations.UpdateWalletRequestsTable{})
	migrator.Register(&migrations.AddIsPrimaryToWhitelist{})
	migrator.Register(&migrations.CreateDepositsTable{})
	migrator.Register(&migrations.AddFrozenUntilToWhitelist{})

	// Safeheron Phase 1
	migrator.Register(&migrations.SafeheronPhase1{})

	// Hotfix: frozen_balance NOT NULL without DEFAULT caused first-deposit failures
	migrator.Register(&migrations.AccountFrozenBalanceDefault{})

	// Approval Callback Service: approval_records + sweep_transactions
	migrator.Register(&migrations.CreateApprovalAndSweepTables{})

	// v1.1 Phase 1 AML hard block: approval_records.aml_risk_level
	migrator.Register(&migrations.AddAmlRiskLevelToApprovalRecords{})

	if err := migrator.Migrate(); err != nil {
		log.Fatal("Migration failed:", err)
	}

	status, err := migrator.GetStatus()
	if err != nil {
		log.Fatal("Failed to get status:", err)
	}

	fmt.Println("\nMigration Status:")
	for _, s := range status {
		fmt.Printf("  %s: %s - %s\n", s.Version, s.Status, s.Name)
	}

	fmt.Println("\nDone!")
}
