// job-example demonstrates an explicit application transaction plus a durable outbox.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"trest.local/trest/adapters/postgres"
	"trest.local/trest/job"
	"trest.local/trest/operation"
)

type deliveryInput struct {
	InvoiceID string `json:"invoice_id"`
}
type receipt struct {
	InvoiceID string `json:"invoice_id"`
	Delivered bool   `json:"delivered"`
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	migrate := flag.Bool("migrate", false, "explicitly apply queue and example schema to TREST_POSTGRES_DSN")
	invoiceID := flag.String("invoice", "invoice-demo-1", "synthetic invoice id and idempotency key")
	flag.Parse()
	dsn := os.Getenv("TREST_POSTGRES_DSN")
	if dsn == "" {
		return errors.New("set TREST_POSTGRES_DSN to a disposable example database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := postgres.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	if *migrate {
		if _, err := db.ExecContext(ctx, job.MigrationSQL()); err != nil {
			return err
		}
		_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS trest_example_invoice (id text PRIMARY KEY,tenant_id text NOT NULL);
		CREATE TABLE IF NOT EXISTS trest_example_delivery (invoice_id text PRIMARY KEY,tenant_id text NOT NULL)`)
		if err != nil {
			return err
		}
		fmt.Println("Applied queue and synthetic example tables. Run again without -migrate to publish and process an invoice.")
		return nil
	}
	// This trusted identity is local demonstration data. A deployed application
	// obtains identity from verified credentials and owns tenant/resource policy.
	ctx = operation.WithPrincipal(ctx, operation.Principal{ID: "demo-user", TenantID: "demo-tenant"})
	registry := operation.NewRegistry(operation.AuthorizerFunc(func(_ context.Context, p operation.Principal, permission string, _ any) error {
		if p.ID != "demo-user" || p.TenantID != "demo-tenant" || permission != "invoices:deliver" {
			return operation.NewError(operation.Forbidden, "not permitted", nil)
		}
		return nil
	}))
	var queue *job.Store
	_, err = operation.Register(registry, operation.Definition[deliveryInput, receipt]{ID: "invoices.deliver.v1", Summary: "Record a synthetic invoice delivery", Access: operation.Permission("invoices:deliver"), SideEffect: "write"}, func(ctx context.Context, in deliveryInput) (receipt, error) {
		payload, err := json.Marshal(in)
		if err != nil {
			return receipt{}, err
		}
		// Explicitly protect database effects and their result in one transaction.
		result, _, err := queue.DoEffect(ctx, job.Request{OperationID: "invoices.deliver.v1", PayloadVersion: "1", Input: payload, CallerScope: "delivery", IdempotencyKey: in.InvoiceID}, func(ctx context.Context, tx *sql.Tx) (json.RawMessage, error) {
			p, _ := operation.PrincipalFromContext(ctx)
			if _, err := tx.ExecContext(ctx, `INSERT INTO trest_example_delivery(invoice_id,tenant_id) VALUES($1,$2)`, in.InvoiceID, p.TenantID); err != nil {
				return nil, err
			}
			return json.Marshal(receipt{InvoiceID: in.InvoiceID, Delivered: true})
		})
		if err != nil {
			return receipt{}, err
		}
		var output receipt
		if err := json.Unmarshal(result, &output); err != nil {
			return receipt{}, err
		}
		return output, nil
	})
	if err != nil {
		return err
	}
	if err := registry.Freeze(); err != nil {
		return err
	}
	queue, err = job.New(db, registry, job.Options{})
	if err != nil {
		return err
	}
	payload, err := json.Marshal(deliveryInput{InvoiceID: *invoiceID})
	if err != nil {
		return err
	}
	request := job.Request{OperationID: "invoices.deliver.v1", PayloadVersion: "1", Input: payload, CallerScope: "invoice-created", IdempotencyKey: *invoiceID, RetrySafe: true}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO trest_example_invoice(id,tenant_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, *invoiceID, "demo-tenant"); err != nil {
		return err
	}
	outboxID, err := queue.Publish(ctx, tx, request)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("publisher commit uncertain; retry the same invoice id: %w", err)
	}
	jobID, err := queue.DispatchOne(ctx)
	if err != nil {
		return err
	}
	worker, err := job.NewWorker(queue, time.Second)
	if err != nil {
		return err
	}
	if _, err := worker.RunOne(ctx); err != nil {
		return err
	}
	record, err := queue.Get(ctx, jobID)
	if err != nil {
		return err
	}
	fmt.Printf("outbox=%s job=%s state=%s attempts=%d result=%s\n", outboxID, jobID, record.State, record.Attempts, record.Output)
	return nil
}
