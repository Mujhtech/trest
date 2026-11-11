package docsui_test

import (
	"github.com/mujhtech/trest/internal/docsui"
	"strings"
	"testing"
)

func TestPageEscapesConfigurationAndWorksOffline(t *testing.T) {
	page := string(docsui.Page(`title </title><script>alert(1)</script>`, `/docs/</script><script>alert(2)</script>`))
	if strings.Contains(page, `<script>alert(1)`) || strings.Contains(page, `<script>alert(2)`) {
		t.Fatal("configuration escaped script context")
	}
	for _, unexpected := range []string{"src=\"http", "localStorage", "sessionStorage", "__TREST_"} {
		if strings.Contains(page, unexpected) {
			t.Fatalf("unexpected page content %q", unexpected)
		}
	}
	for _, expected := range []string{"textContent", "target.origin!==location.origin", "redirect:'error'", "credentials:'same-origin'", "AbortController", "FormData"} {
		if !strings.Contains(page, expected) {
			t.Fatalf("missing behavior %q", expected)
		}
	}
}

func TestPageSeparatesWebSocketConnectionsFromHTTPRequests(t *testing.T) {
	page := string(docsui.Page("Example", "/docs/openapi.json"))
	for _, expected := range []string{
		`id="protocol-note"`,
		"generated typed WebSocket client or a native WebSocket client",
		"entry?.op?.['x-trest-protocol']==='websocket'",
		"$('send').disabled=native",
		"if(!selected||isWebSocket(selected))return",
		"controller=null;updateProtocol()",
	} {
		if !strings.Contains(page, expected) {
			t.Fatalf("missing WebSocket request boundary %q", expected)
		}
	}
}
