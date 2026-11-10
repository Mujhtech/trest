package invoices_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"trest.local/trest"
	"trest.local/trest/clientgen"
	"trest.local/trest/examples/invoices"
)

func setup(t *testing.T) (*trest.App, *invoices.Service) {
	t.Helper()
	service := invoices.NewService()
	app, err := invoices.App(service, map[string]string{"acme-test": "acme", "globex-test": "globex"})
	if err != nil {
		t.Fatal(err)
	}
	return app, service
}
func request(t *testing.T, app http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	out := httptest.NewRecorder()
	app.ServeHTTP(out, req)
	return out
}
func decode[T any](t *testing.T, response *httptest.ResponseRecorder, status int) T {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status %d, wanted %d: %s", response.Code, status, response.Body.String())
	}
	var out T
	if err := json.Unmarshal(response.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func create(t *testing.T, app http.Handler) invoices.Invoice {
	t.Helper()
	return decode[invoices.Invoice](t, request(t, app, "POST", "/api/invoices", "acme-test", `{"customer_id":"customer-acme","amount_minor":12500,"currency":"NGN"}`), 201)
}

func TestPublicAPIInvoiceLifecycle(t *testing.T) {
	app, _ := setup(t)
	if got := request(t, app, "GET", "/", "", ""); got.Code != 200 || !strings.Contains(got.Body.String(), "/docs") {
		t.Fatalf("welcome: %d %s", got.Code, got.Body.String())
	}
	if got := request(t, app, "GET", "/api/invoices", "", ""); got.Code != 401 {
		t.Fatal(got.Code)
	}
	inv := create(t, app)
	if inv.TenantID != "acme" || inv.CreatedAt.IsZero() {
		t.Fatalf("invoice: %#v", inv)
	}
	page := decode[invoices.Page](t, request(t, app, "GET", "/api/invoices?currency=NGN&limit=1&offset=0", "acme-test", ""), 200)
	if page.Total != 1 || len(page.Items) != 1 || page.Limit != 1 {
		t.Fatalf("list: %#v", page)
	}
	other := decode[invoices.Page](t, request(t, app, "GET", "/api/invoices", "globex-test", ""), 200)
	if other.Total != 0 || other.Items == nil {
		t.Fatalf("tenant list: %#v", other)
	}
	got := decode[invoices.Invoice](t, request(t, app, "GET", "/api/invoices/"+inv.ID, "acme-test", ""), 200)
	if got.ID != inv.ID {
		t.Fatal(got)
	}
	if got := request(t, app, "GET", "/api/invoices/"+inv.ID, "globex-test", ""); got.Code != 403 {
		t.Fatal(got.Code)
	}
	updated := decode[invoices.Invoice](t, request(t, app, "PUT", "/api/invoices/"+inv.ID, "acme-test", `{"amount_minor":14000,"status":"paid","memo":"Settled"}`), 200)
	if updated.AmountMinor != 14000 || updated.Status != "paid" || updated.Memo == nil {
		t.Fatal(updated)
	}
	if got := request(t, app, "DELETE", "/api/invoices/"+inv.ID, "acme-test", ""); got.Code != 204 || got.Body.Len() != 0 {
		t.Fatalf("delete: %d %s", got.Code, got.Body.String())
	}
	if got := request(t, app, "GET", "/api/invoices/"+inv.ID, "acme-test", ""); got.Code != 404 {
		t.Fatal(got.Code)
	}
}
func TestRejectedRequestsDoNotCreateInvoices(t *testing.T) {
	app, service := setup(t)
	for _, test := range []struct {
		name, body string
		status     int
	}{
		{"cross_tenant", `{"customer_id":"customer-globex","amount_minor":1,"currency":"NGN"}`, 403},
		{"negative_amount", `{"customer_id":"customer-acme","amount_minor":-1,"currency":"NGN"}`, 400},
		{"currency_enum", `{"customer_id":"customer-acme","amount_minor":1,"currency":"EUR"}`, 400},
		{"forged_identity", `{"customer_id":"customer-acme","amount_minor":1,"currency":"NGN","tenant_id":"globex"}`, 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			out := request(t, app, "POST", "/api/invoices", "acme-test", test.body)
			if out.Code != test.status {
				t.Fatalf("%d %s", out.Code, out.Body.String())
			}
			if out.Header().Get("X-Request-ID") == "" {
				t.Fatal("missing correlation")
			}
		})
	}
	page, err := service.List(context.Background(), "acme", invoices.Filter{Limit: 100})
	if err != nil || page.Total != 0 {
		t.Fatalf("unexpected effect: %#v %v", page, err)
	}
	for _, query := range []string{"limit=0", "limit=101", "limit=1&limit=2", "currency=EUR", "offset=-1"} {
		if out := request(t, app, "GET", "/api/invoices?"+query, "acme-test", ""); out.Code != 400 {
			t.Fatalf("query %s returned %d: %s", query, out.Code, out.Body.String())
		}
	}
}
func TestUploadAndFiniteTenantEventFeed(t *testing.T) {
	app, _ := setup(t)
	inv := create(t, app)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "receipt.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.WriteString(part, "receipt evidence"); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/invoices/"+inv.ID+"/attachment", &body)
	req.Header.Set("Authorization", "Bearer acme-test")
	req.Header.Set("Content-Type", writer.FormDataContentType())
	out := httptest.NewRecorder()
	app.ServeHTTP(out, req)
	updated := decode[invoices.Invoice](t, out, 200)
	if updated.Attachment == nil || updated.Attachment.Name != "receipt.txt" || updated.Attachment.Bytes != 16 || len(updated.Attachment.SHA256) != 64 {
		t.Fatalf("attachment: %#v", updated.Attachment)
	}
	stream := request(t, app, "GET", "/api/invoices/events", "acme-test", "")
	if stream.Code != 200 || stream.Header().Get("Content-Type") != "text/event-stream" || !strings.Contains(stream.Body.String(), "event: snapshot") || !strings.Contains(stream.Body.String(), inv.ID) {
		t.Fatalf("stream: %d %s", stream.Code, stream.Body.String())
	}
	other := request(t, app, "GET", "/api/invoices/events", "globex-test", "")
	if strings.Contains(other.Body.String(), inv.ID) {
		t.Fatal("tenant invoice leaked in feed")
	}
}

type observedWriter struct {
	header http.Header
	first  chan struct{}
	once   sync.Once
}

func (w *observedWriter) Header() http.Header { return w.header }
func (w *observedWriter) WriteHeader(int)     {}
func (w *observedWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.first) })
	return len(p), nil
}
func (w *observedWriter) Flush() {}
func TestLiveFeedStopsWhenRequestIsCancelled(t *testing.T) {
	app, _ := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest("GET", "/api/invoices/events?watch=true", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer acme-test")
	writer := &observedWriter{header: make(http.Header), first: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if recovered := recover(); recovered != nil && recovered != http.ErrAbortHandler {
				t.Errorf("unexpected stream panic: %v", recovered)
			}
		}()
		app.ServeHTTP(writer, req)
	}()
	select {
	case <-writer.first:
	case <-time.After(2 * time.Second):
		t.Fatal("initial event never arrived")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream ignored cancellation")
	}
}
func TestWorkshopContractGeneratesClientsAndDocs(t *testing.T) {
	app, _ := setup(t)
	document, err := app.OpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []string{`"minimum": 1`, `"maximum": 100000000`, `"multipart/form-data"`, `"text/event-stream"`, `"date-time"`} {
		if !bytes.Contains(document, []byte(item)) {
			t.Fatalf("missing contract detail %s", item)
		}
	}
	if _, err = clientgen.Go(document, "invoiceclient"); err != nil {
		t.Fatal(err)
	}
	if _, err = clientgen.TypeScript(document); err != nil {
		t.Fatal(err)
	}
	docs := request(t, app, "GET", "/docs", "", "")
	if docs.Code != 200 || !strings.Contains(docs.Body.String(), "Send request") {
		t.Fatalf("docs: %d", docs.Code)
	}
}
