package invoices

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mujhtech/trest"
)

type invoiceID struct {
	ID string `path:"id" json:"id" minLength:"1"`
}
type createRequest struct {
	Body CreateInput `body:"json" json:"body"`
}
type updateRequest struct {
	ID   string      `path:"id" json:"id"`
	Body UpdateInput `body:"json" json:"body"`
}
type listRequest struct {
	Currency *string `query:"currency" json:"currency,omitempty" enum:"NGN,USD"`
	Status   *string `query:"status" json:"status,omitempty" enum:"draft,paid"`
	Limit    int     `query:"limit" json:"limit" default:"20" min:"1" max:"100"`
	Offset   int     `query:"offset" json:"offset" default:"0" min:"0"`
}
type uploadRequest struct {
	ID   string                `path:"id" json:"id"`
	File *multipart.FileHeader `form:"file" json:"-" required:"true"`
}

// StreamEvent carries either the initial snapshot or a subsequent tenant change.
// The event name identifies the variant; clients retain a typed payload for both.
type StreamEvent struct {
	Snapshot *Page   `json:"snapshot,omitempty"`
	Change   *Change `json:"change,omitempty"`
}

type eventsRequest struct {
	Watch bool `query:"watch" json:"watch" default:"false"`
}

// App composes a reusable invoice module with docs and a public welcome route.
// credentials maps explicitly supplied bearer tokens to synthetic tenants.
func App(service *Service, credentials map[string]string, options ...trest.Component) (*trest.App, error) {
	if service == nil {
		return nil, errors.New("invoice service is required")
	}
	components := []trest.Component{
		trest.Info("Invoice workshop", "1.0.0"),
		trest.Get("/", trest.Const("Trest invoice workshop · open /docs")).Name("welcome"),
		trest.Docs("/docs"),
		trest.Get("/readyz", trest.Const("ready")).Name("readiness"),
		trest.Group("/api",
			trest.Authenticate(authenticate(credentials)),
			trest.RequireAuth(),
			trest.Limits(1<<20, 1<<20),
			trest.OnError(func(_ context.Context, err error) error { return publicError(err) }),
			Routes(service),
		),
	}
	return trest.New(append(components, options...)...)
}

// Routes is independently reusable under a different prefix or authentication
// composition. Every resource method also enforces tenant ownership in Service.
func Routes(service *Service) *trest.Router {
	tenant := func(ctx context.Context) string { p, _ := trest.Identity(ctx); return p.TenantID }
	owns := func(ctx context.Context, p trest.Principal, in invoiceID) error {
		_, err := service.Get(ctx, p.TenantID, in.ID)
		return publicError(err)
	}
	loadedInvoice := trest.NewKey[Invoice]()
	getInvoice := trest.Resolve(
		trest.Get("/{id}", func(ctx context.Context, _ invoiceID) (Invoice, error) {
			invoice, ok := trest.Value(ctx, loadedInvoice)
			if !ok {
				return Invoice{}, errors.New("resolved invoice unavailable")
			}
			return invoice, nil
		}),
		loadedInvoice,
		func(ctx context.Context, in invoiceID) (Invoice, error) {
			return service.Get(ctx, tenant(ctx), in.ID)
		},
	).Name("getInvoice").Summary("Read an invoice").Authorize(func(ctx context.Context, p trest.Principal, _ invoiceID) error {
		invoice, ok := trest.Value(ctx, loadedInvoice)
		if !ok || invoice.TenantID != p.TenantID {
			return publicError(ErrForbidden)
		}
		return nil
	}).MapError(publicError)
	return trest.Group("/invoices",
		trest.RequireAuth(),
		trest.Get("", func(ctx context.Context, in listRequest) (Page, error) {
			return service.List(ctx, tenant(ctx), Filter{Currency: value(in.Currency), Status: value(in.Status), Limit: in.Limit, Offset: in.Offset})
		}).Name("listInvoices").Summary("List your invoices").MapError(publicError),
		trest.Post("", func(ctx context.Context, in createRequest) (Invoice, error) {
			return service.Create(ctx, tenant(ctx), in.Body)
		}).Name("createInvoice").Summary("Create an invoice").Status(http.StatusCreated).Authorize(func(_ context.Context, p trest.Principal, in createRequest) error {
			if !service.OwnsCustomer(p.TenantID, in.Body.CustomerID) {
				return publicError(ErrForbidden)
			}
			return nil
		}).MapError(publicError),
		getInvoice,
		trest.Put("/{id}", func(ctx context.Context, in updateRequest) (Invoice, error) {
			return service.Update(ctx, tenant(ctx), in.ID, in.Body)
		}).Name("updateInvoice").Summary("Update an invoice").Authorize(func(ctx context.Context, p trest.Principal, in updateRequest) error {
			return owns(ctx, p, invoiceID{ID: in.ID})
		}).MapError(publicError),
		trest.Delete("/{id}", func(ctx context.Context, in invoiceID) (trest.Reply, error) {
			if err := service.Delete(ctx, tenant(ctx), in.ID); err != nil {
				return trest.Reply{}, err
			}
			return trest.NoContent(), nil
		}).Name("deleteInvoice").Summary("Delete an invoice").Status(http.StatusNoContent).Authorize(owns).MapError(publicError),
		trest.Post("/{id}/attachment", func(ctx context.Context, in uploadRequest) (Invoice, error) {
			if in.File == nil {
				return Invoice{}, ErrInvalid
			}
			file, err := in.File.Open()
			if err != nil {
				return Invoice{}, err
			}
			defer func() { _ = file.Close() }()
			digest := sha256.New()
			n, err := io.Copy(digest, io.LimitReader(file, (512<<10)+1))
			if err != nil {
				return Invoice{}, err
			}
			if n > 512<<10 {
				return Invoice{}, ErrInvalid
			}
			return service.Attach(ctx, tenant(ctx), in.ID, Attachment{Name: filepath.Base(in.File.Filename), Bytes: n, SHA256: hex.EncodeToString(digest.Sum(nil))})
		}).Name("uploadAttachment").Summary("Attach a file (up to 512 KiB)").Authorize(func(ctx context.Context, p trest.Principal, in uploadRequest) error {
			return owns(ctx, p, invoiceID{ID: in.ID})
		}).MapError(publicError),
		trest.Get("/events", func(ctx context.Context, in eventsRequest) (trest.Events[StreamEvent], error) {
			identity := tenant(ctx)
			return trest.EventStream(func(ctx context.Context, writer *trest.JSONEventWriter[StreamEvent]) error {
				snapshot, changes, unsubscribe := service.Subscribe(identity)
				defer unsubscribe()
				if err := writer.Send("", "snapshot", StreamEvent{Snapshot: &snapshot}); err != nil {
					return err
				}
				if !in.Watch {
					return nil
				}
				for {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case event, ok := <-changes:
						if !ok {
							return errors.New("subscriber fell behind; request a new snapshot")
						}
						if err := writer.Send(strconv.FormatInt(event.Sequence, 10), event.Kind, StreamEvent{Change: &event}); err != nil {
							return err
						}
					}
				}
			}), nil
		}).Name("invoiceEvents").Summary("Snapshot and optional live tenant updates"),
	)
}
func value(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
func authenticate(credentials map[string]string) func(*http.Request) (trest.Principal, error) {
	copy := make(map[string]string, len(credentials))
	for token, tenant := range credentials {
		if token != "" && tenant != "" {
			copy[token] = tenant
		}
	}
	return func(r *http.Request) (trest.Principal, error) {
		scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if ok && strings.EqualFold(scheme, "Bearer") && token != "" {
			for expected, tenant := range copy {
				if subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1 {
					return trest.Principal{ID: "demo-" + tenant, TenantID: tenant}, nil
				}
			}
		}
		return trest.Principal{}, trest.NewError(trest.Unauthenticated, "A valid bearer token is required.", nil)
	}
}
func publicError(err error) error {
	switch {
	case errors.Is(err, ErrNotFound):
		return trest.NewError(trest.NotFound, "Invoice not found.", err)
	case errors.Is(err, ErrForbidden):
		return trest.NewError(trest.Forbidden, "You cannot access this customer or invoice.", err)
	case errors.Is(err, ErrInvalid):
		return trest.NewError(trest.InvalidInput, "Invoice details are invalid.", err)
	default:
		return err
	}
}
