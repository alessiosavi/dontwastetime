package graph

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestMessageHeaders_Parsed(t *testing.T) {
	var path, sel string
	_, c, _ := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		path, sel = r.URL.EscapedPath(), r.URL.Query().Get("$select")
		_, _ = w.Write([]byte(`{"id":"AAMk-1","internetMessageHeaders":[
			{"name":"List-Unsubscribe","value":"<mailto:u@news.example>"},
			{"name":"Auto-Submitted","value":"auto-generated"}]}`))
	})
	hs, err := c.MessageHeaders(context.Background(), "marketing@example.test", "AAMk-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(hs) != 2 || hs[0] != (Header{Name: "List-Unsubscribe", Value: "<mailto:u@news.example>"}) || hs[1].Name != "Auto-Submitted" {
		t.Fatalf("headers: %+v", hs)
	}
	if !strings.HasSuffix(path, "/users/marketing@example.test/messages/AAMk-1") || sel != "internetMessageHeaders" {
		t.Errorf("path %q select %q", path, sel)
	}
}

func TestListAttachments_FiltersInlineAndType(t *testing.T) {
	_, c, _ := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"value":[
			{"@odata.type":"#microsoft.graph.fileAttachment","id":"a1","name":"Fattura_12.pdf","contentType":"application/pdf","size":1200,"isInline":false,"contentBytes":"SGVsbG8="},
			{"@odata.type":"#microsoft.graph.fileAttachment","id":"a2","name":"logo.png","contentType":"image/png","size":10,"isInline":true,"contentBytes":"SGVsbG8="},
			{"@odata.type":"#microsoft.graph.itemAttachment","id":"a3","name":"forwarded","contentType":"message/rfc822","size":99,"isInline":false},
			{"@odata.type":"#microsoft.graph.fileAttachment","id":"a4","name":"fattura.xml","contentType":"text/xml","size":800,"isInline":false,"contentBytes":"SGVsbG8="}]}`))
	})
	atts, err := c.ListAttachments(context.Background(), "admin@example.test", "AAMk-9")
	if err != nil {
		t.Fatal(err)
	}
	want := []Attachment{
		{ID: "a1", Name: "Fattura_12.pdf", ContentType: "application/pdf", Size: 1200},
		{ID: "a4", Name: "fattura.xml", ContentType: "text/xml", Size: 800},
	}
	if !reflect.DeepEqual(atts, want) {
		t.Fatalf("attachments = %+v, want %+v", atts, want)
	}
	// Metadata only: the struct must have no place to carry content bytes.
	at := reflect.TypeOf(Attachment{})
	for i := range at.NumField() {
		if f := at.Field(i); f.Type.Kind() == reflect.Slice || strings.Contains(strings.ToLower(f.Name), "content") && f.Name != "ContentType" {
			t.Errorf("Attachment.%s could carry content", f.Name)
		}
	}
}

func TestProbeRead_TypedErrors(t *testing.T) {
	var q string
	_, c, _ := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		q = r.URL.Query().Get("$top")
		if strings.Contains(r.URL.Path, "legal") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`{"value":[{"id":"x"}]}`))
	})
	if err := c.ProbeRead(context.Background(), "tech@example.test", "inbox"); err != nil || q != "1" {
		t.Fatalf("probe ok mailbox: err=%v $top=%q", err, q)
	}
	var fe *ErrMailboxForbidden
	if err := c.ProbeRead(context.Background(), "legal@example.test", "sentitems"); !errors.As(err, &fe) {
		t.Fatalf("probe forbidden: %v", err)
	}
}

func TestSendMail_BodyShape(t *testing.T) {
	var got map[string]any
	var method, path string
	_, c, _ := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.EscapedPath()
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Errorf("body not JSON: %v", err)
		}
		w.WriteHeader(http.StatusAccepted)
	})
	err := c.SendMail(context.Background(), "admin@example.test", []string{"a@example.test", "b@example.test"}, "Riepilogo", "<p>ciao</p>", false)
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || !strings.HasSuffix(path, "/users/admin@example.test/sendMail") {
		t.Errorf("%s %s", method, path)
	}
	if save, ok := got["saveToSentItems"].(bool); !ok || save {
		t.Errorf("saveToSentItems must be explicitly false, got %v", got["saveToSentItems"])
	}
	msg := got["message"].(map[string]any)
	body := msg["body"].(map[string]any)
	if body["contentType"] != "HTML" || body["content"] != "<p>ciao</p>" || msg["subject"] != "Riepilogo" {
		t.Errorf("message = %v", msg)
	}
	to := msg["toRecipients"].([]any)
	if len(to) != 2 || to[1].(map[string]any)["emailAddress"].(map[string]any)["address"] != "b@example.test" {
		t.Errorf("toRecipients = %v", to)
	}
}

func TestSendMail_202Only(t *testing.T) {
	var calls atomic.Int32
	_, c, _ := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	err := c.SendMail(context.Background(), "admin@example.test", []string{"a@example.test"}, "s", "h", false)
	if err == nil {
		t.Fatal("a non-202 success must be an error")
	}
	if err := c.SendMail(context.Background(), "admin@example.test", nil, "s", "h", false); err == nil {
		t.Fatal("no recipients must be refused")
	}
	if calls.Load() != 1 {
		t.Errorf("refused send must not hit the network: %d calls", calls.Load())
	}
}
